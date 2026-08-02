package collections

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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
	policy := cachePolicyForConstraint(cfg, exact)

	rootMetadata, base, err := loadRootMetadataCached(ctx, deps, col, policy)
	if err != nil {
		return nil, fmt.Errorf("failed to load root metadata: %w", err)
	}
	versionsURL := normalizeVersionsURL(base, rootMetadata.VersionsURL)
	if !strings.HasSuffix(versionsURL, "/") {
		versionsURL += "/"
	}
	// The server-supplied reference is quoted, the two derived values are
	// not: a URL this program built is its own, while rootMetadata.VersionsURL
	// is raw JSON a server chose and safeout.Clean keeps the one character it
	// would need to add a line of its own.
	runtime.Output.Debugf("versions_url resolved: base=%s ref=%q -> %s", base, rootMetadata.VersionsURL, versionsURL)

	versionURL := rootMetadata.HighestVersion.Href

	if exact {
		versionURL = versionsURL + version + "/"
	}

	versionURL = normalizeVersionsURL(base, versionURL)
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
	for _, srv := range serverCandidates(deps.cfg, col) {
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
		var statusErr *cacheManager.HTTPStatusError
		if errors.As(err, &statusErr) && statusErr.Code == http.StatusNotFound {
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
			var statusErr *cacheManager.HTTPStatusError
			if errors.As(err, &statusErr) {
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
		// One benign, intended behavior change: once a winner is known, a
		// later collection's 404 surfaces as the winner root's 404 rather
		// than the last candidate's, since the losing candidates are no
		// longer probed - same error class, fewer round trips.
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
	base := normalizeVersionsURL(source, versionsURL)
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	url := fmt.Sprintf("%s%s/", base, version)
	var info types.GalaxyCollectionVersionInfo
	if err := fetchJSONWithCachePolicy(ctx, runtime, url, st, &info, policy); err != nil {
		return nil, err
	}
	return &info, nil
}

// normalizeVersionsURL resolves version URLs relative to a source.
func normalizeVersionsURL(source, versionsURL string) string {
	base := strings.TrimSpace(versionsURL)
	if after, ok := strings.CutPrefix(base, "https//"); ok {
		base = "https://" + after
	}
	if after, ok := strings.CutPrefix(base, "http//"); ok {
		base = "http://" + after
	}
	if strings.HasPrefix(base, "https://") || strings.HasPrefix(base, "http://") {
		return base
	}
	return resolveURL(source, base)
}
