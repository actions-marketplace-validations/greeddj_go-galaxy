package local

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	bolt "go.etcd.io/bbolt"
)

func TestBackendOpenDoesNotOpenBolt(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	metaPath := filepath.Join(dir, helpers.StoreSnapshotMeta)

	// Hold the meta DB's file lock externally. If Open still touched Bolt,
	// it would either fail (timeout) or hang - here it must do neither.
	holder, err := bolt.Open(metaPath, helpers.FileMod, nil)
	if err != nil {
		t.Fatalf("failed to open holder DB: %v", err)
	}
	defer func() {
		_ = holder.Close()
	}()

	b := New(dir)
	if err := b.Open(context.Background()); err != nil {
		t.Fatalf("expected Open to succeed without touching Bolt, got %v", err)
	}
}

func TestBackendLockFailsFastWhenHeld(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	ctx := context.Background()

	b1 := openedBackend(t, dir)
	release1 := lockOrFatal(t, b1)

	b2 := openedBackend(t, dir)
	assertLockFailsFast(t, b2)

	if err := release1(); err != nil {
		t.Fatalf("release1 failed: %v", err)
	}

	release2 := lockOrFatal(t, b2)
	if err := release2(); err != nil {
		t.Fatalf("release2 failed: %v", err)
	}

	if err := b1.Close(ctx); err != nil {
		t.Fatalf("b1.Close failed: %v", err)
	}
	if err := b2.Close(ctx); err != nil {
		t.Fatalf("b2.Close failed: %v", err)
	}
}

// openedBackend creates and opens a Backend rooted at dir, failing the test
// on any error.
func openedBackend(t *testing.T, dir string) *Backend {
	t.Helper()
	b := New(dir)
	if err := b.Open(context.Background()); err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	return b
}

// lockOrFatal acquires b's lock, failing the test on any error.
func lockOrFatal(t *testing.T, b *Backend) func() error {
	t.Helper()
	release, err := b.Lock(context.Background())
	if err != nil {
		t.Fatalf("Lock failed: %v", err)
	}
	return release
}

// assertLockFailsFast asserts that b.Lock returns ErrAnotherInstanceIsRunning
// promptly instead of blocking, proving the lock is taken before any Bolt
// file is opened.
func assertLockFailsFast(t *testing.T, b *Backend) {
	t.Helper()

	type lockResult struct {
		release func() error
		err     error
	}
	resultCh := make(chan lockResult, 1)
	go func() {
		release, err := b.Lock(context.Background())
		resultCh <- lockResult{release: release, err: err}
	}()

	select {
	case res := <-resultCh:
		if !errors.Is(res.err, helpers.ErrAnotherInstanceIsRunning) {
			t.Fatalf("expected ErrAnotherInstanceIsRunning, got %v", res.err)
		}
		if res.release != nil {
			t.Fatalf("expected nil release on failed lock, got non-nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Lock hung instead of failing fast")
	}
}
