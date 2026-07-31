package s3

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
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
	// the lock before giving up - with errS3LockWaitTimeout if this wait
	// ever observed another acquirer holding the lock, or
	// errS3LockWaitNoHolderObserved otherwise (see waitCeilingErr).
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

	// consecutiveRetryNow counts consecutive retryNow handoffs from
	// tryAcquireOnce/reclaimIfExpired without an intervening backoff sleep.
	// A conforming backend only ever produces one such handoff in a row
	// (the object vanished right after our failed create; the next attempt
	// either succeeds or observes a live/expired object), so bounding this
	// counter guards against a misbehaving backend that answers the
	// create-if-absent PUT with 412 while HEAD keeps reporting the object
	// as missing, which would otherwise spin PUT+HEAD with no backoff at
	// all for the entire waitCeiling window.
	var consecutiveRetryNow int

	// observedHolder records whether ANY attempt during this wait completed
	// an observation that another acquirer holds the lock - see lockAttempt
	// and waitCeilingErr for what counts. It accumulates before the switch
	// below so every branch (including retryNow's continue) contributes,
	// and it is never reset: one observation anywhere in the wait is enough
	// to classify the eventual timeout as contention rather than an
	// unreachable backend.
	var observedHolder bool

	for attempt := 0; ; attempt++ {
		res, err := b.tryAcquireOnce(waitCtx, key, token)
		observedHolder = observedHolder || res.observed
		switch {
		case err != nil:
			return nil, acquireLockAttemptErr(ctx, waitCtx, observedHolder, err)
		case res.release != nil:
			return res.release, nil
		case res.retryNow:
			consecutiveRetryNow++
			if consecutiveRetryNow <= maxImmediateLockRetries {
				continue
			}
			// The immediate-retry budget is exhausted: exhausting it
			// means a misbehaving backend (repeatedly answering the
			// create-if-absent PUT with 412 while HEAD keeps reporting
			// the object as missing), so degrade to the same backoff
			// sleep the normal-miss branch takes below instead of
			// spinning without one.
		default:
			consecutiveRetryNow = 0
		}
		if err := b.lockBackoff(ctx, waitCtx, attempt, observedHolder); err != nil {
			return nil, err
		}
	}
}

// acquireLockAttemptErr classifies a failed attempt's error inside
// acquireLock's loop. It is split out purely to keep acquireLock under the
// cyclomatic-complexity budget: it is one classification step, not a
// decision point of its own.
//
// An in-flight S3 call can fail with a raw transport error (e.g. "context
// deadline exceeded") the instant waitCtx's timeout fires, before ever
// reaching the backoff select in lockBackoff. Once waitCtx is done, classify
// why: a caller cancellation must still surface as such (so the exit-code
// taxonomy maps a Ctrl-C to ExitInterrupt); otherwise, waitCeilingErr decides
// by observed - whether some earlier attempt in this wait already produced a
// completed observation of another acquirer - not by whether this particular
// attempt happened to end in an error, since the ceiling lands inside an
// in-flight attempt about as often as inside a backoff sleep (see
// waitCeilingErr's doc comment for why that ordering matters). A live waitCtx
// means the error is this attempt's own, unrelated to the ceiling, and is
// returned unclassified.
func acquireLockAttemptErr(ctx, waitCtx context.Context, observed bool, err error) error {
	if waitCtx.Err() != nil {
		return waitCeilingErr(ctx, observed, err)
	}
	return err
}

// lockAttempt describes what one create-or-reclaim step learned. The zero
// value means "this attempt proved nothing": a new return path that forgets
// to set observed degrades to "contention not proven", never to a false
// claim that another acquirer was seen. release and a non-nil error are
// mutually exclusive: acquireLock's loop checks err before release, so a
// producer that returned both would silently drop the closure, leaking the
// heartbeat goroutine and the lock object until its TTL elapses. No current
// path returns both.
type lockAttempt struct {
	release  func() error // non-nil once this call holds the lock
	retryNow bool         // the object vanished between our PUT and the HEAD; retry with no backoff
	observed bool         // the backend answered that another acquirer has it
}

// tryAcquireOnce performs a single create-or-reclaim step against the lock
// object. opCtx bounds all the S3 calls made during this step (the shared
// wait-ceiling context). See lockAttempt for what the returned value means;
// a non-nil error means the step itself failed rather than merely losing the
// race.
func (b *Backend) tryAcquireOnce(opCtx context.Context, key, token string) (lockAttempt, error) {
	deadline := time.Now().UTC().Add(b.lock.ttl)
	putErr := b.putLock(opCtx, key, token, deadline, true)
	if putErr == nil {
		// claim itself decides whether this counts as an observation of
		// another acquirer (a foreign token raced in ahead of us) or a
		// successful claim; either way it is this step's whole answer.
		return b.claim(opCtx, key, token)
	}
	if !errors.Is(putErr, errS3PreconditionFailed) {
		return lockAttempt{}, putErr
	}
	return b.reclaimIfExpired(opCtx, key, token)
}

// reclaimIfExpired runs after a failed create-if-absent PUT: it HEADs the
// existing object and, if its TTL has elapsed, reclaims it (delete then
// recreate under our token). See tryAcquireOnce for the return contract.
//
// Observing an expired holder is deliberately NOT recorded as observed: an
// expired lock is ours to take, not evidence that another acquirer holds it.
// Three outcomes below do count: a live, unexpired holder; another creator
// winning the race for the object this call just deleted; or, via the
// recreate's own call into claim, a foreign token found on the object this
// call just recreated - the post-reclaim counterpart to claim's own
// fresh-create race (see claim's doc comment).
func (b *Backend) reclaimIfExpired(opCtx context.Context, key, token string) (lockAttempt, error) {
	headers, headErr := b.client.headObject(opCtx, key)
	switch {
	case errors.Is(headErr, errS3NotFound):
		// The holder released (or its create raced with a delete) right
		// after our precondition failure: retry the create immediately.
		return lockAttempt{retryNow: true}, nil
	case headErr != nil:
		return lockAttempt{}, headErr
	}

	if !lockExpired(headers, b.lock.ttl) {
		// The holder is still live: back off and retry later. This is the
		// primary contention signal.
		return lockAttempt{observed: true}, nil
	}

	// The holder's TTL has elapsed: reclaim by deleting unconditionally and
	// recreating the object with a fresh deadline under our token.
	if err := b.client.deleteObject(opCtx, key); err != nil {
		return lockAttempt{}, err
	}
	reclaimDeadline := time.Now().UTC().Add(b.lock.ttl)
	putErr := b.putLock(opCtx, key, token, reclaimDeadline, true)
	if putErr == nil {
		return b.claim(opCtx, key, token)
	}
	if !errors.Is(putErr, errS3PreconditionFailed) {
		return lockAttempt{}, putErr
	}
	// Somebody else's create won the race for the object this call just
	// deleted: that creator is another acquirer we observed, even though
	// the reclaim itself lost.
	return lockAttempt{observed: true}, nil
}

// claim verifies that the lock object this call just wrote is still ours
// (guarding against a concurrent writer racing in between the PUT and this
// check) and, if so, starts the heartbeat and returns the release closure. A
// foreign token found on the object counts as observing another acquirer,
// covering both callers: the fresh-create race and the post-reclaim race.
func (b *Backend) claim(opCtx context.Context, key, token string) (lockAttempt, error) {
	ours, err := b.verifyOwner(opCtx, key, token)
	if err != nil {
		return lockAttempt{}, err
	}
	if !ours {
		return lockAttempt{observed: true}, nil
	}
	//nolint:contextcheck // startHeartbeat's release closure deliberately builds its own fresh
	// context instead of reusing opCtx: opCtx may already be canceled by the time release runs.
	return lockAttempt{release: b.startHeartbeat(key, token)}, nil
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
		return err // unreachable for this fixed four-string struct; kept as a defensive guard.
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

// waitCeilingErr classifies why the wait-ceiling context ended. The
// discriminator is positive evidence, not timing: observed reports whether
// any attempt during this wait completed an answer from the backend that
// another acquirer holds the lock (see lockAttempt), accumulated by
// acquireLock across the whole wait and never reset - one observation
// anywhere in the wait is enough. inFlight is the raw error the attempt in
// progress when the ceiling fired returned, if any; it is consulted only
// when observed is false.
//
// The check order is load-bearing:
//
//  1. parent.Err() != nil: the caller's own context was canceled or hit its
//     deadline first. That error is propagated unchanged, regardless of
//     observed or inFlight, so the exit-code taxonomy still maps a caller
//     Ctrl-C to ExitInterrupt.
//  2. observed: some attempt in this wait already produced a completed
//     answer that another acquirer holds the lock. This is genuine
//     contention: errS3LockWaitTimeout. Checked before inFlight,
//     deliberately: the acquisition loop alternates an attempt and a
//     backoff sleep, so the ceiling lands inside an in-flight attempt about
//     as often as inside a sleep, and ordinary contention therefore ends
//     with inFlight non-nil about as often as it ends with inFlight nil.
//     Testing inFlight first would misclassify ordinary contention as an
//     unreachable backend purely on where the ceiling happened to land.
//  3. inFlight != nil: the ceiling fired with a request still outstanding
//     and no prior observation - errS3LockWaitNoHolderObserved, wrapping
//     inFlight for diagnostic context.
//  4. otherwise: the ceiling fired during a backoff sleep with no prior
//     observation at all - a misbehaving backend that never once produced
//     a usable answer (e.g. a create-if-absent PUT that keeps answering
//     412 while a follow-up HEAD keeps reporting the object missing).
//     Also errS3LockWaitNoHolderObserved, with no cause to wrap.
//
// RESIDUAL: stale evidence, and only in the hanging/silent case. An
// attempt's error only ever reaches this function once the ceiling has
// already fired - acquireLockAttemptErr returns any attempt error
// unclassified while waitCtx is still live, so a backend that keeps
// answering, even with failures, never gets this far (see the next
// paragraph). The residual is real only when this run observed a holder
// once, early in the wait, and the backend then goes silent - hangs, or
// stops answering at all - for the rest of the wait: errS3LockWaitTimeout
// still reports contention in that case, since it claims "at some point
// during this wait the backend told us another acquirer had it", never "the
// backend was healthy when we gave up".
// TestLockAcquireTimesOutAfterObservationThenSilence (lock_test.go) is this
// residual's executable proof: an observed holder followed by a HEAD that
// never answers again until the ceiling.
//
// A backend that answers only with failures is a different case, not this
// residual: any attempt error while the wait ceiling is still live ends the
// whole acquisition immediately (acquireLockAttemptErr's own live-waitCtx
// branch), so a HEAD/PUT/DELETE that comes back with its own 5xx surfaces
// that call's own errS3*Failed sentinel directly, not either wait-ceiling
// one. It only reaches errS3LockWaitNoHolderObserved in the narrower case
// where that attempt - including its own internal client-level retries - is
// still unresolved once the ceiling has already elapsed. Either way the
// class is identical, since errS3*Failed already carries
// helpers.ErrCacheBackendUnavailable on its own.
// TestLockAcquireSurfacesAHardFailureOverTheCeiling (lock_test.go) is this
// paragraph's executable proof.
//
// The cause is rendered with %v, never %w: this sentinel describes why work
// ended, so leaving context.DeadlineExceeded/context.Canceled reachable
// through errors.Is here would hand a hostile or dead endpoint the interrupt
// exit code instead of the network one, since exitcode.FromError checks
// cancellation first.
func waitCeilingErr(parent context.Context, observed bool, inFlight error) error {
	if err := parent.Err(); err != nil {
		return err
	}
	if observed {
		return errS3LockWaitTimeout
	}
	if inFlight != nil {
		//nolint:errorlint // deliberately %v, not %w: see the doc comment above.
		return fmt.Errorf("%w: %v", errS3LockWaitNoHolderObserved, inFlight)
	}
	return errS3LockWaitNoHolderObserved
}

// lockBackoff sleeps a full-jitter exponential backoff interval before the
// next acquisition attempt, honoring waitCtx cancellation. It returns
// waitCeilingErr(parent, observed, nil) once waitCtx (the shared
// wait-ceiling context derived from parent) expires: nil for inFlight
// because no request is outstanding during a backoff sleep, and observed is
// acquireLock's own accumulated flag, passed through unchanged.
func (b *Backend) lockBackoff(parent, waitCtx context.Context, attempt int, observed bool) error {
	timer := time.NewTimer(helpers.BackoffDelay(b.lock.backoffBase, b.lock.backoffCap, attempt))
	defer timer.Stop()
	select {
	case <-waitCtx.Done():
		return waitCeilingErr(parent, observed, nil)
	case <-timer.C:
		return nil
	}
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
