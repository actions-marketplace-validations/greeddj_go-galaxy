package collections

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"sync"

	"github.com/psvmcc/hub/pkg/types"
)

// prefetcher coordinates background metadata and artifact downloads.
type prefetcher struct {
	meta   map[string]*types.GalaxyCollectionVersionInfo
	errs   map[string]error
	done   map[string]chan struct{}
	cancel context.CancelFunc
	// prefetched holds, per collection key, the artifact a prefetch worker
	// already downloaded and committed to the shared cache, keyed until it is
	// either handed off to a consuming install or warm worker via Wait
	// (ownership transfers there) or reclaimed by Close (for a key whose
	// consumer never ran).
	prefetched map[string]downloadResult
	// presence is the set of artifact keys the buildPrefetchTasks scan found
	// cached and left unscheduled for prefetch - see cachedArtifacts' own doc
	// comment for why it is populated once and never mutated afterward, and
	// isCacheHit's for how an install worker uses membership in it to skip a
	// redundant repeat of that same probe.
	presence map[string]bool
	mu       sync.Mutex
	wg       sync.WaitGroup
}

// startPrefetcher schedules prefetch tasks for collections. levels orders the
// surviving tasks so background downloads track the level-ordered install
// consumer instead of racing in map order. warm passes nil levels - it has no
// install order to track - which lands every task at level 0 and leaves
// sortTasksByLevel's key tie-break as the whole queue order.
func startPrefetcher(ctx context.Context, deps prefetchDeps, collections map[string]collection, levels [][]string) *prefetcher {
	cfg := deps.cfg
	artifacts := deps.artifacts
	p := &prefetcher{
		meta:       make(map[string]*types.GalaxyCollectionVersionInfo),
		errs:       make(map[string]error),
		done:       make(map[string]chan struct{}),
		prefetched: make(map[string]downloadResult),
	}
	// --dry-run must never download an artifact ahead of time - that is
	// exactly the write a dry run suppresses - so it disables the prefetcher
	// here, the same structural way NoCache already does, rather than
	// threading a dry-run flag into prefetchOne itself. presence stays nil in
	// this disabled shape (see cachedArtifacts), so an install worker gets no
	// hint for any key and probes the store itself.
	if cfg == nil || cfg.NoCache || cfg.DryRun || artifacts == nil {
		return p
	}

	tasks := buildPrefetchTasks(ctx, deps, collections, p)
	if len(tasks) == 0 {
		return p
	}
	sortTasksByLevel(tasks, buildLevelIndex(levels))

	// Derive a cancellable child context so Close can abort every in-flight
	// worker download without waiting for the caller's own ctx to end - the
	// worker pool must never outlive runInstall's backend lock.
	pfCtx, cancel := context.WithCancel(ctx)
	p.cancel = cancel

	taskCh := makeTaskChannel(tasks)
	startPrefetchWorkers(pfCtx, deps, p, taskCh)
	return p
}

// buildLevelIndex maps each install key to its zero-based install level, so the
// prefetcher can order its download queue to match the level-ordered install
// consumer. Pre-sized to the total key count.
func buildLevelIndex(levels [][]string) map[string]int {
	total := 0
	for _, level := range levels {
		total += len(level)
	}
	index := make(map[string]int, total)
	for i, level := range levels {
		for _, key := range level {
			index[key] = i
		}
	}
	return index
}

// sortTasksByLevel orders prefetch tasks by ascending install level, with the
// collection key as a deterministic tie-break within a level, so background
// downloads track the level-ordered install consumer instead of racing in map
// order. Prefetch order is a latency optimization only and never affects
// correctness: each consuming worker blocks on Wait(key) for its own artifact
// regardless of the order it was fetched. A key absent from levelIndex just
// defaults to level 0, which is harmless: on install's path that absence
// cannot happen while collections and levels derive from the same graph, and
// warm's nil levels put every key there on purpose, leaving the key tie-break
// as its whole queue order.
func sortTasksByLevel(tasks []collection, levelIndex map[string]int) {
	slices.SortFunc(tasks, func(a, b collection) int {
		ka, kb := a.key(), b.key()
		if c := cmp.Compare(levelIndex[ka], levelIndex[kb]); c != 0 {
			return c
		}
		return strings.Compare(ka, kb)
	})
}

// buildPrefetchTasks decides, for every candidate collection, whether it
// needs a prefetch task, probing the artifact cache in parallel (bounded by
// cfg.DownloadWorkers, not cfg.Workers - this scan only issues Has probes,
// never an extraction, so it shares the download pool's network-bound sizing
// rather than the install/warm pool's CPU-bound one) since Has is the
// dominant per-collection cost. The parallel
// phase only writes to disjoint keep[i]/cached[i] slice elements, so no
// result mutex is needed; the survivor collection afterward is sequential,
// which keeps p.register calls, the presence map write, and the returned
// task order single-threaded and deterministic given the input snapshot.
//
// The presence set records a key whenever cached[i] is true, with no
// separate check against keep[i]: shouldSchedulePrefetch never returns both
// true for the same collection, so a key this scan schedules a prefetch task
// for can never also land in presence. That matters because the prefetcher
// itself is about to become a concurrent writer of that exact key, so the
// scan's own answer for it is not a hint an install worker should be handed -
// see isCacheHit's own doc comment for what an install worker does instead
// for a scheduled key whose prefetch failed.
func buildPrefetchTasks(
	ctx context.Context,
	deps prefetchDeps,
	collections map[string]collection,
	p *prefetcher,
) []collection {
	cols := make([]collection, 0, len(collections))
	for _, col := range collections {
		cols = append(cols, col)
	}

	keep := make([]bool, len(cols))
	cached := make([]bool, len(cols))
	var wg sync.WaitGroup
	sem := make(chan struct{}, max(deps.cfg.DownloadWorkers, 1))
	for i := range cols {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			keep[i], cached[i] = shouldSchedulePrefetch(ctx, deps, cols[i])
		})
	}
	wg.Wait()

	presence := make(map[string]bool, len(cols))
	tasks := make([]collection, 0, len(cols))
	for i, col := range cols {
		if cached[i] {
			presence[artifactKey(col)] = true
		}
		if keep[i] {
			p.register(col.key())
			tasks = append(tasks, col)
		}
	}
	p.presence = presence
	return tasks
}

// shouldSchedulePrefetch returns (schedule, cached): whether col needs a
// prefetch task (schedule), and, when the artifact cache was actually probed
// to decide, whether that probe found the artifact present (cached). schedule
// and cached are never both true for the same collection - see
// buildPrefetchTasks' own doc comment for how it relies on that to record
// col's cache key in its presence set unconditionally on cached alone.
//
// schedule is a Galaxy-type source that is neither already installed nor
// already present in the artifact cache. A Has() error is treated as
// "schedule it" - the same fail-open behavior the original sequential scan
// had, where a probe error fell through to scheduling rather than silently
// skipping the collection - and reports cached=false: an errored probe found
// nothing an install worker should trust as a hint.
//
// This deliberately calls installRecordMatches, not canSkipInstall: a wrong
// "already installed" here only skips a background download ahead of time,
// which installCollection's own strict canSkipInstall check corrects at
// actual install time, at the cost of a lost prefetch head start - not
// correctness. Spending a full tree-tally walk per candidate here, on top of
// the one installCollection already pays for the same collection, would
// double the cost this unit adds for no benefit.
//
// A nil deps.root or an identity that fails newInstallTarget's own guard both
// mean "not installed" here: fail-open for this scan, matching the Has()
// error handling below - a wrong "needs prefetch" only costs a redundant
// background download, never correctness.
func shouldSchedulePrefetch(ctx context.Context, deps prefetchDeps, col collection) (bool, bool) {
	if !isGalaxyType(col.Type) {
		return false, false
	}
	if target, ok := newInstallTarget(deps.root, deps.cfg, col); ok && installRecordMatches(target, col, deps.st) {
		return false, false
	}
	ok, err := deps.artifacts.Has(ctx, artifactKey(col))
	if err != nil {
		return true, false
	}
	return !ok, ok
}

func makeTaskChannel(tasks []collection) chan collection {
	taskCh := make(chan collection, len(tasks))
	for _, col := range tasks {
		taskCh <- col
	}
	close(taskCh)
	return taskCh
}

// startPrefetchWorkers spins up the background download pool that drains
// taskCh, sized to cfg.DownloadWorkers rather than cfg.Workers: a prefetch
// worker only downloads and hashes an artifact into a temp file - see
// prefetchOne's own nil extractStore/root, which keeps it off the
// extraction path entirely - so it shares the network-bound download pool's
// sizing rather than the install/warm pool's CPU-bound one.
func startPrefetchWorkers(
	pfCtx context.Context,
	deps prefetchDeps,
	p *prefetcher,
	taskCh chan collection,
) {
	cfg := deps.cfg
	for range max(cfg.DownloadWorkers, 1) {
		p.wg.Go(func() {
			for col := range taskCh {
				meta, result, err := prefetchOne(pfCtx, deps, col)
				p.finish(col.key(), meta, result, err)
			}
		})
	}
}

// prefetchOne loads metadata for col and downloads its artifact ahead of
// time. The returned downloadResult is the same value downloadCollectionToCache
// produces for the main install path (its temp path, verified sha, and
// cleanup), handed back so the install worker can reuse it instead of
// fetching the artifact a second time; a metadata error returns a zero
// downloadResult, since there is nothing new to hand off in that case.
func prefetchOne(
	ctx context.Context,
	deps prefetchDeps,
	col collection,
) (*types.GalaxyCollectionVersionInfo, downloadResult, error) {
	meta, err := loadCollectionMetadata(ctx, deps.collectionDeps, col)
	if err != nil {
		return nil, downloadResult{}, err
	}
	// No Has() re-probe: buildPrefetchTasks already established this key was
	// absent, and within a single run this prefetcher is the only committer of
	// the key - each key maps to exactly one task, and the key's own install
	// worker blocks on this prefetch via Wait before it could commit - so the
	// key cannot have appeared in the cache between the scan and now. The
	// single case a re-probe here would still catch (the scan's Has erroring
	// while the key was in fact cached) costs one byte-identical redundant
	// re-download instead, which is safe (same origin bytes, idempotent commit
	// and ingest) and far rarer than the per-prefetch Has such a re-probe
	// would put on the critical path.
	// useCache stays true: the artifact must still be committed to the shared
	// cache for every other consumer (a different project, a later run), on
	// top of handing its temp off to this run's own install worker.
	// extractStore and root are both nil: this downloadDeps feeds only
	// downloadCollectionToCache, which never touches the collections tree or
	// the extracted store, so neither is needed here. The verify context is nil
	// for the reason stated on prefetchDeps itself: this worker fills a
	// policy-free shared cache and installs nothing.
	downloadDeps := newInstallDeps(deps.cfg, deps.runtime, deps.st, deps.artifacts, nil, nil, nil, nil)
	result, err := downloadCollectionToCache(ctx, downloadDeps, artifactKey(col), col.Source, meta, true)
	if err != nil {
		return meta, downloadResult{}, err
	}
	return meta, result, nil
}

// Wait blocks until prefetch for key completes and returns its metadata, any
// artifact the prefetcher already downloaded for it, and its error. Ownership
// of the returned downloadResult's temp (if any) transfers to the caller here:
// the entry is deleted from p.prefetched before returning, so Close will not
// also try to reclaim a temp this call already handed off.
func (p *prefetcher) Wait(key string) (*types.GalaxyCollectionVersionInfo, downloadResult, bool, error) {
	p.mu.Lock()
	done := p.done[key]
	p.mu.Unlock()
	if done == nil {
		return nil, downloadResult{}, false, nil
	}
	<-done
	p.mu.Lock()
	meta := p.meta[key]
	err := p.errs[key]
	result := p.prefetched[key]
	delete(p.prefetched, key)
	p.mu.Unlock()
	return meta, result, true, err
}

// Close cancels any in-flight prefetch downloads and blocks until every
// prefetch worker has returned. It is idempotent and safe to call on a
// prefetcher that was constructed disabled (no cancel func, no workers).
//
// cancel() itself is safe to call more than once and unblocks any worker
// parked in an artifact GET; downloadRetryable already returns false for
// context.Canceled/DeadlineExceeded, and helpers.Retry bails on a canceled
// ctx during backoff, so a canceled worker drains its remaining taskCh tasks
// fast (each fails closed, non-retryable) and returns. finish is still
// called for every task even on cancellation, so no Wait(key) can deadlock.
//
// Close also reclaims every prefetched artifact still sitting in
// p.prefetched: a key whose consumer never ran (a prior install level failed
// and installLevels broke before scheduling it, or a canceled warm stopped
// dispatching) never has its temp claimed by Wait, so without this drain it
// would leak on disk. Wait (during the consuming loop) and Close (the
// deferred join after it) are never concurrent, but the drain still takes
// p.mu to honor this type's mutex discipline uniformly.
func (p *prefetcher) Close() {
	if p.cancel != nil {
		p.cancel()
	}
	p.wg.Wait()

	p.mu.Lock()
	for key, result := range p.prefetched {
		cleanupIfNeeded(result.Cleanup)
		delete(p.prefetched, key)
	}
	p.mu.Unlock()
}

// cachedArtifacts returns the presence hints buildPrefetchTasks collected
// during its scan, keyed by artifactKey. The map is built once, inside
// startPrefetcher (via buildPrefetchTasks), before any install worker is
// dispatched, and is never mutated afterward - the identical argument
// tlsDispatchTransport.insecureOrigins (internal/galaxy/fetch/client.go)
// already makes for its own construct-once, read-only-after map: concurrent
// readers need no lock because there is never a concurrent writer. It stays
// nil for a disabled prefetcher (cfg.NoCache, cfg.DryRun, a nil artifact
// store), which is indistinguishable from an empty map to the one caller
// that reads it (isCacheHit, through a plain map index that treats a nil map
// as always-empty).
func (p *prefetcher) cachedArtifacts() map[string]bool {
	return p.presence
}

// register allocates a completion channel for a key.
func (p *prefetcher) register(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.done[key]; ok {
		return
	}
	p.done[key] = make(chan struct{})
}

// finish records completion data for a prefetch task, stashing any
// downloaded artifact in p.prefetched so a later Wait(key) can hand it off to
// an install worker instead of it being fetched again.
func (p *prefetcher) finish(key string, meta *types.GalaxyCollectionVersionInfo, result downloadResult, err error) {
	p.mu.Lock()
	p.meta[key] = meta
	p.errs[key] = err
	if result.Path != "" {
		p.prefetched[key] = result
	}
	done := p.done[key]
	p.mu.Unlock()
	if done != nil {
		close(done)
	}
}
