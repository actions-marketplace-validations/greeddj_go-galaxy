// Package cache_test exercises WithStateDeadline as an external test
// package specifically so it can import internal/cache/local (E3's real
// local.Backend fixture) without an import cycle: internal/cache/local
// itself imports internal/galaxy/cache for its Backend/ArtifactStore
// interfaces, so a same-package (internal) test file here could never import
// it back.
package cache_test

// This file pins WithStateDeadline's core drift guarantee: every one of the
// four state operations is bounded, Lock/ClearFiles are deliberately not,
// and the whole thing is inert for the local backend by construction.
//
// Each test below was verified against a real revert of the production
// change it pins, and this comment quotes the actual observed output:
//
//   - TestWithStateDeadlineBoundsEveryStateOperation, dropping SaveStore's
//     own context.WithTimeout wrapping in statedeadline.go (calling
//     b.Backend.SaveStore(ctx, st) directly, so it inherits the caller's
//     unbounded context instead), makes the SaveStore subtest hang until the
//     harness kills it - run with a bounded -timeout so it fails instead of
//     blocking the suite forever, observed as:
//     "panic: test timed out after 5s
//     running tests:
//     TestWithStateDeadlineBoundsEveryStateOperation (5s)
//     TestWithStateDeadlineBoundsEveryStateOperation/SaveStore (5s)"
//     with the stuck goroutine's frame at
//     "github.com/greeddj/go-galaxy/internal/galaxy/cache_test.
//     (*stubStateBackend).SaveStore(...)" blocked on <-ctx.Done().
//   - TestWithStateDeadlineDoesNotBoundLockOrClearFiles, wrapping Lock with
//     the same per-operation budget the four state methods use, makes the
//     Lock call fail before the stub's 3x-budget delay elapses, observed as:
//     "Lock() error = cache state object deadline exceeded after 30ms: ...,
//     want nil (Lock must not be bounded by the state-object budget)"

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/cache/local"
	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// stateDeadlineTestBudget is the fixed per-operation budget every stub-backed
// test in this file uses.
const stateDeadlineTestBudget = 30 * time.Millisecond

// stubStateBackend is a minimal cacheManager.Backend whose four state
// operations either block until their context ends (blocking == true) or
// return fixed values immediately, and whose Lock/ClearFiles can be made to
// take a fixed, deliberately long delay before succeeding, to prove
// WithStateDeadline leaves them unbounded.
type stubStateBackend struct {
	store      *store.Store
	registry   *store.ProjectRegistry
	lockDelay  time.Duration
	clearDelay time.Duration
	blocking   bool
}

func (s *stubStateBackend) Open(_ context.Context) error  { return nil }
func (s *stubStateBackend) Close(_ context.Context) error { return nil }

// Lock waits out s.lockDelay (or the context ending, whichever comes first)
// before reporting success, so a test can prove this call is never bounded
// by the state-object budget.
func (s *stubStateBackend) Lock(ctx context.Context) (func() error, error) {
	if s.lockDelay > 0 {
		timer := time.NewTimer(s.lockDelay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return func() error { return nil }, nil
}

// LoadStore blocks on ctx until it ends and returns ctx.Err() when blocking
// is set; otherwise it returns s.store immediately.
func (s *stubStateBackend) LoadStore(ctx context.Context) (*store.Store, error) {
	if s.blocking {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return s.store, nil
}

// SaveStore blocks on ctx until it ends and returns ctx.Err() when blocking
// is set; otherwise it returns nil immediately.
func (s *stubStateBackend) SaveStore(ctx context.Context, _ *store.Store) error {
	if s.blocking {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

// ClearFiles waits out s.clearDelay (or the context ending, whichever comes
// first) before reporting success, mirroring Lock.
func (s *stubStateBackend) ClearFiles(ctx context.Context) error {
	if s.clearDelay > 0 {
		timer := time.NewTimer(s.clearDelay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	return nil
}

// RecordProject blocks on ctx until it ends and returns ctx.Err() when
// blocking is set; otherwise it returns nil immediately.
func (s *stubStateBackend) RecordProject(ctx context.Context, _, _ string) error {
	if s.blocking {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

// LoadProjectRegistry blocks on ctx until it ends and returns ctx.Err() when
// blocking is set; otherwise it returns s.registry immediately.
func (s *stubStateBackend) LoadProjectRegistry(ctx context.Context) (*store.ProjectRegistry, error) {
	if s.blocking {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return s.registry, nil
}

func (s *stubStateBackend) Artifacts() cacheManager.ArtifactStore { return nil }
func (s *stubStateBackend) SweepTemp(_ context.Context) error     { return nil }

// TestWithStateDeadlineBoundsEveryStateOperation tables over all four state
// operations against a stub whose implementations block forever absent a
// context deadline, asserting each is bounded by WithStateDeadline into
// helpers.ErrStateObjectDeadline (matching neither context sentinel through
// errors.Is). The positive control - the identical wrapper over a
// non-blocking stub - proves the wrapper is otherwise transparent: it
// returns nil and the stub's own recorded *store.Store/*store.ProjectRegistry
// values unchanged.
func TestWithStateDeadlineBoundsEveryStateOperation(t *testing.T) {
	t.Parallel()

	blocking := &stubStateBackend{blocking: true}
	wrapped := cacheManager.WithStateDeadline(blocking, stateDeadlineTestBudget)

	cases := []struct {
		call func() error
		name string
	}{
		{name: "LoadStore", call: func() error {
			_, err := wrapped.LoadStore(context.Background())
			return err
		}},
		{name: "SaveStore", call: func() error {
			return wrapped.SaveStore(context.Background(), store.New())
		}},
		{name: "LoadProjectRegistry", call: func() error {
			_, err := wrapped.LoadProjectRegistry(context.Background())
			return err
		}},
		{name: "RecordProject", call: func() error {
			return wrapped.RecordProject(context.Background(), "requirements.yml", "collections")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assertStateDeadlineFired(t, tc.name, tc.call())
		})
	}

	assertStateDeadlinePositiveControl(t)
}

// assertStateDeadlineFired fails the test unless err matches
// helpers.ErrStateObjectDeadline and neither context sentinel through
// errors.Is - split out of TestWithStateDeadlineBoundsEveryStateOperation
// purely to stay under the cyclomatic-complexity budget.
func assertStateDeadlineFired(t *testing.T, name string, err error) {
	t.Helper()
	if !errors.Is(err, helpers.ErrStateObjectDeadline) {
		t.Fatalf("%s error = %v, want errors.Is ErrStateObjectDeadline", name, err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%s error = %v, must not match context.DeadlineExceeded", name, err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("%s error = %v, must not match context.Canceled", name, err)
	}
}

// assertStateDeadlinePositiveControl is
// TestWithStateDeadlineBoundsEveryStateOperation's positive control, split
// out purely to stay under the cyclomatic-complexity budget: the identical
// wrapper over a non-blocking stub returns nil and the stub's own recorded
// *store.Store/*store.ProjectRegistry values unchanged, proving the wrapper
// is otherwise transparent.
func assertStateDeadlinePositiveControl(t *testing.T) {
	t.Helper()
	wantStore := store.New()
	wantRegistry := &store.ProjectRegistry{Projects: map[string]store.ProjectRecord{}}
	nonBlocking := &stubStateBackend{store: wantStore, registry: wantRegistry}
	wrappedOK := cacheManager.WithStateDeadline(nonBlocking, stateDeadlineTestBudget)

	gotStore, err := wrappedOK.LoadStore(context.Background())
	if err != nil || gotStore != wantStore {
		t.Fatalf("positive control: LoadStore = (%v, %v), want (%v, nil)", gotStore, err, wantStore)
	}
	if err := wrappedOK.SaveStore(context.Background(), store.New()); err != nil {
		t.Fatalf("positive control: SaveStore = %v, want nil", err)
	}
	gotRegistry, err := wrappedOK.LoadProjectRegistry(context.Background())
	if err != nil || gotRegistry != wantRegistry {
		t.Fatalf("positive control: LoadProjectRegistry = (%v, %v), want (%v, nil)", gotRegistry, err, wantRegistry)
	}
	if err := wrappedOK.RecordProject(context.Background(), "requirements.yml", "collections"); err != nil {
		t.Fatalf("positive control: RecordProject = %v, want nil", err)
	}
}

// TestWithStateDeadlineDoesNotBoundLockOrClearFiles asserts Lock and
// ClearFiles both pass through WithStateDeadline unbounded: a stub whose
// implementations of both take 3x the wrapped budget still succeeds through
// the wrapper. This is falsifiable because
// TestWithStateDeadlineBoundsEveryStateOperation already proves the wrapper
// is capable of producing the sentinel at the identical budget - so a
// success here is a real refusal to bound these two methods, not a fixture
// too generous to ever fail.
func TestWithStateDeadlineDoesNotBoundLockOrClearFiles(t *testing.T) {
	t.Parallel()

	stub := &stubStateBackend{
		lockDelay:  3 * stateDeadlineTestBudget,
		clearDelay: 3 * stateDeadlineTestBudget,
	}
	wrapped := cacheManager.WithStateDeadline(stub, stateDeadlineTestBudget)

	release, err := wrapped.Lock(context.Background())
	if err != nil {
		t.Fatalf("Lock() error = %v, want nil (Lock must not be bounded by the state-object budget)", err)
	}
	if err := release(); err != nil {
		t.Fatalf("release() error = %v, want nil", err)
	}

	if err := wrapped.ClearFiles(context.Background()); err != nil {
		t.Fatalf("ClearFiles() error = %v, want nil (ClearFiles must not be bounded by the state-object budget)", err)
	}
}

// TestWithStateDeadlineIsInertForTheLocalBackend is the falsifiable form of
// "the local backend is not penalized": a real local.Backend over a fresh
// t.TempDir() cache, wrapped with a 1-nanosecond budget, still succeeds on
// every one of the four state operations. This holds by construction, not by
// luck: local.Backend's own methods take a context parameter named "_" and
// never consult it, so their returned errors (nil, here) never carry a
// context signal for deadlineError's causal precondition to act on,
// regardless of how long ago the wrapped context's 1ns deadline elapsed. The
// positive control - the identical 1ns budget wrapping the blocking stub -
// proves 1ns is still a budget capable of firing, so this is not merely a
// budget too generous to ever bind.
func TestWithStateDeadlineIsInertForTheLocalBackend(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	backend := local.New(cacheDir)
	wrapped := cacheManager.WithStateDeadline(backend, time.Nanosecond)

	if err := wrapped.Open(context.Background()); err != nil {
		t.Fatalf("Open: %v", err)
	}

	reqPath := filepath.Join(t.TempDir(), "requirements.yml")
	if err := os.WriteFile(reqPath, []byte("collections: []\n"), helpers.FileMod); err != nil {
		t.Fatalf("write requirements.yml: %v", err)
	}

	if err := wrapped.RecordProject(context.Background(), reqPath, cacheDir); err != nil {
		t.Fatalf("RecordProject: %v, want nil (the local backend ignores its context parameter)", err)
	}
	if _, err := wrapped.LoadProjectRegistry(context.Background()); err != nil {
		t.Fatalf("LoadProjectRegistry: %v, want nil", err)
	}
	st, err := wrapped.LoadStore(context.Background())
	if err != nil {
		t.Fatalf("LoadStore: %v, want nil", err)
	}
	if err := wrapped.SaveStore(context.Background(), st); err != nil {
		t.Fatalf("SaveStore: %v, want nil", err)
	}

	stub := &stubStateBackend{blocking: true}
	wrappedStub := cacheManager.WithStateDeadline(stub, time.Nanosecond)
	if _, err := wrappedStub.LoadStore(context.Background()); !errors.Is(err, helpers.ErrStateObjectDeadline) {
		t.Fatalf("positive control: LoadStore = %v, want errors.Is ErrStateObjectDeadline (1ns must still be able to fire)", err)
	}
}
