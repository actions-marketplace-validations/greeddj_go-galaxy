package collections

// This file proves buildPrefetchTasks's parallel, Workers-bounded scan: that
// it actually runs concurrently rather than sequentially (Test A), and that
// the concurrent scan schedules the exact same set a sequential scan would,
// including the fail-open-on-Has-error case and the already-installed skip
// (Test B). Both stubs here only need to satisfy cacheManager.ArtifactStore's
// Has probe - Fetch, TempFile, Commit, and Delete are never called by
// buildPrefetchTasks, so they return a sentinel error to make an accidental
// call fail loudly instead of silently.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// errStubNotImplemented is returned by the stub ArtifactStore methods this
// file's tests never exercise, so an unexpected call fails the test loudly
// instead of returning a misleadingly successful zero value.
var errStubNotImplemented = errors.New("stub: method not implemented")

// concurrentProbeArtifacts is a stub cacheManager.ArtifactStore whose Has
// blocks every caller until `target` probes are simultaneously in flight,
// recording peak observed concurrency along the way. This lets a test
// distinguish a genuinely parallel scan from a sequential one: a sequential
// scan never has more than one Has call in flight, so it never reaches
// `target` and never unblocks on its own, escaping only via the caller's
// ctx.Done() - which is exactly the timeout a broken (sequential)
// implementation would hit.
type concurrentProbeArtifacts struct {
	gate       chan struct{}
	mu         sync.Mutex
	target     int
	inFlight   int
	peak       int
	gateClosed bool
}

// Has blocks until concurrentProbeArtifacts.target calls are simultaneously
// in flight (or ctx is done), tracking peak concurrency in a.peak. The gate
// closes only once: with Workers < len(collections), inFlight can climb back
// up to target in a later batch too, and closing an already-closed channel
// would panic.
func (a *concurrentProbeArtifacts) Has(ctx context.Context, _ string) (bool, error) {
	a.mu.Lock()
	a.inFlight++
	if a.inFlight > a.peak {
		a.peak = a.inFlight
	}
	if a.inFlight == a.target && !a.gateClosed {
		a.gateClosed = true
		close(a.gate)
	}
	a.mu.Unlock()

	select {
	case <-a.gate:
	case <-ctx.Done():
	}

	a.mu.Lock()
	a.inFlight--
	a.mu.Unlock()
	return false, nil
}

func (a *concurrentProbeArtifacts) Fetch(context.Context, string) (cacheManager.ArtifactFile, error) {
	return cacheManager.ArtifactFile{}, errStubNotImplemented
}

func (a *concurrentProbeArtifacts) TempFile(context.Context, string) (*os.File, func(), error) {
	return nil, nil, errStubNotImplemented
}

func (a *concurrentProbeArtifacts) Commit(context.Context, string, string, map[string]string) (cacheManager.ArtifactFile, error) {
	return cacheManager.ArtifactFile{}, errStubNotImplemented
}

func (a *concurrentProbeArtifacts) Delete(context.Context, string) error {
	return errStubNotImplemented
}

// TestBuildPrefetchTasksProbesConcurrentlyBoundedByWorkers proves
// buildPrefetchTasks fans its Has probes out in parallel, bounded by
// cfg.Workers.
//
// Discrimination: with Workers=2 and a gate that closes at 2 concurrent
// probes, a correct parallel implementation reaches peak==2 almost instantly
// and returns all 6 tasks well inside the 2s timeout. A sequential
// implementation never has more than one Has call in flight, so it never
// reaches the target of 2 and never closes the gate; each of its Has calls
// then blocks until ctx.Done() fires, so the test would both record peak==1
// and run out the full 2s timeout before the first call even returns.
func TestBuildPrefetchTasksProbesConcurrentlyBoundedByWorkers(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	const target = 2
	art := &concurrentProbeArtifacts{gate: make(chan struct{}), target: target}
	cfg := &config.Config{Workers: 2, DownloadPath: t.TempDir()}
	root := newTestCollectionsRoot(t, cfg.DownloadPath)
	deps := newPrefetchDeps(cfg, infra.New(noopPrinter{}, http.DefaultClient), store.New(), art, root)

	const n = 6
	collections := make(map[string]collection, n)
	for i := range n {
		col := collection{Namespace: "acme", Name: fmt.Sprintf("col%d", i), Version: "1.0.0", Type: "galaxy"}
		collections[col.key()] = col
	}

	p := &prefetcher{done: make(map[string]chan struct{})}
	tasks := buildPrefetchTasks(ctx, deps, collections, p)

	if len(tasks) != n {
		t.Fatalf("len(tasks) = %d, want %d", len(tasks), n)
	}
	if art.peak != target {
		t.Fatalf("peak concurrent Has calls = %d, want %d (scan must be Workers-bounded parallel)", art.peak, target)
	}
}

// presenceArtifacts is a deterministic stub cacheManager.ArtifactStore keyed
// by artifact key: present reports which keys are already cached, and errKeys
// reports which keys' Has call should fail instead.
type presenceArtifacts struct {
	present map[string]bool
	errKeys map[string]bool
}

// errStubHas is the error presenceArtifacts.Has returns for a key listed in
// errKeys, standing in for a transient cache-probe failure.
var errStubHas = errors.New("stub: has probe failed")

func (a *presenceArtifacts) Has(_ context.Context, key string) (bool, error) {
	if a.errKeys[key] {
		return false, errStubHas
	}
	return a.present[key], nil
}

func (a *presenceArtifacts) Fetch(context.Context, string) (cacheManager.ArtifactFile, error) {
	return cacheManager.ArtifactFile{}, errStubNotImplemented
}

func (a *presenceArtifacts) TempFile(context.Context, string) (*os.File, func(), error) {
	return nil, nil, errStubNotImplemented
}

func (a *presenceArtifacts) Commit(context.Context, string, string, map[string]string) (cacheManager.ArtifactFile, error) {
	return cacheManager.ArtifactFile{}, errStubNotImplemented
}

func (a *presenceArtifacts) Delete(context.Context, string) error {
	return errStubNotImplemented
}

// seedAlreadyInstalled makes installRecordMatches report col as already
// installed under cfg.DownloadPath: it creates the install directory, a
// valid .extract-done.<sha> marker, and the sibling
// <ns>.<name>-<version>.info/GALAXY.yml file installRecordMatches requires,
// and records a matching entry in st. This is what drives
// shouldSchedulePrefetch's installRecordMatches-is-true branch, which the
// concurrent scan inherits unchanged from the pre-rewrite sequential one.
func seedAlreadyInstalled(t *testing.T, cfg *config.Config, st *store.Store, col collection) {
	t.Helper()
	const installedSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	target := newTestInstallTarget(t, cfg, col)
	if err := os.MkdirAll(target.path, helpers.DirMod); err != nil {
		t.Fatalf("mkdir installPath: %v", err)
	}
	seedValidExtractMarker(t, target, installedSHA)
	infoDir := filepath.Join(cfg.DownloadPath, "ansible_collections", col.Namespace+"."+col.Name+"-"+col.Version+".info")
	if err := os.MkdirAll(infoDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir infoDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(infoDir, "GALAXY.yml"), []byte("format_version: 1.0.0\n"), helpers.FileMod); err != nil {
		t.Fatalf("write GALAXY.yml: %v", err)
	}
	st.SetInstalled(col.key(), store.InstalledEntry{
		InstallPath:    target.path,
		ArtifactSHA256: installedSHA,
		InstalledAt:    time.Now().UTC(),
	})
}

// TestBuildPrefetchTasksSchedulesExactlyTheRightSet proves the concurrent scan
// applies the exact same predicate a sequential scan would, including its
// fail-open behavior on a Has() error: c1 is absent so it is scheduled, c2 is
// present so it is skipped, c3's Has errors so it is scheduled anyway
// (fail-open), c4 is not a Galaxy-type source so it is skipped regardless of
// cache state, and c5 is already installed (canSkipInstall reports true) so
// it is skipped without ever reaching the Has probe.
func TestBuildPrefetchTasksSchedulesExactlyTheRightSet(t *testing.T) {
	t.Parallel()
	c1 := collection{Namespace: "acme", Name: "absent", Version: "1.0.0", Type: "galaxy"}
	c2 := collection{Namespace: "acme", Name: "present", Version: "1.0.0", Type: "galaxy"}
	c3 := collection{Namespace: "acme", Name: "erroring", Version: "1.0.0", Type: "galaxy"}
	c4 := collection{Namespace: "acme", Name: "nongalaxy", Version: "1.0.0", Type: "git"}
	c5 := collection{Namespace: "acme", Name: "installed", Version: "1.0.0", Type: "galaxy"}

	art := &presenceArtifacts{
		present: map[string]bool{artifactKey(c2): true},
		errKeys: map[string]bool{artifactKey(c3): true},
	}
	cfg := &config.Config{Workers: 2, DownloadPath: t.TempDir()}
	st := store.New()
	seedAlreadyInstalled(t, cfg, st, c5)
	root := newTestCollectionsRoot(t, cfg.DownloadPath)
	deps := newPrefetchDeps(cfg, infra.New(noopPrinter{}, http.DefaultClient), st, art, root)

	collections := map[string]collection{
		c1.key(): c1,
		c2.key(): c2,
		c3.key(): c3,
		c4.key(): c4,
		c5.key(): c5,
	}

	p := &prefetcher{done: make(map[string]chan struct{})}
	tasks := buildPrefetchTasks(t.Context(), deps, collections, p)

	want := map[string]bool{c1.key(): true, c3.key(): true}
	got := make(map[string]bool, len(tasks))
	for _, col := range tasks {
		got[col.key()] = true
	}
	if len(got) != len(tasks) {
		t.Fatalf("buildPrefetchTasks returned duplicate keys: %v", tasks)
	}
	if len(got) != len(want) || !got[c1.key()] || !got[c3.key()] {
		t.Fatalf("scheduled set = %v, want %v", got, want)
	}
	if got[c5.key()] {
		t.Fatalf("c5 (already installed) must not be scheduled: %v", got)
	}

	if len(p.done) != len(want) {
		t.Fatalf("p.done has %d entries, want %d: %v", len(p.done), len(want), p.done)
	}
	for key := range want {
		if _, ok := p.done[key]; !ok {
			t.Fatalf("p.done missing registered key %s", key)
		}
	}
}
