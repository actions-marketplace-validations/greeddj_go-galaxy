package requirements

import (
	"errors"
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
			collections, rolesFound, err := ParseCollections([]byte(tc.input), tc.source)
			if err != nil {
				t.Fatalf("ParseCollections error: %v", err)
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
	return []parseCollectionsAcceptedCase{
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
			_, _, err := ParseCollections([]byte(tc.input), tc.source)
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
	return []parseCollectionsRejectedCase{
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
	}
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
			collections, rolesFound, err := ParseCollections([]byte(input), "https://default")
			if err != nil {
				t.Fatalf("ParseCollections error: %v", err)
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
// test has always existed to pin: helpers.SplitFQDN does not split three
// parts, so there is no ambiguous split for the explicit namespace to
// conflict with, and reporting one would send the operator looking for a
// contradiction that is not there.
//
// Until the name alphabet existed this entry parsed successfully and was
// carried as a collection called "a.b.c".
func TestParseCollectionsNamespaceWithThreePartNameIsRejectedAsAName(t *testing.T) {
	t.Parallel()
	input := "- namespace: foo\n  name: a.b.c\n"
	_, _, err := ParseCollections([]byte(input), "https://default")
	if !errors.Is(err, helpers.ErrInvalidCollectionName) {
		t.Fatalf("ParseCollections error = %v, want errors.Is helpers.ErrInvalidCollectionName", err)
	}
	if errors.Is(err, helpers.ErrConflictingNamespaceName) {
		t.Fatalf("a three-part name must not be reported as a namespace conflict: %v", err)
	}

	// Positive control on the same shape: an explicit namespace with a
	// dot-free name is accepted, so the rejection above is the dots and not
	// the explicit-namespace form itself.
	collections, _, err := ParseCollections([]byte("- namespace: acme\n  name: widgets\n"), "https://default")
	if err != nil {
		t.Fatalf("ParseCollections with an explicit namespace and a plain name: %v", err)
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
// carries no dot for it to split. Before this, such a namespace was carried
// into the resolver, which printed it - on an ordinary run with no flags -
// and the run then failed while a URL was being built, unclassified.
func TestParseCollectionsRejectsNamesOutsideTheAlphabet(t *testing.T) {
	t.Parallel()
	for _, tc := range rejectedRequirementNameCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := ParseCollections([]byte(tc.input), "https://default")
			if !errors.Is(err, helpers.ErrInvalidCollectionName) {
				t.Fatalf("ParseCollections error = %v, want errors.Is helpers.ErrInvalidCollectionName", err)
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
			collections, _, err := ParseCollections([]byte(tc.input), "https://default")
			if err != nil {
				t.Fatalf("ParseCollections(%q): %v", tc.input, err)
			}
			if len(collections) != 1 || collections[0].Namespace != "acme" || collections[0].Name != "widgets" {
				t.Fatalf("ParseCollections(%q) = %#v", tc.input, collections)
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
