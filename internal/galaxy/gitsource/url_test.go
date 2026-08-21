package gitsource

import (
	"errors"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

type urlCase struct {
	wantErr error
	name    string
	raw     string
	want    string
	origin  string
}

func acceptedURLCases() []urlCase {
	return []urlCase{
		{name: "https", raw: "https://github.com/acme/app.git", want: "https://github.com/acme/app.git", origin: "https://github.com:443"},
		{
			name: "https keeps .git spelling apart", raw: "https://github.com/acme/app",
			want: "https://github.com/acme/app", origin: "https://github.com:443",
		},
		{
			name: "scheme and host lower-cased", raw: "HTTPS://GitHub.COM/Acme/App.git",
			want: "https://github.com/Acme/App.git", origin: "https://github.com:443",
		},
		{name: "default port dropped", raw: "https://h.example:443/r", want: "https://h.example/r", origin: "https://h.example:443"},
		{name: "explicit port kept", raw: "https://h.example:8443/r", want: "https://h.example:8443/r", origin: "https://h.example:8443"},
		{name: "http loopback", raw: "http://127.0.0.1:8080/r.git", want: "http://127.0.0.1:8080/r.git", origin: "http://127.0.0.1:8080"},
		{
			name: "ssh url", raw: "ssh://git@gitlab.example/group/repo.git",
			want: "ssh://git@gitlab.example/group/repo.git", origin: "ssh://gitlab.example:22",
		},
		{
			name: "ssh url with port", raw: "ssh://git@gitlab.example:2222/group/repo.git",
			want: "ssh://git@gitlab.example:2222/group/repo.git", origin: "ssh://gitlab.example:2222",
		},
		{name: "scp-like", raw: "git@github.com:acme/app.git", want: "git@github.com:acme/app.git", origin: "ssh://github.com:22"},
		{
			name: "scp-like absolute path", raw: "git@host.example:/srv/git/app.git",
			want: "git@host.example:/srv/git/app.git", origin: "ssh://host.example:22",
		},
		{
			name: "scp-like host lower-cased", raw: "git@GitHub.com:acme/app.git",
			want: "git@github.com:acme/app.git", origin: "ssh://github.com:22",
		},
		{name: "ipv6 host", raw: "https://[::1]:9443/r", want: "https://[::1]:9443/r", origin: "https://[::1]:9443"},
		{
			name: "tilde and percent in path", raw: "ssh://git@host.example/~user/repo%20x",
			want: "ssh://git@host.example/~user/repo%20x", origin: "ssh://host.example:22",
		},
	}
}

func refusedURLCases() []urlCase {
	return []urlCase{
		{name: "empty", raw: "", wantErr: helpers.ErrInvalidGitURL},
		{name: "file scheme", raw: "file:///srv/repo", wantErr: helpers.ErrInvalidGitURL},
		{name: "git scheme", raw: "git://host.example/repo", wantErr: helpers.ErrInvalidGitURL},
		{name: "git+ prefix not stripped here", raw: "git+https://host.example/repo", wantErr: helpers.ErrInvalidGitURL},
		{name: "bare path", raw: "/srv/repo", wantErr: helpers.ErrInvalidGitURL},
		{name: "relative path", raw: "./repo", wantErr: helpers.ErrInvalidGitURL},
		{name: "host:path without user", raw: "host.example:acme/app.git", wantErr: helpers.ErrInvalidGitURL},
		{name: "scp with port", raw: "git@host.example:2222:acme/app.git", wantErr: helpers.ErrInvalidGitURL},
		{ //nolint:gosec // a fixture URL, not a credential
			name: "https userinfo", raw: "https://user:token@host.example/repo",
			wantErr: helpers.ErrGitURLUserinfo,
		},
		{name: "https user only", raw: "https://user@host.example/repo", wantErr: helpers.ErrGitURLUserinfo},
		{ //nolint:gosec // a fixture URL, not a credential
			name: "ssh password", raw: "ssh://git:secret@host.example/repo",
			wantErr: helpers.ErrGitURLUserinfo,
		},
		{name: "ssh without user", raw: "ssh://host.example/repo", wantErr: helpers.ErrInvalidGitURL},
		{name: "query", raw: "https://host.example/repo?x=1", wantErr: helpers.ErrInvalidGitURL},
		{name: "fragment", raw: "https://host.example/repo#sub", wantErr: helpers.ErrInvalidGitURL},
		{name: "missing host", raw: "https:///repo", wantErr: helpers.ErrInvalidGitURL},
		{name: "missing path", raw: "https://host.example", wantErr: helpers.ErrInvalidGitURL},
		{name: "root path", raw: "https://host.example/", wantErr: helpers.ErrInvalidGitURL},
		{name: "leading dash segment", raw: "ssh://git@host.example/-upload-pack=x/repo", wantErr: helpers.ErrInvalidGitURL},
		{name: "scp leading dash", raw: "git@host.example:-x/repo", wantErr: helpers.ErrInvalidGitURL},
		{name: "quote in path", raw: "ssh://git@host.example/a'b/repo", wantErr: helpers.ErrInvalidGitURL},
		{name: "semicolon in path", raw: "https://host.example/a;b", wantErr: helpers.ErrInvalidGitURL},
		{name: "space in scp path", raw: "git@host.example:acme/my repo", wantErr: helpers.ErrInvalidGitURL},
		{name: "backslash in path", raw: `git@host.example:acme\app`, wantErr: helpers.ErrInvalidGitURL},
		{name: "control rune in path", raw: "https://host.example/a\x01b", wantErr: helpers.ErrInvalidGitURL},
		{name: "parent segment", raw: "https://host.example/org/../other/repo.git", wantErr: helpers.ErrInvalidGitURL},
		{name: "encoded parent segment", raw: "https://host.example/org/%2e%2e/other/repo.git", wantErr: helpers.ErrInvalidGitURL},
		{name: "scp parent segment", raw: "git@host.example:org/../other/repo.git", wantErr: helpers.ErrInvalidGitURL},
		{name: "dot segment", raw: "https://host.example/./repo.git", wantErr: helpers.ErrInvalidGitURL},
		{name: "empty segment", raw: "https://host.example/org//repo.git", wantErr: helpers.ErrInvalidGitURL},
	}
}

func TestParseURL(t *testing.T) {
	t.Parallel()
	for _, tt := range acceptedURLCases() {
		t.Run("accept "+tt.name, func(t *testing.T) {
			t.Parallel()
			checkAcceptedURL(t, tt)
		})
	}
	for _, tt := range refusedURLCases() {
		t.Run("refuse "+tt.name, func(t *testing.T) {
			t.Parallel()
			checkRefusedURL(t, tt)
		})
	}
}

// checkAcceptedURL asserts the canonical spelling, the origin, and that the
// canonical form parses back to the same value.
func checkAcceptedURL(t *testing.T, tt urlCase) {
	t.Helper()
	got, err := ParseURL(tt.raw)
	if err != nil {
		t.Fatalf("ParseURL(%q): %v", tt.raw, err)
	}
	if got.String() != tt.want {
		t.Fatalf("String() = %q, want %q", got.String(), tt.want)
	}
	if got.Origin() != tt.origin {
		t.Fatalf("Origin() = %q, want %q", got.Origin(), tt.origin)
	}
	again, err := ParseURL(got.String())
	if err != nil || again != got {
		t.Fatalf("canonical form does not round-trip: %+v vs %+v (%v)", again, got, err)
	}
}

// checkRefusedURL asserts the sentinel and that no credential from the input
// leaks into the message.
func checkRefusedURL(t *testing.T, tt urlCase) {
	t.Helper()
	_, err := ParseURL(tt.raw)
	if !errors.Is(err, tt.wantErr) {
		t.Fatalf("ParseURL(%q) error = %v, want %v", tt.raw, err, tt.wantErr)
	}
	if strings.Contains(err.Error(), "token") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("error message echoes a credential: %q", err.Error())
	}
}

func TestParsePrefix(t *testing.T) {
	t.Parallel()
	accepted := map[string]string{
		"https://gitlab.example":             "https://gitlab.example",
		"https://gitlab.example/":            "https://gitlab.example",
		"https://gitlab.example/group/":      "https://gitlab.example/group",
		"ssh://gitlab.example":               "ssh://gitlab.example",
		"ssh://gitlab.example:2222/group":    "ssh://gitlab.example:2222/group",
		"http://127.0.0.1:8080":              "http://127.0.0.1:8080",
		"HTTPS://GitLab.Example/Group/Sub/":  "https://gitlab.example/Group/Sub",
		"https://[2001:db8::1]:8443/org":     "https://[2001:db8::1]:8443/org",
		"ssh://git.example.internal/teams/x": "ssh://git.example.internal/teams/x",
	}
	for raw, want := range accepted {
		got, err := ParsePrefix(raw)
		if err != nil {
			t.Fatalf("ParsePrefix(%q): %v", raw, err)
		}
		if got.String() != want {
			t.Fatalf("ParsePrefix(%q).String() = %q, want %q", raw, got.String(), want)
		}
	}
	refused := []string{
		"",
		"gitlab.example",
		"git@gitlab.example:group",
		"ssh://git@gitlab.example",
		"https://user:pw@gitlab.example",
		"https://gitlab.example?x=1",
		"file:///srv",
	}
	for _, raw := range refused {
		if _, err := ParsePrefix(raw); err == nil {
			t.Fatalf("ParsePrefix(%q) accepted, want refusal", raw)
		}
	}
}

func TestIsLoopback(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]bool{
		"http://127.0.0.1/r":  true,
		"http://localhost/r":  true,
		"http://[::1]:8080/r": true,
		"http://10.0.0.1/r":   false,
		"http://h.example/r":  false,
	} {
		u, err := ParseURL(raw)
		if err != nil {
			t.Fatalf("ParseURL(%q): %v", raw, err)
		}
		if u.IsLoopback() != want {
			t.Fatalf("IsLoopback(%q) = %t, want %t", raw, u.IsLoopback(), want)
		}
	}
}

type splitCase struct {
	name       string
	spec       string
	version    string
	wantURL    string
	wantRef    string
	wantSubdir string
}

func splitCases() []splitCase {
	return []splitCase{
		{name: "plain", spec: "https://h/r.git", version: "", wantURL: "https://h/r.git", wantRef: "HEAD"},
		{name: "star version is HEAD", spec: "https://h/r.git", version: "*", wantURL: "https://h/r.git", wantRef: "HEAD"},
		{name: "version key", spec: "https://h/r.git", version: "main", wantURL: "https://h/r.git", wantRef: "main"},
		{name: "git+ stripped", spec: "git+https://h/r.git", version: "v1", wantURL: "https://h/r.git", wantRef: "v1"},
		{name: "GIT+ stripped case-insensitively", spec: "GIT+ssh://git@h/r.git", version: "", wantURL: "ssh://git@h/r.git", wantRef: "HEAD"},
		{name: "comma suffix wins over version", spec: "https://h/r.git,v2", version: "main", wantURL: "https://h/r.git", wantRef: "v2"},
		{name: "empty comma suffix stays empty", spec: "https://h/r.git,", version: "main", wantURL: "https://h/r.git", wantRef: ""},
		{
			name: "fragment is subdir", spec: "https://h/r.git#/ns/coll/", version: "",
			wantURL: "https://h/r.git", wantRef: "HEAD", wantSubdir: "ns/coll",
		},
		{name: "subdir then ref", spec: "https://h/r.git#sub,main", version: "", wantURL: "https://h/r.git", wantRef: "main", wantSubdir: "sub"},
		{
			name: "ref then subdir is ansible's failure shape", spec: "https://h/r.git,main#sub", version: "",
			wantURL: "https://h/r.git", wantRef: "main#sub",
		},
		{name: "scp-like", spec: "git@h:o/r.git", version: "", wantURL: "git@h:o/r.git", wantRef: "HEAD"},
		{name: "whitespace trimmed", spec: "  https://h/r.git  ", version: " main ", wantURL: "https://h/r.git", wantRef: "main"},
	}
}

func TestSplitSCM(t *testing.T) {
	t.Parallel()
	for _, tt := range splitCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotURL, gotRef, gotSubdir := SplitSCM(tt.spec, tt.version)
			if gotURL != tt.wantURL || gotRef != tt.wantRef || gotSubdir != tt.wantSubdir {
				t.Fatalf("SplitSCM(%q, %q) = (%q, %q, %q), want (%q, %q, %q)",
					tt.spec, tt.version, gotURL, gotRef, gotSubdir, tt.wantURL, tt.wantRef, tt.wantSubdir)
			}
		})
	}
}

func TestIsPointer(t *testing.T) {
	t.Parallel()
	cases := []struct {
		value string
		want  bool
	}{
		{value: "git+https://h/r", want: true},
		{value: "GIT+SSH://git@h/r", want: true},
		{value: "git@h:o/r.git", want: true},
		{value: " git@h:o/r.git", want: true},
		{value: "https://h/r.git", want: false},
		{value: "acme.app", want: false},
		{value: "gitlab.example:o/r", want: false},
		{value: "github.com/acme/app", want: false},
	}
	for _, tt := range cases {
		if IsPointer(tt.value) != tt.want {
			t.Fatalf("IsPointer(%q) = %t, want %t", tt.value, !tt.want, tt.want)
		}
	}
}
