package collections

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// failureRecorder is a concurrency-safe collector of per-collection install
// failures. It keeps the repo's documented invariant ("failures tracked with
// atomic.Int32") for the hot-path count check between levels, while also
// retaining each failure's cause so the run's final error can name what
// actually went wrong instead of only how many collections failed.
//
// Pointer receivers throughout: recvcheck forbids mixing value and pointer
// receivers on the same type, and record must mutate through a pointer.
type failureRecorder struct {
	causes []error
	mu     sync.Mutex
	n      atomic.Int32
}

// record appends err to the recorder's causes and bumps its count. It is the
// only writer of either field; every other method only reads.
func (r *failureRecorder) record(err error) {
	r.mu.Lock()
	r.causes = append(r.causes, err)
	// Bumped inside the critical section, not after it, so that summary -
	// which reads both fields under the same lock - can never observe a
	// count that trails its own causes. Keeping the two in step is a
	// property of this type rather than of the order its callers happen to
	// run in, and it costs nothing on a path taken only when a collection
	// has already failed.
	r.n.Add(1)
	r.mu.Unlock()
}

// count reports the number of failures recorded so far. Called by the
// level-break check between install levels, so it stays a plain
// atomic.Int32 load rather than taking the mutex, keeping that hot-path
// check as cheap as a single atomic load.
func (r *failureRecorder) count() int32 {
	return r.n.Load()
}

// summary snapshots the recorder into an immutable failureSummary. The
// snapshot is internally consistent - its count always matches the causes it
// carries - whether or not workers are still running, because record moves
// both fields under one lock. In practice it is called once, at the end of a
// level loop or a warm run, by which point every worker has been joined; that
// is when it happens to be called, not a precondition it relies on.
func (r *failureRecorder) summary() failureSummary {
	r.mu.Lock()
	defer r.mu.Unlock()
	// errors.Join(r.causes...) over an empty slice returns nil without
	// allocating (it special-cases n==0 internally), so the success path -
	// zero recorded causes - never allocates here. Do not "optimize" this
	// into an explicit `if len(r.causes) == 0` branch; it would just
	// duplicate what errors.Join already does for free.
	return failureSummary{count: r.n.Load(), cause: errors.Join(r.causes...)}
}

// failureSummary is an immutable snapshot of a failureRecorder, returned by
// value from the level loops and warmCollections. A sync.Mutex cannot be
// copied, so the recorder itself never leaves the frame that owns it; this
// is the shape callers thread through finalizeInstall/warmWithState instead.
type failureSummary struct {
	// cause is the errors.Join of every recorded cause; nil when count == 0.
	cause error
	count int32
}

// join folds another summary into this one: the counts add and the causes
// are joined, so a run that records collection failures and role failures
// separately reports one summary. A nil cause on either side is left out,
// as errors.Join leaves nil out.
func (s failureSummary) join(other failureSummary) failureSummary {
	return failureSummary{count: s.count + other.count, cause: errors.Join(s.cause, other.cause)}
}

// installError builds the run's headline error for the install command. The
// literal "%w for %d collections" wording is part of the run's user-facing
// output and must stay stable: changing it would change what operators and
// log-scrapers see for the same underlying condition.
func (s failureSummary) installError() error {
	return s.wrap(fmt.Errorf("%w for %d collections", helpers.ErrInstallationFailed, s.count))
}

// warmError builds the run's headline error for the warm command. The
// literal "%w: warm failed for %d collections" wording is part of the run's
// user-facing output and must stay stable for the same reason installError's
// does.
//
// This is a separate method from installError, rather than one method taking
// the headline format as a parameter, so both literal strings stay visible
// at their own type instead of being reconstructed from a shared template.
func (s failureSummary) warmError() error {
	return s.wrap(fmt.Errorf("%w: warm failed for %d collections", helpers.ErrInstallationFailed, s.count))
}

// outdatedError builds the run's headline error for the outdated command. The
// literal "%w for %d collections" wording matches installError's own bare
// shape: both name their sentinel and a count with no extra verb, because
// the sentinel alone already says what failed (helpers.ErrInstallationFailed,
// helpers.ErrLatestVersionLookupFailed). warmError prefixes "warm failed"
// instead, since helpers.ErrInstallationFailed alone would not say warm was
// the command that failed.
func (s failureSummary) outdatedError() error {
	return s.wrap(fmt.Errorf("%w for %d collections", helpers.ErrLatestVersionLookupFailed, s.count))
}

// wrap folds headline together with the summary's recorded causes: nil when
// nothing failed, headline unchanged when there is no cause to attach, and a
// *summaryError joining both otherwise.
//
// The middle branch is defensive only and unreachable today: record is the
// sole writer of either field, it is never called with a nil error, and it
// moves count and causes together under one lock, so a nonzero count always
// arrives with a cause. It is kept so the function is total rather than
// resting on that invariant holding forever.
func (s failureSummary) wrap(headline error) error {
	if s.count == 0 {
		return nil
	}
	if s.cause == nil {
		return headline
	}
	return &summaryError{msg: headline.Error(), joined: errors.Join(headline, s.cause)}
}

// summaryError presents a one-line summary message while still exposing the
// full per-collection cause tree through Unwrap.
//
// The message is deliberately the same one-line summary finalizeInstall (and
// warmWithState) have always produced, because every per-collection cause
// was already printed to stderr in real time by the worker that hit it
// ("Failed: %s.%s error: %s") - repeating them in Start's tail would
// duplicate the log, not inform. Unwrap exposes the errors.Join tree instead,
// so errors.Is still matches both helpers.ErrInstallationFailed (the
// headline) and every per-collection sentinel behind it.
//
// Always construct as &summaryError{...}; pointer receivers on both methods.
type summaryError struct {
	joined error
	msg    string
}

// Error returns the one-line headline, never the full joined tree.
func (e *summaryError) Error() string { return e.msg }

// Unwrap exposes the errors.Join tree of headline plus every recorded cause,
// so errors.Is/errors.As walk through to any sentinel behind either.
func (e *summaryError) Unwrap() error { return e.joined }
