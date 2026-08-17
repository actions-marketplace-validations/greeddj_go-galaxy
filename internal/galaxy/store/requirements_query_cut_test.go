package store

import (
	"bytes"
	"testing"
)

// requirementsQueryCapabilityMarker is the sensitive part of the query
// string below, checked for on its own rather than as a substring of the
// whole query for the identical reason
// internal/galaxy/collections/signature_query_persistence_test.go's own
// signatureCapabilityMarker is - see that constant's own doc comment for
// the argument.
const requirementsQueryCapabilityMarker = "X-Amz-Signature=deadbeefcapability"

// requirementsQueryCapabilityQuery is a presigned-URL-shaped query string,
// standing in for the bearer capability the persist-side cut
// (copyRequirementsCutQuery, snapshot.go) exists to keep out of a shared
// snapshot object.
const requirementsQueryCapabilityQuery = requirementsQueryCapabilityMarker + "&X-Amz-Expires=3600"

// requirementsSourceWithQuery and requirementsSourceStripped are the same
// source before and after the cut: scheme, host and path identical, query
// present on one and absent on the other.
const (
	requirementsSourceWithQuery = "https://sigs.example.com/acme-app.asc?" + requirementsQueryCapabilityQuery
	requirementsSourceStripped  = "https://sigs.example.com/acme-app.asc"
)

// TestMarshalSnapshotCutsRequirementsSignatureQueryWrittenDirectly pins the
// finding this file exists for: a Requirements entry that reached the store
// by a path other than Store.SetRequirements must still lose its signature
// sources' query on the way into a persisted snapshot.
//
// The entry below is written straight onto the map rather than through
// SetRequirements, which is what makes this the frozen-path shape the
// finding is about: an install --frozen run that takes
// resolveOrLoadLockfile's lockfile branch never calls
// buildRequirementsSpec/recordResolution - the only path that used to run a
// signature source through normalizeSignatures' own query cut - so an entry
// a pre-fix binary once wrote unstripped, or any other write this program
// never routed through SetRequirements, is exactly what this test seeds.
// SetRequirements' own per-entry Signatures clone (see its doc comment) is
// deliberately not exercised here, since it is not what protects this shape.
func TestMarshalSnapshotCutsRequirementsSignatureQueryWrittenDirectly(t *testing.T) {
	t.Parallel()
	st := New()
	st.Requirements["acme.app"] = RequirementSpec{
		Constraint: "1.0.0",
		Source:     "https://galaxy.example.com",
		Signatures: []string{requirementsSourceWithQuery},
	}

	blob, err := st.MarshalSnapshot()
	if err != nil {
		t.Fatalf("MarshalSnapshot error: %v", err)
	}

	if bytes.Contains(blob, []byte(requirementsQueryCapabilityMarker)) {
		t.Fatalf("snapshot contains a requirements signature capability %q", requirementsQueryCapabilityMarker)
	}

	// Positive control: the source's informational part is still present, so
	// the absence above is the cut working, not the field going missing
	// entirely.
	if !bytes.Contains(blob, []byte(requirementsSourceStripped)) {
		t.Fatalf("snapshot payload does not contain the stripped source %q at all: %s", requirementsSourceStripped, blob)
	}

	// The live store's own value must stay exactly what was written: the cut
	// runs only over snapshotData's RLock-protected copy, and must never
	// write through the aliased slice this entry's Signatures still is.
	live := st.Requirements["acme.app"].Signatures
	if len(live) != 1 || live[0] != requirementsSourceWithQuery {
		t.Fatalf("live store Requirements mutated by MarshalSnapshot: got %v, want [%q]", live, requirementsSourceWithQuery)
	}
}
