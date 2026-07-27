package helpers

import (
	"net/url"
	"strings"
)

// Origin returns the normalized network origin of u: lower(scheme) + "://"
// + lower(hostname) + ":" + port, where hostname is u.Hostname() (which
// already strips IPv6 brackets) and port is u.Port() when explicit, or the
// scheme's well-known default (443 for https, 80 for http, "" otherwise)
// when it is not. Two URLs that differ only in case, in an explicit vs.
// implicit default port, or in path/query/fragment compare equal under
// Origin - they are the same endpoint for TLS and auth purposes, which is
// exactly the granularity a transport (connection reuse, certificate
// verification, credential attachment) cares about.
//
// This lives in internal/galaxy/helpers rather than internal/galaxy/config
// because internal/galaxy/fetch also needs it (to map an actual request URL
// onto a configured server's origin) and fetch must not import config;
// helpers has no dependencies and is already imported by both.
func Origin(u *url.URL) string {
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == "" {
		port = defaultPortForScheme(scheme)
	}
	return scheme + "://" + host + ":" + port
}

// defaultPortForScheme returns the well-known port implied by scheme when a
// URL omits an explicit one, so "https://h" and "https://h:443" normalize
// to the same Origin. An unrecognized scheme yields "", which still keeps
// Origin's output well-formed and stable, just not collapsed with any
// explicit-port variant (there being no well-known default to collapse to).
func defaultPortForScheme(scheme string) string {
	switch scheme {
	case "https":
		return "443"
	case "http":
		return "80"
	default:
		return ""
	}
}
