package s3

import (
	"errors"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// cacheBackendClass names which of the three helpers cache-backend sentinels
// - or none - a given package-private S3 sentinel is expected to carry, per
// variables.go's own partition doc.
type cacheBackendClass int

const (
	classNone cacheBackendClass = iota
	classUnavailable
	classUnusable
	classBusy
)

// sentinelClassCases is every sentinel declared in variables.go's var block,
// listed exhaustively rather than sampled - the same convention
// cmd/go-galaxy/exitcode/exitcode_test.go's galaxyServerConfigSentinels
// establishes (see its own doc comment): each one is a hand-classified fact
// about this package's error vocabulary, and a missed or silently
// reclassified entry would move a failure's exit code with nothing to catch
// it.
//
//nolint:gochecknoglobals // a fixed table consumed by one test, not mutable shared state
var sentinelClassCases = []struct {
	err   error
	name  string
	class cacheBackendClass
}{
	{name: "errS3TransportFailed", err: errS3TransportFailed, class: classUnavailable},
	{name: "errS3BucketNotFound", err: errS3BucketNotFound, class: classUnavailable},
	{name: "errS3BucketHeadFailed", err: errS3BucketHeadFailed, class: classUnavailable},
	{name: "errS3CreateBucketFailed", err: errS3CreateBucketFailed, class: classUnavailable},
	{name: "errS3BucketRequestFailed", err: errS3BucketRequestFailed, class: classUnavailable},
	{name: "errS3GetFailed", err: errS3GetFailed, class: classUnavailable},
	{name: "errS3HeadFailed", err: errS3HeadFailed, class: classUnavailable},
	{name: "errS3PutFailed", err: errS3PutFailed, class: classUnavailable},
	{name: "errS3DeleteFailed", err: errS3DeleteFailed, class: classUnavailable},
	{name: "errS3InvalidEndpoint", err: errS3InvalidEndpoint, class: classUnusable},
	{name: "errS3ConditionalPutUnsupported", err: errS3ConditionalPutUnsupported, class: classUnusable},
	{name: "errS3RedirectRefused", err: errS3RedirectRefused, class: classUnusable},
	{name: "errS3LockWaitTimeout", err: errS3LockWaitTimeout, class: classBusy},
	{name: "errS3LockWaitNoHolderObserved", err: errS3LockWaitNoHolderObserved, class: classUnavailable},
	{name: "errS3LockLost", err: errS3LockLost, class: classNone},
	{name: "errS3TokenGeneration", err: errS3TokenGeneration, class: classNone},
	{name: "errS3NotFound", err: errS3NotFound, class: classNone},
	{name: "errS3BucketEmpty", err: errS3BucketEmpty, class: classNone},
	{name: "errS3PreconditionFailed", err: errS3PreconditionFailed, class: classNone},
	{name: "errS3HTTPClientNil", err: errS3HTTPClientNil, class: classNone},
	{name: "errS3ClientNil", err: errS3ClientNil, class: classNone},
	{name: "errArtifactSHA256Mismatch", err: errArtifactSHA256Mismatch, class: classNone},
}

// TestSentinelClassPartitionIsExhaustiveAndExclusive walks every sentinel
// declared in variables.go and asserts it carries exactly the one helpers
// cache-backend class its row names, matching that class through errors.Is
// and matching NEITHER of the other two. Without the exclusivity half, a
// future edit that silently added a second class to some sentinel's wrap -
// e.g. errS3LockWaitTimeout also picking up
// helpers.ErrCacheBackendUnavailable - would move that failure's exit code
// with nothing here to catch it, since cmd/go-galaxy/exitcode's FromError
// would just silently pick whichever class it happens to check first
// (network before usage; see variables.go's own doc comment on this
// invariant).
//
// KILLING MUTATION, run and reverted: adding helpers.ErrCacheBackendUnavailable
// to errS3LockWaitTimeout's wrap makes the "errS3LockWaitTimeout" subtest
// fail with:
//
//	sentinel_class_test.go:87: errors.Is(err, helpers.ErrCacheBackendUnavailable) = true, want false
func TestSentinelClassPartitionIsExhaustiveAndExclusive(t *testing.T) {
	t.Parallel()
	for _, tt := range sentinelClassCases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			wantUnavailable := tt.class == classUnavailable
			wantUnusable := tt.class == classUnusable
			wantBusy := tt.class == classBusy

			if got := errors.Is(tt.err, helpers.ErrCacheBackendUnavailable); got != wantUnavailable {
				t.Errorf("errors.Is(err, helpers.ErrCacheBackendUnavailable) = %v, want %v", got, wantUnavailable)
			}
			if got := errors.Is(tt.err, helpers.ErrCacheBackendUnusable); got != wantUnusable {
				t.Errorf("errors.Is(err, helpers.ErrCacheBackendUnusable) = %v, want %v", got, wantUnusable)
			}
			if got := errors.Is(tt.err, helpers.ErrCacheBusy); got != wantBusy {
				t.Errorf("errors.Is(err, helpers.ErrCacheBusy) = %v, want %v", got, wantBusy)
			}
		})
	}
}
