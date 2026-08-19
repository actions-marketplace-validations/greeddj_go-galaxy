package s3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// TestVerifyArtifactSHAWrapsBothSentinels proves that a read-time sha256
// mismatch wraps both errArtifactSHA256Mismatch - so any existing match
// against the package-local sentinel still holds - and
// helpers.ErrSHA256Mismatch, so the collections layer can classify the
// failure as recoverable without importing s3-specific error types. It also
// proves a matching sum still passes cleanly.
func TestVerifyArtifactSHAWrapsBothSentinels(t *testing.T) {
	t.Parallel()

	expectedSum := sha256.Sum256([]byte("real artifact bytes"))
	expected := hex.EncodeToString(expectedSum[:])
	wrongSum := sha256.Sum256([]byte("different bytes entirely"))

	err := verifyArtifactSHA(map[string]string{"sha256": expected}, wrongSum[:])
	if err == nil {
		t.Fatalf("expected a mismatch error, got nil")
	}
	if !errors.Is(err, helpers.ErrSHA256Mismatch) {
		t.Errorf("errors.Is(err, helpers.ErrSHA256Mismatch) = false, want true (err: %v)", err)
	}
	if !errors.Is(err, errArtifactSHA256Mismatch) {
		t.Errorf("errors.Is(err, errArtifactSHA256Mismatch) = false, want true (err: %v)", err)
	}

	if err := verifyArtifactSHA(map[string]string{"sha256": expected}, expectedSum[:]); err != nil {
		t.Errorf("matching sum: got %v, want nil", err)
	}
}

// TestVerifyArtifactSHARejectsCaseOnlyDifference proves the comparison is
// exact (==), not strings.EqualFold: an expected digest that differs from
// the actual, real sha256 only by case is rejected as
// helpers.ErrSHA256Mismatch, not silently accepted as an equivalent
// spelling. Lowercase hex is the only shape this program ever writes, so an
// uppercase (or mixed-case) expected value can only mean a non-canonical
// metadata sidecar - exactly the condition the local backend has always
// refused via its own IsSHA256Hex gate, and that a case-insensitive
// comparison here would let through instead.
func TestVerifyArtifactSHARejectsCaseOnlyDifference(t *testing.T) {
	t.Parallel()

	sum := sha256.Sum256([]byte("real artifact bytes"))
	actual := hex.EncodeToString(sum[:])
	upper := strings.ToUpper(actual)

	err := verifyArtifactSHA(map[string]string{"sha256": upper}, sum[:])
	if err == nil {
		t.Fatal("expected a case-only difference to be rejected, got nil")
	}
	if !errors.Is(err, helpers.ErrSHA256Mismatch) {
		t.Errorf("errors.Is(err, helpers.ErrSHA256Mismatch) = false, want true (err: %v)", err)
	}
}

// TestFetchRefusesAnObjectWhoseRecordedDigestDisagreesWithItsBytes proves
// Fetch re-verifies the object's recorded digest against the freshly
// downloaded bytes before returning the file, rather than trusting the
// caller to notice the mismatch itself - the mechanism dryRunPinVerdict's own
// doc comment (internal/galaxy/collections/dryrun.go) cites for the S3
// backend's half of the recorded-digest asymmetry. It also proves the
// refused download leaves no temp file behind under tmpBase, pinning the
// cleanupIfNeeded call on that same refusal arm.
//
// KILLING MUTATION, run and reverted: dropping Fetch's own verifyArtifactSHA
// call, so a downloaded object is returned without its recorded digest ever
// being checked against the bytes, makes this test fail with:
//
//	artifacts_test.go:110: expected Fetch to refuse an object whose recorded
//	digest disagrees with its bytes
//
// Dropping only that same arm's cleanupIfNeeded call, leaving the refusal
// itself intact, instead fails the leftover-temp assertion below it, which is
// what proves that assertion is load-bearing rather than decorative:
//
//	artifacts_test.go:115: expected no leftover temp file under tmpBase after
//	a refused Fetch, found [- .artifact-<random>]
func TestFetchRefusesAnObjectWhoseRecordedDigestDisagreesWithItsBytes(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := t.Context()
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if b.artifacts.tmpBase == "" {
		b.artifacts.tmpBase = t.TempDir()
	}

	const key = "digest-mismatch.tar.gz"
	body := []byte("tarball bytes for a digest mismatch test")
	wrongSum := sha256.Sum256([]byte("some other, unrelated bytes"))
	putArtifactWithRecordedDigest(ctx, t, b, key, body, hex.EncodeToString(wrongSum[:]))

	_, err := b.artifacts.Fetch(ctx, key)
	if err == nil {
		t.Fatal("expected Fetch to refuse an object whose recorded digest disagrees with its bytes")
	}
	if !errors.Is(err, helpers.ErrSHA256Mismatch) {
		t.Errorf("errors.Is(err, helpers.ErrSHA256Mismatch) = false, want true (err: %v)", err)
	}
	assertNoLeftoverTempFiles(t, b.artifacts.tmpBase)

	// Positive control: the identical fixture, with the object's recorded
	// digest matching its own bytes, must be accepted - proving the refusal
	// above is a real refusal of a genuine mismatch, not evidence Fetch can
	// never succeed against this fixture at all.
	correctSum := sha256.Sum256(body)
	putArtifactWithRecordedDigest(ctx, t, b, key, body, hex.EncodeToString(correctSum[:]))

	file, err := b.artifacts.Fetch(ctx, key)
	if err != nil {
		t.Fatalf("Fetch with a matching recorded digest: %v", err)
	}
	got, err := os.ReadFile(file.Path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", file.Path, err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("fetched bytes = %q, want %q", got, body)
	}
	file.Cleanup()
}

// putArtifactWithRecordedDigest stores body under key with a single
// x-amz-meta-sha256 header set to digest, failing the test on any put
// error. Factored out of the digest-mismatch test above solely to keep that
// test's own cyclomatic complexity under the linter's ceiling - it has no
// behavior of its own beyond the one putObject call.
func putArtifactWithRecordedDigest(ctx context.Context, t *testing.T, b *Backend, key string, body []byte, digest string) {
	t.Helper()
	if err := b.client.putObject(ctx, b.artifacts.objectKey(key), bytes.NewReader(body), int64(len(body)),
		putObjectAttrs{contentType: "application/gzip", meta: map[string]string{"sha256": digest}}, putCondition{}); err != nil {
		t.Fatalf("putObject: %v", err)
	}
}

// assertNoLeftoverTempFiles fails the test if tmpBase contains any entry,
// pinning Fetch's cleanupIfNeeded call on its digest-mismatch refusal arm.
func assertNoLeftoverTempFiles(t *testing.T, tmpBase string) {
	t.Helper()
	entries, err := os.ReadDir(tmpBase)
	if err != nil {
		t.Fatalf("ReadDir(tmpBase): %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no leftover temp file under tmpBase after a refused Fetch, found %v", entries)
	}
}
