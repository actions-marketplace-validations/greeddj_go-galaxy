package gitsource

import (
	"errors"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

func TestLocatorRoundTrip(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		loc  Locator
		want string
	}{
		{name: "unpinned root", loc: Locator{URL: "https://h/r.git"}, want: "git+https://h/r.git#"},
		{name: "unpinned subdir", loc: Locator{URL: "https://h/r.git", Subdir: "ns/coll"}, want: "git+https://h/r.git#ns/coll"},
		{name: "pinned root", loc: Locator{URL: "https://h/r.git", Commit: testCommit}, want: "git+https://h/r.git#@" + testCommit},
		{
			name: "pinned subdir", loc: Locator{URL: "git@h:o/r.git", Subdir: "coll", Commit: testCommit},
			want: "git+git@h:o/r.git#coll@" + testCommit,
		},
		{
			name: "ssh user in url", loc: Locator{URL: "ssh://git@h:2222/o/r.git", Commit: testCommit},
			want: "git+ssh://git@h:2222/o/r.git#@" + testCommit,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tt.loc.String()
			if got != tt.want {
				t.Fatalf("String() = %q, want %q", got, tt.want)
			}
			if !IsLocator(got) {
				t.Fatalf("IsLocator(%q) = false", got)
			}
			parsed, err := ParseLocator(got)
			if err != nil {
				t.Fatalf("ParseLocator(%q): %v", got, err)
			}
			if parsed != tt.loc {
				t.Fatalf("ParseLocator(%q) = %+v, want %+v", got, parsed, tt.loc)
			}
			if parsed.Pinned() != (tt.loc.Commit != "") {
				t.Fatalf("Pinned() = %t", parsed.Pinned())
			}
		})
	}
}

func TestParseLocatorRefusals(t *testing.T) {
	t.Parallel()
	refused := []string{
		"https://h/r.git",                               // no prefix
		"git+https://h/r.git",                           // no "#"
		"git+HTTPS://h/r.git#",                          // url not canonical
		"git+https://h/r.git#@" + "abc",                 // short commit
		"git+https://h/r.git#@" + testCommit[:39] + "G", // non-hex
		"git+https://h/r.git#../x@" + testCommit,        // unsafe subdir
		"git+https://h/r.git#.git@" + testCommit,        // git dir subdir
		"git+https://user:pw@h/r.git#@" + testCommit,    // credential in url
		"git+file:///srv/r#@" + testCommit,              // refused scheme
	}
	for _, raw := range refused {
		if _, err := ParseLocator(raw); !errors.Is(err, helpers.ErrInvalidGitLocator) {
			t.Fatalf("ParseLocator(%q) error = %v, want ErrInvalidGitLocator", raw, err)
		}
	}
	if IsLocator("https://h/r.git") {
		t.Fatalf("IsLocator accepted a Galaxy base")
	}
}

func TestParseSubdir(t *testing.T) {
	t.Parallel()
	accepted := map[string]string{
		"":             "",
		"/":            "",
		"coll":         "coll",
		"/ns/coll/":    "ns/coll",
		"a.b/c-d/e_f":  "a.b/c-d/e_f",
		"  /x/  ":      "x",
		"plugins/1.0":  "plugins/1.0",
		"UPPER/Case":   "UPPER/Case",
		"dots.in.name": "dots.in.name",
	}
	for raw, want := range accepted {
		got, err := ParseSubdir(raw)
		if err != nil || got != want {
			t.Fatalf("ParseSubdir(%q) = (%q, %v), want %q", raw, got, err, want)
		}
	}
	refused := []string{"..", "a/../b", "./a", "a//b", ".git", "A/.GIT/b", "a@b", `a\b`, "a\x01b", "a\nb"}
	for _, raw := range refused {
		if _, err := ParseSubdir(raw); !errors.Is(err, helpers.ErrInvalidGitSubdir) {
			t.Fatalf("ParseSubdir(%q) error = %v, want ErrInvalidGitSubdir", raw, err)
		}
	}
}

func TestPinKey(t *testing.T) {
	t.Parallel()
	a := PinKey("https://h/r.git", "main", "")
	b := PinKey("https://h/r.git", "main", "sub")
	c := PinKey("https://h/r.git", "dev", "")
	if a == b || a == c || b == c {
		t.Fatalf("pin keys collide: %q %q %q", a, b, c)
	}
}
