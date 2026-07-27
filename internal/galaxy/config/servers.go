package config

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/urfave/cli/v3"
)

// Secret wraps a Galaxy API token so that leaking it by accident is
// structurally hard rather than merely a matter of remembering to redact
// it: fmt (%v %s %q %x %X %+v %#v), encoding/json, and yaml.v3 all render a
// Secret as a fixed redacted placeholder, and the plaintext is reachable
// through exactly one method, Reveal, whose name is deliberately loud and
// rare so every call site is easy to find by grep.
//
// No token, and no value derived from a token - not even a hash - may ever
// be persisted: not to the Bolt snapshot, the S3 snapshot, the project
// registry, the lockfile, the metrics file, or GALAXY.yml. Those are all
// state that outlives a single run and can be read by a principal who does
// not need the token itself to install anything. A Secret's redacted
// renderings are safe to write to any of them; Reveal's return value is
// not.
//
// The zero Secret is a valid, usable value that reports "unset" everywhere
// (IsSet, String, GoString, MarshalJSON, MarshalYAML), matching an
// unconfigured Server.Token.
type Secret struct {
	value string
}

// NewSecret wraps value in a Secret. It is the only way to construct a
// non-zero Secret from outside this package, since the field is
// unexported.
func NewSecret(value string) Secret {
	return Secret{value: value}
}

// IsSet reports whether the secret carries a non-empty token.
func (s Secret) IsSet() bool {
	return s.value != ""
}

// Reveal returns the plaintext token. Every call site must be the thing
// that legitimately needs the plaintext (e.g. setting an Authorization
// header) - never a debug print, a log line, or anything that ends up in
// persisted state; use the redacted String/MarshalJSON/MarshalYAML forms
// for all of those instead.
func (s Secret) Reveal() string {
	return s.value
}

// String implements fmt.Stringer. fmt consults it for the %v, %s, %q, %x,
// and %X verbs (and %+v, since the "+" flag does not change which of those
// rules applies), so a Secret embedded anywhere in a value passed to
// fmt.Print/Sprintf/etc. never renders its plaintext.
func (s Secret) String() string {
	return s.redacted()
}

// GoString implements fmt.GoStringer, covering the one verb String does
// not: %#v. Without it, %#v would fall back to reflecting into the
// unexported value field regardless of exported-ness.
func (s Secret) GoString() string {
	return s.redacted()
}

// MarshalJSON implements json.Marshaler so a Secret embedded in any
// JSON-serialized structure renders the redacted placeholder instead of
// the plaintext.
func (s Secret) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.redacted())
}

// MarshalYAML implements yaml.Marshaler (gopkg.in/yaml.v3, already a
// project dependency) for the same reason as MarshalJSON, covering
// GALAXY.yml-shaped output.
func (s Secret) MarshalYAML() (any, error) {
	return s.redacted(), nil
}

// sameAs reports whether two secrets carry the same token. It exists so
// that comparing secrets - which config validation must do, to reject two
// servers sharing an origin with different credentials - never has to go
// through Reveal: Reveal's value is that it is rare and greppable, and a
// comparison is not a legitimate reason to hand out the plaintext. Being
// unexported, it also cannot widen the plaintext's reach outside this
// package.
func (s Secret) sameAs(other Secret) bool {
	return s.value == other.value
}

// redacted renders the single placeholder every redaction path shares,
// distinguishing "no token configured" from "token configured but hidden"
// without ever branching on the plaintext's actual content.
func (s Secret) redacted() string {
	if s.value == "" {
		return "[unset]"
	}
	return "[REDACTED]"
}

// Server is one configured Galaxy server: its identity, its normalized
// endpoint, and the credential/TLS policy this tool applies to it. ID is ""
// for the implicit single server (no server_list configured), and the
// non-empty id exactly as written in server_list otherwise.
type Server struct {
	ID                    string
	URL                   string
	Token                 Secret
	InsecureSkipTLSVerify bool
}

// galaxyServerHardErrorKeys are the [galaxy_server.<id>] keys ansible
// supports for Basic auth (username, password) and Keycloak/SSO token
// exchange (auth_url, client_id), neither of which this tool implements.
// Any of these present in a section fails config loading outright, before
// any request is made, instead of silently sending an unauthenticated
// request and surfacing a confusing 401 later.
//
//nolint:gochecknoglobals // a fixed, immutable lookup table, not mutable shared state
var galaxyServerHardErrorKeys = map[string]bool{
	"username":  true,
	"password":  true,
	"auth_url":  true,
	"client_id": true,
}

// galaxyServerKnownKeys are every [galaxy_server.<id>] key this resolver
// recognizes at all, whether accepted (url, token, validate_certs),
// accepted as a no-op (api_version), or hard-errored
// (galaxyServerHardErrorKeys). Any section key outside this set only warns:
// an operator's ansible.cfg may carry a per-server key ansible itself
// understands that this tool does not (yet) need to act on.
//
//nolint:gochecknoglobals // a fixed, immutable lookup table, not mutable shared state
var galaxyServerKnownKeys = map[string]bool{
	"url":            true,
	"token":          true,
	"validate_certs": true,
	"api_version":    true,
	"username":       true,
	"password":       true,
	"auth_url":       true,
	"client_id":      true,
}

// serverIDPattern is the charset ansible does not itself validate but this
// tool does: a "." would make the "[galaxy_server.<id>]" section grammar
// ambiguous, and every other character is unsafe to fold into an
// ANSIBLE_GALAXY_SERVER_<ID>_* environment variable name.
var serverIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// resolveServers computes cfg.Servers - and, from its head, cfg.Server -
// from the CLI flags, ansible.cfg, and the ANSIBLE_-prefixed env surface
// ansible itself defines for multi-server configuration. It must run after
// applyAnsibleConfig, since it relies on cfg.Server already holding the
// single-server precedence result ([galaxy] server, then the --server
// flag default) for the case where no server_list is configured at all.
//
// Precedence, in order:
//  1. An explicit --server (c.IsSet("server"), covering the flag and both
//     its env names) collapses everything to one server: if its value
//     matches a server_list id exactly (case-sensitive), that server is
//     used with its own token and TLS setting; otherwise the value is used
//     verbatim as an anonymous single-server URL, and server_list plays no
//     further part - not even to validate it.
//  2. Otherwise a non-empty server_list wins, in list order.
//  3. Otherwise [galaxy] server from ansible.cfg (already folded into
//     cfg.Server by pickConfigValue).
//  4. Otherwise the --server flag default (likewise already in cfg.Server).
//
// cfg.Servers is never empty on success: cfg.Server always ends up equal to
// cfg.Servers[0].URL.
func resolveServers(cfg *Config, c *cli.Command, ansCfg ansibleConfig) error {
	ids := resolveServerList(ansCfg)

	servers, err := resolveServerCandidates(cfg, c, ids, ansCfg.GalaxyServers)
	if err != nil {
		return err
	}
	if err := applyTokenFlag(c, servers); err != nil {
		return err
	}

	cfg.Servers = servers
	cfg.Server = servers[0].URL
	cfg.Warnings = append(cfg.Warnings, tlsWarnings(servers)...)
	return nil
}

// applyTokenFlag folds an explicitly set --token (or GO_GALAXY_TOKEN) into
// the resolved server list, in place.
//
// It is go-galaxy's own convenience for the common single-server case -
// point the tool at one private hub and hand it a credential - without
// making the operator write an ansible.cfg section for it. It is deliberately
// the only go-galaxy-spelled name in this whole surface: everything else is
// ANSIBLE_-prefixed, because everything else exists for drop-in fidelity.
//
// It is allowed only when exactly one server is effective (the built-in
// default, [galaxy] server, --server as a URL, or --server naming a
// server_list id). With a multi-entry server_list in effect there is no
// answer to "which server is this credential for", and silently picking one
// could send a private hub's token to the public Galaxy, so it is a hard
// error instead.
//
// Being explicitly set to the empty string clears the token, so a pipeline
// can force an anonymous run by exporting GO_GALAXY_TOKEN= without editing
// any config. An unset flag - including a command that never registers it -
// leaves the list untouched.
func applyTokenFlag(c *cli.Command, servers []Server) error {
	if !c.IsSet("token") {
		return nil
	}
	if len(servers) > 1 {
		return fmt.Errorf("%w: %d servers configured", helpers.ErrAmbiguousGalaxyToken, len(servers))
	}
	if len(servers) == 0 {
		return nil
	}

	token := NewSecret(strings.TrimSpace(c.String("token")))
	_, parsed, err := normalizeServerURL(servers[0].URL)
	if err != nil {
		return fmt.Errorf("%w: server %q", helpers.ErrInvalidGalaxyServerURL, servers[0].ID)
	}
	if err := checkTokenTransport(servers[0].ID, token, parsed); err != nil {
		return err
	}
	servers[0].Token = token
	return nil
}

// resolveServerCandidates implements the precedence chain documented on
// resolveServers, returning the final, origin-conflict-checked server list
// (never empty on success). Any unknown-key warnings collected while
// building the servers are appended to cfg.Warnings directly, since they
// are non-fatal regardless of which branch below produced them.
func resolveServerCandidates(
	cfg *Config, c *cli.Command, ids []string, sections map[string]map[string]string,
) ([]Server, error) {
	if c.IsSet("server") {
		server, warnings, err := resolveExplicitServer(c.String("server"), ids, sections)
		cfg.Warnings = append(cfg.Warnings, warnings...)
		if err != nil {
			return nil, err
		}
		return []Server{server}, nil
	}

	if len(ids) > 0 {
		if err := validateServerIDs(ids); err != nil {
			return nil, err
		}
		servers, warnings, err := buildServerList(ids, sections)
		cfg.Warnings = append(cfg.Warnings, warnings...)
		if err != nil {
			return nil, err
		}
		if err := checkOriginConflicts(servers); err != nil {
			return nil, err
		}
		return servers, nil
	}

	server, err := buildImplicitServer(cfg.Server)
	if err != nil {
		return nil, err
	}
	return []Server{server}, nil
}

// resolveExplicitServer implements precedence rule 1: value names a
// server_list id (exact, case-sensitive match) selects that one server,
// built the same way the list path would build it (its own env overrides,
// key validation, and hard-error checks all still apply); any other value
// is used verbatim as an anonymous single-server URL via buildImplicitServer,
// and the rest of server_list - including its own id/key validation - is
// never consulted, matching ansible's "the flag simply wins" behavior.
func resolveExplicitServer(value string, ids []string, sections map[string]map[string]string) (Server, []string, error) {
	for _, id := range ids {
		if id != value {
			continue
		}
		if !serverIDPattern.MatchString(id) {
			return Server{}, nil, fmt.Errorf("%w: %q", helpers.ErrInvalidGalaxyServerID, id)
		}
		return buildServer(id, sections[id])
	}
	server, err := buildImplicitServer(value)
	return server, nil, err
}

// resolveServerList resolves the [galaxy] server_list value - or its
// ANSIBLE_GALAXY_SERVER_LIST env override, which wins outright whenever the
// env var is set at all, even to an empty string - into its ordered,
// trimmed, non-empty ids. An all-whitespace value from either source is
// treated as unset, matching an absent key.
func resolveServerList(ansCfg ansibleConfig) []string {
	raw := ansCfg.Galaxy.ServerList
	if v, ok := os.LookupEnv("ANSIBLE_GALAXY_SERVER_LIST"); ok {
		raw = v
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}

	parts := strings.Split(raw, ",")
	ids := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			ids = append(ids, p)
		}
	}
	return ids
}

// validateServerIDs checks the two list-level invariants ansible itself
// does not enforce but this tool does: every id matches serverIDPattern,
// and no two ids collide - either as an exact repeat or by differing only
// in case, since both would resolve to the same
// ANSIBLE_GALAXY_SERVER_<ID>_* environment variable prefix.
func validateServerIDs(ids []string) error {
	seen := make(map[string]string, len(ids))
	for _, id := range ids {
		if !serverIDPattern.MatchString(id) {
			return fmt.Errorf("%w: %q", helpers.ErrInvalidGalaxyServerID, id)
		}
		lower := strings.ToLower(id)
		if prior, ok := seen[lower]; ok {
			return fmt.Errorf("%w: %q and %q", helpers.ErrDuplicateGalaxyServerID, prior, id)
		}
		seen[lower] = id
	}
	return nil
}

// buildServerList builds every id in server_list, in list order, into a
// Server, collecting unknown-key warnings across all of them. It stops and
// returns the first hard error encountered, along with whatever warnings
// had already been collected.
func buildServerList(ids []string, sections map[string]map[string]string) ([]Server, []string, error) {
	servers := make([]Server, 0, len(ids))
	var warnings []string
	for _, id := range ids {
		server, w, err := buildServer(id, sections[id])
		warnings = append(warnings, w...)
		if err != nil {
			return nil, warnings, err
		}
		servers = append(servers, server)
	}
	return servers, warnings, nil
}

// buildServer resolves one [galaxy_server.<id>] section into a Server. kv
// is nil-safe: an id with no ini section at all (purely env-driven, the
// point of ansible's ANSIBLE_GALAXY_SERVER_<ID>_* naming scheme in
// environments like Docker or Kubernetes where dashes in a shell-unsettable
// id are still fine) builds normally from its env overrides alone.
func buildServer(id string, kv map[string]string) (Server, []string, error) {
	keys := sortedKeys(kv)

	if err := checkHardErrorKeys(id, keys); err != nil {
		return Server{}, nil, err
	}
	if v, ok := kv["api_version"]; ok && v != "v3" {
		return Server{}, nil, fmt.Errorf("%w: server %q api_version %q", helpers.ErrUnsupportedGalaxyServerAPIVersion, id, v)
	}
	warnings := unknownKeyWarnings(id, keys)

	normalized, parsed, err := resolveServerURL(id, envOrIni(id, "URL", kv["url"]))
	if err != nil {
		return Server{}, warnings, err
	}

	token := NewSecret(envOrIni(id, "TOKEN", kv["token"]))
	insecure, err := resolveValidateCerts(id, envOrIni(id, "VALIDATE_CERTS", kv["validate_certs"]))
	if err != nil {
		return Server{}, warnings, err
	}

	if err := checkTokenTransport(id, token, parsed); err != nil {
		return Server{}, warnings, err
	}

	return Server{ID: id, URL: normalized, Token: token, InsecureSkipTLSVerify: insecure}, warnings, nil
}

// checkHardErrorKeys returns ErrUnsupportedGalaxyServerKey, naming the
// server id and the offending key, for the first hard-error key (in sorted
// order, so the outcome is deterministic regardless of map iteration) found
// among keys.
func checkHardErrorKeys(id string, keys []string) error {
	for _, key := range keys {
		if galaxyServerHardErrorKeys[key] {
			return fmt.Errorf("%w: server %q key %q", helpers.ErrUnsupportedGalaxyServerKey, id, key)
		}
	}
	return nil
}

// unknownKeyWarnings returns one warning per key in keys that
// galaxyServerKnownKeys does not recognize, in sorted order for
// deterministic output.
func unknownKeyWarnings(id string, keys []string) []string {
	var warnings []string
	for _, key := range keys {
		if !galaxyServerKnownKeys[key] {
			warnings = append(warnings, fmt.Sprintf("unsupported key %q in [galaxy_server.%s] ignored", key, id))
		}
	}
	return warnings
}

// resolveServerURL requires and normalizes the url configured for server
// id, returning both the normalized string and its parsed form so callers
// need not reparse it for the userinfo and origin checks.
func resolveServerURL(id, raw string) (string, *url.URL, error) {
	if raw == "" {
		return "", nil, fmt.Errorf("%w: server %q", helpers.ErrMissingGalaxyServerURL, id)
	}
	normalized, parsed, err := normalizeServerURL(raw)
	if err != nil {
		return "", nil, fmt.Errorf("%w: server %q", helpers.ErrInvalidGalaxyServerURL, id)
	}
	if parsed.User != nil {
		return "", nil, fmt.Errorf("%w: server %q", helpers.ErrGalaxyServerURLUserinfo, id)
	}
	return normalized, parsed, nil
}

// resolveValidateCerts parses the effective validate_certs value (already
// env/ini-resolved by the caller) into InsecureSkipTLSVerify: an unset key
// (raw == "") defaults to certs verified, never a silent false, and an
// unparseable value is always a hard error.
func resolveValidateCerts(id, raw string) (bool, error) {
	if raw == "" {
		return false, nil
	}
	validate, ok := parseAnsibleBool(raw)
	if !ok {
		return false, fmt.Errorf("%w: server %q value %q", helpers.ErrInvalidValidateCerts, id, raw)
	}
	return !validate, nil
}

// parseAnsibleBool parses one of ansible's boolean spellings,
// case-insensitively: true/false, yes/no, on/off, 1/0. ok is false for
// anything else.
func parseAnsibleBool(raw string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "yes", "on", "1":
		return true, true
	case "false", "no", "off", "0":
		return false, true
	default:
		return false, false
	}
}

// envOrIni resolves one per-server key using "env beats ini" precedence:
// ANSIBLE_GALAXY_SERVER_<ID>_<KEY> - id exactly as written in server_list,
// key one of "URL", "TOKEN", "VALIDATE_CERTS", with no character
// translation of either (ansible does not case- or dash-normalize the id,
// so neither does this) - wins outright when the environment variable is
// set at all, even to an empty string; otherwise iniValue is used.
func envOrIni(id, key, iniValue string) string {
	if v, ok := os.LookupEnv("ANSIBLE_GALAXY_SERVER_" + strings.ToUpper(id) + "_" + key); ok {
		return v
	}
	return iniValue
}

// buildImplicitServer builds the single anonymous server ("" id, no token,
// certs verified) used whenever no server_list is in effect: rawURL is
// already the fully precedence-resolved single-server value ([galaxy]
// server, the --server flag, or an explicit anonymous --server override).
//
// An empty rawURL is not itself an error: BuildCollectionConfig's
// documented contract is that a command which does not register the
// "server" flag at all (cleanup, today) reads it back as the Go zero value
// via pickConfigValue, and never reads Config.Server or Config.Servers.
// Hard-failing that command's config build over a field it never asked for
// would violate the very contract this package already promises every
// other unregistered flag; a real, flag-registering command's default is
// never empty (see cmd/go-galaxy/helpers/flags.go's defaultServerURL), so
// this only ever fires for the "not registered at all" case in practice.
func buildImplicitServer(rawURL string) (Server, error) {
	if rawURL == "" {
		return Server{}, nil
	}
	normalized, parsed, err := normalizeServerURL(rawURL)
	if err != nil {
		return Server{}, helpers.ErrInvalidGalaxyServerURL
	}
	if parsed.User != nil {
		return Server{}, helpers.ErrGalaxyServerURLUserinfo
	}
	return Server{URL: normalized}, nil
}

// normalizeServerURL trims raw, strips one pair of surrounding double
// quotes (ansible.cfg's INI parser does not do this, but a value
// copy-pasted from a shell export often carries them), trims trailing
// slashes, and parses the result as an absolute URL. The parsed *url.URL is
// returned alongside the normalized string so callers can reuse it for the
// userinfo and origin checks without parsing twice.
func normalizeServerURL(raw string) (string, *url.URL, error) {
	v := strings.TrimSpace(raw)
	if len(v) >= 2 && strings.HasPrefix(v, `"`) && strings.HasSuffix(v, `"`) {
		v = v[1 : len(v)-1]
	}
	v = strings.TrimRight(v, "/")

	parsed, err := url.Parse(v)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", nil, helpers.ErrInvalidGalaxyServerURL
	}
	return v, parsed, nil
}

// checkTokenTransport rejects a token configured for a plaintext,
// non-loopback origin. Sending a credential over http:// hands it to any
// passive observer on the path - a proxy, a capture, a log - which no
// ansible-compatibility argument justifies, since validate_certs never
// turns http:// into https:// either. Loopback is exempt: nothing leaves
// the machine. id names the server in the error; it is "" for the implicit
// single server and for a --token applied to it, which still reads
// sensibly.
func checkTokenTransport(id string, token Secret, parsed *url.URL) error {
	if !token.IsSet() || parsed == nil {
		return nil
	}
	if !strings.EqualFold(parsed.Scheme, "http") || isLoopbackHost(parsed.Hostname()) {
		return nil
	}
	return fmt.Errorf("%w: server %q (%s)", helpers.ErrInsecureTokenTransport, id, helpers.Origin(parsed))
}

// isLoopbackHost reports whether host - already bracket-stripped via
// url.URL.Hostname() - refers to the local machine: the literal name
// "localhost", or an IP literal in 127.0.0.0/8 or the IPv6 loopback ::1.
// DNS is never consulted here, so a hostname that merely happens to resolve
// to loopback at runtime (e.g. via /etc/hosts) is treated as non-loopback;
// this is a config-time syntactic check, not a network probe.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// checkOriginConflicts enforces "one origin maps to exactly one transport":
// if two configured servers share a normalized origin (see helpers.Origin),
// they must agree on both validate_certs and token, since a later unit
// resolves the actual transport (TLS policy, credential attachment) per
// origin rather than per configured server id.
func checkOriginConflicts(servers []Server) error {
	seen := make(map[string]Server, len(servers))
	for _, s := range servers {
		parsed, err := url.Parse(s.URL)
		if err != nil {
			// s.URL was already normalized and validated by buildServer; an
			// already-valid absolute URL string always reparses cleanly.
			return fmt.Errorf("%w: server %q", helpers.ErrInvalidGalaxyServerURL, s.ID)
		}
		origin := helpers.Origin(parsed)

		prior, ok := seen[origin]
		if !ok {
			seen[origin] = s
			continue
		}
		if prior.InsecureSkipTLSVerify != s.InsecureSkipTLSVerify {
			return fmt.Errorf("%w: %q and %q share origin %s", helpers.ErrConflictingServerTLSPolicy, prior.ID, s.ID, origin)
		}
		if !prior.Token.sameAs(s.Token) {
			return fmt.Errorf("%w: %q and %q share origin %s", helpers.ErrConflictingServerToken, prior.ID, s.ID, origin)
		}
	}
	return nil
}

// tlsWarnings queues, once per configured server (never per request), the
// two TLS-related operator warnings: certificate verification disabled,
// and - only when that same server also carries a token - a token being
// sent over that unverified connection. The exact wording is load-bearing:
// it is asserted verbatim by tests and read by operators deciding whether
// to trust a CI log.
func tlsWarnings(servers []Server) []string {
	var warnings []string
	for _, s := range servers {
		if !s.InsecureSkipTLSVerify {
			continue
		}
		parsed, err := url.Parse(s.URL)
		if err != nil {
			continue // s.URL was already validated when the server was built.
		}
		origin := helpers.Origin(parsed)
		warnings = append(warnings, fmt.Sprintf(
			"TLS certificate verification is DISABLED for Galaxy server %q (%s); "+
				"this run cannot detect a man-in-the-middle on that host", s.ID, origin))
		if s.Token.IsSet() {
			warnings = append(warnings, fmt.Sprintf(
				"A Galaxy API token is being sent to server %q (%s) over a connection whose certificate is not "+
					"verified; the token can be captured by an on-path attacker", s.ID, origin))
		}
	}
	return warnings
}

// sortedKeys returns kv's keys in sorted order, so callers that must report
// "the first offending key" or "every unrecognized key" do so
// deterministically instead of depending on Go's randomized map iteration.
// kv may be nil (an id with no ini section at all).
func sortedKeys(kv map[string]string) []string {
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
