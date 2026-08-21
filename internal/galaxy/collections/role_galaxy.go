package collections

import (
	"context"
	"errors"
	"fmt"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/galaxyv1"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/requirements"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// galaxyRolePinKey is the store key of a Galaxy role's own pin: which
// repository and tag the name resolved to at the requested version, so a
// rerun rebuilds the git request without the two v1 round trips. Keyed by
// the Galaxy name and the version asked for, the requirement line itself,
// as a git pin is keyed by its own. The git pin beneath it is keyed by the
// repository and tag, and is what carries the commit.
func galaxyRolePinKey(galaxyName, requested string) string {
	return "galaxy\n" + galaxyName + "\n" + requested
}

// resolveGalaxyRole resolves a Galaxy role: the name is mapped to a
// repository and a tag - from the Galaxy pin when the cache policy allows,
// else through the v1 API of the first configured server that knows it -
// and from there the role takes the git path exactly as an scm role does,
// with its own pin, its own --refresh advertisement and its own artifact.
// The Galaxy pin is recorded after the git path answered, since it carries
// the commit the git path resolved.
func resolveGalaxyRole(ctx context.Context, deps collectionDeps, req requirements.RoleRequirement) (rolePin, error) {
	policy := cacheManager.PolicyForConstraint(deps.cfg, req.Version != "")
	key := galaxyRolePinKey(req.Src, req.Version)
	res, server, ok, err := replayGalaxyPin(deps, key, policy)
	if err != nil {
		return rolePin{}, err
	}
	if !ok {
		if deps.cfg != nil && deps.cfg.Offline {
			return rolePin{}, fmt.Errorf("%w: Galaxy role %s@%s is not recorded in the cache",
				helpers.ErrOfflineMode, req.Src, displayRoleVersion(req.Version))
		}
		res, server, err = lookupGalaxyRole(ctx, deps, req, policy)
		if err != nil {
			return rolePin{}, err
		}
	}
	greq := gitRoleRequest{
		name:    req.Name,
		pinKey:  gitsource.PinKey(res.RepoURL.String(), res.Ref.Name, ""),
		display: helpers.URLForMessage(res.RepoURL.String()),
		url:     res.RepoURL,
		ref:     res.Ref,
	}
	greq.cred, _ = gitsource.MatchCredential(res.RepoURL, gitCredentialsOf(deps))
	pin, err := resolveGitRoleRequest(ctx, deps, greq, res.GalaxySHA)
	if err != nil {
		return rolePin{}, err
	}
	pin.galaxyName = req.Src
	pin.server = server
	pin.version = res.Version
	if policy.Write {
		deps.st.SetRolePin(key, store.RolePinEntry{
			Repository: res.RepoURL.String(),
			Commit:     pin.commit,
			Version:    res.Version,
			Ref:        res.Ref.Name,
			GalaxySHA:  res.GalaxySHA,
			Server:     server,
		})
	}
	return pin, nil
}

// replayGalaxyPin rebuilds the v1 answer from the Galaxy pin when the policy
// allows a read and the run is not refreshing (--offline outranks --refresh,
// as it does everywhere else): the repository must be a GitHub repository
// as a live answer would be, the ref a qualified tag or branch, and the
// version a role version, since a pin is cache state and is judged as a
// server's answer would be.
func replayGalaxyPin(deps collectionDeps, key string, policy cacheManager.Policy) (galaxyv1.Resolution, string, bool, error) {
	refreshing := deps.cfg != nil && deps.cfg.Refresh && !deps.cfg.Offline
	if !policy.Read || refreshing {
		return galaxyv1.Resolution{}, "", false, nil
	}
	pin, ok := deps.st.GetRolePin(key)
	if !ok {
		return galaxyv1.Resolution{}, "", false, nil
	}
	res, err := galaxyPinResolution(pin)
	if err != nil {
		return galaxyv1.Resolution{}, "", false, err
	}
	return res, pin.Server, true, nil
}

// galaxyPinResolution re-validates a Galaxy pin's fields into a resolution.
func galaxyPinResolution(pin store.RolePinEntry) (galaxyv1.Resolution, error) {
	repo, err := gitsource.ParseURL(pin.Repository)
	if err != nil {
		return galaxyv1.Resolution{}, fmt.Errorf("%w: recorded Galaxy role pin names repository %s",
			helpers.ErrInvalidGitLocator, helpers.URLForMessage(pin.Repository))
	}
	if err := galaxyv1.ValidateRepository(repo); err != nil {
		return galaxyv1.Resolution{}, fmt.Errorf("recorded Galaxy role pin: %w", err)
	}
	ref, err := gitsource.ParseRef(pin.Ref)
	if err != nil || ref.Kind != gitsource.RefQualified || !helpers.IsRoleVersion(pin.Version) {
		return galaxyv1.Resolution{}, fmt.Errorf("%w: recorded Galaxy role pin names ref %q for version %q",
			helpers.ErrInvalidRoleVersion, helpers.TruncateForMessage(pin.Ref), helpers.TruncateForMessage(pin.Version))
	}
	return galaxyv1.Resolution{RepoURL: repo, Ref: ref, Version: pin.Version, GalaxySHA: pin.GalaxySHA}, nil
}

// lookupGalaxyRole walks the configured servers in order and asks each
// one's v1 API for the role. A server without a v1 API and a server that
// has one but not the role are both passed over; any other answer aborts
// the walk, since a different server could not route around it. When no
// server answered, the error says which of the two it was: no v1 anywhere
// is configuration, a role nobody has is resolution. The base of the server
// that answered is returned beside the resolution, for the lockfile's
// provenance.
func lookupGalaxyRole(
	ctx context.Context, deps collectionDeps, req requirements.RoleRequirement, policy cacheManager.Policy,
) (galaxyv1.Resolution, string, error) {
	owner, name, ok := helpers.SplitRoleName(req.Src)
	if !ok {
		return galaxyv1.Resolution{}, "", fmt.Errorf("%w: %q", helpers.ErrInvalidRoleName, helpers.TruncateForMessage(req.Src))
	}
	fetch := func(ctx context.Context, u string, out any, policy cacheManager.Policy) error {
		return fetchJSONWithCachePolicy(ctx, deps.runtime, u, deps.st, out, policy)
	}
	var (
		withoutV1 []string
		anyV1     bool
	)
	for _, srv := range unpinnedServerCandidates(deps.cfg) {
		deps.runtime.Output.Debugf("role %s: v1 lookup on server %s", req.Src, srv.label())
		res, found, warnings, err := galaxyv1.Resolve(ctx, fetch, srv.base, owner, name, req.Version, policy)
		for _, w := range warnings {
			deps.runtime.Output.Warnf("%s: %s", req.Src, w)
		}
		switch {
		case errors.Is(err, helpers.ErrGalaxyRoleAPIUnavailable):
			withoutV1 = append(withoutV1, srv.label())
			continue
		case err != nil:
			return galaxyv1.Resolution{}, "", fmt.Errorf("server %s: %w", srv.label(), err)
		case found:
			return res, srv.base, nil
		}
		anyV1 = true
	}
	if !anyV1 {
		return galaxyv1.Resolution{}, "", fmt.Errorf("%w: none of %v serves it; roles need galaxy.ansible.com or a standalone Galaxy NG",
			helpers.ErrGalaxyRoleAPIUnavailable, withoutV1)
	}
	return galaxyv1.Resolution{}, "", fmt.Errorf("%w: %s", helpers.ErrRoleNotFound, req.Src)
}
