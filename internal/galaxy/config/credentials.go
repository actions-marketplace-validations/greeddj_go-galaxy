package config

import (
	"fmt"
	"slices"
	"strings"
)

// This file holds the grammar the GO_GALAXY_GIT_* and GO_GALAXY_URL_*
// credential surfaces share: how a declared id list is judged and how
// variables under a declared id's prefix are scanned for unknown keys. The
// two surfaces are parameterized over the same helpers rather than written
// twice so their rules cannot drift; everything specific to one surface -
// which keys exist, which kinds a binding may take - stays in its own file.

// credentialIDList splits a comma-separated id list and applies the same two
// list-level rules validateServerIDs applies to server ids, for the same
// reason: each id becomes an environment variable prefix, so it must be
// spellable as one and must not collide with another by case alone. A blank
// list is empty rather than invalid; a blank element inside a non-blank list
// is invalid, since it is what a stray comma produces. Every refusal wraps
// sentinel and names listVar; envPrefix appears only in the case-fold
// message, which names the folded variable prefix two ids would share.
func credentialIDList(raw, listVar, envPrefix string, sentinel error) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	ids := make([]string, 0, len(parts))
	seen := make(map[string]string, len(parts))
	for _, part := range parts {
		id := strings.TrimSpace(part)
		if id == "" {
			return nil, fmt.Errorf("%w: %s carries an empty id", sentinel, listVar)
		}
		if !serverIDPattern.MatchString(id) {
			return nil, fmt.Errorf("%w: %s lists id %q, which is not spellable as an environment variable (allowed: A-Z a-z 0-9 _ -)",
				sentinel, listVar, id)
		}
		lower := strings.ToLower(id)
		if prior, ok := seen[lower]; ok {
			return nil, fmt.Errorf("%w: %s lists %q and %q, which read the same %s%s_* variables",
				sentinel, listVar, prior, id, envPrefix, strings.ToUpper(id))
		}
		seen[lower] = id
		ids = append(ids, id)
	}
	return ids, nil
}

// unknownCredentialVariableWarnings returns one warning per environment
// variable that sits under a declared id's prefix and is none of the
// surface's keys. The known names of every id are collected first, because
// one id's prefix can be a prefix of another's (ids "a" and "a_b" share one
// prefix), and a variable that is a known key of the longer id must not be
// reported as an unknown key of the shorter one. The result is sorted so the
// warning order does not depend on the environment's own.
func unknownCredentialVariableWarnings(ids, environ, keys []string, varFor func(id, key string) string) []string {
	known := make(map[string]bool, len(ids)*len(keys))
	prefixes := make([]string, 0, len(ids))
	for _, id := range ids {
		prefixes = append(prefixes, varFor(id, ""))
		for _, key := range keys {
			known[varFor(id, key)] = true
		}
	}
	var warnings []string
	for _, entry := range environ {
		name, _, _ := strings.Cut(entry, "=")
		if known[name] {
			continue
		}
		for _, prefix := range prefixes {
			if strings.HasPrefix(name, prefix) {
				warnings = append(warnings, fmt.Sprintf("unsupported variable %s ignored", name))
				break
			}
		}
	}
	slices.Sort(warnings)
	return warnings
}
