package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// GitCredentialKind tells a Basic (http(s)) binding from an ssh key binding.
// It is derived from the variables an id carries and is checked against the
// binding URL's scheme at load time, so a consumer never has to decide which
// of the two field groups applies.
type GitCredentialKind uint8

const (
	// GitCredentialBasic is a username and password sent as HTTP Basic auth
	// to an https (or loopback http) origin.
	GitCredentialBasic GitCredentialKind = iota + 1
	// GitCredentialSSHKey is a PEM private key, with an optional passphrase,
	// offered to an ssh origin.
	GitCredentialSSHKey
)

// GitCredential is one host-bound git credential as this package holds it:
// the id it was declared under, the binding URL (a gitsource.ParsePrefix
// result), and the Secret-wrapped fields of exactly one kind. The Secret
// fields of the other kind are zero. The plaintext leaves this form in one
// place only, the command wiring that builds a gitsource.Credential from it.
type GitCredential struct {
	ID            string
	Username      string
	Password      Secret
	SSHKeyPEM     Secret
	SSHPassphrase Secret
	URL           gitsource.URL
	Kind          GitCredentialKind
}

const (
	// gitCredentialsListEnv lists the declared credential ids; nothing else
	// under the prefix is read for an id that is not listed here.
	gitCredentialsListEnv = "GO_GALAXY_GIT_CREDENTIALS" //nolint:gosec // an environment variable name, not a credential
	// gitCredentialEnvPrefix is the common prefix of every per-id variable,
	// GO_GALAXY_GIT_<ID>_<KEY>, with <ID> upper-cased exactly as envOrIni
	// upper-cases a server id - which is why two ids differing only in case
	// are refused: they fold onto one variable prefix.
	gitCredentialEnvPrefix = "GO_GALAXY_GIT_" //nolint:gosec // an environment variable name prefix, not a credential

	gitKeyURL              = "URL"
	gitKeyUsername         = "USERNAME"
	gitKeyPassword         = "PASSWORD"
	gitKeySSHKey           = "SSH_KEY"
	gitKeySSHKeyFile       = "SSH_KEY_FILE"
	gitKeySSHKeyPassphrase = "SSH_KEY_PASSPHRASE" //nolint:gosec // an environment variable name suffix, not a credential

	// gitSchemeSSH is the one binding scheme an ssh key applies to; the
	// other two schemes gitsource.ParsePrefix admits take Basic.
	gitSchemeSSH = "ssh"
)

// gitCredentialKeys is every key this tool reads for a declared id; the
// unknown-variable scan treats any other name under the id's prefix as
// unsupported.
func gitCredentialKeys() []string {
	return []string{gitKeyURL, gitKeyUsername, gitKeyPassword, gitKeySSHKey, gitKeySSHKeyFile, gitKeySSHKeyPassphrase}
}

// gitCredentialEnv is the raw per-id variable surface, read once per id so
// every rule below judges the same snapshot and names the variable it read.
type gitCredentialEnv struct {
	id         string
	url        string
	username   string
	password   string
	sshKey     string
	sshKeyFile string
	passphrase string
}

// loadGitCredentials fills cfg.GitCredentials from the environment. The
// surface is GO_GALAXY_GIT_CREDENTIALS, a comma-separated list of ids, and
// for each id the GO_GALAXY_GIT_<ID>_{URL,USERNAME,PASSWORD,SSH_KEY,
// SSH_KEY_FILE,SSH_KEY_PASSPHRASE} variables. An unset or empty list means
// no credentials and is not an error. A variable whose value is empty counts
// as unset everywhere below, so an exported-but-blank secret never produces
// a half-configured binding.
//
// Checks run in a fixed order so the first failure a broken configuration
// reports is stable: the id list's grammar, then per id in list order the
// URL, then the kind rules, then duplicates across ids. Every refusal is
// helpers.ErrGitCredentialInvalid and names a VARIABLE, never a value: the
// value of any of these variables is, or sits beside, a credential. The one
// exception in sentinel is a Basic credential bound to plaintext http on a
// non-loopback host, which is helpers.ErrInsecureTokenTransport for the same
// reason checkTokenTransport refuses a Galaxy token there: nothing about a
// git remote makes a password on the wire safer than a token.
//
// An ssh key named by file is read here, at configuration time, into a
// Secret: an unreadable path fails the run before any network is touched,
// and the fetcher never handles a path. A GO_GALAXY_GIT_<ID>_* variable for
// a declared id that is not one of the six keys is queued on cfg.Warnings
// and ignored, the way an unknown [galaxy_server.<id>] key is.
func loadGitCredentials(cfg *Config) error {
	ids, err := gitCredentialIDs(os.Getenv(gitCredentialsListEnv))
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}

	creds := make([]GitCredential, 0, len(ids))
	for _, id := range ids {
		cred, err := buildGitCredential(readGitCredentialEnv(id))
		if err != nil {
			return err
		}
		creds = append(creds, cred)
	}
	if err := checkGitCredentialURLConflicts(creds); err != nil {
		return err
	}
	cfg.Warnings = append(cfg.Warnings, unknownGitVariableWarnings(ids, os.Environ())...)
	cfg.GitCredentials = creds
	return nil
}

// gitCredentialIDs splits the id list and applies the same two list-level
// rules validateServerIDs applies to server ids, for the same reason: each
// id becomes an environment variable prefix, so it must be spellable as one
// and must not collide with another by case alone. A blank list is empty
// rather than invalid; a blank element inside a non-blank list is invalid,
// since it is what a stray comma produces.
func gitCredentialIDs(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	ids := make([]string, 0, len(parts))
	seen := make(map[string]string, len(parts))
	for _, part := range parts {
		id := strings.TrimSpace(part)
		if id == "" {
			return nil, fmt.Errorf("%w: %s carries an empty id", helpers.ErrGitCredentialInvalid, gitCredentialsListEnv)
		}
		if !serverIDPattern.MatchString(id) {
			return nil, fmt.Errorf("%w: %s lists id %q, which is not spellable as an environment variable (allowed: A-Z a-z 0-9 _ -)",
				helpers.ErrGitCredentialInvalid, gitCredentialsListEnv, id)
		}
		lower := strings.ToLower(id)
		if prior, ok := seen[lower]; ok {
			return nil, fmt.Errorf("%w: %s lists %q and %q, which read the same %s%s_* variables",
				helpers.ErrGitCredentialInvalid, gitCredentialsListEnv, prior, id, gitCredentialEnvPrefix, strings.ToUpper(id))
		}
		seen[lower] = id
		ids = append(ids, id)
	}
	return ids, nil
}

// gitCredentialVar composes the variable name for one key of one id.
func gitCredentialVar(id, key string) string {
	return gitCredentialEnvPrefix + strings.ToUpper(id) + "_" + key
}

// readGitCredentialEnv snapshots the six variables of one id. The non-secret
// values are trimmed so a trailing newline from a CI secret store does not
// change a path or a URL; the secrets are taken verbatim, since a password
// or a PEM may legitimately end in whitespace.
func readGitCredentialEnv(id string) gitCredentialEnv {
	return gitCredentialEnv{
		id:         id,
		url:        strings.TrimSpace(os.Getenv(gitCredentialVar(id, gitKeyURL))),
		username:   strings.TrimSpace(os.Getenv(gitCredentialVar(id, gitKeyUsername))),
		password:   os.Getenv(gitCredentialVar(id, gitKeyPassword)),
		sshKey:     os.Getenv(gitCredentialVar(id, gitKeySSHKey)),
		sshKeyFile: strings.TrimSpace(os.Getenv(gitCredentialVar(id, gitKeySSHKeyFile))),
		passphrase: os.Getenv(gitCredentialVar(id, gitKeySSHKeyPassphrase)),
	}
}

// buildGitCredential settles one id: its URL first, then which kind the
// scheme demands and whether the variables present form exactly that kind.
func buildGitCredential(env gitCredentialEnv) (GitCredential, error) {
	urlVar := gitCredentialVar(env.id, gitKeyURL)
	if env.url == "" {
		return GitCredential{}, fmt.Errorf("%w: %s is not set", helpers.ErrGitCredentialInvalid, urlVar)
	}
	parsed, err := gitsource.ParsePrefix(env.url)
	if err != nil {
		return GitCredential{}, fmt.Errorf("%w: %s: %w", helpers.ErrGitCredentialInvalid, urlVar, err)
	}

	basicVars := env.presentVars(gitKeyUsername, gitKeyPassword)
	sshVars := env.presentVars(gitKeySSHKey, gitKeySSHKeyFile, gitKeySSHKeyPassphrase)
	if parsed.Scheme == gitSchemeSSH {
		if len(basicVars) > 0 {
			return GitCredential{}, fmt.Errorf("%w: %s names an ssh URL, which takes an ssh key, not %s",
				helpers.ErrGitCredentialInvalid, urlVar, strings.Join(basicVars, " and "))
		}
		return buildGitSSHCredential(env, parsed)
	}
	if len(sshVars) > 0 {
		return GitCredential{}, fmt.Errorf("%w: %s names an %s URL, which takes a username and password, not %s",
			helpers.ErrGitCredentialInvalid, urlVar, parsed.Scheme, strings.Join(sshVars, " and "))
	}
	return buildGitBasicCredential(env, parsed)
}

// presentVars returns the variable names, among keys, whose value is set.
func (env gitCredentialEnv) presentVars(keys ...string) []string {
	values := map[string]string{
		gitKeyUsername:         env.username,
		gitKeyPassword:         env.password,
		gitKeySSHKey:           env.sshKey,
		gitKeySSHKeyFile:       env.sshKeyFile,
		gitKeySSHKeyPassphrase: env.passphrase,
	}
	var present []string
	for _, key := range keys {
		if values[key] != "" {
			present = append(present, gitCredentialVar(env.id, key))
		}
	}
	return present
}

// buildGitBasicCredential requires both halves of a Basic pair: a username
// alone authenticates nothing, and a password alone has no principal to be
// checked against, so either shape is a configuration that was meant to be
// the other and is refused rather than sent.
func buildGitBasicCredential(env gitCredentialEnv, parsed gitsource.URL) (GitCredential, error) {
	userVar := gitCredentialVar(env.id, gitKeyUsername)
	passVar := gitCredentialVar(env.id, gitKeyPassword)
	switch {
	case env.username == "" && env.password == "":
		return GitCredential{}, fmt.Errorf("%w: id %q configures neither %s and %s nor an ssh key",
			helpers.ErrGitCredentialInvalid, env.id, userVar, passVar)
	case env.username == "":
		return GitCredential{}, fmt.Errorf("%w: %s is set without %s", helpers.ErrGitCredentialInvalid, passVar, userVar)
	case env.password == "":
		return GitCredential{}, fmt.Errorf("%w: %s is set without %s", helpers.ErrGitCredentialInvalid, userVar, passVar)
	}
	if parsed.Scheme == "http" && !parsed.IsLoopback() {
		return GitCredential{}, fmt.Errorf("%w: git credential %q (%s)", helpers.ErrInsecureTokenTransport, env.id, parsed.Origin())
	}
	return GitCredential{
		ID:       env.id,
		URL:      parsed,
		Username: env.username,
		Password: NewSecret(env.password),
		Kind:     GitCredentialBasic,
	}, nil
}

// buildGitSSHCredential requires exactly one source for the key and reads a
// file-sourced key now, so the PEM is held the same way whichever variable
// supplied it and a bad path fails before the run reaches a remote.
func buildGitSSHCredential(env gitCredentialEnv, parsed gitsource.URL) (GitCredential, error) {
	keyVar := gitCredentialVar(env.id, gitKeySSHKey)
	fileVar := gitCredentialVar(env.id, gitKeySSHKeyFile)
	switch {
	case env.sshKey != "" && env.sshKeyFile != "":
		return GitCredential{}, fmt.Errorf("%w: %s and %s are both set; the key comes from one of them",
			helpers.ErrGitCredentialInvalid, keyVar, fileVar)
	case env.sshKey == "" && env.sshKeyFile == "" && env.passphrase != "":
		return GitCredential{}, fmt.Errorf("%w: %s is set without %s or %s",
			helpers.ErrGitCredentialInvalid, gitCredentialVar(env.id, gitKeySSHKeyPassphrase), keyVar, fileVar)
	case env.sshKey == "" && env.sshKeyFile == "":
		return GitCredential{}, fmt.Errorf("%w: id %q configures neither %s nor %s nor a username and password",
			helpers.ErrGitCredentialInvalid, env.id, keyVar, fileVar)
	}
	pem := env.sshKey
	if env.sshKeyFile != "" {
		// The path is operator-configured from the environment, never
		// repository content, which is why reading it is not an inclusion
		// this tool has to defend against.
		data, err := os.ReadFile(filepath.Clean(env.sshKeyFile))
		if err != nil {
			return GitCredential{}, fmt.Errorf("%w: %s names a key this process cannot read: %w",
				helpers.ErrGitCredentialInvalid, fileVar, err)
		}
		pem = string(data)
	}
	return GitCredential{
		ID:            env.id,
		URL:           parsed,
		SSHKeyPEM:     NewSecret(pem),
		SSHPassphrase: NewSecret(env.passphrase),
		Kind:          GitCredentialSSHKey,
	}, nil
}

// checkGitCredentialURLConflicts refuses two ids bound to one canonical URL:
// gitsource.MatchCredential picks the longest path prefix and relies on this
// check to make a tie impossible, so a second binding to the same URL would
// otherwise be silently ignored or silently preferred by list order.
func checkGitCredentialURLConflicts(creds []GitCredential) error {
	seen := make(map[string]string, len(creds))
	for _, cred := range creds {
		key := cred.URL.String()
		if prior, ok := seen[key]; ok {
			return fmt.Errorf("%w: %s and %s bind the same URL", helpers.ErrGitCredentialInvalid,
				gitCredentialVar(prior, gitKeyURL), gitCredentialVar(cred.ID, gitKeyURL))
		}
		seen[key] = cred.ID
	}
	return nil
}

// unknownGitVariableWarnings returns one warning per environment variable
// that sits under a declared id's prefix and is none of the six keys. The
// known names of every id are collected first, because one id's prefix can
// be a prefix of another's (ids "a" and "a_b" share GO_GALAXY_GIT_A_), and a
// variable that is a known key of the longer id must not be reported as an
// unknown key of the shorter one. The result is sorted so the warning order
// does not depend on the environment's own.
func unknownGitVariableWarnings(ids []string, environ []string) []string {
	keys := gitCredentialKeys()
	known := make(map[string]bool, len(ids)*len(keys))
	prefixes := make([]string, 0, len(ids))
	for _, id := range ids {
		prefixes = append(prefixes, gitCredentialVar(id, ""))
		for _, key := range keys {
			known[gitCredentialVar(id, key)] = true
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
