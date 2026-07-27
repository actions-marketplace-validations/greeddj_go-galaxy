package collections

import (
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// serverCandidate is one server a collection's root-metadata fetch may try,
// in the order serverCandidates returns. base is the normalized (no
// trailing slash) URL to fetch against; id is the matching entry's
// server_list id when one was found, or "" for the implicit single server,
// an anonymous ad-hoc base, or a source: value that matched no configured
// server. id exists purely for operator-facing error attribution (label);
// the credential and TLS policy always follow base's actual origin via
// internal/galaxy/fetch's own origin-keyed dispatch, regardless of whether
// id is set.
type serverCandidate struct {
	base string
	id   string
}

// label names the server for an operator-facing error: its configured
// server_list id when it has one, else its base URL - so a run aborting on
// an auth failure or an exhausted retry budget always names something an
// operator can recognize, even for the implicit single server (id "") or an
// anonymous source: pin.
func (s serverCandidate) label() string {
	if s.id != "" {
		return s.id
	}
	return s.base
}

// serverCandidates returns the ordered list of servers col's root-metadata
// fetch may try.
//
// A pinned collection (col.Source non-empty) always yields exactly one
// candidate - a source: value pins its fqdn to one server for the whole
// run, and the list is never consulted for it - resolved by
// pinnedServerCandidate.
//
// An unpinned collection (col.Source empty) yields every cfg.Servers entry
// in list order, deduplicated by normalized base: the "walk the configured
// list, first match wins" behavior this unit exists to deliver.
//
// cfg == nil never happens in production (BuildCollectionConfig always
// returns a non-nil *Config) but is handled defensively, yielding nil.
func serverCandidates(cfg *config.Config, col collection) []serverCandidate {
	if cfg == nil {
		return nil
	}
	if col.Source != "" {
		return []serverCandidate{pinnedServerCandidate(cfg, col.Source)}
	}
	return unpinnedServerCandidates(cfg)
}

// pinnedServerCandidate resolves col.Source (already known non-empty) to a
// single serverCandidate:
//
//  1. An exact, case-sensitive match against a configured server's
//     server_list id wins outright, using that server's own URL as the base.
//  2. Otherwise the value is normalized and compared by network origin
//     (helpers.Origin) against every configured server's URL. A match
//     KEEPS the given (normalized) URL as the base - a source: may
//     legitimately name a repo-scoped path under the same origin, e.g.
//     "<server>/content/published/" - but adopts that server's id for error
//     attribution. The credential and TLS policy still follow automatically,
//     since internal/galaxy/fetch dispatches by origin, not by the exact
//     configured URL: this is the origin-keying design paying off here.
//  3. A source matching neither is an anonymous, unmatched base (id "").
func pinnedServerCandidate(cfg *config.Config, source string) serverCandidate {
	for _, srv := range cfg.Servers {
		if srv.ID != "" && srv.ID == source {
			return serverCandidate{base: srv.URL, id: srv.ID}
		}
	}

	normalized := normalizeServerBase(source)
	if origin, ok := parsedOrigin(normalized); ok {
		for _, srv := range cfg.Servers {
			if srvOrigin, ok := parsedOrigin(normalizeServerBase(srv.URL)); ok && srvOrigin == origin {
				return serverCandidate{base: normalized, id: srv.ID}
			}
		}
	}
	return serverCandidate{base: normalized}
}

// unpinnedServerCandidates returns every cfg.Servers entry as a candidate,
// in list order, deduplicated by normalized base. When cfg.Servers is empty
// - the shape every hand-built *config.Config in this package's tests still
// uses, and the shape any caller that predates multi-server support still
// has - it falls back to the single candidate {base: cfg.Server}, exactly
// the effective server every such caller already had.
func unpinnedServerCandidates(cfg *config.Config) []serverCandidate {
	if len(cfg.Servers) == 0 {
		return []serverCandidate{{base: normalizeServerBase(cfg.Server)}}
	}

	seen := make(map[string]bool, len(cfg.Servers))
	out := make([]serverCandidate, 0, len(cfg.Servers))
	for _, srv := range cfg.Servers {
		base := normalizeServerBase(srv.URL)
		if base == "" || seen[base] {
			continue
		}
		seen[base] = true
		out = append(out, serverCandidate{base: base, id: srv.ID})
	}
	return out
}

// normalizeServerBase trims whitespace, strips one surrounding quote pair
// (an ansible.cfg value quirk), and removes a trailing slash - the same
// normalization apiRootCandidates applies before deriving API root variants.
func normalizeServerBase(value string) string {
	trimmed := strings.TrimSpace(strings.Trim(value, "\""))
	return strings.TrimRight(trimmed, "/")
}

// parsedOrigin parses value as an absolute URL and returns its
// helpers.Origin. It reports ("", false) for anything that fails to parse
// or lacks a scheme/host, which pinnedServerCandidate then never treats as
// matching anything.
func parsedOrigin(value string) (string, bool) {
	u, err := url.Parse(value)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", false
	}
	return helpers.Origin(u), true
}

// rootMetaCandidate is a single root-metadata URL candidate together with
// the server base and API root it was derived from. tryServerRootMetadata
// uses the (base, apiRoot) pair to record the winning apiRoot in the memo
// once a candidate's fetch succeeds.
type rootMetaCandidate struct {
	url     string
	base    string
	apiRoot string
}

// rootMetadataURLCandidates builds candidate root metadata URLs for one
// server base.
//
// If memo already knows the winning apiRoot for base (a prior collection
// resolved it against this same server in this same phase), only that
// apiRoot's two trailing-slash variants are emitted, skipping the losing
// variants entirely. Otherwise every apiRoot variant apiRootCandidates
// derives for base is emitted, in apiRootCandidates' own priority order - so
// an empty memo always probes /api/v3 first, matching the common case's
// shape, and only reaches the Galaxy NG / Automation Hub shaped /v3 and /v2
// fallbacks (and the legacy bare /api) if that first probe 404s.
func rootMetadataURLCandidates(base string, col collection, memo *apiRootMemo) []rootMetaCandidate {
	seen := make(map[string]bool)
	var out []rootMetaCandidate

	add := func(u, apiRoot string) {
		if u == "" || seen[u] {
			return
		}
		seen[u] = true
		out = append(out, rootMetaCandidate{url: u, base: base, apiRoot: apiRoot})
	}

	addWithVariants := func(apiRoot string) {
		u := fmt.Sprintf("%s/collections/%s/%s/", apiRoot, col.Namespace, col.Name)
		add(u, apiRoot)
		add(strings.TrimRight(u, "/"), apiRoot)
	}

	if winningRoot, ok := memo.winner(base); ok {
		addWithVariants(winningRoot)
		return out
	}
	for _, apiRoot := range apiRootCandidates(base) {
		addWithVariants(apiRoot)
	}

	return out
}

// joinCandidateURLs renders a candidate list's URLs as a comma-joined string
// for debug logging, without allocating an intermediate []string.
func joinCandidateURLs(candidates []rootMetaCandidate) string {
	var b strings.Builder
	for i, cand := range candidates {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(cand.url)
	}
	return b.String()
}

// apiRootCandidates derives API root candidates from a base URL, in priority
// order: /api/v3, /v3, /api/v2, /v2, /api. The bare /v3 and /v2 candidates
// exist for a Galaxy NG / Automation Hub shaped deployment, which mounts the
// v3 API directly under its own base path (e.g.
// "https://console.redhat.com/api/automation-hub" serves v3 collections at
// "<base>/v3/collections/...", not "<base>/api/v3/collections/..."). /api/v3
// stays first since it is galaxy.ansible.com's shape and therefore the
// overwhelmingly common case: the fallback candidates only cost anything
// when the first probe 404s.
func apiRootCandidates(base string) []string {
	trimmed := normalizeServerBase(base)
	if trimmed == "" {
		return nil
	}

	var out []string
	add := func(value string) {
		value = strings.TrimRight(value, "/")
		if slices.Contains(out, value) {
			return
		}
		out = append(out, value)
	}

	// A base already ending in one of these suffixes names its own API root
	// unambiguously, so it is used as-is rather than appended to - appending
	// would otherwise double up the suffix (e.g. ".../api/v3/api/v3").
	switch {
	case strings.HasSuffix(trimmed, "/api/v3"):
		add(trimmed)
	case strings.HasSuffix(trimmed, "/api/v2"):
		add(trimmed)
	case strings.HasSuffix(trimmed, "/v3"):
		add(trimmed)
	case strings.HasSuffix(trimmed, "/v2"):
		add(trimmed)
	case strings.HasSuffix(trimmed, "/api"):
		add(trimmed + "/v3")
		add(trimmed + "/v2")
		add(trimmed)
	default:
		add(trimmed + "/api/v3")
		add(trimmed + "/v3")
		add(trimmed + "/api/v2")
		add(trimmed + "/v2")
		add(trimmed + "/api")
	}

	return out
}
