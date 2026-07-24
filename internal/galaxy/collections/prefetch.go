package collections

import (
	"context"
	"path/filepath"
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
	// either handed off to an install worker via Wait (ownership transfers
	// there) or reclaimed by Close (for a key whose install level never ran).
	prefetched map[string]downloadResult
	mu         sync.Mutex
	wg         sync.WaitGroup
}

// startPrefetcher schedules prefetch tasks for collections.
func startPrefetcher(ctx context.Context, deps prefetchDeps, collections map[string]collection) *prefetcher {
	cfg := deps.cfg
	artifacts := deps.artifacts
	p := &prefetcher{
		meta:       make(map[string]*types.GalaxyCollectionVersionInfo),
		errs:       make(map[string]error),
		done:       make(map[string]chan struct{}),
		prefetched: make(map[string]downloadResult),
	}
	if cfg == nil || cfg.NoCache || artifacts == nil {
		return p
	}

	tasks := buildPrefetchTasks(ctx, deps, collections, p)
	if len(tasks) == 0 {
		return p
	}

	// Derive a cancellable child context so Close can abort every in-flight
	// worker download without waiting for the caller's own ctx to end - the
	// worker pool must never outlive runInstall's backend lock.
	pfCtx, cancel := context.WithCancel(ctx)
	p.cancel = cancel

	taskCh := makeTaskChannel(tasks)
	startPrefetchWorkers(pfCtx, deps, p, taskCh)
	return p
}

func buildPrefetchTasks(
	ctx context.Context,
	deps prefetchDeps,
	collections map[string]collection,
	p *prefetcher,
) []collection {
	cfg := deps.cfg
	st := deps.st
	artifacts := deps.artifacts
	tasks := make([]collection, 0, len(collections))
	for _, col := range collections {
		if !isGalaxyType(col.Type) {
			continue
		}
		installPath := filepath.Join(cfg.DownloadPath, "ansible_collections", col.Namespace, col.Name)
		if canSkipInstall(cfg, col, installPath, st) {
			continue
		}
		if ok, err := artifacts.Has(ctx, artifactKey(col)); err == nil && ok {
			continue
		}
		p.register(col.key())
		tasks = append(tasks, col)
	}
	return tasks
}

func makeTaskChannel(tasks []collection) chan collection {
	taskCh := make(chan collection, len(tasks))
	for _, col := range tasks {
		taskCh <- col
	}
	close(taskCh)
	return taskCh
}

func startPrefetchWorkers(
	pfCtx context.Context,
	deps prefetchDeps,
	p *prefetcher,
	taskCh chan collection,
) {
	cfg := deps.cfg
	for range max(cfg.Workers, 1) {
		p.wg.Go(func() {
			for col := range taskCh {
				meta, result, err := prefetchOne(pfCtx, deps, col)
				p.finish(col.key(), meta, result, err)
			}
		})
	}
}

// prefetchOne loads metadata for col and, unless the artifact is already
// cached, downloads it ahead of time. The returned downloadResult is the
// same value downloadCollectionToCache produces for the main install path
// (its temp path, verified sha, and cleanup), handed back so the install
// worker can reuse it instead of fetching the artifact a second time; a
// metadata error, a Has() error, or an already-cached artifact all return a
// zero downloadResult, since there is nothing new to hand off in those cases.
func prefetchOne(
	ctx context.Context,
	deps prefetchDeps,
	col collection,
) (*types.GalaxyCollectionVersionInfo, downloadResult, error) {
	meta, err := loadCollectionMetadata(ctx, deps.collectionDeps, col)
	if err != nil {
		return nil, downloadResult{}, err
	}
	key := artifactKey(col)
	ok, statErr := deps.artifacts.Has(ctx, key)
	if statErr != nil {
		return meta, downloadResult{}, statErr
	}
	if ok {
		return meta, downloadResult{}, nil
	}
	// useCache stays true: the artifact must still be committed to the shared
	// cache for every other consumer (a different project, a later run), on
	// top of handing its temp off to this run's own install worker.
	result, err := downloadCollectionToCache(ctx, newInstallDeps(deps.cfg, deps.runtime, deps.st, deps.artifacts, nil, nil), key, meta, true)
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
// p.prefetched: a key whose install level never ran (a prior level failed
// and installLevels broke before scheduling it) never has its temp claimed
// by Wait, so without this drain it would leak on disk. Wait (during the
// levels loop) and Close (runInstall's deferred join, after the loop) are
// never concurrent, but the drain still takes p.mu to honor this type's
// mutex discipline uniformly.
func (p *prefetcher) Close() {
	if p.cancel != nil {
		p.cancel()
	}
	p.wg.Wait()

	p.mu.Lock()
	for key, result := range p.prefetched {
		if result.Cleanup != nil {
			result.Cleanup()
		}
		delete(p.prefetched, key)
	}
	p.mu.Unlock()
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
