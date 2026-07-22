package s3

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestOpenProbePassesOnConformingBackend confirms that against a backend
// that correctly enforces If-None-Match, Open succeeds and leaves no trace:
// the probe object (deleted by probeConditionalPut's deferred cleanup) must
// not still be present under the locks prefix afterward. The probe key
// carries a random suffix, so this checks for any leftover key containing
// conditionalProbeObject rather than one exact name.
func TestOpenProbePassesOnConformingBackend(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := context.Background()

	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	keys, err := b.client.listObjects(ctx, b.key(locksPrefix))
	if err != nil {
		t.Fatalf("listObjects: %v", err)
	}
	for _, key := range keys {
		if strings.Contains(key, conditionalProbeObject) {
			t.Fatalf("expected the probe object to be cleaned up, found %q among %v", key, keys)
		}
	}
}

// TestOpenProbeFailsOnNonConformingBackend confirms that against a backend
// that silently ignores If-None-Match and overwrites, Open fails with
// errS3ConditionalPutUnsupported and leaves the backend in the same
// not-opened state as an ensureBucket failure (b.client == nil), so it can
// never be mistaken for a usable, opened backend.
func TestOpenProbeFailsOnNonConformingBackend(t *testing.T) {
	t.Parallel()
	b := newNonConformingTestBackend(t)
	ctx := context.Background()

	err := b.Open(ctx)
	if !errors.Is(err, errS3ConditionalPutUnsupported) {
		t.Fatalf("expected errS3ConditionalPutUnsupported, got %v", err)
	}
	if b.client != nil {
		t.Fatalf("expected the backend to be left in the not-opened state (b.client == nil) after a failed probe")
	}
}
