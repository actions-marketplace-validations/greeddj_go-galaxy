package store

import (
	"testing"
	"time"
)

// TestGitPinCloneOnWriteAndRead proves SetGitPin and GetGitPin both deep-copy:
// a caller mutating the value it passed in, or the one it got back, never
// reaches the stored entry - the same contract SetInstalled holds for Deps.
func TestGitPinCloneOnWriteAndRead(t *testing.T) {
	t.Parallel()
	const name = "app"
	st := New()
	entry := GitPinEntry{
		Commit: testGitPinCommit,
		Collections: []GitPinCollection{{
			Namespace: "acme", Name: name, Version: "1.0.0",
			Dependencies: map[string]string{"a.b": ">=1.0.0"},
		}},
	}
	st.SetGitPin(testGitPinKey, entry)
	entry.Collections[0].Dependencies["a.b"] = "mutated"
	entry.Collections[0].Name = "mutated"

	got, ok := st.GetGitPin(testGitPinKey)
	if !ok {
		t.Fatalf("pin missing")
	}
	if got.Collections[0].Name != name || got.Collections[0].Dependencies["a.b"] != ">=1.0.0" {
		t.Fatalf("SetGitPin did not clone: %#v", got)
	}
	got.Collections[0].Dependencies["a.b"] = "mutated-read"
	got.Collections[0].Name = "mutated-read"
	again, _ := st.GetGitPin(testGitPinKey)
	if again.Collections[0].Name != name || again.Collections[0].Dependencies["a.b"] != ">=1.0.0" {
		t.Fatalf("GetGitPin did not clone: %#v", again)
	}
	if again.FetchedAt.IsZero() || time.Since(again.FetchedAt) > time.Minute {
		t.Fatalf("FetchedAt not stamped with the current time: %v", again.FetchedAt)
	}
}

// TestGitPinIgnoresEmptyKeyOrCommit proves a pin naming no commit, or no key,
// is never recorded: such an entry would replay into a failure.
func TestGitPinIgnoresEmptyKeyOrCommit(t *testing.T) {
	t.Parallel()
	st := New()
	st.SetGitPin("", GitPinEntry{Commit: testGitPinCommit})
	st.SetGitPin(testGitPinKey, GitPinEntry{})
	if len(st.GitPins) != 0 || st.Dirty() {
		t.Fatalf("an empty key or commit was recorded: %#v", st.GitPins)
	}
	if _, ok := st.GetGitPin(testGitPinKey); ok {
		t.Fatalf("GetGitPin found a pin that was never set")
	}
	var nilStore *Store
	nilStore.SetGitPin(testGitPinKey, GitPinEntry{Commit: testGitPinCommit})
	if _, ok := nilStore.GetGitPin(testGitPinKey); ok {
		t.Fatalf("nil store answered a pin")
	}
}

// TestClearCachesDropsGitPins pins that --clear-cache forgets git pins along
// with the other remote answers, so the next resolve re-advertises.
func TestClearCachesDropsGitPins(t *testing.T) {
	t.Parallel()
	st := New()
	st.SetGitPin(testGitPinKey, GitPinEntry{Commit: testGitPinCommit})
	st.ClearCaches()
	if _, ok := st.GetGitPin(testGitPinKey); ok {
		t.Fatalf("ClearCaches kept the git pin")
	}
}

// TestGitPinsSurviveSnapshotWithoutAWindow proves a pin older than every
// retention window still round-trips through snapshotData: pins are
// invalidated by the requirements signature and --refresh, never by age.
func TestGitPinsSurviveSnapshotWithoutAWindow(t *testing.T) {
	t.Parallel()
	st := New()
	st.GitPins[testGitPinKey] = GitPinEntry{
		FetchedAt: time.Now().UTC().Add(-400 * 24 * time.Hour),
		Commit:    testGitPinCommit,
	}
	data := st.snapshotData()
	if _, ok := data.GitPins[testGitPinKey]; !ok {
		t.Fatalf("an old git pin was evicted from the snapshot")
	}
}
