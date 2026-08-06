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

// TestOpenProbeFailsWithoutCompareAndSwap covers the second conditional write
// the lock protocol needs, in the three ways a backend can fail to provide it.
// All three leave the backend not-opened, exactly as the create-if-absent
// probe's own failure does, so none can be mistaken for a usable backend.
//
// The rows are three distinct defects, and each is caught by a different one of
// the probe's three checks - which is why the probe has three:
//
//   - a backend that ignores If-Match answers the swap with success, so two
//     reclaimers would both take an expired lock and both believe they hold it.
//     The arbitration is silently absent, and the stale-ETag check is what sees
//     it.
//   - a backend that never names a version answers reads without an ETag, so
//     there is nothing to condition a swap on at all. The protocol refuses
//     rather than downgrading to an unconditional write, and the ETag check is
//     what sees it.
//   - a backend that refuses EVERY If-Match, matching or not, passes both
//     checks above - nothing about it looks permissive - and would then fail
//     every reclaim at runtime, turning one dead holder's lock object into a
//     permanent one. Only the probe's positive half, the swap against the
//     current ETag, sees it.
//
// TestOpenProbePassesOnConformingBackend is the positive control for all three:
// Open against a conforming fake reaches the same checks and passes them, so a
// failure here cannot be the probe refusing every backend alike.
//
// KILLING MUTATIONS, run and reverted. Deleting probeCompareAndSwap's call from
// probeConditionalPut - the whole swap probe - fails all three rows identically
// (one line, wrapped here):
//
//	probe_test.go:123: Open error = <nil>, want one matching cache backend cannot be used as
//	configured: s3 backend does not support compare-and-swap PUT (If-Match against an ETag);
//	an expired lock holder cannot be reclaimed safely
//
// Dropping only the positive half of the probe, the swap against the current
// ETag, fails the refuse-everything row alone, with the same message. The other
// two rows still pass, which is the evidence that the third check is not
// redundant with the two before it: an earlier revision of this test omitted
// that row, and dropping the positive half then failed nothing at all.
func TestOpenProbeFailsWithoutCompareAndSwap(t *testing.T) {
	t.Parallel()

	tests := []struct {
		arm  func(*fakeS3)
		name string
	}{
		{
			name: "a backend that accepts a swap it should have refused",
			arm:  func(f *fakeS3) { f.ignoreIfMatch = true },
		},
		{
			name: "a backend that never names an object's version",
			arm:  func(f *fakeS3) { f.suppressETag = true },
		},
		{
			name: "a backend that refuses every swap alike",
			arm:  func(f *fakeS3) { f.refuseIfMatch = true },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := newFakeS3()
			tc.arm(fake)
			b := newTestBackendWithFake(t, fake)

			err := b.Open(context.Background())
			if !errors.Is(err, errS3CompareAndSwapUnsupported) {
				t.Fatalf("Open error = %v, want one matching %v", err, errS3CompareAndSwapUnsupported)
			}
			if b.client != nil {
				t.Fatalf("expected the backend to be left in the not-opened state (b.client == nil) after a failed probe")
			}
		})
	}
}
