package collections

import (
	"os"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/extracted"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

type collectionDeps struct {
	cfg     *config.Config
	runtime *infra.Infra
	st      *store.Store

	// apiRoots memoizes, per server base, the API root that already answered
	// successfully for this phase, so a non-v3-first server is not
	// re-probed on every collection. Scoped to one collectionDeps: resolve,
	// install, and prefetch each get their own memo (see newCollectionDeps).
	apiRoots *apiRootMemo

	// unmatchedSources memoizes the source: values already warned about this
	// phase, so one misconfigured host produces one line rather than one per
	// collection pinned to it. Scoped exactly like apiRoots above.
	unmatchedSources *unmatchedSourceMemo
}

type installDeps struct {
	collectionDeps

	artifacts    cacheManager.ArtifactStore
	extractStore *extracted.Store
	// root is the single os.Root every install-side write in this run
	// funnels through (see installroot.go), so a symlinked ansible_collections
	// or namespace/name component under cfg.DownloadPath cannot redirect a
	// write outside it. It is nil for warm, which never touches the
	// collections tree at all - newInstallTarget's own nil-root guard then
	// makes any accidental collections-tree call from that path fail closed
	// rather than by convention.
	root *os.Root
	// presence carries the prefetcher's own scan-time cache-presence hints
	// (see prefetcher.cachedArtifacts), keyed by artifactKey, so isCacheHit
	// can skip a redundant repeat of a probe the scan already ran. It is nil
	// for warm, which starts no prefetcher at all, and for the prefetcher's
	// own downloadDeps, which never calls isCacheHit at all; a nil map reads
	// as "no hint" everywhere it is indexed, so both callers need no special
	// case.
	presence map[string]bool
}

type prefetchDeps struct {
	collectionDeps

	artifacts cacheManager.ArtifactStore
	// root is threaded through so shouldSchedulePrefetch's cheap
	// already-installed check (installRecordMatches) can be evaluated through
	// the same rooted target the real install uses - see installDeps.root's
	// own doc comment for what this closes.
	root *os.Root
}

func newCollectionDeps(cfg *config.Config, runtime *infra.Infra, st *store.Store) collectionDeps {
	return collectionDeps{
		cfg:              cfg,
		runtime:          runtime,
		st:               st,
		apiRoots:         newAPIRootMemo(),
		unmatchedSources: newUnmatchedSourceMemo(),
	}
}

func newInstallDeps(
	cfg *config.Config,
	runtime *infra.Infra,
	st *store.Store,
	artifacts cacheManager.ArtifactStore,
	extractStore *extracted.Store,
	root *os.Root,
	presence map[string]bool,
) installDeps {
	return installDeps{
		collectionDeps: newCollectionDeps(cfg, runtime, st),
		artifacts:      artifacts,
		extractStore:   extractStore,
		root:           root,
		presence:       presence,
	}
}

func newPrefetchDeps(
	cfg *config.Config,
	runtime *infra.Infra,
	st *store.Store,
	artifacts cacheManager.ArtifactStore,
	root *os.Root,
) prefetchDeps {
	return prefetchDeps{
		collectionDeps: newCollectionDeps(cfg, runtime, st),
		artifacts:      artifacts,
		root:           root,
	}
}
