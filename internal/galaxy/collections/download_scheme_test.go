package collections

import (
	"errors"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/cache/local"
	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/psvmcc/hub/pkg/types"
)

// nonHTTPDownloadURL is the refused download_url both validateDownloadInputs
// tests below drive: a file: URL is the shape a poisoned snapshot would take
// to aim an artifact fetch at the local filesystem instead of at a server.
const nonHTTPDownloadURL = "file:///etc/passwd"

// userinfoDownloadURL is the refused download_url the userinfo tests drive: a
// perfectly fetchable https URL carrying a credential a server, not the
// operator, chose.
// #nosec G101 -- test fixture literal, not a real credential
const userinfoDownloadURL = "https://u:p@h/a.tar.gz"

// checkDownloadURLCase is one table entry for TestCheckDownloadURL. wantErr is
// the sentinel the case must carry, or nil for a URL that must be accepted.
type checkDownloadURLCase struct {
	wantErr error
	name    string
	raw     string
}

// checkDownloadURLCases enumerates what checkDownloadURL accepts - an
// absolute http or https URL naming a host and carrying no userinfo, whatever
// the case of its scheme - and every shape it refuses: a scheme this pipeline
// never speaks, a URL whose authority is empty, a relative reference carrying
// no scheme at all, a string url.Parse rejects outright, and a URL embedding a
// credential either as a user:password pair or as a bare username.
//
// Two rows carry the whole order argument between them. "ftp scheme refused
// with userinfo" is an otherwise-unfetchable URL that also embeds a
// credential, and it must report the scheme: the userinfo sentinel claims on
// itself that it only ever names a URL that was otherwise perfectly fetchable,
// and that claim is only true while the scheme and host are judged first.
// "at sign in the path accepted" is the negative control for the other
// direction: "@" in a path is ordinary, so a check written as "contains @"
// rather than as url.Parse's own view of the authority would refuse it.
func checkDownloadURLCases() []checkDownloadURLCase {
	return []checkDownloadURLCase{
		{name: "https accepted", raw: "https://h/a.tar.gz", wantErr: nil},
		{name: "http accepted", raw: "http://h/a.tar.gz", wantErr: nil},
		{name: "scheme case is not significant", raw: "HTTPS://H/a.tar.gz", wantErr: nil},
		{name: "at sign in the path accepted", raw: "https://h/x@y.tar.gz", wantErr: nil},
		{name: "file scheme refused", raw: nonHTTPDownloadURL, wantErr: helpers.ErrUnsupportedDownloadURLScheme},
		{name: "ftp scheme refused", raw: "ftp://h/a", wantErr: helpers.ErrUnsupportedDownloadURLScheme},
		// #nosec G101 -- test fixture literal, not a real credential
		{name: "ftp scheme refused with userinfo", raw: "ftp://u:p@h/x", wantErr: helpers.ErrUnsupportedDownloadURLScheme},
		{name: "relative reference refused", raw: "/local/a.tar.gz", wantErr: helpers.ErrUnsupportedDownloadURLScheme},
		{name: "empty host refused", raw: "https:///a", wantErr: helpers.ErrUnsupportedDownloadURLScheme},
		{
			// A raw control character makes url.Parse fail outright - the same
			// input shape offServerHostGuardCases uses for its own unparseable
			// rows.
			name: "unparseable url refused", raw: "http://exa\x7fmple.com", wantErr: helpers.ErrUnsupportedDownloadURLScheme,
		},
		{name: "userinfo pair refused", raw: userinfoDownloadURL, wantErr: helpers.ErrDownloadURLUserinfo},
		{name: "bare username refused", raw: "https://u@h/a.tar.gz", wantErr: helpers.ErrDownloadURLUserinfo},
	}
}

// TestCheckDownloadURL drives the check directly over every shape in
// checkDownloadURLCases.
func TestCheckDownloadURL(t *testing.T) {
	t.Parallel()

	for _, tt := range checkDownloadURLCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := checkDownloadURL(tt.raw)
			if tt.wantErr == nil {
				if got != nil {
					t.Fatalf("checkDownloadURL(%q) = %v, want nil", tt.raw, got)
				}
				return
			}
			if !errors.Is(got, tt.wantErr) {
				t.Fatalf("checkDownloadURL(%q) = %v, want %v", tt.raw, got, tt.wantErr)
			}
		})
	}
}

// TestCheckDownloadURLDoesNotEchoCredential proves the refusal names the
// offending server without reproducing what the server smuggled into the URL.
// The three negative checks and the positive one are independent t.Errorf
// calls rather than a t.Fatalf chain on purpose: each has to be reachable on
// its own, since a message that leaks the password and a message that names no
// host at all are different defects with different repairs, and a chain would
// only ever report whichever came first.
//
// The positive check is what keeps the three negative ones honest: a refusal
// that rendered nothing at all would satisfy them and would be useless to an
// operator, who needs to know which download URL was refused.
//
// Killing mutation, run: rendering raw instead of display in checkDownloadURL's
// userinfo arm fails all four checks - the three negative ones on the value it
// now renders, and the positive one because the whole raw value does not
// contain the cut form that check looks for:
//
//	download_scheme_test.go:128: refusal message contains the password:
//	collection download url must not contain userinfo:
//	"https://u:sup3rsecret@h/a.tar.gz?X-Amz-Signature=deadbeef#frag"
//
// The other three lines label that same rendered value differently.
func TestCheckDownloadURLDoesNotEchoCredential(t *testing.T) {
	t.Parallel()

	// #nosec G101 -- test fixture literal, not a real credential
	const raw = "https://u:sup3rsecret@h/a.tar.gz?X-Amz-Signature=deadbeef#frag"
	err := checkDownloadURL(raw)
	if err == nil {
		t.Fatal("checkDownloadURL accepted a userinfo-bearing URL, want a refusal")
	}
	msg := err.Error()

	if strings.Contains(msg, "sup3rsecret") {
		t.Errorf("refusal message contains the password: %s", msg)
	}
	if strings.Contains(msg, "u:") {
		t.Errorf("refusal message contains the userinfo prefix %q: %s", "u:", msg)
	}
	if strings.Contains(msg, "X-Amz-Signature") {
		t.Errorf("refusal message contains the presigned query: %s", msg)
	}
	if !strings.Contains(msg, "https://h/a.tar.gz") {
		t.Errorf("refusal message does not name the refused URL's origin and path: %s", msg)
	}
}

// downloadInputsFixture builds the cfg and artifact store validateDownloadInputs
// needs to get past its own nil checks, leaving meta.DownloadURL as the only
// input left for it to judge.
func downloadInputsFixture(t *testing.T) (*config.Config, cacheManager.ArtifactStore) {
	t.Helper()
	cacheDir := t.TempDir()
	return &config.Config{CacheDir: cacheDir}, local.NewArtifacts(cacheDir)
}

// TestValidateDownloadInputsRejectsNonHTTPScheme proves the refusal is raised
// by validateDownloadInputs itself - once per artifact acquisition, before any
// request is built - and carries helpers.ErrUnsupportedDownloadURLScheme so
// cmd/go-galaxy/exitcode can classify it. TestValidateDownloadInputsAcceptsHTTPS
// is the positive control on the identical fixture: without it, "it refused"
// would be indistinguishable from a fixture incapable of acceptance.
func TestValidateDownloadInputsRejectsNonHTTPScheme(t *testing.T) {
	t.Parallel()
	cfg, artifacts := downloadInputsFixture(t)

	err := validateDownloadInputs(cfg, artifacts, &types.GalaxyCollectionVersionInfo{DownloadURL: nonHTTPDownloadURL})

	// Killing mutation, run: deleting the checkDownloadURL call from
	// validateDownloadInputs leaves the file: URL accepted and fails this
	// assertion with `validateDownloadInputs("file:///etc/passwd") = <nil>,
	// want helpers.ErrUnsupportedDownloadURLScheme`. The positive control
	// below keeps passing under that mutation, which is what it is for: it
	// shows the fixture reaches an acceptance, so this refusal is the check's
	// doing rather than the fixture's.
	if !errors.Is(err, helpers.ErrUnsupportedDownloadURLScheme) {
		t.Fatalf("validateDownloadInputs(%q) = %v, want helpers.ErrUnsupportedDownloadURLScheme", nonHTTPDownloadURL, err)
	}
}

// TestValidateDownloadInputsRejectsUserinfo proves the userinfo half of the
// same check is reached from validateDownloadInputs too, on the identical
// fixture, and carries its own sentinel rather than the scheme one.
//
// The positive control is inline rather than borrowed from
// TestValidateDownloadInputsAcceptsHTTPS below: it is the same URL with the
// credential deleted and nothing else changed, so an acceptance there proves
// the refusal above is about the userinfo and not about the host, the path or
// the fixture.
//
// Killing mutation, run: deleting the checkDownloadURL call from
// validateDownloadInputs fails this assertion with
//
//	download_scheme_test.go:198: validateDownloadInputs("https://u:p@h/a.tar.gz")
//	= <nil>, want helpers.ErrDownloadURLUserinfo
//
// while the control below keeps passing, which is what shows the refusal is
// the check's doing rather than the fixture's.
func TestValidateDownloadInputsRejectsUserinfo(t *testing.T) {
	t.Parallel()
	cfg, artifacts := downloadInputsFixture(t)

	err := validateDownloadInputs(cfg, artifacts, &types.GalaxyCollectionVersionInfo{DownloadURL: userinfoDownloadURL})
	if !errors.Is(err, helpers.ErrDownloadURLUserinfo) {
		t.Fatalf("validateDownloadInputs(%q) = %v, want helpers.ErrDownloadURLUserinfo", userinfoDownloadURL, err)
	}

	const cleanDownloadURL = "https://h/a.tar.gz"
	if err := validateDownloadInputs(cfg, artifacts, &types.GalaxyCollectionVersionInfo{DownloadURL: cleanDownloadURL}); err != nil {
		t.Fatalf("control: validateDownloadInputs(%q) = %v, want nil", cleanDownloadURL, err)
	}
}

// TestValidateDownloadInputsAcceptsHTTPS is the positive control described on
// TestValidateDownloadInputsRejectsNonHTTPScheme: the same cfg, artifact store
// and metadata shape, differing only in the download URL's scheme, must pass
// validateDownloadInputs cleanly.
func TestValidateDownloadInputsAcceptsHTTPS(t *testing.T) {
	t.Parallel()
	cfg, artifacts := downloadInputsFixture(t)

	meta := &types.GalaxyCollectionVersionInfo{DownloadURL: "https://galaxy.example.invalid/a.tar.gz"}

	if err := validateDownloadInputs(cfg, artifacts, meta); err != nil {
		t.Fatalf("validateDownloadInputs with an https download url = %v, want nil", err)
	}
}
