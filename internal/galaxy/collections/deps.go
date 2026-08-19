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
	// verify is this run's signature verification state, or nil for a run that
	// verifies nothing - which is what every call site tests through
	// verifyContext.enabled(), never by reading this field. It is shared by
	// every install and warm worker: see verifyContext for why one keyring and
	// one fetcher per run, read concurrently, is the contract rather than an
	// economy.
	verify *verifyContext
	// presence carries the prefetcher's own scan-time cache-presence hints
	// (see prefetcher.cachedArtifacts), keyed by artifactKey, so isCacheHit
	// can skip a redundant repeat of a probe the scan already ran. install
	// and warm each pass their own run's prefetcher's set; it is nil only
	// for the prefetcher's own downloadDeps, which never calls isCacheHit at
	// all - and a nil map reads as "no hint" everywhere it is indexed, so
	// that caller needs no special case.
	presence map[string]bool
}

// prefetchDeps deliberately carries no verifyContext. A prefetch worker fills
// the shared artifact cache and installs nothing, and that cache is policy-free
// by design: its entries are keyed by artifact, not by which run's keyring
// would accept them, so a verdict reached here could not be recorded anywhere a
// later run may read it. Verification belongs to the worker that is about to
// write a collection into a tree, which is where prepareWithRecovery's action
// closure runs it.
type prefetchDeps struct {
	collectionDeps

	artifacts cacheManager.ArtifactStore
	// root is threaded through so shouldSchedulePrefetch's cheap
	// already-installed check (installRecordMatches) can be evaluated through
	// the same rooted target the real install uses - see installDeps.root's
	// own doc comment for what this closes. warm passes nil, exactly as its
	// installDeps does: it installs nothing, so "already installed" is never
	// its reason to skip a prefetch, and shouldSchedulePrefetch reads a nil
	// root as "not installed" and decides on the cache probe alone.
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
	verify *verifyContext,
) installDeps {
	return installDeps{
		collectionDeps: newCollectionDeps(cfg, runtime, st),
		artifacts:      artifacts,
		extractStore:   extractStore,
		root:           root,
		presence:       presence,
		verify:         verify,
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
