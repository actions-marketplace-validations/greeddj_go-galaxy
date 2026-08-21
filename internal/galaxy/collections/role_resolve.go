package collections

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/requirements"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// resolvedRole is one role the run installs: its install name, the locator
// its artifact is keyed under and its record and lockfile entry carry (the
// one string every consumer compares, exactly as a git collection's Source),
// the repository and ref it came from, the concrete version, the Galaxy name
// and the server that answered for a role that came through the Galaxy API,
// and the install names of the roles its meta depends on, in declaration
// order.
type resolvedRole struct {
	Name       string
	Source     string
	Repository string
	Ref        string
	Version    string
	GalaxyName string
	Server     string
	Kind       string
	// Requested is the version the requirement asked for, as written ("" for
	// the highest), kept apart from Version so a later request for the same
	// role is compared with what was asked, not with what was chosen.
	Requested string
	Deps      []string
}

// key renders the role as "name@version" for messages and the warmed set.
func (r resolvedRole) key() string { return r.Name + "@" + r.Version }

// roleResolution is what resolveRoles produced: every role by install name,
// and the order they were discovered in - requirements first, in file order,
// then each level of dependencies in declaration order - which is the order
// first-wins was decided in and the order the install reports in.
type roleResolution struct {
	roles map[string]resolvedRole
	order []string
}

// resolveRoles turns the roles: entries into the set of roles to install,
// walking their dependencies breadth-first. Each level is resolved on the
// download-worker pool and merged in input order, so which requirement wins
// a name is decided by the file's and the meta's declaration order, never by
// which fetch finished first - ansible's first-wins, made deterministic. A
// later requirement for a name already taken with a different source or
// version is reported and ignored, where ansible is silent. A dependency
// ansible would not look up (a local role, a collection's role) is skipped
// with a line of its own; one it would refuse is a usage error naming the
// role that declared it. --no-deps stops the walk at the requirements.
func resolveRoles(ctx context.Context, deps collectionDeps, roots []requirements.RoleRequirement) (roleResolution, error) {
	res := roleResolution{roles: make(map[string]resolvedRole, len(roots))}
	if len(roots) == 0 {
		return res, nil
	}
	deps.runtime.Output.Printf("🎭 resolve roles")
	queue := make([]roleRequest, 0, len(roots))
	for _, root := range roots {
		queue = append(queue, roleRequest{req: root, declaredBy: "requirements.yml"})
	}
	for len(queue) > 0 {
		level := dedupeRoleLevel(deps, res.roles, queue)
		pins, err := resolveRoleLevel(ctx, deps, level)
		if err != nil {
			return roleResolution{}, err
		}
		queue = queue[:0]
		for i, rr := range level {
			role := rolePinToResolved(rr.req, pins[i])
			res.roles[role.Name] = role
			res.order = append(res.order, role.Name)
			if len(res.roles) > helpers.RoleGraphMaxRoles {
				return roleResolution{}, fmt.Errorf("%w: more than %d roles discovered through dependencies",
					helpers.ErrInvalidRoleEntry, helpers.RoleGraphMaxRoles)
			}
			next, err := roleDependencies(deps, rr.req.Name, pins[i].deps)
			if err != nil {
				return roleResolution{}, err
			}
			queue = append(queue, next...)
		}
	}
	fillRoleDeps(&res, deps)
	return res, nil
}

// roleRequest is one requirement waiting to be resolved and the role (or
// the file) that declared it, for messages.
type roleRequest struct {
	req        requirements.RoleRequirement
	declaredBy string
}

// dedupeRoleLevel drops from level every request whose install name is
// already taken - by an earlier level or by an earlier entry of this one -
// reporting a conflict when what was asked for differs.
func dedupeRoleLevel(deps collectionDeps, taken map[string]resolvedRole, level []roleRequest) []roleRequest {
	out := make([]roleRequest, 0, len(level))
	inLevel := make(map[string]requirements.RoleRequirement, len(level))
	for _, rr := range level {
		var prior requirements.RoleRequirement
		if got, ok := taken[rr.req.Name]; ok {
			prior = requirements.RoleRequirement{Name: got.Name, Src: roleSrcOf(got), Version: got.Requested, Type: got.Kind}
		} else if got, ok := inLevel[rr.req.Name]; ok {
			prior = got
		} else {
			inLevel[rr.req.Name] = rr.req
			out = append(out, rr)
			continue
		}
		if prior.Src != rr.req.Src || prior.Version != rr.req.Version {
			deps.runtime.Output.Warnf("role %s: already requested as %s@%s; ignoring %s@%s asked for by %s (first wins, as in ansible-galaxy)",
				rr.req.Name, prior.Src, displayRoleVersion(prior.Version), rr.req.Src, displayRoleVersion(rr.req.Version), rr.declaredBy)
		}
	}
	return out
}

// roleSrcOf renders what a resolved role was asked for as, the way the
// requirement spelled it: the Galaxy name for a Galaxy role, the repository
// for a git role.
func roleSrcOf(r resolvedRole) string {
	if r.GalaxyName != "" {
		return r.GalaxyName
	}
	return r.Repository
}

func displayRoleVersion(v string) string {
	if v == "" {
		return "latest"
	}
	return v
}

// resolveRoleLevel resolves one level's requests concurrently and returns
// their pins in input order; the first error wins.
func resolveRoleLevel(ctx context.Context, deps collectionDeps, level []roleRequest) ([]rolePin, error) {
	pins := make([]rolePin, len(level))
	errs := make([]error, len(level))
	var wg sync.WaitGroup
	sem := make(chan struct{}, max(deps.cfg.DownloadWorkers, 1))
	for i, rr := range level {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			pins[i], errs[i] = resolveRoleRequest(ctx, deps, rr.req)
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			return nil, fmt.Errorf("role %s: %w", level[i].req.Name, err)
		}
	}
	return pins, nil
}

// resolveRoleRequest resolves one requirement by its kind and records the
// pin in the memo under the install name.
func resolveRoleRequest(ctx context.Context, deps collectionDeps, req requirements.RoleRequirement) (rolePin, error) {
	var pin rolePin
	var err error
	switch req.Type {
	case requirements.TypeGit:
		pin, err = resolveGitRole(ctx, deps, req)
	default:
		pin, err = resolveGalaxyRole(ctx, deps, req)
	}
	if err != nil {
		return rolePin{}, err
	}
	pin.kind = req.Type
	deps.roleMemo.put(req.Name, pin)
	return pin, nil
}

// rolePinToResolved is the resolved view of a pin; Deps is filled by
// fillRoleDeps once every name is known.
func rolePinToResolved(req requirements.RoleRequirement, pin rolePin) resolvedRole {
	return resolvedRole{
		Name:       req.Name,
		Source:     pin.locator,
		Repository: pin.repository,
		Ref:        pin.ref,
		Version:    pin.version,
		GalaxyName: pin.galaxyName,
		Server:     pin.server,
		Kind:       req.Type,
		Requested:  req.Version,
	}
}

// roleDependencies turns the dependencies one role's meta declared into the
// next level's requests, skipping the shapes ansible skips and refusing the
// ones it would refuse. Under --no-deps nothing is walked.
func roleDependencies(deps collectionDeps, declaredBy string, declared []gitsource.RoleDependency) ([]roleRequest, error) {
	if deps.cfg != nil && deps.cfg.NoDeps {
		return nil, nil
	}
	out := make([]roleRequest, 0, len(declared))
	for i, dep := range declared {
		req, skip, err := requirements.ParseRoleDependency(dep)
		if err != nil {
			return nil, fmt.Errorf("role %s declares dependency %d: %w", declaredBy, i, err)
		}
		switch skip {
		case requirements.DependencyLocal:
			deps.runtime.Output.Debugf("role %s depends on %s, a local role; not installed", declaredBy, roleDepDisplay(dep))
		case requirements.DependencyCollection:
			deps.runtime.Output.Warnf("role %s depends on %s, a collection's role; install the collection instead", declaredBy, roleDepDisplay(dep))
		case requirements.DependencyInstalled:
			out = append(out, roleRequest{req: req, declaredBy: "role " + declaredBy})
		}
	}
	return out, nil
}

// roleDepDisplay renders a dependency for a message: the source as written,
// or the name when that is all it carried.
func roleDepDisplay(dep gitsource.RoleDependency) string {
	if dep.Src != "" {
		return helpers.URLForMessage(dep.Src)
	}
	return helpers.TruncateForMessage(dep.Name)
}

// fillRoleDeps records, on every resolved role, the install names of the
// dependencies that ended up in the resolution: the names an install worker
// and the lockfile carry. A dependency that was skipped, or lost a name to
// an earlier requirement, contributes the name that won.
func fillRoleDeps(res *roleResolution, deps collectionDeps) {
	for name, role := range res.roles {
		pin, ok := deps.roleMemo.get(name)
		if !ok {
			continue
		}
		for _, dep := range pin.deps {
			req, skip, err := requirements.ParseRoleDependency(dep)
			if err != nil || skip != requirements.DependencyInstalled {
				continue
			}
			if _, ok := res.roles[req.Name]; ok && req.Name != name {
				role.Deps = append(role.Deps, req.Name)
			}
		}
		res.roles[name] = role
	}
}

// gitRoleRequest is a git role requirement with its parts judged: the URL,
// the ref, the credential bound to the URL, and the pin key the store
// records it under.
type gitRoleRequest struct {
	name    string
	pinKey  string
	display string
	cred    gitsource.Credential
	url     gitsource.URL
	ref     gitsource.Ref
}

func newGitRoleRequest(deps collectionDeps, req requirements.RoleRequirement) (gitRoleRequest, error) {
	u, err := gitsource.ParseURL(req.Src)
	if err != nil {
		return gitRoleRequest{}, err
	}
	ref, err := gitsource.ParseRef(req.Version)
	if err != nil {
		return gitRoleRequest{}, err
	}
	cred, _ := gitsource.MatchCredential(u, gitCredentialsOf(deps))
	return gitRoleRequest{
		name:    req.Name,
		pinKey:  gitsource.PinKey(u.String(), ref.Name, ""),
		display: helpers.URLForMessage(u.String()),
		cred:    cred,
		url:     u,
		ref:     ref,
	}, nil
}

// resolveGitRole resolves a git role the way expandGitRoot resolves a git
// collection: the recorded pin is replayed when the cache policy allows a
// read and the artifact is still cached; a miss under --offline is refused;
// --refresh re-advertises a branch or tag and keeps the pin when the commit
// is unchanged; otherwise the repository is fetched and the role built.
func resolveGitRole(ctx context.Context, deps collectionDeps, req requirements.RoleRequirement) (rolePin, error) {
	greq, err := newGitRoleRequest(deps, req)
	if err != nil {
		return rolePin{}, err
	}
	return resolveGitRoleRequest(ctx, deps, greq, "")
}

// resolveGitRoleRequest is the git path proper, shared by an scm role and a
// Galaxy role once its name has been mapped to a repository and a tag.
// galaxySHA is the commit the Galaxy API recorded for the tag, "" for an scm
// role, cross-checked against what the repository advertises.
func resolveGitRoleRequest(ctx context.Context, deps collectionDeps, greq gitRoleRequest, galaxySHA string) (rolePin, error) {
	policy := cacheManager.PolicyForConstraint(deps.cfg, greq.ref.IsCommit())
	if policy.Read {
		if pin, ok := deps.st.GetRolePin(greq.pinKey); ok {
			if replayed, err := replayRolePin(ctx, deps, greq, pin); err != nil || replayed.locator != "" {
				return replayed, err
			}
		}
	}
	if deps.cfg != nil && deps.cfg.Offline {
		return rolePin{}, fmt.Errorf("%w: role source %s@%s is not recorded in the cache", helpers.ErrOfflineMode, greq.display, greq.ref.Name)
	}
	if refreshed, ok, err := refreshRolePin(ctx, deps, greq, policy, galaxySHA); ok || err != nil {
		return refreshed, err
	}
	return acquireRole(ctx, deps, greq, policy, "", galaxySHA)
}

// refreshRolePin is the cheap half of --refresh for a branch or tag pin: one
// advertisement, and if the commit is unchanged and the artifact is still
// cached, the pin stands. ok=false when there is no pin to refresh.
func refreshRolePin(
	ctx context.Context, deps collectionDeps, greq gitRoleRequest, policy cacheManager.Policy, galaxySHA string,
) (rolePin, bool, error) {
	if deps.cfg == nil || !deps.cfg.Refresh || greq.ref.IsCommit() {
		return rolePin{}, false, nil
	}
	pin, ok := deps.st.GetRolePin(greq.pinKey)
	if !ok {
		return rolePin{}, false, nil
	}
	commit, _, err := deps.runtime.Git.Advertise(ctx, greq.url, greq.ref, greq.cred)
	if err != nil {
		return rolePin{}, false, err
	}
	if commit == pin.Commit {
		replayed, err := replayRolePin(ctx, deps, greq, pin)
		if err != nil {
			return rolePin{}, true, err
		}
		if replayed.locator != "" {
			if policy.Write {
				deps.st.SetRolePin(greq.pinKey, pin)
			}
			return replayed, true, nil
		}
	}
	refreshed, err := acquireRole(ctx, deps, greq, policy, commit, galaxySHA)
	return refreshed, true, err
}

// replayRolePin turns a recorded pin into a rolePin, re-validating what it
// carries - the pin is cache state, judged on the way in exactly as a
// remote's answer is - and only when the artifact it names is still in the
// store: a pin whose artifact was swept would send the install phase to the
// remote anyway, so discovery fetches now, while it holds the ref. The zero
// rolePin reports "not replayable".
func replayRolePin(ctx context.Context, deps collectionDeps, greq gitRoleRequest, pin store.RolePinEntry) (rolePin, error) {
	if !gitsource.IsCommitHash(pin.Commit) {
		return rolePin{}, fmt.Errorf("%w: recorded role pin for %s names commit %q", helpers.ErrInvalidGitLocator, greq.display, pin.Commit)
	}
	if !helpers.IsRoleVersion(pin.Version) {
		return rolePin{}, fmt.Errorf("%w: recorded role pin for %s names version %q",
			helpers.ErrInvalidRoleVersion, greq.display, helpers.TruncateForMessage(pin.Version))
	}
	if pin.Repository != greq.url.String() {
		return rolePin{}, fmt.Errorf("%w: recorded role pin for %s names repository %s",
			helpers.ErrInvalidGitLocator, greq.display, helpers.URLForMessage(pin.Repository))
	}
	locator := gitsource.Locator{URL: greq.url.String(), Commit: pin.Commit}.String()
	if !roleArtifactCached(ctx, deps, locator, greq.name, pin.Version) {
		return rolePin{}, nil
	}
	return rolePin{
		deps:       pinDepsToSource(pin.Deps),
		locator:    locator,
		commit:     pin.Commit,
		repository: greq.url.String(),
		ref:        greq.ref.Name,
		version:    pin.Version,
		galaxySHA:  pin.GalaxySHA,
		roleName:   pin.GalaxyRoleName,
	}, nil
}

func roleArtifactCached(ctx context.Context, deps collectionDeps, locator, name, version string) bool {
	if deps.gitStore == nil {
		return false
	}
	ok, err := deps.gitStore.Has(ctx, helpers.ArtifactKey(locator, helpers.RoleArtifactFilename(name, version)))
	return err == nil && ok
}

func pinDepsToSource(deps []store.RolePinDep) []gitsource.RoleDependency {
	out := make([]gitsource.RoleDependency, 0, len(deps))
	for _, d := range deps {
		out = append(out, gitsource.RoleDependency{Src: d.Src, Scm: d.Scm, Version: d.Version, Name: d.Name})
	}
	return out
}

func sourceDepsToPin(deps []gitsource.RoleDependency) []store.RolePinDep {
	out := make([]store.RolePinDep, 0, len(deps))
	for _, d := range deps {
		out = append(out, store.RolePinDep{Src: d.Src, Scm: d.Scm, Version: d.Version, Name: d.Name})
	}
	return out
}

// acquireRole fetches the repository, builds the role, commits or keeps the
// artifact, records the pin, and returns it. commit, when non-empty, is the
// tip an advertisement just resolved, so the acquisition fetches exactly
// that commit. galaxySHA is what the Galaxy API recorded for the tag, kept
// on the pin beside the commit the repository advertised.
func acquireRole(
	ctx context.Context, deps collectionDeps, greq gitRoleRequest, policy cacheManager.Policy, commit, galaxySHA string,
) (rolePin, error) {
	runtime := deps.runtime
	runtime.Output.Printf("🎭 fetching role %s from %s@%s", greq.name, greq.display, greq.ref.Name)
	start := time.Now()
	gitCtx, cancel := context.WithTimeout(ctx, runtime.GitDeadline())
	defer cancel()
	result, err := runtime.Git.AcquireRole(gitCtx, gitsource.RoleRequest{
		URL:      greq.url,
		Ref:      greq.ref,
		Commit:   commit,
		Auth:     greq.cred,
		TempFile: gitTempFile(deps),
	})
	if err != nil {
		return rolePin{}, artifactDeadlineError(ctx, gitCtx, runtime.GitDeadline(), err)
	}
	runtime.Output.DebugSincef(start, "fetch %s@%s (%d bytes)", greq.display, greq.ref.Name, result.BytesFetched)
	runtime.Metrics.AddBytesDownloaded(result.BytesFetched)
	for _, warning := range result.Warnings {
		runtime.Output.Warnf("%s: %s", greq.display, warning)
	}
	if galaxySHA != "" && galaxySHA != result.Commit {
		runtime.Output.Warnf("%s: Galaxy recorded commit %s for %s but the repository advertises %s; "+
			"the repository wins, as it does for ansible-galaxy", greq.display, galaxySHA, greq.ref.Name, result.Commit)
	}
	version := roleVersionFor(greq.ref, result.RefName)
	pin := rolePin{
		deps:       result.Dependencies,
		locator:    gitsource.Locator{URL: greq.url.String(), Commit: result.Commit}.String(),
		commit:     result.Commit,
		repository: greq.url.String(),
		ref:        greq.ref.Name,
		version:    version,
		galaxySHA:  galaxySHA,
		roleName:   result.GalaxyRoleName,
	}
	if err := storeRoleArtifact(ctx, deps, &pin, greq.name, result); err != nil {
		return rolePin{}, err
	}
	if policy.Write {
		deps.st.SetRolePin(greq.pinKey, store.RolePinEntry{
			Repository:     greq.url.String(),
			Commit:         result.Commit,
			Version:        version,
			GalaxySHA:      galaxySHA,
			GalaxyRoleName: result.GalaxyRoleName,
			Deps:           sourceDepsToPin(result.Dependencies),
		})
	}
	return pin, nil
}

// roleVersionFor is the concrete version a role installs as: the tag, the
// branch name, or the commit the requirement spelled, and for HEAD the name
// of whatever HEAD pointed at - what ansible-galaxy writes into
// .galaxy_install_info and what ansible-galaxy role list shows.
func roleVersionFor(ref gitsource.Ref, refName string) string {
	name := ref.Name
	if ref.Kind == gitsource.RefHEAD {
		name = refName
	}
	name = strings.TrimPrefix(strings.TrimPrefix(name, "refs/heads/"), "refs/tags/")
	if name == "" || !helpers.IsRoleVersion(name) {
		return "HEAD"
	}
	return name
}

// storeRoleArtifact commits the built artifact to the artifact store under
// its locator-scoped key, or hands it to the memo under --no-cache, or
// discards it under --dry-run - storeGitArtifacts' three-way switch for a
// single artifact.
func storeRoleArtifact(ctx context.Context, deps collectionDeps, pin *rolePin, name string, result gitsource.RoleResult) error {
	switch {
	case deps.cfg != nil && deps.cfg.DryRun:
		cleanupIfNeeded(result.Cleanup)
	case keepsPrebuilt(deps):
		pin.prebuilt = &downloadResult{Path: result.ArtifactPath, SHA: result.ArtifactSHA, Cleanup: result.Cleanup}
	default:
		key := helpers.ArtifactKey(pin.locator, helpers.RoleArtifactFilename(name, pin.version))
		if _, err := commitDownload(ctx, deps.gitStore, key, result.ArtifactPath, result.ArtifactSHA, result.Cleanup); err != nil {
			return fmt.Errorf("committing role artifact %s: %w", name, err)
		}
		deps.runtime.Metrics.AddCacheMiss()
	}
	return nil
}
