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
//     flow below - 404 -> errS3NotFound, 412 -> errS3PreconditionFailed - and
//     a redirect, which never reaches status classification at all: c.client's
//     CheckRedirect hook refuses it before Do returns, landing it in the
//     Unusable bullet below as errS3RedirectRefused), or
//     errS3TransportFailed, which Client.do wraps around a transport-level
//     failure while the caller's own context is still live - a connect
//     refusal, a DNS failure, a TLS failure, a dial timeout, or a
//     response-header timeout, none of which ever produced a response to
//     answer with. A status-level failure lands here even when it looks
//     permanent, e.g. a 403 answered to createBucket:
//     this class means "the remote was reached and is the one saying no or
//     staying silent", not "retrying is expected to help". errS3BucketNotFound
//     carries it too: ensureBucket already resolves a bucket-absent HEAD by
//     creating the bucket, so the only way this sentinel escapes is a list, or
//     a PUT carrying no If-Match, answered 404 mid-run against a bucket Open
//     already ensured exists - a remote-state condition, not a configuration
//     one. The If-Match exception is not a special case but the same rule read
//     precisely: a 404 answering a compare-and-swap names the object the write
//     was conditioned on, never the bucket, and is consumed as control flow
//     like every other 404 (see handlePutResponse, client.go). errS3LockWaitNoHolderObserved
//     carries it too: acquireLockLoop's wait-ceiling arm (waitCeilingErr, lock.go)
//     reports it when the wait ceiling elapses without this wait ever having
//     observed another acquirer holding the lock - covering an endpoint that
//     accepts connections and never replies, and one that contradicts itself
//     about whether the lock object exists. A backend that instead answers
//     only with failures does not reach this sentinel the same way: its own
//     errS3*Failed sentinel surfaces directly while the wait ceiling is
//     still live (see waitCeilingErr's own doc comment), landing in this
//     class anyway only because errS3*Failed already carries it.
//   - helpers.ErrCacheBackendUnusable: the configured backend cannot provide a
//     guarantee this tool requires, and never retryable - a backend that does
//     not enforce create-if-absent (errS3ConditionalPutUnsupported) or does not
//     honor compare-and-swap (errS3CompareAndSwapUnsupported), the two
//     conditional writes the lock protocol is built on, or an
//     endpoint that fails to parse into a usable host (errS3InvalidEndpoint),
//     all discovered once at Open against a probe this tool controls rather
//     than against arbitrary remote state - except for the one runtime path
//     that can reach errS3CompareAndSwapUnsupported after Open passed: a HEAD
//     answered without an ETag leaves reclaimIfExpired nothing to condition its
//     swap on, which is the same missing guarantee observed against real remote
//     state rather than against the probe; or an endpoint that answers any
//     request with an HTTP redirect (errS3RedirectRefused), discovered
//     wherever it first happens rather than only at Open. SigV4 signs the
//     Host header and the canonical URI, so a redirect to a different path or
//     host cannot produce a verifiable request in any shape this client
//     emits. Every member of this class shares the same remedy: a
//     configuration change, addressing the store the way it wants to be
//     addressed, not a retry.
//   - helpers.ErrCacheBusy: this acquisition observed another acquirer
//     holding the lock - a live, unexpired holder; a foreign token on the
//     object this call just wrote; or a writer whose own write beat this
//     call's compare-and-swap to an expired holder's object - and the wait
//     ceiling elapsed before it was released (errS3LockWaitTimeout). The verdict
//     rests on an answer the backend gave during this wait, never on what
//     happened to be in flight when the ceiling fired, so it does not
//     depend on where in the acquisition loop the ceiling falls. It claims
//     that the backend told us, at some point during this wait, that
//     someone else had it - not that the backend was still healthy when the
//     wait ended. A wait that never obtained such an answer - an endpoint
//     that accepts connections and never replies, or contradicts itself -
//     ends with errS3LockWaitNoHolderObserved instead, which carries
//     helpers.ErrCacheBackendUnavailable.
//   - Nothing, split into three honest groups rather than one: (a)
//     errS3ClientNil, errS3HTTPClientNil, and errS3BucketEmpty guard
//     construction-time state no operator input reaches; (b) errS3LockLost is
//     a real runtime ownership race (another acquirer reclaimed the lock
//     after this holder's heartbeat stalled past its TTL) that carries none
//     of the three classes because it is a fourth condition with an exit
//     class of its own: it wraps helpers.ErrCacheLockLost, which maps to
//     ExitCacheBusy, the same way ExitCacheCorrupt's own sentinels sit
//     outside this three-way partition. Ownership taken away mid-run says
//     nothing about whether the backend can be reached or can serve as one -
//     it served this acquisition and every heartbeat perfectly - so none of
//     the three would be an honest answer; (c) errS3TokenGeneration is a
//     crypto/rand failure deliberately left generic, since a source that
//     cannot be trusted to generate a lock token is not a cache-backend
//     condition this partition is about.
//     errS3NotFound and errS3PreconditionFailed are consumed as control flow
//     by their own callers - Has, LoadStore, LoadProjectRegistry, and the
//     lock protocol's tryAcquireOnce/reclaimIfExpired (lock.go) and
//     probeConditionalPut (backend.go) - rather than being classified;
//     errS3ConditionalConflict is consumed the same way by the lock protocol
//     but carries this class anyway, since unlike those two it has a reachable
//     path out of the package: probeConditionalPut surfaces it rather than
//     retrying, and a conflict there is a remote condition a later run may
//     well not hit;
//     errS3NotFound is not always fully absorbed, though:
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
	errS3LockLost                 = fmt.Errorf("%w: s3 lock ownership was lost to another holder", helpers.ErrCacheLockLost)
	errS3LockWaitTimeout          = fmt.Errorf("%w: s3 lock wait ceiling exceeded", helpers.ErrCacheBusy)
	errS3LockWaitNoHolderObserved = fmt.Errorf(
		"%w: s3 lock wait ceiling elapsed without ever observing a lock holder",
		helpers.ErrCacheBackendUnavailable,
	)
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
	errS3TransportFailed           = fmt.Errorf("%w: s3 request failed", helpers.ErrCacheBackendUnavailable)
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
	errS3ConditionalConflict = fmt.Errorf(
		"%w: s3 conditional write conflicted with a concurrent request",
		helpers.ErrCacheBackendUnavailable,
	)
	errS3CompareAndSwapUnsupported = fmt.Errorf(
		"%w: s3 backend does not support compare-and-swap PUT (If-Match against an ETag); "+
			"an expired lock holder cannot be reclaimed safely",
		helpers.ErrCacheBackendUnusable,
	)
	errS3RedirectRefused = fmt.Errorf("%w: s3 endpoint answered with a redirect", helpers.ErrCacheBackendUnusable)
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
	// under the locks prefix at Open to verify the backend enforces both
	// conditional writes the lock protocol uses (If-None-Match and If-Match);
	// a per-process random suffix is appended to avoid colliding with a stale
	// probe left behind by a crashed prior run.
	conditionalProbeObject = ".conditional-probe"

	// staleProbeETag is the entity tag Open's compare-and-swap probe swaps
	// against to prove the backend refuses a mismatch. It names a version no
	// backend can ever have minted, so "it did not match" is a property of the
	// value rather than of what the probe object happens to hold - which is
	// what lets the probe assert a refusal without first having to produce a
	// genuinely superseded ETag.
	staleProbeETag = `"go-galaxy-stale-probe-etag"`

	// lockTokenBytes is the number of random bytes read from crypto/rand to
	// build a lock token; hex-encoded, this yields a 32-character token.
	lockTokenBytes = 16

	// lockTTL is the lifetime a lock holder is granted before another
	// acquirer is allowed to consider it dead and reclaim it.
	//
	// It is also the whole of what stands between a holder that dies without
	// running its release and every other runner sharing the bucket. A Go
	// fatal error - a stack overflow, an out-of-memory kill, a runtime
	// throw - runs no deferred function, so releaseLock never executes and the
	// lock object survives here until this deadline passes. The honest cost is
	// larger than the wait itself while more than lockWaitCeiling of that
	// deadline is left: a runner arriving then spends its whole wait contending
	// and exits ExitCacheBusy without ever reaching the work, where one arriving
	// later waits out what remains and proceeds. So one dead holder can cost
	// another runner five minutes and a red build before the cache is usable
	// again.
	// Shortening this deadline is not the remedy - it would let a live but
	// slow holder be reclaimed, which is the failure this value exists to
	// prevent - so the remedy is upstream, in not dying: internal/gzipstream
	// closes one such death that was measured, a gzip member flood exhausting
	// the goroutine stack, and any other fatal-error path costs the bucket
	// exactly this same wait.
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
	// contending for the lock before giving up - with errS3LockWaitTimeout
	// if this wait ever observed another acquirer holding the lock, or
	// errS3LockWaitNoHolderObserved otherwise (see waitCeilingErr).
	lockWaitCeiling = 5 * time.Minute
	// lockBackoffBase and lockBackoffCap bound the full-jitter exponential
	// backoff between failed acquisition attempts.
	lockBackoffBase = 250 * time.Millisecond
	lockBackoffCap  = 5 * time.Second

	// maxImmediateLockRetries bounds how many consecutive retryNow handoffs
	// acquireLockLoop's loop honors with an immediate retry (no backoff sleep)
	// before it degrades to the normal backoff path. It exists purely as an
	// acquirer-side guard against a misbehaving S3-compatible backend that
	// answers the create-if-absent PUT with 412 (precondition failed) while
	// a follow-up HEAD keeps reporting the object as missing: without this
	// bound, that inconsistency would drive a continuous PUT+HEAD spin with
	// no backoff at all, up to the full waitCeiling.
	maxImmediateLockRetries = 8

	// s3RetryMaxAttempts bounds how many times an idempotent S3 verb (GET,
	// HEAD, DELETE, list, and an unconditional PUT) is attempted before its
	// last failure is returned as final. A verb reached through a surface with
	// no wall-clock budget of its own (Open's headBucket, an Artifacts.Has/HEAD
	// probe, ClearFiles's listing and deletes) waits up to roughly
	// s3RetryMaxAttempts times its own per-attempt cost, plus the jittered
	// backoff between attempts bounded by s3RetryBackoffBase and
	// s3RetryBackoffCap, before giving up on a transport failure. Which of the
	// two terms dominates depends on the failure: the backoff does when an
	// attempt is near-instant (a connect refusal), the per-attempt cost does
	// when an attempt runs to a response-header timeout. A verb reached under
	// an existing budget (the state-object deadline, the artifact download
	// deadline, the lock wait ceiling) is unaffected, since that budget's own
	// context cuts the retry loop short regardless of this count.
	s3RetryMaxAttempts = 4
	// s3RetryBackoffBase and s3RetryBackoffCap bound the full-jitter
	// exponential backoff between retried attempts of an idempotent S3 verb.
	s3RetryBackoffBase = 200 * time.Millisecond
	s3RetryBackoffCap  = 5 * time.Second
)
