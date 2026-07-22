package s3

import (
	"errors"
	"time"
)

var (
	errS3BucketIsEmpty        = errors.New("s3 bucket is empty")
	errS3HttpClientIsNil      = errors.New("s3 http client is nil")
	errS3LockLost             = errors.New("s3 lock ownership was lost to another holder")
	errS3LockWaitTimeout      = errors.New("s3 lock wait ceiling exceeded")
	errS3TokenGeneration      = errors.New("s3 lock token generation failed")
	errS3NotFound             = errors.New("s3 object not found")
	errS3BucketNotFound       = errors.New("s3 bucket not found")
	errS3BucketEmpty          = errors.New("s3 bucket is empty")
	errS3BucketHeadFailed     = errors.New("s3 bucket head is failed")
	errS3CreateBucketFailed   = errors.New("s3 create bucket failed")
	errS3BucketRequestFailed  = errors.New("s3 bucket request failed")
	errS3PreconditionFailed   = errors.New("s3 precondition failed")
	errS3HTTPClientNil        = errors.New("s3 http client is nil")
	errS3InvalidEndpoint      = errors.New("s3 invalid endpoint")
	errS3GetFailed            = errors.New("s3 get object failed")
	errS3HeadFailed           = errors.New("s3 head object failed")
	errS3PutFailed            = errors.New("s3 put object failed")
	errS3DeleteFailed         = errors.New("s3 delete object failed")
	errS3ClientNil            = errors.New("s3 client is nil")
	errArtifactSHA256Mismatch = errors.New("s3 artifact sha256 mismatch")
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
)
