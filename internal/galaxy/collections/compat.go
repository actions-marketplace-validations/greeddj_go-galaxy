package collections

import (
	"context"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// installedEntry aliases the store entry type for compatibility.
type installedEntry = store.InstalledEntry

// requirementSpec aliases the store requirement spec for compatibility.
type requirementSpec = store.RequirementSpec

// fetchJSONWithCachePolicy fetches JSON using cache policy and context,
// binding the request to runtime.MetadataDeadline() so no collections call
// site can pass a wrong (or missing) metadata fetch budget - the budget is
// derived from runtime here rather than accepted as a caller-supplied
// parameter.
func fetchJSONWithCachePolicy(
	ctx context.Context,
	runtime *infra.Infra,
	url string,
	st *store.Store,
	out any,
	policy cacheManager.Policy,
) error {
	return cacheManager.FetchJSONWithCachePolicy(ctx, runtime.HTTP, url, st, out, policy, runtime.MetadataDeadline())
}

// cachePolicyForConstraint builds a cache policy from config options.
func cachePolicyForConstraint(cfg *config.Config, exact bool) cacheManager.Policy {
	return cacheManager.PolicyForConstraint(cfg, exact)
}
