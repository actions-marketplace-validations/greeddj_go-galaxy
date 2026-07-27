package metrics

import "sync/atomic"

// Counters accumulates one run's artifact-cache tallies. Its methods are
// safe for concurrent use by the per-level install pool, the prefetch
// workers, and the warm pool with no external locking, and every method is
// nil-receiver-safe so a caller never needs a guard. One Counters belongs to
// one Infra, which belongs to one run: infra.New allocates a fresh set, so
// the totals a Report reads describe exactly the run that produced them.
type Counters struct {
	hits            atomic.Int64
	misses          atomic.Int64
	bytesDownloaded atomic.Int64
}

// Totals is a point-in-time read of a Counters.
type Totals struct {
	BytesDownloaded int64
	CacheHits       int64
	CacheMisses     int64
}

// AddCacheHit records one artifact whose bytes were served from the artifact
// cache. Callers increment this exactly once per successful
// ArtifactStore.Fetch, never per collection - see the package doc on Report
// for why a collection can contribute both a hit and a miss.
func (c *Counters) AddCacheHit() {
	if c == nil {
		return
	}
	c.hits.Add(1)
}

// AddCacheMiss records one artifact whose bytes were acquired from the
// origin. Callers increment this exactly once per successful
// downloadCollectionToCache call - the whole retry-bounded acquisition, not
// per HTTP attempt - so a retried transient failure counts one miss, not one
// per attempt.
func (c *Counters) AddCacheMiss() {
	if c == nil {
		return
	}
	c.misses.Add(1)
}

// AddBytesDownloaded records n bytes read from a Galaxy origin's artifact
// response body. Unlike AddCacheMiss, this is counted per download attempt,
// including bytes read by an attempt that then failed and was retried, so it
// is not derivable from CacheMisses alone. A cache hit contributes zero here
// - including an S3 hit, whose object transfer is a real network round trip
// to the cache backend, but is not artifact-download traffic and is
// deliberately not counted by this method. n <= 0 is ignored: there is
// nothing to add, and a negative value would only be a caller bug.
func (c *Counters) AddBytesDownloaded(n int64) {
	if c == nil || n <= 0 {
		return
	}
	c.bytesDownloaded.Add(n)
}

// Totals returns a point-in-time snapshot of c. A nil receiver reads as an
// all-zero Totals, matching the nil-receiver-safety of the Add* methods.
func (c *Counters) Totals() Totals {
	if c == nil {
		return Totals{}
	}
	return Totals{
		CacheHits:       c.hits.Load(),
		CacheMisses:     c.misses.Load(),
		BytesDownloaded: c.bytesDownloaded.Load(),
	}
}
