package collections

// This file pins two things: artifactDeadlineError's classification table in
// isolation (TestArtifactDeadlineErrorClassification), and the cache-hit
// fetchArtifact arm's deadline enforcement end to end through installCollection
// (TestCachedArtifactFetchHonorsTheDownloadDeadline), reusing the stub pattern
// s3_cache_recovery_test.go established for standing in for a slow S3 object
// read without duplicating that package's unexported fakeS3 test double.
//
// Each test below was verified against a real revert of the production
// change it pins, and this comment quotes the actual output observed:
//
//   - TestArtifactDeadlineErrorClassification, changing deadline.go's %v to
//     %w in the sentinel's rendering of its cause, makes the "own deadline
//     fired while the parent is still live" case fail with:
//     "artifactDeadlineError = artifact download deadline exceeded after 1s:
//     client.Do: context deadline exceeded, must not match
//     context.DeadlineExceeded"
//   - The same test, dropping the parent.Err() != nil guard from
//     artifactDeadlineError, makes the "parent's own deadline (not this
//     acquisition's budget) expired first" case fail with:
//     "artifactDeadlineError = artifact download deadline exceeded after 1s:
//     client.Do: context deadline exceeded, want unchanged client.Do:
//     context deadline exceeded"
//   - TestCachedArtifactFetchHonorsTheDownloadDeadline, reverting
//     fetchArtifact's cache-hit arm to call artifacts.Fetch(ctx, ...)
//     directly (dropping the context.WithTimeout and both
//     artifactDeadlineError calls), makes the test hang until the harness
//     kills it:
//     "panic: test timed out after 30s
//     running tests:
//     TestCachedArtifactFetchHonorsTheDownloadDeadline (30s)"
//     with the stuck goroutine's frame at
//     "github.com/greeddj/go-galaxy/internal/galaxy/collections.
//     (*blockingFetchArtifacts).Fetch(...)" directly beneath
//     "github.com/greeddj/go-galaxy/internal/galaxy/collections.
//     fetchArtifact(...)" in the same trace.
//   - The same test, widening prepareWithRecovery's prepareInstall-error arm
//     from errors.Is(err, helpers.ErrSHA256Mismatch) to an unconditional
//     canRetryCacheHit(deps, fromCache, forceDownload) check, makes it fail
//     with:
//     "Delete calls = 1, want 0 (the deadline is outside the
//     evict-and-refetch recovery class)"
//     confirming the eviction gate really is open in this fixture once the
//     acquisition deadline fires on a cache hit, and that the exact
//     helpers.ErrSHA256Mismatch check is the only thing holding Delete at
//     zero. (This fixture has no Galaxy server configured, so the widened
//     mutation's forced second attempt actually fails at metadata
//     resolution rather than reproducing the deadline error; the call-count
//     assertions are checked ahead of the error-shape ones specifically so
//     this Delete-count failure is the one that surfaces, rather than being
//     masked by an earlier errors.Is(err, ErrArtifactDownloadDeadline)
//     mismatch.)
//   - The same test, deleting artifactDeadlineError's helpers.ErrSHA256Mismatch
//     exclusion, makes the "own deadline fired while parent is live, cause is
//     an artifact integrity failure" case normalize instead of passing
//     through unchanged, failing with:
//     "artifactDeadlineError = artifact download deadline exceeded after 1s:
//     sha256 mismatch: aaaa != bbbb, want unchanged sha256 mismatch: aaaa !=
//     bbbb"

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/cache/local"
	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// artifactDeadlineBudget is the fixed budget every artifactDeadlineError case
// in this file passes; its value is irrelevant to the classification, only
// its presence in the rendered message (checked by
// assertArtifactDeadlineErrorNormalized's caller) matters.
const artifactDeadlineBudget = time.Second

// errTestArtifactDeadlineCause wraps context.DeadlineExceeded, mirroring the
// real shape a stalled http.Client.Do or artifacts.Fetch call produces once
// dlCtx expires: this is what makes the %v-not-%w assertion in
// assertArtifactDeadlineErrorNormalized meaningful. If artifactDeadlineError
// wrapped this with %w instead, errors.Is(got, context.DeadlineExceeded)
// would turn true and steal the exit-code classification exactly as the
// sentinel's own doc comment warns against.
var errTestArtifactDeadlineCause = fmt.Errorf("client.Do: %w", context.DeadlineExceeded)

// artifactDeadlineErrorCase is one artifactDeadlineErrorCases table row.
type artifactDeadlineErrorCase struct {
	buildParent func() (context.Context, context.CancelFunc)
	buildDl     func(parent context.Context) (context.Context, context.CancelFunc)
	err         error
	name        string
	wantSame    bool
}

// artifactDeadlineErrorCases is TestArtifactDeadlineErrorClassification's
// table, hoisted to package level so the test function itself stays within
// the linter's length budget.
//
//nolint:gochecknoglobals // a fixed table consumed by one test, not mutable shared state.
var artifactDeadlineErrorCases = []artifactDeadlineErrorCase{
	{
		name: "this run's own deadline fired while the parent is still live",
		buildParent: func() (context.Context, context.CancelFunc) {
			return context.WithCancel(context.Background())
		},
		buildDl: func(parent context.Context) (context.Context, context.CancelFunc) {
			dlCtx, cancel := context.WithTimeout(parent, time.Nanosecond)
			<-dlCtx.Done()
			return dlCtx, cancel
		},
		err: errTestArtifactDeadlineCause,
	},
	{
		// Without this case, assertArtifactDeadlineErrorNormalized's
		// errors.Is(got, context.Canceled) check could never fail against any
		// case in this table: the only other case reaching that assertion
		// supplies a cause wrapping context.DeadlineExceeded, and got itself
		// wraps only the sentinel via %w, so context.Canceled was never
		// actually reachable to prove the check discriminating. This case
		// closes that gap with the exact shape ErrArtifactDownloadDeadline's
		// own doc comment names as the other real cause: the read-inactivity
		// watchdog aborting a stall by canceling its own derived context,
		// which races (and here, is overtaken by) this acquisition's budget
		// expiring.
		name: "this run's own deadline fired while the parent is still live, cause wraps context.Canceled",
		buildParent: func() (context.Context, context.CancelFunc) {
			return context.WithCancel(context.Background())
		},
		buildDl: func(parent context.Context) (context.Context, context.CancelFunc) {
			dlCtx, cancel := context.WithTimeout(parent, time.Nanosecond)
			<-dlCtx.Done()
			return dlCtx, cancel
		},
		err: fmt.Errorf("body read: %w", context.Canceled),
	},
	{
		// An artifact-integrity failure must keep its own identity even when
		// this acquisition's own deadline expired in the same instant:
		// helpers.ErrSHA256Mismatch is exitcode's ExitIntegrity signal and
		// prepareWithRecovery's evict-and-refetch trigger, and relabeling it
		// into the deadline sentinel here would erase both, with the %v
		// rendering putting it permanently out of errors.Is's reach on top.
		// Killing mutation: deleting artifactDeadlineError's ErrSHA256Mismatch
		// exclusion makes this case normalize instead of passing through
		// unchanged, failing on "want unchanged".
		name: "own deadline fired while parent is live, cause is an artifact integrity failure: error passes through unchanged",
		buildParent: func() (context.Context, context.CancelFunc) {
			return context.WithCancel(context.Background())
		},
		buildDl: func(parent context.Context) (context.Context, context.CancelFunc) {
			dlCtx, cancel := context.WithTimeout(parent, time.Nanosecond)
			<-dlCtx.Done()
			return dlCtx, cancel
		},
		err:      fmt.Errorf("%w: aaaa != bbbb", helpers.ErrSHA256Mismatch),
		wantSame: true,
	},
	{
		// A store that cannot serve as a cache backend keeps its own
		// identity for the same reason. helpers.ErrCacheBackendUnusable is
		// exitcode's ExitUsage signal - "no retry can help, change the
		// configuration" - while the deadline sentinel is ExitNetwork, whose
		// whole meaning is "retry later"; relabeling would tell a CI job to
		// retry a configuration that can never work, and hands the remote the
		// choice of which class it gets, since it controls whether a transfer
		// stalls to the budget before it answers.
		// Killing mutation: deleting artifactDeadlineError's
		// ErrCacheBackendUnusable exclusion fails this case with
		// "artifactDeadlineError = artifact download deadline exceeded after
		// 1s: cache backend cannot be used as configured: endpoint answered
		// with a redirect, want unchanged ...".
		name: "own deadline fired while parent is live, cause is an unusable backend: error passes through unchanged",
		buildParent: func() (context.Context, context.CancelFunc) {
			return context.WithCancel(context.Background())
		},
		buildDl: func(parent context.Context) (context.Context, context.CancelFunc) {
			dlCtx, cancel := context.WithTimeout(parent, time.Nanosecond)
			<-dlCtx.Done()
			return dlCtx, cancel
		},
		err:      fmt.Errorf("%w: endpoint answered with a redirect", helpers.ErrCacheBackendUnusable),
		wantSame: true,
	},
	{
		name: "parent explicitly canceled: error passes through unchanged",
		buildParent: func() (context.Context, context.CancelFunc) {
			parent, cancel := context.WithCancel(context.Background())
			cancel()
			return parent, cancel
		},
		buildDl: func(parent context.Context) (context.Context, context.CancelFunc) {
			return context.WithTimeout(parent, artifactDeadlineBudget)
		},
		err:      context.Canceled,
		wantSame: true,
	},
	{
		// This is the case that actually distinguishes the parent.Err() guard
		// from checking dlCtx.Err() alone: dlCtx is a child of parent, so
		// once parent's own deadline expires first, dlCtx inherits
		// context.DeadlineExceeded from it too - dlCtx.Err() alone cannot
		// tell "my own budget expired" apart from "I merely inherited my
		// parent's expiry". Without the parent.Err() != nil check ahead of
		// it, this would be misclassified as this acquisition's own deadline
		// firing, when the real cause is the caller's context ending first.
		name: "parent's own deadline (not this acquisition's budget) expired first: error passes through unchanged",
		buildParent: func() (context.Context, context.CancelFunc) {
			parent, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
			<-parent.Done()
			return parent, cancel
		},
		buildDl: func(parent context.Context) (context.Context, context.CancelFunc) {
			dlCtx, cancel := context.WithTimeout(parent, artifactDeadlineBudget)
			<-dlCtx.Done()
			return dlCtx, cancel
		},
		err:      errTestArtifactDeadlineCause,
		wantSame: true,
	},
	{
		name: "dlCtx still live: error passes through unchanged",
		buildParent: func() (context.Context, context.CancelFunc) {
			return context.WithCancel(context.Background())
		},
		buildDl: func(parent context.Context) (context.Context, context.CancelFunc) {
			return context.WithTimeout(parent, time.Hour)
		},
		err:      errTestArtifactDeadlineCause,
		wantSame: true,
	},
	{
		name: "nil error stays nil",
		buildParent: func() (context.Context, context.CancelFunc) {
			return context.WithCancel(context.Background())
		},
		buildDl: func(parent context.Context) (context.Context, context.CancelFunc) {
			dlCtx, cancel := context.WithTimeout(parent, time.Nanosecond)
			<-dlCtx.Done()
			return dlCtx, cancel
		},
		err:      nil,
		wantSame: true,
	},
}

// TestArtifactDeadlineErrorClassification pins artifactDeadlineError's
// normalization table in isolation, independent of any HTTP or cache
// plumbing.
func TestArtifactDeadlineErrorClassification(t *testing.T) {
	t.Parallel()
	for _, tc := range artifactDeadlineErrorCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			parent, parentCancel := tc.buildParent()
			defer parentCancel()
			dlCtx, dlCancel := tc.buildDl(parent)
			defer dlCancel()

			got := artifactDeadlineError(parent, dlCtx, artifactDeadlineBudget, tc.err)
			if tc.wantSame {
				assertArtifactDeadlineErrorUnchanged(t, got, tc.err)
				return
			}
			assertArtifactDeadlineErrorNormalized(t, got, tc.err)
		})
	}
}

// TestArtifactDeadlineErrorIsIdempotent asserts a second normalization pass
// over an already-normalized error returns an identical message rather than
// doubling the sentinel or its rendered cause into it - the contract a call
// site relies on when it may normalize once inside a retry attempt and again
// around the whole helpers.Retry loop.
func TestArtifactDeadlineErrorIsIdempotent(t *testing.T) {
	t.Parallel()
	parent, parentCancel := context.WithCancel(t.Context())
	defer parentCancel()
	dlCtx, dlCancel := context.WithTimeout(parent, time.Nanosecond)
	defer dlCancel()
	<-dlCtx.Done()

	once := artifactDeadlineError(parent, dlCtx, artifactDeadlineBudget, errTestArtifactDeadlineCause)
	twice := artifactDeadlineError(parent, dlCtx, artifactDeadlineBudget, once)

	if twice.Error() != once.Error() {
		t.Fatalf("re-normalizing changed the message: once=%q twice=%q", once.Error(), twice.Error())
	}
	if n := strings.Count(twice.Error(), helpers.ErrArtifactDownloadDeadline.Error()); n != 1 {
		t.Fatalf("sentinel text appears %d times in %q, want exactly 1 (idempotency must not double-wrap)", n, twice.Error())
	}
}

// assertArtifactDeadlineErrorUnchanged fails the test unless got is want,
// left untouched by artifactDeadlineError. A nil want is compared with a
// plain nil check (never a mismatched-error footgun, since both sides are
// then the untyped nil the comparison actually needs); a non-nil want is
// compared through errors.Is, matching artifactDeadlineError's own
// pass-through contract of returning err verbatim rather than a copy.
func assertArtifactDeadlineErrorUnchanged(t *testing.T, got, want error) {
	t.Helper()
	if want == nil {
		if got != nil {
			t.Fatalf("artifactDeadlineError = %v, want nil", got)
		}
		return
	}
	if !errors.Is(got, want) {
		t.Fatalf("artifactDeadlineError = %v, want unchanged %v", got, want)
	}
}

// assertArtifactDeadlineErrorNormalized fails the test unless got is cause
// normalized into the sentinel: it matches helpers.ErrArtifactDownloadDeadline,
// it does not also match context.DeadlineExceeded or context.Canceled (the
// %v-not-%w contract), and its message contains cause's own text.
func assertArtifactDeadlineErrorNormalized(t *testing.T, got, cause error) {
	t.Helper()
	if !errors.Is(got, helpers.ErrArtifactDownloadDeadline) {
		t.Fatalf("artifactDeadlineError = %v, want errors.Is ErrArtifactDownloadDeadline", got)
	}
	if errors.Is(got, context.DeadlineExceeded) {
		t.Fatalf("artifactDeadlineError = %v, must not match context.DeadlineExceeded", got)
	}
	if errors.Is(got, context.Canceled) {
		t.Fatalf("artifactDeadlineError = %v, must not match context.Canceled", got)
	}
	if !strings.Contains(got.Error(), cause.Error()) {
		t.Fatalf("artifactDeadlineError = %q, want it to contain the cause %q", got.Error(), cause.Error())
	}
}

// blockingFetchArtifacts wraps a real local.Artifacts store, standing in for
// a slow cache read (the S3 backend's artifact GET streaming a body over
// HTTP, per Fetch's own doc comment): Has, TempFile, Commit, and (when
// blocking is disabled) Fetch itself all delegate to the real local store, so
// a positive-control run against this stub behaves exactly like a real cache
// hit. When blocking is true, Fetch instead ignores the cached bytes on disk
// entirely and blocks on <-ctx.Done(), returning ctx.Err() - reproducing a
// dripped read that never completes on its own. fetchCalls/deleteCalls count
// every call so a test can assert on them.
type blockingFetchArtifacts struct {
	*local.Artifacts

	blocking    bool
	fetchCalls  atomic.Int32
	deleteCalls atomic.Int32
}

// Fetch blocks on ctx until it is done and returns ctx.Err() when blocking is
// set; otherwise it delegates to the real local store.
func (a *blockingFetchArtifacts) Fetch(ctx context.Context, key string) (cacheManager.ArtifactFile, error) {
	a.fetchCalls.Add(1)
	if a.blocking {
		<-ctx.Done()
		return cacheManager.ArtifactFile{}, ctx.Err()
	}
	return a.Artifacts.Fetch(ctx, key)
}

// Delete counts every call before delegating to the real local store.
func (a *blockingFetchArtifacts) Delete(ctx context.Context, key string) error {
	a.deleteCalls.Add(1)
	return a.Artifacts.Delete(ctx, key)
}

// newDeadlineTestFixture builds a cache-primed collection, config, and
// installDeps sharing cacheDir with stub, mirroring
// s3_cache_recovery_test.go's own priming: only Has() needs to report the
// key present for prepareInstall to take the cache-hit fast path, so the
// seeded bytes' actual content is irrelevant when Fetch is stubbed.
func newDeadlineTestFixture(
	t *testing.T,
	cacheDir string,
	stub *blockingFetchArtifacts,
	deadline time.Duration,
) (collection, installDeps) {
	t.Helper()
	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}

	cfg := &config.Config{CacheDir: cacheDir, Workers: 1}
	runtime := infra.New(noopPrinter{}, http.DefaultClient)
	runtime.ArtifactDownloadDeadline = deadline
	st := store.New()

	deps := installDeps{
		collectionDeps: newCollectionDeps(cfg, runtime, st),
		artifacts:      stub,
		root:           newTestCollectionsRoot(t, filepath.Join(t.TempDir(), "install")),
	}
	return col, deps
}

// TestCachedArtifactFetchHonorsTheDownloadDeadline proves fetchArtifact's
// cache-hit arm bounds a slow Fetch with the same acquisition deadline a
// fresh download gets, and that this deadline sits entirely outside
// prepareWithRecovery's evict-and-refetch class: the sentinel never matches
// helpers.ErrSHA256Mismatch, so it must never trigger an eviction. Delete
// being asserted at exactly zero here is a genuine killing assertion against
// a future widening of that recovery arm to something broader than an exact
// helpers.ErrSHA256Mismatch check, not a tautology: calling installCollection
// (rather than fetchArtifact directly) is what exercises prepareWithRecovery
// at all.
func TestCachedArtifactFetchHonorsTheDownloadDeadline(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}

	stub := &blockingFetchArtifacts{Artifacts: local.NewArtifacts(cacheDir), blocking: true}
	col, deps := newDeadlineTestFixture(t, cacheDir, stub, 20*time.Millisecond)
	artifactPath := filepath.Join(cacheDir, artifactKey(col))
	mustWriteFile(t, artifactPath, []byte("stand-in for a cached artifact whose read never completes"))

	err := installCollection(context.Background(), col, deps, nil, nil, downloadResult{})
	if err == nil {
		t.Fatal("expected an error once the download deadline fired, got nil")
	}
	// The call-count assertions are checked ahead of the error-shape ones
	// deliberately: a mutation that widens prepareWithRecovery's
	// prepareInstall-error arm past its exact helpers.ErrSHA256Mismatch check
	// evicts and forces a second attempt, which - in this fixture, with no
	// Galaxy server configured to resolve metadata against - fails at
	// metadata resolution rather than reproducing the deadline error. That
	// changes what err looks like, but Delete was already called once by the
	// time that second attempt ran, so checking deleteCalls first is what
	// lets that specific mutation surface here rather than being masked by
	// the errors.Is(err, ErrArtifactDownloadDeadline) check below.
	if got := stub.fetchCalls.Load(); got != 1 {
		t.Fatalf("Fetch calls = %d, want 1", got)
	}
	if got := stub.deleteCalls.Load(); got != 0 {
		t.Fatalf("Delete calls = %d, want 0 (the deadline is outside the evict-and-refetch recovery class)", got)
	}
	if !errors.Is(err, helpers.ErrArtifactDownloadDeadline) {
		t.Fatalf("expected errors.Is ErrArtifactDownloadDeadline, got %v", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("must not match context.Canceled: %v", err)
	}
}

// TestCachedArtifactFetchDeadlinePositiveControl is the same fixture with
// blocking disabled, proving the stub itself is capable of a normal
// successful cache-hit install and that the 20ms deadline used above is not
// what would fail this scenario absent the injected block.
func TestCachedArtifactFetchDeadlinePositiveControl(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}

	stub := &blockingFetchArtifacts{Artifacts: local.NewArtifacts(cacheDir), blocking: false}
	col, deps := newDeadlineTestFixture(t, cacheDir, stub, 20*time.Millisecond)
	artifactPath := filepath.Join(cacheDir, artifactKey(col))
	mustWriteFile(t, artifactPath, []byte("stand-in for a real cached artifact, unpacked by installCollection below"))

	// installCollection would go on to extract a real tar.gz from the fetched
	// path, which the seeded placeholder bytes above are not; this positive
	// control exercises fetchArtifact itself, the exact call the deadline
	// wraps, rather than the full extraction pipeline beyond it.
	_, err := fetchArtifact(context.Background(), deps, col, nil, true, true)
	if err != nil {
		t.Fatalf("expected the unblocked stub to delegate to the real local store, got %v", err)
	}
	if got := stub.fetchCalls.Load(); got != 1 {
		t.Fatalf("Fetch calls = %d, want 1", got)
	}
}
