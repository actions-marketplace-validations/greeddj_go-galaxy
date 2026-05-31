// Package metrics serializes a small JSON report describing the most
// recent install/warm run, intended for ingestion by CI dashboards.
package metrics

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// Report captures a single run's outcome.
type Report struct {
	StartedAt    time.Time     `json:"started_at"`
	FinishedAt   time.Time     `json:"finished_at"`
	Command      string        `json:"command"`
	Server       string        `json:"server,omitempty"`
	LockfilePath string        `json:"lockfile,omitempty"`
	LockfileHash string        `json:"lockfile_hash,omitempty"`
	Duration     time.Duration `json:"duration_ns"`
	Collections  int           `json:"collections"`
	Failures     int           `json:"failures"`
	Frozen       bool          `json:"frozen,omitempty"`
	Offline      bool          `json:"offline,omitempty"`
}

// Write marshals the report and writes it to path. If path is empty this
// is a no-op so callers can pass cfg.MetricsFile unconditionally.
func Write(path string, r Report) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), helpers.DirMod); err != nil {
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, helpers.FileMod)
}
