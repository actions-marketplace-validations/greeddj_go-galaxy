package cache

// This file pins LockLostError's decision table: which (parent, holder, err)
// shapes produce a lock-loss verdict, which are returned untouched, and - the
// point of the last two rows - what a verdict deliberately puts out of reach
// of errors.Is.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// errRunFailed stands in for whatever the run itself produced in the rows
// that do not care about its class: only its non-nilness and its identity
// matter there, so one static sentinel serves them all rather than a fresh
// dynamic error per row - the same convention
// internal/cache/s3/lock_test.go's errRawInFlightPlaceholder follows.
var errRunFailed = errors.New("the run's own failure")

// lockLostVerdict names the three answers LockLostError can give, so each row
// below states which one it expects instead of carrying its own assertion
// closure.
type lockLostVerdict int

const (
	// verdictUnchanged means the row's own err must come back matchable, and
	// must not have been reclassified into a lock-loss verdict.
	verdictUnchanged lockLostVerdict = iota
	// verdictBareSentinel means the result must be exactly
	// helpers.ErrCacheLockLost with nothing wrapped around it: the run itself
	// produced no error to render.
	verdictBareSentinel
	// verdictWrapped means the result must match helpers.ErrCacheLockLost
	// through errors.Is and carry the run error's text, while every sentinel
	// in the row's mustNotMatch list stays unreachable through errors.Is.
	verdictWrapped
)

// lockLostCase is one row of TestLockLostError's table. fixture builds the
// two contexts, since a canceled one cannot be written as a literal.
type lockLostCase struct {
	err     error
	fixture func(t *testing.T) (parent, holder context.Context)
	name    string
	// mustNotMatch lists sentinels the verdict must have flattened out of
	// reach; only verdictWrapped rows populate it.
	mustNotMatch []error
	want         lockLostVerdict
}

// liveHolder is the ordinary shape: an uncanceled parent and a holder derived
// from it that is still live, i.e. this run still holds the lock.
func liveHolder(t *testing.T) (context.Context, context.Context) {
	t.Helper()
	holder, cancel := context.WithCancelCause(context.Background())
	t.Cleanup(func() { cancel(nil) })
	return context.Background(), holder
}

// nilHolder is what Backend.Lock returns when nothing was ever acquired, and
// what initInstall propagates from the arm that never reached the lock at
// all. It is a required row rather than a defensive one: context.Cause(nil)
// panics on a nil interface, so without LockLostError's first guard this row
// would not fail an assertion, it would crash the run.
func nilHolder(t *testing.T) (context.Context, context.Context) {
	t.Helper()
	return context.Background(), nil
}

// holderCanceledWith builds a fixture whose holder is already canceled with
// cause, against a live parent - the shape a backend produces the instant it
// learns something about the lock this run holds.
func holderCanceledWith(cause error) func(t *testing.T) (context.Context, context.Context) {
	return func(t *testing.T) (context.Context, context.Context) {
		t.Helper()
		holder, cancel := context.WithCancelCause(context.Background())
		cancel(cause)
		return context.Background(), holder
	}
}

// canceledParentWithLostHolder is the Ctrl-C shape at its most adversarial:
// the holder is canceled with a genuine loss cause FIRST, so cancellation
// being first-cancel-wins keeps that cause readable, and only then does the
// caller cancel. Both facts are therefore true at once, which is what makes
// this row pin the ORDER of LockLostError's checks rather than merely one of
// them: the caller's own cancellation has to outrank a real loss, or an
// interrupted run would report exit 8 instead of exit 130.
func canceledParentWithLostHolder(t *testing.T) (context.Context, context.Context) {
	t.Helper()
	parent, cancelParent := context.WithCancel(context.Background())
	holder, cancelHolder := context.WithCancelCause(parent)
	cancelHolder(helpers.ErrCacheLockLost)
	cancelParent()
	return parent, holder
}

// lockLostCases is TestLockLostError's table, built by a function rather than
// declared as a package-level var so it needs no gochecknoglobals exemption.
func lockLostCases() []lockLostCase {
	return []lockLostCase{
		{
			name:    "a nil holder is returned unchanged",
			fixture: nilHolder,
			err:     errRunFailed,
			want:    verdictUnchanged,
		},
		{
			name:    "a canceled parent outranks a genuinely lost holder",
			fixture: canceledParentWithLostHolder,
			err:     errRunFailed,
			want:    verdictUnchanged,
		},
		{
			name:    "a live holder is returned unchanged",
			fixture: liveHolder,
			err:     errRunFailed,
			want:    verdictUnchanged,
		},
		{
			// The local backend returns the caller's own context as the
			// holder, so a holder that ended for any reason other than a lost
			// lock - a plain cancel, most commonly - must stay whatever it
			// already was.
			name:    "a holder canceled for another reason is returned unchanged",
			fixture: holderCanceledWith(context.Canceled),
			err:     errRunFailed,
			want:    verdictUnchanged,
		},
		{
			// The success path: the run finished every piece of work without
			// an error of its own, and is still a failure, because detection
			// is late by construction and the work was already done without
			// exclusivity by the time the heartbeat noticed.
			name:    "a lost holder with no run error is the bare sentinel",
			fixture: holderCanceledWith(helpers.ErrCacheLockLost),
			err:     nil,
			want:    verdictBareSentinel,
		},
		{
			// The real shape: the run failed BECAUSE the holder context it
			// was working under got canceled, so its own error carries
			// context.Canceled. Rendering that with %w would hand a stolen
			// lock the interrupt exit code, since exitcode.FromError checks
			// context.Canceled ahead of every other class.
			name:         "a lost holder wraps the run error without leaving context.Canceled reachable",
			fixture:      holderCanceledWith(helpers.ErrCacheLockLost),
			err:          fmt.Errorf("save: %w", context.Canceled),
			mustNotMatch: []error{context.Canceled},
			want:         verdictWrapped,
		},
		{
			// Supersession, stated executably: a run that both lost the lock
			// and failed an integrity check reports the lock-loss class, not
			// the integrity one. That is deliberate rather than collateral -
			// once another holder is writing the same cache, this run's own
			// sha256 mismatch may simply be that other holder rewriting an
			// artifact underneath it, so exclusivity is the actionable fact.
			name:         "a lost holder supersedes the run's own integrity failure",
			fixture:      holderCanceledWith(helpers.ErrCacheLockLost),
			err:          fmt.Errorf("install: %w", helpers.ErrSHA256Mismatch),
			mustNotMatch: []error{helpers.ErrSHA256Mismatch},
			want:         verdictWrapped,
		},
	}
}

func TestLockLostError(t *testing.T) {
	t.Parallel()
	for _, tt := range lockLostCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			parent, holder := tt.fixture(t)
			got := LockLostError(parent, holder, tt.err)
			switch tt.want {
			case verdictUnchanged:
				assertLockLostUnchanged(t, tt.err, got)
			case verdictBareSentinel:
				assertLockLostBareSentinel(t, got)
			case verdictWrapped:
				assertLockLostWrapped(t, tt, got)
			}
		})
	}
}

// assertLockLostUnchanged fails the test unless got is still want: not
// reclassified into a lock-loss verdict, and still matchable as the run's own
// error.
//
// The lock-loss check comes first deliberately. A verdict that fired when it
// must not have fails both checks at once - the %v rendering puts want out of
// reach of errors.Is too - so whichever check is written first is the only one
// that can ever report it, and "a verdict fired that must not have" is the
// fact this helper exists to state. That ordering is also what leaves the
// second check reachable on its own, since the shape that fails it alone is a
// guard arm returning nil: no verdict, and no run error either.
//
// KILLING MUTATIONS, run and reverted, one per check - both reported against
// TestLockLostError's own call site, since this helper calls t.Helper().
// Deleting LockLostError's parent.Err() guard, so a caller's own cancellation
// stops outranking a genuinely lost holder, fires the first:
//
//	locklost_test.go:182: LockLostError = cache lock ownership was lost to another holder: the run's own failure, want no lock-loss verdict
//
// Rewriting its third guard's arm to `return nil`, so a holder that ended for
// some other reason erases the run's error instead of returning it, fires the
// second - and leaves the first green, since nil is no verdict:
//
//	locklost_test.go:182: LockLostError = <nil>, want unchanged the run's own failure
func assertLockLostUnchanged(t *testing.T, want, got error) {
	t.Helper()
	if errors.Is(got, helpers.ErrCacheLockLost) {
		t.Fatalf("LockLostError = %v, want no lock-loss verdict", got)
	}
	if !errors.Is(got, want) {
		t.Fatalf("LockLostError = %v, want unchanged %v", got, want)
	}
}

// assertLockLostBareSentinel fails the test unless got is exactly the
// sentinel, with nothing wrapped around it: there was no run error to render,
// so anything else means a message was invented.
func assertLockLostBareSentinel(t *testing.T, got error) {
	t.Helper()
	if !errors.Is(got, helpers.ErrCacheLockLost) {
		t.Fatalf("LockLostError = %v, want errors.Is helpers.ErrCacheLockLost", got)
	}
	if got.Error() != helpers.ErrCacheLockLost.Error() {
		t.Fatalf("LockLostError = %q, want the bare sentinel %q", got.Error(), helpers.ErrCacheLockLost.Error())
	}
}

// assertLockLostWrapped fails the test unless got is a lock-loss verdict that
// carries the run error's text while leaving every sentinel the row listed
// unreachable through errors.Is.
func assertLockLostWrapped(t *testing.T, tt lockLostCase, got error) {
	t.Helper()
	if !errors.Is(got, helpers.ErrCacheLockLost) {
		t.Fatalf("LockLostError = %v, want errors.Is helpers.ErrCacheLockLost", got)
	}
	if !strings.Contains(got.Error(), tt.err.Error()) {
		t.Fatalf("LockLostError = %q, want it to carry the run error %q", got.Error(), tt.err.Error())
	}
	// KILLING MUTATION, run and reverted: rendering the cause with %w instead
	// of %v in LockLostError's final fmt.Errorf leaves both listed sentinels
	// reachable again, failing both verdictWrapped rows of the table (the
	// reported line is TestLockLostError's own call site, since these helpers
	// call t.Helper()):
	//
	//	locklost_test.go:186: still matches context canceled: cache lock ownership was lost to another holder: save: context canceled
	//	locklost_test.go:186: still matches sha256 mismatch: cache lock ownership was lost to another holder: install: sha256 mismatch
	//
	// The two checks above stay green under that mutation - the sentinel
	// still matches and the message still carries the cause's text - so this
	// one is genuinely reachable rather than shadowed by them.
	for _, unreachable := range tt.mustNotMatch {
		if errors.Is(got, unreachable) {
			t.Fatalf("still matches %v: %v", unreachable, got)
		}
	}
}
