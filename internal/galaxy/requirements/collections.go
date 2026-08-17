// Package requirements parses an ansible-style requirements.yml into the
// collection entries the resolver works from. It accepts both shapes ansible
// writes - a bare list, or a mapping carrying a collections: key - with each
// item either a "namespace.name" string or a mapping, reports separately
// whether the file declared roles (which this tool does not install), and
// refuses any other shape rather than guessing at it.
//
// This is one of the boundaries an untrusted identifier enters the program
// through, so an entry is validated here rather than downstream:
// parseCollectionItem is where both item shapes converge on the collection
// name alphabet, and validateRequirement rejects a non-Galaxy type: and a
// source: embedding URL userinfo before anything can print or request it.
package requirements

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/signature"
	"go.yaml.in/yaml/v3"
)

// Collections is a list of collection requirements.
type Collections = []CollectionRequirement

// CollectionRequirement describes a single collection requirement entry.
type CollectionRequirement struct {
	Namespace  string
	Name       string
	Version    string
	Source     string
	Type       string
	Signatures []string
}

// LoadCollections reads and parses requirements from a file.
func LoadCollections(path, defaultSource string) (Collections, bool, error) {
	//nolint:gosec // path is user-provided requirements file.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false, err
	}
	return ParseCollections(data, defaultSource)
}

// ParseCollections parses requirements data and returns collections and roles flag.
func ParseCollections(data []byte, defaultSource string) (Collections, bool, error) {
	var raw any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, false, err
	}
	return parseCollectionsRaw(raw, defaultSource)
}

// parseCollectionsRaw parses a decoded requirements payload.
func parseCollectionsRaw(raw any, defaultSource string) (Collections, bool, error) {
	switch v := raw.(type) {
	case map[string]any:
		rolesFound := false
		if _, ok := v["roles"]; ok {
			rolesFound = true
		}
		if collectionsRaw, ok := v["collections"]; ok {
			cols, err := parseCollectionList(collectionsRaw, defaultSource)
			return cols, rolesFound, err
		}
		if rolesFound {
			return nil, rolesFound, nil
		}
		return nil, rolesFound, helpers.ErrUnsupportedRequirementsFormat
	case []any:
		cols, err := parseCollectionList(v, defaultSource)
		return cols, false, err
	default:
		return nil, false, helpers.ErrUnsupportedRequirementsFormat
	}
}

// parseCollectionList parses a list of collection items. A nil raw value
// (ansible accepts a bare "collections:" or "collections: ~" as an empty
// list) yields an empty result rather than an error; any other non-list
// value (e.g. a scalar) is still rejected.
func parseCollectionList(raw any, defaultSource string) (Collections, error) {
	if raw == nil {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, helpers.ErrInvalidCollectionsList
	}
	items := make(Collections, 0, len(list))
	for _, item := range list {
		req, err := parseCollectionItem(item, defaultSource)
		if err != nil {
			return nil, err
		}
		items = append(items, req)
	}
	return items, nil
}

// parseCollectionItem parses a single collection entry and checks the
// resulting identity against the collection-name alphabet.
//
// The check lives here rather than inside helpers.SplitFQDN because the
// explicit form - a mapping with its own `namespace:` and `name:` keys - never
// reaches SplitFQDN at all: normalizeCollectionName calls it only when the
// name still carries a dot. So a requirements file could name a collection its
// own lockfile could not, and an explicit namespace carrying a newline reached
// the resolver, which printed it, before anything looked at it. Both parse
// branches converge here, which is what makes this the boundary rather than
// one of the two paths through it.
func parseCollectionItem(item any, defaultSource string) (CollectionRequirement, error) {
	req, err := parseCollectionItemByShape(item, defaultSource)
	if err != nil {
		return CollectionRequirement{}, err
	}
	if !helpers.IsCollectionNamePart(req.Namespace) || !helpers.IsCollectionNamePart(req.Name) {
		return CollectionRequirement{}, fmt.Errorf("%w: %q.%q must each match ^[a-z][a-z0-9_]*$",
			helpers.ErrInvalidCollectionName, req.Namespace, req.Name)
	}
	return req, nil
}

// parseCollectionItemByShape dispatches on the entry's YAML shape; it is the
// former body of parseCollectionItem, split out so the alphabet check above
// covers both branches without either having to remember it.
func parseCollectionItemByShape(item any, defaultSource string) (CollectionRequirement, error) {
	switch v := item.(type) {
	case string:
		return parseCollectionStringItem(v, defaultSource)
	case map[string]any:
		return parseCollectionMapItem(v, defaultSource)
	default:
		return CollectionRequirement{}, fmt.Errorf("%w: %v", helpers.ErrUnsupportedCollectionFormat, item)
	}
}

func parseCollectionStringItem(value string, defaultSource string) (CollectionRequirement, error) {
	name := strings.TrimSpace(value)
	if name == "" {
		return CollectionRequirement{}, helpers.ErrEmptyCollectionName
	}
	if looksLikeSourceName(name) {
		return CollectionRequirement{}, fmt.Errorf("%w %q (only Galaxy API sources are supported)", helpers.ErrUnsupportedCollectionSource, name)
	}
	namespace, collection, ok := helpers.SplitFQDN(name)
	if !ok {
		return CollectionRequirement{}, fmt.Errorf("%w: %q", helpers.ErrInvalidCollectionName, name)
	}
	return CollectionRequirement{
		Namespace: namespace,
		Name:      collection,
		Version:   "*",
		Source:    defaultSource,
	}, nil
}

func parseCollectionMapItem(value map[string]any, defaultSource string) (CollectionRequirement, error) {
	req := parseCollectionMapFields(value)
	if err := checkNamespaceNameConflict(req); err != nil {
		return CollectionRequirement{}, err
	}
	req = normalizeCollectionName(req)
	return finalizeCollectionRequirement(req, defaultSource, value)
}

func parseCollectionMapFields(value map[string]any) CollectionRequirement {
	req := CollectionRequirement{}
	if raw, ok := value["namespace"].(string); ok {
		req.Namespace = strings.TrimSpace(raw)
	}
	if raw, ok := value["name"]; ok {
		req.Name = strings.TrimSpace(fmt.Sprint(raw))
	}
	if raw, ok := value["source"].(string); ok {
		req.Source = strings.TrimSpace(raw)
	}
	if raw, ok := value["type"].(string); ok {
		req.Type = strings.ToLower(strings.TrimSpace(raw))
	}
	if raw, ok := value["signatures"]; ok {
		req.Signatures = parseStringList(raw)
	}
	if raw, ok := value["version"]; ok {
		req.Version = strings.TrimSpace(fmt.Sprint(raw))
	}
	return req
}

// checkNamespaceNameConflict rejects an explicit namespace combined with a
// dotted name, e.g. namespace: foo + name: bar.baz. normalizeCollectionName
// would otherwise keep the explicit namespace but silently overwrite name
// with the dotted name's last segment, installing a different collection
// than either field implies alone. The conditions mirror exactly those
// under which normalizeCollectionName would perform that split: only a
// namespace that is actually about to be shadowed is flagged, not every
// dotted name.
func checkNamespaceNameConflict(req CollectionRequirement) error {
	if req.Namespace == "" || req.Name == "" || !strings.Contains(req.Name, ".") ||
		req.Type != "" || looksLikeSourceName(req.Name) {
		return nil
	}
	if _, _, ok := helpers.SplitFQDN(req.Name); !ok {
		return nil
	}
	return fmt.Errorf("%w: namespace %q with dotted name %q", helpers.ErrConflictingNamespaceName, req.Namespace, req.Name)
}

func normalizeCollectionName(req CollectionRequirement) CollectionRequirement {
	if req.Name == "" || !strings.Contains(req.Name, ".") || req.Type != "" || looksLikeSourceName(req.Name) {
		return req
	}
	namespace, collection, ok := helpers.SplitFQDN(req.Name)
	if !ok {
		return req
	}
	if req.Namespace == "" {
		req.Namespace = namespace
	}
	req.Name = collection
	return req
}

func finalizeCollectionRequirement(req CollectionRequirement, defaultSource string, raw any) (CollectionRequirement, error) {
	if err := validateRequirement(req, raw); err != nil {
		return CollectionRequirement{}, err
	}
	req = applyRequirementDefaults(req, defaultSource)
	req, err := normalizeRequirementNamespace(req)
	if err != nil {
		return CollectionRequirement{}, err
	}
	return req, nil
}

func validateRequirement(req CollectionRequirement, raw any) error {
	// Checked first, ahead of every other branch below (including the
	// req.Name == "" one immediately following): that branch echoes the
	// entire raw item back in its error message for diagnostic purposes, and
	// raw may itself carry the very userinfo-bearing source: this check
	// exists to catch. Validating source before ever touching raw means an
	// invalid entry ("name" missing or empty) can never smuggle a credential
	// out through its own error message.
	if err := checkSourceUserinfo(req); err != nil {
		return err
	}
	// Second, and ahead of the raw-echoing branch below for the same reason
	// checkSourceUserinfo is first: a signatures: entry can carry a credential
	// of its own, and this check refuses one without printing it.
	if err := checkSignatureSources(req, raw); err != nil {
		return err
	}
	if req.Name == "" {
		return fmt.Errorf("%w: %v", helpers.ErrInvalidCollectionEntry, raw)
	}
	if req.Type == "git" || req.Type == "url" {
		return fmt.Errorf("%w %q (only galaxy is supported)", helpers.ErrUnsupportedCollectionType, req.Type)
	}
	if req.Type != "" && req.Type != "galaxy" {
		return fmt.Errorf("%w %q (only galaxy is supported)", helpers.ErrUnsupportedCollectionType, req.Type)
	}
	if req.Type == "" && looksLikeSourceName(req.Name) {
		return fmt.Errorf("%w %q (only Galaxy API sources are supported)", helpers.ErrUnsupportedCollectionSource, req.Name)
	}
	return nil
}

// checkSignatureSources validates the one repository-authored field of a
// requirements entry that no other boundary judges: signatures:.
//
// Everything it refuses would otherwise be refused, or silently mangled, much
// later and much worse. A source this tool cannot fetch reaches an install
// worker and fails one collection among however many, classified as that
// collection's failure rather than as the configuration error it is. A source
// carrying userinfo is a credential in repository content, and by then it has
// been copied into the resolved snapshot - a shared S3 object in a
// multi-runner cache - where url.URL.String() renders it back in plain text;
// the refusal here names the value with its userinfo cut off, exactly as the
// fetch's own would. A source's query reaches that same snapshot too, but is
// cut rather than refused (normalizeSignatures,
// internal/galaxy/collections/resolve.go). A value that is not a string at
// all is turned by parseStringList's fmt.Sprint arm into a plausible-looking
// source ("map[]", "false", "0") that nothing downstream can tell from one an
// author wrote. And more sources than helpers.MaxSignaturesPerCollection
// allows is a list this tool would take only the first 64 of - warned about
// downstream by gatherLimit, but still not what the file asked for, which
// is a worse answer than refusing the file outright.
//
// helpers.MaxSignaturesPerCollection is one number enforced at two layers,
// and the two must be read together: this gate bounds what one entry may
// DECLARE, never what the gather does once the combined candidate set -
// this entry's own sources plus whatever the server offers alongside the
// artifact - exceeds that bound. gatherLimit
// (internal/galaxy/collections/verify.go) owns that second layer and
// reports it; per the one-home rule, this gate's job ends at load time.
//
// What it does NOT do is re-state the grammar: signature.ValidateRequirementSource
// answers what a source may be, and the fetch answers through the same
// function, so a value accepted here cannot be refused there or the reverse.
//
// The shape check reads raw rather than req.Signatures, because by the time a
// requirement carries []string the evidence is gone: parseStringList has
// already turned whatever was written into strings. Both bearing shapes are
// accepted - a list of strings, and a single string, which is what
// parseStringList itself accepts - and the refusal names a Go type rather than
// a value, since a YAML mapping's contents are repository content this message
// has no reason to render.
func checkSignatureSources(req CollectionRequirement, raw any) error {
	if err := checkSignatureSourceShape(raw); err != nil {
		return err
	}
	if len(req.Signatures) > helpers.MaxSignaturesPerCollection {
		return fmt.Errorf("%w: %d declared, at most %d are gathered",
			helpers.ErrTooManySignatureSources, len(req.Signatures), helpers.MaxSignaturesPerCollection)
	}
	for _, source := range req.Signatures {
		if err := signature.ValidateRequirementSource(source); err != nil {
			return err
		}
	}

	return nil
}

// checkSignatureSourceShape refuses a signatures: value that is neither a list
// of strings nor a single string, naming the offending Go type and never its
// content. An absent key, and a key whose value is nil, are both left alone:
// declaring nothing is how an entry asks for nothing.
func checkSignatureSourceShape(raw any) error {
	item, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	value, ok := item["signatures"]
	if !ok || value == nil {
		return nil
	}
	switch v := value.(type) {
	case string:
		return nil
	case []any:
		for _, entry := range v {
			if _, ok := entry.(string); !ok {
				return fmt.Errorf("%w: signatures: carries a %T where a source string was expected",
					helpers.ErrUnsupportedSignatureSource, entry)
			}
		}

		return nil
	default:
		return fmt.Errorf("%w: signatures: is a %T, not a list of source strings",
			helpers.ErrUnsupportedSignatureSource, value)
	}
}

// checkSourceUserinfo rejects an explicit "source:" that embeds userinfo
// (e.g. "https://user:pass@hub/"), the same shape config.Server's own URL
// already refuses (see helpers.ErrGalaxyServerURLUserinfo). Unlike a
// configured server, a per-collection source: comes straight from
// requirements.yml - repository content, not an operator-controlled
// config file - and an unmatched source: flows unchanged into root-metadata
// request URLs, debug log lines, HTTP error strings, the resolved snapshot,
// the lockfile, and GALAXY.yml (see serverCandidates/pinnedServerCandidate
// in package collections). url.URL.String() renders a userinfo password
// back out in plain text, so without this check any of those repository-
// content-controlled sinks could leak a credential embedded in the source.
//
// A source: naming a bare server_list id (e.g. "internal", never URL-shaped)
// is left alone: url.Parse succeeds on it but yields no scheme/host, so it
// never reaches the User-set branch below.
func checkSourceUserinfo(req CollectionRequirement) error {
	if req.Source == "" {
		return nil
	}
	parsed, err := url.Parse(req.Source)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil
	}
	if parsed.User != nil {
		return fmt.Errorf("%w: collection %q", helpers.ErrGalaxyServerURLUserinfo, req.Name)
	}
	return nil
}

func applyRequirementDefaults(req CollectionRequirement, defaultSource string) CollectionRequirement {
	if req.Version == "" {
		req.Version = "*"
	}
	if req.Source == "" && (req.Type == "galaxy" || (req.Type == "" && !looksLikeSourceName(req.Name))) {
		req.Source = defaultSource
	}
	return req
}

func normalizeRequirementNamespace(req CollectionRequirement) (CollectionRequirement, error) {
	if req.Namespace != "" || req.Type != "" || looksLikeSourceName(req.Name) {
		return req, nil
	}
	namespace, collection, ok := helpers.SplitFQDN(req.Name)
	if !ok {
		return CollectionRequirement{}, fmt.Errorf("%w: %q", helpers.ErrInvalidCollectionName, req.Name)
	}
	req.Namespace = namespace
	req.Name = collection
	return req, nil
}

// parseStringList converts an arbitrary value to a string slice.
func parseStringList(value any) []string {
	switch v := value.(type) {
	case nil:
		return nil
	case string:
		item := strings.TrimSpace(v)
		if item == "" {
			return nil
		}
		return []string{item}
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			str := strings.TrimSpace(fmt.Sprint(item))
			if str == "" {
				continue
			}
			out = append(out, str)
		}
		return out
	default:
		str := strings.TrimSpace(fmt.Sprint(v))
		if str == "" {
			return nil
		}
		return []string{str}
	}
}

// looksLikeSourceName reports whether the value looks like a URL or path.
func looksLikeSourceName(value string) bool {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return false
	}
	lower := strings.ToLower(trimmed)
	switch {
	case strings.Contains(lower, "://"):
		return true
	case strings.HasPrefix(lower, "git+"):
		return true
	case strings.HasPrefix(lower, "git@"):
		return true
	case strings.HasPrefix(lower, "./"),
		strings.HasPrefix(lower, "../"),
		strings.HasPrefix(lower, "/"),
		strings.HasPrefix(lower, "~"):
		return true
	}
	return false
}
