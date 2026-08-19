package collections

// This file drives prewarmRootMetadata only indirectly, through
// resolveCollectionsInternal - exactly as production code reaches it - since
// prewarmRootMetadata itself returns nothing and swallows every error, so
// there is nothing to assert on directly at its own call boundary. Every
// test here shares one fixture shape: a fakegalaxy server, a hand-built
// *config.Config, and an *infra.Infra wired through a concurrencyBarrier so
// a test can observe how many metadata requests this run's HTTP client had
// in flight simultaneously, not merely how many it made in total.

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// concurrencyBarrierTimeout bounds how long concurrencyBarrier.RoundTrip
// ever blocks a request that never sees enough concurrent siblings to open
// the barrier on its own. It is paid at most once per test - and only by a
// run whose request shape never reaches want - so it is long enough to never
// flake under CI load without meaningfully slowing a passing run.
const concurrencyBarrierTimeout = 2 * time.Second

// concurrencyBarrier is an http.RoundTripper that wraps another one and
// blocks each request until want requests are simultaneously in flight
// through it, opening exactly once and staying open for every request that
// follows. It exists to observe genuine transport-level concurrency - how
// many requests a caller had outstanding at once - rather than merely how
// many requests were made in total, which a fully sequential caller could
// also produce given enough of them.
//
// The wrapped base transport must not cap per-host connections below want:
// http.DefaultTransport's own default (MaxIdleConnsPerHost, separate from
// the concurrency this type enforces) is generous enough for every want this
// package's tests use, and httptest.Server.Client()'s own transport (the
// base every test here wraps) carries no host-level cap of its own either -
// but a transport that did cap connections below want would make every
// request past the cap queue behind an already-open connection instead of
// dialing a new one, defeating the very concurrency this type exists to
// observe.
type concurrencyBarrier struct {
	base     http.RoundTripper
	open     chan struct{}
	mu       sync.Mutex
	timeout  time.Duration
	want     int
	inFlight int
	peak     int
	total    int
	opened   bool
}

// RoundTrip blocks a request behind the barrier until want requests are
// simultaneously in flight (or until timeout elapses, or the request's own
// context ends - whichever comes first), then delegates to base.
func (b *concurrencyBarrier) RoundTrip(req *http.Request) (*http.Response, error) {
	if ch := b.enter(); ch != nil {
		timer := time.NewTimer(b.timeout)
		defer timer.Stop()
		select {
		case <-ch:
		case <-timer.C:
			b.release()
		case <-req.Context().Done():
			b.release()
		}
	}
	resp, err := b.base.RoundTrip(req)
	b.leave()
	return resp, err
}

// enter records one more in-flight request, updates total and peak, and
// opens the barrier - exactly once, the moment inFlight first reaches want -
// so every request blocked on it, and every one that arrives afterward,
// proceeds without waiting further. It returns the channel RoundTrip must
// select on when the barrier is not yet open, or nil once it is (RoundTrip
// then skips the select entirely).
func (b *concurrencyBarrier) enter() chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.inFlight++
	b.total++
	b.peak = max(b.peak, b.inFlight)
	if b.inFlight >= b.want && !b.opened {
		b.opened = true
		close(b.open)
	}
	if b.opened {
		return nil
	}
	return b.open
}

// release opens the barrier without inFlight having reached want, used by
// RoundTrip's own timeout and context-cancellation arms so a request that
// gives up waiting does not leave every other request blocked on this
// barrier stuck forever. It is idempotent: a release racing an already-open
// barrier - whether opened by a concurrent enter reaching want, or by
// another release - is a no-op.
func (b *concurrencyBarrier) release() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.opened {
		b.opened = true
		close(b.open)
	}
}

// leave records that one in-flight request completed, once RoundTrip's own
// call to the wrapped base transport returns.
func (b *concurrencyBarrier) leave() {
	b.mu.Lock()
	b.inFlight--
	b.mu.Unlock()
}

// peakConcurrency reports the highest inFlight value enter ever observed:
// the largest number of requests this barrier ever saw outstanding through
// it at the same instant.
func (b *concurrencyBarrier) peakConcurrency() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.peak
}

// requests reports the total number of RoundTrip calls this barrier has
// seen, regardless of how many were ever concurrent - the transport-level
// analog of fakegalaxy.Server.Total, and the only observable available
// against a request a hub-shaped fake server 404s before routing it to any
// counted endpoint (see TestPrewarmSharesTheResolvePhaseAPIRootMemo).
func (b *concurrencyBarrier) requests() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.total
}

// newPrewarmBarrierRuntime builds an *infra.Infra whose HTTP client is a
// fresh http.Client wrapping a fresh concurrencyBarrier armed for want -
// never the *http.Client srv.Client() itself returns, which every other
// fixture in this file, and this one, still needs unmodified for any other
// caller sharing srv.
func newPrewarmBarrierRuntime(srv *fakegalaxy.Server, want int) (*infra.Infra, *concurrencyBarrier) {
	barrier := &concurrencyBarrier{
		base:    srv.Client().Transport,
		open:    make(chan struct{}),
		timeout: concurrencyBarrierTimeout,
		want:    want,
	}
	return infra.New(noopPrinter{}, &http.Client{Transport: barrier}), barrier
}

// unpinnedPrewarmRoots registers n distinct single-version collections
// (acme.c0..acme.cN-1, each at "1.0.0") against srv and returns their roots,
// each an unpinned ("*") requirement pointing at source. It is shared by
// every test in this file that needs several independent, unpinned roots
// against one server.
func unpinnedPrewarmRoots(srv *fakegalaxy.Server, source string, n int) []collection {
	roots := make([]collection, n)
	for i := range roots {
		name := fmt.Sprintf("c%d", i)
		srv.AddVersion("acme", name, "1.0.0", nil)
		roots[i] = collection{Namespace: "acme", Name: name, Version: "*", Constraint: "*", Source: source}
	}
	return roots
}

// exactPinnedPrewarmRoots registers n distinct single-version collections
// (acme.p0..acme.pN-1, each at "1.0.0") against srv and returns their roots,
// each pinned to that exact version.
func exactPinnedPrewarmRoots(srv *fakegalaxy.Server, source string, n int) []collection {
	roots := make([]collection, n)
	for i := range roots {
		name := fmt.Sprintf("p%d", i)
		srv.AddVersion("acme", name, "1.0.0", nil)
		roots[i] = collection{Namespace: "acme", Name: name, Version: "1.0.0", Constraint: "1.0.0", Source: source}
	}
	return roots
}

// TestPrewarmFetchesRootMetadataConcurrently proves prewarmRootMetadata
// actually overlaps its requests, up to cfg.Workers, for a run with more
// unpinned roots than workers - the shape a serial resolve would instead pay
// for one root-metadata round trip at a time.
//
// The four assertions run in this order deliberately: (b) and (d) are each
// pinned by exactly one of this test's two killing mutations (see below),
// and the order proves each is genuinely the first failing line for its own
// mutation - not merely a later one a fatal failure above it would mask.
func TestPrewarmFetchesRootMetadataConcurrently(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	roots := unpinnedPrewarmRoots(srv, srv.URL(), 8)

	runtime, barrier := newPrewarmBarrierRuntime(srv, 4)
	cfg := &config.Config{Server: srv.URL(), Workers: 4, NoDeps: true}
	deps := newCollectionDeps(cfg, runtime, store.New())

	_, _, err := resolveCollectionsInternal(context.Background(), deps, roots, resolveNestedPartial)
	if err != nil {
		t.Fatalf("resolveCollectionsInternal: %v", err)
	}
	// Killing mutation, run: deleting the prewarmRootMetadata(...) call from
	// resolveCollectionsInternal makes every one of these 8 root-metadata
	// requests happen one at a time on the solve's own single goroutine, so
	// peakConcurrency() never exceeds 1. Observed:
	//
	//	peak concurrency = 1, want >= 4
	if peak := barrier.peakConcurrency(); peak < 4 {
		t.Fatalf("peak concurrency = %d, want >= 4", peak)
	}
	// Documentary, not pinned by either mutation below: with no prewarm at
	// all, the solve itself still issues exactly one root-metadata request
	// per root (8 distinct collections, one registered version each, no
	// shared apiRoot memo warm ahead of time), so this count alone cannot
	// distinguish the prewarm's presence from its absence - only (b) and (d)
	// do that. It is asserted anyway so a regression changing the resolve's
	// own request shape is still caught here.
	if got := srv.Count(fakegalaxy.EndpointRootMetadata); got != 8 {
		t.Fatalf("root metadata requests = %d, want 8", got)
	}
	// Killing mutation, run: adding `_, _ = mp.Universe(ctx, fqdn)` to
	// prewarmOne's unpinned arm makes every one of the 8 roots also page its
	// versions list during the prewarm - a request the solve itself never
	// needed, since each collection's single registered version already
	// satisfies its unconstrained ("*") requirement via Highest alone.
	// Observed:
	//
	//	versions list requests = 8, want 0
	if got := srv.Count(fakegalaxy.EndpointVersionsList); got != 0 {
		t.Fatalf("versions list requests = %d, want 0", got)
	}
}

// TestPrewarmSkippedUnderRefresh is the mandatory positive control for
// TestPrewarmFetchesRootMetadataConcurrently: the identical 8-unpinned-root
// fixture, but with cfg.Refresh set, which makes
// cacheManager.PolicyForConstraint's non-exact policy write-only (Read:
// false) and therefore
// prewarmPolicyUsable-false for every one of these unconstrained roots -
// prewarmOne skips each one without ever calling the provider, so the
// barrier only ever sees the solve's own strictly sequential requests. This
// simultaneously proves the barrier CAN observe a sequential shape (peak
// stays at exactly 1), which is what makes
// TestPrewarmFetchesRootMetadataConcurrently's own peak >= 4 assertion mean
// what it claims rather than being an artifact of the barrier itself.
func TestPrewarmSkippedUnderRefresh(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	roots := unpinnedPrewarmRoots(srv, srv.URL(), 8)

	runtime, barrier := newPrewarmBarrierRuntime(srv, 4)
	cfg := &config.Config{Server: srv.URL(), Workers: 4, NoDeps: true, Refresh: true}
	deps := newCollectionDeps(cfg, runtime, store.New())

	_, _, err := resolveCollectionsInternal(context.Background(), deps, roots, resolveNestedPartial)
	if err != nil {
		t.Fatalf("resolveCollectionsInternal: %v", err)
	}
	// Killing mutation, run: removing prewarmOne's own
	// prewarmPolicyUsable(cacheManager.PolicyForConstraint(deps.cfg, exact)) check
	// makes every one of these 8 unpinned roots' prewarm goroutines call
	// mp.Highest regardless of --refresh, so up to cfg.Workers (4) of them
	// race the barrier concurrently, same as
	// TestPrewarmFetchesRootMetadataConcurrently's own (b). Observed:
	//
	//	peak concurrency = 4, want 1 (prewarm must be skipped under --refresh)
	if peak := barrier.peakConcurrency(); peak != 1 {
		t.Fatalf("peak concurrency = %d, want 1 (prewarm must be skipped under --refresh)", peak)
	}
	// Documentary: --refresh does not change how many root-metadata requests
	// the solve itself makes for 8 unpinned, single-version roots - only
	// whether the prewarm contributed any of them ahead of time.
	if got := srv.Count(fakegalaxy.EndpointRootMetadata); got != 8 {
		t.Fatalf("root metadata requests = %d, want 8", got)
	}
}

// TestPrewarmSkippedWhenSnapshotReplays proves the prewarmRootMetadata call
// site's position: placed after resolveCollectionsInternal's snapshot-replay
// return, a resolve that replays a persisted snapshot must keep issuing zero
// metadata requests - a warm run's whole point is paying nothing on the
// network, and a prewarm placed above that return would spend it anyway.
//
// The second resolve deliberately runs against a fresh Store seeded with
// only the resolved-snapshot fields recordResolution itself writes -
// ResolvedAll, GraphSnapshot, MetaRequirements, Requirements - never with the
// APICache/DepsCache/Versions entries the first resolve's own solve also
// warmed along the way. Reusing that same warm Store for the second call
// would let a prewarm hoisted above the snapshot-replay check ride an
// already-warm document cache back down to zero requests regardless of
// whether the snapshot-replay path was ever reached first, which would make
// this test pass even under the mutation it exists to catch; a fresh Store
// carrying only the resolved answer is what makes reaching the snapshot
// return before prewarmRootMetadata runs the only way to keep this at zero.
func TestPrewarmSkippedWhenSnapshotReplays(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	roots := unpinnedPrewarmRoots(srv, srv.URL(), 4)

	// want 1: this barrier is wired only for construction-shape consistency
	// with every other test in this file; nothing here asserts on it, since
	// the whole point is that a replayed resolve reaches no transport call
	// at all for this function to have observed.
	runtime, _ := newPrewarmBarrierRuntime(srv, 1)
	cfg := &config.Config{Server: srv.URL(), Workers: 4, NoDeps: true}
	deps := newCollectionDeps(cfg, runtime, store.New())

	resolved, graph, err := resolveCollectionsInternal(context.Background(), deps, roots, resolveTopLevel)
	if err != nil {
		t.Fatalf("first resolveCollectionsInternal: %v", err)
	}
	srv.ResetCounts()

	replaySt := store.New()
	reqSpec := buildRequirementsSpec(roots)
	reqHash := requirementsSignatureFromSpec(reqSpec, cfg.NoDeps, serversSignature(cfg))
	recordResolution(replaySt, resolved, graph, reqHash, cfg.Server, reqSpec)
	replayDeps := newCollectionDeps(cfg, runtime, replaySt)

	if _, _, err := resolveCollectionsInternal(context.Background(), replayDeps, roots, resolveTopLevel); err != nil {
		t.Fatalf("second resolveCollectionsInternal: %v", err)
	}
	// Killing mutation, run: hoisting the prewarmRootMetadata(...) call above
	// resolveCollectionsInternal's snapshot-replay block makes this second,
	// snapshot-satisfied call fire the prewarm anyway, issuing one
	// root-metadata request per unpinned root before the snapshot is ever
	// consulted. Observed:
	//
	//	srv.Total() = 4, want 0 (a snapshot-replay resolve must issue zero metadata requests)
	if total := srv.Total(); total != 0 {
		t.Fatalf("srv.Total() = %d, want 0 (a snapshot-replay resolve must issue zero metadata requests)", total)
	}
}

// TestPrewarmSkippedForExactPinWithNoDeps proves prewarmOne's
// exact-pin-under-no-deps skip: the solve makes no request at all for such a
// root (NewNoDepsProvider's Dependencies never reaches the wrapped
// provider), so a prewarm would be the only request issued for it - and this
// asserts none is. Its positive control is
// TestPrewarmFetchesVersionMetadataForExactPin, the identical fixture shape
// with cfg.NoDeps flipped to false.
func TestPrewarmSkippedForExactPinWithNoDeps(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	roots := exactPinnedPrewarmRoots(srv, srv.URL(), 2)

	runtime, _ := newPrewarmBarrierRuntime(srv, 1)
	cfg := &config.Config{Server: srv.URL(), Workers: 2, NoDeps: true}
	deps := newCollectionDeps(cfg, runtime, store.New())

	if _, _, err := resolveCollectionsInternal(context.Background(), deps, roots, resolveNestedPartial); err != nil {
		t.Fatalf("resolveCollectionsInternal: %v", err)
	}
	// Killing mutation, run: removing prewarmOne's `exact && deps.cfg.NoDeps`
	// skip makes every one of these 2 exactly-pinned roots' prewarm
	// goroutines call mp.Dependencies despite --no-deps, each costing one
	// root-metadata request and one version-detail request the solve itself
	// never makes for such a root. Observed:
	//
	//	srv.Total() = 4, want 0 (an exactly pinned root under --no-deps must issue zero metadata requests)
	if total := srv.Total(); total != 0 {
		t.Fatalf("srv.Total() = %d, want 0 (an exactly pinned root under --no-deps must issue zero metadata requests)", total)
	}
}

// TestPrewarmFetchesVersionMetadataForExactPin is
// TestPrewarmSkippedForExactPinWithNoDeps's positive control: the identical
// exactly-pinned fixture shape, with cfg.NoDeps false, proves prewarmOne's
// exact-pin arm genuinely calls mp.Dependencies and genuinely overlaps that
// call across roots up to cfg.Workers - not merely that the no-deps skip
// above leaves nothing else to test.
func TestPrewarmFetchesVersionMetadataForExactPin(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	roots := exactPinnedPrewarmRoots(srv, srv.URL(), 4)

	runtime, barrier := newPrewarmBarrierRuntime(srv, 4)
	cfg := &config.Config{Server: srv.URL(), Workers: 4}
	deps := newCollectionDeps(cfg, runtime, store.New())

	if _, _, err := resolveCollectionsInternal(context.Background(), deps, roots, resolveNestedPartial); err != nil {
		t.Fatalf("resolveCollectionsInternal: %v", err)
	}
	// Killing mutation, run: returning nil from prewarmOne's exact arm
	// without ever calling mp.Dependencies leaves every request to the
	// solve's own single goroutine, so no two of these 4 roots' fetches ever
	// overlap. Observed:
	//
	//	peak concurrency = 1, want >= 4
	if peak := barrier.peakConcurrency(); peak < 4 {
		t.Fatalf("peak concurrency = %d, want >= 4", peak)
	}
	// Documentary: with or without the exact-pin prewarm arm, the total
	// request shape for 4 exactly-pinned, dependency-following roots is
	// identical - one root-metadata request and one version-detail request
	// per root, whichever goroutine (prewarm's or the solve's own) happens
	// to make it - so this pair alone does not distinguish the prewarm's
	// presence from its absence; only (b) above does.
	rootMeta, detail := srv.Count(fakegalaxy.EndpointRootMetadata), srv.Count(fakegalaxy.EndpointVersionDetail)
	if rootMeta != 4 || detail != 4 {
		t.Fatalf("root metadata = %d, version detail = %d, want 4 and 4", rootMeta, detail)
	}
}

// TestPrewarmSharesTheResolvePhaseAPIRootMemo is the only test in this file
// pinning the shared-deps requirement itself: prewarmOne must build its
// per-root provider over deps - the same collectionDeps
// resolveCollectionsInternal and solveCollections both use - rather than a
// freshly scoped one, so the apiRootMemo a prewarm goroutine populates is
// the very memo the solve afterward reads from, not a copy of it.
//
// A Galaxy NG / Automation Hub shaped server is what makes this observable:
// against it, a plain fakegalaxy.New server's own v3-first route would let
// every request succeed on its very first candidate regardless of whether
// the memo is shared, so a fresh-memo bug would cost nothing extra to
// notice. Against a hub base the winning URL is only the third candidate
// tried: apiRootCandidates puts the galaxy.ansible.com-shaped /api/v3 ahead
// of the hub-shaped /v3, and each apiRoot contributes both trailing-slash
// variants, so two 404s precede the hit - and an unshared memo makes every
// root pay that losing pair instead of only the first one paying it.
//
// srv.Count cannot serve this assertion: a hub 404s the /api/v3 probe
// before ServeHTTP ever routes it to a counted endpoint (see ServeHTTP's own
// doc comment, internal/testing/fakegalaxy/fakegalaxy.go), so those losing
// requests are real round trips this test must be able to see, and only the
// barrier's own transport-level counter sees them.
func TestPrewarmSharesTheResolvePhaseAPIRootMemo(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.NewAtBasePath(t, hubBasePath)
	base := srv.URL() + hubBasePath
	roots := unpinnedPrewarmRoots(srv, base, 3)

	runtime, barrier := newPrewarmBarrierRuntime(srv, 1)
	cfg := &config.Config{Server: base, Workers: 1, NoDeps: true}
	deps := newCollectionDeps(cfg, runtime, store.New())

	if _, _, err := resolveCollectionsInternal(context.Background(), deps, roots, resolveNestedPartial); err != nil {
		t.Fatalf("resolveCollectionsInternal: %v", err)
	}
	// Arithmetic: the hub 404s the galaxy.ansible.com-shaped /api/v3 probe in
	// both its trailing-slash and no-trailing-slash variants before serving
	// the hub-shaped /v3 candidate, so the first collection resolved against
	// this base costs 3 requests (2 losing, 1 winning) and records the
	// winning apiRoot into the shared memo; the second and third each cost 1
	// (their own single winning-apiRoot request; nothing else about them was
	// ever cached, since each is a distinct collection). Prewarm alone costs
	// 3+1+1 = 5. Because solveCollections' own provider is now built the
	// identical way (see solve.go), the solve afterward finds every one of
	// these 3 roots' root-metadata documents already cached under the exact
	// URL prewarm fetched them at, so it costs 0 further requests. Total: 5.
	//
	// Workers is 1 here precisely so those three walks are strictly ordered:
	// with more workers the first wave would all read the memo before any of
	// them had recorded a winner, and a correct run would cost 9 too.
	//
	// Killing mutation, run: building prewarmOne's own provider with
	// NewMetadataProvider(deps.cfg, deps.runtime, deps.st, sources) instead of
	// newMetadataProviderWithDeps(deps, sources) gives every one of the 3
	// prewarm goroutines its own throwaway apiRootMemo instead of sharing
	// deps.apiRoots, so each root repeats the full losing-then-winning walk
	// independently during prewarm (3+3+3 = 9); deps.apiRoots itself is never
	// populated by any of them, so the solve's own provider (built over the
	// real, still-empty deps.apiRoots) repeats the first root's own losing
	// pair once more (2 further requests) before its winning candidate is
	// served from the document cache prewarm warmed - a hit that finally
	// records the winner into deps.apiRoots, leaving 0 for the other two.
	// Observed:
	//
	//	requests() = 11, want 5 (prewarm and the solve must share one apiRootMemo against this base)
	if got := barrier.requests(); got != 5 {
		t.Fatalf("requests() = %d, want 5 (prewarm and the solve must share one apiRootMemo against this base)", got)
	}
}

// TestPrewarmSkippedWithoutStore proves prewarmEnabled's nil-store skip:
// resolveCollectionsInternal explicitly supports a nil store, and with
// nothing this function fetches ever written anywhere, warming ahead of the
// solve would only double the run's metadata traffic rather than saving any
// of it. Its positive control is TestPrewarmFetchesRootMetadataConcurrently,
// the identical 8-unpinned-root, Workers-4 fixture built with a real
// *store.Store instead of nil - proving the skip is specific to the nil
// store, not to the fixture itself.
func TestPrewarmSkippedWithoutStore(t *testing.T) {
	t.Parallel()
	srv := fakegalaxy.New(t)
	roots := unpinnedPrewarmRoots(srv, srv.URL(), 8)

	// want 1: as in TestPrewarmSkippedForExactPinWithNoDeps, this barrier is
	// wired only for construction-shape consistency with every other test in
	// this file; nothing here asserts on peak concurrency, since the whole
	// point is that a nil store keeps every request on the solve's own
	// single goroutine, with no prewarm goroutine ever dispatched.
	runtime, _ := newPrewarmBarrierRuntime(srv, 1)
	cfg := &config.Config{Server: srv.URL(), Workers: 4, NoDeps: true}
	deps := newCollectionDeps(cfg, runtime, nil)

	if _, _, err := resolveCollectionsInternal(context.Background(), deps, roots, resolveNestedPartial); err != nil {
		t.Fatalf("resolveCollectionsInternal: %v", err)
	}
	// Killing mutation, run: removing prewarmEnabled's `deps.st == nil` check
	// lets every one of these 8 unpinned roots' prewarm goroutines call
	// mp.Highest despite the nil store, each costing one root-metadata
	// request with nothing recorded for the solve to read back, so the solve
	// afterward repeats the identical 8 requests on its own single goroutine
	// instead of finding any of them already answered. Observed:
	//
	//	root metadata requests = 16, want 8 (prewarm must be skipped without a store)
	if got := srv.Count(fakegalaxy.EndpointRootMetadata); got != 8 {
		t.Fatalf("root metadata requests = %d, want 8 (prewarm must be skipped without a store)", got)
	}
}
