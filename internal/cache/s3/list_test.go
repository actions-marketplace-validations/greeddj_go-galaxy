package s3

import (
	"bytes"
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// TestListObjectsRejectsOversizedResponse proves listObjectsPage's read of a
// ListObjectsV2 XML page is bounded by helpers.S3ListMaxSize: a response that
// streams well past the cap must fail with helpers.ErrArtifactTooLarge, and
// the endpoint must receive exactly one request, proving the failure is
// terminal (via s3Retryable's existing ErrArtifactTooLarge arm) rather than
// retried.
func TestListObjectsRejectsOversizedResponse(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()

	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}
	fake.oversizedList = true

	_, err := b.client.listObjects(ctx, b.key(artifactsPrefix))
	if !errors.Is(err, helpers.ErrArtifactTooLarge) {
		t.Fatalf("listObjects() error = %v, want ErrArtifactTooLarge", err)
	}

	if got := fake.requestCount(bucketListKey, http.MethodGet); got != 1 {
		t.Fatalf("expected exactly 1 request (an over-cap list response is terminal, not retried), got %d", got)
	}
}

// TestListObjectsAcceptsNormalResponse confirms wrapping listObjectsPage's
// XML read in helpers.NewSizeLimitedReader does not disturb an ordinary,
// well-under-cap ListObjectsV2 response: every key put into the fake is
// still returned correctly. TestOpenProbePassesOnConformingBackend already
// drives this same normal list path (via Open's probe cleanup check), so
// this test's narrower purpose is to prove the size cap introduced above is
// a no-op for a realistic multi-object page.
func TestListObjectsAcceptsNormalResponse(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := t.Context()

	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	want := []string{
		b.key(artifactsPrefix, "a.tar.gz"),
		b.key(artifactsPrefix, "b.tar.gz"),
		b.key(artifactsPrefix, "c.tar.gz"),
	}
	for _, key := range want {
		if err := b.client.putObject(ctx, key, bytes.NewReader([]byte("x")), 1, "application/octet-stream", "", nil, false, ""); err != nil {
			t.Fatalf("putObject(%q): %v", key, err)
		}
	}

	got, err := b.client.listObjects(ctx, b.key(artifactsPrefix))
	if err != nil {
		t.Fatalf("listObjects() error = %v, want nil", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("listObjects() = %v, want %v", got, want)
	}
}
