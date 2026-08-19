package collections

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/psvmcc/hub/pkg/types"
)

// loadCollectionMetadata resolves and fetches metadata for a collection version.
func loadCollectionMetadata(
	ctx context.Context,
	deps collectionDeps,
	col collection,
) (*types.GalaxyCollectionVersionInfo, error) {
	cfg := deps.cfg
	runtime := deps.runtime
	st := deps.st

	version, exact, err := exactVersionFromConstraints([]string{col.Version})
	if err != nil {
		return nil, err
	}
	policy := cacheManager.PolicyForConstraint(cfg, exact)

	rootMetadata, base, err := loadRootMetadataCached(ctx, deps, col, policy)
	if err != nil {
		return nil, fmt.Errorf("failed to load root metadata: %w", err)
	}
	versionsURL, err := normalizeVersionsURL(base, rootMetadata.VersionsURL)
	if err != nil {
		return nil, err
	}
	if !strings.HasSuffix(versionsURL, "/") {
		versionsURL += "/"
	}
	// The server-supplied reference is quoted, the two derived values are
	// not: a URL this program built is its own, while rootMetadata.VersionsURL
	// is raw JSON a server chose and safeout.Clean keeps the one character it
	// would need to add a line of its own.
	//
	// Both values a server had a hand in are cut, and reaching this line is no
	// argument for leaving either one whole: checkMetadataURLUserinfo returns
	// nil for every value url.Parse itself refuses, so a credential arrives
	// here inside a value that guard never judged. helpers.WithoutCredentials
	// is textual and needs no successful parse, which is exactly what makes it
	// right for that value.
	//
	// base is not cut, and the operative half of why is that it is not a
	// server's answer: it is the winning server's own normalized base, so
	// nothing a server declared decides what it says. Its own authorship is
	// weaker than that - a source: in requirements.yml or in a lockfile is
	// repository content, not the operator's configuration - and the query a
	// source: may carry rides along uncut wherever this base is rendered.
	// apiRootCandidates (server_candidates.go) holds that gap.
	runtime.Output.Debugf("versions_url resolved: base=%s ref=%q -> %s",
		base, helpers.WithoutCredentials(rootMetadata.VersionsURL), helpers.WithoutCredentials(versionsURL))

	versionURL := rootMetadata.HighestVersion.Href

	if exact {
		versionURL = versionsURL + version + "/"
	}

	versionURL, err = normalizeVersionsURL(base, versionURL)
	if err != nil {
		return nil, err
	}
	var versionMetadataInfo types.GalaxyCollectionVersionInfo
	if err := fetchJSONWithCachePolicy(ctx, runtime, versionURL, st, &versionMetadataInfo, policy); err != nil {
		return nil, err
	}

	return &versionMetadataInfo, nil
}

// loadRootMetadataCached loads a collection's root metadata, walking the
// configured server list (serverCandidates) until one server answers: the
// first server whose root-metadata fetch succeeds owns col's fqdn for the
// rest of this run, and its (normalized, no trailing slash) base is
// returned alongside the metadata so callers can resolve every URL relative
// to the server that actually served it, not to a stale or empty
// col.Source.
//
// A pinned collection (col.Source non-empty) always has exactly one server
// candidate, so this degenerates to trying that one server's apiRoot
// variants. An unpinned collection walks every configured server in order:
// a 404 across all of one server's apiRoot candidates advances to the next
// server (see tryServerRootMetadata); a credential failure or an exhausted
// retry budget aborts immediately instead of falling through.
func loadRootMetadataCached(
	ctx context.Context,
	deps collectionDeps,
	col collection,
	policy cacheManager.Policy,
) (*types.GalaxyCollection, string, error) {
	var lastErr error
	for _, srv := range serverCandidates(deps, col) {
		meta, ok, err := tryServerRootMetadata(ctx, deps, col, policy, srv)
		if ok {
			return meta, srv.base, nil
		}
		if err == nil {
			continue
		}
		// A plain 404 exhausting every apiRoot candidate of this server means
		// only that this server does not have col, not that the server is
		// unreachable or misconfigured: remember it and advance to the next
		// server. Anything else - the auth/unavailable sentinels
		// tryServerRootMetadata already wrapped, or any other unclassified
		// error - is not something a different server could route around
		// (and, for auth/availability, must not be silently routed around),
		// so it aborts the whole walk immediately.
		if statusErr, ok := errors.AsType[*cacheManager.HTTPStatusError](err); ok && statusErr.Code == http.StatusNotFound {
			lastErr = err
			continue
		}
		return nil, "", err
	}
	if lastErr != nil {
		return nil, "", lastErr
	}
	return nil, "", helpers.ErrLoadMetadataFailed
}

// tryServerRootMetadata tries every apiRoot candidate of one server for
// col's root metadata. It reports (meta, true, nil) on success; (nil,
// false, nil) or (nil, false, a 404 *cacheManager.HTTPStatusError) when
// every candidate 404s (the caller advances to the next server, using the
// last 404 for its own final error if no server ever succeeds); and (nil,
// false, a non-nil error) for anything that must abort the whole walk: a
// 401/403 wrapped in helpers.ErrGalaxyAuthFailed, a retryable status
// (helpers.IsRetryableHTTPStatus) exhausted after fetchJSONWithCachePolicy's
// own retry budget wrapped in helpers.ErrGalaxyServerUnavailable, or any
// other error returned as-is. This is the only place that classifies a
// root-metadata fetch failure by HTTP status.
func tryServerRootMetadata(
	ctx context.Context,
	deps collectionDeps,
	col collection,
	policy cacheManager.Policy,
	srv serverCandidate,
) (*types.GalaxyCollection, bool, error) {
	runtime := deps.runtime
	st := deps.st

	var lastErr error
	candidates := rootMetadataURLCandidates(srv.base, col, deps.apiRoots)
	runtime.Output.Debugf("root metadata candidates for %s on server %s: %s", col.key(), srv.label(), joinCandidateURLs(candidates))

	for _, cand := range candidates {
		runtime.Output.Debugf("root metadata GET %s", cand.url)
		var root types.GalaxyCollection
		if err := fetchJSONWithCachePolicy(ctx, runtime, cand.url, st, &root, policy); err != nil {
			if statusErr, ok := errors.AsType[*cacheManager.HTTPStatusError](err); ok {
				switch {
				case statusErr.Code == http.StatusNotFound:
					runtime.Output.Debugf("root metadata 404 %s", cand.url)
					lastErr = err
					continue
				case statusErr.Code == http.StatusUnauthorized, statusErr.Code == http.StatusForbidden:
					return nil, false, fmt.Errorf("%w: server %s: %w", helpers.ErrGalaxyAuthFailed, srv.label(), err)
				case helpers.IsRetryableHTTPStatus(statusErr.Code):
					return nil, false, fmt.Errorf("%w: server %s: %w", helpers.ErrGalaxyServerUnavailable, srv.label(), err)
				}
			}
			return nil, false, err
		}
		runtime.Output.Debugf("root metadata OK %s", cand.url)
		// Record the winner only on success: a 404 here means only that this
		// collection is absent under this apiRoot, not that the apiRoot is
		// wrong, so a 404 must never blacklist an apiRoot for the server.
		// One benign, intended consequence of the memo: once a winner is
		// known, a later collection's 404 surfaces as the winner root's 404
		// rather than the last candidate's, since the losing candidates are
		// never probed again - same error class, fewer round trips.
		deps.apiRoots.recordWinner(cand.base, cand.apiRoot)
		return &root, true, nil
	}
	return nil, false, lastErr
}

// fetchVersionMetadataCached fetches metadata for a specific version.
func fetchVersionMetadataCached(
	ctx context.Context,
	deps collectionDeps,
	source string,
	versionsURL string,
	version string,
	policy cacheManager.Policy,
) (*types.GalaxyCollectionVersionInfo, error) {
	runtime := deps.runtime
	st := deps.st

	version = strings.TrimSpace(strings.TrimPrefix(version, "= "))
	base, err := normalizeVersionsURL(source, versionsURL)
	if err != nil {
		return nil, err
	}
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	versionURL := fmt.Sprintf("%s%s/", base, version)
	var info types.GalaxyCollectionVersionInfo
	if err := fetchJSONWithCachePolicy(ctx, runtime, versionURL, st, &info, policy); err != nil {
		return nil, err
	}
	return &info, nil
}

// normalizeVersionsURL resolves version URLs relative to a source, and refuses
// one embedding userinfo.
//
// It is the single chokepoint every server-chosen metadata reference passes
// through on the way to becoming a request URL: the versions_url a root
// metadata document declares, and the href its highest_version names - either
// arriving fresh from a server or replayed out of a poisonable cached
// snapshot. Naming them by role rather than by call site is deliberate; the
// property is that no such reference becomes a request URL without passing
// here, which a list of today's callers would stop describing the moment one
// is added.
//
// The refusal is on this function's OUTPUT rather than on versionsURL, so it
// is correct whichever input contributed the credential: a relative reference
// resolved against a source that carries one is refused exactly like an
// absolute reference carrying its own.
//
// Placement: this guard sits downstream of loadRootMetadataCached's
// status-routed candidate walk, by construction rather than by convention -
// no call to this function is inside that walk or inside tryServerRootMetadata
// - which is what leaves that walk's contract untouched, a 404 across one
// server's candidates advancing to the next server and a 401/403 or exhausted
// 5xx budget aborting the run. It must not be moved inside the walk: a refusal
// raised there would need an HTTP status meaning to be routed by, and there is
// no honest one - this failure is not a server answering anything, so calling
// it a 404 would silently advance to the next server and calling it a 401
// would abort the run naming a credential problem that does not exist.
//
// Exactly one shape stays outside the rule: a metadata URL naming a scheme
// this tool does not speak. Such a value is already fail-closed, by net/http's
// own "unsupported protocol scheme", so what is missing is a classified
// refusal rather than containment - the difference checkDownloadURL's own
// scheme allow-list exists to make on the download side. That allow-list is
// deliberately not mirrored here: the fallback paths below (resolveURL and
// joinURL beneath it) can yield shapes this refusal has not surveyed, and a
// guess about which of them are legitimate would refuse a real deployment's
// metadata rather than an attacker's.
func normalizeVersionsURL(source, versionsURL string) (string, error) {
	base := strings.TrimSpace(versionsURL)
	if after, ok := strings.CutPrefix(base, "https//"); ok {
		base = "https://" + after
	}
	if after, ok := strings.CutPrefix(base, "http//"); ok {
		base = "http://" + after
	}
	if !strings.HasPrefix(base, "https://") && !strings.HasPrefix(base, "http://") {
		base = resolveURL(source, base)
	}
	return base, checkMetadataURLUserinfo(base)
}

// checkMetadataURLUserinfo refuses a Galaxy metadata URL that embeds userinfo,
// naming the value through helpers.URLForMessage - the display form every
// refusal in this program renders a URL by, whichever boundary that URL entered
// through, and the one place the argument for its cuts lives.
//
// It refuses only a userinfo it can PROVE, which is why a value url.Parse
// itself refuses passes through rather than being refused, unlike
// checkDownloadURL's own parse arm. That pass-through is not a claim that such
// a value carries no credential - helpers.WithoutUserinfo's own doc comment
// says the opposite, that the values most needing the cut are exactly the ones
// url.Parse refuses, and names an invalid port as its example. It is a claim
// about this function's remit: refusing an unparseable value here would be the
// allow-list expansion normalizeVersionsURL scopes out above - a verdict about
// what a metadata URL may be at all, over shapes nothing here has surveyed.
//
// What the pass-through leaves behind is a rendering question rather than a
// containment one, and it is answered at the sinks: net/http refuses to build
// a request from a value it cannot parse, so nothing is fetched and no Basic
// credential is composed, while every line this program composes about that
// value cuts it through helpers.WithoutCredentials, which is textual and needs
// no successful parse. The refused build renders nothing either: the
// *url.Error net/http returns from it would name the whole value, credential
// included, so internal/galaxy/cache's fetchJSONBodyOnce drops that error for
// helpers.ErrMetadataRequestBuildFailed, which names the failure and no part
// of the value it refused - that sentinel holds the argument for replacing the
// message rather than cutting it.
func checkMetadataURLUserinfo(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User == nil {
		return nil
	}
	return fmt.Errorf("%w: %q", helpers.ErrMetadataURLUserinfo, helpers.URLForMessage(raw))
}
