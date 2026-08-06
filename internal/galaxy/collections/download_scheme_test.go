package collections

import (
	"errors"
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

// downloadURLAllowedCase is one table entry for TestDownloadURLAllowed.
type downloadURLAllowedCase struct {
	name string
	raw  string
	want bool
}

// downloadURLAllowedCases enumerates what downloadURLAllowed accepts - an
// absolute http or https URL naming a host, whatever the case of its scheme -
// and every shape it refuses: a scheme this pipeline never speaks, a URL whose
// authority is empty, a relative reference carrying no scheme at all, and a
// string url.Parse rejects outright.
func downloadURLAllowedCases() []downloadURLAllowedCase {
	return []downloadURLAllowedCase{
		{name: "https accepted", raw: "https://h/a.tar.gz", want: true},
		{name: "http accepted", raw: "http://h/a.tar.gz", want: true},
		{name: "scheme case is not significant", raw: "HTTPS://H/a.tar.gz", want: true},
		{name: "file scheme refused", raw: nonHTTPDownloadURL, want: false},
		{name: "ftp scheme refused", raw: "ftp://h/a", want: false},
		{name: "relative reference refused", raw: "/local/a.tar.gz", want: false},
		{name: "empty host refused", raw: "https:///a", want: false},
		{
			// A raw control character makes url.Parse fail outright - the same
			// input shape offServerHostGuardCases uses for its own unparseable
			// rows.
			name: "unparseable url refused", raw: "http://exa\x7fmple.com", want: false,
		},
	}
}

// TestDownloadURLAllowed drives the predicate directly over every shape in
// downloadURLAllowedCases.
func TestDownloadURLAllowed(t *testing.T) {
	t.Parallel()

	for _, tt := range downloadURLAllowedCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := downloadURLAllowed(tt.raw); got != tt.want {
				t.Fatalf("downloadURLAllowed(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
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

	// Killing mutation: deleting the downloadURLAllowed call from
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
