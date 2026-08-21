package fetch

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// TestGitClientRefusesCrossOriginRedirect proves the git client never issues
// a redirected request to a different origin: the second server sees no
// request at all, so no Authorization header can have reached it.
func TestGitClientRefusesCrossOriginRedirect(t *testing.T) {
	t.Parallel()
	var secondHits int
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondHits++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(second.Close)
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, second.URL+"/elsewhere", http.StatusFound)
	}))
	t.Cleanup(first.Close)

	client := NewGit(5 * time.Second)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, first.URL+"/repo.git/info/refs", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth("ci", "token")
	resp, err := client.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, helpers.ErrGitTransportFailed) {
		t.Fatalf("cross-origin redirect error = %v, want ErrGitTransportFailed", err)
	}
	if secondHits != 0 {
		t.Fatalf("the redirect target received %d requests", secondHits)
	}
}

// TestGitClientFollowsSameOriginRedirect pins the shape that must keep
// working: a redirect within the origin is followed, without a Referer.
func TestGitClientFollowsSameOriginRedirect(t *testing.T) {
	t.Parallel()
	var landed bool
	var referer string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repo/info/refs" {
			http.Redirect(w, r, srv.URL+"/repo.git/info/refs", http.StatusMovedPermanently)
			return
		}
		landed = true
		referer = r.Header.Get("Referer")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	client := NewGit(5 * time.Second)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/repo/info/refs?service=git-upload-pack", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("same-origin redirect: %v", err)
	}
	_ = resp.Body.Close()
	if !landed || referer != "" {
		t.Fatalf("landed=%t referer=%q, want landed with no Referer", landed, referer)
	}
}

// TestCheckGitRedirectPortChangeIsCrossOrigin pins that a port change alone
// counts as leaving the origin.
func TestCheckGitRedirectPortChangeIsCrossOrigin(t *testing.T) {
	t.Parallel()
	first, _ := url.Parse("https://h.example/r")
	next, _ := url.Parse("https://h.example:8443/r")
	err := checkGitRedirect(&http.Request{URL: next, Header: http.Header{}}, []*http.Request{{URL: first}})
	if !errors.Is(err, helpers.ErrGitTransportFailed) {
		t.Fatalf("port change error = %v, want ErrGitTransportFailed", err)
	}
	same, _ := url.Parse("https://h.example/r.git/info/refs")
	if err := checkGitRedirect(&http.Request{URL: same, Header: http.Header{}}, []*http.Request{{URL: first}}); err != nil {
		t.Fatalf("same origin refused: %v", err)
	}
}

// TestGitClientCapsAnErrorBody proves a non-2xx body is cut at the cap while
// a success body streams whole: go-git reads an error body to the end, and a
// hostile remote must not be able to make that read unbounded.
func TestGitClientCapsAnErrorBody(t *testing.T) {
	t.Parallel()
	big := bytes.Repeat([]byte("x"), int(helpers.GitErrorBodyMaxSize)*3)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ok" {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusInternalServerError)
		}
		_, _ = w.Write(big)
	}))
	t.Cleanup(srv.Close)
	client := NewGit(5 * time.Second)
	for path, want := range map[string]int{"/ok": len(big), "/fail": int(helpers.GitErrorBodyMaxSize)} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("Do(%s): %v", path, err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil || len(body) != want {
			t.Fatalf("%s: read %d bytes (err %v), want %d", path, len(body), err, want)
		}
	}
}
