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
// variants entirely. Otherwise all apiRoot variants are emitted, exactly as
// before memoization existed - so an empty memo produces the identical
// candidate set as today.
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

// apiRootCandidates derives API root candidates from a base URL.
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

	lower := trimmed
	switch {
	case strings.HasSuffix(lower, "/api/v3"):
		add(trimmed)
	case strings.HasSuffix(lower, "/api/v2"):
		add(trimmed)
	case strings.HasSuffix(lower, "/api"):
		add(trimmed + "/v3")
		add(trimmed + "/v2")
		add(trimmed)
	default:
		add(trimmed + "/api/v3")
		add(trimmed + "/api/v2")
		add(trimmed + "/api")
	}

	return out
}
