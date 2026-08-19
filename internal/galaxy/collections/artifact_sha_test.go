package collections

// This file proves resolveArtifactSHA's own guard: a value read from a trust
// boundary (meta.Artifact.Sha256, a cache sidecar's artifactMeta["sha256"])
// is rejected with helpers.ErrMalformedArtifactSHA256 unless it is exactly
// helpers.IsSHA256Hex, while a value this process just computed (artifactSHA,
// or a fresh archive.FileHashSHA256 result) is never subjected to that check
// - and that the four precedence arms (pin, meta hit, sidecar hit,
// fallback hash) still behave when every value in play is well-formed. Each
// case also pins the provenance result: computed is true exactly on the arms
// whose sha this process derived over the bytes at path (artifactSHA passed
// through, either file-hash arm), false on the recorded sources - the bit the
// extracted store's Ensure takes to decide whether ingesting must re-hash the
// tarball.

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

// TestResolveArtifactSHARejectsMalformedMetaSha256 checks that a traversal, a
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

			got, computed, err := resolveArtifactSHA(path, meta, nil, "", "")
			if !errors.Is(err, helpers.ErrMalformedArtifactSHA256) {
				t.Fatalf("resolveArtifactSHA error = %v, want errors.Is helpers.ErrMalformedArtifactSHA256", err)
			}
			if got != "" {
				t.Fatalf("resolveArtifactSHA sha = %q, want empty on rejection", got)
			}
			if computed {
				t.Fatalf("resolveArtifactSHA computed = true, want false on rejection")
			}
		})
	}
}

// TestResolveArtifactSHARejectsMalformedSidecarSha256 checks that the same three
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

			got, computed, err := resolveArtifactSHA(path, nil, artifactMeta, "", "")
			if !errors.Is(err, helpers.ErrMalformedArtifactSHA256) {
				t.Fatalf("resolveArtifactSHA error = %v, want errors.Is helpers.ErrMalformedArtifactSHA256", err)
			}
			if got != "" {
				t.Fatalf("resolveArtifactSHA sha = %q, want empty on rejection", got)
			}
			if computed {
				t.Fatalf("resolveArtifactSHA computed = true, want false on rejection")
			}
		})
	}
}

// TestResolveArtifactSHATrustsOwnComputationUnvalidated pins the
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
	got, computed, err := resolveArtifactSHA(path, meta, nil, notActuallyHex, "")
	if err != nil {
		t.Fatalf("resolveArtifactSHA error = %v, want nil (artifactSHA is never validated)", err)
	}
	if got != notActuallyHex {
		t.Fatalf("resolveArtifactSHA sha = %q, want %q returned unvalidated", got, notActuallyHex)
	}
	if !computed {
		t.Fatalf("resolveArtifactSHA computed = false, want true (artifactSHA is this process's own digest)")
	}
}

// requireResolvedSHA fails unless resolveArtifactSHA returned exactly the
// expected digest and provenance. arm names the precedence arm the
// expectation pins, so a failure identifies the arm rather than a bare bool.
func requireResolvedSHA(t *testing.T, got string, computed bool, err error, wantSHA string, wantComputed bool, arm string) {
	t.Helper()
	if err != nil {
		t.Fatalf("resolveArtifactSHA error = %v (%s)", err, arm)
	}
	if got != wantSHA {
		t.Fatalf("resolveArtifactSHA sha = %q, want %q (%s)", got, wantSHA, arm)
	}
	if computed != wantComputed {
		t.Fatalf("resolveArtifactSHA computed = %v, want %v (%s)", computed, wantComputed, arm)
	}
}

// TestResolveArtifactSHAPrecedence checks that the precedence arms still
// behave when every value in play is well-formed - a process-computed
// artifactSHA wins over everything (a pin included) without touching the
// file, a lockfile pin with no artifactSHA forces a real file hash
// regardless of what meta or the sidecar claim, a well-formed meta hit is
// used directly, a well-formed sidecar hit is used when meta is absent, and
// the fallback hashes the file when nothing else is available.
func TestResolveArtifactSHAPrecedence(t *testing.T) {
	t.Parallel()
	content := []byte("real tarball bytes resolveArtifactSHA must hash on the pin and fallback arms")
	path := mustWriteTarball(t, content)
	realHash := sha256Hex(content)

	t.Run("pin forces a real file hash", func(t *testing.T) {
		t.Parallel()
		meta := &types.GalaxyCollectionVersionInfo{}
		meta.Artifact.Sha256 = validMarkerSHA // must be ignored: a pin always re-hashes the file.
		got, computed, err := resolveArtifactSHA(path, meta, map[string]string{"sha256": validMarkerSHA}, "", realHash)
		requireResolvedSHA(t, got, computed, err, realHash, true, "the pin arm hashes the file itself")
	})

	t.Run("process-computed sha wins over pin without re-reading the file", func(t *testing.T) {
		t.Parallel()
		// The path deliberately names a file that does not exist: a non-empty
		// artifactSHA is already a digest of the fetched bytes, so the pin arm's
		// file hash must never run - if it did, this call would fail on the
		// missing file instead of returning the process-computed sha.
		missing := filepath.Join(t.TempDir(), "never-written.tar.gz")
		got, computed, err := resolveArtifactSHA(missing, nil, nil, validMarkerSHA, realHash)
		requireResolvedSHA(t, got, computed, err, validMarkerSHA, true,
			"a process-computed sha must win without the pin arm re-reading the file")
	})

	t.Run("well-formed meta hit is used directly", func(t *testing.T) {
		t.Parallel()
		meta := &types.GalaxyCollectionVersionInfo{}
		meta.Artifact.Sha256 = validMarkerSHA
		got, computed, err := resolveArtifactSHA(path, meta, nil, "", "")
		requireResolvedSHA(t, got, computed, err, validMarkerSHA, false,
			"a meta hit is a recorded value, not our hash")
	})

	t.Run("well-formed sidecar hit is used when meta is absent", func(t *testing.T) {
		t.Parallel()
		got, computed, err := resolveArtifactSHA(path, nil, map[string]string{"sha256": validMarkerSHA}, "", "")
		requireResolvedSHA(t, got, computed, err, validMarkerSHA, false,
			"a sidecar hit is a recorded value, not our hash")
	})

	t.Run("fallback hashes the file when nothing else is available", func(t *testing.T) {
		t.Parallel()
		got, computed, err := resolveArtifactSHA(path, nil, nil, "", "")
		requireResolvedSHA(t, got, computed, err, realHash, true, "the fallback arm hashes the file itself")
	})
}

// TestPayloadFromPrefetchedMarksSHAComputed proves the prefetch handoff's
// stream-computed hash keeps its provenance: the payload built from a
// handed-off downloadResult reports artifactSHAComputed, which is what lets
// the extracted store ingest the temp without hashing bytes this run already
// hashed while streaming the download. The temp's path deliberately names a
// file that is never written, so any re-read on this path fails loudly
// instead of silently costing the I/O the handoff exists to avoid.
func TestPayloadFromPrefetchedMarksSHAComputed(t *testing.T) {
	t.Parallel()
	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	prefetched := downloadResult{
		Path: filepath.Join(t.TempDir(), "never-written.tar.gz"),
		SHA:  validMarkerSHA,
	}

	payload, servedFromCache, err := payloadFromPrefetched(col, nil, prefetched)
	if err != nil {
		t.Fatalf("payloadFromPrefetched error = %v", err)
	}
	if servedFromCache {
		t.Fatalf("servedFromCache = true, want false (fresh origin bytes, not a cache hit)")
	}
	if payload.artifactSHA != validMarkerSHA {
		t.Fatalf("payload.artifactSHA = %q, want %q", payload.artifactSHA, validMarkerSHA)
	}
	if !payload.artifactSHAComputed {
		t.Fatalf("payload.artifactSHAComputed = false, want true (the handoff sha was hashed from the downloaded stream)")
	}
}
