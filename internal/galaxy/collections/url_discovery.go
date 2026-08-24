package collections

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/greeddj/go-galaxy/internal/galaxy/archive"
	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/collectionbuild"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/manifest"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/galaxy/urlsource"
)

// expandSourceRoots runs both source-kind expansions - git first, then url -
// and, when either actually expanded something, checks the fully expanded
// list for a fqdn two roots both produce. The check waits until here because
// an unexpanded root of the other kind has no identity yet: two url roots
// judged during the git expansion would collide on the empty fqdn.
func expandSourceRoots(ctx context.Context, deps collectionDeps, roots []collection) ([]collection, error) {
	expanding := anyUnpinnedGit(roots) || anyUnpinnedURL(roots)
	roots, err := expandGitRoots(ctx, deps, roots)
	if err != nil {
		return nil, err
	}
	roots, err = expandURLRoots(ctx, deps, roots)
	if err != nil {
		return nil, err
	}
	if expanding {
		return checkExpandedDuplicates(roots)
	}
	return roots, nil
}

// urlPin is what discovery learned about the collection a url requirement
// serves, in the shape the solver and the install phase consume: the pinned
// locator, the sha256 of the origin bytes, the exact version its
// MANIFEST.json declares, its validated dependency map, and - under
// --no-cache only - the downloaded artifact itself, handed to the install
// phase so the URL is not fetched a second time.
type urlPin struct {
	prebuilt *downloadResult
	deps     map[string]string
	locator  string
	sha256   string
	version  string
}

// urlDiscoveryMemo is the run-wide table of discovered url collections,
// keyed by fqdn: gitDiscoveryMemo's counterpart, created once per run
// (initInstall) and shared by the resolve, prefetch and install phases for
// the same reasons.
type urlDiscoveryMemo struct {
	pins map[string]urlPin
	mu   sync.Mutex
}

func newURLDiscoveryMemo() *urlDiscoveryMemo {
	return &urlDiscoveryMemo{pins: make(map[string]urlPin)}
}

func (m *urlDiscoveryMemo) put(fqdn string, pin urlPin) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pins[fqdn] = pin
}

// takePrebuilt hands out a --no-cache download exactly once: the install
// worker that takes it owns its cleanup from then on.
func (m *urlDiscoveryMemo) takePrebuilt(fqdn string) (downloadResult, bool) {
	if m == nil {
		return downloadResult{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	pin, ok := m.pins[fqdn]
	if !ok || pin.prebuilt == nil {
		return downloadResult{}, false
	}
	result := *pin.prebuilt
	pin.prebuilt = nil
	m.pins[fqdn] = pin
	return result, true
}

// snapshot returns a copy of the pin table for the solver provider.
func (m *urlDiscoveryMemo) snapshot() map[string]urlPin {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]urlPin, len(m.pins))
	for fqdn, pin := range m.pins {
		pin.prebuilt = nil
		out[fqdn] = pin
	}
	return out
}

// cleanup removes every --no-cache download nobody took.
func (m *urlDiscoveryMemo) cleanup() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for fqdn, pin := range m.pins {
		if pin.prebuilt != nil {
			cleanupIfNeeded(pin.prebuilt.Cleanup)
			pin.prebuilt = nil
			m.pins[fqdn] = pin
		}
	}
}

// expandURLRoots replaces every unpinned url root with the collection its
// tarball holds, leaving every other root untouched and in place. It runs
// immediately after expandGitRoots and is the only place a url origin is
// contacted during resolution.
//
// Per url root, in order: the recorded pin for the URL is replayed when the
// cache policy allows a read (the policy's TTL is deliberately ignored, as a
// git pin's is - a pin is keyed by the URL itself, so editing it is a new
// key, and it is invalidated by --refresh and --clear-cache, never by the
// clock); a miss under --offline is helpers.ErrOfflineMode; otherwise the
// tarball is downloaded, its identity and dependencies read out of its
// MANIFEST.json, the artifact committed to the artifact store (or kept as a
// temp file under --no-cache for the install phase, or discarded under
// --dry-run, which downloads to learn the identity and writes nothing), and
// the pin recorded when the policy allows a write. There is no cheaper
// refresh probe than the download itself - a URL has no advertisement - so
// --refresh simply re-downloads; unchanged bytes land on the same locator
// and the same artifact key.
//
// A version: the requirements entry asserted is checked against the
// manifest's on both the fresh and the replay path, so an edited assertion
// is judged on every run rather than only on the run that downloads.
//
// Roots are processed on the download-worker pool and merged in input
// order, so the expanded list is deterministic; the duplicate check over
// the fully expanded list runs in expandSourceRoots, once both source kinds
// have an identity.
func expandURLRoots(ctx context.Context, deps collectionDeps, roots []collection) ([]collection, error) {
	if !anyUnpinnedURL(roots) {
		return roots, nil
	}
	results := make([][]collection, len(roots))
	errs := make([]error, len(roots))
	var wg sync.WaitGroup
	sem := make(chan struct{}, max(deps.cfg.DownloadWorkers, 1))
	for i, root := range roots {
		if !root.isURL() || isPinnedURLLocator(root.Source) {
			results[i] = []collection{root}
			continue
		}
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			results[i], errs[i] = expandURLRoot(ctx, deps, root)
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	expanded := make([]collection, 0, len(roots))
	for _, group := range results {
		expanded = append(expanded, group...)
	}
	return expanded, nil
}

func anyUnpinnedURL(roots []collection) bool {
	for _, root := range roots {
		if root.isURL() && !isPinnedURLLocator(root.Source) {
			return true
		}
	}
	return false
}

func isPinnedURLLocator(source string) bool {
	loc, err := urlsource.ParseLocator(source)
	return err == nil && loc.Pinned()
}

// urlRootRequest is one unpinned url root taken apart: the canonical URL,
// the version the entry asserted ("" for none), the pin key and the display
// form for messages.
type urlRootRequest struct {
	rawURL    string
	requested string
	pinKey    string
	display   string
}

func newURLRootRequest(root collection) (urlRootRequest, error) {
	loc, err := root.urlLocator()
	if err != nil {
		return urlRootRequest{}, err
	}
	return urlRootRequest{
		rawURL:    loc.URL,
		requested: root.Constraint,
		pinKey:    urlsource.PinKey(loc.URL),
		display:   helpers.URLForMessage(loc.URL),
	}, nil
}

// expandURLRoot resolves one unpinned url root into its collection.
func expandURLRoot(ctx context.Context, deps collectionDeps, root collection) ([]collection, error) {
	req, err := newURLRootRequest(root)
	if err != nil {
		return nil, err
	}
	policy := cacheManager.PolicyForConstraint(deps.cfg, false)
	if policy.Read {
		if pin, ok := deps.st.GetURLPin(req.pinKey); ok {
			return replayURLPin(deps, req, pin)
		}
	}
	if deps.cfg != nil && deps.cfg.Offline {
		return nil, fmt.Errorf("%w: url source %s is not recorded in the cache", helpers.ErrOfflineMode, req.display)
	}
	return acquireURLRoot(ctx, deps, req, policy)
}

// replayURLPin turns a recorded pin into an expanded root, re-validating
// everything it carries: the pin is cache state, and cache state is judged
// on the way in exactly as an origin's answer is.
func replayURLPin(deps collectionDeps, req urlRootRequest, pin store.URLPinEntry) ([]collection, error) {
	if !helpers.IsSHA256Hex(pin.SHA256) {
		return nil, fmt.Errorf("%w: recorded pin for %s names sha256 %q", helpers.ErrInvalidURLLocator, req.display, pin.SHA256)
	}
	col, p, err := urlCollectionRoot(req, pin.SHA256, pin.Namespace, pin.Name, pin.Version, pin.Dependencies)
	if err != nil {
		return nil, err
	}
	deps.urlMemo.put(col.fqdn(), p)
	return []collection{col}, nil
}

// acquireURLRoot downloads the tarball, reads its identity, commits or keeps
// the artifact, records the pin, and returns the expanded root.
func acquireURLRoot(ctx context.Context, deps collectionDeps, req urlRootRequest, policy cacheManager.Policy) ([]collection, error) {
	result, err := downloadURLToTemp(ctx, deps, req.rawURL)
	if err != nil {
		return nil, err
	}
	manifestBytes, err := manifest.ReadFromTarGz(ctx, result.Path)
	if err != nil {
		cleanupIfNeeded(result.Cleanup)
		return nil, fmt.Errorf("%s: %w", req.display, err)
	}
	meta, err := collectionbuild.ParseManifestInfo(manifestBytes)
	if err != nil {
		cleanupIfNeeded(result.Cleanup)
		return nil, fmt.Errorf("%s: %w", req.display, err)
	}
	col, pin, err := urlCollectionRoot(req, result.SHA, meta.Namespace, meta.Name, meta.Version, meta.Dependencies)
	if err != nil {
		cleanupIfNeeded(result.Cleanup)
		return nil, err
	}
	entry := store.URLPinEntry{
		SHA256:       result.SHA,
		Namespace:    meta.Namespace,
		Name:         meta.Name,
		Version:      meta.Version,
		Dependencies: meta.Dependencies,
	}
	if err := storeURLArtifact(ctx, deps, col, result, &pin); err != nil {
		return nil, err
	}
	if policy.Write {
		deps.st.SetURLPin(req.pinKey, entry)
	}
	deps.urlMemo.put(col.fqdn(), pin)
	return []collection{col}, nil
}

// storeURLArtifact commits the downloaded artifact to the artifact store
// under its locator-scoped key, or hands it to the pin under --no-cache, or
// discards it under --dry-run - storeGitArtifacts' three-way switch for one
// artifact.
func storeURLArtifact(ctx context.Context, deps collectionDeps, col collection, result downloadResult, pin *urlPin) error {
	switch {
	case deps.cfg != nil && deps.cfg.DryRun:
		cleanupIfNeeded(result.Cleanup)
	case keepsPrebuilt(deps):
		pin.prebuilt = &downloadResult{Path: result.Path, SHA: result.SHA, Cleanup: result.Cleanup}
	default:
		key := helpers.ArtifactKey(col.Source, helpers.ArtifactFilename(col.Namespace, col.Name, col.Version))
		if _, err := commitDownload(ctx, deps.gitStore, key, result.Path, result.SHA, result.Cleanup); err != nil {
			return fmt.Errorf("committing url artifact %s: %w", col.key(), err)
		}
		deps.runtime.Metrics.AddCacheMiss()
	}
	return nil
}

// urlCollectionRoot validates the discovered collection's identity and
// dependencies and returns it as an exact-pin root together with the pin the
// solver will answer from. The identity came from the artifact's own
// MANIFEST.json or from a cached pin, and both are judged by the same
// predicates a Galaxy server's answer is; the version the requirements entry
// asserted, when it made one, must be the version the artifact is built as.
func urlCollectionRoot(req urlRootRequest, sha, namespace, name, version string,
	rawDeps map[string]string,
) (collection, urlPin, error) {
	// The relaxed alphabet, not the Galaxy one: the manifest is authored
	// outside any Galaxy server, and real artifacts carry mixed-case
	// namespaces ansible installs. See helpers.IsURLCollectionNamePart.
	if !helpers.IsURLCollectionNamePart(namespace) || !helpers.IsURLCollectionNamePart(name) {
		return collection{}, urlPin{}, fmt.Errorf("%w: %s declares %q.%q",
			helpers.ErrInvalidCollectionName, req.display, namespace, name)
	}
	if !helpers.IsExactVersion(version) {
		return collection{}, urlPin{}, fmt.Errorf("%w: %s declares %s.%s version %q",
			helpers.ErrGitCollectionVersionNotExact, req.display, namespace, name, version)
	}
	if req.requested != "" && req.requested != version {
		return collection{}, urlPin{}, fmt.Errorf("%w: %s is built as %s.%s@%s, not %s",
			helpers.ErrURLCollectionVersionMismatch, req.display, namespace, name, version, req.requested)
	}
	parsedDeps, err := parseDependencies(rawDeps)
	if err != nil {
		return collection{}, urlPin{}, fmt.Errorf("%s.%s from %s: %w", namespace, name, req.display, err)
	}
	locator := urlsource.Locator{URL: req.rawURL, SHA256: sha}.String()
	pin := urlPin{locator: locator, sha256: sha, version: version, deps: parsedDeps}
	return collection{
		Namespace:  namespace,
		Name:       name,
		Version:    version,
		Source:     locator,
		Constraint: version,
		Type:       typeURL,
		SHA256:     sha,
	}, pin, nil
}

// downloadURLToTemp downloads rawURL over the run's url client into a temp
// file under the artifact store, hashing while streaming and probing the
// result's tar.gz shape, with the retry policy and single
// ArtifactDownloadDeadline budget downloadCollectionToCache applies to a
// Galaxy download. It commits nothing: the caller decides what the bytes
// become once their identity is known.
func downloadURLToTemp(ctx context.Context, deps collectionDeps, rawURL string) (downloadResult, error) {
	if err := checkDownloadURL(rawURL); err != nil {
		return downloadResult{}, err
	}
	runtime := deps.runtime
	if runtime == nil || runtime.URLHTTP == nil {
		return downloadResult{}, fmt.Errorf("%w: no url download client is wired into this run", helpers.ErrConfigIsNil)
	}
	if strings.HasPrefix(strings.ToLower(rawURL), "http://") {
		// Warnf, not Printf: the plaintext transport of the very bytes every
		// later sha256 check descends from must survive --quiet in CI.
		runtime.Output.Warnf("Downloading %s over plaintext http", helpers.WithoutCredentials(rawURL))
	}
	budget := runtime.ArtifactDeadline()
	dlCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	var result downloadResult
	err := helpers.Retry(dlCtx, helpers.FetchRetryPolicy(), func() error {
		attempted, attemptErr := attemptURLDownload(dlCtx, deps, rawURL)
		if attemptErr != nil {
			return artifactDeadlineError(ctx, dlCtx, budget, attemptErr)
		}
		result = attempted
		return nil
	}, downloadRetryable)
	if err != nil {
		return downloadResult{}, artifactDeadlineError(ctx, dlCtx, budget, err)
	}
	return result, nil
}

// attemptURLDownload performs one full download attempt: a fresh GET over
// the url client, streamed to a fresh temp file under the size cap with the
// sha256 computed on the way, then the tar.gz shape probe. Every failure
// path cleans up what it created, so a retried attempt never leaks a temp
// file. The error shapes mirror downloadCollection's exactly, so
// downloadRetryable and the exit classifier read both paths the same.
func attemptURLDownload(ctx context.Context, deps collectionDeps, rawURL string) (downloadResult, error) {
	runtime := deps.runtime
	runtime.Output.Printf("Downloading %s", helpers.WithoutCredentials(rawURL))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, http.NoBody)
	if err != nil {
		return downloadResult{}, err
	}
	resp, err := runtime.URLHTTP.Do(req)
	if err != nil {
		// helpers.CutTransportURL for the reason downloadCollection gives: a
		// url source is routinely a presigned or token-parameterized URL,
		// and net/http's own masking leaves the query intact.
		return downloadResult{}, &downloadAttemptError{err: helpers.CutTransportURL(rawURL, err)}
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return downloadResult{}, &downloadAttemptError{
			err:    fmt.Errorf("%w: %s (%s)", helpers.ErrDownloadFailed, helpers.WithoutCredentials(rawURL), resp.Status),
			status: resp.StatusCode,
		}
	}
	tmpPath, cleanup, sha, err := writeURLBodyToTemp(ctx, deps, resp.Body)
	if err != nil {
		cleanupIfNeeded(cleanup)
		return downloadResult{}, err
	}
	if err := archive.ProbeTarGz(ctx, tmpPath); err != nil {
		cleanupIfNeeded(cleanup)
		return downloadResult{}, err
	}
	return downloadResult{Path: tmpPath, SHA: sha, Cleanup: cleanup}, nil
}

// writeURLBodyToTemp is writeDownloadToTemp over the discovery-phase deps: a
// temp file from gitTempFile (the artifact store's own TempFile when a store
// is at hand), the download size cap, and the sha256 computed while
// streaming.
func writeURLBodyToTemp(ctx context.Context, deps collectionDeps, body io.Reader) (string, func(), string, error) {
	tmpFile, cleanup, err := gitTempFile(deps)(ctx)
	if err != nil {
		return "", cleanup, "", err
	}
	hasher := sha256.New()
	writer := io.MultiWriter(tmpFile, hasher)
	limited := helpers.NewSizeLimitedReader(body, helpers.ArtifactMaxDownloadSize)
	n, err := io.Copy(writer, limited)
	deps.runtime.Metrics.AddBytesDownloaded(n)
	if err != nil {
		_ = tmpFile.Close()
		return "", cleanup, "", fmt.Errorf("collection artifact download: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return "", cleanup, "", err
	}
	return tmpFile.Name(), cleanup, hex.EncodeToString(hasher.Sum(nil)), nil
}
