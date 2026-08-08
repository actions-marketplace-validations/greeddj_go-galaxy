package collections

import (
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/galaxy/metrics"
)

// writeRunMetrics persists a JSON metrics report when cfg.MetricsFile is set.
// Best-effort: failures are logged but never fail the run.
//
// CacheHits/CacheMisses/BytesDownloaded report 0/0/0 for any run that never
// touched an ArtifactStore, which is every "lock" and "outdated" run by
// construction: neither command opens one - runLock resolves and writes a
// lockfile, and Outdated only reads the lockfile and queries each server's
// metadata, so runtime.Metrics genuinely accumulates nothing during either
// path. This is a truthful zero, not a gap in this report.
//
// The totals read here are best-effort on a failed run. runInstall registers
// `defer plan.prefetch.Close()`, so on a successful run every prefetch task
// has already been joined through Wait before this function runs, and the
// totals are complete. On a failed run, though, installLevels breaks out of
// its level loop before scheduling any later level, while the prefetcher may
// already have background workers in flight for that unscheduled level; such
// a worker can still land its own miss and bytes after Totals() is read here
// (Close only cancels and joins it afterward, once runInstall unwinds), so a
// failed run's counters in this report are a lower bound, not an exact count.
//
// frozen is passed by the caller rather than read from cfg because the
// report's frozen field states what the run HONORED, not what was
// CONFIGURED: install and warm route resolution through
// resolveOrLoadLockfile, which branches on cfg.Frozen, so they pass it
// through; lock honors the flag too - lockFrozen gates a fresh resolve
// against the on-disk lockfile instead of consuming it over the network the
// way install/warm do, but it is still --frozen actually changing what this
// run does - so its call sites pass cfg.Frozen through as well, and honored
// and configured coincide there exactly as they do for install and warm.
// Outdated is the opposite case: it passes a literal false unconditionally,
// because it never honors --frozen at all - the lockfile is already its only
// source of the locked side and the server is always asked for the latest,
// with or without the flag - so honored and configured genuinely differ
// there, unlike every other caller of this function. Offline stays
// cfg-derived ON PURPOSE - it governs the HTTP transport for every command,
// lock and outdated included - so cfg.Offline is always the truth there.
func writeRunMetrics(
	cfg *config.Config,
	runtime *infra.Infra,
	command string,
	start time.Time,
	collections, failures int,
	frozen bool,
) {
	if cfg == nil || cfg.MetricsFile == "" {
		return
	}
	// metrics.Report has no field distinguishing a dry run from a real one, so
	// a dry run's report is indistinguishable from an install that actually
	// happened - the same class of untruth as a report claiming a run honored
	// a flag it never honored. Worse, tryLockfileHash below reads the
	// lockfile fresh off disk, so a dry run's report would pair an OLD
	// lockfile hash with a NEW resolve's counts. This guard lives inside
	// writeRunMetrics itself, not at each call site, so install, warm, and
	// lock all inherit it for free.
	if cfg.DryRun {
		runtime.Output.Warnf("--dry-run: skipping metrics report to %s", cfg.MetricsFile)
		return
	}
	now := time.Now()
	totals := runtime.Metrics.Totals()
	report := metrics.Report{
		Command:         command,
		StartedAt:       start.UTC(),
		FinishedAt:      now.UTC(),
		Duration:        now.Sub(start),
		CacheHits:       totals.CacheHits,
		CacheMisses:     totals.CacheMisses,
		BytesDownloaded: totals.BytesDownloaded,
		Collections:     collections,
		Failures:        failures,
		Server:          cfg.Server,
		Frozen:          frozen,
		Offline:         cfg.Offline,
		LockfilePath:    lockfile.ResolveDefaultPath(cfg.RequirementsFile, cfg.LockFile),
		LockfileHash:    tryLockfileHash(cfg),
	}
	if err := metrics.Write(cfg.MetricsFile, report); err != nil {
		runtime.Output.Printf("⚠️ Failed to write metrics %s: %v", cfg.MetricsFile, err)
	}
}

func tryLockfileHash(cfg *config.Config) string {
	path := lockfile.ResolveDefaultPath(cfg.RequirementsFile, cfg.LockFile)
	lf, err := lockfile.Load(path)
	if err != nil {
		return ""
	}
	hash, err := lf.Hash()
	if err != nil {
		return ""
	}
	return hash
}
