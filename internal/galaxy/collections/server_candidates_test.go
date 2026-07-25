package collections

import (
	"reflect"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
)

// TestRootMetadataURLCandidatesEmptyMemoMatchesFullSet asserts that with an
// empty (or nil) memo, rootMetadataURLCandidates returns the exact same
// candidate set the pre-memoization implementation produced: all apiRoot
// variants (v3, v2, bare), each with and without a trailing slash, for every
// server base. This locks down behavior-preservation for the common
// first-probe case.
func TestRootMetadataURLCandidatesEmptyMemoMatchesFullSet(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Server: "https://galaxy.example.com"}
	col := collection{Namespace: "acme", Name: "widgets"}
	const base = "https://galaxy.example.com"

	want := []rootMetaCandidate{
		{url: base + "/api/v3/collections/acme/widgets/", base: base, apiRoot: base + "/api/v3"},
		{url: base + "/api/v3/collections/acme/widgets", base: base, apiRoot: base + "/api/v3"},
		{url: base + "/api/v2/collections/acme/widgets/", base: base, apiRoot: base + "/api/v2"},
		{url: base + "/api/v2/collections/acme/widgets", base: base, apiRoot: base + "/api/v2"},
		{url: base + "/api/collections/acme/widgets/", base: base, apiRoot: base + "/api"},
		{url: base + "/api/collections/acme/widgets", base: base, apiRoot: base + "/api"},
	}

	for name, memo := range map[string]*apiRootMemo{"empty memo": newAPIRootMemo(), "nil memo": nil} {
		got := rootMetadataURLCandidates(cfg, col, memo)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: candidate set mismatch\n got: %+v\nwant: %+v", name, got, want)
		}
	}
}

// TestRootMetadataURLCandidatesWithRecordedWinner asserts that once a base's
// winning apiRoot is recorded, only that apiRoot's two trailing-slash
// variants are emitted for that base - the losing apiRoot variants are
// dropped entirely, not merely reordered.
func TestRootMetadataURLCandidatesWithRecordedWinner(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Server: "https://galaxy.example.com"}
	col := collection{Namespace: "acme", Name: "widgets"}
	const base = "https://galaxy.example.com"
	const winningRoot = base + "/api/v2"

	memo := newAPIRootMemo()
	memo.recordWinner(base, winningRoot)

	got := rootMetadataURLCandidates(cfg, col, memo)
	want := []rootMetaCandidate{
		{url: winningRoot + "/collections/acme/widgets/", base: base, apiRoot: winningRoot},
		{url: winningRoot + "/collections/acme/widgets", base: base, apiRoot: winningRoot},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("candidate set mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

// TestRootMetadataURLCandidatesWinnerAppliesOnlyToItsOwnBase asserts that a
// recorded winner for one server base does not affect a second, unrelated
// base in the same candidate list (e.g. an explicit collection source
// alongside the configured server): the second base still gets the full
// unrestricted apiRoot set.
func TestRootMetadataURLCandidatesWinnerAppliesOnlyToItsOwnBase(t *testing.T) {
	t.Parallel()
	const winningBase = "https://galaxy.example.com"
	const otherBase = "https://other.example.com"
	cfg := &config.Config{Server: winningBase}
	col := collection{Namespace: "acme", Name: "widgets", Source: otherBase}

	memo := newAPIRootMemo()
	memo.recordWinner(winningBase, winningBase+"/api/v2")

	got := rootMetadataURLCandidates(cfg, col, memo)

	var winningBaseCount, otherBaseCount int
	for _, cand := range got {
		switch cand.base {
		case winningBase:
			winningBaseCount++
			if cand.apiRoot != winningBase+"/api/v2" {
				t.Fatalf("expected only the winning apiRoot for %q, got candidate %+v", winningBase, cand)
			}
		case otherBase:
			otherBaseCount++
		default:
			t.Fatalf("unexpected candidate base %q in %+v", cand.base, cand)
		}
	}
	if winningBaseCount != 2 {
		t.Fatalf("expected 2 candidates (both trailing-slash variants) for the winning base, got %d", winningBaseCount)
	}
	if otherBaseCount != 6 {
		t.Fatalf("expected the unrestricted 6-candidate set for the other base, got %d", otherBaseCount)
	}
}
