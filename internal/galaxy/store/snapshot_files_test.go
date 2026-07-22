package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	bolt "go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"
)

func TestOpenDBsReturnsCacheBusyOnTimeout(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	metaPath := filepath.Join(dir, helpers.StoreSnapshotMeta)

	// Hold the meta DB's file lock externally to simulate a concurrent
	// process already occupying the cache.
	holder, err := bolt.Open(metaPath, helpers.FileMod, nil)
	if err != nil {
		t.Fatalf("failed to open holder DB: %v", err)
	}
	defer func() {
		_ = holder.Close()
	}()

	const shortTimeout = 200 * time.Millisecond
	start := time.Now()
	_, err = OpenDBs(dir, shortTimeout)
	elapsed := time.Since(start)

	if !errors.Is(err, helpers.ErrCacheBusy) {
		t.Fatalf("expected ErrCacheBusy, got %v", err)
	}
	if !errors.Is(err, bolterrors.ErrTimeout) {
		t.Fatalf("expected wrapped bolt.ErrTimeout, got %v", err)
	}
	if elapsed >= 2*time.Second {
		t.Fatalf("expected OpenDBs to fail fast, took %v", elapsed)
	}
}
