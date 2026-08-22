package urlsource

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

const (
	schemeHTTPS = "https"
	schemeHTTP  = "http"
)

// URL is a parsed, canonical tarball URL. Scheme is https or http; Host is
// lower-cased, an IPv6 literal kept in brackets; Port is empty when it is the
// scheme's default; Path is the escaped path exactly as written after
// validation; Query is the raw query string, part of the source's identity.
type URL struct {
	Scheme string
	Host   string
	Port   string
	Path   string
	Query  string
}

// Prefix is a parsed credential binding: the origin a GO_GALAXY_URL_* variable
// names, plus an optional path prefix. It is the url counterpart of a git
// credential's binding URL and carries no query, fragment or userinfo.
type Prefix struct {
	Scheme string
	Host   string
	Port   string
	Path   string
}

// hostPattern bounds a host name: DNS labels, an IPv4 literal, or a bracketed
// IPv6 literal.
var hostPattern = regexp.MustCompile(`^(\[[0-9A-Fa-f:.]+\]|[A-Za-z0-9.-]+)$`)

// IsHTTPURL reports whether value spells an http(s) URL: the cheap dispatch
// test the requirements parser runs before this grammar judges the value.
func IsHTTPURL(value string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	return strings.HasPrefix(lower, schemeHTTPS+"://") || strings.HasPrefix(lower, schemeHTTP+"://")
}

// ParseURL parses raw as a tarball URL and returns its canonical form. It
// accepts absolute https:// and http:// URLs and refuses everything else with
// helpers.ErrInvalidURLRequirement; a credential in the URL is
// helpers.ErrURLRequirementUserinfo. See the package comment for why the path
// is validated as strictly as it is.
func ParseURL(raw string) (URL, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return URL{}, fmt.Errorf("%w: empty", helpers.ErrInvalidURLRequirement)
	}
	if strings.Contains(trimmed, "#") {
		return URL{}, fmt.Errorf("%w: a fragment is not part of a tarball URL: %s",
			helpers.ErrInvalidURLRequirement, helpers.URLForMessage(trimmed))
	}
	parsed, scheme, err := parseSchemeHost(trimmed)
	if err != nil {
		return URL{}, err
	}
	if parsed.User != nil {
		return URL{}, fmt.Errorf("%w: %s", helpers.ErrURLRequirementUserinfo, helpers.URLForMessage(trimmed))
	}
	path := parsed.EscapedPath()
	if err := checkSourcePath(path); err != nil {
		return URL{}, err
	}
	if err := checkQuery(parsed); err != nil {
		return URL{}, err
	}
	if (path == "" || path == "/") && parsed.RawQuery == "" {
		return URL{}, fmt.Errorf("%w: %s names nothing beyond its origin",
			helpers.ErrInvalidURLRequirement, helpers.URLForMessage(trimmed))
	}
	return URL{
		Scheme: scheme,
		Host:   hostWithBrackets(parsed),
		Port:   explicitPort(scheme, parsed.Port()),
		Path:   path,
		Query:  parsed.RawQuery,
	}, nil
}

// ParsePrefix parses raw as a credential binding: an https or http origin
// with an optional path prefix. The path may be empty (the binding then
// covers the whole origin) and loses its trailing slashes; a query, fragment
// or userinfo is never part of a binding. Every refusal is
// helpers.ErrURLCredentialInvalid, because the value reaches this function
// only from a GO_GALAXY_URL_<ID>_URL variable.
func ParsePrefix(raw string) (Prefix, error) {
	parsed, scheme, err := parsePrefixShape(strings.TrimSpace(raw))
	if err != nil {
		return Prefix{}, err
	}
	path := strings.TrimRight(parsed.EscapedPath(), "/")
	if path != "" {
		if err := checkPath(path, true); err != nil {
			return Prefix{}, fmt.Errorf("%w: %w", helpers.ErrURLCredentialInvalid, err)
		}
	}
	return Prefix{
		Scheme: scheme,
		Host:   hostWithBrackets(parsed),
		Port:   explicitPort(scheme, parsed.Port()),
		Path:   path,
	}, nil
}

// parsePrefixShape runs the shape refusals a binding answers to before its
// path is judged: an empty value, a "#" anywhere, a scheme or host
// parseSchemeHost refuses, userinfo, and a query.
func parsePrefixShape(trimmed string) (*url.URL, string, error) {
	if trimmed == "" {
		return nil, "", fmt.Errorf("%w: empty url", helpers.ErrURLCredentialInvalid)
	}
	if strings.Contains(trimmed, "#") {
		return nil, "", fmt.Errorf("%w: a query or fragment is not part of a credential binding",
			helpers.ErrURLCredentialInvalid)
	}
	parsed, scheme, err := parseSchemeHost(trimmed)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %w", helpers.ErrURLCredentialInvalid, err)
	}
	if parsed.User != nil {
		return nil, "", fmt.Errorf("%w: a credential binding names no user", helpers.ErrURLCredentialInvalid)
	}
	if parsed.RawQuery != "" || parsed.ForceQuery {
		return nil, "", fmt.Errorf("%w: a query or fragment is not part of a credential binding",
			helpers.ErrURLCredentialInvalid)
	}
	return parsed, scheme, nil
}

// parseSchemeHost runs the checks ParseURL and ParsePrefix share: an absolute
// URL, a scheme of https or http, and a well-formed host.
func parseSchemeHost(trimmed string) (*url.URL, string, error) {
	if !strings.Contains(trimmed, "://") {
		return nil, "", fmt.Errorf("%w: %q is not an absolute http(s) URL",
			helpers.ErrInvalidURLRequirement, helpers.URLForMessage(trimmed))
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %s", helpers.ErrInvalidURLRequirement, helpers.URLForMessage(trimmed))
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != schemeHTTPS && scheme != schemeHTTP {
		return nil, "", fmt.Errorf("%w: scheme %q is not supported (https or http)",
			helpers.ErrInvalidURLRequirement, scheme)
	}
	if parsed.Hostname() == "" || !hostPattern.MatchString(hostWithBrackets(parsed)) {
		return nil, "", fmt.Errorf("%w: missing or invalid host in %s",
			helpers.ErrInvalidURLRequirement, helpers.URLForMessage(trimmed))
	}
	return parsed, scheme, nil
}

// checkSourcePath judges a requirement URL's path. The plain form follows
// checkPath below. The one structural form it admits beyond that is an
// embedded absolute http(s) URL - the caching-proxy shape
// http://front/<upstream-url>, where a front host reads the rest of the
// request path as the URL it fetches and caches. The segments before the
// embedded URL follow the plain rule, and the embedded URL must itself parse
// as a url source and be spelled byte for byte in its canonical form, so one
// upstream artifact has one spelling, one pin and one credential-match
// reading. Under that rule the only empty segment such a path can carry is
// the embedded scheme's own "//" separator; every other empty segment, and
// every dot segment anywhere, stays refused.
func checkSourcePath(path string) error {
	head, embedded := splitEmbeddedPath(path)
	if embedded == "" {
		return checkPath(path, false)
	}
	if head != "" {
		if err := checkPath(head, true); err != nil {
			return err
		}
	}
	parsed, err := ParseURL(embedded)
	if err != nil {
		return fmt.Errorf("embedded upstream URL: %w", err)
	}
	if parsed.String() != embedded {
		return fmt.Errorf("%w: embedded upstream URL is not in its canonical spelling: %s",
			helpers.ErrInvalidURLRequirement, helpers.URLForMessage(embedded))
	}
	return nil
}

// splitEmbeddedPath splits path at the first embedded absolute http(s) URL,
// returning the leading segments (empty when the URL starts the path) and
// the URL itself, without the "/" that ends the head; the URL half is empty
// when the path carries none. Only the canonical lower-case scheme spelling
// is recognized, and only on a "/" boundary - anything else falls through to
// the plain-segment rule and its refusals.
func splitEmbeddedPath(path string) (string, string) {
	idx := len(path)
	for _, marker := range []string{"/" + schemeHTTPS + "://", "/" + schemeHTTP + "://"} {
		if i := strings.Index(path, marker); i >= 0 && i < idx {
			idx = i
		}
	}
	if idx == len(path) {
		return path, ""
	}
	return path[:idx], path[idx+1:]
}

// checkPath refuses a path this tool will not send or match on: a rune
// outside the conservative alphabet below (which keeps quotes, whitespace,
// control runes and shell metacharacters out of every rendered message and
// out of the credential prefix match; anything else stays expressible
// percent-encoded), and a dot or empty segment (see the package comment). A
// binding prefix reaches this with its trailing slashes already cut, a
// requirement URL as written; both share one rule so the match and the
// request can never read one path differently.
func checkPath(path string, prefix bool) error {
	for _, r := range path {
		if !isPathRune(r) {
			return fmt.Errorf("%w: path carries a character this tool does not send in a URL: %q",
				helpers.ErrInvalidURLRequirement, r)
		}
	}
	if path == "" || (path == "/" && !prefix) {
		return nil
	}
	for segment := range strings.SplitSeq(strings.TrimPrefix(path, "/"), "/") {
		folded := strings.ReplaceAll(strings.ToLower(segment), "%2e", ".")
		if folded == "" || folded == "." || folded == ".." {
			return fmt.Errorf("%w: path carries an empty or dot segment", helpers.ErrInvalidURLRequirement)
		}
	}
	return nil
}

// checkQuery bounds the query string to the path alphabet plus the runes a
// query needs ("&", "?", ";"). A query is served back to exactly one host, so
// the concern is rendering and nothing else; helpers.URLForMessage cuts it
// from every message regardless.
func checkQuery(parsed *url.URL) error {
	for _, r := range parsed.RawQuery {
		if !isPathRune(r) && r != '&' && r != '?' && r != ';' {
			return fmt.Errorf("%w: query carries a character this tool does not send in a URL: %q",
				helpers.ErrInvalidURLRequirement, r)
		}
	}
	return nil
}

// isPathRune is the url-source path alphabet. It extends the git path
// alphabet with the sub-delims real release paths carry - parentheses,
// commas, "=", ":" and "!" - and admits nothing that needs quoting in a
// terminal or a shell.
func isPathRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	case r == '/', r == '.', r == '-', r == '_', r == '~', r == '+', r == '%', r == '@',
		r == '(', r == ')', r == ',', r == '=', r == ':', r == '!':
		return true
	default:
		return false
	}
}

// hostWithBrackets returns the lower-cased host, re-bracketing an IPv6
// literal the way it has to be written back into a URL.
func hostWithBrackets(parsed *url.URL) string {
	host := strings.ToLower(parsed.Hostname())
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}

// explicitPort drops a port equal to the scheme's default so two spellings of
// the same origin compare equal.
func explicitPort(scheme, port string) string {
	if port == defaultPort(scheme) {
		return ""
	}
	return port
}

func defaultPort(scheme string) string {
	switch scheme {
	case schemeHTTPS:
		return "443"
	case schemeHTTP:
		return "80"
	default:
		return ""
	}
}

// String renders the canonical spelling: scheme://host[:port]path[?query].
// A URL never carries userinfo or a fragment, so the result is always safe to
// persist; messages still render it through helpers.URLForMessage, which cuts
// the query.
func (u URL) String() string {
	var b strings.Builder
	b.WriteString(u.Scheme)
	b.WriteString("://")
	b.WriteString(u.Host)
	if u.Port != "" {
		b.WriteByte(':')
		b.WriteString(u.Port)
	}
	b.WriteString(u.Path)
	if u.Query != "" {
		b.WriteByte('?')
		b.WriteString(u.Query)
	}
	return b.String()
}

// String renders the canonical binding spelling: scheme://host[:port][path].
func (p Prefix) String() string {
	var b strings.Builder
	b.WriteString(p.Scheme)
	b.WriteString("://")
	b.WriteString(p.Host)
	if p.Port != "" {
		b.WriteByte(':')
		b.WriteString(p.Port)
	}
	b.WriteString(p.Path)
	return b.String()
}

// Origin is scheme://host:port with the default port filled in, rendered
// exactly as helpers.Origin renders a request URL's origin - an IPv6 literal
// without its brackets - because the credential match compares the two
// byte for byte.
func (p Prefix) Origin() string {
	port := p.Port
	if port == "" {
		port = defaultPort(p.Scheme)
	}
	return p.Scheme + "://" + strings.Trim(p.Host, "[]") + ":" + port
}

// IsLoopback reports whether the host is a loopback address literal or
// "localhost", the one case a plaintext http credential is tolerated for.
// Only a literal counts: a DNS name such as "127.example.com" resolves
// wherever its owner points it, so it earns no exemption.
func (p Prefix) IsLoopback() bool {
	host := strings.Trim(p.Host, "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
