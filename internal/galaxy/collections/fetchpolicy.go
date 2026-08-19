package collections

import (
	"context"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

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
