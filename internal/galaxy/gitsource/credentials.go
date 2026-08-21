package gitsource

import "strings"

// Credential is one host-bound git credential in the plain form the fetcher
// consumes: the binding URL (a ParsePrefix result), and either a Basic
// username and password for http(s) or an ssh private key with its optional
// passphrase. It carries secrets in the clear, which is why it is built in
// exactly one place, the command wiring that reveals config.GitCredential's
// Secret values, and never persisted, printed or formatted: nothing in this
// package or in gitfetch renders one, and a test in config pins that the
// Secret-bearing form redacts.
type Credential struct {
	URL           URL
	Username      string
	Password      string
	SSHPassphrase string
	SSHKey        []byte
}

// IsZero reports whether the credential binds nothing: the value
// MatchCredential returns when no binding covers a URL.
func (c Credential) IsZero() bool {
	return c.URL.Host == "" && c.Username == "" && c.Password == "" && len(c.SSHKey) == 0
}

// MatchCredential returns the credential bound to u, if any. A binding
// applies when its origin (scheme, host and effective port) equals u's and u's
// path equals the binding path or lies beneath it; among several, the longest
// path prefix wins. The ssh user of u is not part of the match - a binding
// names a host, and the requirement URL names the login. Duplicate bindings
// are refused at configuration time, so a tie cannot happen here.
func MatchCredential(u URL, creds []Credential) (Credential, bool) {
	var best Credential
	bestLen := -1
	for _, cred := range creds {
		if cred.URL.Origin() != u.Origin() {
			continue
		}
		// Both sides are compared without their leading slash so a binding
		// written as ssh://host/org also covers the scp-like git@host:org/repo,
		// whose path is spelled relative.
		prefix := strings.TrimPrefix(cred.URL.Path, "/")
		path := strings.TrimPrefix(u.Path, "/")
		if prefix != "" && path != prefix && !strings.HasPrefix(path, prefix+"/") {
			continue
		}
		if len(prefix) > bestLen {
			best, bestLen = cred, len(prefix)
		}
	}
	return best, bestLen >= 0
}
