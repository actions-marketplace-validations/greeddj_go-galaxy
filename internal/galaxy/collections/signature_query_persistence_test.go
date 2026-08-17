package collections

// This file pins the persisted-spec query cut: a signature source's query
// string must not survive into buildRequirementsSpec's output, must not
// survive into the serialized snapshot that output feeds, and a store
// carrying an entry written before the cut existed must not fail a run over
// it.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// signatureCapabilityMarker is the sensitive part of the query string below,
// checked for on its own because json.Marshal HTML-escapes '&' into the
// six-byte sequence \u0026 (backslash, u, 0, 0, 2, 6): a needle spanning the
// '&' between the two query parameters would never literally appear in
// serialized bytes even when the query survives unstripped, silently
// defeating the byte-level check this file uses it for.
const signatureCapabilityMarker = "X-Amz-Signature=deadbeefcapability"

// signatureCapabilityQuery is a presigned-URL-shaped query string, standing
// in for the bearer capability normalizeSignatures' query cut exists to keep
// out of persisted state.
const signatureCapabilityQuery = signatureCapabilityMarker + "&X-Amz-Expires=3600"

// signatureSourceWithQuery and signatureSourceStripped are the same source
// before and after the cut: scheme, host and path identical, query present
// on one and absent on the other.
const (
	signatureSourceWithQuery = "https://sigs.example.com/acme-app.asc?" + signatureCapabilityQuery
	signatureSourceStripped  = "https://sigs.example.com/acme-app.asc"
)

// TestBuildRequirementsSpecCutsSignatureQuery proves buildRequirementsSpec
// (through normalizeSignatures) strips a declared signature source's query
// before it ever reaches the persisted spec, and that the cut is a cut
// rather than a wipe: scheme, host and path survive byte for byte.
//
// The positive control lives on the same fixture as the refusal it proves
// something about: without it, a stored value of "" would also carry no "?"
// and this test would pass for the wrong reason.
func TestBuildRequirementsSpecCutsSignatureQuery(t *testing.T) {
	t.Parallel()
	roots := []collection{{
		Namespace:  "acme",
		Name:       "app",
		Constraint: "*",
		Source:     "https://galaxy.example.com",
		Signatures: []string{signatureSourceWithQuery},
	}}

	spec := buildRequirementsSpec(roots)
	got := spec["acme.app"].Signatures
	if len(got) != 1 {
		t.Fatalf("stored signatures = %v, want exactly one entry", got)
	}
	if strings.Contains(got[0], "?") {
		t.Fatalf("stored signature %q still carries a query", got[0])
	}

	// Positive control: the scheme, host and path are not merely absent, they
	// are the exact bytes the source declared - proving the cut removed only
	// the query rather than the whole value.
	if got[0] != signatureSourceStripped {
		t.Fatalf("stored signature = %q, want %q (scheme/host/path preserved byte for byte)", got[0], signatureSourceStripped)
	}
}

// TestRequirementsSnapshotDoesNotCarrySignatureQueryCapability is the sink assertion:
// a spec built from a query-bearing signature source, once stored through
// Store.SetRequirements and serialized through Store.MarshalSnapshot, must never
// contain the capability substring. It is deliberately insensitive to which cut
// enforces that, so it survives a refactor that moves the cut. The per-cut jobs are
// pinned one site each: TestBuildRequirementsSpecCutsSignatureQuery above pins the
// producer cut, TestMarshalSnapshotCutsRequirementsSignatureQueryWrittenDirectly
// (internal/galaxy/store) pins the persist cut.
//
// KILLING MUTATION, run and reverted: both cuts that could strip this query
// neutralized at once - normalizeSignatures' own helpers.WithoutQuery call
// replaced by the bare trimmed value it used to append (resolve.go), and
// store.snapshotData's own persist-side backstop (copyRequirementsCutQuery)
// replaced by the maps.Copy it replaced
// (internal/galaxy/store/snapshot.go) - applied through go test -overlay
// against both files at once so neither production file was left edited.
// Either cut alone now suffices to pass this test, which is the backstop's
// whole point, so this is the smallest mutation that still kills it. The
// substring check fails:
//
//	signature_query_persistence_test.go:121: snapshot contains a signature capability "X-Amz-Signature=deadbeefcapability"
func TestRequirementsSnapshotDoesNotCarrySignatureQueryCapability(t *testing.T) {
	t.Parallel()
	roots := []collection{{
		Namespace:  "acme",
		Name:       "app",
		Constraint: "*",
		Source:     "https://galaxy.example.com",
		Signatures: []string{signatureSourceWithQuery},
	}}
	spec := buildRequirementsSpec(roots)

	st := store.New()
	st.SetRequirements(spec)
	blob, err := st.MarshalSnapshot()
	if err != nil {
		t.Fatalf("MarshalSnapshot: %v", err)
	}

	if bytes.Contains(blob, []byte(signatureCapabilityMarker)) {
		t.Fatalf("snapshot contains a signature capability %q", signatureCapabilityMarker)
	}

	// Positive control: the source's informational part is still present, so
	// the absence above is the cut working, not the field going missing
	// entirely.
	if !bytes.Contains(blob, []byte(signatureSourceStripped)) {
		t.Fatalf("snapshot payload does not contain the stripped signature source %q at all: %s", signatureSourceStripped, blob)
	}

	var decoded struct {
		Requirements map[string]store.RequirementSpec `json:"requirements"`
	}
	if err := json.Unmarshal(blob, &decoded); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if got := decoded.Requirements["acme.app"].Signatures; len(got) != 1 || got[0] != signatureSourceStripped {
		t.Fatalf("round-tripped signatures = %v, want [%q]", got, signatureSourceStripped)
	}
}

// legacyRequirementsSignature reproduces requirementsSignatureFromSpec's
// pre-cut formula: trim and sort only, no helpers.WithoutQuery. It exists so
// TestUnstrippedPersistedSignatureQuerySelfHeals can seed a store the way a
// binary predating this cut actually would have left it - a requirements
// hash computed the same, self-consistent way the persisted spec itself was
// written, over an unstripped signature.
func legacyRequirementsSignature(t *testing.T, spec map[string]requirementSpec, noDeps bool, serversSig string) string {
	t.Helper()
	parts := make([]string, 0, len(spec))
	for fqdn, entry := range spec {
		constraint := entry.Constraint
		if constraint == "" {
			constraint = "*"
		}
		sigs := make([]string, len(entry.Signatures))
		for i, value := range entry.Signatures {
			sigs[i] = strings.TrimSpace(value)
		}
		parts = append(parts, fmt.Sprintf("%s|%s|%s|%s|%s", fqdn, constraint, entry.Source, entry.Type, strings.Join(sigs, ",")))
	}
	header := fmt.Sprintf("no-deps=%t\nservers=%s", noDeps, serversSig)
	sum := sha256.Sum256([]byte(header + "\n" + strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:])
}

// TestUnstrippedPersistedSignatureQuerySelfHeals is the upgrade direction:
// a store whose Requirements bucket was written before this cut existed -
// signature query intact, and a requirements hash computed the same,
// pre-cut way over it - does not fail a run against it. The stored hash and
// the freshly recomputed one disagree once this binary's own
// normalizeSignatures strips the query, so snapshotMatchesRequirements and
// tryIncrementalResolve's own identical check both refuse to treat the
// snapshot as current; the run falls back to a full resolve instead of
// failing, and recordResolutionIfNeeded then rewrites the persisted spec in
// the current, stripped form.
//
// Both checks failing here is a property of the upgrade direction alone, not
// of the cut in general: the downgrade direction disagrees, and
// normalizeSignatures' own doc comment (resolve.go) holds that half of the
// argument.
func TestUnstrippedPersistedSignatureQuerySelfHeals(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "app", testVersion100, nil)

	cfg := &config.Config{Server: srv.URL(), Workers: 2}
	runtime := infra.New(noopPrinter{}, srv.Client())
	st := store.New()

	root := collection{
		Namespace: "acme", Name: "app", Constraint: "^1.0.0",
		Source: srv.URL(), Signatures: []string{signatureSourceWithQuery},
	}

	oldSpec := map[string]requirementSpec{
		"acme.app": {Constraint: "^1.0.0", Source: srv.URL(), Signatures: []string{signatureSourceWithQuery}},
	}
	oldHash := legacyRequirementsSignature(t, oldSpec, cfg.NoDeps, serversSignature(cfg))
	st.SetMetaRequirements(oldHash, cfg.Server)
	st.SetRequirements(oldSpec)

	resolved, _, err := resolveCollectionsInternal(
		context.Background(), newCollectionDeps(cfg, runtime, st), []collection{root}, true, true,
	)
	if err != nil {
		t.Fatalf("resolveCollectionsInternal (unstripped persisted spec) = %v, want nil", err)
	}
	if resolved["acme.app"].Version != testVersion100 {
		t.Fatalf("resolved[acme.app].Version = %q, want %q", resolved["acme.app"].Version, testVersion100)
	}

	got := st.RequirementsSnapshot()["acme.app"].Signatures
	if len(got) != 1 || got[0] != signatureSourceStripped {
		t.Fatalf("persisted spec after self-heal = %v, want [%q]", got, signatureSourceStripped)
	}
}

// TestLegacyRequirementsSignatureAgreesOnlyOnAStrippedSpec pins the
// downgrade-direction identity normalizeSignatures' own doc comment now
// states: legacyRequirementsSignature (the pre-cut formula - trim and sort
// only) agrees with today's requirementsSignatureFromSpec on a spec whose
// signature sources are already query-free, because helpers.WithoutQuery is
// a no-op over a value with nothing left to cut. That agreement is what lets
// an old binary's own self-consistency check pass against a hash a newer
// binary computed, take the targeted incremental path, and restore the
// query on write - the mechanism that doc comment's downgrade paragraph
// describes.
//
// The two formulas disagree on the identical spec shape carrying an
// unstripped query instead, asserted here as the fixture's own positive
// control, in the same test as the agreement it qualifies: without it, an
// equality assertion that never fires on any input - because the two
// formulas happened to always agree, or the test built a spec neither could
// tell apart - would pass for the wrong reason.
func TestLegacyRequirementsSignatureAgreesOnlyOnAStrippedSpec(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Server: "https://galaxy.example.com"}
	serversSig := serversSignature(cfg)

	strippedSpec := map[string]requirementSpec{
		"acme.app": {Constraint: "^1.0.0", Source: cfg.Server, Signatures: []string{signatureSourceStripped}},
	}
	legacyStripped := legacyRequirementsSignature(t, strippedSpec, cfg.NoDeps, serversSig)
	currentStripped := requirementsSignatureFromSpec(strippedSpec, cfg.NoDeps, serversSig)
	if legacyStripped != currentStripped {
		t.Fatalf("legacy = %s, current = %s, want equal over an already-stripped spec (the downgrade direction's own agreement)",
			legacyStripped, currentStripped)
	}

	// Positive control: the same two formulas over the same spec shape, but
	// carrying the query the stripped fixture above lacks, must disagree -
	// this is the upgrade direction's own mismatch, and its absence here
	// would mean the equality above proved nothing about the cut
	// specifically.
	unstrippedSpec := map[string]requirementSpec{
		"acme.app": {Constraint: "^1.0.0", Source: cfg.Server, Signatures: []string{signatureSourceWithQuery}},
	}
	legacyUnstripped := legacyRequirementsSignature(t, unstrippedSpec, cfg.NoDeps, serversSig)
	currentUnstripped := requirementsSignatureFromSpec(unstrippedSpec, cfg.NoDeps, serversSig)
	if legacyUnstripped == currentUnstripped {
		t.Fatalf("legacy = %s, current = %s, want different over an unstripped spec (the upgrade direction's own mismatch)",
			legacyUnstripped, currentUnstripped)
	}
}
