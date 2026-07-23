package collections

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/archive"
	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/psvmcc/hub/pkg/types"
)

const versionLimit = 100

// maxVersionPages bounds how many offset-paginated requests
// loadVersionsListCached will issue for a single versions list (at
// versionLimit entries per page, roughly 10,000 versions) before it gives up
// and fails hard, rather than trusting an upstream server that keeps
// reporting more pages forever.
const maxVersionPages = 100

// installCollection downloads, extracts, and records a collection install.
func installCollection(
	ctx context.Context,
	col collection,
	deps installDeps,
	resolvedDeps []string,
	metaOverride *types.GalaxyCollectionVersionInfo,
) error {
	cfg := deps.cfg
	runtime := deps.runtime
	st := deps.st

	installStart := time.Now()
	defer func() {
		runtime.Output.DebugSincef(installStart, "%s", col.key())
	}()

	filename := fmt.Sprintf("%s-%s-%s.tar.gz", col.Namespace, col.Name, col.Version)
	installPath := filepath.Join(cfg.DownloadPath, "ansible_collections", col.Namespace, col.Name)

	if canSkipInstall(cfg, col, installPath, st) {
		runtime.Output.Printf("⏭️ Skipping install, already installed: %s/%s/%s", col.Namespace, col.Name, col.Version)
		return nil
	}

	payload, err := prepareAndExtract(ctx, deps, col, metaOverride, filename, installPath)
	if err != nil {
		return err
	}
	// The winning payload (the one that made it past verify+extract) is
	// cleaned up here by the caller; any losing attempt along the way was
	// already cleaned inside prepareAndExtract, so there is neither a double
	// cleanup nor a leaked temp file.
	if payload.artifact.Cleanup != nil {
		defer payload.artifact.Cleanup()
	}

	depsList, err := resolveDependencies(ctx, installPath, deps, resolvedDeps, col, filename)
	if err != nil {
		return err
	}
	writeGalaxyInfoIfPresent(runtime, cfg, col, payload.meta)
	recordInstall(st, col, installPath, payload.artifactSHA, depsList)
	return nil
}

// verifyPinnedSHA enforces a lockfile SHA256 pin against the resolved
// artifact hash. An empty pin means the lockfile recorded no checksum for
// this collection, so the check is a no-op, keeping older lockfiles usable.
// A non-empty pin that does not match the actual artifact hash fails the
// install so a frozen run can never install drifted bytes.
func verifyPinnedSHA(col collection, actual string) error {
	expected := strings.TrimSpace(col.SHA256)
	if expected == "" {
		return nil
	}
	got := strings.TrimSpace(actual)
	if expected == got {
		return nil
	}
	return fmt.Errorf("%w: %s: locked %s != actual %s", helpers.ErrSHA256Mismatch, col.key(), expected, got)
}

type installPayload struct {
	meta        *types.GalaxyCollectionVersionInfo
	artifact    artifactData
	artifactSHA string
}

type artifactData struct {
	Meta    map[string]string
	Cleanup func()
	Path    string
	SHA     string
}

// isCacheHit reports whether col's artifact can be served straight from the
// artifact cache: forceDownload was not requested (the corruption recovery
// path forces a fresh download rather than trusting a hit that may still be
// the very bytes that were just evicted), caching is enabled, an artifact
// store is configured, and that store actually reports the key present.
// Factored out of prepareInstall to keep its own branching under the
// cyclomatic complexity budget.
func isCacheHit(ctx context.Context, deps installDeps, col collection, forceDownload bool) bool {
	if forceDownload || deps.cfg.NoCache || deps.artifacts == nil {
		return false
	}
	return artifactExists(ctx, deps.artifacts, col)
}

// prepareInstall resolves metadata (if needed) and produces an installable
// artifact for col, either from the artifact cache or by downloading. The
// returned bool reports whether the artifact actually came from a cache hit
// (servedFromCache), which the caller uses to decide whether a subsequent
// verify/extract failure is worth one bounded eviction-and-refetch attempt.
//
// forceDownload skips the cache-hit check entirely, forcing a fresh download
// regardless of what Has() would report. This is used by the corruption
// recovery path after an eviction: rather than relying on Delete having
// already physically removed the file (a backend detail this layer should
// not depend on) or paying for a second Has() syscall on the hot no-eviction
// path, the caller who just evicted simply says so directly.
func prepareInstall(
	ctx context.Context,
	deps installDeps,
	col collection,
	metaOverride *types.GalaxyCollectionVersionInfo,
	filename string,
	forceDownload bool,
) (installPayload, bool, error) {
	cfg := deps.cfg
	runtime := deps.runtime

	meta := metaOverride
	useCache := !cfg.NoCache
	cacheHit := isCacheHit(ctx, deps, col, forceDownload)

	// Fast path: artifact is already in cache and the caller did not push
	// metadata. We have everything required to install - namespace/name/version
	// from col, SHA from a sidecar (local or S3) or, failing that, by hashing
	// the file - so we can skip the metadata roundtrip entirely.
	if cacheHit && meta == nil {
		runtime.Output.Printf("📦 Using cached %s", filename)
		payload, err := prepareFromCache(ctx, deps, col)
		return payload, true, err
	}

	meta, err := resolveMetadata(ctx, deps.collectionDeps, col, meta, cacheHit)
	if err != nil && !errors.Is(err, helpers.ErrMetadataUnavailable) {
		return installPayload{}, cacheHit, err
	}

	artifact, err := fetchArtifact(ctx, deps, col, meta, cacheHit, useCache)
	if err != nil {
		return installPayload{}, cacheHit, err
	}
	artifactSHA, err := resolveArtifactSHA(artifact.Path, meta, artifact.Meta, artifact.SHA, col.SHA256)
	if err != nil {
		if artifact.Cleanup != nil {
			artifact.Cleanup()
		}
		return installPayload{}, cacheHit, err
	}
	return installPayload{meta: meta, artifact: artifact, artifactSHA: artifactSHA}, cacheHit, nil
}

// verifyAndExtract enforces col's lockfile SHA256 pin (if any) against
// payload's resolved artifact hash and then extracts the artifact into
// installPath. This is the verify+extract step installCollection always ran
// inline; it is factored out so prepareAndExtract can run it a second time
// against a freshly downloaded replacement after evicting a corrupt cache
// hit.
func verifyAndExtract(_ context.Context, deps installDeps, col collection, payload installPayload, installPath, filename string) error {
	if err := verifyPinnedSHA(col, payload.artifactSHA); err != nil {
		return err
	}
	extractStart := time.Now()
	err := extractCollection(col, payload.artifact.Path, installPath, deps.runtime, deps.extractStore, payload.artifactSHA)
	if err != nil {
		return fmt.Errorf("failed to extract %s: %w", filename, err)
	}
	deps.runtime.Output.DebugSincef(extractStart, "%s", "extract "+col.key())
	return nil
}

// prepareWithRecovery prepares an installable artifact for col and runs
// action against it, with one bounded eviction-and-refetch retry when a
// cache-hit artifact turns out to be corrupt. Without this, a single bad
// cached tarball - wrong hash, bytes that fail to extract, or bytes that no
// longer hash to the sha they are keyed under - would fail every future use
// of that collection forever, since nothing else would ever remove it.
// "Once" is enforced structurally, not by a counter: eviction unconditionally
// sets forceDownload to true, and the next iteration's guard (forceDownload
// is now true) returns on any failure instead of evicting again, so this
// loop can run at most twice and never recurses.
//
// The eviction itself deletes ONLY the cached tarball and its sha256
// sidecar (via the ArtifactStore.Delete interface method) and deliberately
// never touches deps.extractStore. Deleting the tarball already removes the
// only precondition for the extracted-store invariant this would otherwise
// risk breaking: with no tarball on disk, a later cache hit has nothing left
// to extract against the (already-trusted) sidecar, so the forbidden
// "tarball present, CAS entry absent" state is never produced. When an
// extracted store is configured, the forced refetch below repopulates both
// stores consistently: the CAS tree via the ingest/promote pipeline and the
// tarball+sidecar via Commit, both keyed by the freshly hashed sha. A CAS
// tree is itself content-addressable state shared across every project that
// happens to reference that sha, and remains correct for all of them, so
// deleting it because one project's cache hit was corrupt would destroy
// correct content the rest of the fleet still needs. It would also be
// pointless: a failed extraction never leaves a ready CAS tree behind in the
// first place, since extractInto/Ensure clean up their own tmp directory on
// failure and leave any already-finalized final path untouched.
//
// Eviction is skipped, and the failure surfaces as-is, whenever retrying
// would not help or would be destructive: this is already the retry
// (forceDownload is set), the artifact never came from a cache hit in the
// first place (so nothing stale to evict caused this), or the run is
// offline, where deleting the only local copy with no way to refetch it
// would be pure data loss for no benefit.
//
// Accepted tradeoff: every cache-hit action failure - regardless of cause -
// spends exactly one evict-and-refetch attempt with no classification of
// *why* it failed. A rare transient failure therefore costs one wasted
// refetch of otherwise-good bytes; that cost is bounded to a single retry and
// the end result is still correct. The one exception is the prepareInstall
// failure arm below, which does classify: only helpers.ErrSHA256Mismatch (the
// S3 backend's read-time integrity check) is worth retrying there, since
// every other prepareInstall failure (a metadata error, a download error, an
// offline miss) would just reproduce identically against the same cache
// entry or the same origin.
//
// canRetryCacheHit and evictCorruptCachedArtifact are factored out so both
// recovery arms below share one guard and one log line - kept out of this
// function's own body to stay under its cyclomatic complexity budget.
func prepareWithRecovery(
	ctx context.Context,
	deps installDeps,
	col collection,
	metaOverride *types.GalaxyCollectionVersionInfo,
	filename string,
	action func(installPayload) error,
) (installPayload, error) {
	forceDownload := false
	for {
		payload, fromCache, err := prepareInstall(ctx, deps, col, metaOverride, filename, forceDownload)
		if err != nil {
			// A cache-resident integrity failure surfaced by prepareInstall
			// itself - the S3 backend verifies a fetched object's sha256 on
			// read - is recovered once by evicting the object and refetching
			// from the origin. The local backend does not sha-check on read,
			// so this arm never fires for it.
			if canRetryCacheHit(deps, fromCache, forceDownload) && errors.Is(err, helpers.ErrSHA256Mismatch) {
				evictCorruptCachedArtifact(ctx, deps, col, filename, err)
				forceDownload = true
				continue
			}
			return installPayload{}, err
		}
		actionErr := action(payload)
		if actionErr == nil {
			return payload, nil
		}
		if payload.artifact.Cleanup != nil {
			payload.artifact.Cleanup()
		}
		if !canRetryCacheHit(deps, fromCache, forceDownload) {
			return installPayload{}, actionErr
		}
		evictCorruptCachedArtifact(ctx, deps, col, filename, actionErr)
		forceDownload = true
	}
}

// canRetryCacheHit reports whether prepareWithRecovery may spend its one
// bounded eviction-and-refetch attempt: only for a genuine cache-hit artifact
// (fromCache), not on what is already the retry (forceDownload), and only
// when the run can actually reach the origin to replace it (not
// deps.cfg.Offline) - offline, deleting the only local copy with nothing to
// refetch it from would be pure data loss for no benefit.
func canRetryCacheHit(deps installDeps, fromCache, forceDownload bool) bool {
	return fromCache && !forceDownload && !deps.cfg.Offline
}

// evictCorruptCachedArtifact logs and deletes col's cached artifact ahead of
// a forced refetch. Both of prepareWithRecovery's recovery arms - a
// prepareInstall-level integrity failure and an action-level one - call this
// so the log line reads identically regardless of which one fired.
func evictCorruptCachedArtifact(ctx context.Context, deps installDeps, col collection, filename string, cause error) {
	deps.runtime.Output.Printf("♻️ Evicting corrupt cached %s and refetching: %v", filename, cause)
	if deps.artifacts != nil {
		_ = deps.artifacts.Delete(ctx, artifactKey(col))
	}
}

// prepareAndExtract prepares an installable artifact for col and verifies +
// extracts it into installPath, delegating the bounded eviction-and-refetch
// retry to prepareWithRecovery.
func prepareAndExtract(
	ctx context.Context,
	deps installDeps,
	col collection,
	metaOverride *types.GalaxyCollectionVersionInfo,
	filename, installPath string,
) (installPayload, error) {
	return prepareWithRecovery(ctx, deps, col, metaOverride, filename, func(payload installPayload) error {
		return verifyAndExtract(ctx, deps, col, payload, installPath, filename)
	})
}

func prepareFromCache(ctx context.Context, deps installDeps, col collection) (installPayload, error) {
	artifact, err := fetchArtifact(ctx, deps, col, nil, true, true)
	if err != nil {
		return installPayload{}, err
	}
	artifactSHA, err := resolveArtifactSHA(artifact.Path, nil, artifact.Meta, artifact.SHA, col.SHA256)
	if err != nil {
		if artifact.Cleanup != nil {
			artifact.Cleanup()
		}
		return installPayload{}, err
	}
	return installPayload{meta: nil, artifact: artifact, artifactSHA: artifactSHA}, nil
}

func resolveDependencies(
	ctx context.Context,
	installPath string,
	deps installDeps,
	resolvedDeps []string,
	col collection,
	filename string,
) ([]string, error) {
	cfg := deps.cfg
	runtime := deps.runtime

	if resolvedDeps != nil || cfg.NoDeps {
		return resolvedDeps, nil
	}
	depsStart := time.Now()
	depsList, err := installDependencies(ctx, installPath, deps)
	if err != nil {
		return nil, fmt.Errorf("failed to install dependencies for %s: %w", filename, err)
	}
	runtime.Output.DebugSincef(depsStart, "%s", "deps "+col.key())
	return depsList, nil
}

func writeGalaxyInfoIfPresent(
	runtime *infra.Infra,
	cfg *config.Config,
	col collection,
	meta *types.GalaxyCollectionVersionInfo,
) {
	if err := writeGalaxyInfo(cfg, col, meta); err != nil {
		runtime.Output.Printf("⚠️ Failed to write GALAXY.yml: %v", err)
	}
}

func recordInstall(st *store.Store, col collection, installPath, artifactSHA string, deps []string) {
	if st == nil {
		return
	}
	st.SetInstalled(col.key(), installedEntry{
		InstallPath:    installPath,
		Source:         col.Source,
		ArtifactSHA256: artifactSHA,
		InstalledAt:    time.Now().UTC(),
		Deps:           deps,
	})
	if deps != nil {
		st.SetGraph(col.key(), deps)
	}
}

func artifactExists(ctx context.Context, artifacts cacheManager.ArtifactStore, col collection) bool {
	ok, err := artifacts.Has(ctx, artifactKey(col))
	return err == nil && ok
}

func fetchArtifact(
	ctx context.Context,
	deps installDeps,
	col collection,
	meta *types.GalaxyCollectionVersionInfo,
	cacheHit bool,
	useCache bool,
) (artifactData, error) {
	runtime := deps.runtime
	artifacts := deps.artifacts

	if !cacheHit {
		if deps.cfg != nil && deps.cfg.Offline {
			return artifactData{}, fmt.Errorf("%w: artifact %s not in cache", helpers.ErrOfflineMode, col.key())
		}
		downloadStart := time.Now()
		result, err := downloadCollectionToCache(ctx, deps, artifactKey(col), meta, useCache)
		if err != nil {
			return artifactData{}, err
		}
		runtime.Output.DebugSincef(downloadStart, "%s", "download "+col.key())
		return artifactData{Path: result.Path, Cleanup: result.Cleanup, SHA: result.SHA}, nil
	}
	if artifacts == nil {
		return artifactData{}, helpers.ErrArtifactCacheNotConfigured
	}
	cached, err := artifacts.Fetch(ctx, artifactKey(col))
	if err != nil {
		return artifactData{}, err
	}
	return artifactData{Path: cached.Path, Cleanup: cached.Cleanup, Meta: cached.Meta}, nil
}

// resolveArtifactSHA determines the sha256 to record for an installed
// artifact, preferring cheaper sources but never trusting an unverified one
// when the lockfile pins an exact hash. artifactSHA (set only on a fresh
// download) is always a hash of the actual bytes just streamed, so it is
// authoritative regardless of pin. Everything else - a cache hit's recorded
// metadata or its sidecar - was written by an earlier process and never
// re-verified against the bytes on disk today, so a non-empty pin skips
// straight to hashing the real tarball: trusting a recorded value here would
// let drifted-on-disk bytes slip past a frozen install by coincidentally
// matching a stale pin. Without a pin, the cheaper recorded sources are used
// in order, falling back to hashing the file only when none are available.
func resolveArtifactSHA(
	path string,
	meta *types.GalaxyCollectionVersionInfo,
	artifactMeta map[string]string,
	artifactSHA string,
	pin string,
) (string, error) {
	if sha := strings.TrimSpace(artifactSHA); sha != "" {
		return sha, nil
	}
	if strings.TrimSpace(pin) != "" {
		return archive.FileHashSHA256(path)
	}
	if meta != nil {
		if sha := strings.TrimSpace(meta.Artifact.Sha256); sha != "" {
			return sha, nil
		}
	}
	if artifactMeta != nil {
		if sha := strings.TrimSpace(artifactMeta["sha256"]); sha != "" {
			return sha, nil
		}
	}
	return archive.FileHashSHA256(path)
}

// artifactKey builds the cache key for a collection tarball.
func artifactKey(col collection) string {
	filename := fmt.Sprintf("%s-%s-%s.tar.gz", col.Namespace, col.Name, col.Version)
	return url.QueryEscape(filename)
}

// installEntryMatches reports whether a recorded install entry still points
// at installPath with a known artifact hash consistent with any lockfile pin
// on col. An empty pin allows any recorded hash, so non-frozen installs keep
// their prior skip behavior unchanged.
func installEntryMatches(col collection, entry store.InstalledEntry, installPath string) bool {
	if entry.InstallPath == "" || entry.InstallPath != installPath {
		return false
	}
	if entry.ArtifactSHA256 == "" {
		return false
	}
	if col.SHA256 != "" && strings.TrimSpace(entry.ArtifactSHA256) != strings.TrimSpace(col.SHA256) {
		return false
	}
	return true
}

// canSkipInstall reports whether a collection is already installed.
func canSkipInstall(cfg *config.Config, col collection, installPath string, st *store.Store) bool {
	if cfg == nil || st == nil {
		return false
	}
	entry, ok := st.GetInstalled(col.key())
	if !ok || !installEntryMatches(col, entry, installPath) {
		return false
	}

	marker := filepath.Join(installPath, ".extract-done."+entry.ArtifactSHA256)
	if _, err := os.Stat(marker); err != nil {
		return false
	}

	infoDir := filepath.Join(cfg.DownloadPath, "ansible_collections", fmt.Sprintf("%s.%s-%s.info", col.Namespace, col.Name, col.Version))
	if _, err := os.Stat(filepath.Join(infoDir, "GALAXY.yml")); err != nil {
		return false
	}

	return true
}

// downloadCollection performs a single attempt at fetching an artifact and
// returns the HTTP response. It never retries itself: a transport-level
// failure (client.Do returning an error before any response arrives) and a
// non-200 status are both wrapped in a *downloadAttemptError so the caller's
// outer helpers.Retry loop - which re-runs this whole establish step
// together with the stream-to-temp and sha verify that follow it - can
// classify whether the failure is worth repeating.
func downloadCollection(ctx context.Context, runtime *infra.Infra, collectionURL string) (*http.Response, error) {
	runtime.Output.Printf("🌐 Downloading %s", collectionURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, collectionURL, http.NoBody)
	if err != nil {
		return nil, err
	}
	resp, err := runtime.HTTP.Do(req)
	if err != nil {
		return nil, &downloadAttemptError{err: err}
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, &downloadAttemptError{
			err:    fmt.Errorf("%w: %s (%s)", helpers.ErrDownloadFailed, collectionURL, resp.Status),
			status: resp.StatusCode,
		}
	}
	return resp, nil
}

// downloadResult describes a downloaded artifact file and metadata.
type downloadResult struct {
	Cleanup func()
	Path    string
	SHA     string
}

// downloadCollectionToCache downloads an artifact and optionally stores it,
// retrying the whole establish+stream-to-temp+verify attempt (per
// downloadRetryable) up to helpers.FetchRetryPolicy's bound. Each retried
// attempt opens a fresh HTTP response and a fresh temporary file (or
// extraction session), via attemptDownloadToCache, so nothing from a failed
// attempt - including any temp file it created - survives into the next one.
func downloadCollectionToCache(
	ctx context.Context,
	deps installDeps,
	key string,
	meta *types.GalaxyCollectionVersionInfo,
	useCache bool,
) (downloadResult, error) {
	if err := validateDownloadInputs(deps.cfg, deps.artifacts, meta); err != nil {
		return downloadResult{}, err
	}

	var result downloadResult
	err := helpers.Retry(ctx, helpers.FetchRetryPolicy(), func() error {
		attempted, attemptErr := attemptDownloadToCache(ctx, deps, key, meta, useCache)
		if attemptErr != nil {
			return attemptErr
		}
		result = attempted
		return nil
	}, downloadRetryable)
	if err != nil {
		return downloadResult{}, err
	}
	return result, nil
}

// warnIfOffServerDownloadHost emits a warning when an artifact's download URL
// points at a host other than the configured Galaxy server. The metadata that
// supplies the URL can come from a cached snapshot, which a bucket writer
// could poison to redirect a download off-server; surfacing the mismatch
// gives a visible signal in CI logs without blocking, since a legitimate
// deployment (an enterprise content host or an object-storage URL) may serve
// downloads from a different host than its API server. A missing server or
// an unparseable URL is not warned about, to avoid false alarms. The warning
// goes out through Warnf rather than Printf: this is a security/integrity
// detection signal, so it must survive --quiet (Warnf always emits, to
// stderr, unlike the transient Printf tier) rather than risk being silenced
// in the very CI mode where it matters most.
func warnIfOffServerDownloadHost(runtime *infra.Infra, cfg *config.Config, downloadURL string) {
	if cfg == nil || strings.TrimSpace(cfg.Server) == "" {
		return
	}
	server, err := url.Parse(cfg.Server)
	if err != nil {
		return
	}
	dl, err := url.Parse(downloadURL)
	if err != nil {
		return
	}
	serverHost := strings.ToLower(server.Hostname())
	dlHost := strings.ToLower(dl.Hostname())
	if serverHost == "" || dlHost == "" || serverHost == dlHost {
		return
	}
	runtime.Output.Warnf(
		"Downloading %s from host %q, which differs from the configured server host %q",
		downloadURL, dlHost, serverHost,
	)
}

// attemptDownloadToCache performs one full download attempt: it opens a
// fresh HTTP response via downloadCollection, then either tees it through
// streamDownloadAndExtract (when an extracted store is configured) or writes
// it to a fresh temp file and verifies its sha256 directly. Every failure
// path below already cleans up whatever it created, so a caller retrying a
// failed attempt never leaks a temp file or partial extraction across
// attempts. When deps.extractStore is configured, the body is teed into a
// sha256 hasher, the artifact tmp file, and a tar.gz extraction pipeline
// that populates the content-addressable extracted store, all in a single
// pass.
//
// warnIfOffServerDownloadHost runs here, ahead of the actual GET: this is the
// single point every artifact download passes through, whether reached from
// the prefetcher or from the main install path's fetchArtifact, and whether
// meta came in fresh from the API or from a (poisonable) cached snapshot.
func attemptDownloadToCache(
	ctx context.Context,
	deps installDeps,
	key string,
	meta *types.GalaxyCollectionVersionInfo,
	useCache bool,
) (downloadResult, error) {
	warnIfOffServerDownloadHost(deps.runtime, deps.cfg, meta.DownloadURL)
	resp, err := downloadCollection(ctx, deps.runtime, meta.DownloadURL)
	if err != nil {
		return downloadResult{}, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if deps.extractStore != nil {
		return streamDownloadAndExtract(ctx, deps, key, meta, resp.Body, useCache)
	}

	tmpPath, cleanup, sha, err := writeDownloadToTemp(ctx, deps.artifacts, resp.Body)
	if err != nil {
		cleanupIfNeeded(cleanup)
		return downloadResult{}, err
	}
	if err := verifyDownloadSHA(meta, sha); err != nil {
		cleanupIfNeeded(cleanup)
		return downloadResult{}, err
	}
	if useCache {
		return commitDownload(ctx, deps.artifacts, key, tmpPath, sha, cleanup)
	}
	return downloadResult{Path: tmpPath, SHA: sha, Cleanup: cleanup}, nil
}

// streamDownloadAndExtract reads the artifact body once, tee-ing it into the
// sha256 hasher, the artifact tmp file, and a pipe consumed by the extracted
// store's gzip+tar pipeline. On success the extracted tree is promoted to
// <root>/<sha> atomically, so subsequent installs hardlink from there.
func streamDownloadAndExtract(
	ctx context.Context,
	deps installDeps,
	key string,
	meta *types.GalaxyCollectionVersionInfo,
	body io.Reader,
	useCache bool,
) (downloadResult, error) {
	tmpFile, tmpCleanup, err := deps.artifacts.TempFile(ctx, helpers.ArtifactDownloadTempPrefix)
	if err != nil {
		return downloadResult{}, err
	}

	pr, pw := io.Pipe()
	type ingestOutcome struct {
		err error
		tmp string
	}
	ingestCh := make(chan ingestOutcome, 1)
	go func() {
		tmp, ingestErr := deps.extractStore.IngestReader(pr)
		ingestCh <- ingestOutcome{tmp: tmp, err: ingestErr}
	}()

	hasher := sha256.New()
	limited := helpers.NewSizeLimitedReader(body, helpers.ArtifactMaxDownloadSize)
	_, copyErr := io.Copy(io.MultiWriter(tmpFile, hasher, pw), limited)
	if copyErr != nil {
		_ = pw.CloseWithError(copyErr)
	} else {
		_ = pw.Close()
	}
	closeErr := tmpFile.Close()
	out := <-ingestCh

	if err := firstNonNil(copyErr, closeErr, out.err); err != nil {
		if out.tmp != "" {
			_ = os.RemoveAll(out.tmp)
		}
		cleanupIfNeeded(tmpCleanup)
		return downloadResult{}, err
	}

	sha := hex.EncodeToString(hasher.Sum(nil))
	if err := verifyDownloadSHA(meta, sha); err != nil {
		_ = os.RemoveAll(out.tmp)
		cleanupIfNeeded(tmpCleanup)
		return downloadResult{}, err
	}
	if _, err := deps.extractStore.Promote(out.tmp, sha); err != nil {
		cleanupIfNeeded(tmpCleanup)
		return downloadResult{}, err
	}
	if useCache {
		return commitDownload(ctx, deps.artifacts, key, tmpFile.Name(), sha, tmpCleanup)
	}
	return downloadResult{Path: tmpFile.Name(), SHA: sha, Cleanup: tmpCleanup}, nil
}

func firstNonNil(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func validateDownloadInputs(cfg *config.Config, artifacts cacheManager.ArtifactStore, meta *types.GalaxyCollectionVersionInfo) error {
	if meta == nil {
		return helpers.ErrMetadataIsNil
	}
	if meta.DownloadURL == "" {
		return helpers.ErrMissingDownloadURL
	}
	if cfg == nil {
		return helpers.ErrConfigIsNil
	}
	if artifacts == nil {
		return helpers.ErrArtifactCacheNotConfigured
	}
	return nil
}

func writeDownloadToTemp(ctx context.Context, artifacts cacheManager.ArtifactStore, body io.Reader) (string, func(), string, error) {
	tmpFile, cleanup, err := artifacts.TempFile(ctx, helpers.ArtifactDownloadTempPrefix)
	if err != nil {
		return "", cleanup, "", err
	}
	hasher := sha256.New()
	writer := io.MultiWriter(tmpFile, hasher)
	limited := helpers.NewSizeLimitedReader(body, helpers.ArtifactMaxDownloadSize)
	if _, err := io.Copy(writer, limited); err != nil {
		_ = tmpFile.Close()
		return "", cleanup, "", err
	}
	if err := tmpFile.Close(); err != nil {
		return "", cleanup, "", err
	}
	return tmpFile.Name(), cleanup, hex.EncodeToString(hasher.Sum(nil)), nil
}

func verifyDownloadSHA(meta *types.GalaxyCollectionVersionInfo, sha string) error {
	expected := strings.TrimSpace(meta.Artifact.Sha256)
	if expected == "" || expected == sha {
		return nil
	}
	return fmt.Errorf("%w: %s != %s", helpers.ErrSHA256Mismatch, expected, sha)
}

func commitDownload(
	ctx context.Context,
	artifacts cacheManager.ArtifactStore,
	key string,
	tmpPath string,
	sha string,
	cleanup func(),
) (downloadResult, error) {
	stored, err := artifacts.Commit(ctx, key, tmpPath, map[string]string{"sha256": sha})
	if err != nil {
		cleanupIfNeeded(cleanup)
		return downloadResult{}, err
	}
	return downloadResult{Path: stored.Path, SHA: sha, Cleanup: stored.Cleanup}, nil
}

func cleanupIfNeeded(cleanup func()) {
	if cleanup != nil {
		cleanup()
	}
}

// resolveMetadata loads metadata when needed and handles cache-hit warnings.
func resolveMetadata(
	ctx context.Context,
	deps collectionDeps,
	col collection,
	meta *types.GalaxyCollectionVersionInfo,
	cacheHit bool,
) (*types.GalaxyCollectionVersionInfo, error) {
	runtime := deps.runtime
	if meta != nil {
		return meta, nil
	}
	metaStart := time.Now()
	meta, err := loadCollectionMetadata(ctx, deps, col)
	runtime.Output.DebugSincef(metaStart, "%s", "metadata "+col.key())
	if err != nil {
		if cacheHit {
			runtime.Output.Printf("⚠️ Failed to load metadata for %s: %v", col.key(), err)
			return nil, helpers.ErrMetadataUnavailable
		}
		return nil, fmt.Errorf("failed to load metadata: %w", err)
	}
	return meta, nil
}

// installDependencies installs dependent collections from MANIFEST.json.
func installDependencies(ctx context.Context, installPath string, depsCtx installDeps) ([]string, error) {
	cfg := depsCtx.cfg
	runtime := depsCtx.runtime

	manifestPath := filepath.Join(installPath, "MANIFEST.json")
	//nolint:gosec // manifestPath is derived from install path and is trusted.
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	var parsed types.GalaxyCollectionVersionInfoManifest
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("invalid MANIFEST.json: %w", err)
	}

	deps := make([]string, 0, len(parsed.CollectionInfo.Dependencies))
	for fqdn, version := range parsed.CollectionInfo.Dependencies {
		parts := strings.Split(fqdn, ".")
		if len(parts) != helpers.CollectionNameParts {
			runtime.Output.Printf("⚠️ Skipping invalid dependency: %s", fqdn)
			continue
		}
		depCol := collection{Namespace: parts[0], Name: parts[1], Version: version, Source: cfg.Server}
		deps = append(deps, depCol.key())
		runtime.Output.Printf("🔁 Installing dependency: %s %s", fqdn, version)
		if err := installCollection(ctx, depCol, depsCtx, nil, nil); err != nil {
			return nil, fmt.Errorf("failed to install dependency %s: %w", fqdn, err)
		}
	}
	return deps, nil
}
