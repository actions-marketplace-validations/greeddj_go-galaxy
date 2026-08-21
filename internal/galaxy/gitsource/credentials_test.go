package gitsource

import "testing"

func mustPrefix(t *testing.T, raw string) URL {
	t.Helper()
	u, err := ParsePrefix(raw)
	if err != nil {
		t.Fatalf("ParsePrefix(%q): %v", raw, err)
	}
	return u
}

func mustURL(t *testing.T, raw string) URL {
	t.Helper()
	u, err := ParseURL(raw)
	if err != nil {
		t.Fatalf("ParseURL(%q): %v", raw, err)
	}
	return u
}

func TestMatchCredential(t *testing.T) {
	t.Parallel()
	creds := []Credential{
		{URL: mustPrefix(t, "https://gitlab.example"), Username: "host"},
		{URL: mustPrefix(t, "https://gitlab.example/group"), Username: "group"},
		{URL: mustPrefix(t, "https://gitlab.example:8443"), Username: "port"},
		{URL: mustPrefix(t, "ssh://gitlab.example"), Username: "ssh-host"},
		{URL: mustPrefix(t, "ssh://gitlab.example/teams/x"), Username: "ssh-teams"},
		{URL: mustPrefix(t, "http://127.0.0.1:8080"), Username: "loop"},
	}
	cases := []struct {
		raw  string
		want string
		ok   bool
	}{
		{raw: "https://gitlab.example/other/repo.git", want: "host", ok: true},
		{raw: "https://gitlab.example/group/repo.git", want: "group", ok: true},
		{raw: "https://gitlab.example/group", want: "group", ok: true},
		{raw: "https://gitlab.example/groupie/repo.git", want: "host", ok: true},
		{raw: "https://gitlab.example:8443/a/b", want: "port", ok: true},
		{raw: "https://GitLab.Example/x", want: "host", ok: true},
		{raw: "http://gitlab.example/x", ok: false},
		{raw: "https://github.com/acme/app.git", ok: false},
		{raw: "ssh://git@gitlab.example/a/b.git", want: "ssh-host", ok: true},
		{raw: "ssh://deploy@gitlab.example/teams/x/repo.git", want: "ssh-teams", ok: true},
		{raw: "git@gitlab.example:teams/x/repo.git", want: "ssh-teams", ok: true},
		{raw: "git@gitlab.example:teams/y/repo.git", want: "ssh-host", ok: true},
		{raw: "ssh://git@gitlab.example:2222/a/b.git", ok: false},
		{raw: "http://127.0.0.1:8080/r.git", want: "loop", ok: true},
	}
	for _, tt := range cases {
		got, ok := MatchCredential(mustURL(t, tt.raw), creds)
		if ok != tt.ok || got.Username != tt.want {
			t.Fatalf("MatchCredential(%q) = (%q, %t), want (%q, %t)", tt.raw, got.Username, ok, tt.want, tt.ok)
		}
		if !ok && !got.IsZero() {
			t.Fatalf("MatchCredential(%q) returned a non-zero credential without a match", tt.raw)
		}
	}
	if _, ok := MatchCredential(mustURL(t, "https://gitlab.example/x"), nil); ok {
		t.Fatalf("MatchCredential matched against no bindings")
	}
}
