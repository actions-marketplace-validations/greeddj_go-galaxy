package requirements

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// TestParseCollectionsAcceptedCases covers the requirements shapes
// ParseCollections accepts. Each row carries its own assertions, since what an
// accepted shape has to produce differs per shape rather than fitting one set
// of expected fields.
func TestParseCollectionsAcceptedCases(t *testing.T) {
	t.Parallel()
	for _, tc := range parseCollectionsAcceptedCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, err := Parse([]byte(tc.input), tc.source)
			collections, rolesFound := f.Collections, len(f.Roles) > 0
			if err != nil {
				t.Fatalf("Parse error: %v", err)
			}
			tc.check(t, collections, rolesFound)
		})
	}
}

// parseCollectionsAcceptedCase is one row of TestParseCollectionsAcceptedCases.
type parseCollectionsAcceptedCase struct {
	check  func(t *testing.T, collections Collections, rolesFound bool)
	name   string
	input  string
	source string
}

// parseCollectionsAcceptedCases enumerates the input shapes ParseCollections
// accepts: the plain string list, the roles-only and null-collections forms,
// the explicit empty list, both ways an entry can name a collection, and the
// two source: forms the userinfo check leaves alone. Each row's assertions sit
// in their own named function below rather than in a closure here, so one row's
// branches are not counted against the whole table.
func parseCollectionsAcceptedCases() []parseCollectionsAcceptedCase {
	return append(signatureShapeAcceptedCases(), []parseCollectionsAcceptedCase{
		{
			name:   "string list",
			input:  "- community.general\n- ansible.posix\n",
			source: "https://default",
			check:  checkAcceptedStringList,
		},
		{
			name:   "roles only",
			input:  "roles:\n  - geerlingguy.foo\n",
			source: "https://default",
			check:  checkAcceptedRolesOnly,
		},
		{
			// checks that a null collections value alongside a roles key still
			// reports rolesFound, matching the roles-only case, with no error
			// and no collections.
			name:   "null collections value with roles",
			input:  "collections:\nroles:\n  - geerlingguy.foo\n",
			source: "https://default",
			check:  checkAcceptedNullValueWithRoles,
		},
		{
			// a regression guard: an explicit empty list ("collections: []")
			// is accepted and yields zero collections and no roles.
			name:   "explicit empty list",
			input:  "collections: []\n",
			source: "https://default",
			check:  checkAcceptedEmptyList,
		},
		{
			// a regression guard: an explicit namespace with a plain
			// (non-dotted) name is unaffected and resolves normally.
			name:   "explicit namespace with plain name",
			input:  "- namespace: foo\n  name: bar\n",
			source: "https://default",
			check:  checkAcceptedNamespaceWithPlainName,
		},
		{
			// a regression guard: a dotted name with no explicit namespace is
			// unaffected and still splits normally, since there is nothing for
			// the split to conflict with.
			name:   "dotted name without namespace",
			input:  "- name: bar.baz\n",
			source: "https://default",
			check:  checkAcceptedDottedNameWithoutNamespace,
		},
		{
			// checks that a source: naming a bare server_list id (never
			// URL-shaped: no "://") is left alone by the userinfo check -
			// url.Parse succeeds on it but yields no scheme/host, so it never
			// reaches the userinfo branch.
			name:   "source bare server_list id",
			input:  "- name: ns.name\n  source: internal\n",
			source: "",
			check:  checkAcceptedBareSourceID,
		},
		{
			// a regression guard: a URL-shaped source: carrying no userinfo
			// passes the userinfo check and is carried through verbatim.
			name:   "source plain URL",
			input:  "- name: ns.name\n  source: https://hub.example/api/\n",
			source: "",
			check:  checkAcceptedPlainSourceURL,
		},
	}...)
}

// signatureShapeAcceptedCases is the positive control for every signatures:
// refusal in parseCollectionsRejectedCases: it proves the gate is about SHAPE
// and about the cap, not about the field being present at all.
//
// The four rows are the shapes that carried nothing before checkSignatureSources
// existed and must keep carrying nothing: an explicit empty list, an absent
// value, a list of blank strings, and a single string - which is what
// parseStringList itself accepts and therefore what the shape check has to keep
// accepting. The fifth is the cap's own boundary, exactly at
// helpers.MaxSignaturesPerCollection, which must be accepted where one more is
// refused.
func signatureShapeAcceptedCases() []parseCollectionsAcceptedCase {
	return []parseCollectionsAcceptedCase{
		{
			name:   "signatures empty list",
			input:  "- name: ns.name\n  signatures: []\n",
			source: "https://default",
			check:  checkAcceptedNoSignatures,
		},
		{
			name:   "signatures absent",
			input:  "- name: ns.name\n  signatures:\n",
			source: "https://default",
			check:  checkAcceptedNoSignatures,
		},
		{
			name:   "signatures list of blanks",
			input:  "- name: ns.name\n  signatures:\n    - \"\"\n    - \"  \"\n",
			source: "https://default",
			check:  checkAcceptedNoSignatures,
		},
		{
			name:   "signatures single string",
			input:  "- name: ns.name\n  signatures: file:///keys/ns-name.asc\n",
			source: "https://default",
			check:  checkAcceptedOneSignature,
		},
		{
			name:   "signatures exactly at the cap",
			input:  signatureSourcesAtCapInput(),
			source: "https://default",
			check:  checkAcceptedSignaturesAtCap,
		},
	}
}

// signatureSourcesAtCapInput builds an entry declaring exactly
// helpers.MaxSignaturesPerCollection sources, the last value the gate accepts.
func signatureSourcesAtCapInput() string {
	var b strings.Builder
	b.WriteString("- name: ns.name\n  signatures:\n")
	for i := range helpers.MaxSignaturesPerCollection {
		fmt.Fprintf(&b, "    - https://sigs.example/%d.asc\n", i)
	}

	return b.String()
}

// checkAcceptedNoSignatures asserts a row whose signatures: value contributes
// nothing at all.
func checkAcceptedNoSignatures(t *testing.T, collections Collections, _ bool) {
	t.Helper()
	if len(collections) != 1 {
		t.Fatalf("expected 1 collection, got %d", len(collections))
	}
	if len(collections[0].Signatures) != 0 {
		t.Fatalf("expected no signatures, got %v", collections[0].Signatures)
	}
}

// checkAcceptedOneSignature asserts the single-string row: the value survives
// as one source rather than being refused for not being a list.
func checkAcceptedOneSignature(t *testing.T, collections Collections, _ bool) {
	t.Helper()
	if len(collections) != 1 {
		t.Fatalf("expected 1 collection, got %d", len(collections))
	}
	want := []string{"file:///keys/ns-name.asc"}
	if got := collections[0].Signatures; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("expected signatures %v, got %v", want, got)
	}
}

// checkAcceptedSignaturesAtCap asserts the boundary row: exactly
// helpers.MaxSignaturesPerCollection sources are kept, none dropped.
func checkAcceptedSignaturesAtCap(t *testing.T, collections Collections, _ bool) {
	t.Helper()
	if len(collections) != 1 {
		t.Fatalf("expected 1 collection, got %d", len(collections))
	}
	if got := len(collections[0].Signatures); got != helpers.MaxSignaturesPerCollection {
		t.Fatalf("expected %d signatures, got %d", helpers.MaxSignaturesPerCollection, got)
	}
}

// checkAcceptedStringList asserts the "string list" row: both entries parse,
// and the first one carries the defaults a bare name gets.
func checkAcceptedStringList(t *testing.T, collections Collections, rolesFound bool) {
	t.Helper()
	if rolesFound {
		t.Fatalf("unexpected rolesFound")
	}
	if len(collections) != 2 {
		t.Fatalf("expected 2 collections, got %d", len(collections))
	}
	if collections[0].Namespace != "community" || collections[0].Name != "general" {
		t.Fatalf("unexpected collection[0]: %#v", collections[0])
	}
	if collections[0].Version != "*" {
		t.Fatalf("expected default version '*', got %q", collections[0].Version)
	}
	if collections[0].Source != "https://default" {
		t.Fatalf("expected default source, got %q", collections[0].Source)
	}
}

// checkAcceptedRolesOnly asserts the "roles only" row. The assertion is on the
// nil collections value itself, not on its length: a roles-only file must leave
// the collections list unset rather than merely empty, which an emptiness check
// would not distinguish from the null and empty-list rows.
func checkAcceptedRolesOnly(t *testing.T, collections Collections, rolesFound bool) {
	t.Helper()
	if !rolesFound {
		t.Fatalf("expected rolesFound")
	}
	if collections != nil {
		t.Fatalf("expected nil collections, got %#v", collections)
	}
}

// checkAcceptedNullValueWithRoles asserts the "null collections value with
// roles" row.
func checkAcceptedNullValueWithRoles(t *testing.T, collections Collections, rolesFound bool) {
	t.Helper()
	if !rolesFound {
		t.Fatalf("expected rolesFound")
	}
	if len(collections) != 0 {
		t.Fatalf("expected 0 collections, got %d", len(collections))
	}
}

// checkAcceptedEmptyList asserts the "explicit empty list" row.
func checkAcceptedEmptyList(t *testing.T, collections Collections, rolesFound bool) {
	t.Helper()
	if rolesFound {
		t.Fatalf("unexpected rolesFound")
	}
	if len(collections) != 0 {
		t.Fatalf("expected 0 collections, got %d", len(collections))
	}
}

// checkAcceptedNamespaceWithPlainName asserts the "explicit namespace with
// plain name" row.
func checkAcceptedNamespaceWithPlainName(t *testing.T, collections Collections, _ bool) {
	t.Helper()
	if len(collections) != 1 {
		t.Fatalf("expected 1 collection, got %d", len(collections))
	}
	if collections[0].Namespace != "foo" || collections[0].Name != "bar" {
		t.Fatalf("unexpected collection[0]: %#v", collections[0])
	}
}

// checkAcceptedDottedNameWithoutNamespace asserts the "dotted name without
// namespace" row.
func checkAcceptedDottedNameWithoutNamespace(t *testing.T, collections Collections, _ bool) {
	t.Helper()
	if len(collections) != 1 {
		t.Fatalf("expected 1 collection, got %d", len(collections))
	}
	if collections[0].Namespace != "bar" || collections[0].Name != "baz" {
		t.Fatalf("unexpected collection[0]: %#v", collections[0])
	}
}

// checkAcceptedBareSourceID asserts the "source bare server_list id" row.
func checkAcceptedBareSourceID(t *testing.T, collections Collections, _ bool) {
	t.Helper()
	if len(collections) != 1 || collections[0].Source != "internal" {
		t.Fatalf("unexpected collections: %#v", collections)
	}
}

// checkAcceptedPlainSourceURL asserts the "source plain URL" row.
func checkAcceptedPlainSourceURL(t *testing.T, collections Collections, _ bool) {
	t.Helper()
	if len(collections) != 1 || collections[0].Source != "https://hub.example/api/" {
		t.Fatalf("unexpected collections: %#v", collections)
	}
}

// TestParseCollectionsRejectedCases covers the requirements shapes
// ParseCollections refuses, each row naming the sentinel the refusal has to
// carry and, for an input embedding a credential, the substring the error must
// not echo back.
func TestParseCollectionsRejectedCases(t *testing.T) {
	t.Parallel()
	for _, tc := range parseCollectionsRejectedCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(tc.input), tc.source)
			if err == nil {
				t.Fatalf("expected error")
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("expected %v, got %v", tc.wantErr, err)
			}
			if tc.mustNotContain != "" && strings.Contains(err.Error(), tc.mustNotContain) {
				t.Fatalf("error must not echo the rejected source's credential, got %v", err)
			}
		})
	}
}

// parseCollectionsRejectedCase is one row of TestParseCollectionsRejectedCases.
// An empty mustNotContain skips the substring check, which only the
// credential-bearing rows need.
type parseCollectionsRejectedCase struct {
	wantErr        error
	name           string
	input          string
	source         string
	mustNotContain string
}

// parseCollectionsRejectedCases enumerates the input shapes ParseCollections
// refuses: the two unsupported requirements formats, a scalar collections
// value, both namespace/name conflicts, and the two entry shapes a
// credential-bearing source: can arrive in.
func parseCollectionsRejectedCases() []parseCollectionsRejectedCase {
	return append([]parseCollectionsRejectedCase{
		{
			name:    "unsupported format",
			input:   "foo: bar\n",
			source:  "https://default",
			wantErr: helpers.ErrUnsupportedRequirementsFormat,
		},
		{
			name:    "unsupported source",
			input:   "- https://example.com/collections\n",
			source:  "https://default",
			wantErr: helpers.ErrUnsupportedCollectionSource,
		},
		{
			// a regression guard: a scalar collections value (neither null nor
			// a list) must still be rejected; only nil is accepted.
			name:    "scalar collections value",
			input:   "collections: foo\n",
			source:  "https://default",
			wantErr: helpers.ErrInvalidCollectionsList,
		},
		{
			// checks that an explicit namespace combined with a dotted name is
			// rejected, since normalizeCollectionName would otherwise silently
			// keep the explicit namespace and overwrite name with only the
			// dotted name's last segment - installing a different collection
			// than either field implies alone.
			name:    "namespace plus dotted name conflict",
			input:   "- namespace: foo\n  name: bar.baz\n",
			source:  "https://default",
			wantErr: helpers.ErrConflictingNamespaceName,
		},
		{
			// checks that the conflict is rejected unconditionally - even when
			// the explicit namespace happens to match the dotted name's own
			// namespace segment, so the two fields "look" consistent. This is
			// intentional: the rule is about the shape of the input (namespace
			// + dotted name is ambiguous), not about whether this particular
			// combination happens to resolve harmlessly.
			name:    "namespace plus dotted name conflict even when consistent",
			input:   "- namespace: community\n  name: community.general\n",
			source:  "https://default",
			wantErr: helpers.ErrConflictingNamespaceName,
		},
		{
			// pins the closed hole: a requirements.yml "source:" carrying
			// embedded userinfo (a credential in the URL itself) must be
			// rejected at parse time with ErrGalaxyServerURLUserinfo, the same
			// sentinel config.Server's own URL validation uses. Without this
			// check the userinfo-bearing URL flows unchanged into root-metadata
			// request URLs, debug logs, HTTP error strings, the lockfile, and
			// GALAXY.yml, all of which would then render the embedded password
			// in plain text via url.URL.String().
			name: "source userinfo rejected",
			// #nosec G101 -- test fixture literal, not a real credential
			input:          "- name: ns.name\n  source: https://user:tok3n-must-not-leak@hub.example/api/\n",
			source:         "",
			wantErr:        helpers.ErrGalaxyServerURLUserinfo,
			mustNotContain: "tok3n-must-not-leak",
		},
		{
			// a regression guard for an ordering bug: an entry missing
			// "name" fails with ErrInvalidCollectionEntry, whose message echoes
			// the raw item back for diagnostics - and that raw item can itself
			// carry the very credential-bearing source: this package's userinfo
			// check exists to catch. The userinfo check must run before that
			// raw dump, not after, or an invalid entry becomes a way to smuggle
			// the credential out through its own error message.
			name: "invalid entry does not leak source credential",
			// #nosec G101 -- test fixture literal, not a real credential
			input:          "- source: https://user:tok3n-must-not-leak@hub.example/api/\n  version: \"*\"\n",
			source:         "",
			wantErr:        helpers.ErrGalaxyServerURLUserinfo,
			mustNotContain: "tok3n-must-not-leak",
		},
	}, signatureSourceRejectedCases()...)
}

// signatureSourceRejectedCases is the signatures: half of the table above,
// split out for the length budget rather than because it is a separate
// concern: every row is one way checkSignatureSources refuses a value a
// requirements file declared.
func signatureSourceRejectedCases() []parseCollectionsRejectedCase {
	return append([]parseCollectionsRejectedCase{
		{
			// The signatures: field is repository content like source: and,
			// until checkSignatureSources existed, the only one of this struct's
			// fields no boundary judged. A source this tool cannot fetch reached
			// an install worker and failed one collection there, classified as
			// that collection's failure rather than as the configuration error
			// it is.
			name:    "unfetchable signature source rejected",
			input:   "- name: ns.name\n  signatures:\n    - ftp://sigs.example/ns-name.asc\n",
			source:  "https://default",
			wantErr: helpers.ErrUnsupportedSignatureSource,
		},
		{
			// The same shape source: already refuses, one field over, and with
			// the same obligation: the refusal must not print what it refuses.
			// Measured before this check existed, in a serialized snapshot
			// shared across runners:
			// "signatures":["https://ci-bot:s3cr3t@sig.example/acme-app.asc"].
			name: "signature source userinfo rejected without echoing it",
			// #nosec G101 -- test fixture literal, not a real credential
			input:          "- name: ns.name\n  signatures:\n    - https://bot:tok3n-must-not-leak@sig.example/a.asc\n",
			source:         "https://default",
			wantErr:        helpers.ErrSignatureSourceUserinfo,
			mustNotContain: "tok3n-must-not-leak",
		},
		{
			// Without the shape check, parseStringList's fmt.Sprint arm turns a
			// mapping into the plausible-looking source "map[]", which nothing
			// downstream can tell from one an author wrote.
			name:    "signatures mapping rejected",
			input:   "- name: ns.name\n  signatures:\n    key: value\n",
			source:  "https://default",
			wantErr: helpers.ErrUnsupportedSignatureSource,
		},
		{
			// The same arm one level in: a list carrying a non-string element,
			// which fmt.Sprint would render as "false" or "0".
			name:    "signatures list element that is not a string rejected",
			input:   "- name: ns.name\n  signatures:\n    - false\n",
			source:  "https://default",
			wantErr: helpers.ErrUnsupportedSignatureSource,
		},
	}, fileSourceRejectedCases()...)
}

// fileSourceRejectedCases is the file-scheme half of the signatures: table,
// split out for the length budget. Every row is a file URL that named no local
// path this tool can read.
func fileSourceRejectedCases() []parseCollectionsRejectedCase {
	return []parseCollectionsRejectedCase{
		{
			// The five shapes measured accepted at load and refused at fetch
			// before checkFileSource moved into the shared grammar. Each is a
			// file URL naming no local path this tool can read; joined behind
			// helpers.ErrInstallationFailed they exited 5 rather than 2, which
			// is the exit-class defect this gate exists to close.
			name:    "file source naming another host",
			input:   "- name: ns.name\n  signatures:\n    - file://otherhost/abs/sig.asc\n",
			source:  "https://default",
			wantErr: helpers.ErrUnsupportedSignatureSource,
		},
		{
			name:    "file source naming an evil host",
			input:   "- name: ns.name\n  signatures:\n    - file://evil.example/abs/sig.asc\n",
			source:  "https://default",
			wantErr: helpers.ErrUnsupportedSignatureSource,
		},
		{
			name:    "bare file scheme",
			input:   "- name: ns.name\n  signatures:\n    - \"file:\"\n",
			source:  "https://default",
			wantErr: helpers.ErrUnsupportedSignatureSource,
		},
		{
			name:    "file scheme with an empty authority and no path",
			input:   "- name: ns.name\n  signatures:\n    - file://\n",
			source:  "https://default",
			wantErr: helpers.ErrUnsupportedSignatureSource,
		},
		{
			name:    "file scheme naming localhost and no path",
			input:   "- name: ns.name\n  signatures:\n    - file://localhost\n",
			source:  "https://default",
			wantErr: helpers.ErrUnsupportedSignatureSource,
		},
		{
			// This row pins the cap on what one entry may DECLARE, and only
			// that: gatherLimit (internal/galaxy/collections/verify.go) is the
			// separate, later boundary that decides what a gather does once
			// the combined candidate set - this entry's own sources plus
			// whatever the server offers - exceeds MaxSignaturesPerCollection,
			// and reports it.
			name:    "more signature sources than the cap allows",
			input:   tooManySignatureSourcesInput(),
			source:  "https://default",
			wantErr: helpers.ErrTooManySignatureSources,
		},
	}
}

// tooManySignatureSourcesInput builds a requirements entry declaring one more
// signature source than helpers.MaxSignaturesPerCollection permits. It is
// generated from the constant rather than spelled out, since the point is the
// boundary rather than any particular count - and the row below it in
// parseCollectionsAcceptedCases proves the cap itself is accepted.
func tooManySignatureSourcesInput() string {
	var b strings.Builder
	b.WriteString("- name: ns.name\n  signatures:\n")
	for i := range helpers.MaxSignaturesPerCollection + 1 {
		fmt.Fprintf(&b, "    - https://sigs.example/%d.asc\n", i)
	}

	return b.String()
}

// TestParseCollectionsNullValue checks that ansible's null-collections-list
// idioms ("collections:" and "collections: ~") are accepted as an empty
// list rather than rejected, since both unmarshal collections_path to a nil
// interface value.
func TestParseCollectionsNullValue(t *testing.T) {
	t.Parallel()
	inputs := map[string]string{
		"bare key":   "collections:\n",
		"tilde null": "collections: ~\n",
	}
	for name, input := range inputs {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, err := Parse([]byte(input), "https://default")
			collections, rolesFound := f.Collections, len(f.Roles) > 0
			if err != nil {
				t.Fatalf("Parse error: %v", err)
			}
			if rolesFound {
				t.Fatalf("unexpected rolesFound")
			}
			if len(collections) != 0 {
				t.Fatalf("expected 0 collections, got %d", len(collections))
			}
		})
	}
}

// TestParseCollectionsNamespaceWithThreePartNameIsRejectedAsAName checks a
// three-part dotted name (e.g. "a.b.c") with an explicit namespace set, and
// checks it for two things at once. It is rejected - no Galaxy server has a
// collection whose name contains a dot - and it is rejected as an invalid
// name rather than as a namespace/name conflict, which is the property this
// test exists to pin: helpers.SplitFQDN does not split three
// parts, so there is no ambiguous split for the explicit namespace to
// conflict with, and reporting one would send the operator looking for a
// contradiction that is not there.
//
// Without the name alphabet this entry parses successfully and is carried
// as a collection called "a.b.c".
func TestParseCollectionsNamespaceWithThreePartNameIsRejectedAsAName(t *testing.T) {
	t.Parallel()
	input := "- namespace: foo\n  name: a.b.c\n"
	_, err := Parse([]byte(input), "https://default")
	if !errors.Is(err, helpers.ErrInvalidCollectionName) {
		t.Fatalf("Parse error = %v, want errors.Is helpers.ErrInvalidCollectionName", err)
	}
	if errors.Is(err, helpers.ErrConflictingNamespaceName) {
		t.Fatalf("a three-part name must not be reported as a namespace conflict: %v", err)
	}

	// Positive control on the same shape: an explicit namespace with a
	// dot-free name is accepted, so the rejection above is the dots and not
	// the explicit-namespace form itself.
	f, err := Parse([]byte("- namespace: acme\n  name: widgets\n"), "https://default")
	collections := f.Collections
	if err != nil {
		t.Fatalf("Parse with an explicit namespace and a plain name: %v", err)
	}
	if len(collections) != 1 || collections[0].Namespace != "acme" || collections[0].Name != "widgets" {
		t.Fatalf("unexpected collections: %#v", collections)
	}
}

// TestParseCollectionsRejectsNamesOutsideTheAlphabet covers the boundary a
// requirements file is: every identity it declares is checked against the
// alphabet a Galaxy server itself accepts, so a name this file could not
// install is refused where it was written rather than much later.
//
// The explicit-namespace row is why the check sits at the entry level and not
// inside helpers.SplitFQDN: that form never reaches SplitFQDN, since the name
// carries no dot for it to split. Without the entry-level check such a
// namespace reaches the resolver, which prints it - on an ordinary run with
// no flags - and the run then fails while a URL is being built, unclassified.
func TestParseCollectionsRejectsNamesOutsideTheAlphabet(t *testing.T) {
	t.Parallel()
	for _, tc := range rejectedRequirementNameCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(tc.input), "https://default")
			if !errors.Is(err, helpers.ErrInvalidCollectionName) {
				t.Fatalf("Parse error = %v, want errors.Is helpers.ErrInvalidCollectionName", err)
			}
		})
	}

	// Control on the same two shapes: the dotted form and the explicit form
	// both parse when their identities are inside the alphabet, so neither
	// rejection above is the shape itself being refused.
	controls := []struct {
		name  string
		input string
	}{
		{name: "dotted name", input: "- name: acme.widgets\n"},
		{name: "explicit namespace, plain name", input: "- namespace: acme\n  name: widgets\n"},
	}
	for _, tc := range controls {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, err := Parse([]byte(tc.input), "https://default")
			collections := f.Collections
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.input, err)
			}
			if len(collections) != 1 || collections[0].Namespace != "acme" || collections[0].Name != "widgets" {
				t.Fatalf("Parse(%q) = %#v", tc.input, collections)
			}
		})
	}
}

// rejectedRequirementNameCase is one row of
// TestParseCollectionsRejectsNamesOutsideTheAlphabet.
type rejectedRequirementNameCase struct {
	name  string
	input string
}

// rejectedRequirementNameCases covers the forged-line shape in each of the two
// places a requirements entry can declare an identity, plus the two ordinary
// alphabet violations.
func rejectedRequirementNameCases() []rejectedRequirementNameCase {
	return []rejectedRequirementNameCase{
		{name: "forged line in an explicit namespace", input: "- namespace: \"acme\\n[CRITICAL] X\"\n  name: widgets\n"},
		{name: "forged line in a dotted name", input: "- name: \"acme.widgets\\n[CRITICAL] X\"\n"},
		{name: "uppercase", input: "- name: Acme.Widgets\n"},
		{name: "hyphen", input: "- name: acme.my-widgets\n"},
	}
}
