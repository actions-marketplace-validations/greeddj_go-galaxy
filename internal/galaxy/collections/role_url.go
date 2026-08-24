package collections

import (
	"context"
	"fmt"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/requirements"
	"github.com/greeddj/go-galaxy/internal/galaxy/rolebuild"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/galaxy/tartree"
	"github.com/greeddj/go-galaxy/internal/galaxy/urlsource"
)

// urlRolePinKeyPrefix scopes a url role's pin in the shared role_pins
// bucket: "url\n" plus the canonical URL. One newline makes the key space
// provably disjoint from a git pin's (url\nref\nsubdir - two newlines) and a
// Galaxy pin's (galaxy\nname\nversion - two newlines).
const urlRolePinKeyPrefix = "url\n"

// urlRoleRequest is a url role requirement with its parts judged: the
// canonical URL, the version label the entry asked for ("" for none), the
// pin key and the display form for messages.
type urlRoleRequest struct {
	name      string
	rawURL    string
	requested string
	pinKey    string
	display   string
}

func newURLRoleRequest(req requirements.RoleRequirement) (urlRoleRequest, error) {
	u, err := urlsource.ParseURL(req.Src)
	if err != nil {
		return urlRoleRequest{}, err
	}
	return urlRoleRequest{
		name:      req.Name,
		rawURL:    u.String(),
		requested: req.Version,
		pinKey:    urlRolePinKeyPrefix + u.String(),
		display:   helpers.URLForMessage(u.String()),
	}, nil
}

// resolveURLRole resolves a url role the way expandURLRoot resolves a url
// collection: the recorded pin is replayed when the cache policy allows a
// read and the repacked artifact is still cached; a miss under --offline is
// refused; --refresh re-downloads, since a URL has no cheaper probe; else
// the tarball is downloaded and repacked into this tool's canonical role
// artifact.
func resolveURLRole(ctx context.Context, deps collectionDeps, req requirements.RoleRequirement) (rolePin, error) {
	ureq, err := newURLRoleRequest(req)
	if err != nil {
		return rolePin{}, err
	}
	policy := cacheManager.PolicyForConstraint(deps.cfg, false)
	if policy.Read {
		if pin, ok := deps.st.GetRolePin(ureq.pinKey); ok {
			if replayed, err := replayURLRolePin(ctx, deps, ureq, pin); err != nil || replayed.locator != "" {
				return replayed, err
			}
		}
	}
	if deps.cfg != nil && deps.cfg.Offline {
		return rolePin{}, fmt.Errorf("%w: role source %s is not recorded in the cache", helpers.ErrOfflineMode, ureq.display)
	}
	return acquireURLRole(ctx, deps, ureq, policy)
}

// replayURLRolePin turns a recorded pin into a rolePin, re-validating what
// it carries and requiring the repacked artifact it names to still be in
// the store, for the reasons replayRolePin gives. The zero rolePin reports
// "not replayable".
func replayURLRolePin(ctx context.Context, deps collectionDeps, ureq urlRoleRequest, pin store.RolePinEntry) (rolePin, error) {
	if !helpers.IsSHA256Hex(pin.SHA256) {
		return rolePin{}, fmt.Errorf("%w: recorded role pin for %s names sha256 %q",
			helpers.ErrInvalidURLLocator, ureq.display, helpers.TruncateForMessage(pin.SHA256))
	}
	if !helpers.IsRoleVersion(pin.Version) {
		return rolePin{}, fmt.Errorf("%w: recorded role pin for %s names version %q",
			helpers.ErrInvalidRoleVersion, ureq.display, helpers.TruncateForMessage(pin.Version))
	}
	if pin.URL != ureq.rawURL {
		return rolePin{}, fmt.Errorf("%w: recorded role pin for %s names url %s",
			helpers.ErrInvalidURLLocator, ureq.display, helpers.URLForMessage(pin.URL))
	}
	if version := urlRoleVersion(ureq.requested, pin.SHA256); version != pin.Version {
		return rolePin{}, fmt.Errorf("%w: role %s locked as version %q, requirements ask for %q",
			helpers.ErrInvalidRoleVersion, ureq.name, pin.Version, version)
	}
	locator := urlsource.Locator{URL: ureq.rawURL, SHA256: pin.SHA256}.String()
	if !roleArtifactCached(ctx, deps, locator, ureq.name, pin.Version) {
		return rolePin{}, nil
	}
	return rolePin{
		deps:     pinDepsToSource(pin.Deps),
		locator:  locator,
		url:      ureq.rawURL,
		sha256:   pin.SHA256,
		version:  pin.Version,
		roleName: pin.GalaxyRoleName,
	}, nil
}

// acquireURLRole downloads the tarball, repacks it into the canonical role
// artifact through tartree and rolebuild, commits or keeps the artifact, and
// records the pin. The origin sha256 - the bytes as served, before the
// repack - is the pin: it is what a later run's re-download is compared to.
func acquireURLRole(ctx context.Context, deps collectionDeps, ureq urlRoleRequest, policy cacheManager.Policy) (rolePin, error) {
	runtime := deps.runtime
	runtime.Output.Printf("Fetching role %s from %s", ureq.name, ureq.display)
	result, err := downloadURLToTemp(ctx, deps, ureq.rawURL)
	if err != nil {
		return rolePin{}, err
	}
	built, err := repackURLRole(ctx, deps, ureq, result)
	if err != nil {
		return rolePin{}, err
	}
	for _, warning := range built.Warnings {
		runtime.Output.Warnf("%s: %s", ureq.display, warning)
	}
	version := urlRoleVersion(ureq.requested, result.SHA)
	pin := rolePin{
		deps:     built.Meta.Dependencies,
		locator:  urlsource.Locator{URL: ureq.rawURL, SHA256: result.SHA}.String(),
		url:      ureq.rawURL,
		sha256:   result.SHA,
		version:  version,
		roleName: built.Meta.RoleName,
	}
	roleResult := gitsource.RoleResult{
		Cleanup:        built.Cleanup,
		Dependencies:   built.Meta.Dependencies,
		GalaxyRoleName: built.Meta.RoleName,
		ArtifactPath:   built.ArtifactPath,
		ArtifactSHA:    built.SHA256,
	}
	if err := storeRoleArtifact(ctx, deps, &pin, ureq.name, roleResult); err != nil {
		return rolePin{}, err
	}
	if policy.Write {
		deps.st.SetRolePin(ureq.pinKey, store.RolePinEntry{
			URL:            ureq.rawURL,
			SHA256:         result.SHA,
			Version:        version,
			GalaxyRoleName: built.Meta.RoleName,
			Deps:           sourceDepsToPin(built.Meta.Dependencies),
		})
	}
	return pin, nil
}

// repackURLRole loads the downloaded tarball as a role tree and rebuilds it
// into this tool's canonical role artifact, releasing the download and the
// extraction whatever the outcome: only the repacked artifact travels on.
func repackURLRole(ctx context.Context, deps collectionDeps, ureq urlRoleRequest, result downloadResult) (rolebuild.Built, error) {
	defer cleanupIfNeeded(result.Cleanup)
	tree, err := tartree.Load(ctx, result.Path, tempDirOf(deps))
	if err != nil {
		return rolebuild.Built{}, fmt.Errorf("%s: %w", ureq.display, err)
	}
	defer tree.Cleanup()
	if prefix := tree.SkippedPrefix(); prefix != "" {
		deps.runtime.Output.Debugf("Role %s: the archive wraps the role in %q; the prefix is not installed", ureq.name, prefix)
	}
	built, err := rolebuild.Build(ctx, tree, rolebuild.TempFileFunc(gitTempFile(deps)))
	if err != nil {
		return rolebuild.Built{}, fmt.Errorf("%s: %w", ureq.display, err)
	}
	return built, nil
}

// tempDirOf is the run's temp directory callback, with os.TempDir standing
// in when no runtime is wired (a test building deps by hand).
func tempDirOf(deps collectionDeps) func() string {
	if deps.runtime != nil && deps.runtime.TempDir != nil {
		return deps.runtime.TempDir
	}
	return func() string { return "" }
}

// urlRoleVersionLen is how much of the origin sha256 names a url role's
// default version label: the artifact-key fingerprint length, enough to
// tell two artifacts apart in a directory listing without dominating it.
const urlRoleVersionLen = helpers.ArtifactKeyFingerprintLen

// urlRoleVersion is the concrete version a url role installs as: the label
// the requirement asked for, else the leading urlRoleVersionLen hex digits
// of the origin sha256 - content-derived, stable across runs, and a legal
// role version.
func urlRoleVersion(requested, sha string) string {
	if requested != "" {
		return requested
	}
	if len(sha) < urlRoleVersionLen {
		return sha
	}
	return sha[:urlRoleVersionLen]
}

// urlRoleFetchToCache re-downloads a pinned url role's tarball and repacks
// it, the install-time counterpart of urlFetchToCache: reached after a
// --dry-run discovery, on a --frozen run whose cached artifact was evicted,
// or through a cache miss on a shared cache. The origin now serving bytes
// with a different sha256 than the pin is
// helpers.ErrURLArtifactSHA256Mismatch: the artifact this tool would install
// is a repack, so the refusal names the origin bytes the pin actually
// covers.
func urlRoleFetchToCache(ctx context.Context, deps installDeps, r resolvedRole, useCache bool) (downloadResult, error) {
	loc, err := urlsource.ParseLocator(r.Source)
	if err != nil {
		return downloadResult{}, err
	}
	if !loc.Pinned() {
		return downloadResult{}, fmt.Errorf("%w: role %s is not pinned to a sha256", helpers.ErrInvalidURLLocator, r.Name)
	}
	ureq := urlRoleRequest{name: r.Name, rawURL: loc.URL, display: helpers.URLForMessage(loc.URL)}
	result, err := downloadURLToTemp(ctx, deps.collectionDeps, loc.URL)
	if err != nil {
		return downloadResult{}, err
	}
	if result.SHA != loc.SHA256 {
		cleanupIfNeeded(result.Cleanup)
		return downloadResult{}, fmt.Errorf("%w: %s now serves sha256 %s for role %s, the pin records %s",
			helpers.ErrURLArtifactSHA256Mismatch, ureq.display, result.SHA, r.Name, loc.SHA256)
	}
	built, err := repackURLRole(ctx, deps.collectionDeps, ureq, result)
	if err != nil {
		return downloadResult{}, err
	}
	if !useCache || deps.artifacts == nil {
		return downloadResult{Path: built.ArtifactPath, SHA: built.SHA256, Cleanup: built.Cleanup}, nil
	}
	stored, err := commitDownload(ctx, deps.artifacts, roleArtifactKey(r), built.ArtifactPath, built.SHA256, built.Cleanup)
	if err != nil {
		return downloadResult{}, err
	}
	deps.runtime.Metrics.AddCacheMiss()
	return stored, nil
}
