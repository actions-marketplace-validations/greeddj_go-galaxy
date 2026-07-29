package collections

import (
	"context"
	"sort"
	"sync"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// dryRunBanner announces, once per run, that --dry-run is active. It always
// goes out through Warnf - stderr, survives --quiet - rather than Printf or
// PersistentPrintf: --dry-run is GO_GALAXY_DRY_RUN-sourced, so an org-wide CI
// environment block can silently turn every install into a no-op with
// nothing else in the log to say so. A banner that could itself be
// suppressed by the very same environment that caused it would defeat the
// point of having one.
func dryRunBanner(runtime *infra.Infra) {
	runtime.Output.Warnf("--dry-run is active: no artifacts will be downloaded and nothing will be installed")
}

// dryRunClassification is one collection's dry-run verdict, computed by the
// parallel probe pass in classifyDryRun and consumed by its sequential report
// pass. upToDate and cached are mutually meaningful only in that order: a
// caller must check upToDate first, exactly as the report pass does.
type dryRunClassification struct {
	upToDate bool
	cached   bool
}

// classifyDryRun reports, without mutating anything, what an install would
// do for every collection in collections, and returns how many of them would
// fail outright (see below). The per-collection report strings below are
// install-shaped ("Would install:", "Would fail:"); a future warm caller
// passing checkInstalled=false gets a report that is still correct in
// substance (up-to-date is never reported, since warm tracks no install path
// to compare against) but wrong in wording, and must supply its own verb
// rather than reuse this one's literal strings as-is.
//
// checkInstalled gates whether a collection whose on-disk install already
// satisfies installRecordMatches AND checkExtractMarker (marker.go) is
// reported as already up to date - the same two-step gate canSkipInstall
// itself is built on (installRecordMatches's cheap store/marker-presence/
// GALAXY.yml check, then the tree-tally comparison), so a collection this
// function calls "up to date" is exactly one a real install would also skip.
// This still does not call canSkipInstall directly: canSkipInstall's own
// verifyExtractMarker wraps that same tally comparison with logging and a
// best-effort os.Remove of a drifted marker, and a preview must never delete
// state as a side effect of describing it. checkExtractMarker is the pure,
// read-only half of that wrapper - no logging, no deletion - built for
// exactly this caller; classifyOneDryRun uses it directly instead of
// duplicating the tally comparison here, so the two can never drift apart.
//
// The would-fail count lets the caller fail the run closed with the same
// exit classification a real --offline install would hit: a collection that
// is not up to date, has no cached artifact, and cannot be downloaded because
// --offline is set would fail fetchArtifact's own offline guard on a real
// run, so reporting it as "would install" here would be the one lie a preview
// command cannot afford - claiming success for a certain failure.
//
// What this evaluates, in full: the installed-record check (installRecordMatches),
// the extract-marker tally (checkExtractMarker), artifact-cache presence
// (dryRunArtifactCached, mirroring isCacheHit), and offline reachability. Two
// things a real install also checks are deliberately NOT evaluated here:
//
//   - Lockfile pin verification. Under --frozen --offline, a cached tarball
//     that no longer hashes to its pin fails the real run closed:
//     resolveArtifactSHA re-hashes the actual bytes whenever col.SHA256 is
//     non-empty, verifyPinnedSHA rejects a mismatch, and the evict-and-refetch
//     recovery (prepareWithRecovery) refuses to evict while offline - there is
//     nothing to replace the evicted bytes with. This preview instead reports
//     such a collection as "would install (artifact cached)". That line is
//     still factually true - it states cache presence, not pin validity - and
//     verifying the pin here would require the artifact's actual bytes: on
//     the S3 backend, ArtifactStore.Fetch downloads the whole object to do
//     that, which would make the preview as expensive as the install itself
//     and would falsify dryRunBanner's own promise that no artifacts are
//     downloaded. The preview and the run can therefore disagree in exactly
//     this one dimension - cache presence versus pin validity - and that gap
//     is accepted, not hidden.
//   - warnIfOffServerDownloadHost (install.go). Not evaluated, because it
//     needs a collection's actual artifact metadata (the download URL), which
//     this preview does not otherwise fetch; evaluating it here would add a
//     metadata request per collection to a command whose entire value is
//     being cheap. This is not a new exposure: the dry run downloads nothing
//     from any host, on- or off-server, so there is nothing for the warning
//     to have caught in the first place.
func classifyDryRun(
	ctx context.Context,
	runtime *infra.Infra,
	cfg *config.Config,
	st *store.Store,
	artifacts cacheManager.ArtifactStore,
	collections map[string]collection,
	checkInstalled bool,
) int {
	keys := make([]string, 0, len(collections))
	for key := range collections {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	// classifyOneDryRun can cost a real Has() round trip on the S3 backend
	// (dryRunArtifactCached) and, for an already-installed collection, a full
	// filepath.WalkDir tally pass (checkExtractMarker) - the same per-collection
	// cost a real install already pays, and the dry path has no downloads to
	// hide that latency behind. buildPrefetchTasks already parallelizes the
	// artifact-cache probe for exactly this reason; this does the same for
	// both probes together. Results land in disjoint slots of a pre-sized
	// slice, keyed by index into the already-sorted keys, so the second,
	// sequential pass below still reports in deterministic order regardless
	// of which goroutine finishes first.
	results := make([]dryRunClassification, len(keys))
	var wg sync.WaitGroup
	sem := make(chan struct{}, max(cfg.Workers, 1))
	for i, key := range keys {
		col := collections[key]
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			results[i] = classifyOneDryRun(ctx, cfg, st, artifacts, col, checkInstalled)
		})
	}
	wg.Wait()

	var wouldInstall, upToDate, wouldFail int
	for i, key := range keys {
		res := results[i]
		switch {
		case res.upToDate:
			upToDate++
			runtime.Output.PersistentPrintf("Up to date: %s", key)
		case !res.cached && cfg.Offline:
			wouldFail++
			runtime.Output.Errorf("Would fail: %s (not cached and --offline forbids downloading)", key)
		case res.cached:
			wouldInstall++
			runtime.Output.Okf("Would install: %s (artifact cached)", key)
		default:
			wouldInstall++
			runtime.Output.Okf("Would install: %s (would download)", key)
		}
	}
	runtime.Output.PersistentPrintf(
		"Dry run: %d would install, %d already up to date, %d would fail",
		wouldInstall, upToDate, wouldFail,
	)
	return wouldFail
}

// classifyOneDryRun computes a single collection's dry-run classification:
// whether its on-disk install already satisfies the same two-step gate
// canSkipInstall itself uses - installRecordMatches, then a tally comparison
// - and otherwise whether its artifact is already cached. It is the unit of
// work classifyDryRun's parallel probe pass runs per collection.
//
// installRecordMatches (a handful of os.Stat calls) runs first and gates the
// tree walk: checkExtractMarker's scanTree pass - the same cost a real
// install's canSkipInstall pays for the same collection - is only paid for a
// collection that already looks installed by the cheap check, exactly as
// canSkipInstall itself only calls verifyExtractMarker after its own
// installRecordMatches call passes.
func classifyOneDryRun(
	ctx context.Context,
	cfg *config.Config,
	st *store.Store,
	artifacts cacheManager.ArtifactStore,
	col collection,
	checkInstalled bool,
) dryRunClassification {
	if checkInstalled {
		installPath := collectionInstallPath(cfg, col)
		if installRecordMatches(cfg, col, installPath, st) {
			// installRecordMatches already established a well-formed
			// InstalledEntry exists for this key, so GetInstalled succeeding
			// here is not itself in question - only its extract marker still
			// matching is.
			if entry, ok := st.GetInstalled(col.key()); ok && checkExtractMarker(installPath, entry.ArtifactSHA256).matches() {
				return dryRunClassification{upToDate: true}
			}
		}
	}
	return dryRunClassification{cached: dryRunArtifactCached(ctx, cfg, artifacts, col)}
}

// dryRunArtifactCached reports whether col's artifact would actually be
// served from cache by a real install attempt, by mirroring isCacheHit's own
// predicate (install.go): NoCache disables cache reads exactly as isCacheHit's
// guard does, and forceDownload never applies here (a dry run never evicts or
// retries, since it never prepares an artifact at all). These two predicates
// must never drift apart - a mirror, not an independent reimplementation. A
// proven case for why this matters: before this guard existed, --no-cache
// with a warm cache made this function say "artifact cached" while
// isCacheHit itself returned false, so a real install would still download
// despite the dry run's optimistic report. cfg is never nil here - classifyDryRun,
// this function's only caller, already dereferences it unconditionally
// (cfg.Workers, cfg.Offline) before this is ever reached - so, exactly like
// isCacheHit itself, there is no defensive nil check.
func dryRunArtifactCached(ctx context.Context, cfg *config.Config, artifacts cacheManager.ArtifactStore, col collection) bool {
	if cfg.NoCache || artifacts == nil {
		return false
	}
	return artifactExists(ctx, artifacts, col)
}
