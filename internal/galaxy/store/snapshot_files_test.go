package store

import (
	"errors"
	"os"
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
	dbPath := filepath.Join(dir, helpers.StoreDBLocal)

	// Hold the consolidated DB's file lock externally to simulate a
	// concurrent process already occupying the cache.
	holder, err := bolt.Open(dbPath, helpers.FileMod, nil)
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

// TestOpenBoltClassifiesGarbageAsCorruptButNotAPermissionFailure proves
// openBolt's corruption arm matches a closed set of bbolt sentinels rather
// than acting as a catch-all for any bolt.Open failure: garbage bytes at
// the DB path fail one of bbolt's own corruption checks and classify as
// helpers.ErrCorruptSnapshotStore, while a permission failure on a
// genuinely valid Bolt file at the identical path does not. Without the
// second half, the exclusion openBolt's own doc comment describes (an
// OS-level failure like this one staying unclassified) would be
// documentary only - nothing would prove openBolt actually discriminates
// rather than simply wrapping every non-timeout error.
func TestOpenBoltClassifiesGarbageAsCorruptButNotAPermissionFailure(t *testing.T) {
	t.Parallel()

	t.Run("garbage bytes classify as corrupt", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		dbPath := filepath.Join(dir, helpers.StoreDBLocal)
		if err := os.WriteFile(dbPath, []byte("not a bolt database"), helpers.FileMod); err != nil {
			t.Fatalf("failed to write garbage db file: %v", err)
		}

		_, err := OpenDBs(dir, helpers.BoltOpenTimeout)
		if !errors.Is(err, helpers.ErrCorruptSnapshotStore) {
			t.Fatalf("expected ErrCorruptSnapshotStore, got %v", err)
		}
	})

	// Skipped when running as root, since root bypasses the permission bits
	// this subtest relies on to force the failure - the same root guard the
	// cleanup package's own permission-based tests use.
	t.Run("permission failure does not classify as corrupt", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root; permission-based open failure cannot be forced")
		}
		t.Parallel()
		dir := t.TempDir()
		dbPath := filepath.Join(dir, helpers.StoreDBLocal)

		// Seed a genuinely valid, empty Bolt database first, then strip every
		// permission bit: a garbage-bytes file would itself trip the
		// corruption arm this subtest must not exercise, so only the file's
		// permission bit - never its bytes - may force the failure.
		seed, err := bolt.Open(dbPath, helpers.FileMod, nil)
		if err != nil {
			t.Fatalf("failed to seed a valid bolt database: %v", err)
		}
		if err := seed.Close(); err != nil {
			t.Fatalf("failed to close the seeded database: %v", err)
		}
		if err := os.Chmod(dbPath, 0o000); err != nil {
			t.Fatalf("failed to chmod db file unreadable: %v", err)
		}
		t.Cleanup(func() {
			if err := os.Chmod(dbPath, helpers.FileMod); err != nil {
				t.Errorf("failed to restore db file perms: %v", err)
			}
		})

		_, err = OpenDBs(dir, helpers.BoltOpenTimeout)
		if err == nil {
			t.Fatalf("expected OpenDBs to fail against an unreadable db file")
		}
		if errors.Is(err, helpers.ErrCorruptSnapshotStore) {
			t.Fatalf("expected a permission failure not to classify as ErrCorruptSnapshotStore, got %v", err)
		}
	})
}
