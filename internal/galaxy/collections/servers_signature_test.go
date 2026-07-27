package collections

import (
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
)

// hubURL and publicURL are two distinct configured server URLs, enough to
// exercise list order and membership without standing a server up.
const (
	hubURL    = "https://hub.example.com/api/automation-hub"
	publicURL = "https://galaxy.ansible.com"
)

// twoServerConfig builds a config whose server list is the given entries, in
// order. Every signature test differs only in that list.
func twoServerConfig(servers ...config.Server) *config.Config {
	cfg := &config.Config{Servers: servers}
	if len(servers) > 0 {
		cfg.Server = servers[0].URL
	}
	return cfg
}

// TestServersSignatureChangesOnReorder asserts list order is part of the
// signature. Under first-match ownership the order decides which server owns
// a collection, so the same two servers in the other order are a different
// resolution problem whose answer must not be reused.
func TestServersSignatureChangesOnReorder(t *testing.T) {
	t.Parallel()

	forward := serversSignature(twoServerConfig(
		config.Server{ID: "hub", URL: hubURL},
		config.Server{ID: "public", URL: publicURL},
	))
	reversed := serversSignature(twoServerConfig(
		config.Server{ID: "public", URL: publicURL},
		config.Server{ID: "hub", URL: hubURL},
	))

	if forward == reversed {
		t.Fatalf("expected a reordered server list to change the signature, both got %q", forward)
	}
}

// TestServersSignatureIgnoresTokenValue asserts a rotated token leaves the
// signature untouched: only whether a credential exists is hashed, never the
// credential. The signature is persisted in the snapshot, and no
// token-derived value may ever land there.
func TestServersSignatureIgnoresTokenValue(t *testing.T) {
	t.Parallel()

	first := serversSignature(twoServerConfig(
		config.Server{ID: "hub", URL: hubURL, Token: config.NewSecret("first-token")},
	))
	rotated := serversSignature(twoServerConfig(
		config.Server{ID: "hub", URL: hubURL, Token: config.NewSecret("second-token-entirely-different")},
	))

	if first != rotated {
		t.Fatalf("expected a rotated token to leave the signature unchanged: %q != %q", first, rotated)
	}
}

// TestServersSignatureChangesWhenTokenAppears asserts the one credential
// transition that does matter: an anonymous server gaining a token can start
// revealing collections an anonymous read could not see, so a prior
// resolution must not be reused across it.
func TestServersSignatureChangesWhenTokenAppears(t *testing.T) {
	t.Parallel()

	anonymous := serversSignature(twoServerConfig(config.Server{ID: "hub", URL: hubURL}))
	authenticated := serversSignature(twoServerConfig(
		config.Server{ID: "hub", URL: hubURL, Token: config.NewSecret("t")},
	))

	if anonymous == authenticated {
		t.Fatalf("expected adding a token to change the signature, both got %q", anonymous)
	}
}

// TestServersSignatureChangesOnAppendedServer asserts a configured but
// seemingly unused server still changes the signature. Whether a server is
// unused is not knowable without resolving, so this is deliberately
// conservative: the cost is one cold resolve, and nobody has to reason about
// which additions could matter.
func TestServersSignatureChangesOnAppendedServer(t *testing.T) {
	t.Parallel()

	single := serversSignature(twoServerConfig(config.Server{ID: "hub", URL: hubURL}))
	appended := serversSignature(twoServerConfig(
		config.Server{ID: "hub", URL: hubURL},
		config.Server{ID: "public", URL: publicURL},
	))

	if single == appended {
		t.Fatalf("expected an appended server to change the signature, both got %q", single)
	}
}

// TestServersSignatureFallsBackToSingleServer asserts a config with no
// Servers slice - the shape every hand-built config and every caller
// predating multi-server support still has - hashes as the one effective
// server it actually resolves against, so such a config keeps a stable
// signature rather than collapsing every distinct server onto one value.
func TestServersSignatureFallsBackToSingleServer(t *testing.T) {
	t.Parallel()

	legacy := serversSignature(&config.Config{Server: hubURL})
	explicit := serversSignature(twoServerConfig(config.Server{URL: hubURL}))
	if legacy != explicit {
		t.Fatalf("expected a Servers-less config to hash as its single effective server: %q != %q", legacy, explicit)
	}

	other := serversSignature(&config.Config{Server: publicURL})
	if legacy == other {
		t.Fatalf("expected two different single servers to hash differently, both got %q", legacy)
	}

	if got := serversSignature(nil); got != "" {
		t.Fatalf("serversSignature(nil) = %q, want the empty string", got)
	}
}

// TestServersSignatureIsPipeFree asserts the property the whole
// fixed-position header scheme rests on: the value fed into the "servers="
// header is hex, so it can never contain the "|" that separates a per-root
// line's five fields, and therefore can never be mistaken for one.
func TestServersSignatureIsPipeFree(t *testing.T) {
	t.Parallel()

	sig := serversSignature(twoServerConfig(
		config.Server{ID: "hub", URL: hubURL + "/weird|path", Token: config.NewSecret("a|b")},
		config.Server{ID: "public", URL: publicURL},
	))

	if strings.Contains(sig, "|") {
		t.Fatalf("serversSignature returned %q, which contains a %q separator", sig, "|")
	}
	if sig == "" {
		t.Fatal("serversSignature returned the empty string for a populated list")
	}
}

// TestRequirementsSignatureFoldsInServerList asserts the header actually
// reaches the requirements signature: the same requirements against a
// different effective server list must not reuse each other's resolution.
func TestRequirementsSignatureFoldsInServerList(t *testing.T) {
	t.Parallel()

	spec := buildRequirementsSpec([]collection{
		{Namespace: "acme", Name: "app", Constraint: ">=1.0.0"},
	})

	hubFirst := serversSignature(twoServerConfig(
		config.Server{ID: "hub", URL: hubURL},
		config.Server{ID: "public", URL: publicURL},
	))
	publicFirst := serversSignature(twoServerConfig(
		config.Server{ID: "public", URL: publicURL},
		config.Server{ID: "hub", URL: hubURL},
	))

	withHubFirst := requirementsSignatureFromSpec(spec, false, hubFirst)
	if withHubFirst == requirementsSignatureFromSpec(spec, false, publicFirst) {
		t.Fatal("expected the requirements signature to change with the effective server list")
	}
	if withHubFirst != requirementsSignatureFromSpec(spec, false, hubFirst) {
		t.Fatal("requirements signature is not deterministic across repeated calls")
	}
	// The --no-deps header must stay independently significant: the two
	// fixed-position headers cannot mask one another.
	if withHubFirst == requirementsSignatureFromSpec(spec, true, hubFirst) {
		t.Fatal("expected --no-deps to remain significant alongside the server-list header")
	}
}
