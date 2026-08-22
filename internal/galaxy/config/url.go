package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/urlsource"
)

// URLCredential is one origin-bound url-source credential as this package
// holds it: the id it was declared under, the binding prefix (a
// urlsource.ParsePrefix result), and the Secret-wrapped Bearer token. The
// plaintext leaves this form in one place only, the command wiring that
// builds a fetch.URLBinding from it. A url credential deliberately has one
// kind: the token is sent as "Authorization: Bearer <token>", which every
// artifact host this surface exists for accepts, and a second kind can be
// added compatibly while an absent one cannot be removed.
type URLCredential struct {
	ID    string
	Token Secret
	URL   urlsource.Prefix
}

const (
	// urlCredentialsListEnv lists the declared credential ids; nothing else
	// under the prefix is read for an id that is not listed here.
	urlCredentialsListEnv = "GO_GALAXY_URL_CREDENTIALS" //nolint:gosec // an environment variable name, not a credential
	// urlCredentialEnvPrefix is the common prefix of every per-id variable,
	// GO_GALAXY_URL_<ID>_<KEY>, with <ID> upper-cased exactly as a git
	// credential id is - which is why two ids differing only in case are
	// refused: they fold onto one variable prefix.
	urlCredentialEnvPrefix = "GO_GALAXY_URL_" //nolint:gosec // an environment variable name prefix, not a credential

	urlKeyURL   = "URL"
	urlKeyToken = "TOKEN"
)

// urlCredentialKeys is every key this tool reads for a declared id; the
// unknown-variable scan treats any other name under the id's prefix as
// unsupported.
func urlCredentialKeys() []string {
	return []string{urlKeyURL, urlKeyToken}
}

// loadURLCredentials fills cfg.URLCredentials from the environment. The
// surface is GO_GALAXY_URL_CREDENTIALS, a comma-separated list of ids, and
// for each id the GO_GALAXY_URL_<ID>_{URL,TOKEN} variables. An unset or
// empty list means no credentials and is not an error. A variable whose
// value is empty counts as unset, so an exported-but-blank secret never
// produces a half-configured binding.
//
// Checks run in a fixed order so the first failure a broken configuration
// reports is stable: the id list's grammar, then per id in list order the
// URL, then the token, then duplicates across ids. Every refusal is
// helpers.ErrURLCredentialInvalid and names a VARIABLE, never a value: the
// value of any of these variables is, or sits beside, a credential. The one
// exception in sentinel is a token bound to plaintext http on a non-loopback
// host, which is helpers.ErrInsecureTokenTransport for the same reason
// checkTokenTransport refuses a Galaxy token there: nothing about an
// artifact host makes a Bearer token on the wire safer than a Galaxy one.
//
// A GO_GALAXY_URL_<ID>_* variable for a declared id that is not one of the
// two keys is queued on cfg.Warnings and ignored, the way an unknown
// GO_GALAXY_GIT_<ID>_* variable is.
func loadURLCredentials(cfg *Config) error {
	ids, err := credentialIDList(os.Getenv(urlCredentialsListEnv),
		urlCredentialsListEnv, urlCredentialEnvPrefix, helpers.ErrURLCredentialInvalid)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}

	creds := make([]URLCredential, 0, len(ids))
	for _, id := range ids {
		cred, err := buildURLCredential(id)
		if err != nil {
			return err
		}
		creds = append(creds, cred)
	}
	if err := checkURLCredentialURLConflicts(creds); err != nil {
		return err
	}
	cfg.Warnings = append(cfg.Warnings,
		unknownCredentialVariableWarnings(ids, os.Environ(), urlCredentialKeys(), urlCredentialVar)...)
	cfg.URLCredentials = creds
	return nil
}

// urlCredentialVar composes the variable name for one key of one id.
func urlCredentialVar(id, key string) string {
	return urlCredentialEnvPrefix + strings.ToUpper(id) + "_" + key
}

// buildURLCredential settles one id: its URL first, then the token. The URL
// is trimmed so a trailing newline from a CI secret store does not change
// the binding; the token is taken verbatim, since a token may legitimately
// end in whitespace.
func buildURLCredential(id string) (URLCredential, error) {
	urlVar := urlCredentialVar(id, urlKeyURL)
	rawURL := strings.TrimSpace(os.Getenv(urlVar))
	if rawURL == "" {
		return URLCredential{}, fmt.Errorf("%w: %s is not set", helpers.ErrURLCredentialInvalid, urlVar)
	}
	parsed, err := urlsource.ParsePrefix(rawURL)
	if err != nil {
		// ParsePrefix's own error already wraps the sentinel; this adds the
		// variable the value came from without echoing the value.
		return URLCredential{}, fmt.Errorf("%s: %w", urlVar, err)
	}
	token := os.Getenv(urlCredentialVar(id, urlKeyToken))
	if token == "" {
		return URLCredential{}, fmt.Errorf("%w: %s is not set", helpers.ErrURLCredentialInvalid, urlCredentialVar(id, urlKeyToken))
	}
	if parsed.Scheme == "http" && !parsed.IsLoopback() {
		return URLCredential{}, fmt.Errorf("%w: url credential %q (%s)", helpers.ErrInsecureTokenTransport, id, parsed.Origin())
	}
	return URLCredential{ID: id, URL: parsed, Token: NewSecret(token)}, nil
}

// checkURLCredentialURLConflicts refuses two ids bound to one canonical
// prefix: the transport's longest-path-prefix match relies on this check to
// make a tie impossible, so a second binding to the same prefix would
// otherwise be silently ignored or silently preferred by list order.
func checkURLCredentialURLConflicts(creds []URLCredential) error {
	seen := make(map[string]string, len(creds))
	for _, cred := range creds {
		key := cred.URL.String()
		if prior, ok := seen[key]; ok {
			return fmt.Errorf("%w: %s and %s bind the same URL", helpers.ErrURLCredentialInvalid,
				urlCredentialVar(prior, urlKeyURL), urlCredentialVar(cred.ID, urlKeyURL))
		}
		seen[key] = cred.ID
	}
	return nil
}
