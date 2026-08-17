package collections_test

// This file end-to-end pins that a real install --frozen run reaches
// store.snapshotData's persist-side signature-query cut against a real Bolt
// file on disk, not merely Store.MarshalSnapshot called directly in
// isolation: internal/galaxy/collections/signature_query_persistence_test.go
// (an internal test package) and internal/galaxy/store/requirements_query_cut_test.go
// both pin the cut itself; this file is the wiring proof that install
// --frozen actually reaches it. Reverting only copyRequirementsCutQuery
// back to maps.Copy (internal/galaxy/store/snapshot.go) leaves every test in
// this package green, every --frozen e2e included, and fails only the
// store-level test - nothing outside this file proved install --frozen ever
// persisted a stripped signature source into a real cache directory.

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/cache/local"
	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// frozenPersistCapabilityMarker is the sensitive part of the seeded query
// string below, checked for on its own rather than as a substring of the
// whole query for the identical reason
// internal/galaxy/collections/signature_query_persistence_test.go's own
// signatureCapabilityMarker is - see that constant's own doc comment for
// the argument.
const frozenPersistCapabilityMarker = "X-Amz-Signature=deadbeefcapability"

// frozenPersistSignatureSourceWithQuery and
// frozenPersistSignatureSourceStripped are the same signature source before
// and after the cut, seeded directly onto the store below.
const (
	frozenPersistSignatureSourceWithQuery = "https://sigs.example.com/acme-app.asc?" + frozenPersistCapabilityMarker + "&X-Amz-Expires=3600"
	frozenPersistSignatureSourceStripped  = "https://sigs.example.com/acme-app.asc"
)

// seedRequirementsWithQuerySignature opens a fresh local backend against
// cacheDir, loads whatever store is there (empty, on a cache this fresh),
// writes a Requirements entry carrying a query-bearing signature source
// directly onto the map - bypassing Store.SetRequirements, the shape a
// pre-fix binary leaves and the only shape install --frozen can itself
// encounter, since resolveOrLoadLockfile's lockfile branch never calls
// buildRequirementsSpec/recordResolution at all - and saves it, closing the
// backend before returning so the frozen install below opens its own.
func seedRequirementsWithQuerySignature(t *testing.T, cacheDir, source string) {
	t.Helper()
	ctx := context.Background()
	backend := local.New(cacheDir)
	if err := backend.Open(ctx); err != nil {
		t.Fatalf("seed backend.Open: %v", err)
	}
	defer func() { _ = backend.Close(ctx) }()

	st, err := backend.LoadStore(ctx)
	if err != nil {
		t.Fatalf("seed backend.LoadStore: %v", err)
	}
	st.Requirements["acme.app"] = store.RequirementSpec{
		Constraint: "*",
		Source:     source,
		Signatures: []string{frozenPersistSignatureSourceWithQuery},
	}
	if err := backend.SaveStore(ctx, st); err != nil {
		t.Fatalf("seed backend.SaveStore: %v", err)
	}
}

// TestFrozenInstallPersistsRequirementsWithoutSignatureQuery proves that a
// real install --frozen run - driven through collections.Start against a
// real local cache directory, not Store.MarshalSnapshot called directly -
// reaches store.snapshotData's persist-side query cut and commits a
// stripped Requirements entry into the real Bolt file on disk. A frozen
// install never calls recordResolution (resolveOrLoadLockfile's lockfile
// branch skips it entirely), so the persist-side cut is the only thing that
// can strip the query the seed step below plants; if it did not run, the
// query would survive into the reloaded store unchanged.
//
// KILLING MUTATION, run and reverted via go test -overlay:
// copyRequirementsCutQuery (internal/galaxy/store/snapshot.go) replaced by
// the maps.Copy it replaced. internal/galaxy/collections' whole test suite
// stays green under that mutation, every --frozen e2e included; only this
// test's own assertion fails:
//
//	frozen_snapshot_persist_e2e_test.go:116: persisted acme.app signature still carries a query
func TestFrozenInstallPersistsRequirementsWithoutSignatureQuery(t *testing.T) {
	f := newE2EFixture(t)
	newFrozenPinFixture(t, f)
	seedRequirementsWithQuerySignature(t, f.cfg.CacheDir, f.cfg.Server)

	if err := collections.Start(context.Background(), f.cfg, f.runtime); err != nil {
		t.Fatalf("Start (frozen install): %v", err)
	}

	ctx := context.Background()
	backend := local.New(f.cfg.CacheDir)
	if err := backend.Open(ctx); err != nil {
		t.Fatalf("reload backend.Open: %v", err)
	}
	defer func() { _ = backend.Close(ctx) }()

	st, err := backend.LoadStore(ctx)
	if err != nil {
		t.Fatalf("reload backend.LoadStore: %v", err)
	}

	got := st.RequirementsSnapshot()["acme.app"].Signatures
	if len(got) != 1 {
		t.Fatalf("persisted acme.app signatures = %v, want exactly one entry", got)
	}
	if strings.Contains(got[0], "?") {
		t.Fatalf("persisted acme.app signature still carries a query")
	}

	// Positive control: the scheme, host and path are not merely absent a
	// query, they are the exact bytes the seeded source declared - proving
	// the cut removed only the query rather than the whole value.
	if got[0] != frozenPersistSignatureSourceStripped {
		t.Fatalf("persisted acme.app signature = %q, want %q", got[0], frozenPersistSignatureSourceStripped)
	}

	// Positive control at the byte level, mirroring
	// TestRequirementsSnapshotDoesNotCarrySignatureQueryCapability: the
	// capability substring must be absent from a fresh re-marshal of what
	// was actually loaded back from the real Bolt file, while the
	// informational part of the source survives - proving the absence above
	// is the cut working, not the field going missing entirely.
	blob, err := st.MarshalSnapshot()
	if err != nil {
		t.Fatalf("MarshalSnapshot of the reloaded store: %v", err)
	}
	if bytes.Contains(blob, []byte(frozenPersistCapabilityMarker)) {
		t.Fatalf("re-marshaled store still contains a signature capability %q", frozenPersistCapabilityMarker)
	}
	if !bytes.Contains(blob, []byte(frozenPersistSignatureSourceStripped)) {
		t.Fatalf("re-marshaled store missing the stripped signature source %q", frozenPersistSignatureSourceStripped)
	}
}
