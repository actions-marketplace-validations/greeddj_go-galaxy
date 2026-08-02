package collections

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
)

// errNoHighestVersion is lookupOutdated's plain local error for a root
// metadata document that answered but named no highest_version. It carries
// no sentinel of its own, deliberately: this failure classifies the same way
// every other lookup failure does, through outdated's own aggregation
// headline (helpers.ErrLatestVersionLookupFailed), so a dedicated sentinel
// would add a name nothing needs to match on.
var errNoHighestVersion = errors.New("no highest_version in metadata")

// outdatedEntry summarizes a single collection's locked-vs-latest delta. Err
// is nil on success (whether or not a newer version exists) and non-nil when
// the lookup itself failed - one field rather than a Message string plus a
// Failed bool, which could disagree with each other (a non-empty Message
// with Failed false, or the reverse) in a way a single error value cannot.
type outdatedEntry struct {
	Err    error
	Name   string
	Locked string
	Latest string
	Newer  bool
}

// Outdated loads the lockfile and queries Galaxy for the latest available
// version of every locked collection in parallel, printing a summary report.
// Exit status is non-zero only when at least one query failed.
//
// outdated deliberately opens no cache backend: it never calls
// internal/cache.New, so it never takes the exclusive distributed lock (it
// can run alongside an install or warm against the same cache directory or
// bucket) and every version it reports is a live answer from the server
// rather than one served from cached metadata, which is the whole point of
// asking what is "latest".
func Outdated(ctx context.Context, cfg *config.Config, runtime *infra.Infra) error {
	if cfg == nil {
		// Defensive only: every production call site (cmd/go-galaxy/commands's
		// runCollectionCommand) always builds a non-nil *config.Config before
		// reaching here. Kept so this function is total rather than resting on
		// that invariant holding forever.
		return helpers.ErrConfigIsNil
	}
	if cfg.Offline {
		return fmt.Errorf("%w: outdated requires network access", helpers.ErrOfflineMode)
	}
	// Before loading the lockfile, not after: this disclosure is about the
	// run's own configuration and must not depend on whether the lockfile
	// happens to load.
	warnUnhonoredFlags(runtime, cfg)

	start := time.Now()
	lockPath := lockfile.ResolveDefaultPath(cfg.RequirementsFile, cfg.LockFile)
	lf, err := lockfile.LoadRequired(lockPath)
	if err != nil {
		return err
	}

	results := queryLatestVersions(ctx, cfg, runtime, lf)
	// Sorted once, here, before both the report and the error build below, so
	// the report's line order and the joined-cause order in the returned
	// error agree with each other. reportOutdated itself mutates nothing.
	sort.Slice(results, func(i, j int) bool { return results[i].Name < results[j].Name })

	reportOutdated(runtime, results, lockPath)

	var failures failureRecorder
	for _, r := range results {
		if r.Err != nil {
			failures.record(r.Err)
		}
	}
	summary := failures.summary()
	// outdated's report describes the run's own work - the lookups it
	// performed and how many failed - never the verdict of how many
	// collections are behind: a dashboard branches on the exit code for that,
	// never on a report that can look clean either way. frozen is a literal
	// false, never cfg.Frozen: outdated never honors --frozen (see
	// warnUnhonoredFlags), so passing cfg.Frozen through would re-commit the
	// exact false claim already fixed for `lock`'s own report.
	writeRunMetrics(cfg, runtime, "outdated", start, len(results), int(summary.count), false)
	return summary.outdatedError()
}

// unhonoredFlags returns the human-readable name of every flag this run
// configured that outdated cannot act on, in a fixed order so
// warnUnhonoredFlags' single warning line is deterministic across runs. It
// returns nil - allocating nothing - when the run configured none of them,
// which is the common case; this only allocates on a misconfigured run.
//
// --cache-dir and --download-path are deliberately excluded, for a reason
// distinct from every flag checked below: config.Config records a resolved
// value, not whether the operator actually set the flag, so a defaulted
// string path is indistinguishable at this layer from one explicitly passed.
// Telling them apart would require threading cli.Command.IsSet down from the
// CLI layer into internal/galaxy, which is a layer violation this package
// does not accept.
func unhonoredFlags(cfg *config.Config) []string {
	var names []string
	if cfg.ClearCache {
		names = append(names, "--clear-cache")
	}
	if cfg.NoCache {
		names = append(names, "--no-cache")
	}
	if cfg.Refresh {
		names = append(names, "--refresh")
	}
	if cfg.NoDeps {
		names = append(names, "--no-deps")
	}
	if cfg.Frozen {
		names = append(names, "--frozen")
	}
	if cfg.S3Cache.Enabled {
		names = append(names, "--s3-bucket")
	}
	return names
}

// warnUnhonoredFlags discloses, in at most one stderr line, every flag this
// run configured that outdated cannot honor. This is a design property, not
// an oversight: outdated opens no cache backend at all, so every cache-shaped
// flag has nothing to act on, --no-deps has no dependency graph to skip
// resolving, and --frozen has no meaning left to honor since the lockfile is
// already the only source of the locked side and the server is always asked
// for the latest. The flag is announced rather than rejected as a usage
// error, because the same values commonly arrive from an ambient CI
// environment variable block shared with install/warm/lock, where refusing
// to run over a flag that is merely inert would be the harshest possible
// outcome for the least harmful mistake.
func warnUnhonoredFlags(runtime *infra.Infra, cfg *config.Config) {
	names := unhonoredFlags(cfg)
	if len(names) == 0 {
		return
	}
	runtime.Output.Warnf(
		"outdated does not honor %s; it only reads the lockfile and queries each server for the latest version",
		strings.Join(names, ", "),
	)
}

func queryLatestVersions(ctx context.Context, cfg *config.Config, runtime *infra.Infra, lf *lockfile.File) []outdatedEntry {
	deps := newCollectionDeps(cfg, runtime, nil)
	out := make([]outdatedEntry, len(lf.Collections))
	var wg sync.WaitGroup
	workers := max(cfg.Workers, 1)
	jobs := make(chan int, len(lf.Collections))
	for i := range lf.Collections {
		jobs <- i
	}
	close(jobs)
	for range workers {
		wg.Go(func() {
			for i := range jobs {
				out[i] = lookupOutdated(ctx, deps, lf.Collections[i])
			}
		})
	}
	wg.Wait()
	return out
}

func lookupOutdated(ctx context.Context, deps collectionDeps, e lockfile.Entry) outdatedEntry {
	ns, name, ok := helpers.SplitFQDN(e.Name)
	if !ok {
		return outdatedEntry{
			Name:   e.Name,
			Locked: e.Version,
			Err:    fmt.Errorf("%w: invalid name %q", helpers.ErrLockfileInvalid, e.Name),
		}
	}
	source := e.Source
	if source == "" {
		source = deps.cfg.Server
	}
	col := collection{Namespace: ns, Name: name, Source: source}
	policy := cachePolicyForConstraint(deps.cfg, false)
	root, err := resolveRootMetadata(ctx, deps, col, policy, e.Name)
	if err != nil {
		return outdatedEntry{Name: e.Name, Locked: e.Version, Err: err}
	}
	latest := strings.TrimSpace(root.meta.HighestVersion.Version)
	if latest == "" {
		return outdatedEntry{Name: e.Name, Locked: e.Version, Err: errNoHighestVersion}
	}
	return classifyOutdated(e.Name, e.Version, latest)
}

// classifyOutdated compares locked against latest and builds the resulting
// entry. A version that fails to parse as semver is reported through Err
// (not silently treated as up to date), so the command still exits non-zero
// and the operator sees which entry needs attention.
//
// This function draws no conclusion about which side is more likely to fail:
// it is callable with any two strings and treats both the same way. In
// production, after lockfile.Load has validated every entry's version is
// helpers.IsExactVersion, the locked side is provably parseable by the time
// this runs, so a parse failure here can in practice only be latest - the
// server's own highest_version - but that is a fact about this function's
// one production caller, not an invariant classifyOutdated itself enforces.
func classifyOutdated(name, locked, latest string) outdatedEntry {
	newer, err := isNewerVersion(latest, locked)
	if err != nil {
		return outdatedEntry{Name: name, Locked: locked, Latest: latest, Err: fmt.Errorf("version parse: %w", err)}
	}
	return outdatedEntry{Name: name, Locked: locked, Latest: latest, Newer: newer}
}

func isNewerVersion(latest, locked string) (bool, error) {
	l, err := semver.NewVersion(latest)
	if err != nil {
		return false, err
	}
	c, err := semver.NewVersion(locked)
	if err != nil {
		return false, err
	}
	return l.GreaterThan(c), nil
}

// reportOutdated prints one line per locked collection plus a trailing
// summary, entirely through the Printer so every line is sanitized and
// --quiet-aware like the rest of the program's output.
//
// Every line here is result tier (Okf/PersistentPrintf/Errorf), matching the
// fact that this report is itself the run's product: an up-to-date entry
// uses Okf since there is nothing to do, an outdated entry uses
// PersistentPrintf since it is a neutral finding rather than a success or a
// failure, and a failed lookup uses Errorf on stderr so a diagnostic never
// contaminates stdout. Result tier means --quiet suppresses none of these
// four lines - it only ever suppresses the transient tier - which is a
// deliberate divergence from classifyDryRun's own dry-run report mapping:
// reporting "there is a newer version available" through a green checkmark
// would be a wrong statement, so the two reports are not aligned on purpose.
//
// The failure line alone renders r.Name with %q; the other two render it
// with %s, matching every other report in this package. r.Name is an
// untrusted identifier - it comes straight from the lockfile, with no
// character-class validation beyond helpers.SplitFQDN's "exactly one dot,
// both halves non-empty" - and %q does two things to it: it delimits it, so
// a reader can see exactly where it ends, and it escapes any control
// character inside it before safeout.Clean ever sees that occurrence. The
// two defenses therefore overlap on this one operand rather than one
// standing in for the other. Clean still runs over the whole formatted line
// and is what bounds r.Err, whose text comes from a Galaxy server. On the
// up-to-date and outdated lines there is no overlap at all - Clean alone
// bounds r.Name there - which is why the property is pinned on Clean itself
// (internal/safeout) and on every Printer tier (internal/progress) rather
// than on this call site.
//
// Quoting only this one line is deliberate: the up-to-date and outdated
// lines are the normal report an operator reads on every run, and
// %q-quoting a name that is almost always benign would make that ordinary
// report harder to read.
func reportOutdated(runtime *infra.Infra, results []outdatedEntry, lockPath string) {
	upToDate, outdated, failed := 0, 0, 0
	for _, r := range results {
		switch {
		case r.Err != nil:
			runtime.Output.Errorf("Lookup failed: %q@%s: %s", r.Name, r.Locked, r.Err)
			failed++
		case r.Newer:
			runtime.Output.PersistentPrintf("Outdated: %s %s -> %s", r.Name, r.Locked, r.Latest)
			outdated++
		default:
			runtime.Output.Okf("Up to date: %s@%s", r.Name, r.Locked)
			upToDate++
		}
	}
	runtime.Output.PersistentPrintf("%s: %d up to date, %d outdated, %d failed", lockPath, upToDate, outdated, failed)
}
