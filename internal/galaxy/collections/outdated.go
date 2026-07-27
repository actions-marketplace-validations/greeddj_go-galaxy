package collections

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/Masterminds/semver/v3"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
)

var errOutdatedLookupFailed = errors.New("one or more outdated lookups failed")

// outdatedEntry summarizes a single collection's locked-vs-latest delta.
type outdatedEntry struct {
	Name    string
	Locked  string
	Latest  string
	Message string
	Newer   bool
	Failed  bool
}

// Outdated loads the lockfile and queries Galaxy for the latest available
// version of every locked collection in parallel, printing a summary table.
// Exit status is non-zero only when at least one query failed.
func Outdated(ctx context.Context, cfg *config.Config, runtime *infra.Infra) error {
	if cfg == nil || cfg.Offline {
		return fmt.Errorf("%w: outdated requires network access", helpers.ErrOfflineMode)
	}
	lockPath := lockfile.ResolveDefaultPath(cfg.RequirementsFile, cfg.LockFile)
	lf, err := lockfile.Load(lockPath)
	if err != nil {
		return fmt.Errorf("load lockfile %s: %w", lockPath, err)
	}
	results := queryLatestVersions(ctx, cfg, runtime, lf)
	printOutdated(runtime, results)
	for _, r := range results {
		if r.Failed {
			return errOutdatedLookupFailed
		}
	}
	return nil
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
		return outdatedEntry{Name: e.Name, Locked: e.Version, Failed: true, Message: "invalid collection name"}
	}
	source := e.Source
	if source == "" {
		source = deps.cfg.Server
	}
	col := collection{Namespace: ns, Name: name, Source: source}
	policy := cachePolicyForConstraint(deps.cfg, false)
	root, err := resolveRootMetadata(ctx, deps, col, policy, e.Name)
	if err != nil {
		return outdatedEntry{Name: e.Name, Locked: e.Version, Failed: true, Message: err.Error()}
	}
	latest := strings.TrimSpace(root.meta.HighestVersion.Version)
	if latest == "" {
		return outdatedEntry{Name: e.Name, Locked: e.Version, Failed: true, Message: "no highest_version in metadata"}
	}
	return classifyOutdated(e.Name, e.Version, latest)
}

// classifyOutdated compares locked against latest and builds the resulting
// entry. A version that fails to parse as semver is reported as a failure
// (not silently treated as up-to-date), so the command still exits non-zero
// and the operator sees which entry needs attention.
func classifyOutdated(name, locked, latest string) outdatedEntry {
	newer, err := isNewerVersion(latest, locked)
	if err != nil {
		return outdatedEntry{Name: name, Locked: locked, Latest: latest, Failed: true, Message: "version parse: " + err.Error()}
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

func printOutdated(_ *infra.Infra, results []outdatedEntry) {
	sort.Slice(results, func(i, j int) bool { return results[i].Name < results[j].Name })
	upd, fail, ok := 0, 0, 0
	for _, r := range results {
		switch {
		case r.Failed:
			fmt.Printf("✗  %-50s  %s  →  ?  (%s)\n", r.Name, r.Locked, r.Message) //nolint:forbidigo
			fail++
		case r.Newer:
			fmt.Printf("⬆  %-50s  %s  →  %s\n", r.Name, r.Locked, r.Latest) //nolint:forbidigo
			upd++
		default:
			fmt.Printf("✓  %-50s  %s\n", r.Name, r.Locked) //nolint:forbidigo
			ok++
		}
	}
	fmt.Printf("\nSummary: %d up-to-date · %d outdated · %d failed\n", ok, upd, fail) //nolint:forbidigo
}
