package store

import (
	"encoding/json"
	"testing"
	"time"
)

// TestDirtyIsFalseAfterEveryLoadPath proves Dirty starts false on every path
// that hands a caller a *Store without this process having written into it
// through a mutator: a fresh New(), a real local Save-then-Load round trip,
// and a json.Unmarshal of MarshalSnapshot's own output - the shape the S3
// backend's LoadStore produces on a successful fetch. Each fixture also
// carries the mandatory positive control: a single SetGraph call on that same
// store flips Dirty to true, proving the false result above is a real
// observation rather than a predicate that can never fire.
func TestDirtyIsFalseAfterEveryLoadPath(t *testing.T) {
	t.Parallel()

	cases := []struct {
		build func(t *testing.T) *Store
		name  string
	}{
		{name: "New", build: func(t *testing.T) *Store {
			t.Helper()
			return New()
		}},
		{name: "Save-then-Load", build: func(t *testing.T) *Store {
			t.Helper()
			dbs := openTestDBs(t)
			fixed := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
			mustSave(t, dbs, buildTestStore(fixed))
			return mustLoad(t, dbs)
		}},
		{name: "MarshalSnapshot-then-Unmarshal", build: func(t *testing.T) *Store {
			t.Helper()
			fixed := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
			data, err := buildTestStore(fixed).MarshalSnapshot()
			if err != nil {
				t.Fatalf("MarshalSnapshot error: %v", err)
			}
			decoded := New()
			if err := json.Unmarshal(data, decoded); err != nil {
				t.Fatalf("json.Unmarshal error: %v", err)
			}
			return decoded
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st := tc.build(t)
			if st.Dirty() {
				t.Fatalf("expected Dirty() == false immediately after %s, got true", tc.name)
			}

			// Positive control: a single mutator call on this same store must
			// flip Dirty to true.
			st.SetGraph("a.b@1.0.0", []string{"c.d@1.2.3"})
			if !st.Dirty() {
				t.Fatalf("expected Dirty() == true after SetGraph on the %s fixture, got false", tc.name)
			}
		})
	}
}

// dirtyMutatorCase is one row of TestEveryMutatorMarksDirty: a mutator call
// against a fresh store, and the name reported on failure.
type dirtyMutatorCase struct {
	call func(st *Store)
	name string
}

// dirtyMutatorCases lists every one of Store's 14 write-locked mutators, one
// row each, so a fresh call to each is proven to flip Dirty to true. This is
// the closed table dirty_audit_test.go's AST gate cross-checks structurally;
// keeping the two in sync is manual, which is exactly why that gate exists as
// a backstop.
func dirtyMutatorCases() []dirtyMutatorCase {
	return []dirtyMutatorCase{
		{name: "SetInstalled", call: func(st *Store) {
			st.SetInstalled("a.b@1.0.0", InstalledEntry{ArtifactSHA256: testArtifactSHA})
		}},
		{name: "DeleteInstalled", call: func(st *Store) {
			st.DeleteInstalled("a.b@1.0.0")
		}},
		{name: "SetWarmed", call: func(st *Store) {
			st.SetWarmed("a.b@1.0.0", "warmed-sha")
		}},
		{name: "SetGitPin", call: func(st *Store) {
			st.SetGitPin(testGitPinKey, GitPinEntry{Commit: testGitPinCommit})
		}},
		{name: "SetDepsCache", call: func(st *Store) {
			st.SetDepsCache("deps", map[string]string{"a.b": testDepsConstraint})
		}},
		{name: "DeleteDepsCache", call: func(st *Store) {
			st.DeleteDepsCache("deps")
		}},
		{name: "SetAPICache", call: func(st *Store) {
			st.SetAPICache("api", APICacheEntry{URL: "https://example.com/api"})
		}},
		{name: "ClearCaches", call: func(st *Store) {
			st.ClearCaches()
		}},
		{name: "SetVersionsCache", call: func(st *Store) {
			st.SetVersionsCache("versions", []string{"1.0.0"})
		}},
		{name: "SetResolvedAll", call: func(st *Store) {
			st.SetResolvedAll(map[string]ResolvedEntry{"a.b": {Version: "1.0.0"}})
		}},
		{name: "SetGraph", call: func(st *Store) {
			st.SetGraph("a.b@1.0.0", []string{"c.d@1.2.3"})
		}},
		{name: "DeleteGraph", call: func(st *Store) {
			st.DeleteGraph("a.b@1.0.0")
		}},
		{name: "SetGraphSnapshot", call: func(st *Store) {
			st.SetGraphSnapshot(map[string][]string{"a.b@1.0.0": {"c.d@1.2.3"}})
		}},
		{name: "SetRequirements", call: func(st *Store) {
			st.SetRequirements(map[string]RequirementSpec{"a.b": {Constraint: "1.0.0"}})
		}},
		{name: "SetMetaRequirements", call: func(st *Store) {
			st.SetMetaRequirements("req-hash", "https://example.com")
		}},
	}
}

// TestEveryMutatorMarksDirty proves each of Store's 14 write-locked mutators
// sets Dirty to true, on a fresh store, from a single call - including
// DeleteInstalled and DeleteGraph deleting a key that was never present,
// which pins that the flag is set unconditionally rather than only when the
// value actually changed.
func TestEveryMutatorMarksDirty(t *testing.T) {
	t.Parallel()

	for _, tc := range dirtyMutatorCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st := New()
			if st.Dirty() {
				t.Fatalf("expected a fresh New() store to report Dirty() == false before %s", tc.name)
			}
			tc.call(st)
			if !st.Dirty() {
				t.Fatalf("expected Dirty() == true after %s, got false", tc.name)
			}
		})
	}
}
