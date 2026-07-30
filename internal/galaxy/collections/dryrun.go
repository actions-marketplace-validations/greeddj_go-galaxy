package collections

import (
	"context"
	"os"
	"sort"
	"strings"
	"sync"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/extracted"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// dryRunBanner announces, once per run, that --dry-run is active. It always
// goes out through Warnf - stderr, survives --quiet - rather than Printf or
// PersistentPrintf: --dry-run is GO_GALAXY_DRY_RUN-sourced, so an org-wide CI
// environment block can silently turn every install or warm into a no-op with
// nothing else in the log to say so. A banner that could itself be
// suppressed by the very same environment that caused it would defeat the
// point of having one.
//
// The trailing clause names the one write a dry run does NOT suppress:
// saveDryRunSnapshotIfPersisted still saves the resolved metadata caches
// (APICache/DepsCache/Versions) when a persisted snapshot already existed.
// A banner that said only "nothing will be cached" would be false in exactly
// the dimension this whole design turns on - the split between a command's
// product (artifacts, the extracted tree, the warmed set) and pure,
// reconstructible cache - so the banner states that split instead of
// glossing over it.
func dryRunBanner(runtime *infra.Infra) {
	runtime.Output.Warnf(
		"--dry-run is active: no artifact will be downloaded, installed, or cached; the resolved metadata caches are still saved",
	)
}

// dryRunClassification is one collection's dry-run verdict, computed by the
// parallel probe pass in classifyDryRun and consumed by its sequential report
// pass. settled and cached are mutually meaningful only in that order: a
// caller must check settled first, exactly as the report pass does.
type dryRunClassification struct {
	// settled means this command's product already exists for this
	// collection - for install, an install record whose tree still matches
	// its extract marker; for warm, a cached artifact whose extracted tree is
	// present under a known sha.
	settled bool
	cached  bool
}

// dryRunProbe answers one collection's dry-run verdict without mutating
// anything. Each command supplies its own, closing over exactly the state its
// verdict depends on, which is what keeps classifyDryRun's own signature free
// of every command's inputs.
type dryRunProbe func(ctx context.Context, col collection) dryRunClassification

// dryRunVerbs holds the wording one command's dry-run report uses, so
// classifyDryRun's own report pass stays command-agnostic while install and
// warm read differently. Fields are nouns describing what would happen, not
// format strings, and are always set by name.
type dryRunVerbs struct {
	// settled is the tier-report verb for a collection whose product already
	// exists (install: "Up to date"; warm: "Already warm").
	settled string
	// action is the tier-report verb for a collection that would still need
	// work (install: "Would install"; warm: "Would warm").
	action string
	// summaryAction is the lowercase noun phrase used in the trailing summary
	// line's would-act count (install: "would install"; warm: "would warm").
	summaryAction string
	// summarySettled is the lowercase noun phrase used in the trailing
	// summary line's settled count (install: "already up to date"; warm:
	// "already warm").
	summarySettled string
}

// installDryRunVerbs is the wording `install --dry-run` reports with, kept
// byte-identical to the wording this command used before warm's own dry run
// existed.
//
//nolint:gochecknoglobals // a fixed, immutable wording table, not mutable shared state.
var installDryRunVerbs = dryRunVerbs{
	settled:        "Up to date",
	action:         "Would install",
	summaryAction:  "would install",
	summarySettled: "already up to date",
}

// warmDryRunVerbs is the wording `warm --dry-run` reports with, declared
// adjacent to installDryRunVerbs so the two commands' wording is diffable in
// one place.
//
//nolint:gochecknoglobals // a fixed, immutable wording table, not mutable shared state.
var warmDryRunVerbs = dryRunVerbs{
	settled:        "Already warm",
	action:         "Would warm",
	summaryAction:  "would warm",
	summarySettled: "already warm",
}

// classifyDryRun reports, without mutating anything, what a command would do
// for every collection in collections, driven entirely by probe - the
// per-collection verdict function the caller supplies - and returns how many
// of them would fail outright (see below). verbs supplies the report's
// wording, letting install and warm share this one pass instead of each
// hand-rolling their own.
//
// The would-fail count covers exactly one case: a collection that is not
// settled, has no cached artifact, and cannot be downloaded because
// --offline is set, which would fail fetchArtifact's own offline guard on a
// real run (install and warm both funnel through it) - reporting it as
// "would act" here would be the one lie a preview command cannot afford,
// claiming success for a certain failure. This is not a general guarantee
// that a nonzero return means the run will fail and a zero return means it
// will succeed: the !res.cached && cfg.Offline arm is unreachable once
// res.settled is true, and neither probe evaluates pin validity against a
// cached artifact's actual bytes - installDryRunProbe's own pin bullet and
// warmDryRunSHA's doc comment both cover the resulting gap and why closing
// it is rejected, and are not restated here.
//
// A settled verdict under --frozen --offline is the one case where that gap
// can turn into an outright failure the preview cannot see, so the report
// pass below is preceded by a one-time operator warning under exactly that
// combination.
func classifyDryRun(
	ctx context.Context,
	runtime *infra.Infra,
	cfg *config.Config,
	collections map[string]collection,
	verbs dryRunVerbs,
	probe dryRunProbe,
) int {
	keys := make([]string, 0, len(collections))
	for key := range collections {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	// probe can cost a real Has() round trip on the S3 backend
	// (dryRunArtifactCached) and, for install, a full filepath.WalkDir tally
	// pass (checkExtractMarker) - the same per-collection cost a real install
	// already pays, and the dry path has no downloads to hide that latency
	// behind. buildPrefetchTasks already parallelizes the artifact-cache
	// probe for exactly this reason; this does the same for both probes
	// together. Results land in disjoint slots of a pre-sized slice, keyed by
	// index into the already-sorted keys, so the second, sequential pass
	// below still reports in deterministic order regardless of which
	// goroutine finishes first.
	results := make([]dryRunClassification, len(keys))
	var wg sync.WaitGroup
	sem := make(chan struct{}, max(cfg.Workers, 1))
	for i, key := range keys {
		col := collections[key]
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			results[i] = probe(ctx, col)
		})
	}
	wg.Wait()
	warnIfFrozenOffline(runtime, cfg)

	var wouldAct, settled, wouldFail int
	for i, key := range keys {
		res := results[i]
		switch {
		case res.settled:
			settled++
			runtime.Output.PersistentPrintf("%s: %s", verbs.settled, key)
		case !res.cached && cfg.Offline:
			wouldFail++
			runtime.Output.Errorf("Would fail: %s (not cached and --offline forbids downloading)", key)
		case res.cached:
			wouldAct++
			runtime.Output.Okf("%s: %s (artifact cached)", verbs.action, key)
		default:
			wouldAct++
			runtime.Output.Okf("%s: %s (would download)", verbs.action, key)
		}
	}
	runtime.Output.PersistentPrintf(
		"Dry run: %d %s, %d %s, %d would fail",
		wouldAct, verbs.summaryAction, settled, verbs.summarySettled, wouldFail,
	)
	return wouldFail
}

// warnIfFrozenOffline emits classifyDryRun's one-time --frozen --offline
// disclosure, factored out to keep classifyDryRun itself under the
// cyclomatic complexity budget. Warnf, not Printf: it must survive --quiet
// and reach stderr, matching every other integrity-adjacent signal in this
// tree, and its wording deliberately does not contain the banner's
// "--dry-run is active" substring, so a test counting banner occurrences
// never double-counts this warning.
func warnIfFrozenOffline(runtime *infra.Infra, cfg *config.Config) {
	if !cfg.Frozen || !cfg.Offline {
		return
	}
	runtime.Output.Warnf(
		"--frozen --offline: this preview checks that an artifact is cached, not that it still matches its pin; " +
			"a cached artifact whose bytes drifted will fail the real run, which cannot refetch while offline",
	)
}

// installDryRunProbe returns install's dry-run verdict function, closing over
// the store and artifact cache a real install would consult.
//
// What it evaluates, in full: the installed-record check (installRecordMatches),
// the extract-marker tally (checkExtractMarker), artifact-cache presence
// (dryRunArtifactCached, mirroring isCacheHit), and offline reachability
// (handled by classifyDryRun itself). Two things a real install also checks
// are deliberately NOT evaluated here:
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
//
// installRecordMatches (a handful of target.root.Stat calls) runs first and
// gates the tree walk: checkExtractMarker's scanTree pass - the same cost a
// real install's canSkipInstall pays for the same collection - is only paid
// for a collection that already looks installed by the cheap check, exactly
// as canSkipInstall itself only calls verifyExtractMarker after its own
// installRecordMatches call passes. This still does not call canSkipInstall
// directly: canSkipInstall's own verifyExtractMarker wraps that same tally
// comparison with logging and a best-effort removal of a drifted marker, and
// a preview must never delete state as a side effect of describing it.
// checkExtractMarker is the pure, read-only half of that wrapper - no
// logging, no deletion - built for exactly this caller.
//
// root is nil whenever cfg.DownloadPath does not exist yet (openCollectionsRoot's
// own dry-run contract: a dry run never creates the directory it is only
// describing), or when it was never opened at all. newInstallTarget's own
// nil-root guard then makes every collection report ok=false here, which
// this probe treats as "not settled" - pessimistic, matching warmDryRunSHA's
// own stated convention, never optimistic - rather than as an error.
func installDryRunProbe(cfg *config.Config, st *store.Store, artifacts cacheManager.ArtifactStore, root *os.Root) dryRunProbe {
	return func(ctx context.Context, col collection) dryRunClassification {
		if target, ok := newInstallTarget(root, cfg, col); ok && installRecordMatches(target, col, st) {
			// installRecordMatches already established a well-formed
			// InstalledEntry exists for this key, so GetInstalled succeeding
			// here is not itself in question - only its extract marker still
			// matching is.
			if entry, ok := st.GetInstalled(col.key()); ok && checkExtractMarker(target, entry.ArtifactSHA256).matches() {
				return dryRunClassification{settled: true}
			}
		}
		return dryRunClassification{cached: dryRunArtifactCached(ctx, cfg, artifacts, col)}
	}
}

// warmDryRunProbe returns warm's dry-run verdict function, closing over the
// artifact cache, the extracted store, and the persisted warmed set a real
// warm would consult.
//
// Warm's product is the cache itself, so a collection is settled only when
// BOTH halves of that product already exist: the tarball in the artifact
// store AND its extracted tree materialized in the content-addressable
// store. Cache presence alone is not enough and reporting it as such would
// be the one lie a preview cannot afford. The two stores are independent by
// construction: with --s3-bucket the artifact store is remote while
// newExtractStore is always local to cfg.CacheDir, so a fresh runner against
// a warm bucket has every artifact "cached" and not one extracted tree -
// exactly the run whose whole cost this preview exists to report. The
// extracted half needs a sha, and learning a cached artifact's sha from the
// artifact store would cost ArtifactStore.Fetch - a full object download on
// the S3 backend - which would make the preview as expensive as the run and
// falsify the banner's own promise. warmDryRunSHA therefore names the sha
// from the two sources already in hand, and a collection whose sha cannot be
// named is reported as "would warm" rather than guessed at: pessimistic,
// never optimistic.
func warmDryRunProbe(
	cfg *config.Config, artifacts cacheManager.ArtifactStore, extractStore *extracted.Store, warmed map[string]string,
) dryRunProbe {
	return func(ctx context.Context, col collection) dryRunClassification {
		if !dryRunArtifactCached(ctx, cfg, artifacts, col) {
			return dryRunClassification{}
		}
		return dryRunClassification{cached: true, settled: extractStore.Ready(warmDryRunSHA(col, warmed))}
	}
}

// warmDryRunSHA names the sha warm's dry-run probe should check the extracted
// store under, preferring a non-empty lockfile pin (col.SHA256) over the
// snapshot's own warmed record.
//
// A lockfile pin is the sha the run is required to end at: resolveArtifactSHA
// re-hashes the real bytes under a non-empty pin and verifyPinnedSHA rejects
// a mismatch, so a frozen run's actual product lives under that sha, not
// whatever an older warm happened to record. The warmed entry is the
// snapshot's own claim about what a previous warm materialized, and is the
// only source available on an unpinned run.
//
// This reports cache presence, not content validity, matching the same gap
// installDryRunProbe already accepts for install's pin check. Concretely: a
// cached tarball whose bytes drift in place while its sidecar and extracted
// tree are left untouched still names the same sha here, so this preview
// still reports "Already warm". Online, that gap costs nothing but latency -
// prepareWithRecovery's bounded evict-and-refetch silently repairs it on the
// real run. Under --offline, it costs correctness: canRetryCacheHit refuses
// to evict while offline, since there is nothing to replace the evicted
// bytes with, so the real run fails closed with helpers.ErrSHA256Mismatch
// while this preview reported "Already warm" and a would-fail count of zero.
// That escalation from cost to failure is accepted for the same reason
// install's pin gap already is: detecting it requires re-hashing the
// artifact's actual bytes, which on the S3 backend means ArtifactStore.Fetch
// downloading the whole object - the exact cost this preview exists to
// avoid. A recorded sidecar sha cannot substitute for that hash either: the
// sidecar is precisely what the drifted bytes no longer match, so trusting
// it here would not detect the drift it exists to catch. classifyDryRun's
// own --frozen --offline warning (see its doc comment) is this gap's only
// mitigation, and it is a disclosure, not a fix.
func warmDryRunSHA(col collection, warmed map[string]string) string {
	if sha := strings.TrimSpace(col.SHA256); sha != "" {
		return sha
	}
	return warmed[col.key()]
}

// dryRunArtifactCached reports whether col's artifact would actually be
// served from cache by a real install/warm attempt, by mirroring isCacheHit's
// own predicate (install.go): NoCache disables cache reads exactly as
// isCacheHit's guard does, and forceDownload never applies here (a dry run
// never evicts or retries, since it never prepares an artifact at all). These
// two predicates must never drift apart - a mirror, not an independent
// reimplementation. A proven case for why this matters: before this guard
// existed, --no-cache with a warm cache made this function say "artifact
// cached" while isCacheHit itself returned false, so a real install would
// still download despite the dry run's optimistic report. cfg is never nil
// here - classifyDryRun's own callers already dereference it unconditionally
// (cfg.Workers, cfg.Offline) before this is ever reached - so, exactly like
// isCacheHit itself, there is no defensive nil check.
func dryRunArtifactCached(ctx context.Context, cfg *config.Config, artifacts cacheManager.ArtifactStore, col collection) bool {
	if cfg.NoCache || artifacts == nil {
		return false
	}
	return artifactExists(ctx, artifacts, col)
}
