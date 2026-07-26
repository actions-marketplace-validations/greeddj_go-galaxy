package collections

import (
	"reflect"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
)

// TestRootMetadataURLCandidatesEmptyMemoMatchesFullSet asserts that with an
// empty (or nil) memo, rootMetadataURLCandidates returns every apiRoot
// variant apiRootCandidates derives for the base - in apiRootCandidates' own
// priority order (/api/v3, /v3, /api/v2, /v2, /api) - each with and without
// a trailing slash, for every server base. This locks down
// behavior-preservation for the common first-probe case: /api/v3 stays
// first, so the galaxy.ansible.com shape never pays for the Galaxy NG /
// Automation Hub fallback candidates.
func TestRootMetadataURLCandidatesEmptyMemoMatchesFullSet(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Server: "https://galaxy.example.com"}
	col := collection{Namespace: "acme", Name: "widgets"}
	const base = "https://galaxy.example.com"

	want := []rootMetaCandidate{
		{url: base + "/api/v3/collections/acme/widgets/", base: base, apiRoot: base + "/api/v3"},
		{url: base + "/api/v3/collections/acme/widgets", base: base, apiRoot: base + "/api/v3"},
		{url: base + "/v3/collections/acme/widgets/", base: base, apiRoot: base + "/v3"},
		{url: base + "/v3/collections/acme/widgets", base: base, apiRoot: base + "/v3"},
		{url: base + "/api/v2/collections/acme/widgets/", base: base, apiRoot: base + "/api/v2"},
		{url: base + "/api/v2/collections/acme/widgets", base: base, apiRoot: base + "/api/v2"},
		{url: base + "/v2/collections/acme/widgets/", base: base, apiRoot: base + "/v2"},
		{url: base + "/v2/collections/acme/widgets", base: base, apiRoot: base + "/v2"},
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
	if otherBaseCount != 10 {
		t.Fatalf("expected the unrestricted 10-candidate set for the other base, got %d", otherBaseCount)
	}
}

// apiRootCandidatesCase is one TestAPIRootCandidates table entry.
type apiRootCandidatesCase struct {
	base string
	want []string
}

// apiRootCandidatesCases is TestAPIRootCandidates' table, pulled out to a
// package-level var purely to keep the test function itself short: every
// case is otherwise independent and reused nowhere else.
//
//nolint:gochecknoglobals // test-only fixture, not runtime-mutable state.
var apiRootCandidatesCases = map[string]apiRootCandidatesCase{
	"bare base": {
		base: "https://galaxy.example.com",
		want: []string{
			"https://galaxy.example.com/api/v3",
			"https://galaxy.example.com/v3",
			"https://galaxy.example.com/api/v2",
			"https://galaxy.example.com/v2",
			"https://galaxy.example.com/api",
		},
	},
	"base ending /api": {
		base: "https://galaxy.example.com/api",
		want: []string{
			"https://galaxy.example.com/api/v3",
			"https://galaxy.example.com/api/v2",
			"https://galaxy.example.com/api",
		},
	},
	"base ending /api/v3": {
		base: "https://galaxy.example.com/api/v3",
		want: []string{"https://galaxy.example.com/api/v3"},
	},
	"base ending /api/v2": {
		base: "https://galaxy.example.com/api/v2",
		want: []string{"https://galaxy.example.com/api/v2"},
	},
	"base ending /v3": {
		base: "https://hub.example.com/api/automation-hub/v3",
		want: []string{"https://hub.example.com/api/automation-hub/v3"},
	},
	"base ending /v2": {
		base: "https://hub.example.com/api/automation-hub/v2",
		want: []string{"https://hub.example.com/api/automation-hub/v2"},
	},
	"quoted value": {
		base: `"https://galaxy.example.com"`,
		want: []string{
			"https://galaxy.example.com/api/v3",
			"https://galaxy.example.com/v3",
			"https://galaxy.example.com/api/v2",
			"https://galaxy.example.com/v2",
			"https://galaxy.example.com/api",
		},
	},
	"trailing-slash value": {
		base: "https://galaxy.example.com/",
		want: []string{
			"https://galaxy.example.com/api/v3",
			"https://galaxy.example.com/v3",
			"https://galaxy.example.com/api/v2",
			"https://galaxy.example.com/v2",
			"https://galaxy.example.com/api",
		},
	},
	"empty string": {
		base: "",
		want: nil,
	},
}

// TestAPIRootCandidates is a table test over apiRootCandidates' own suffix
// handling and normalization, independent of rootMetadataURLCandidates'
// collections/ns/name URL building.
func TestAPIRootCandidates(t *testing.T) {
	t.Parallel()
	for name, tc := range apiRootCandidatesCases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := apiRootCandidates(tc.base)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("apiRootCandidates(%q) = %+v, want %+v", tc.base, got, tc.want)
			}
		})
	}
}
