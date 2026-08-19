package collections

import (
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"reflect"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
)

// TestRootMetadataURLCandidatesEmptyMemoMatchesFullSet asserts that with an
// empty (or nil) memo, rootMetadataURLCandidates returns every apiRoot
// variant apiRootCandidates derives for base - in apiRootCandidates' own
// priority order (/api/v3, /v3, /api/v2, /v2, /api) - each with and without
// a trailing slash. This locks down behavior-preservation for the common
// first-probe case: /api/v3 stays first, so the galaxy.ansible.com shape
// never pays for the Galaxy NG / Automation Hub fallback candidates.
func TestRootMetadataURLCandidatesEmptyMemoMatchesFullSet(t *testing.T) {
	t.Parallel()
	col := collection{Namespace: "acme", Name: "widgets"}
	const base = "https://galaxy.example.com"

	want := []rootMetaCandidate{
		{url: base + "/api/v3/collections/acme/widgets/", base: base, apiRoot: base + "/api/v3"},
		{url: base + "/api/v3/collections/acme/widgets", base: base, apiRoot: base + "/api/v3"},
		{url: base + "/v3/collections/acme/widgets/", base: base, apiRoot: base + "/v3"},
		{url: base + "/v3/collections/acme/widgets", base: base, apiRoot: base + "/v3"},
		{url: base + "/api/v2/collections/acme/widgets/", base: base, apiRoot: base + "/api/v2"},
		{url: base + "/api/v2/collections/acme/widgets", base: base, apiRoot: base + "/api/v2"},
		{url: base + "/v2/collections/acme/widgets/", base: base, apiRoot: base + "/v2"},
		{url: base + "/v2/collections/acme/widgets", base: base, apiRoot: base + "/v2"},
		{url: base + "/api/collections/acme/widgets/", base: base, apiRoot: base + "/api"},
		{url: base + "/api/collections/acme/widgets", base: base, apiRoot: base + "/api"},
	}

	for name, memo := range map[string]*apiRootMemo{"empty memo": newAPIRootMemo(), "nil memo": nil} {
		got := rootMetadataURLCandidates(base, col, memo)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: candidate set mismatch\n got: %+v\nwant: %+v", name, got, want)
		}
	}
}

// TestRootMetadataURLCandidatesWithRecordedWinner asserts that once base's
// winning apiRoot is recorded, only that apiRoot's two trailing-slash
// variants are emitted - the losing apiRoot variants are dropped entirely,
// not merely reordered.
func TestRootMetadataURLCandidatesWithRecordedWinner(t *testing.T) {
	t.Parallel()
	col := collection{Namespace: "acme", Name: "widgets"}
	const base = "https://galaxy.example.com"
	const winningRoot = base + "/api/v2"

	memo := newAPIRootMemo()
	memo.recordWinner(base, winningRoot)

	got := rootMetadataURLCandidates(base, col, memo)
	want := []rootMetaCandidate{
		{url: winningRoot + "/collections/acme/widgets/", base: base, apiRoot: winningRoot},
		{url: winningRoot + "/collections/acme/widgets", base: base, apiRoot: winningRoot},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("candidate set mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

// apiRootCandidatesCase is one TestAPIRootCandidates table entry.
type apiRootCandidatesCase struct {
	base string
	want []string
}

// apiRootCandidatesCases is TestAPIRootCandidates' table, pulled out to a
// package-level var purely to keep the test function itself short: every
// case is otherwise independent and reused nowhere else.
//
//nolint:gochecknoglobals // test-only fixture, not runtime-mutable state.
var apiRootCandidatesCases = map[string]apiRootCandidatesCase{
	"bare base": {
		base: "https://galaxy.example.com",
		want: []string{
			"https://galaxy.example.com/api/v3",
			"https://galaxy.example.com/v3",
			"https://galaxy.example.com/api/v2",
			"https://galaxy.example.com/v2",
			"https://galaxy.example.com/api",
		},
	},
	"base ending /api": {
		base: "https://galaxy.example.com/api",
		want: []string{
			"https://galaxy.example.com/api/v3",
			"https://galaxy.example.com/api/v2",
			"https://galaxy.example.com/api",
		},
	},
	"base ending /api/v3": {
		base: "https://galaxy.example.com/api/v3",
		want: []string{"https://galaxy.example.com/api/v3"},
	},
	"base ending /api/v2": {
		base: "https://galaxy.example.com/api/v2",
		want: []string{"https://galaxy.example.com/api/v2"},
	},
	"base ending /v3": {
		base: "https://hub.example.com/api/automation-hub/v3",
		want: []string{"https://hub.example.com/api/automation-hub/v3"},
	},
	"base ending /v2": {
		base: "https://hub.example.com/api/automation-hub/v2",
		want: []string{"https://hub.example.com/api/automation-hub/v2"},
	},
	"quoted value": {
		base: `"https://galaxy.example.com"`,
		want: []string{
			"https://galaxy.example.com/api/v3",
			"https://galaxy.example.com/v3",
			"https://galaxy.example.com/api/v2",
			"https://galaxy.example.com/v2",
			"https://galaxy.example.com/api",
		},
	},
	"trailing-slash value": {
		base: "https://galaxy.example.com/",
		want: []string{
			"https://galaxy.example.com/api/v3",
			"https://galaxy.example.com/v3",
			"https://galaxy.example.com/api/v2",
			"https://galaxy.example.com/v2",
			"https://galaxy.example.com/api",
		},
	},
	"empty string": {
		base: "",
		want: nil,
	},
}

// TestAPIRootCandidates is a table test over apiRootCandidates' own suffix
// handling and normalization, independent of rootMetadataURLCandidates'
// collections/ns/name URL building.
func TestAPIRootCandidates(t *testing.T) {
	t.Parallel()
	for name, tc := range apiRootCandidatesCases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := apiRootCandidates(tc.base)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("apiRootCandidates(%q) = %+v, want %+v", tc.base, got, tc.want)
			}
		})
	}
}

// TestNormalizeServerBase pins normalizeServerBase's declared order of
// operations: whitespace is trimmed before the quotes are inspected (so a
// value quoted inside surrounding whitespace still loses its quotes), exactly
// one surrounding double-quote pair is stripped and only when both ends carry
// one (an unbalanced quote is kept as-is), and trailing slashes are removed
// last.
func TestNormalizeServerBase(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		value string
		want  string
	}{
		"plain":                     {value: "https://hub.example.com", want: "https://hub.example.com"},
		"trailing slash":            {value: "https://hub.example.com/", want: "https://hub.example.com"},
		"trailing slash run":        {value: "https://hub.example.com///", want: "https://hub.example.com"},
		"surrounding whitespace":    {value: "  https://hub.example.com/  ", want: "https://hub.example.com"},
		"quoted":                    {value: "\"https://hub.example.com/\"", want: "https://hub.example.com"},
		"whitespace outside quotes": {value: " \"https://hub.example.com/\" ", want: "https://hub.example.com"},
		"unbalanced leading quote":  {value: "\"https://hub.example.com", want: "\"https://hub.example.com"},
		"unbalanced trailing quote": {value: "https://hub.example.com\"", want: "https://hub.example.com\""},
		"single quote character":    {value: "\"", want: "\""},
		"only one pair stripped":    {value: "\"\"https://hub.example.com\"\"", want: "\"https://hub.example.com\""},
		"empty quoted pair":         {value: "\"\"", want: ""},
		"empty":                     {value: "", want: ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := normalizeServerBase(tc.value); got != tc.want {
				t.Fatalf("normalizeServerBase(%q) = %q, want %q", tc.value, got, tc.want)
			}
		})
	}
}

// TestServerCandidateLabel pins serverCandidate.label's own precedence: a
// non-empty id always wins, else base.
func TestServerCandidateLabel(t *testing.T) {
	t.Parallel()
	if got := (serverCandidate{base: "https://a.example", id: "a"}).label(); got != "a" {
		t.Fatalf("label() = %q, want the id %q", got, "a")
	}
	if got := (serverCandidate{base: "https://a.example"}).label(); got != "https://a.example" {
		t.Fatalf("label() = %q, want the base when id is empty", got)
	}
}

// serverCandidatesCase is one TestServerCandidates table entry.
type serverCandidatesCase struct {
	cfg  *config.Config
	name string
	col  collection
	want []serverCandidate
}

// multiServerTestCfg is the shared two-server config most
// serverCandidatesCases entries pin against.
//
//nolint:gochecknoglobals // test-only fixture, not runtime-mutable state.
var multiServerTestCfg = &config.Config{
	Server: "https://a.example",
	Servers: []config.Server{
		{ID: "a", URL: "https://a.example"},
		{ID: "b", URL: "https://b.example"},
	},
}

// serverCandidatesCases is TestServerCandidates' table, hoisted to package
// level so the test function itself stays within the complexity budget:
// pinned by id, pinned by origin (a source: naming a different path under a
// configured server's origin), a pinned source matching nothing, the
// unpinned multi-server walk (in order, deduplicated by normalized base),
// an empty Servers list falling back to cfg.Server, and cfg == nil.
//
//nolint:gochecknoglobals // test-only fixture, not runtime-mutable state.
var serverCandidatesCases = []serverCandidatesCase{
	{
		name: "pinned by id",
		cfg:  multiServerTestCfg,
		col:  collection{Namespace: "acme", Name: "widgets", Source: "b"},
		want: []serverCandidate{{base: "https://b.example", id: "b"}},
	},
	{
		name: "pinned by origin, different path",
		cfg:  multiServerTestCfg,
		col:  collection{Namespace: "acme", Name: "widgets", Source: "https://b.example/content/published"},
		want: []serverCandidate{{base: "https://b.example/content/published", id: "b"}},
	},
	{
		name: "pinned, matches nothing",
		cfg:  multiServerTestCfg,
		col:  collection{Namespace: "acme", Name: "widgets", Source: "https://other.example"},
		want: []serverCandidate{{base: "https://other.example"}},
	},
	{
		name: "unpinned walks every server in order, deduplicated",
		cfg: &config.Config{
			Servers: []config.Server{
				{ID: "a", URL: "https://a.example/"},
				{ID: "b", URL: "https://b.example"},
				{ID: "a2", URL: "https://a.example"},
			},
		},
		col: collection{Namespace: "acme", Name: "widgets"},
		want: []serverCandidate{
			{base: "https://a.example", id: "a"},
			{base: "https://b.example", id: "b"},
		},
	},
	{
		name: "Servers empty falls back to cfg.Server",
		cfg:  &config.Config{Server: "https://default.example"},
		col:  collection{Namespace: "acme", Name: "widgets"},
		want: []serverCandidate{{base: "https://default.example"}},
	},
	{
		name: "nil config yields nil",
		cfg:  nil,
		col:  collection{Namespace: "acme", Name: "widgets"},
		want: nil,
	},
}

// TestServerCandidates walks serverCandidatesCases, checking one
// representative scenario per rule serverCandidates documents.
func TestServerCandidates(t *testing.T) {
	t.Parallel()
	for _, tc := range serverCandidatesCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := serverCandidates(collectionDeps{cfg: tc.cfg}, tc.col)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("serverCandidates = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestUnmatchedSourceIsWarnedAboutOncePerHost covers the signal a source:
// naming a host no configured server does carries. Such a source is still
// requested - the run continues, mirroring the download path's own
// host-mismatch warning rather than refusing - and is named once per host.
//
// The three rows are the three answers this can have, and each is needed:
// silence on a match by id, silence on a match by origin (the case a
// repo-scoped path under a configured host produces, which must not warn),
// and exactly one warning on a host configured nowhere. Without the two silent
// rows a warning that fired for every source would pass just as well.
func TestUnmatchedSourceIsWarnedAboutOncePerHost(t *testing.T) {
	t.Parallel()

	for _, tc := range unmatchedSourceCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			printer := &capturingPrinter{}
			deps := newCollectionDeps(tc.cfg, infra.New(printer, nil), nil)
			// Called twice with the same source, which is what a lockfile
			// pinning several collections to one host produces: the count
			// below is what pins the deduplication, not just the warning.
			serverCandidates(deps, collection{Namespace: "acme", Name: "widgets", Source: tc.source})
			serverCandidates(deps, collection{Namespace: "acme", Name: "gadgets", Source: tc.source})

			var warned int
			for _, w := range printer.warns {
				if strings.Contains(w, "matches no configured Galaxy server") {
					warned++
				}
			}
			if warned != tc.wantWarnings {
				t.Fatalf("%d warnings, want %d: %v", warned, tc.wantWarnings, printer.warns)
			}
		})
	}
}

// unmatchedSourceCase is one row of TestUnmatchedSourceIsWarnedAboutOncePerHost.
type unmatchedSourceCase struct {
	cfg          *config.Config
	name         string
	source       string
	wantWarnings int
}

// unmatchedSourceCases pairs one configured server with the three kinds of
// source: value that can point at it, or not.
func unmatchedSourceCases() []unmatchedSourceCase {
	configured := &config.Config{
		Servers: []config.Server{{ID: "hub", URL: "https://hub.example/api/"}},
	}
	return []unmatchedSourceCase{
		{name: "matched by id", cfg: configured, source: "hub", wantWarnings: 0},
		{
			name:         "matched by origin under a repo-scoped path",
			cfg:          configured,
			source:       "https://hub.example/api/content/published/",
			wantWarnings: 0,
		},
		{
			name:         "host configured nowhere",
			cfg:          configured,
			source:       "https://attacker.example/api/",
			wantWarnings: 1,
		},
	}
}
