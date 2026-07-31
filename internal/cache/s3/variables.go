package s3

import (
	"errors"
	"fmt"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// Every sentinel below that can escape this package into cmd/go-galaxy/exitcode
// carries at most one of the three helpers cache-backend classes. The
// partition splits on WHERE a failure was discovered, not on a per-status
// judgment of how permanent it looks:
//
//   - helpers.ErrCacheBackendUnavailable: a status the remote itself answered
//     (a non-2xx from an idempotent verb - GET/HEAD/PUT/DELETE/list - against
//     an object or the bucket, excepting the two statuses consumed as control
//     flow below: 404 -> errS3NotFound, 412 -> errS3PreconditionFailed), or a
//     transport-level failure surfaced through Client.do while the caller's
//     own context is still live - a connect refusal, a DNS failure, a TLS
//     failure, a dial timeout, or a response-header timeout, none of which
//     ever produced a response to answer with. A status-level failure lands
//     here even when it looks permanent, e.g. a 403 answered to createBucket:
//     this class means "the remote was reached and is the one saying no or
//     staying silent", not "retrying is expected to help". errS3BucketNotFound
//     carries it too: ensureBucket already resolves a bucket-absent HEAD by
//     creating the bucket, so the only way this sentinel escapes is a PUT or
//     list answered 404 mid-run against a bucket Open already ensured exists -
//     a remote-state condition, not a configuration one.
//   - helpers.ErrCacheBackendUnusable: the configured backend cannot provide a
//     guarantee this tool requires - discovered once at Open, against a probe
//     this tool controls, rather than against arbitrary remote state, and
//     never retryable - a backend that does not enforce conditional PUT
//     (errS3ConditionalPutUnsupported) or an endpoint that fails to parse into
//     a usable host (errS3InvalidEndpoint).
//   - helpers.ErrCacheBusy: the lock is held by another holder and the wait
//     ceiling elapsed before it was released (errS3LockWaitTimeout).
//     DISCLOSED, not hidden: acquireLock's wait-ceiling arm
//     (waitCeilingErr, lock.go) reports this class whenever the shared
//     wait-ceiling context has itself expired by the time an attempt's error
//     is observed, even when that attempt's own failure was actually a
//     transport-level one (helpers.ErrCacheBackendUnavailable) that merely
//     raced the ceiling rather than being caused by it - a genuine dead
//     backend can therefore surface as mere contention if its failure lands
//     in the last stretch of the wait ceiling. The window this can happen in
//     is bounded by one request's own worst-case latency before it errors
//     out, dominated under default configuration by
//     helpers.FetchDefaultTimeout's 30-second ResponseHeaderTimeout - narrow
//     against lockWaitCeiling's 5 minutes, but not zero. This is registered
//     separately, not fixed here.
//   - Nothing, split into three honest groups rather than one: (a)
//     errS3ClientNil, errS3HTTPClientNil, and errS3BucketEmpty guard
//     construction-time state no operator input reaches; (b) errS3LockLost is
//     a real runtime ownership race (another acquirer reclaimed the lock
//     after this holder's heartbeat stalled past its TTL) that stays
//     unclassified because it never reaches exitcode.FromError at all - every
//     caller only logs it (internal/galaxy/collections/start.go's three
//     `state.release()` sites) rather than propagating it as this run's own
//     error; (c) errS3TokenGeneration is a crypto/rand failure deliberately
//     left generic, since a source that cannot be trusted to generate a lock
//     token is not a cache-backend condition this partition is about.
//     errS3NotFound and errS3PreconditionFailed are consumed as control flow
//     by their own callers - Has, LoadStore, LoadProjectRegistry, and the
//     lock protocol (lock.go:148, lock.go:183, backend.go:295) - rather than
//     being classified; errS3NotFound is not always fully absorbed, though:
//     Artifacts.Fetch surfaces it to a cache-hit arm
//     (internal/galaxy/collections/install.go's fetchArtifact) whose object
//     vanished between Has and Fetch, where it becomes a per-collection cause
//     that classifies by the install-failure aggregation, deliberately not by
//     a cache-backend class - a vanished cache entry is this program's own
//     bookkeeping catching up with reality, not evidence the backend itself
//     is unavailable. errArtifactSHA256Mismatch already carries
//     helpers.ErrSHA256Mismatch at its call site, which exitcode checks ahead
//     of every class here.
//
// No sentinel may carry more than one of the three classes: FromError checks
// the network class before the usage class, so a sentinel carrying both would
// have the usage class silently win only when it happens not to be reached
// first, making the classification depend on FromError's own ordering rather
// than on what the sentinel means.
var (
	errS3LockLost                  = errors.New("s3 lock ownership was lost to another holder")
	errS3LockWaitTimeout           = fmt.Errorf("%w: s3 lock wait ceiling exceeded", helpers.ErrCacheBusy)
	errS3TokenGeneration           = errors.New("s3 lock token generation failed")
	errS3NotFound                  = errors.New("s3 object not found")
	errS3BucketNotFound            = fmt.Errorf("%w: s3 bucket not found", helpers.ErrCacheBackendUnavailable)
	errS3BucketEmpty               = errors.New("s3 bucket is empty")
	errS3BucketHeadFailed          = fmt.Errorf("%w: s3 bucket head is failed", helpers.ErrCacheBackendUnavailable)
	errS3CreateBucketFailed        = fmt.Errorf("%w: s3 create bucket failed", helpers.ErrCacheBackendUnavailable)
	errS3BucketRequestFailed       = fmt.Errorf("%w: s3 bucket request failed", helpers.ErrCacheBackendUnavailable)
	errS3PreconditionFailed        = errors.New("s3 precondition failed")
	errS3HTTPClientNil             = errors.New("s3 http client is nil")
	errS3InvalidEndpoint           = fmt.Errorf("%w: s3 invalid endpoint", helpers.ErrCacheBackendUnusable)
	errS3GetFailed                 = fmt.Errorf("%w: s3 get object failed", helpers.ErrCacheBackendUnavailable)
	errS3HeadFailed                = fmt.Errorf("%w: s3 head object failed", helpers.ErrCacheBackendUnavailable)
	errS3PutFailed                 = fmt.Errorf("%w: s3 put object failed", helpers.ErrCacheBackendUnavailable)
	errS3DeleteFailed              = fmt.Errorf("%w: s3 delete object failed", helpers.ErrCacheBackendUnavailable)
	errS3ClientNil                 = errors.New("s3 client is nil")
	errArtifactSHA256Mismatch      = errors.New("s3 artifact sha256 mismatch")
	errS3ConditionalPutUnsupported = fmt.Errorf(
		"%w: s3 backend does not enforce conditional PUT (If-None-Match); distributed locking cannot guarantee mutual exclusion",
		helpers.ErrCacheBackendUnusable,
	)
)

const (
	emptySHA256     = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	statePrefix     = "state"
	artifactsPrefix = "artifacts"
	locksPrefix     = "locks"
	storeObject     = "store.json.gz"
	projectsObject  = "projects.json"
	lockObject      = "cache.lock"
	peekBytes       = 2
	headerLength    = 2

	// s3ErrorBodyLimit bounds how much of a non-2xx response body is read
	// when looking for an S3 XML <Error> document, so a misbehaving or
	// unexpectedly large error body cannot force an unbounded read.
	s3ErrorBodyLimit = 8 << 10

	// conditionalProbeObject is the base name of the probe object written
	// under the locks prefix at Open to verify the backend enforces
	// conditional PUT (If-None-Match); a per-process random suffix is
	// appended to avoid colliding with a stale probe left behind by a
	// crashed prior run.
	conditionalProbeObject = ".conditional-probe"

	// lockTokenBytes is the number of random bytes read from crypto/rand to
	// build a lock token; hex-encoded, this yields a 32-character token.
	lockTokenBytes = 16

	// lockTTL is the lifetime a lock holder is granted before another
	// acquirer is allowed to consider it dead and reclaim it.
	lockTTL = 10 * time.Minute
	// heartbeatInterval is how often a live holder refreshes the lock
	// object's deadline in the background.
	heartbeatInterval = 3 * time.Minute
	// heartbeatOpTimeout bounds each individual heartbeat HEAD/PUT pair so a
	// stalled S3 call cannot delay the next tick indefinitely.
	heartbeatOpTimeout = 30 * time.Second
	// lockReleaseTimeout bounds the release path's own S3 calls on a fresh
	// context.
	lockReleaseTimeout = 30 * time.Second
	// lockWaitCeiling bounds the total time acquireLock will spend
	// contending for the lock before giving up with errS3LockWaitTimeout.
	lockWaitCeiling = 5 * time.Minute
	// lockBackoffBase and lockBackoffCap bound the full-jitter exponential
	// backoff between failed acquisition attempts.
	lockBackoffBase = 250 * time.Millisecond
	lockBackoffCap  = 5 * time.Second

	// maxImmediateLockRetries bounds how many consecutive retryNow handoffs
	// acquireLock's loop honors with an immediate retry (no backoff sleep)
	// before it degrades to the normal backoff path. It exists purely as an
	// acquirer-side guard against a misbehaving S3-compatible backend that
	// answers the create-if-absent PUT with 412 (precondition failed) while
	// a follow-up HEAD keeps reporting the object as missing: without this
	// bound, that inconsistency would drive a continuous PUT+HEAD spin with
	// no backoff at all, up to the full waitCeiling.
	maxImmediateLockRetries = 8

	// s3RetryMaxAttempts bounds how many times an idempotent S3 verb (GET,
	// HEAD, DELETE, list, and an unconditional PUT) is attempted before its
	// last failure is returned as final.
	s3RetryMaxAttempts = 4
	// s3RetryBackoffBase and s3RetryBackoffCap bound the full-jitter
	// exponential backoff between retried attempts of an idempotent S3 verb.
	s3RetryBackoffBase = 200 * time.Millisecond
	s3RetryBackoffCap  = 5 * time.Second
)
