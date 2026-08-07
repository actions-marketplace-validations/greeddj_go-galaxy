package collections_test

// This file proves the WIRING of the clean-save skip decorator through
// collections.Start's real initInstall, end to end against a real local
// backend and a real fake Galaxy server. Nothing in internal/galaxy/store's
// or internal/galaxy/cache's own unit tests for the flag and the decorator
// drives Start at all, so nothing there proves initInstall actually wraps
// its backend with cacheManager.WithCleanSaveSkip.

import (
	"context"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/cache/local"
	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
)

// reloadLastSnapshot opens a fresh local backend against cacheDir, loads the
// persisted store, and returns its Meta.LastSnapshot, closing the backend
// before returning. It never reuses a backend collections.Start itself
// touched, so each call is an independent observation of what is actually on
// disk after that run finished.
func reloadLastSnapshot(t *testing.T, cacheDir string) time.Time {
	t.Helper()
	ctx := context.Background()
	backend := local.New(cacheDir)
	if err := backend.Open(ctx); err != nil {
		t.Fatalf("backend.Open: %v", err)
	}
	defer func() { _ = backend.Close(ctx) }()

	st, err := backend.LoadStore(ctx)
	if err != nil {
		t.Fatalf("backend.LoadStore: %v", err)
	}
	return st.MetaSnapshot().LastSnapshot
}

// TestIdleInstallDoesNotRewriteTheSnapshot proves that a second install run
// against an already-fully-satisfied cache leaves the persisted snapshot's
// LastSnapshot untouched, and that a run which genuinely has something new to
// resolve still advances it. A first install populates the cache and stamps
// a LastSnapshot; a second, entirely idle install - identical
// requirements.yml, identical server, nothing left to resolve or install -
// must leave that stamp exactly as it was; the positive control - a real
// requirements.yml change that forces a fresh resolve - must advance it,
// proving the unchanged stamp above is a genuine observation of "this run
// wrote nothing", not an artifact of the snapshot never being reloaded at
// all.
func TestIdleInstallDoesNotRewriteTheSnapshot(t *testing.T) {
	f := newE2EFixture(t)

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("first Start (populate the cache): %v", err)
	}
	firstStamp := reloadLastSnapshot(t, f.cfg.CacheDir)
	if firstStamp.IsZero() {
		t.Fatal("expected a non-zero LastSnapshot after the first install")
	}

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("second Start (fully idle): %v", err)
	}
	idleStamp := reloadLastSnapshot(t, f.cfg.CacheDir)
	if !idleStamp.Equal(firstStamp) {
		t.Fatalf(
			"LastSnapshot after an idle install = %v, want unchanged from %v (an idle run must not rewrite the snapshot)",
			idleStamp, firstStamp,
		)
	}

	// Positive control: a genuine requirements.yml change forces a fresh
	// resolve, which writes into the store through the normal setters and
	// must advance the stamp.
	f.server.AddVersion("acme", "extra", testVersion100, nil)
	writeRequirementsMulti(t, f.cfg.RequirementsFile, "acme.app", "acme.extra")
	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("third Start (a real requirements change): %v", err)
	}
	changedStamp := reloadLastSnapshot(t, f.cfg.CacheDir)
	if !changedStamp.After(idleStamp) {
		t.Fatalf("LastSnapshot after a real requirements change = %v, want strictly after %v", changedStamp, idleStamp)
	}
}
