package collections

// This file proves resolveArtifactSHA's own guard: a value read from a trust
// boundary (meta.Artifact.Sha256, a cache sidecar's artifactMeta["sha256"])
// is rejected with helpers.ErrMalformedArtifactSHA256 unless it is exactly
// helpers.IsSHA256Hex, while a value this process just computed (artifactSHA,
// or a fresh archive.FileHashSHA256 result) is never subjected to that check
// - and that the four existing precedence arms (pin, meta hit, sidecar hit,
// fallback hash) still behave when every value in play is well-formed.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/psvmcc/hub/pkg/types"
)

// traversalArtifactSHA is a path-traversal string in exactly the shape
// resolveArtifactSHA must reject when it arrives via meta.Artifact.Sha256 or
// artifactMeta["sha256"] - the same string TestVerifyExtractMarkerRefusesTraversalSHA
// uses at the marker layer, so a failure here and a failure there are
// provably about the same class of value.
const traversalArtifactSHA = "../../../../../../home/ci/.ssh/authorized_keys"

// mustWriteTarball writes arbitrary bytes to a fresh file under t.TempDir()
// and returns its path, standing in for a downloaded/cached artifact whose
// real on-disk bytes archive.FileHashSHA256 can hash. The content does not
// need to be a valid tar.gz: resolveArtifactSHA only ever hashes it, never
// extracts it.
func mustWriteTarball(t *testing.T, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "artifact.tar.gz")
	if err := os.WriteFile(path, content, helpers.FileMod); err != nil {
		t.Fatalf("write tarball: %v", err)
	}
	return path
}

// TestResolveArtifactSHARejectsMalformedMetaSha256 is T8: a traversal, a
// short-but-hex value, and an uppercase value arriving via
// meta.Artifact.Sha256 - the raw Galaxy API JSON branch - are all rejected
// with helpers.ErrMalformedArtifactSHA256 and an empty resolved sha, never
// silently passed through or falling back to hashing the file.
func TestResolveArtifactSHARejectsMalformedMetaSha256(t *testing.T) {
	t.Parallel()
	path := mustWriteTarball(t, []byte("irrelevant tarball bytes"))

	cases := []struct {
		name string
		sha  string
	}{
		{"traversal", traversalArtifactSHA},
		{"short but hex", "deadbeef"},
		{"uppercase", strings.ToUpper(validMarkerSHA)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			meta := &types.GalaxyCollectionVersionInfo{}
			meta.Artifact.Sha256 = tc.sha

			got, err := resolveArtifactSHA(path, meta, nil, "", "")
			if !errors.Is(err, helpers.ErrMalformedArtifactSHA256) {
				t.Fatalf("resolveArtifactSHA error = %v, want errors.Is helpers.ErrMalformedArtifactSHA256", err)
			}
			if got != "" {
				t.Fatalf("resolveArtifactSHA sha = %q, want empty on rejection", got)
			}
		})
	}
}

// TestResolveArtifactSHARejectsMalformedSidecarSha256 is T9: the same three
// malformed shapes, this time arriving via artifactMeta["sha256"] - the
// cache-sidecar branch - are rejected identically.
func TestResolveArtifactSHARejectsMalformedSidecarSha256(t *testing.T) {
	t.Parallel()
	path := mustWriteTarball(t, []byte("irrelevant tarball bytes"))

	cases := []struct {
		name string
		sha  string
	}{
		{"traversal", traversalArtifactSHA},
		{"short but hex", "deadbeef"},
		{"uppercase", strings.ToUpper(validMarkerSHA)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			artifactMeta := map[string]string{"sha256": tc.sha}

			got, err := resolveArtifactSHA(path, nil, artifactMeta, "", "")
			if !errors.Is(err, helpers.ErrMalformedArtifactSHA256) {
				t.Fatalf("resolveArtifactSHA error = %v, want errors.Is helpers.ErrMalformedArtifactSHA256", err)
			}
			if got != "" {
				t.Fatalf("resolveArtifactSHA sha = %q, want empty on rejection", got)
			}
		})
	}
}

// TestResolveArtifactSHATrustsOwnComputationUnvalidated is T10: it pins the
// "never validate what we just computed" half of the rule against a future
// reviewer who might otherwise "complete" the guard. artifactSHA is this
// process's own hex.EncodeToString(hasher.Sum(nil)) over bytes it just
// streamed, so a non-empty artifactSHA argument must be returned exactly as
// given - including one that is NOT valid hex - without resolveArtifactSHA
// ever inspecting it, since artifactSHA always wins before any other
// argument (including a malformed meta.Artifact.Sha256) is even looked at.
func TestResolveArtifactSHATrustsOwnComputationUnvalidated(t *testing.T) {
	t.Parallel()
	path := mustWriteTarball(t, []byte("irrelevant tarball bytes"))

	meta := &types.GalaxyCollectionVersionInfo{}
	meta.Artifact.Sha256 = traversalArtifactSHA // must never be reached: artifactSHA wins first.

	const notActuallyHex = "this-is-not-hex-at-all-but-must-pass-through-unvalidated"
	got, err := resolveArtifactSHA(path, meta, nil, notActuallyHex, "")
	if err != nil {
		t.Fatalf("resolveArtifactSHA error = %v, want nil (artifactSHA is never validated)", err)
	}
	if got != notActuallyHex {
		t.Fatalf("resolveArtifactSHA sha = %q, want %q returned unvalidated", got, notActuallyHex)
	}
}

// TestResolveArtifactSHAPrecedence is T11: the four existing precedence arms
// still behave when every value in play is well-formed - a lockfile pin
// forces a real file hash regardless of what meta or the sidecar claim, a
// well-formed meta hit is used directly, a well-formed sidecar hit is used
// when meta is absent, and the fallback hashes the file when nothing else is
// available.
func TestResolveArtifactSHAPrecedence(t *testing.T) {
	t.Parallel()
	content := []byte("real tarball bytes resolveArtifactSHA must hash on the pin and fallback arms")
	path := mustWriteTarball(t, content)
	realHash := sha256Hex(content)

	t.Run("pin forces a real file hash", func(t *testing.T) {
		t.Parallel()
		meta := &types.GalaxyCollectionVersionInfo{}
		meta.Artifact.Sha256 = validMarkerSHA // must be ignored: a pin always re-hashes the file.
		got, err := resolveArtifactSHA(path, meta, map[string]string{"sha256": validMarkerSHA}, "", realHash)
		if err != nil {
			t.Fatalf("resolveArtifactSHA error = %v", err)
		}
		if got != realHash {
			t.Fatalf("resolveArtifactSHA sha = %q, want the real file hash %q", got, realHash)
		}
	})

	t.Run("well-formed meta hit is used directly", func(t *testing.T) {
		t.Parallel()
		meta := &types.GalaxyCollectionVersionInfo{}
		meta.Artifact.Sha256 = validMarkerSHA
		got, err := resolveArtifactSHA(path, meta, nil, "", "")
		if err != nil {
			t.Fatalf("resolveArtifactSHA error = %v", err)
		}
		if got != validMarkerSHA {
			t.Fatalf("resolveArtifactSHA sha = %q, want %q", got, validMarkerSHA)
		}
	})

	t.Run("well-formed sidecar hit is used when meta is absent", func(t *testing.T) {
		t.Parallel()
		got, err := resolveArtifactSHA(path, nil, map[string]string{"sha256": validMarkerSHA}, "", "")
		if err != nil {
			t.Fatalf("resolveArtifactSHA error = %v", err)
		}
		if got != validMarkerSHA {
			t.Fatalf("resolveArtifactSHA sha = %q, want %q", got, validMarkerSHA)
		}
	})

	t.Run("fallback hashes the file when nothing else is available", func(t *testing.T) {
		t.Parallel()
		got, err := resolveArtifactSHA(path, nil, nil, "", "")
		if err != nil {
			t.Fatalf("resolveArtifactSHA error = %v", err)
		}
		if got != realHash {
			t.Fatalf("resolveArtifactSHA sha = %q, want the real file hash %q", got, realHash)
		}
	})
}
