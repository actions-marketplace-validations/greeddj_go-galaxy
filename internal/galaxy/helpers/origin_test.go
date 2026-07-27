package helpers

import (
	"net/url"
	"testing"
)

// TestOrigin checks the normalization rules documented on Origin: scheme
// and host are lowercased, an implicit default port (443 for https, 80 for
// http) is made explicit so it compares equal to the same port written out
// literally, a non-default port is preserved, a different scheme never
// collapses, and an IPv6 literal loses its brackets.
func TestOrigin(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "https, implicit default port", raw: "https://h", want: "https://h:443"},
		{name: "https, explicit default port", raw: "https://h:443", want: "https://h:443"},
		{name: "uppercase host, explicit port, trailing slash", raw: "https://H:443/", want: "https://h:443"},
		{name: "uppercase scheme", raw: "HTTPS://h", want: "https://h:443"},
		{name: "non-default port does not collapse", raw: "https://h:8443", want: "https://h:8443"},
		{name: "http does not collapse with https", raw: "http://h", want: "http://h:80"},
		{name: "http, implicit default port", raw: "http://h", want: "http://h:80"},
		{name: "http, explicit default port", raw: "http://h:80", want: "http://h:80"},
		{name: "ipv6 literal loses its brackets", raw: "https://[::1]:9000", want: "https://::1:9000"},
		{name: "ipv6 literal, implicit default port", raw: "https://[::1]", want: "https://::1:443"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			u, err := url.Parse(tt.raw)
			if err != nil {
				t.Fatalf("url.Parse(%q) error = %v, want nil", tt.raw, err)
			}
			if got := Origin(u); got != tt.want {
				t.Errorf("Origin(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

// TestOriginCollapsesEquivalentSpellings checks that every URL spelling
// that ought to denote the same endpoint - implicit vs. explicit default
// port, host case, scheme case, and an incidental trailing slash - produces
// the exact same Origin value, not merely values that individually look
// right in isolation.
func TestOriginCollapsesEquivalentSpellings(t *testing.T) {
	t.Parallel()

	equivalents := []string{"https://h", "https://h:443", "https://H:443/", "HTTPS://h"}

	var want string
	for i, raw := range equivalents {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("url.Parse(%q) error = %v, want nil", raw, err)
		}
		got := Origin(u)
		if i == 0 {
			want = got
			continue
		}
		if got != want {
			t.Errorf("Origin(%q) = %q, want %q (same as Origin(%q))", raw, got, want, equivalents[0])
		}
	}

	// https://h:8443 and http://h must each stand apart from that group.
	for _, raw := range []string{"https://h:8443", "http://h"} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("url.Parse(%q) error = %v, want nil", raw, err)
		}
		if got := Origin(u); got == want {
			t.Errorf("Origin(%q) = %q, want it to differ from the https://h group (%q)", raw, got, want)
		}
	}
}
