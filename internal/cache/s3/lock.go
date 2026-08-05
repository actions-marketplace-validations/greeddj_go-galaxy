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
	// reclaimSettle is how long claimReclaimed pauses between recreating a
	// dead holder's lock object under our token and verifying that the
	// object still records it, giving a competing reclaimer's own write time
	// to arrive where that verification can see it. Zero disables the pause,
	// which is what a test wanting the pre-settle timing sets.
	reclaimSettle time.Duration
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

// acquireLock owns the lifetime of one lock holding: it establishes the
// holder context, runs the acquisition state machine (acquireLockLoop), and
// hands both the holder context and the release closure back on success. The
// holder context is canceled - with errS3LockLost as its cause - the moment
// the heartbeat observes that some other acquirer's token now sits on the
// lock object, so a run that has provably lost exclusivity stops working
// instead of installing, committing, and persisting under a lock it no longer
// holds.
//
// holderCtx is derived from ctx, the caller's own context, and deliberately
// NOT from the wait-ceiling context acquireLockLoop builds internally: that
// one expires lockWaitCeiling (5 minutes) after the acquisition began, so a
// holder context derived from it would kill every run that holds the lock
// for longer than five minutes - which is most of the runs this lock exists
// to protect in the first place.
//
// On failure nothing is held, so holderCancel runs here and the returned
// context is nil; that nil is a shape callers handle rather than an accident
// (see cache.LockLostError's own nil-holder guard). On success holderCancel
// must outlive this function, which is why it is threaded down to the
// heartbeat and to the release closure rather than deferred here.
func (b *Backend) acquireLock(ctx context.Context, key string) (context.Context, func() error, error) {
	holderCtx, holderCancel := context.WithCancelCause(ctx)
	release, err := b.acquireLockLoop(ctx, key, holderCancel)
	if err != nil {
		holderCancel(err)
		return nil, nil, err
	}
	return holderCtx, release, nil
}

// acquireLockLoop creates or reclaims the lock object at key using a
// token-verified, create-if-absent protocol: acquirers race to PUT with
// If-None-Match: *, and an existing object is only ever reclaimed after its
// writer-recorded deadline (via lockExpired) shows its holder's TTL has
// elapsed. All attempts run against waitCtx, a fixed wait-ceiling derived
// from ctx. The release closure returned on success does not use ctx (or
// waitCtx) at all: it runs its own S3 calls on a fresh context, since it may
// be invoked long after acquireLock returned and ctx may by then already be
// canceled (e.g. the install run it belongs to was interrupted) - using it
// would leak the lock object until its TTL elapses instead of releasing it.
//
// holderCancel is carried through every attempt purely so the heartbeat this
// loop eventually starts can reach it; nothing in the acquisition protocol
// itself reads or calls it.
func (b *Backend) acquireLockLoop(ctx context.Context, key string, holderCancel context.CancelCauseFunc) (func() error, error) {
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
		res, err := b.tryAcquireOnce(waitCtx, key, token, holderCancel)
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
// acquireLockLoop's loop. It is split out purely to keep that function under the
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
// mutually exclusive: acquireLockLoop's loop checks err before release, so a
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
// race. holderCancel is carried through to whichever branch ends up starting
// the heartbeat and is never called here.
func (b *Backend) tryAcquireOnce(
	opCtx context.Context, key, token string, holderCancel context.CancelCauseFunc,
) (lockAttempt, error) {
	deadline := time.Now().UTC().Add(b.lock.ttl)
	putErr := b.putLock(opCtx, key, token, deadline, true)
	if putErr == nil {
		// claim itself decides whether this counts as an observation of
		// another acquirer (a foreign token raced in ahead of us) or a
		// successful claim; either way it is this step's whole answer.
		return b.claim(opCtx, key, token, holderCancel)
	}
	if !errors.Is(putErr, errS3PreconditionFailed) {
		return lockAttempt{}, putErr
	}
	return b.reclaimIfExpired(opCtx, key, token, holderCancel)
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
//
// The delete this function issues is unconditional, so the decision it rests
// on - the HEAD above - is re-read immediately before it (recheckBeforeDelete)
// and the recreate's own ownership check is taken after a settle interval
// (claimReclaimed). Both narrow the interval in which two acquirers acting on
// the same expired object end up believing they hold the lock; neither
// arbitrates between them. The only conditional write this client issues is
// create-if-absent (If-None-Match: *), and its delete carries no condition at
// all, so nothing on this path can arbitrate between two writers that each
// observed the object absent, and two reclaimers can still diverge here. What
// bounds a divergence that survives them is the heartbeat's own token check,
// which cancels the holder context (see startHeartbeat and heartbeatTick) once
// the object records somebody else's token.
//
// Every error return taken after the reclaim PUT has been issued abandons the
// object that PUT may have created, so a lock this run never held is released
// on the way out - and when that release itself fails, expires with the TTL
// exactly as it would have anyway; abandonReclaimedLock states what it costs.
func (b *Backend) reclaimIfExpired(
	opCtx context.Context, key, token string, holderCancel context.CancelCauseFunc,
) (lockAttempt, error) {
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
	staleToken := headers.Get("X-Amz-Meta-Token")

	// The holder's TTL has elapsed: reclaim by deleting unconditionally and
	// recreating the object with a fresh deadline under our token.
	if attempt, proceed, err := b.recheckBeforeDelete(opCtx, key, staleToken); !proceed {
		return attempt, err
	}
	if err := b.client.deleteObject(opCtx, key); err != nil {
		return lockAttempt{}, err
	}
	reclaimDeadline := time.Now().UTC().Add(b.lock.ttl)
	putErr := b.putLock(opCtx, key, token, reclaimDeadline, true)
	if putErr == nil {
		attempt, err := b.claimReclaimed(opCtx, key, token, holderCancel)
		if err != nil {
			//nolint:contextcheck // abandonReclaimedLock builds its own fresh context instead of
			// taking opCtx, and must: opCtx dying is one of the ways the claim above fails, so a
			// cleanup running on opCtx would fail for the very reason it was needed.
			b.abandonReclaimedLock(key, token)
		}
		return attempt, err
	}
	if !errors.Is(putErr, errS3PreconditionFailed) {
		// A conditional PUT that failed without answering 412 may still have
		// landed: putObject never retries one precisely because a transport
		// failure there is indistinguishable from a lost success. The abandon
		// is correct in both readings of that ambiguity, since releaseLock
		// deletes only an object recording our token and treats a missing one
		// as already released.
		//
		// DOCUMENTED-UNCOVERED: no test reaches this call. The failure that
		// makes it necessary is a PUT the remote applied whose response never
		// came back, which the in-memory fake cannot model - arming a PUT
		// failure here only ever produces the other reading, where nothing
		// landed and the abandon is a HEAD that finds no object. Removing the
		// call loses exactly the unmodelable case: a reclaim whose PUT landed
		// under a dropped response leaves an object carrying this run's token,
		// with no heartbeat behind it, blocking every other acquirer for a
		// full lock TTL.
		//
		//nolint:contextcheck // abandonReclaimedLock builds its own fresh context instead of
		// taking opCtx for the same reason as the arm above: opCtx is a plausible cause of the
		// PUT failure being cleaned up after, so it cannot be what the cleanup runs on.
		b.abandonReclaimedLock(key, token)
		return lockAttempt{}, putErr
	}
	// Somebody else's create won the race for the object this call just
	// deleted: that creator is another acquirer we observed, even though
	// the reclaim itself lost. Nothing is abandoned here - a 412 proves the
	// object exists and was written by somebody else, so this call created
	// nothing that could outlive it.
	return lockAttempt{observed: true}, nil
}

// recheckBeforeDelete re-reads the lock object immediately before
// reclaimIfExpired's unconditional delete and reports whether that delete
// should still be issued. Its purpose is what the delete would otherwise
// destroy: between the HEAD that judged the holder expired and the delete
// itself, another acquirer can have reclaimed the object and be holding it
// under a fresh deadline, and deleting there takes a live lock away from a
// run that is already working under it. Only an object that still looks like
// the one the expiry decision was taken about - expired, and still recording
// staleToken - is deleted; anything else hands the decision back to the
// acquisition loop untouched.
//
// The three outcomes that decline the delete map onto lockAttempt as follows.
// A vanished object is retryNow, exactly as reclaimIfExpired's own HEAD treats
// it: the create is worth attempting again immediately. A live holder is
// observed, the same contention signal a live holder produces anywhere else. A
// different but still expired token is neither: whoever wrote it holds nothing,
// so it is a lost round rather than a sighting - reporting it as observed
// would let an expired object decide the wait's final classification, which
// reclaimIfExpired's own doc comment states it may not.
//
// A tokenless object stays reclaimable: an absent header compares equal to an
// absent staleToken, so a lock object written without one is deleted rather
// than left to block acquisition until somebody removes it by hand.
//
// This is a narrowing, not a decision procedure. The window it removes is the
// span between the two HEADs; a competing reclaim landing between this HEAD
// and the delete a moment later is still deleted, since this client's delete
// is unconditional and cannot express "only if it still records this token".
func (b *Backend) recheckBeforeDelete(opCtx context.Context, key, staleToken string) (lockAttempt, bool, error) {
	headers, err := b.client.headObject(opCtx, key)
	switch {
	case errors.Is(err, errS3NotFound):
		return lockAttempt{retryNow: true}, false, nil
	case err != nil:
		return lockAttempt{}, false, err
	}
	live := !lockExpired(headers, b.lock.ttl)
	if live || headers.Get("X-Amz-Meta-Token") != staleToken {
		return lockAttempt{observed: live}, false, nil
	}
	return lockAttempt{}, true, nil
}

// claimReclaimed is claim with a settle interval in front of it, used only by
// the reclaim path. The reclaim's create-if-absent PUT proves the object was
// absent when it landed, which is a weaker fact than it looks: a second
// reclaimer working from the same expired object deletes ours and creates its
// own straight after, and both then verify a token each has just written.
// Waiting b.lock.reclaimSettle before verifying gives such a delete-and-create
// time to arrive, so this call's own verifyOwner has a chance to read the
// other acquirer's token and report the loss instead of holding alongside it.
//
// The settle runs BEFORE claim, and that ordering is load-bearing rather than
// stylistic. Verifying after a successful claim would mean undoing one:
// acquireLock builds holderCtx and holderCancel once per acquisition and
// threads the same holderCancel through every attempt of acquireLockLoop, so
// releasing a claim here would cancel the shared holder context, and a later
// attempt in the same loop would then succeed while handing the caller an
// already-canceled context. Settling first leaves nothing to undo: no
// heartbeat has started, no release closure exists, and claim is reused
// unchanged.
//
// Nothing here makes the reclaim mutually exclusive, but the residual is
// narrower than "two writes inside one settle", and the derivation is what
// says how much narrower. Each reclaimer verifies one settle after its own
// PUT, so inside a cluster of writes spanning less than a settle only the last
// writer reads its own token back; every earlier one sees the later write and
// stands down. Two reclaimers both proceed only when their PUTs are separated
// by MORE than a settle, which requires the second one to stall between its
// own recheck HEAD and its own PUT for longer than that. The heartbeat's
// ongoing token check is what ends such a divergence, by canceling the holder
// context of whichever run stops being the object's writer.
func (b *Backend) claimReclaimed(
	opCtx context.Context, key, token string, holderCancel context.CancelCauseFunc,
) (lockAttempt, error) {
	b.settleReclaim(opCtx)
	return b.claim(opCtx, key, token, holderCancel)
}

// settleReclaim waits b.lock.reclaimSettle, or returns early if opCtx ends
// first. A zero or negative interval disables it, which is how a test asks
// for the timing this path had before the settle existed.
//
// A settle that would not fit inside opCtx's remaining budget is skipped
// rather than truncated to what is left of it. opCtx is the acquisition's
// wait-ceiling context, and by the time this runs the reclaim PUT has already
// landed, so budget spent inside the settle is budget the ownership check
// after it never gets. Truncating would make that the ordinary outcome of
// every reclaim near the ceiling; skipping degrades this path to the behavior
// it had before the settle existed, which is the wider double-acquisition
// window this settle set out to narrow.
//
// What the fit rule guarantees is a property of the settle alone, and narrower
// than it reads: the settle itself completes inside the budget, never that the
// ownership check after it does. A budget exceeding the settle by less than
// one round trip passes the check here, pays the settle in full, and then
// fails that ownership check against what is left. A caller cancellation
// reaches the same place from the other side, cutting the settle short and
// failing the check that follows it. Both release the object on the way out:
// every error return taken after the reclaim PUT abandons it
// (abandonReclaimedLock), and a release that itself fails leaves the object to
// expire with the TTL exactly as it would have anyway - which is what bounds
// the consequence this rule does not.
func (b *Backend) settleReclaim(ctx context.Context) {
	if b.lock.reclaimSettle <= 0 {
		return
	}
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= b.lock.reclaimSettle {
		return
	}
	timer := time.NewTimer(b.lock.reclaimSettle)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// claim verifies that the lock object this call just wrote is still ours
// (guarding against a concurrent writer racing in between the PUT and this
// check) and, if so, starts the heartbeat and returns the release closure. A
// foreign token found on the object counts as observing another acquirer,
// covering both paths that reach it: the fresh-create race and, through
// claimReclaimed, the post-reclaim race.
// holderCancel is handed to the heartbeat this call starts, which is the only
// thing that ever invokes it on a loss.
func (b *Backend) claim(
	opCtx context.Context, key, token string, holderCancel context.CancelCauseFunc,
) (lockAttempt, error) {
	ours, err := b.verifyOwner(opCtx, key, token)
	if err != nil {
		return lockAttempt{}, err
	}
	if !ours {
		return lockAttempt{observed: true}, nil
	}
	//nolint:contextcheck // startHeartbeat's release closure deliberately builds its own fresh
	// context instead of reusing opCtx: opCtx may already be canceled by the time release runs.
	return lockAttempt{release: b.startHeartbeat(key, token, holderCancel)}, nil
}

// verifyOwner reports whether the lock object's recorded token matches
// token, i.e. whether this call is (still) the object's writer.
//
// An absent X-Amz-Meta-Token header is a mismatch, not a missing answer, and
// must stay one: the HEAD succeeded, so the backend did answer, and what it
// answered is that nothing on that object identifies this run as its writer.
// Treating that as transient - retry, keep the lock, keep working - would let
// a run go on writing under a lock it may no longer hold, which is the exact
// failure the heartbeat's loss detection exists to stop. A genuinely
// transient failure has its own shape here already: the error return above,
// which heartbeatTick answers by retrying on the next tick.
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
// acquireLockLoop across the whole wait and never reset - one observation
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
// acquireLockLoop's own accumulated flag, passed through unchanged.
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
// release refuses to delete a lock it no longer owns AND cancels the holder
// context through holderCancel so the run itself stops working under a lock
// it no longer holds. Neither
// the heartbeat loop nor the release closure it returns depend on the context
// the caller used to acquire the lock: the heartbeat runs on its own
// independent background context so it keeps refreshing the deadline for as
// long as the holder keeps the lock, and release builds its own fresh,
// timeout-bounded context for its final HEAD/DELETE (see releaseLock) so a
// canceled caller context can never prevent the lock from being released.
func (b *Backend) startHeartbeat(key, token string, holderCancel context.CancelCauseFunc) func() error {
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
				if !b.heartbeatTick(hbCtx, key, token, &lost, holderCancel) {
					return
				}
			}
		}
	}()

	return func() error {
		hbCancel()
		// Two independent facts keep a loss cause readable after release, and
		// the order of the next two statements is one of them. The join below
		// is the HAPPENS-BEFORE: it waits until the heartbeat goroutine has
		// exited, so a tick that was mid-decision has already finished - and
		// already recorded its loss cause - before holderCancel runs at all.
		// first-cancel-wins is the RETENTION: context.CancelCauseFunc keeps
		// the first cause it was given, so the nil below cannot overwrite a
		// loss the heartbeat already set, and cache.LockLostError can still
		// read it afterward.
		//
		// Each fails on its own and neither substitutes for the other.
		// Hoisting holderCancel above the join leaves retention intact and
		// still loses the cause, because the tick has not set one yet - the
		// window is real and is measured by
		// TestReleaseRacingTheTickKeepsTheLossCause (lock_race_test.go),
		// which quotes what that edit reports. Passing an explicit non-nil
		// cause instead of nil leaves the join intact and overwrites nothing,
		// but reclassifies every clean release; that one is caught by
		// TestLockHolderContextStaysLiveWhileOwned's clean-release assertions.
		<-done
		// The holder context always ends here, on the ordinary release path
		// too, so a clean release does not leave a child context hanging off
		// the caller's own for the rest of the process's life.
		holderCancel(nil)
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
// setting lost and canceling the holder context accordingly; transient errors
// (network failures, a momentary S3 hiccup) return true so the next tick
// simply retries rather than declaring the lock lost.
//
// A definitive mismatch is one fact with two consumers, which is why it is
// recorded twice. lost is what the release closure reads, so it refuses to
// delete an object that now belongs to somebody else; holderCancel is what
// the run reads, so it stops doing work it can no longer claim exclusivity
// for. Deriving the first from context.Cause(holderCtx) instead would make
// release's refusal depend on cancellation being first-cancel-wins - the
// release closure cancels the same context on its own ordinary path - so the
// two stay independent.
func (b *Backend) heartbeatTick(
	hbCtx context.Context, key, token string, lost *atomic.Bool, holderCancel context.CancelCauseFunc,
) bool {
	opCtx, cancel := context.WithTimeout(hbCtx, b.lock.heartbeatOpTimeout)
	defer cancel()

	ours, err := b.verifyOwner(opCtx, key, token)
	if err != nil {
		return true
	}
	if !ours {
		lost.Store(true)
		holderCancel(errS3LockLost)
		return false
	}
	// A failure here is transient (e.g. a dropped connection): the next
	// tick will retry with a deadline computed at that later time.
	_ = b.putLock(opCtx, key, token, time.Now().UTC().Add(b.lock.ttl), false)
	return true
}

// releaseLock deletes the lock object only if the HEAD ahead of that delete
// still saw token on it, matching the same token-verified guard as heartbeat
// loss detection, so a release declines the delete for any object it read under
// another acquirer's token. A missing object is treated as already released.
//
// The delete itself is unconditional, so one round trip stands between the HEAD
// and it: a competing acquirer whose write lands in that window is deleted
// anyway, the same residual recheckBeforeDelete discloses for the reclaim
// path's own delete, and bounded by the same backstop - the heartbeat's token
// check cancels the holder context of whichever run stops being the writer.
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

// abandonReclaimedLock releases the lock object a reclaim just created, as the
// cleanup behind one rule: any error reclaimIfExpired returns after its reclaim
// PUT was issued does a best-effort, token-guarded release first, so an object
// created by a reclaim this run never went on to hold is released on the way
// out - and when that release itself fails, is left to expire with the TTL
// exactly as it would have without this call. Without the call that expiry is
// the only outcome: such an object sits in the bucket recording our token with
// no heartbeat behind it to refresh or release it, and every other acquirer
// waits out a full lock TTL against a holder that does not exist - lockTTL is
// twice lockWaitCeiling, so each of them burns its whole wait and gives up. The
// rule is the reclaim path's alone because that is where the settle widens the
// window: tryAcquireOnce's own arms leave the identical orphan - a claim error
// after a successful fresh-create PUT, and a PUT failure that is not a 412 -
// and abandon neither, a residual one round trip wide rather than a settle.
//
// A nil error is deliberately excluded even when the attempt reports observed:
// the object then records the competing writer's token, so releaseLock's own
// guard would decline the delete and the call would be a round trip that
// changes nothing.
//
// Two things it deliberately does not do. It never touches holderCancel: no
// heartbeat has been started on this path - claim starts one, and an error
// here means it did not get that far - so nothing is running under the holder
// context for a cancel to stop, and the cancellation this failure does deserve
// is acquireLock's own, which cancels that context with the acquisition's
// error as the cause once the error returned here reaches it. Cancel-cause is
// first-cancel-wins, so a cancel from here would preempt that cause with a less
// informative one. And it declines the delete for any object releaseLock's own
// HEAD read under a foreign token, so a competing reclaimer's write - the very
// outcome the settle exists to catch - is left untouched; the residual is the
// one round trip between that HEAD and the delete, bounded by the same
// heartbeat token check that bounds every other divergence on this path (see
// releaseLock). Its own failure is discarded because there is nothing further to
// do with it: the run is already returning an error, and an object this call
// could not delete expires on its own after a lock TTL.
func (b *Backend) abandonReclaimedLock(key, token string) {
	// A fresh context, deliberately not the acquisition's: a dead acquisition
	// context - canceled by the caller, or out of wait-ceiling budget - is one
	// of the ways this cleanup is reached, so reusing it would fail the cleanup
	// for the very reason it was needed. It is no worse for the ways that
	// arrive with a live one, since a release wants a budget of its own
	// regardless of what ended the claim. releaseTimeout bounds it rather than
	// a constant of this function's own, since this is the release path's own
	// HEAD/DELETE pair under another name.
	ctx, cancel := context.WithTimeout(context.Background(), b.lock.releaseTimeout)
	defer cancel()
	_ = b.releaseLock(ctx, key, token)
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
