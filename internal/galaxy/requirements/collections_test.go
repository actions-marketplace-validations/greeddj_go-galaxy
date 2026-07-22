package requirements

import (
	"errors"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

func TestParseCollectionsStringList(t *testing.T) {
	t.Parallel()
	input := "- community.general\n- ansible.posix\n"
	collections, rolesFound, err := ParseCollections([]byte(input), "https://default")
	if err != nil {
		t.Fatalf("ParseCollections error: %v", err)
	}
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

func TestParseCollectionsRolesOnly(t *testing.T) {
	t.Parallel()
	input := "roles:\n  - geerlingguy.foo\n"
	collections, rolesFound, err := ParseCollections([]byte(input), "https://default")
	if err != nil {
		t.Fatalf("ParseCollections error: %v", err)
	}
	if !rolesFound {
		t.Fatalf("expected rolesFound")
	}
	if collections != nil {
		t.Fatalf("expected nil collections, got %#v", collections)
	}
}

func TestParseCollectionsUnsupportedFormat(t *testing.T) {
	t.Parallel()
	input := "foo: bar\n"
	_, _, err := ParseCollections([]byte(input), "https://default")
	if err == nil {
		t.Fatalf("expected error")
	}
	if !errors.Is(err, helpers.ErrUnsupportedRequirementsFormat) {
		t.Fatalf("expected ErrUnsupportedRequirementsFormat, got %v", err)
	}
}

func TestParseCollectionsUnsupportedSource(t *testing.T) {
	t.Parallel()
	input := "- https://example.com/collections\n"
	_, _, err := ParseCollections([]byte(input), "https://default")
	if err == nil {
		t.Fatalf("expected error")
	}
	if !errors.Is(err, helpers.ErrUnsupportedCollectionSource) {
		t.Fatalf("expected ErrUnsupportedCollectionSource, got %v", err)
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

// TestParseCollectionsNullValueWithRoles checks that a null collections
// value alongside a roles key still reports rolesFound, matching the
// roles-only case, with no error and no collections.
func TestParseCollectionsNullValueWithRoles(t *testing.T) {
	t.Parallel()
	input := "collections:\nroles:\n  - geerlingguy.foo\n"
	collections, rolesFound, err := ParseCollections([]byte(input), "https://default")
	if err != nil {
		t.Fatalf("ParseCollections error: %v", err)
	}
	if !rolesFound {
		t.Fatalf("expected rolesFound")
	}
	if len(collections) != 0 {
		t.Fatalf("expected 0 collections, got %d", len(collections))
	}
}

// TestParseCollectionsEmptyList is a regression guard: an explicit empty
// list ("collections: []") must keep working the same as before the null
// guard was added.
func TestParseCollectionsEmptyList(t *testing.T) {
	t.Parallel()
	input := "collections: []\n"
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
}

// TestParseCollectionsScalarStillErrors is a regression guard: a scalar
// collections value (neither null nor a list) must still be rejected;
// only nil is newly accepted.
func TestParseCollectionsScalarStillErrors(t *testing.T) {
	t.Parallel()
	input := "collections: foo\n"
	_, _, err := ParseCollections([]byte(input), "https://default")
	if err == nil {
		t.Fatalf("expected error")
	}
	if !errors.Is(err, helpers.ErrInvalidCollectionsList) {
		t.Fatalf("expected ErrInvalidCollectionsList, got %v", err)
	}
}

// TestParseCollectionsNamespaceNameConflict checks that an explicit
// namespace combined with a dotted name is rejected, since
// normalizeCollectionName would otherwise silently keep the explicit
// namespace and overwrite name with only the dotted name's last segment -
// installing a different collection than either field implies alone.
func TestParseCollectionsNamespaceNameConflict(t *testing.T) {
	t.Parallel()
	input := "- namespace: foo\n  name: bar.baz\n"
	_, _, err := ParseCollections([]byte(input), "https://default")
	if err == nil {
		t.Fatalf("expected error")
	}
	if !errors.Is(err, helpers.ErrConflictingNamespaceName) {
		t.Fatalf("expected ErrConflictingNamespaceName, got %v", err)
	}
}

// TestParseCollectionsNamespaceNameConflictEvenWhenConsistent checks that
// the conflict is rejected unconditionally - even when the explicit
// namespace happens to match the dotted name's own namespace segment, so
// the two fields "look" consistent. This is intentional: the rule is about
// the shape of the input (namespace + dotted name is ambiguous), not about
// whether this particular combination happens to resolve harmlessly.
func TestParseCollectionsNamespaceNameConflictEvenWhenConsistent(t *testing.T) {
	t.Parallel()
	input := "- namespace: community\n  name: community.general\n"
	_, _, err := ParseCollections([]byte(input), "https://default")
	if err == nil {
		t.Fatalf("expected error")
	}
	if !errors.Is(err, helpers.ErrConflictingNamespaceName) {
		t.Fatalf("expected ErrConflictingNamespaceName, got %v", err)
	}
}

// TestParseCollectionsNamespaceWithPlainName is a regression guard: an
// explicit namespace with a plain (non-dotted) name is unaffected and
// resolves normally.
func TestParseCollectionsNamespaceWithPlainName(t *testing.T) {
	t.Parallel()
	input := "- namespace: foo\n  name: bar\n"
	collections, _, err := ParseCollections([]byte(input), "https://default")
	if err != nil {
		t.Fatalf("ParseCollections error: %v", err)
	}
	if len(collections) != 1 {
		t.Fatalf("expected 1 collection, got %d", len(collections))
	}
	if collections[0].Namespace != "foo" || collections[0].Name != "bar" {
		t.Fatalf("unexpected collection[0]: %#v", collections[0])
	}
}

// TestParseCollectionsDottedNameWithoutNamespace is a regression guard: a
// dotted name with no explicit namespace is unaffected and still splits
// normally, since there is nothing for the split to conflict with.
func TestParseCollectionsDottedNameWithoutNamespace(t *testing.T) {
	t.Parallel()
	input := "- name: bar.baz\n"
	collections, _, err := ParseCollections([]byte(input), "https://default")
	if err != nil {
		t.Fatalf("ParseCollections error: %v", err)
	}
	if len(collections) != 1 {
		t.Fatalf("expected 1 collection, got %d", len(collections))
	}
	if collections[0].Namespace != "bar" || collections[0].Name != "baz" {
		t.Fatalf("unexpected collection[0]: %#v", collections[0])
	}
}

// TestParseCollectionsNamespaceWithThreePartNameUnaffected checks that a
// three-part dotted name (e.g. "a.b.c"), for which helpers.SplitFQDN does
// not succeed (it only splits exactly two parts), does not trigger a false
// conflict even with an explicit namespace set: there is no ambiguous split
// for it to conflict with, so the name passes through unchanged.
func TestParseCollectionsNamespaceWithThreePartNameUnaffected(t *testing.T) {
	t.Parallel()
	input := "- namespace: foo\n  name: a.b.c\n"
	collections, _, err := ParseCollections([]byte(input), "https://default")
	if err != nil {
		t.Fatalf("ParseCollections error: %v", err)
	}
	if len(collections) != 1 {
		t.Fatalf("expected 1 collection, got %d", len(collections))
	}
	if collections[0].Namespace != "foo" || collections[0].Name != "a.b.c" {
		t.Fatalf("unexpected collection[0]: %#v", collections[0])
	}
}
