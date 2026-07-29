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
	"os"
	"path/filepath"
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

	filename := fmt.Sprintf("%s-%s-%s.tar.gz", col.Namespace, col.Name, col.Version)
	installPath := collectionInstallPath(cfg, col)

	if canSkipInstall(cfg, col, installPath, st, runtime.Output) {
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

	payload, err := prepareAndExtract(ctx, deps, col, metaOverride, prefetched, filename, installPath)
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

	writeGalaxyInfoIfPresent(runtime, cfg, col, payload.meta)
	recordInstall(st, col, installPath, payload.artifactSHA, resolvedDeps)
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
	prefetched downloadResult,
	filename, installPath string,
) (installPayload, error) {
	return prepareWithRecovery(ctx, deps, col, metaOverride, prefetched, filename, func(payload installPayload) error {
		return verifyAndExtract(ctx, deps, col, payload, installPath, filename)
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
// failure on the tier matching its severity. An unsafe identifier is an
// integrity signal - the same class verifyExtractMarker's unsafe-sha arm
// warns on - so it must survive --quiet; an ordinary I/O failure is not.
// Neither tier fails the run: the collection has already been extracted by
// the time this runs, and this call has never failed an install.
//
// The unsafe-identifier warning does not also name the collection: err
// already renders namespace, name, and version with %q (collectionInfoDir's
// caller wraps them that way), and interpolating the raw identifier a second
// time with %s would put an attacker-controlled string - one that could
// carry newlines or ANSI escapes - onto CI stderr verbatim for no added
// information. Same reason verifyExtractMarker uses %q and never %s for a
// sha.
func writeGalaxyInfoIfPresent(
	runtime *infra.Infra,
	cfg *config.Config,
	col collection,
	meta *types.GalaxyCollectionVersionInfo,
) {
	err := writeGalaxyInfo(cfg, col, meta)
	if err == nil {
		return
	}
	if errors.Is(err, helpers.ErrUnsafeCollectionIdentifier) {
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
	cached, err := artifacts.Fetch(ctx, artifactKey(col))
	if err != nil {
		return artifactData{}, err
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
// for the collision this scoping closes: two servers publishing the same
// <ns>-<name>-<version>.tar.gz used to collide on one flat cache slot with no
// way for isCacheHit to detect it.
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

// collectionInstallPath returns the fixed ansible_collections path col would
// land at (or already occupies) under cfg.DownloadPath. This is the single
// definition of that layout; installCollection, shouldSchedulePrefetch, and
// classifyDryRun all call it rather than each computing the join inline, so
// the three call sites can never drift out of sync with each other.
func collectionInstallPath(cfg *config.Config, col collection) string {
	return filepath.Join(cfg.DownloadPath, "ansible_collections", col.Namespace, col.Name)
}

// collectionInfoDir returns the on-disk path of col's GALAXY.yml sidecar
// directory under cfg.DownloadPath, and whether col's identity was safe to
// use at all - the single validating chokepoint every construction of this
// path must go through, so it cannot be computed two different ways, one of
// them unguarded.
//
// col.Namespace, col.Name, and col.Version must each be helpers.IsPathElement
// before they are ever joined: filepath.Join fuses a leading ".." in Version
// into the synthetic "<ns>.<name>-.." element, turning it into a real "up one
// directory" element, so the join absorbs Version's first ".." for free and
// every later ".." in Version pops a real path component off DownloadPath.
// Validating the joined result after the fact cannot close this - by the
// time a path exists to inspect, the escape has already happened - so the
// three components are what is validated, before any join is computed, and
// never the composed "<ns>.<name>-<version>.info" element: cleanup.go's
// removeInstalled validates the same three components with the same
// predicate, so the deleter never refuses a sidecar directory this function
// created as an unsafe identifier. That symmetry is specific to the
// sidecar path: collectionInstallPath performs no such validation on the
// identifiers it joins.
func collectionInfoDir(cfg *config.Config, col collection) (string, bool) {
	if !helpers.IsPathElement(col.Namespace) || !helpers.IsPathElement(col.Name) || !helpers.IsPathElement(col.Version) {
		return "", false
	}
	return filepath.Join(
		cfg.DownloadPath,
		"ansible_collections",
		fmt.Sprintf("%s.%s-%s.info", col.Namespace, col.Name, col.Version),
	), true
}

// installRecordMatches reports whether a collection's store entry, extract
// marker, and GALAXY.yml sidecar are all present and consistent for
// installPath, using only cheap os.Stat calls - no tree walk. This is the
// check shouldSchedulePrefetch uses to decide whether to spend a background
// download ahead of time: a wrong "skip" there only costs a lost prefetch
// head start, since installCollection's canSkipInstall below always
// re-checks strictly (including the tally) before actually skipping the
// install itself, so correctness never depends on this cheap version.
func installRecordMatches(cfg *config.Config, col collection, installPath string, st *store.Store) bool {
	if cfg == nil || st == nil {
		return false
	}
	entry, ok := st.GetInstalled(col.key())
	if !ok || !installEntryMatches(col, entry, installPath) {
		return false
	}

	marker, ok := extractMarkerPath(installPath, entry.ArtifactSHA256)
	if !ok {
		return false
	}
	if _, err := os.Stat(marker); err != nil {
		return false
	}

	infoDir, ok := collectionInfoDir(cfg, col)
	if !ok {
		return false
	}
	if _, err := os.Stat(filepath.Join(infoDir, galaxyYAMLFileName)); err != nil {
		return false
	}

	return true
}

// canSkipInstall reports whether a collection is already installed and its
// extracted tree still matches the tally recorded at extraction time. It
// layers verifyExtractMarker's filepath.WalkDir pass on top of
// installRecordMatches's cheap checks, and is called only from
// installCollection: this is the gate that actually decides whether real
// work is skipped, so unlike the prefetch scan's use of installRecordMatches,
// a false "skip" here would silently keep serving a corrupted shared
// extracted-store cache to every future install. See verifyExtractMarker for
// exactly what the tally catches and does not catch.
func canSkipInstall(cfg *config.Config, col collection, installPath string, st *store.Store, out output.Printer) bool {
	if !installRecordMatches(cfg, col, installPath, st) {
		return false
	}
	entry, ok := st.GetInstalled(col.key())
	if !ok {
		return false
	}
	return verifyExtractMarker(out, installPath, entry.ArtifactSHA256)
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
	base string,
	meta *types.GalaxyCollectionVersionInfo,
	useCache bool,
) (downloadResult, error) {
	if err := validateDownloadInputs(deps.cfg, deps.artifacts, meta); err != nil {
		return downloadResult{}, err
	}

	var result downloadResult
	err := helpers.Retry(ctx, helpers.FetchRetryPolicy(), func() error {
		attempted, attemptErr := attemptDownloadToCache(ctx, deps, key, base, meta, useCache)
		if attemptErr != nil {
			return attemptErr
		}
		result = attempted
		return nil
	}, downloadRetryable)
	if err != nil {
		return downloadResult{}, err
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
