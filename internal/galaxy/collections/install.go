package collections

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/archive"
	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/output"
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
	prefetched downloadResult,
) error {
	cfg := deps.cfg
	runtime := deps.runtime
	st := deps.st

	installStart := time.Now()
	defer func() {
		runtime.Output.DebugSincef(installStart, "%s", col.key())
	}()

	// Built once, here, and threaded through every write below (extraction,
	// the extract marker, the GALAXY.yml sidecar, the skip check): one target,
	// one os.Root, rather than each call site rederiving its own path and
	// risking one of them skipping the validation the others perform.
	target, ok := newInstallTarget(deps.root, cfg, col)
	if !ok {
		return fmt.Errorf("%w: ns=%q name=%q version=%q",
			helpers.ErrUnsafeCollectionIdentifier, col.Namespace, col.Name, col.Version)
	}

	filename := fmt.Sprintf("%s-%s-%s.tar.gz", col.Namespace, col.Name, col.Version)

	if canSkipInstall(target, col, st, runtime.Output) {
		runtime.Output.Printf("⏭️ Skipping install, already installed: %s/%s/%s", col.Namespace, col.Name, col.Version)
		// installCollection may be handed a prefetched temp on a path that skips the install; release it here rather than
		// leave it unclaimed until Close. In today's dispatch this branch is not live: a schedulable prefetch task is never
		// already installed, and an already-installed collection is never scheduled, so its handoff would be empty. This is
		// a defensive guard preserving the exactly-once cleanup contract for any future caller that hands a live temp into
		// an already-installed collection.
		if prefetched.Cleanup != nil {
			prefetched.Cleanup()
		}
		return nil
	}

	payload, err := prepareAndExtract(ctx, deps, col, metaOverride, prefetched, filename, target)
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

	writeGalaxyInfoIfPresent(runtime, target, cfg, col, payload.meta)
	recordInstall(st, col, target.path, payload.artifactSHA, resolvedDeps)
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
	prefetched downloadResult,
	filename string,
	forceDownload bool,
) (installPayload, bool, error) {
	// A temp the prefetcher already downloaded and committed takes priority
	// over every other path: its bytes are already fresh, verified origin
	// bytes, so there is nothing left to fetch. forceDownload (set only by
	// the corruption-recovery retry below) still wins over it: a prefetched
	// temp was never evicted, so retrying against the very same bytes that
	// just failed would only reproduce the same failure.
	if prefetched.Path != "" && !forceDownload {
		return payloadFromPrefetched(col, metaOverride, prefetched)
	}

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
// target's install directory. This is the verify+extract step
// installCollection always ran inline; it is factored out so
// prepareAndExtract can run it a second time against a freshly downloaded
// replacement after evicting a corrupt cache hit.
func verifyAndExtract(
	_ context.Context,
	deps installDeps,
	col collection,
	payload installPayload,
	target installTarget,
	filename string,
) error {
	if err := verifyPinnedSHA(col, payload.artifactSHA); err != nil {
		return err
	}
	extractStart := time.Now()
	err := extractCollection(col, payload.artifact.Path, target, deps.runtime, deps.extractStore, payload.artifactSHA)
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
// Accepted tradeoff: every cache-hit action failure whose cause is
// artifact-side spends exactly one evict-and-refetch attempt with no further
// classification of *why* it failed. A rare transient failure therefore costs
// one wasted refetch of otherwise-good bytes; that cost is bounded to a
// single retry and the end result is still correct. The action arm does carry
// one classification, though: isDestinationSideFailure carves out
// helpers.ErrCollectionsPathEscape and helpers.ErrUnsafeCollectionIdentifier,
// because unlike every artifact-side action failure, neither one is repaired
// by substituting a different artifact - the artifact was never the problem,
// the destination path was - so evicting would only spend a destructive
// write against shared state (an S3 deleteObject) for no chance of success,
// and an attacker-controlled requirements.yml can trigger it deterministically,
// once per affected collection per run, with no race required. The precedent
// is helpers.ErrMalformedArtifactSHA256 sitting outside the retry class in the
// prepareInstall arm below for the identical reason: eviction cannot repair a
// value, or here a location, that was never valid to begin with. The
// prepareInstall failure arm itself classifies for a different reason: only
// helpers.ErrSHA256Mismatch (the S3 backend's read-time integrity check) is
// worth retrying there, since every other prepareInstall failure (a metadata
// error, a download error, an offline miss) would just reproduce identically
// against the same cache entry or the same origin.
//
// canRetryCacheHit and evictCorruptCachedArtifact are factored out so both
// recovery arms below share one guard and one log line - kept out of this
// function's own body to stay under its cyclomatic complexity budget.
func prepareWithRecovery(
	ctx context.Context,
	deps installDeps,
	col collection,
	metaOverride *types.GalaxyCollectionVersionInfo,
	prefetched downloadResult,
	filename string,
	action func(installPayload) error,
) (installPayload, error) {
	forceDownload := false
	for {
		payload, fromCache, err := prepareInstall(ctx, deps, col, metaOverride, prefetched, filename, forceDownload)
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
		if !canRetryCacheHit(deps, fromCache, forceDownload) || isDestinationSideFailure(actionErr) {
			return installPayload{}, actionErr
		}
		evictCorruptCachedArtifact(ctx, deps, col, filename, actionErr)
		forceDownload = true
	}
}

// isDestinationSideFailure reports whether err's cause is the write
// destination rather than the artifact: an os.Root refusal to traverse a
// symlinked path component under cfg.DownloadPath
// (helpers.ErrCollectionsPathEscape) or an unsafe namespace/name/version
// identifier (helpers.ErrUnsafeCollectionIdentifier). prepareWithRecovery's
// action arm otherwise evicts and refetches unclassified, on the premise
// that a fresh artifact might fix whatever failed - a premise that does not
// hold here, since neither cause has anything to do with the bytes just
// verified and extracted. Evicting anyway would still spend a real,
// destructive write against shared state (an S3 deleteObject) with no chance
// of the retry succeeding, and both causes are reachable from an attacker's
// own requirements.yml with no race required, so the eviction would be
// deterministic on every affected run rather than a rare accident.
func isDestinationSideFailure(err error) bool {
	return errors.Is(err, helpers.ErrCollectionsPathEscape) || errors.Is(err, helpers.ErrUnsafeCollectionIdentifier)
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
// extracts it into target's install directory, delegating the bounded
// eviction-and-refetch retry to prepareWithRecovery.
func prepareAndExtract(
	ctx context.Context,
	deps installDeps,
	col collection,
	metaOverride *types.GalaxyCollectionVersionInfo,
	prefetched downloadResult,
	filename string,
	target installTarget,
) (installPayload, error) {
	return prepareWithRecovery(ctx, deps, col, metaOverride, prefetched, filename, func(payload installPayload) error {
		return verifyAndExtract(ctx, deps, col, payload, target, filename)
	})
}

// payloadFromPrefetched builds an installable payload from an artifact the
// prefetcher already downloaded and committed. Its bytes are the origin bytes,
// already sha-verified against meta during the prefetch download, so there is
// nothing to re-fetch. resolveArtifactSHA returns the download hash directly
// (prefetched.SHA is non-empty), so a pinned collection's verifyPinnedSHA
// still compares the pin against a hash of the real bytes - the reuse never
// bypasses the pin. servedFromCache is reported false: these are fresh origin
// bytes, not a stale cache hit, so prepareWithRecovery must not spend an
// evict-and-refetch on a verify/extract failure here.
func payloadFromPrefetched(
	col collection,
	meta *types.GalaxyCollectionVersionInfo,
	prefetched downloadResult,
) (installPayload, bool, error) {
	artifact := artifactData{Path: prefetched.Path, Cleanup: prefetched.Cleanup, SHA: prefetched.SHA}
	artifactSHA, err := resolveArtifactSHA(artifact.Path, meta, artifact.Meta, artifact.SHA, col.SHA256)
	// Every handed-off downloadResult carries a non-empty SHA - hashed during the prefetch download itself - so
	// resolveArtifactSHA always returns through its artifactSHA != "" arm here, and this error path is not reached
	// in practice. It is kept for symmetry with resolveArtifactSHA's other callers, and to fail closed rather than
	// silently install unhashed bytes if a handoff ever carried an empty SHA.
	if err != nil {
		if artifact.Cleanup != nil {
			artifact.Cleanup()
		}
		return installPayload{}, false, err
	}
	return installPayload{meta: meta, artifact: artifact, artifactSHA: artifactSHA}, false, nil
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

// writeGalaxyInfoIfPresent writes col's GALAXY.yml sidecar and reports any
// failure on the tier matching its severity. An unsafe identifier or a
// collections-path escape are both integrity signals - the same class
// verifyExtractMarker's unsafe-sha arm warns on - so they must survive
// --quiet; an ordinary I/O failure is not. Neither tier fails the run: the
// collection has already been extracted by the time this runs, and this call
// has never failed an install.
//
// helpers.ErrUnsafeCollectionIdentifier is defensive-only here: by the time
// this runs, installCollection has already built target via a successful
// newInstallTarget call, so writeGalaxyInfo itself has nothing left to
// validate and cannot produce this sentinel through today's only call path.
// The arm is kept anyway - matching writeExtractMarker's own guard, which
// stays independent of any caller - rather than assuming that invariant
// holds forever.
//
// helpers.ErrCollectionsPathEscape IS reachable: target.root refuses to
// traverse a symlink planted between newInstallTarget's validation and this
// call (a TOCTOU window, however narrow), and classifyCollectionsRootError
// surfaces that refusal through writeGalaxyInfo's two rooted calls.
//
// Neither warning also names the collection: err already renders namespace,
// name, and version (or the offending path component) with %q, and
// interpolating the raw identifier a second time with %s would add no
// information - col already passed newInstallTarget's IsPathElement check
// earlier in installCollection, and Warnf/Printf both sanitize the rendered
// line through safeout.Clean besides. Same reason verifyExtractMarker uses
// %q and never %s for a sha.
func writeGalaxyInfoIfPresent(
	runtime *infra.Infra,
	target installTarget,
	cfg *config.Config,
	col collection,
	meta *types.GalaxyCollectionVersionInfo,
) {
	err := writeGalaxyInfo(target, cfg, col, meta)
	if err == nil {
		return
	}
	if errors.Is(err, helpers.ErrUnsafeCollectionIdentifier) || errors.Is(err, helpers.ErrCollectionsPathEscape) {
		runtime.Output.Warnf("Refusing to write GALAXY.yml: %v", err)
		return
	}
	runtime.Output.Printf("⚠️ Failed to write GALAXY.yml: %v", err)
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
		result, err := downloadCollectionToCache(ctx, deps, artifactKey(col), col.Source, meta, useCache)
		if err != nil {
			return artifactData{}, err
		}
		runtime.Output.DebugSincef(downloadStart, "%s", "download "+col.key())
		return artifactData{Path: result.Path, Cleanup: result.Cleanup, SHA: result.SHA}, nil
	}
	if artifacts == nil {
		return artifactData{}, helpers.ErrArtifactCacheNotConfigured
	}
	// The same helpers.ArtifactDownloadDeadline budget that bounds a fresh
	// origin download (see downloadCollectionToCache) also bounds a
	// cache-hit fetch: the S3 artifact store's Fetch streams a full artifact
	// body over HTTP (internal/cache/s3's downloadToFile), so a byte-drip on
	// that read pins a worker exactly as it would on a fresh download. The
	// local backend's Fetch is a Stat plus a small sidecar read that ignores
	// its context entirely, so the deadline is inert there by construction -
	// applying it here, in collections, rather than inside internal/cache/s3
	// means the backend merely honors the context it is handed, exactly as it
	// already honors caller cancellation, with no constant, sentinel, or
	// policy crossing the Backend seam.
	budget := runtime.ArtifactDeadline()
	fetchCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	cached, err := artifacts.Fetch(fetchCtx, artifactKey(col))
	if err != nil {
		return artifactData{}, artifactDeadlineError(ctx, fetchCtx, budget, err)
	}
	runtime.Metrics.AddCacheHit()
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
//
// meta.Artifact.Sha256 and artifactMeta["sha256"] are each validated against
// helpers.IsSHA256Hex before being returned, and artifactSHA and the two
// archive.FileHashSHA256 results are deliberately not: the rule is validate
// what crossed a trust boundary, never what this process just computed.
// meta.Artifact.Sha256 is raw Galaxy API JSON a server controls; a poisoned
// or lying value there would otherwise flow unchecked into recordInstall and
// recordWarmed, and from there into the persisted snapshot - re-entering
// every later run against that cache, including one against a different,
// honest server. artifactMeta["sha256"] carries the same risk one hop later:
// it is a cache sidecar that could itself have been poisoned by an earlier
// run that took this same path with an unvalidated value. artifactSHA, by
// contrast, is hex.EncodeToString(hasher.Sum(nil)) computed by this process
// over bytes it just streamed, and archive.FileHashSHA256 is this same
// encoder's output over a file already on disk; checking either would only
// be checking our own encoder, at the cost of an extra file hash on the hot
// fresh-download path. Do not "complete" this by validating those too.
//
// A failure here is a hard error, not a fall-through to the next source:
// silently accepting a malformed value and moving on would still let a
// lying server or a corrupt cache pick the sha this run installs under,
// which is exactly the "influence what a run installs" capability the
// project's trust model bounds to a cache writer, not a Galaxy server. The
// blast radius is one collection failing on a server or cache entry already
// broken by verifyDownloadSHA's standards.
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
			if !helpers.IsSHA256Hex(sha) {
				return "", fmt.Errorf("%w: %q", helpers.ErrMalformedArtifactSHA256, sha)
			}
			return sha, nil
		}
	}
	if artifactMeta != nil {
		if sha := strings.TrimSpace(artifactMeta["sha256"]); sha != "" {
			if !helpers.IsSHA256Hex(sha) {
				return "", fmt.Errorf("%w: %q", helpers.ErrMalformedArtifactSHA256, sha)
			}
			return sha, nil
		}
	}
	return archive.FileHashSHA256(path)
}

// artifactKey builds the cache key for a collection tarball, scoped to the
// server col actually resolved from (col.Source, stamped by
// solverResultToResolvedGraph/sourceFor with the server that answered during
// solve - never a stale pin, never a bare cfg.Server). See helpers.ArtifactKey
// for the collision this scoping closes: without it, two servers publishing
// the same <ns>-<name>-<version>.tar.gz would collide on one flat cache slot
// with no way for isCacheHit to detect it.
func artifactKey(col collection) string {
	filename := fmt.Sprintf("%s-%s-%s.tar.gz", col.Namespace, col.Name, col.Version)
	return helpers.ArtifactKey(col.Source, filename)
}

// installEntryMatches reports whether a recorded install entry still points
// at installPath with a known artifact hash consistent with any lockfile pin
// on col, and was installed from the same server col now resolves from. An
// empty pin allows any recorded hash, so non-frozen installs keep their prior
// skip behavior unchanged. The source check exists because a resolved
// collection's Source can change between runs with no version change at all
// - the same namespace.name@version now resolves from a different
// configured server (a source: edit, a server reordering, or a different
// server winning first-match ownership) - and without it, a stale install
// from the old server would be silently kept: canSkipInstall never re-checks
// origin, and an unpinned collection carries no SHA256 to catch the mismatch
// the way a frozen lockfile pin would.
func installEntryMatches(col collection, entry store.InstalledEntry, installPath string) bool {
	if entry.InstallPath == "" || entry.InstallPath != installPath {
		return false
	}
	if entry.ArtifactSHA256 == "" {
		return false
	}
	if entry.Source != col.Source {
		return false
	}
	if col.SHA256 != "" && strings.TrimSpace(entry.ArtifactSHA256) != strings.TrimSpace(col.SHA256) {
		return false
	}
	return true
}

// installRecordMatches reports whether a collection's store entry, extract
// marker, and GALAXY.yml sidecar are all present and consistent for target,
// using only cheap target.root.Stat calls - no tree walk. This is the check
// shouldSchedulePrefetch uses to decide whether to spend a background
// download ahead of time: a wrong "skip" there only costs a lost prefetch
// head start, since installCollection's canSkipInstall below always
// re-checks strictly (including the tally) before actually skipping the
// install itself, so correctness never depends on this cheap version.
//
// Both stats go through target.root rather than a plain os.Stat, which is
// what makes this check itself symlink-swap safe: an install directory or
// .info sidecar that exists only by following a symlink past
// cfg.DownloadPath is not found by root.Stat (it refuses to traverse the
// escaping component), so this correctly reports false rather than mistaking
// a symlink-only fake for a real install.
func installRecordMatches(target installTarget, col collection, st *store.Store) bool {
	if st == nil {
		return false
	}
	entry, ok := st.GetInstalled(col.key())
	if !ok || !installEntryMatches(col, entry, target.path) {
		return false
	}

	markerRelPath, ok := markerRel(target, entry.ArtifactSHA256)
	if !ok {
		return false
	}
	if _, err := target.root.Stat(markerRelPath); err != nil {
		return false
	}

	if _, err := target.root.Stat(path.Join(target.info, galaxyYAMLFileName)); err != nil {
		return false
	}

	return true
}

// canSkipInstall reports whether a collection is already installed and its
// extracted tree still matches the tally recorded at extraction time. It
// layers verifyExtractMarker's fs.WalkDir pass on top of
// installRecordMatches's cheap checks, and is called only from
// installCollection: this is the gate that actually decides whether real
// work is skipped, so unlike the prefetch scan's use of installRecordMatches,
// a false "skip" here would silently keep serving a corrupted shared
// extracted-store cache to every future install. See verifyExtractMarker for
// exactly what the tally catches and does not catch.
func canSkipInstall(target installTarget, col collection, st *store.Store, out output.Printer) bool {
	if !installRecordMatches(target, col, st) {
		return false
	}
	entry, ok := st.GetInstalled(col.key())
	if !ok {
		return false
	}
	return verifyExtractMarker(out, target, entry.ArtifactSHA256)
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
//
// The whole call - every attempt and every backoff sleep between them - runs
// under one shared helpers.ArtifactDownloadDeadline budget (deps.runtime.
// ArtifactDeadline()), established once around the outer helpers.Retry loop
// rather than re-derived per attempt: the unit this budget bounds is
// "acquire one artifact into the cache", the whole of what pins an install
// worker, not any single attempt within it. This deliberately covers more
// than the GET: when deps.extractStore is configured, streamDownloadAndExtract
// runs the single-pass extraction concurrently with the body read, and
// commitDownload's artifacts.Commit can itself be an S3 PUT streaming the
// tarball upstream - a stalled request-body write there is bounded by this
// same deadline even though ResponseHeaderTimeout, which only covers waiting
// for a response, does not reach it. This is the single funnel for the
// origin download from every producer: the install path's fetchArtifact, warm
// (through the same prepareInstall -> fetchArtifact path), and the
// prefetcher's prefetchOne, which calls this function directly.
//
// The outer artifactDeadlineError call after helpers.Retry returns is not
// redundant with the one inside the retry closure: helpers.Retry's own
// backoff wait returns a bare ctx.Err() straight from its select statement,
// never routing that value through the closure, so only the outer call
// normalizes a deadline that expired during a backoff sleep rather than
// during an attempt itself. This path is defensive and deliberately
// uncovered by a test: reaching it needs an attempt to fail retryably while
// dlCtx is still live and the budget to then expire during the jittered
// backoff sleep that follows, a race not deterministically constructible
// without a test seam inside helpers.Retry itself, which is not worth adding
// for this. Its failure mode if this call were ever removed is diagnostic,
// not behavioral: the error would surface as a bare context.DeadlineExceeded
// instead of the wrapped sentinel, and isNetworkError already classifies
// that bare value to the identical ExitNetwork exit code (see
// cmd/go-galaxy/exitcode), so a missing outer call would degrade a log line,
// not a run's outcome.
func downloadCollectionToCache(
	ctx context.Context,
	deps installDeps,
	key string,
	base string,
	meta *types.GalaxyCollectionVersionInfo,
	useCache bool,
) (downloadResult, error) {
	if err := validateDownloadInputs(deps.cfg, deps.artifacts, meta); err != nil {
		return downloadResult{}, err
	}

	budget := deps.runtime.ArtifactDeadline()
	dlCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	var result downloadResult
	err := helpers.Retry(dlCtx, helpers.FetchRetryPolicy(), func() error {
		attempted, attemptErr := attemptDownloadToCache(dlCtx, deps, key, base, meta, useCache)
		if attemptErr != nil {
			return artifactDeadlineError(ctx, dlCtx, budget, attemptErr)
		}
		result = attempted
		return nil
	}, downloadRetryable)
	if err != nil {
		return downloadResult{}, artifactDeadlineError(ctx, dlCtx, budget, err)
	}
	// Counted once per successful, retry-bounded acquisition - not inside
	// attemptDownloadToCache - so a retried 5xx counts one miss, not one per
	// attempt. This is the single funnel for both the main install path
	// (fetchArtifact) and the prefetcher, which calls this function directly.
	deps.runtime.Metrics.AddCacheMiss()
	return result, nil
}

// warnIfOffServerDownloadHost emits a warning when an artifact's download URL
// points at a host other than base, the server that actually resolved this
// collection (col.Source, non-empty by construction at install time - every
// resolved collection is stamped with its winning server by
// solverResultToResolvedGraph). The metadata that supplies the download URL
// can come from a cached snapshot, which a bucket writer could poison to
// redirect a download off-server; surfacing the mismatch gives a visible
// signal in CI logs without blocking, since a legitimate deployment (an
// enterprise content host or an object-storage URL) may serve downloads from
// a different host than its API server. A blank base or an unparseable URL
// is not warned about, to avoid false alarms. The warning goes out through
// Warnf rather than Printf: this is a security/integrity detection signal,
// so it must survive --quiet (Warnf always emits, to stderr, unlike the
// transient Printf tier) rather than risk being silenced in the very CI mode
// where it matters most.
func warnIfOffServerDownloadHost(runtime *infra.Infra, base, downloadURL string) {
	if strings.TrimSpace(base) == "" {
		return
	}
	server, err := url.Parse(base)
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
	base string,
	meta *types.GalaxyCollectionVersionInfo,
	useCache bool,
) (downloadResult, error) {
	warnIfOffServerDownloadHost(deps.runtime, base, meta.DownloadURL)
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

	tmpPath, cleanup, sha, err := writeDownloadToTemp(ctx, deps, resp.Body)
	if err != nil {
		cleanupIfNeeded(cleanup)
		return downloadResult{}, err
	}
	if err := verifyDownloadSHA(meta, sha); err != nil {
		cleanupIfNeeded(cleanup)
		return downloadResult{}, err
	}
	// The shape probe belongs to this arm alone. The extracted-store arm above
	// already answers the same question by construction, since its ingest runs
	// the body through gzip+tar; a second pass there would be pure cost. Here
	// nothing has looked at the bytes at all: verifyDownloadSHA compares
	// against whatever sha the server declared, and declines to compare when
	// the server declared none, so without this an error page can reach a
	// shared cache slot and every later consumer opens it before failing. The
	// probe runs whether or not useCache is set, so the arm has one rule
	// rather than two; its cost is one open and a gzip header read against a
	// full artifact download.
	if err := archive.ProbeTarGz(tmpPath); err != nil {
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
	n, copyErr := io.Copy(io.MultiWriter(tmpFile, hasher, pw), limited)
	deps.runtime.Metrics.AddBytesDownloaded(n)
	if copyErr != nil {
		// helpers.ErrResponseTooLarge is a bare size ceiling shared with three
		// other capped surfaces, so it is wrapped here naming this one. Without
		// the wrap this is the only capped body that does not identify itself,
		// leaving an operator to infer it from the absence of the other labels -
		// which fails exactly where it matters, since a metadata re-resolution
		// inside an install worker prints under the same per-collection line.
		// Deliberately uncovered: ArtifactMaxDownloadSize is 4 GiB, so tripping
		// the ceiling end to end is impractical rather than merely inconvenient.
		copyErr = fmt.Errorf("collection artifact download: %w", copyErr)
		_ = pw.CloseWithError(copyErr)
	} else {
		_ = pw.Close()
	}
	closeErr := tmpFile.Close()
	out := <-ingestCh

	// Discard, not os.RemoveAll: the path came from the extracted store, so
	// the store is what removes it, through its own containment root. A raw
	// RemoveAll here would be the one place in this pipeline aiming that
	// primitive at a path nothing re-validated.
	if err := firstNonNil(copyErr, closeErr, out.err); err != nil {
		_ = deps.extractStore.Discard(out.tmp)
		cleanupIfNeeded(tmpCleanup)
		return downloadResult{}, err
	}

	sha := hex.EncodeToString(hasher.Sum(nil))
	if err := verifyDownloadSHA(meta, sha); err != nil {
		_ = deps.extractStore.Discard(out.tmp)
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

// downloadURLAllowed reports whether raw is a URL this pipeline may fetch: an
// absolute http or https URL that names a host. Anything else - a relative
// reference, a scheme this program never speaks, or a URL with no host at all
// - is refused before a request is built.
//
// This is hardening rather than a live hole today: runtime.HTTP is an ordinary
// *http.Client that registers no additional protocols, so net/http already
// refuses a scheme like file: with "unsupported protocol scheme". What the
// check adds is that the refusal is classified and raised once per artifact
// acquisition, ahead of the request, instead of surfacing per attempt as an
// opaque transport failure - and that registering a protocol on this client
// later cannot silently turn a poisoned download_url into a fetch.
func downloadURLAllowed(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	return (scheme == "http" || scheme == "https") && u.Host != ""
}

func validateDownloadInputs(cfg *config.Config, artifacts cacheManager.ArtifactStore, meta *types.GalaxyCollectionVersionInfo) error {
	if meta == nil {
		return helpers.ErrMetadataIsNil
	}
	if meta.DownloadURL == "" {
		return helpers.ErrMissingDownloadURL
	}
	if !downloadURLAllowed(meta.DownloadURL) {
		// %q, not %s: this string came off a server or out of a cached
		// snapshot, no name alphabet ever judged it, and internal/safeout's
		// printer boundary does not reach the text of a returned error.
		return fmt.Errorf("%w: %q", helpers.ErrUnsupportedDownloadURLScheme, meta.DownloadURL)
	}
	if cfg == nil {
		return helpers.ErrConfigIsNil
	}
	if artifacts == nil {
		return helpers.ErrArtifactCacheNotConfigured
	}
	return nil
}

func writeDownloadToTemp(ctx context.Context, deps installDeps, body io.Reader) (string, func(), string, error) {
	tmpFile, cleanup, err := deps.artifacts.TempFile(ctx, helpers.ArtifactDownloadTempPrefix)
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
		// Wrapped for the same reason, and with the same coverage note, as the
		// streaming-ingest copy above: this is the other fresh-origin download
		// path, and an unwrapped size ceiling here would be the one capped body
		// that never names its own surface.
		return "", cleanup, "", fmt.Errorf("collection artifact download: %w", err)
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
