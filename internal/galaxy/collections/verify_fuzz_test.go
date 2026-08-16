package collections

// FuzzCheckManifestAttribution is this change's one fuzz target, and it sits on
// the one piece of genuinely new decision logic here that reads an
// attacker-chosen document: the identity a signed MANIFEST.json declares.
//
// Everything else this commit added either judges bytes some other package
// already bounds (the signature and chain checks), or judges values a boundary
// validated on the way in (a requirements file's sources). This function is the
// exception: it walks whatever JSON an artifact carries and decides who the
// artifact is about.

import (
	"errors"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// fuzzAttributionIdentity is the collection every fuzzed document is judged
// against. It is spelled out rather than taken from testSignedCollection so a
// change to that fixture cannot quietly change what this target asserts.
//
//nolint:gochecknoglobals // a fixed fixture identity, not mutable shared state.
var fuzzAttributionIdentity = collection{Namespace: "acme", Name: "app", Version: "1.0.0"}

// fuzzAttributionSeeds are the starting points f.Add registers: every document
// shape this package refuses on purpose, the one it accepts, and a handful of
// degenerate shapes that are cheap to state and exercise the decode's own error
// arms rather than its comparison.
//
// The refusal shapes are the folding bypasses foldingBypassShapes carries,
// closed into complete documents - that table's own entries are unterminated,
// since buildArtifactWithManifest appends the chain pointer to them.
func fuzzAttributionSeeds() []string {
	degenerate := []string{
		`{"collection_info":{"namespace":"acme","name":"app","version":"1.0.0"}}`,
		``,
		`{}`,
		`null`,
		`"a bare string"`,
		`{"collection_info":null}`,
		`{"collection_info":{"namespace":1,"name":"app","version":"1.0.0"}}`,
		`{"collection_info":{"namespace":"acme","name":"app","version":"1.0.0"},"collection_info":{}}`,
		strings.Repeat(`{"collection_info":`, 200) + `{}` + strings.Repeat(`}`, 200),
	}

	seeds := make([]string, 0, len(degenerate)+len(foldingBypassShapes))
	seeds = append(seeds, degenerate...)
	for _, shape := range foldingBypassShapes {
		seeds = append(seeds, shape.manifest+`}`)
	}

	return seeds
}

// FuzzCheckManifestAttribution states three invariants, each about what the
// function decides rather than how it decides it, so a rewrite of the decode
// keeps them meaningful:
//
//   - it never panics on any input, which the fuzzing framework enforces around
//     this function. That is not a formality here: the last target this project
//     added found both of its defects exactly that way.
//   - an ACCEPT is only ever an exact agreement. The check is independent of the
//     implementation rather than a second copy of it: a document this function
//     accepts for one identity must be refused for every identity differing in
//     any single component, which can only hold if what it read was byte-for-byte
//     what it was handed. A decode that folded, coerced or normalized anything
//     would accept at least one perturbation too.
//   - a REFUSAL always carries helpers.ErrSignatureAttributionMismatch, so no
//     input escapes into an unclassified error that
//     cmd/go-galaxy/exitcode would report as the generic failure.
func FuzzCheckManifestAttribution(f *testing.F) {
	for _, seed := range fuzzAttributionSeeds() {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, manifestJSON []byte) {
		err := checkManifestAttribution(fuzzAttributionIdentity, manifestJSON)
		if err != nil {
			if !errors.Is(err, helpers.ErrSignatureAttributionMismatch) {
				t.Fatalf("refusal = %v, want errors.Is helpers.ErrSignatureAttributionMismatch", err)
			}

			return
		}
		for _, other := range perturbedIdentities(fuzzAttributionIdentity) {
			if checkManifestAttribution(other, manifestJSON) == nil {
				t.Fatalf("a document accepted for %s was also accepted for %s: an accept must be an exact agreement",
					fuzzAttributionIdentity.key(), other.key())
			}
		}
	})
}

// perturbedIdentities returns col with one component changed at a time, which
// is what makes the acceptance invariant a per-component statement rather than
// a claim about the triple as a whole.
func perturbedIdentities(col collection) []collection {
	namespace, name, version := col, col, col
	namespace.Namespace += "x"
	name.Name += "x"
	version.Version += "1"

	return []collection{namespace, name, version}
}
