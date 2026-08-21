package cache_test

// This file exercises WithCleanSaveSkip. It stays in this external test
// package for the identical reason statedeadline_test.go (this package's
// own doc comment) does: keeping it out of the internal cache package leaves
// the production seam (WithCleanSaveSkip, exported) exactly what a caller in
// another package actually uses.

import (
	"context"
	"testing"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// saveCountingBackend is a minimal cacheManager.Backend whose SaveStore
// counts every call it actually receives, letting a test assert how many
// times WithCleanSaveSkip let a call reach the wrapped backend. It is a
// separate stub from stubStateBackend (statedeadline_test.go) rather than a
// field added to that one: that stub's shape is pinned by a different file's
// tests, and a save counter is not part of the property it exists to prove.
type saveCountingBackend struct {
	saveCalls int
}

func (s *saveCountingBackend) Open(_ context.Context) error  { return nil }
func (s *saveCountingBackend) Close(_ context.Context) error { return nil }

// Lock returns ctx unchanged, matching the local backend's own
// cannot-lose-a-lock contract; this stub is never used to exercise Lock
// itself.
func (s *saveCountingBackend) Lock(ctx context.Context) (context.Context, func() error, error) {
	return ctx, func() error { return nil }, nil
}

// LoadStore returns a fresh, empty store rather than nil: this stub is never
// used to exercise LoadStore itself, and a real Backend never reports success
// alongside a nil store.
func (s *saveCountingBackend) LoadStore(_ context.Context) (*store.Store, error) {
	return store.New(), nil
}

// SaveStore records that it was called and always succeeds.
func (s *saveCountingBackend) SaveStore(_ context.Context, _ *store.Store) error {
	s.saveCalls++
	return nil
}

func (s *saveCountingBackend) ClearFiles(_ context.Context) error { return nil }

func (s *saveCountingBackend) RecordProject(_ context.Context, _, _, _ string) error { return nil }

// LoadProjectRegistry returns a fresh, empty registry rather than nil, for
// the identical reason LoadStore above does.
func (s *saveCountingBackend) LoadProjectRegistry(_ context.Context) (*store.ProjectRegistry, error) {
	return &store.ProjectRegistry{Projects: map[string]store.ProjectRecord{}}, nil
}

func (s *saveCountingBackend) Artifacts() cacheManager.ArtifactStore { return nil }
func (s *saveCountingBackend) SweepTemp(_ context.Context) error     { return nil }

// TestCleanSaveSkipSkipsUnmutatedStore proves WithCleanSaveSkip's SaveStore
// never reaches the wrapped backend for a store reporting Dirty() == false,
// and carries the mandatory positive control on the same store: after one
// mutator call, the identical SaveStore call does reach it.
func TestCleanSaveSkipSkipsUnmutatedStore(t *testing.T) {
	t.Parallel()

	inner := &saveCountingBackend{}
	wrapped := cacheManager.WithCleanSaveSkip(inner)

	st := store.New()
	if err := wrapped.SaveStore(context.Background(), st); err != nil {
		t.Fatalf("SaveStore(clean store) error = %v, want nil", err)
	}
	if inner.saveCalls != 0 {
		t.Fatalf("SaveStore(clean store) reached the wrapped backend %d times, want 0", inner.saveCalls)
	}

	// Positive control, same store: a single mutator call flips Dirty to
	// true, and the identical SaveStore call must now reach the wrapped
	// backend - proving the skip above is a real observation of a clean
	// store, not a decorator that never calls through at all.
	st.SetGraph("a.b@1.0.0", []string{"c.d@1.2.3"})
	if err := wrapped.SaveStore(context.Background(), st); err != nil {
		t.Fatalf("SaveStore(dirty store) error = %v, want nil", err)
	}
	if inner.saveCalls != 1 {
		t.Fatalf("SaveStore(dirty store) reached the wrapped backend %d times, want 1", inner.saveCalls)
	}
}

// TestCleanSaveSkipPassesThroughNilStore proves the st != nil guard in
// SaveStore is load-bearing: Store.Dirty() on a nil receiver returns false,
// so a nil store must still reach the wrapped backend rather than being read
// as "clean" and silently skipped - which would swallow whatever the wrapped
// backend does with a nil store (local.Backend returns helpers.ErrStoreNil
// for it; the S3 backend returns nil) behind a false "nothing to do" skip
// this decorator must never invent.
func TestCleanSaveSkipPassesThroughNilStore(t *testing.T) {
	t.Parallel()

	inner := &saveCountingBackend{}
	wrapped := cacheManager.WithCleanSaveSkip(inner)

	if err := wrapped.SaveStore(context.Background(), nil); err != nil {
		t.Fatalf("SaveStore(nil) error = %v, want nil", err)
	}
	if inner.saveCalls != 1 {
		t.Fatalf(
			"SaveStore(nil) reached the wrapped backend %d times, want 1 (the st != nil guard must let a nil store through)",
			inner.saveCalls,
		)
	}
}
