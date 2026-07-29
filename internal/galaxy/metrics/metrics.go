// Package metrics serializes a small JSON report describing the most
// recent install/warm run, intended for ingestion by CI dashboards.
package metrics

import (
	"encoding/json"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// Report captures a single run's outcome.
//
// CacheHits, CacheMisses, and BytesDownloaded are artifact-level, never
// collection-level: a HIT is one artifact whose bytes came from the artifact
// cache (one successful ArtifactStore.Fetch), a MISS is one artifact whose
// bytes came from the origin (one successful, retry-bounded acquisition -
// a retried 5xx still counts one miss, not one per attempt), and
// BytesDownloaded is every byte read from a Galaxy origin's artifact
// response body, counted per download attempt including a failed, retried
// one. A cache hit contributes zero bytes here, including an S3 hit: that
// object transfer is a real network round trip to the cache backend, but it
// is not artifact-download traffic, so it is deliberately excluded.
// Consequently CacheHits+CacheMisses counts artifact acquisitions, not
// collections, and can exceed Collections: the bounded evict-and-refetch
// recovery path (see the collections package's prepareWithRecovery) makes
// one collection contribute both one hit (the cache-resident artifact that
// turned out corrupt) and one miss (the refetch that replaced it), and a
// collection skipped by canSkipInstall touches no artifact at all, counting
// as neither. BytesDownloaded is therefore not derivable from CacheMisses
// alone. None of the three carries `omitempty`: a consumer must be able to
// tell "zero hits" apart from "field absent" (an older binary that predates
// these counters).
//
// Frozen reports whether the run actually resolved from the lockfile, not
// merely whether --frozen was passed: lock accepts the flag because it
// shares the collection flag set, yet always regenerates the lockfile from a
// fresh resolve, so a lock report never sets it. Offline is
// configuration-wide - it governs the HTTP transport for every command - and
// is reported as configured.
type Report struct {
	StartedAt       time.Time     `json:"started_at"`
	FinishedAt      time.Time     `json:"finished_at"`
	Command         string        `json:"command"`
	Server          string        `json:"server,omitempty"`
	LockfilePath    string        `json:"lockfile,omitempty"`
	LockfileHash    string        `json:"lockfile_hash,omitempty"`
	Duration        time.Duration `json:"duration_ns"`
	CacheHits       int64         `json:"cache_hits"`
	CacheMisses     int64         `json:"cache_misses"`
	BytesDownloaded int64         `json:"bytes_downloaded"`
	Collections     int           `json:"collections"`
	Failures        int           `json:"failures"`
	Frozen          bool          `json:"frozen,omitempty"`
	Offline         bool          `json:"offline,omitempty"`
}

// Write marshals the report and writes it to path. If path is empty this
// is a no-op so callers can pass cfg.MetricsFile unconditionally. The write
// is atomic (see helpers.WriteFileAtomic): a CI consumer never observes a
// truncated report, and a symlink planted at the operator-specified path is
// replaced rather than followed.
func Write(path string, r Report) error {
	if path == "" {
		return nil
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return helpers.WriteFileAtomic(path, data)
}
