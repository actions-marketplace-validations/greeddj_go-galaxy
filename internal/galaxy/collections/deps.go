package collections

import (
	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/extracted"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	bolt "go.etcd.io/bbolt"
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
}

type installDeps struct {
	collectionDeps

	artifacts    cacheManager.ArtifactStore
	db           *bolt.DB
	extractStore *extracted.Store
}

type prefetchDeps struct {
	collectionDeps

	artifacts cacheManager.ArtifactStore
}

func newCollectionDeps(cfg *config.Config, runtime *infra.Infra, st *store.Store) collectionDeps {
	return collectionDeps{cfg: cfg, runtime: runtime, st: st, apiRoots: newAPIRootMemo()}
}

func newInstallDeps(
	cfg *config.Config,
	runtime *infra.Infra,
	st *store.Store,
	artifacts cacheManager.ArtifactStore,
	db *bolt.DB,
	extractStore *extracted.Store,
) installDeps {
	return installDeps{
		collectionDeps: newCollectionDeps(cfg, runtime, st),
		artifacts:      artifacts,
		db:             db,
		extractStore:   extractStore,
	}
}

func newPrefetchDeps(
	cfg *config.Config,
	runtime *infra.Infra,
	st *store.Store,
	artifacts cacheManager.ArtifactStore,
) prefetchDeps {
	return prefetchDeps{
		collectionDeps: newCollectionDeps(cfg, runtime, st),
		artifacts:      artifacts,
	}
}
