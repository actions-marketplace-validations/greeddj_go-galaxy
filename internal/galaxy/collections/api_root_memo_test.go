package collections

import (
	"fmt"
	"sync"
	"testing"
)

// TestAPIRootMemoWinnerAbsentByDefault asserts that a fresh memo reports no
// winner for any base until one is recorded.
func TestAPIRootMemoWinnerAbsentByDefault(t *testing.T) {
	t.Parallel()
	memo := newAPIRootMemo()

	if apiRoot, ok := memo.winner("https://galaxy.example.com"); ok {
		t.Fatalf("expected no winner recorded, got (%q, true)", apiRoot)
	}
}

// TestAPIRootMemoRecordThenWinner asserts that a recorded winner is returned
// verbatim, and that an unrelated base is unaffected.
func TestAPIRootMemoRecordThenWinner(t *testing.T) {
	t.Parallel()
	memo := newAPIRootMemo()
	const base = "https://galaxy.example.com"
	const apiRoot = "https://galaxy.example.com/api/v2"

	memo.recordWinner(base, apiRoot)

	got, ok := memo.winner(base)
	if !ok {
		t.Fatal("expected a winner to be recorded, got none")
	}
	if got != apiRoot {
		t.Fatalf("expected winner %q, got %q", apiRoot, got)
	}

	if _, ok := memo.winner("https://other.example.com"); ok {
		t.Fatal("expected no winner recorded for an unrelated base")
	}
}

// TestAPIRootMemoRecordWinnerOverwrites asserts that recording a new winner
// for the same base replaces the previous one.
func TestAPIRootMemoRecordWinnerOverwrites(t *testing.T) {
	t.Parallel()
	memo := newAPIRootMemo()
	const base = "https://galaxy.example.com"

	memo.recordWinner(base, base+"/api/v2")
	memo.recordWinner(base, base+"/api/v3")

	got, ok := memo.winner(base)
	if !ok || got != base+"/api/v3" {
		t.Fatalf("expected the later recordWinner to win with %q, got (%q, %v)", base+"/api/v3", got, ok)
	}
}

// TestAPIRootMemoNilReceiverIsSafe asserts that a nil *apiRootMemo behaves
// like an empty memo for winner (no panic, no winner) and that recordWinner
// on a nil receiver is a safe no-op. Some tests build a collectionDeps
// literal without setting apiRoots, so the metadata path must tolerate nil.
func TestAPIRootMemoNilReceiverIsSafe(t *testing.T) {
	t.Parallel()
	var memo *apiRootMemo

	if apiRoot, ok := memo.winner("https://galaxy.example.com"); ok {
		t.Fatalf("expected no winner from a nil memo, got (%q, true)", apiRoot)
	}
	memo.recordWinner("https://galaxy.example.com", "https://galaxy.example.com/api/v3")
}

// TestAPIRootMemoConcurrentAccess exercises recordWinner and winner from many
// goroutines concurrently across a handful of bases, matching how the memo
// is actually used: shared by value-copy across an install/resolve/prefetch
// worker pool. Run with -race to confirm the RWMutex actually guards access.
func TestAPIRootMemoConcurrentAccess(t *testing.T) {
	t.Parallel()
	memo := newAPIRootMemo()
	const bases = 8
	const goroutinesPerBase = 16

	var wg sync.WaitGroup
	for b := range bases {
		base := fmt.Sprintf("https://galaxy%d.example.com", b)
		apiRoot := base + "/api/v3"
		for range goroutinesPerBase {
			wg.Go(func() {
				memo.recordWinner(base, apiRoot)
				if got, ok := memo.winner(base); ok && got != apiRoot {
					t.Errorf("base %q: expected winner %q or none, got %q", base, apiRoot, got)
				}
			})
		}
	}
	wg.Wait()

	for b := range bases {
		base := fmt.Sprintf("https://galaxy%d.example.com", b)
		want := base + "/api/v3"
		got, ok := memo.winner(base)
		if !ok || got != want {
			t.Fatalf("base %q: expected winner %q, got (%q, %v)", base, want, got, ok)
		}
	}
}
