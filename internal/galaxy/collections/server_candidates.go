package collections

import (
	"fmt"
	"slices"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
)

// serverBaseCandidates returns candidate base URLs for metadata lookup.
func serverBaseCandidates(cfg *config.Config, col collection) []string {
	seen := make(map[string]bool)
	var out []string

	add := func(value string) {
		trimmed := strings.TrimSpace(strings.Trim(value, "\""))
		trimmed = strings.TrimRight(trimmed, "/")
		if trimmed == "" || seen[trimmed] {
			return
		}
		seen[trimmed] = true
		out = append(out, trimmed)
	}

	add(col.Source)
	if cfg != nil {
		add(cfg.Server)
	}

	return out
}

// rootMetaCandidate is a single root-metadata URL candidate together with
// the server base and API root it was derived from. loadRootMetadataCached
// uses the (base, apiRoot) pair to record the winning apiRoot in the memo
// once a candidate's fetch succeeds.
type rootMetaCandidate struct {
	url     string
	base    string
	apiRoot string
}

// rootMetadataURLCandidates builds candidate root metadata URLs.
//
// If memo already knows the winning apiRoot for a server base (a prior
// collection resolved it in this same phase), only that apiRoot's two
// trailing-slash variants are emitted for that base, skipping the losing
// variants entirely. Otherwise every apiRoot variant apiRootCandidates
// derives for that base is emitted, in apiRootCandidates' own priority
// order - so an empty memo always probes /api/v3 first, matching the common
// case's shape, and only reaches the Galaxy NG / Automation Hub shaped /v3
// and /v2 fallbacks (and the legacy bare /api) if that first probe 404s.
func rootMetadataURLCandidates(cfg *config.Config, col collection, memo *apiRootMemo) []rootMetaCandidate {
	seen := make(map[string]bool)
	var out []rootMetaCandidate

	add := func(url, base, apiRoot string) {
		if url == "" || seen[url] {
			return
		}
		seen[url] = true
		out = append(out, rootMetaCandidate{url: url, base: base, apiRoot: apiRoot})
	}

	addWithVariants := func(base, apiRoot string) {
		url := fmt.Sprintf("%s/collections/%s/%s/", apiRoot, col.Namespace, col.Name)
		add(url, base, apiRoot)
		add(strings.TrimRight(url, "/"), base, apiRoot)
	}

	for _, base := range serverBaseCandidates(cfg, col) {
		if winningRoot, ok := memo.winner(base); ok {
			addWithVariants(base, winningRoot)
			continue
		}
		for _, apiRoot := range apiRootCandidates(base) {
			addWithVariants(base, apiRoot)
		}
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
	trimmed := strings.TrimSpace(strings.Trim(base, "\""))
	trimmed = strings.TrimRight(trimmed, "/")
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
