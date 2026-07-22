package s3

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	mathrand "math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// lockTiming holds the tunable timing parameters of the distributed lock
// state machine. It lives on Backend (rather than as bare package-level
// values) so tests can shrink every interval to make the lock's behavior
// fast and deterministic without touching the production defaults, which
// New populates from the package's lock* constants.
type lockTiming struct {
	// ttl is the lifetime a lock holder is granted before another acquirer
	// may consider it dead and reclaim it.
	ttl time.Duration
	// heartbeatInterval is how often a live holder refreshes the lock
	// object's deadline in the background.
	heartbeatInterval time.Duration
	// heartbeatOpTimeout bounds each individual heartbeat HEAD/PUT pair.
	heartbeatOpTimeout time.Duration
	// releaseTimeout bounds the release path's own S3 calls, which run on a
	// fresh context independent of the (possibly already-canceled) context
	// the caller originally acquired the lock with.
	releaseTimeout time.Duration
	// waitCeiling bounds the total time acquireLock spends contending for
	// the lock before giving up with errS3LockWaitTimeout.
	waitCeiling time.Duration
	// backoffBase and backoffCap bound the full-jitter exponential backoff
	// between failed acquisition attempts.
	backoffBase time.Duration
	backoffCap  time.Duration
}

// lockRecord mirrors the lock object's authoritative X-Amz-Meta-* headers in
// the object body, purely so a human inspecting the object (e.g. via `aws
// s3 cp` and `cat`) can read it without decoding headers. The protocol
// itself never parses this body back; only the metadata headers, read via
// HEAD, are authoritative.
type lockRecord struct {
	Token    string `json:"token"`
	Deadline string `json:"deadline"`
	Owner    string `json:"owner"`
	Updated  string `json:"updated"`
}

// acquireLock creates or reclaims the lock object at key using a
// token-verified, create-if-absent protocol: acquirers race to PUT with
// If-None-Match: *, and an existing object is only ever reclaimed after its
// writer-recorded deadline (via lockExpired) shows its holder's TTL has
// elapsed. All attempts run against waitCtx, a fixed wait-ceiling derived
// from ctx. The release closure returned on success does not use ctx (or
// waitCtx) at all: it runs its own S3 calls on a fresh context, since it may
// be invoked long after acquireLock returned and ctx may by then already be
// canceled (e.g. the install run it belongs to was interrupted) - using it
// would leak the lock object until its TTL elapses instead of releasing it.
func (b *Backend) acquireLock(ctx context.Context, key string) (func() error, error) {
	waitCtx, cancel := context.WithTimeout(ctx, b.lock.waitCeiling)
	defer cancel()

	// One token identifies this entire acquisition attempt (including any
	// retries and the eventual reclaim), so verifyOwner and the heartbeat
	// consistently recognize writes made by this call.
	token, err := generateLockToken()
	if err != nil {
		return nil, err
	}

	for attempt := 0; ; attempt++ {
		release, retryNow, err := b.tryAcquireOnce(waitCtx, key, token)
		switch {
		case err != nil:
			// An in-flight S3 call can fail with a raw transport error
			// (e.g. "context deadline exceeded") the instant waitCtx's
			// timeout fires, before ever reaching the backoff select
			// below. Once waitCtx is done, classify why: a caller
			// cancellation must still surface as such (so the exit-code
			// taxonomy maps a Ctrl-C to ExitInterrupt), while a ceiling
			// that fired on its own becomes errS3LockWaitTimeout.
			if waitCtx.Err() != nil {
				return nil, waitCeilingErr(ctx)
			}
			return nil, err
		case release != nil:
			return release, nil
		case retryNow:
			continue
		}
		if err := b.lockBackoff(ctx, waitCtx, attempt); err != nil {
			return nil, err
		}
	}
}

// tryAcquireOnce performs a single create-or-reclaim step against the lock
// object. opCtx bounds all the S3 calls made during this step (the shared
// wait-ceiling context). It returns a non-nil release function on success;
// retryNow=true when the object vanished between the failed create and the
// follow-up HEAD, so the caller should retry immediately without sleeping
// out a backoff interval; or an error.
func (b *Backend) tryAcquireOnce(opCtx context.Context, key, token string) (func() error, bool, error) {
	deadline := time.Now().UTC().Add(b.lock.ttl)
	putErr := b.putLock(opCtx, key, token, deadline, true)
	if putErr == nil {
		release, ok, err := b.claim(opCtx, key, token)
		if err != nil || ok {
			return release, false, err
		}
		// Another writer's create raced in between our PUT and the
		// verifyOwner HEAD; back off and retry.
		return nil, false, nil
	}
	if !errors.Is(putErr, errS3PreconditionFailed) {
		return nil, false, putErr
	}
	return b.reclaimIfExpired(opCtx, key, token)
}

// reclaimIfExpired runs after a failed create-if-absent PUT: it HEADs the
// existing object and, if its TTL has elapsed, reclaims it (delete then
// recreate under our token). See tryAcquireOnce for the return contract.
func (b *Backend) reclaimIfExpired(opCtx context.Context, key, token string) (func() error, bool, error) {
	headers, headErr := b.client.headObject(opCtx, key)
	switch {
	case errors.Is(headErr, errS3NotFound):
		// The holder released (or its create raced with a delete) right
		// after our precondition failure: retry the create immediately.
		return nil, true, nil
	case headErr != nil:
		return nil, false, headErr
	}

	if !lockExpired(headers, b.lock.ttl) {
		// The holder is still live: back off and retry later.
		return nil, false, nil
	}

	// The holder's TTL has elapsed: reclaim by deleting unconditionally and
	// recreating the object with a fresh deadline under our token.
	if err := b.client.deleteObject(opCtx, key); err != nil {
		return nil, false, err
	}
	reclaimDeadline := time.Now().UTC().Add(b.lock.ttl)
	putErr := b.putLock(opCtx, key, token, reclaimDeadline, true)
	if putErr == nil {
		return b.claim(opCtx, key, token)
	}
	if !errors.Is(putErr, errS3PreconditionFailed) {
		return nil, false, putErr
	}
	// Somebody else's create won the race for the just-deleted object.
	return nil, false, nil
}

// claim verifies that the lock object this call just wrote is still ours
// (guarding against a concurrent writer racing in between the PUT and this
// check) and, if so, starts the heartbeat and returns the release closure.
func (b *Backend) claim(opCtx context.Context, key, token string) (func() error, bool, error) {
	ours, err := b.verifyOwner(opCtx, key, token)
	if err != nil || !ours {
		return nil, false, err
	}
	//nolint:contextcheck // startHeartbeat's release closure deliberately builds its own fresh
	// context instead of reusing opCtx: opCtx may already be canceled by the time release runs.
	return b.startHeartbeat(key, token), true, nil
}

// verifyOwner reports whether the lock object's recorded token matches
// token, i.e. whether this call is (still) the object's writer.
func (b *Backend) verifyOwner(ctx context.Context, key, token string) (bool, error) {
	headers, err := b.client.headObject(ctx, key)
	if err != nil {
		return false, err
	}
	return headers.Get("X-Amz-Meta-Token") == token, nil
}

// putLock writes the lock object's four authoritative metadata headers
// (token, deadline, owner, updated) plus a JSON body mirroring them for
// human debugging. create selects create-if-absent (If-None-Match: *) for
// the initial acquisition and for reclaiming a dead holder's object, versus
// an unconditional overwrite used by the heartbeat to refresh the deadline
// under the same token.
func (b *Backend) putLock(ctx context.Context, key, token string, deadline time.Time, create bool) error {
	now := time.Now().UTC().Format(time.RFC3339)
	owner := lockOwner()
	deadlineStr := deadline.Format(time.RFC3339)

	meta := map[string]string{
		"token":    token,
		"deadline": deadlineStr,
		"owner":    owner,
		"updated":  now,
	}
	body, err := json.Marshal(lockRecord{
		Token:    token,
		Deadline: deadlineStr,
		Owner:    owner,
		Updated:  now,
	})
	if err != nil {
		return err
	}
	reader := bytes.NewReader(body)
	return b.client.putObject(ctx, key, reader, int64(len(body)), "application/json", "", meta, create, "")
}

// lockOwner identifies this process for the lock object's diagnostic Owner
// field; it is never parsed or compared, only ever displayed.
func lockOwner() string {
	host, _ := os.Hostname()
	return host + "/" + strconv.Itoa(os.Getpid())
}

// generateLockToken returns a fresh 32-hex-character token unique to one
// acquisition attempt. It never falls back to a weaker source: a
// crypto/rand failure is reported as errS3TokenGeneration rather than
// silently degrading the lock's uniqueness guarantee.
func generateLockToken() (string, error) {
	buf := make([]byte, lockTokenBytes)
	if _, err := cryptorand.Read(buf); err != nil {
		return "", fmt.Errorf("%w: %w", errS3TokenGeneration, err)
	}
	return hex.EncodeToString(buf), nil
}

// waitCeilingErr classifies why the wait-ceiling context ended. If the
// caller's own context was canceled or hit its deadline, that error is
// propagated unchanged so the exit-code taxonomy still maps a caller Ctrl-C
// to ExitInterrupt; only a ceiling that fired while the caller is still live
// becomes errS3LockWaitTimeout.
func waitCeilingErr(parent context.Context) error {
	if err := parent.Err(); err != nil {
		return err
	}
	return errS3LockWaitTimeout
}

// lockBackoff sleeps a full-jitter exponential backoff interval before the
// next acquisition attempt, honoring waitCtx cancellation. It returns
// waitCeilingErr(parent) once waitCtx (the shared wait-ceiling context
// derived from parent) expires.
func (b *Backend) lockBackoff(parent, waitCtx context.Context, attempt int) error {
	timer := time.NewTimer(backoffDelay(b.lock.backoffBase, b.lock.backoffCap, attempt))
	defer timer.Stop()
	select {
	case <-waitCtx.Done():
		return waitCeilingErr(parent)
	case <-timer.C:
		return nil
	}
}

// backoffDelay returns a full-jitter exponential backoff duration: a
// uniformly random value in [0, min(backoffCap, base*2^attempt)).
func backoffDelay(base, backoffCap time.Duration, attempt int) time.Duration {
	if base <= 0 {
		return 0
	}
	delayCeiling := backoffCap
	// Guard against shifting a duration into overflow for large attempt
	// counts: once the shifted value would meet or exceed backoffCap,
	// clamp immediately instead of computing an undefined/negative shift.
	if attempt >= 0 && attempt < 63 {
		if scaled := base << uint(attempt); scaled > 0 && scaled < backoffCap {
			delayCeiling = scaled
		}
	}
	if delayCeiling <= 0 {
		return 0
	}
	//nolint:gosec // G404: jitter timing only, not security sensitive.
	return time.Duration(mathrand.Int64N(int64(delayCeiling)))
}

// startHeartbeat launches the lock's background lifecycle goroutine and
// returns the release closure bound to it. The heartbeat periodically
// refreshes the lock object's deadline under the same token so a long-running
// holder never needs to re-run the acquisition state machine; if it ever
// observes a different token (meaning some other acquirer reclaimed the
// lock, e.g. after this holder stalled past its TTL), it records the loss so
// release refuses to delete a lock it no longer owns. Neither the heartbeat
// loop nor the release closure it returns depend on the context the caller
// used to acquire the lock: the heartbeat runs on its own independent
// background context so it keeps refreshing the deadline for as long as the
// holder keeps the lock, and release builds its own fresh, timeout-bounded
// context for its final HEAD/DELETE (see releaseLock) so a canceled caller
// context can never prevent the lock from being released.
func (b *Backend) startHeartbeat(key, token string) func() error {
	hbCtx, hbCancel := context.WithCancel(context.Background())
	var lost atomic.Bool
	done := make(chan struct{})

	go func() {
		defer close(done)
		ticker := time.NewTicker(b.lock.heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-ticker.C:
				if !b.heartbeatTick(hbCtx, key, token, &lost) {
					return
				}
			}
		}
	}()

	return func() error {
		hbCancel()
		<-done
		if lost.Load() {
			return errS3LockLost
		}
		// A fresh, timeout-bounded context: the caller's original context
		// (used throughout acquisition) may already be canceled by now -
		// e.g. an interrupted or failed install run - and using it here
		// would make releaseLock's HEAD/DELETE fail immediately, leaking
		// the lock object until its TTL elapses instead of releasing it.
		relCtx, cancel := context.WithTimeout(context.Background(), b.lock.releaseTimeout)
		defer cancel()
		return b.releaseLock(relCtx, key, token)
	}
}

// heartbeatTick refreshes the lock's deadline under token. It returns false
// only on a definitive token mismatch (ownership lost to another acquirer),
// setting lost accordingly; transient errors (network failures, a momentary
// S3 hiccup) return true so the next tick simply retries rather than
// declaring the lock lost.
func (b *Backend) heartbeatTick(hbCtx context.Context, key, token string, lost *atomic.Bool) bool {
	opCtx, cancel := context.WithTimeout(hbCtx, b.lock.heartbeatOpTimeout)
	defer cancel()

	ours, err := b.verifyOwner(opCtx, key, token)
	if err != nil {
		return true
	}
	if !ours {
		lost.Store(true)
		return false
	}
	// A failure here is transient (e.g. a dropped connection): the next
	// tick will retry with a deadline computed at that later time.
	_ = b.putLock(opCtx, key, token, time.Now().UTC().Add(b.lock.ttl), false)
	return true
}

// releaseLock deletes the lock object only if it still records token,
// matching the same token-verified guard as heartbeat loss detection so a
// release call never deletes a lock some other acquirer has since claimed.
// A missing object is treated as already released.
func (b *Backend) releaseLock(ctx context.Context, key, token string) error {
	headers, err := b.client.headObject(ctx, key)
	if errors.Is(err, errS3NotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if headers.Get("X-Amz-Meta-Token") != token {
		return nil
	}
	return b.client.deleteObject(ctx, key)
}

// lockExpired reports whether the lock described by headers may be reclaimed.
// The writer-recorded deadline is authoritative; a malformed or absent deadline
// falls back to age-based staleness against ttl, and uninterpretable timing is
// treated as reclaimable so a corrupt lock object can never permanently block
// acquisition.
func lockExpired(headers http.Header, ttl time.Duration) bool {
	now := time.Now().UTC()
	if dl := strings.TrimSpace(headers.Get("X-Amz-Meta-Deadline")); dl != "" {
		if t, err := time.Parse(time.RFC3339, dl); err == nil {
			return now.After(t)
		}
		// malformed deadline -> do not hard-fail; fall back to age-based staleness
	}
	if lm := strings.TrimSpace(headers.Get("Last-Modified")); lm != "" {
		if t, err := http.ParseTime(lm); err == nil {
			return now.Sub(t) > ttl
		}
	}
	return true // uninterpretable timing -> reclaimable, never a permanent block
}
