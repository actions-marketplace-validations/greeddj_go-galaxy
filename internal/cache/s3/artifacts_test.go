package s3

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
