package s3

// This file covers Artifacts.Meta directly: the tri-state contract
// cacheManager.ArtifactStore's own doc comment requires (absent, present
// with a recorded digest, present with no recorded metadata, present with a
// non-hex recorded digest), that Meta's found always equals Has's own result
// for the identical key, and that Has, which shares headArtifact with Meta,
// still costs exactly one HEAD request.

import (
	"bytes"
	"context"
	"net/http"
	"testing"
)

// artifactsMetaTestKey is the artifact cache key every test in this file
// probes, factored out since none of them ever vary it.
const artifactsMetaTestKey = "ns.name-1.0.0.tar.gz"

// testSHA is a canonical 64-char lowercase hex digest, mirroring the
// identically-named constant in internal/cache/local's own artifacts_test.go
// (a different package, so this is a deliberate duplicate rather than a
// shared import).
const testSHA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// assertS3MetaFoundMatchesHas re-probes artifactsMetaTestKey with Has and
// fails the test unless it reports the identical presence metaFound just
// reported - the equality cacheManager.ArtifactStore's own doc comment
// requires between the two methods, and the one dryRunArtifactMeta
// (internal/galaxy/collections) depends on to keep mirroring isCacheHit.
func assertS3MetaFoundMatchesHas(t *testing.T, artifacts *Artifacts, metaFound bool) {
	t.Helper()
	hasFound, err := artifacts.Has(context.Background(), artifactsMetaTestKey)
	if err != nil {
		t.Fatalf("Has error: %v", err)
	}
	if hasFound != metaFound {
		t.Fatalf("Has found=%v, Meta found=%v, want equal", hasFound, metaFound)
	}
}

// TestArtifactsMetaAbsentReportsNotFound proves Meta reports found=false with
// a nil map and a nil error for a key that was never stored - the identical
// outcome Has itself reports for the same key.
func TestArtifactsMetaAbsentReportsNotFound(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := t.Context()
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	meta, found, err := b.artifacts.Meta(ctx, artifactsMetaTestKey)
	if err != nil {
		t.Fatalf("Meta error: %v", err)
	}
	if found {
		t.Fatalf("expected found=false for an absent key, got true (meta=%#v)", meta)
	}
	if meta != nil {
		t.Fatalf("expected a nil meta map for an absent key, got %#v", meta)
	}
	assertS3MetaFoundMatchesHas(t, b.artifacts, found)
}

// TestArtifactsMetaPresentWithValidDigestReturnsIt proves Meta surfaces a
// committed artifact's recorded sha256 exactly as it was written.
func TestArtifactsMetaPresentWithValidDigestReturnsIt(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := t.Context()
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	body := []byte("tarball bytes")
	if err := b.client.putObject(ctx, b.artifacts.objectKey(artifactsMetaTestKey), bytes.NewReader(body), int64(len(body)),
		putObjectAttrs{contentType: "application/gzip", meta: map[string]string{"sha256": testSHA}}, putCondition{}); err != nil {
		t.Fatalf("putObject: %v", err)
	}

	meta, found, err := b.artifacts.Meta(ctx, artifactsMetaTestKey)
	if err != nil {
		t.Fatalf("Meta error: %v", err)
	}
	if !found {
		t.Fatal("expected found=true for a stored key")
	}
	if got := meta["sha256"]; got != testSHA {
		t.Fatalf("expected Meta to report sha256 %q, got %q", testSHA, got)
	}
	assertS3MetaFoundMatchesHas(t, b.artifacts, found)
}

// TestArtifactsMetaPresentWithNoMetadataReportsFoundNilMeta proves Meta still
// reports found=true for a stored object that carries no x-amz-meta-sha256
// header at all - an object written by something other than this package's
// own Commit, which always computes and writes one - while its own meta map
// is nil: "cached, no recorded metadata", exactly the tri-state
// cacheManager.ArtifactStore's own doc comment names.
func TestArtifactsMetaPresentWithNoMetadataReportsFoundNilMeta(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := t.Context()
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	body := []byte("tarball bytes")
	if err := b.client.putObject(ctx, b.artifacts.objectKey(artifactsMetaTestKey), bytes.NewReader(body), int64(len(body)),
		putObjectAttrs{contentType: "application/gzip"}, putCondition{}); err != nil {
		t.Fatalf("putObject: %v", err)
	}

	meta, found, err := b.artifacts.Meta(ctx, artifactsMetaTestKey)
	if err != nil {
		t.Fatalf("Meta error: %v", err)
	}
	if !found {
		t.Fatal("expected found=true for a stored key with no recorded metadata")
	}
	if meta != nil {
		t.Fatalf("expected a nil meta map with no recorded metadata, got %#v", meta)
	}
	assertS3MetaFoundMatchesHas(t, b.artifacts, found)
}

// TestArtifactsMetaPresentWithNonHexDigestReturnsItVerbatim proves Meta
// applies no shape validation of its own: a stored object whose recorded
// sha256 header is not helpers.IsSHA256Hex is still reported found=true with
// that value returned verbatim. Meta's own contract (backend.go) is silent
// on digest shape - it is a metadata store, not a validator - so the shape
// gate belongs to callers: internal/galaxy/collections' dryRunPinVerdict
// checks helpers.IsSHA256Hex itself before ever trusting a recorded digest.
func TestArtifactsMetaPresentWithNonHexDigestReturnsItVerbatim(t *testing.T) {
	t.Parallel()
	b := newTestBackend(t)
	ctx := t.Context()
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	const nonHex = "not-a-hex-digest-zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"
	body := []byte("tarball bytes")
	if err := b.client.putObject(ctx, b.artifacts.objectKey(artifactsMetaTestKey), bytes.NewReader(body), int64(len(body)),
		putObjectAttrs{contentType: "application/gzip", meta: map[string]string{"sha256": nonHex}}, putCondition{}); err != nil {
		t.Fatalf("putObject: %v", err)
	}

	meta, found, err := b.artifacts.Meta(ctx, artifactsMetaTestKey)
	if err != nil {
		t.Fatalf("Meta error: %v", err)
	}
	if !found {
		t.Fatal("expected found=true for a stored key with a non-hex recorded digest")
	}
	if got := meta["sha256"]; got != nonHex {
		t.Fatalf("expected Meta to return the non-hex digest verbatim %q, got %q", nonHex, got)
	}
	assertS3MetaFoundMatchesHas(t, b.artifacts, found)
}

// TestArtifactsHeadArtifactPropagatesNonNotFoundError proves headArtifact's
// non-404 error arm (return nil, false, err) is reached, and its error
// surfaces, through both of its callers: a HEAD that fails with a non-404
// status must make Has and Meta each return a non-nil error, never fold
// silently into found=false the way a genuine miss does. found=false is
// exactly what a mutation collapsing that arm into "return nil, false, nil"
// would still report, so the assertions below check the returned error
// itself, never found alone - a found-only assertion would pass unchanged
// against that mutation and prove nothing about this arm.
//
// The fault is sized to the client's whole retry budget (s3RetryMaxAttempts),
// mirroring TestHeartbeatSurvivesTransientHeadFailures (lock_test.go): a
// smaller fault is absorbed by headObject's own internal retry and
// headArtifact never sees an error at all, so this arm would go untested
// while the test still passed.
func TestArtifactsHeadArtifactPropagatesNonNotFoundError(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	objectKey := b.artifacts.objectKey(artifactsMetaTestKey)

	fake.failNext(objectKey, http.MethodHead, http.StatusInternalServerError, s3RetryMaxAttempts)
	if _, _, err := b.artifacts.Meta(ctx, artifactsMetaTestKey); err == nil {
		t.Fatal("expected Meta to return a non-nil error for a HEAD that exhausted its retries on a non-404 status")
	}

	// Re-armed independently: the call above already spent the rule armed
	// for it, so Has must be shown to propagate the identical failure on its
	// own HEAD rather than by inheriting an already-exhausted rule.
	fake.failNext(objectKey, http.MethodHead, http.StatusInternalServerError, s3RetryMaxAttempts)
	if _, err := b.artifacts.Has(ctx, artifactsMetaTestKey); err == nil {
		t.Fatal("expected Has to return a non-nil error for a HEAD that exhausted its retries on a non-404 status")
	}
}

// TestArtifactsHasSharesHeadArtifactWithMetaOneHeadRequest proves Has costs
// exactly one HEAD request even though it shares headArtifact with Meta
// (internal/cache/s3/artifacts.go) rather than issuing its own independent
// headObject call - the mechanical basis for
// dryRunArtifactMeta's own claim (internal/galaxy/collections/dryrun.go)
// that replacing a dry run's Has call with a Meta call costs the S3 backend
// nothing extra. Both a present and an absent key are checked, since
// headObject's retry policy could plausibly differ between a 200 and a 404
// response.
func TestArtifactsHasSharesHeadArtifactWithMetaOneHeadRequest(t *testing.T) {
	t.Parallel()
	b, fake := newTestBackendAndFake(t)
	ctx := t.Context()
	if err := b.Open(ctx); err != nil {
		t.Fatalf("Open: %v", err)
	}

	const presentKey = "present.tar.gz"
	const absentKey = "absent.tar.gz"
	body := []byte("tarball bytes")
	if err := b.client.putObject(ctx, b.artifacts.objectKey(presentKey), bytes.NewReader(body), int64(len(body)),
		putObjectAttrs{contentType: "application/gzip", meta: map[string]string{"sha256": testSHA}}, putCondition{}); err != nil {
		t.Fatalf("putObject: %v", err)
	}

	found, err := b.artifacts.Has(ctx, presentKey)
	if err != nil {
		t.Fatalf("Has(present) error: %v", err)
	}
	if !found {
		t.Fatal("expected Has(present) = true")
	}
	if got := fake.requestCount(b.artifacts.objectKey(presentKey), http.MethodHead); got != 1 {
		t.Errorf("HEAD request count for the present key = %d, want exactly 1", got)
	}

	found, err = b.artifacts.Has(ctx, absentKey)
	if err != nil {
		t.Fatalf("Has(absent) error: %v", err)
	}
	if found {
		t.Fatal("expected Has(absent) = false")
	}
	if got := fake.requestCount(b.artifacts.objectKey(absentKey), http.MethodHead); got != 1 {
		t.Errorf("HEAD request count for the absent key = %d, want exactly 1", got)
	}
}
