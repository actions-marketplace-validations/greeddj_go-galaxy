package collections

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"sync"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/extracted"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
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
// pass. fail, settled, and cached are evaluated in exactly the order
// reportDryRunResults' own switch checks them - settled, then the offline
// rule (!cached && cfg.Offline), then fail - which mirrors the order a real
// run reaches the equivalent failures: the command's product is checked
// first (skipped before any network or filesystem write), the offline guard
// fires before an artifact already in hand is ever acted on, and only once
// both of those are past does a pin mismatch or a collections-tree escape
// get a chance to fire, since a real run only detects either one after the
// artifact is already in hand (a pin is checked against resolved bytes; a
// tree escape is checked while extracting).
type dryRunClassification struct {
	// fail, when non-nil, is this preview's refusal to report the collection
	// as actionable: a lockfile pin the cached artifact's recorded digest
	// contradicts with no way to refetch (dryRunPinVerdict - see its own doc
	// comment for the one measured shape where a real run still succeeds), or
	// a collections-tree write a real install would refuse (dryRunNamespaceProbe,
	// installDryRunProbe's own newInstallTarget ok=false arm). Placed first in
	// this struct for fieldalignment: an error is a two-word interface, the
	// widest field here.
	fail error
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
// per-collection verdict function the caller supplies - and returns a
// failureSummary of every collection that would fail outright (see below).
// verbs supplies the report's wording, letting install and warm share this
// one pass instead of each hand-rolling their own.
//
// The failure summary covers two cases. A collection that is not settled, has
// no cached artifact, and cannot be downloaded because --offline is set is a
// certain failure: fetchArtifact's own offline guard would raise it on a real
// run, and install and warm both funnel through it. A collection whose probe
// itself returned a non-nil fail is a refusal rather than a prediction
// (dryRunClassification.fail's own doc comment names its producers, and
// dryRunPinVerdict's own names the one measured shape where a real run still
// succeeds). Reporting either as "would act" would be the one lie a preview
// command cannot afford. The summary is not a two-way guarantee in either
// direction: neither producer of fail evaluates pin validity against a cached
// artifact's actual bytes - dryRunPinVerdict's and warmDryRunSHA's own doc
// comments cover both directions and why closing either is rejected, and are
// not restated here.
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
) failureSummary {
	keys := make([]string, 0, len(collections))
	for key := range collections {
		keys = append(keys, key)
	}
	slices.Sort(keys)

	// probe can cost a real Meta() round trip on the S3 backend
	// (dryRunArtifactMeta) and, for install, a full filepath.WalkDir tally
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

	var failures failureRecorder
	wouldAct, settled := reportDryRunResults(runtime, verbs, keys, results, cfg, &failures)
	summary := failures.summary()
	runtime.Output.PersistentPrintf(
		"Dry run: %d %s, %d %s, %d would fail",
		wouldAct, verbs.summaryAction, settled, verbs.summarySettled, summary.count,
	)
	return summary
}

// reportDryRunResults renders classifyDryRun's per-collection verdicts, in
// sorted key order, recording every would-fail cause into failures. Factored
// out of classifyDryRun to keep its own cyclomatic complexity under budget,
// mirroring warnIfFrozenOffline's own extraction for the same reason.
//
// Switch-case order is LOAD-BEARING, mirroring the order a real run reaches
// the equivalent failures: settled first (the command's product already
// exists, so a real run skips this collection before either an offline
// check or a collections-tree write ever runs); then !res.cached &&
// cfg.Offline (fetchArtifact's own offline guard, which fires before any
// artifact is ever acted on); then res.fail != nil (a pin mismatch or a
// collections-tree escape, both detected only once an artifact is already in
// hand - after the point where the offline guard would already have fired
// instead, on a real run that never got that far); then res.cached (a real
// cache hit with nothing else wrong); then the default download case. The
// probe-failure case's own error is never asked to repeat key: the record
// and the printed line both prepend it themselves, so a probe's own error
// message stays focused on what went wrong rather than restating which
// collection it is about.
func reportDryRunResults(
	runtime *infra.Infra,
	verbs dryRunVerbs,
	keys []string,
	results []dryRunClassification,
	cfg *config.Config,
	failures *failureRecorder,
) (int, int) {
	var wouldAct, settled int
	for i, key := range keys {
		res := results[i]
		switch {
		case res.settled:
			settled++
			runtime.Output.PersistentPrintf("%s: %s", verbs.settled, key)
		case !res.cached && cfg.Offline:
			failures.record(fmt.Errorf("%s: %w: artifact not in cache", key, helpers.ErrOfflineMode))
			runtime.Output.Errorf("Would fail: %s (not cached and --offline forbids downloading)", key)
		case res.fail != nil:
			failures.record(fmt.Errorf("%s: %w", key, res.fail))
			runtime.Output.Errorf("Would fail: %s (%v)", key, res.fail)
		case res.cached:
			wouldAct++
			runtime.Output.Okf("%s: %s (artifact cached)", verbs.action, key)
		default:
			wouldAct++
			runtime.Output.Okf("%s: %s (would download)", verbs.action, key)
		}
	}
	return wouldAct, settled
}

// warnIfFrozenOffline emits classifyDryRun's one-time --frozen --offline
// disclosure, factored out to keep classifyDryRun itself under the
// cyclomatic complexity budget. Warnf, not Printf: it must survive --quiet
// and reach stderr, matching every other integrity-adjacent signal in this
// tree, and its wording deliberately does not contain the banner's
// "--dry-run is active" substring, so a test counting banner occurrences
// never double-counts this warning.
//
// dryRunPinVerdict already checks the cached artifact's recorded digest
// against the lockfile pin, so a drift the recorded digest itself reflects is
// reported as a would-fail collection. What remains uncaught, and what this
// warning discloses, is bytes drifting in place while the recorded digest
// stays exactly as it was - checking that would mean re-hashing the
// artifact's actual bytes, which on the S3 backend means downloading the
// whole object, the cost this preview exists to avoid.
func warnIfFrozenOffline(runtime *infra.Infra, cfg *config.Config) {
	if !cfg.Frozen || !cfg.Offline {
		return
	}
	runtime.Output.Warnf(
		"--frozen --offline: this preview checks the cached artifact's recorded digest against the pin, not its actual bytes; " +
			"bytes that drifted without updating that recorded digest will still fail the real run, which cannot refetch while offline",
	)
}

// installDryRunProbe returns install's dry-run verdict function, closing over
// the store and artifact cache a real install would consult.
//
// What it evaluates, in full: the installed-record check (installRecordMatches),
// the extract-marker tally (checkExtractMarker), the lockfile-pin-versus-recorded-digest
// verdict (dryRunPinVerdict), artifact-cache presence (dryRunArtifactMeta,
// mirroring isCacheHit), the collections-tree write a real install's
// extraction step would attempt (dryRunNamespaceProbe, and this function's
// own newInstallTarget ok=false arm below it), and offline reachability
// (handled by classifyDryRun itself). One thing a real install also checks
// is deliberately NOT evaluated here:
//
//   - warnIfOffServerDownloadHost (install.go). Not evaluated, because it
//     needs a collection's actual artifact metadata (the download URL), which
//     this preview does not otherwise fetch; evaluating it here would add a
//     metadata request per collection to a command whose entire value is
//     being cheap. This is not a new exposure: the dry run downloads nothing
//     from any host, on- or off-server, so there is nothing for the warning
//     to have caught in the first place.
//
// Evaluation order, and why it mirrors the order a real install reaches the
// equivalent checks: installRecordMatches (a handful of target.root.Stat
// calls) runs first and gates the tree walk - checkExtractMarker's scanTree
// pass, the same cost a real install's canSkipInstall pays for the same
// collection, is only paid for a collection that already looks installed by
// the cheap check, exactly as canSkipInstall itself only calls
// verifyExtractMarker after its own matchingInstalledRecord call passes. A
// settled verdict returns immediately, without ever reaching the pin check
// below it: installRecordMatches already requires entry.ArtifactSHA256 ==
// col.SHA256 (when a pin exists), so a settled collection is already
// pin-consistent, exactly mirroring the real run, which skips a settled
// collection (canSkipInstall) without ever opening the tarball to check its
// pin at all. Only once settled is ruled out does the pin predicate run,
// followed by the namespace probe - a real install detects a pin mismatch
// (verifyAndExtract's own verifyPinnedSHA call) strictly before it ever
// attempts the collections-tree write a pin-consistent artifact would then
// go on to make (extractCollection), so this probe checks them in the same
// order. This still does not call canSkipInstall directly for the settled
// check: canSkipInstall's own verifyExtractMarker wraps that same tally
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
// own stated convention, never optimistic - rather than as an error; and,
// specifically for the ok=false arm below, as "nothing to check" rather than
// a would-fail collections-tree escape, since a nil root means downloadPath
// itself does not exist yet, and a real run would create the entire tree
// from scratch rather than refuse any part of it. A non-nil root with
// ok=false, by contrast, means col's own namespace/name/version failed
// helpers.IsPathElement - buildCollectionsMap already rejects that
// identifier before any collection ever reaches a probe, so this arm is
// unreachable in production and exists only so newInstallTarget's contract
// is total: every ok=false outcome this probe can observe has an explicit,
// documented verdict rather than an implicit fallthrough.
func installDryRunProbe(cfg *config.Config, st *store.Store, artifacts cacheManager.ArtifactStore, root *os.Root) dryRunProbe {
	return func(ctx context.Context, col collection) dryRunClassification {
		target, ok := newInstallTarget(root, cfg, col)
		if ok && installRecordMatches(target, col, st) {
			// installRecordMatches already established a well-formed
			// InstalledEntry exists for this key, so GetInstalled succeeding
			// here is not itself in question - only its extract marker still
			// matching is.
			if entry, entryOK := st.GetInstalled(col.key()); entryOK && checkExtractMarker(target, entry.ArtifactSHA256).matches() {
				return dryRunClassification{settled: true}
			}
		}

		meta, cached := dryRunArtifactMeta(ctx, cfg, artifacts, col)
		if err := dryRunPinVerdict(cfg, cached, meta, col); err != nil {
			return dryRunClassification{cached: cached, fail: err}
		}
		if !ok {
			if root == nil {
				return dryRunClassification{cached: cached}
			}
			return dryRunClassification{cached: cached, fail: fmt.Errorf(
				"%w: ns=%q name=%q version=%q", helpers.ErrUnsafeCollectionIdentifier, col.Namespace, col.Name, col.Version,
			)}
		}
		if err := dryRunNamespaceProbe(target); err != nil {
			return dryRunClassification{cached: cached, fail: err}
		}
		return dryRunClassification{cached: cached}
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
//
// The pin predicate (dryRunPinVerdict) runs before extractStore.Ready and,
// when it fires, OVERRIDES a settled verdict rather than merely preceding it -
// this function returns before ever calling Ready, so a collection whose pin
// no longer matches its recorded digest is never reported settled even when
// its extracted tree genuinely is ready. This is the opposite order from
// installDryRunProbe, which checks settled first and returns immediately on
// a match, and the asymmetry is deliberate: a real warm's own verifyPinnedSHA
// call (warmVerifyAndEnsure) fires before Ensure ever populates or reuses the
// extracted tree, so a pin mismatch fails the real run before warm's own
// settled condition is even evaluated - unlike install, where
// installRecordMatches already implies pin consistency by construction (see
// installDryRunProbe's own doc comment), so there is no equivalent order to
// preserve there.
func warmDryRunProbe(
	cfg *config.Config, artifacts cacheManager.ArtifactStore, extractStore *extracted.Store, warmed map[string]string,
) dryRunProbe {
	return func(ctx context.Context, col collection) dryRunClassification {
		meta, cached := dryRunArtifactMeta(ctx, cfg, artifacts, col)
		if !cached {
			return dryRunClassification{}
		}
		if err := dryRunPinVerdict(cfg, cached, meta, col); err != nil {
			return dryRunClassification{cached: true, fail: err}
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

// dryRunArtifactMeta reports col's cached artifact metadata, and whether it
// would actually be served from cache by a real install/warm attempt, by
// mirroring isCacheHit's own predicate (install.go): NoCache disables cache
// reads exactly as isCacheHit's guard does, and forceDownload never applies
// here (a dry run never evicts or retries, since it never prepares an
// artifact at all). These two predicates must never drift apart - a mirror,
// not an independent reimplementation. A proven case for why this matters:
// without this mirroring, --no-cache with a warm cache would make this
// function say "cached" even though isCacheHit itself returns false, so a
// real install would still download despite the dry run's optimistic report.
// cfg is never nil here - classifyDryRun's own callers already dereference
// it unconditionally (cfg.Workers, cfg.Offline) before this is ever reached -
// so, exactly like isCacheHit itself, there is no defensive nil check.
//
// This is backed by exactly one ArtifactStore.Meta call. On the S3 backend,
// Meta and Has share one headArtifact call (see internal/cache/s3/artifacts.go),
// so this still pays exactly the single round trip a bare presence probe
// always did, while additionally handing dryRunPinVerdict the recorded digest
// it needs. The found bool this returns is required to equal what Has would
// have reported for the identical key, which is what preserves this
// function's own mirror of isCacheHit: a backend whose Meta disagreed with
// its own Has about presence would silently break that mirror, and nothing
// here would catch it - internal/cache/local and internal/cache/s3's own
// Meta tests each assert this equality directly instead.
func dryRunArtifactMeta(
	ctx context.Context, cfg *config.Config, artifacts cacheManager.ArtifactStore, col collection,
) (map[string]string, bool) {
	if cfg.NoCache || artifacts == nil {
		return nil, false
	}
	meta, found, err := artifacts.Meta(ctx, artifactKey(col))
	if err != nil || !found {
		return nil, false
	}
	return meta, true
}

// dryRunPinVerdict reports the one certain-failure verdict shared by
// install's and warm's dry-run probes: a lockfile pin that the cached
// artifact's own recorded digest already contradicts, with --offline set so
// a real run has no way to refetch a replacement. It returns nil - "no
// verdict" - far more often than it returns an error, and every "no verdict"
// arm has its own reason:
//
//   - col.SHA256 is empty: nothing is pinned, so there is nothing to
//     contradict.
//   - !cached: nothing recorded to compare against; classifyDryRun's own
//     !cached && cfg.Offline case already reports this collection as a
//     would-fail on its own, through a different, offline-specific message.
//   - the recorded digest is empty: the cached artifact carries no sidecar
//     metadata (a tarball committed by a binary older than the sidecar
//     mechanism, or a cache-hit whose Meta genuinely has nothing to report),
//     so there is nothing to compare against either.
//   - the recorded digest is not helpers.IsSHA256Hex: a malformed recorded
//     digest is deliberately not itself reported as a verdict here. Its
//     consequence is backend-specific - resolveArtifactSHA would fail closed
//     with helpers.ErrMalformedArtifactSHA256 on a real run that reaches it -
//     and branching this preview's verdict on which backend produced the
//     malformed value would leak a persistence detail across the Backend
//     seam this function is on the wrong side of to make that call.
//   - the recorded digest equals the pin: the cached artifact is exactly
//     what the lockfile expects; nothing would fail.
//   - !cfg.Offline: a real run's own canRetryCacheHit can still evict and
//     refetch a mismatched cache hit while online, silently repairing this
//     exact condition, so reporting a certain failure here would be wrong -
//     online, this is a cost (one wasted refetch), never a failure.
//
// Only once every one of those is ruled out - a real pin, a real cache hit,
// a real and well-formed recorded digest that disagrees with the pin, and no
// way to refetch - does this report a would-fail.
//
// What that verdict is, and is not: a refusal, never a prediction. When the
// recorded digest is the tarball's own digest - the ordinary case, and the
// one the e2e fixtures cover - a real --frozen --offline run fails at
// verifyPinnedSHA for exactly this reason and this preview agrees with it.
// When the recorded digest itself was altered while the bytes were left
// intact, the two disagree: under a non-empty pin resolveArtifactSHA
// re-hashes the actual bytes and never reads the recorded digest at all, so
// the local backend installs the collection cleanly while this reports a
// would-fail (measured: exit 0 for real, exit 7 in the preview; the S3
// backend does re-check its recorded digest on read, so it fails there as
// this predicts). The verdict is kept in that shape deliberately - a cached
// artifact whose recorded digest disagrees with its own bytes is damaged -
// but it must not be described as a certain failure, because it is not one.
func dryRunPinVerdict(cfg *config.Config, cached bool, meta map[string]string, col collection) error {
	pin := strings.TrimSpace(col.SHA256)
	if pin == "" {
		return nil
	}
	if !cached {
		return nil
	}
	recorded := strings.TrimSpace(meta["sha256"])
	if recorded == "" {
		return nil
	}
	if !helpers.IsSHA256Hex(recorded) {
		return nil
	}
	if recorded == pin {
		return nil
	}
	if !cfg.Offline {
		return nil
	}
	return fmt.Errorf(
		"%w: the cached artifact's recorded digest does not match the lockfile pin and --offline forbids refetching",
		helpers.ErrSHA256Mismatch,
	)
}

// dryRunNamespaceProbe checks, without creating, modifying, or removing
// anything, whether a real install's extraction step would succeed writing
// under target's namespace directory (ansible_collections/<namespace>) -
// extractCollection's own RemoveAll-then-MkdirAll of target.rel, one level
// below the namespace directory this checks. It is called only when
// newInstallTarget already returned ok=true, so target.root and target.rel
// are both known safe to use.
//
// This is Stat-ONLY, deliberately not Lstat-aware - the opposite rule from
// probeAnsibleCollectionsUsable (installroot.go), and for a structural
// reason: ansible_collections is created with a bare root.MkdirAll and
// nothing else, so a dangling symlink there defeats MkdirAll outright (there
// is no existing valid directory for it to no-op against), which is why that
// probe needs Lstat to tell a dangling symlink apart from a genuinely absent
// entry. A namespace directory, by contrast, is always reached through
// RemoveAll(target.rel) first and only then MkdirAll(target.rel, ...); when
// the namespace component is a dangling symlink, RemoveAll resolves it,
// finds nothing at the far end, and succeeds exactly as it would against an
// absent path, after which MkdirAll creates the namespace directory fresh -
// so a real install SUCCEEDS against a dangling namespace symlink (measured
// directly), and Stat's own fs.ErrNotExist for that shape (the symlink
// follows to a target that does not exist) already reports exactly that
// outcome without needing Lstat at all.
//
// The predicate: Stat(nsRel) reporting fs.ErrNotExist is "ok" (nil) - covers
// both a genuinely absent namespace directory and a dangling symlink, per
// the paragraph above. Stat(nsRel) succeeding but reporting something that
// is not a directory (a regular file, most concretely) is classified through
// classifyCollectionsRootError, the identical classifier
// probeAnsibleCollectionsUsable and openCollectionsRoot's own create=true
// branch already use, so an escaping symlink component classifies
// helpers.ErrCollectionsPathEscape and a regular file stays unclassified -
// though for a namespace-level probe result, unlike an ansible_collections-level
// one, the specific classification does not change the collection's own exit
// code: whatever this returns is recorded as a per-collection cause and
// reaches installDryRun's own failureSummary.installError, joining it behind
// helpers.ErrInstallationFailed exactly as a real run's own extraction
// failure would be, so both land on the install exit class regardless of
// which classification fired underneath it. Any
// other Stat failure (a permission error, for instance) is classified the
// same way, on the conservative assumption that an unrecognized failure here
// is closer to "cannot be used" than to "definitely fine". Stat succeeding
// and reporting a directory is "ok" (nil) - covers both a real, pre-existing
// namespace directory and an in-root relative symlink to one, since Stat
// follows a symlink to what it actually points at.
func dryRunNamespaceProbe(target installTarget) error {
	nsRel := path.Dir(target.rel)
	info, err := target.root.Stat(nsRel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return classifyCollectionsRootError(target.root, nsRel, errCollectionsTreeNotUsable)
	}
	if !info.IsDir() {
		return classifyCollectionsRootError(target.root, nsRel, errCollectionsTreeNotUsable)
	}
	return nil
}
