package store

import "testing"

// TestSignaturesWithoutQueryNoHitReusesBackingArray pins
// signaturesWithoutQuery's own no-allocation, aliasing-preserving claim on
// its no-hit path: when no source in the input carries a query, the
// function must return the input slice itself - not merely a slice equal
// to it - so a caller's later edit to the original backing array is
// visible through the returned slice too. Nothing else in this program
// exercises that path directly; snapshotData's own callers only ever see
// the result through a Requirements entry, never the returned slice header
// itself.
func TestSignaturesWithoutQueryNoHitReusesBackingArray(t *testing.T) {
	t.Parallel()
	sources := []string{
		"https://sigs.example.com/a.asc",
		"https://sigs.example.com/b.asc",
	}

	got := signaturesWithoutQuery(sources)

	// Mutate the original backing array after the call: if got aliases it,
	// the mutation is visible through got too, since both slice headers
	// then view the identical underlying array. A got built from a fresh
	// copy would never observe this.
	sources[0] = "https://sigs.example.com/mutated.asc"
	if got[0] != sources[0] {
		t.Fatalf("got[0] = %q after mutating sources[0] to %q, want the same backing array so the mutation is visible through got",
			got[0], sources[0])
	}

	// Positive control, on a fixture the function is shown capable of
	// cutting: a source that does carry a query is cut, and the slice
	// returned on that path does NOT alias the caller's own backing array -
	// proving the no-hit aliasing above is not just an accident of this
	// function always returning its argument regardless of content.
	withQuery := []string{"https://sigs.example.com/c.asc?tok=SECRET"}
	cut := signaturesWithoutQuery(withQuery)
	if len(cut) != 1 || cut[0] != "https://sigs.example.com/c.asc" {
		t.Fatalf("signaturesWithoutQuery(%v) = %v, want the query cut to %q", withQuery, cut, "https://sigs.example.com/c.asc")
	}
	withQuery[0] = "https://sigs.example.com/c.asc?tok=CHANGED"
	if cut[0] == withQuery[0] {
		t.Fatalf("cut[0] changed after mutating the original query-bearing input, want an independent backing array on the cut path")
	}
}
