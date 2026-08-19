package config

import (
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/urfave/cli/v3"
)

// Secret wraps any credential this tool holds - a Galaxy API token, the S3
// cache's secret key, its session token - so that leaking one by accident is
// structurally hard rather than merely a matter of remembering to redact it:
// fmt (%v %s %q %x %X %+v %#v), encoding/json, and yaml.v3 all render a
// Secret as a fixed redacted placeholder, and the plaintext is reachable
// through exactly one method, Reveal, whose name is deliberately loud and
// rare so every call site is easy to find by grep.
//
// No credential, and no value derived from one - not even a hash - may ever
// be persisted: not to the Bolt snapshot, the S3 snapshot, the project
// registry, the lockfile, the metrics file, or GALAXY.yml. Those are all
// state that outlives a single run and can be read by a principal who does
// not need the credential itself to install anything. A Secret's redacted
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

// MarshalYAML implements yaml.Marshaler (go.yaml.in/yaml/v3, already a
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
	// urlFromAnsibleConfig reports whether URL was sourced from an
	// ansible.cfg file - [galaxy] server or a [galaxy_server.<id>] url -
	// rather than from the operator: anything but the ansible.cfg file
	// itself, which is the flag's own built-in default, --server,
	// GO_GALAXY_SERVER, ANSIBLE_GALAXY_SERVER, and
	// ANSIBLE_GALAXY_SERVER_<ID>_URL. Those five are the whole operator
	// channel list; buildImplicitServer's own doc comment names only the
	// four its path can produce, since a per-server _URL override is read
	// by buildServer and never reaches it. Unexported deliberately:
	// provenance is config-internal and must never reach serverAuths, which
	// needs the resolved origin, token, and TLS policy - never where any of
	// the three came from. tokenPairingOffense is the sole reader.
	urlFromAnsibleConfig bool
	// tokenFromAnsibleConfig mirrors urlFromAnsibleConfig for Token: it
	// reports whether Token was sourced from a [galaxy_server.<id>] token
	// key rather than from the operator (--token, GO_GALAXY_TOKEN, or
	// ANSIBLE_GALAXY_SERVER_<ID>_TOKEN). It is a pure provenance bool, so
	// it also reads true for a server whose section declares no token key
	// at all: envOrIni returns ("", false) for a key nothing set, and this
	// field is that bool negated. Nothing acts on the wrong-looking true,
	// because tokenPairingOffense reaches this field only past its own
	// Token.IsSet() term, which a server carrying no token never survives.
	// That makes the shield the reader's rather than the field's - the
	// asymmetry insecureFromAnsibleConfig below states from its side.
	// tokenPairingOffense is the sole reader.
	tokenFromAnsibleConfig bool
	// insecureFromAnsibleConfig is a conjunction, not a provenance: true only
	// when InsecureSkipTLSVerify is itself true AND that true came from a
	// [galaxy_server.<id>] validate_certs key rather than from
	// ANSIBLE_GALAXY_SERVER_<ID>_VALIDATE_CERTS. It is deliberately not a
	// pure provenance bool mirroring its two siblings above: envOrIni returns
	// ("", false) for a key the section never set at all, so a pure
	// provenance bool would read "the file supplied it" for a server with no
	// validate_certs key whatsoever - an actively wrong value a future gate
	// could read as "this server's TLS policy is file-sourced" when no policy
	// was ever stated. Folding InsecureSkipTLSVerify's own value into this one
	// makes that invalid state unrepresentable: a server with certs verified
	// (the default, or an explicit validate_certs = true) always reads false
	// here regardless of which channel supplied it. tokenFromAnsibleConfig
	// above is the pure provenance bool this one declines to be, and it does
	// carry exactly that wrong-looking true for an absent key; what keeps it
	// harmless is tokenPairingOffense's own Token.IsSet() term, a guarantee
	// that lives in the reader and moves whenever the reader changes, which
	// is what this field's shape does not need. tokenPairingOffense is the
	// sole reader.
	insecureFromAnsibleConfig bool
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
//  1. An explicit --server (c.IsSet("server"), covering the flag and its
//     GO_GALAXY_SERVER spelling) collapses everything to one server: if its
//     value matches a server_list id exactly (case-sensitive), that server is
//     used with its own token and TLS setting; otherwise the value is used
//     verbatim as an anonymous single-server URL, and server_list plays no
//     further part - not even to validate it.
//  2. Otherwise a non-empty server_list wins, in list order.
//  3. Otherwise [galaxy] server from ansible.cfg, or its ANSIBLE_GALAXY_SERVER
//     env spelling, which outranks the ini key but nothing above it (both
//     already folded into cfg.Server by pickConfigValue via
//     ansibleGalaxyServer). The ANSIBLE_ variable belongs here rather than in
//     rule 1 because that is where ansible itself puts it - as the env name of
//     the GALAXY_SERVER config, not as a way to override server_list.
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
	// Must run after applyTokenFlag: that call is what pairs an operator
	// --token/GO_GALAXY_TOKEN with servers[0], so checking either arm of the
	// pairing any earlier would inspect a token that has not been assigned
	// yet.
	if err := checkTokenPairing(servers); err != nil {
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
//
// The token this sets is always operator-sourced (--token or GO_GALAXY_TOKEN
// are both operator channels), so tokenFromAnsibleConfig is cleared alongside
// Token: a section's own token, once overridden here, must no longer read as
// authorizing anything through checkTokenPairing's pairing rule.
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
	servers[0].tokenFromAnsibleConfig = false
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

	// cfg.AnsibleServerUsed && !cfg.AnsibleServerEnvUsed is the predicate for
	// "cfg.Server was supplied by the ansible.cfg file, not by
	// $ANSIBLE_GALAXY_SERVER" - see Config.AnsibleServerEnvUsed's own doc
	// comment.
	server, err := buildImplicitServer(cfg.Server, cfg.AnsibleServerUsed && !cfg.AnsibleServerEnvUsed)
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
// never consulted.
//
// This is reached only for the flag itself and GO_GALAXY_SERVER. The
// ANSIBLE_GALAXY_SERVER spelling is precedence rule 3 and never arrives here,
// so nothing in this path claims to mirror what ansible does with that
// variable - ansible consults it only when no server_list and no -s apply.
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
	// value is --server's own value or GO_GALAXY_SERVER - both operator
	// channels - so the resulting anonymous server's URL is never
	// ansible.cfg-sourced.
	server, err := buildImplicitServer(value, false)
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

	var ids []string
	for p := range strings.SplitSeq(raw, ",") {
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

	rawURL, urlFromEnv := envOrIni(id, "URL", kv["url"])
	normalized, parsed, err := resolveServerURL(id, rawURL)
	if err != nil {
		return Server{}, warnings, err
	}

	rawToken, tokenFromEnv := envOrIni(id, "TOKEN", kv["token"])
	token := NewSecret(rawToken)
	rawValidateCerts, validateCertsFromEnv := envOrIni(id, "VALIDATE_CERTS", kv["validate_certs"])
	insecure, err := resolveValidateCerts(id, rawValidateCerts)
	if err != nil {
		return Server{}, warnings, err
	}

	if err := checkTokenTransport(id, token, parsed); err != nil {
		return Server{}, warnings, err
	}

	return Server{
		ID: id, URL: normalized, Token: token, InsecureSkipTLSVerify: insecure,
		urlFromAnsibleConfig:      !urlFromEnv,
		tokenFromAnsibleConfig:    !tokenFromEnv,
		insecureFromAnsibleConfig: insecure && !validateCertsFromEnv,
	}, warnings, nil
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
//
// The bool reports whether the environment supplied the value: buildServer
// needs it alongside the value itself to record, for every key whose
// provenance the pairing rule weighs, whether the ansible.cfg file or an
// operator channel supplied it - a distinction the resolved string alone
// cannot make once env and ini have already been collapsed into it.
// tokenPairingOffense is what reads those recorded bits, to tell a credential
// the operator supplied, or an address or TLS policy the operator named,
// apart from one this process merely read out of a section.
func envOrIni(id, key, iniValue string) (string, bool) {
	if v, ok := os.LookupEnv("ANSIBLE_GALAXY_SERVER_" + strings.ToUpper(id) + "_" + key); ok {
		return v, true
	}
	return iniValue, false
}

// buildImplicitServer builds the single anonymous server ("" id, no token,
// certs verified) used whenever no server_list is in effect: rawURL is
// already the fully precedence-resolved single-server value ([galaxy]
// server, the --server flag, or an explicit anonymous --server override).
// urlFromAnsibleConfig records that value's own provenance, exactly as
// buildServer does for a server_list entry's url: true when rawURL was
// sourced from the ansible.cfg file itself ([galaxy] server, but not its
// ANSIBLE_GALAXY_SERVER environment spelling), false for every operator
// channel (--server, GO_GALAXY_SERVER, ANSIBLE_GALAXY_SERVER, or the flag's
// own built-in default).
//
// An empty rawURL is not itself an error: BuildCollectionConfig's
// documented contract is that a command which does not register the
// "server" flag at all (cleanup, today) reads it back as the Go zero value
// via pickConfigValue, and never reads Config.Server or Config.Servers.
// Hard-failing that command's config build over a field it never asked for
// would violate the very contract this package already promises every
// other unregistered flag; a real, flag-registering command's default is
// never empty (see cmd/go-galaxy/cliflags/const.go's defaultServerURL), so
// this only ever fires for the "not registered at all" case in practice.
//
// It takes no validate_certs-provenance parameter to mirror
// urlFromAnsibleConfig with, and that is a containment fact rather than an
// omission: ansibleGalaxyConfig's [galaxy] struct carries only CacheDir,
// Server, ServerList, and SignatureKeys - there is no [galaxy] validate_certs
// key for this path to read - so the returned Server's
// insecureFromAnsibleConfig is always its zero value, false, and
// tokenPairingOffense's TLS-policy arm can never fire for a server this
// function built.
func buildImplicitServer(rawURL string, urlFromAnsibleConfig bool) (Server, error) {
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
	return Server{URL: normalized, urlFromAnsibleConfig: urlFromAnsibleConfig}, nil
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

// tokenPairingOffense reports the credential-pairing violation server s
// commits, or nil when none applies. It is the whole predicate
// checkTokenPairing consults, kept separate from the url.Parse and origin
// rendering below it so the pairing logic itself stays a pure function of a
// Server.
//
// A server is exempt - nil regardless of everything else - when no token is
// configured (!Token.IsSet()) or when the token itself came from the same
// ansible.cfg section as whatever else is being checked
// (tokenFromAnsibleConfig): a section that supplied its own credential
// authorizes only itself, never a value layered on top of it from an
// operator channel.
//
// Past that guard, two properties are checked in a fixed order, and the
// order is deliberate: whether the server's URL is file-sourced
// (urlFromAnsibleConfig) first, then - only once that is clear - whether an
// ansible.cfg file disabled certificate verification for it
// (insecureFromAnsibleConfig). A file-sourced destination completes an
// exfiltration on its own, since the operator's token then reaches an
// address the file alone chose; a file-sourced TLS policy still sends the
// token to an address the operator named, merely over a connection this run
// cannot authenticate. The graver fault is reported first, so an operator
// holding both is told the destination problem and reaches the TLS refusal
// only on the rerun, once that first one is fixed. Each of the two returned
// sentinels names exactly one remedy, which is why they are two distinct
// values rather than one error carrying a variable message.
func tokenPairingOffense(s Server) error {
	if !s.Token.IsSet() || s.tokenFromAnsibleConfig {
		return nil
	}
	if s.urlFromAnsibleConfig {
		return helpers.ErrTokenDestinationFromAnsibleConfig
	}
	if s.insecureFromAnsibleConfig {
		return helpers.ErrTokenTLSPolicyFromAnsibleConfig
	}
	return nil
}

// checkTokenPairing refuses a Galaxy token paired with a server this run did
// not source, in full, from the operator: a server URL - [galaxy] server or
// a [galaxy_server.<id>] url - sourced from an ansible.cfg file rather than
// from the operator, or a server whose certificate verification an
// ansible.cfg file disabled for it, in either case unless the token itself
// came from that same file's own [galaxy_server.<id>] section.
// tokenPairingOffense holds the per-server predicate and the fixed order
// between its two checks; this function is the walk over servers, the
// url.Parse needed to render an origin, and the error wrapping - never a
// second copy of the predicate.
//
// The one pairing rule governs two questions: WHERE a credential is sent
// (its destination), and WHETHER the address it is sent to is verified (its
// TLS policy). Both are checked on identical terms, which is what keeps this
// one function rather than two: the predicate is a pairing, never an
// authorship test. This function cannot learn who actually wrote a given
// ansible.cfg, so it does not try to - it
// only asks whether the credential and the property being checked against it
// arrived through the same channel. A section that pairs its own url, or its
// own validate_certs, with its own token (file+file) is accepted, because
// one author supplied both halves and could equally have chosen to send the
// token nowhere at all, or to leave verification on. What is refused is
// exactly the case where the two halves have different authors: a URL, or a
// relaxed TLS policy, a repository's ansible.cfg chose, paired with a token
// the operator supplied through --token, GO_GALAXY_TOKEN, or an
// ANSIBLE_GALAXY_SERVER_<ID>_TOKEN. A repository a CI job merely checks out
// can commit that file and name any address it likes, or disable
// verification for one it names honestly; the operator's export was never
// scoped to that address or that policy, so honoring either there hands the
// credential to whatever the checked-out tree names, or strips the
// authentication of the connection it travels over - the simplest shape
// needs nothing but a bare "[galaxy] server" line and the token environment
// variable CI already exports, with no server_list, no per-server section,
// and no id to guess.
//
// Naming a server through an operator channel is not the same as naming its
// address, or its TLS policy, through one: --server naming a server_list id,
// or ANSIBLE_GALAXY_SERVER_LIST naming which ids are even in play, both pick
// which section applies without supplying that section's own url or
// validate_certs, so neither launders a file-sourced value onto the operator
// channel - only a channel naming the value itself does that (--server given
// the url directly, GO_GALAXY_SERVER, ANSIBLE_GALAXY_SERVER, or
// ANSIBLE_GALAXY_SERVER_<ID>_URL for the destination;
// ANSIBLE_GALAXY_SERVER_<ID>_VALIDATE_CERTS for the TLS policy).
//
// What the rule costs a legitimate deployment is concrete, and the canonical
// ansible CI shape is the one that pays it: a [galaxy_server.<id>] url
// committed to the repository beside an ANSIBLE_GALAXY_SERVER_<ID>_TOKEN the
// job exports. That configuration is refused outright - a config-load failure
// exiting with the usage class, before any request leaves the process - until
// the operator either exports ANSIBLE_GALAXY_SERVER_<ID>_URL naming the
// identical address the section already carries, or moves the token into that
// same section. The price is one duplicated environment variable per
// authenticated server, paid once per pipeline; what it buys is that no
// checked-out file gets to pick where that credential goes.
//
// A section's own token does not authorize an operator token overriding it
// either: whether the section also declares its own "token = x" is a choice
// made by the same untrusted author as the url or the validate_certs beside
// it, and letting that declaration flip the verdict would let a hostile file
// unlock the operator's real credential by adding one line a diff review is
// unlikely to flag as security-relevant. So the file+file pairing above is
// the only shape a section's own token legitimizes; an operator token
// layered on top of it is refused exactly like one layered on a bare url or
// a bare validate_certs.
//
// The TLS-policy half is this codebase's signature-settings principle -
// stated in CLAUDE.md as "a setting able to relax a verification check must
// not come from a file whose author cannot be established", the reason
// ansible's four [galaxy] signature settings are configured from flags and
// environment variables only and never from a discovered ansible.cfg at all
// - applied to validate_certs, but scoped to the credential rather than
// unconditional. validate_certs, unlike those four keys, is inside this
// tool's ansible.cfg drop-in-compatibility promise, so an unauthenticated
// run against a self-hosted, private-CA hub configured entirely from
// ansible.cfg must keep working; refusing validate_certs outright the way
// the signature settings are refused would break that legitimate shape for a
// run sending no credential there at all. The refusal fires only once an
// operator's own token is in the picture alongside it.
//
// [galaxy] itself never reaches the TLS-policy arm: ansibleGalaxyConfig
// carries only CacheDir, Server, ServerList, and SignatureKeys - no
// validate_certs field - so buildImplicitServer, the bare "[galaxy] server"
// path, can never produce a Server with insecureFromAnsibleConfig true. That
// path can only ever commit the destination offense above.
//
// The preferred remedy avoids the setting entirely: point SSL_CERT_FILE or
// SSL_CERT_DIR at the hub's own CA and leave validate_certs unset, which
// needs no export this rule inspects at all. The direct remedy, when
// validate_certs = no genuinely has to stay, mirrors the destination
// refusal's own: ANSIBLE_GALAXY_SERVER_<ID>_VALIDATE_CERTS, naming the
// identical value, moves the policy onto the operator channel too, so the
// pairing holds by construction - not a go-galaxy-specific switch to learn,
// but the same environment surface the rest of this package already reads.
//
// Disclosed residual: this rule narrows an ansible.cfg file's reach over an
// operator's own credential; it does not, and is not meant to, stop a file
// disabling verification for an unauthenticated run, or for a credential the
// file itself supplied (the file+file shape above) - both stay covered by
// tlsWarnings alone, which fires on InsecureSkipTLSVerify regardless of any
// token's provenance.
//
// Two further limitations are disclosed rather than closed.
//
// The first: a server_list id containing "-" makes
// ANSIBLE_GALAXY_SERVER_<ID>_VALIDATE_CERTS - and, identically, its _URL and
// _TOKEN siblings - unsettable by a plain shell `export`, since "-" is not a
// legal character in a POSIX shell variable name. This function is what makes
// that limitation load-bearing: its remedies are what require an operator
// with a dashed id to set a per-server environment variable at all. Four
// routes remain: `env 'ANSIBLE_GALAXY_SERVER_MY-ID_VALIDATE_CERTS=no' go-galaxy
// ...` (env accepts a name a shell export cannot); a container's own `-e`
// flag, which carries the same exemption; renaming the id in server_list to
// use "_" instead of "-"; or --server=<url> naming the address directly,
// which discards the [galaxy_server.<id>] section entirely - its own token
// and validate_certs go with it, so that route is a different configuration
// rather than a workaround for this one.
//
// The second: an operator-sourced url or TLS policy paired with a
// file-sourced token is accepted by design, because the credential being sent is not the
// operator's own - this function has nothing of the operator's to protect
// there, so it stays silent. What that leaves as an accepted risk, beyond
// attribution and the visible collection set: a collection the operator's
// own account could see and the file-supplied token's account cannot now
// resolves as a 404 from that server instead of the intended content, and
// under a multi-entry server_list the resolve silently advances to the next
// configured server rather than surfacing the mismatch.
func checkTokenPairing(servers []Server) error {
	for _, s := range servers {
		offense := tokenPairingOffense(s)
		if offense == nil {
			continue
		}
		parsed, err := url.Parse(s.URL)
		if err != nil {
			// s.URL was already normalized and validated by buildServer or
			// buildImplicitServer; an already-valid absolute URL string
			// always reparses cleanly.
			return fmt.Errorf("%w: server %q", helpers.ErrInvalidGalaxyServerURL, s.ID)
		}
		return fmt.Errorf("%w: server %q (%s)", offense, s.ID, helpers.Origin(parsed))
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
	return slices.Sorted(maps.Keys(kv))
}
