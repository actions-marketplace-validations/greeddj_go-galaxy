package collections

// This file exercises --clear-cache's real (non-dry-run) branch inside
// initInstall: the wiring between clearCacheIfRequested, Store.ClearCaches,
// and Backend.ClearFiles, plus initInstall's own lock-release-and-close
// failure arm for that same call. The file-selection policy underneath
// Backend.ClearFiles - which filenames store.ClearCacheFiles may delete and
// which it must keep - is already covered by
// internal/galaxy/store/cache_dir_test.go and is deliberately not retested
// here: these tests pin the wiring, not the policy. Store.ClearCaches has no
// file-selection policy at all; it replaces the API, deps, and versions maps
// in memory.

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/cache/local"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/extracted"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// Fixture keys seedClearCacheStore writes and
// TestInitInstallClearCacheWipesArtifactsAndMetadataCaches asserts
// against, named once here so the seeding side and the assertion side
// cannot drift apart.
const (
	clearCacheAPIKey       = "acme.api@1.0.0"
	clearCacheDepsKey      = "acme.deps@1.0.0"
	clearCacheVersionsKey  = "acme.versions"
	clearCacheInstalledKey = "acme.installed@1.0.0"
	clearCacheWarmedKey    = "acme.warmed@1.0.0"
)

// seedClearCacheStore persists a Store at cacheDir through a throwaway local
// backend instance - Open, LoadStore, populate, SaveStore, Close - mirroring
// what a prior real run would have left on disk. It seeds both sides of the
// boundary --clear-cache draws: the API, deps, and versions caches
// Store.ClearCaches empties, and the installed and warmed entries it must
// leave alone. Both sides are seeded here so a caller can assert the
// boundary rather than only the emptied side of it. It returns the fixed
// artifact sha256 the seeded Installed and Warmed entries share, so a caller
// can place a matching extracted-store directory without ever deriving the
// value from the code under test.
func seedClearCacheStore(t *testing.T, cacheDir string) string {
	t.Helper()
	ctx := context.Background()
	sha := strings.Repeat("a", 64)

	seed := local.New(cacheDir)
	if err := seed.Open(ctx); err != nil {
		t.Fatalf("seed Open: %v", err)
	}
	seedStore, err := seed.LoadStore(ctx)
	if err != nil {
		t.Fatalf("seed LoadStore: %v", err)
	}
	// FetchedAt must be stamped now: snapshotData age-evicts an APICache
	// entry against FetchedAt at save time, so a zero-value FetchedAt would
	// be silently dropped before SaveStore ever writes it - and the later
	// "GetAPICache misses" assertion would then pass for the wrong reason,
	// because the entry was never persisted at all rather than because
	// --clear-cache wiped it.
	seedStore.SetAPICache(clearCacheAPIKey, store.APICacheEntry{
		FetchedAt: time.Now().UTC(),
		URL:       "https://example.test/api",
		Body:      []byte("{}"),
	})
	seedStore.SetDepsCache(clearCacheDepsKey, map[string]string{"acme.dep": "*"})
	seedStore.SetVersionsCache(clearCacheVersionsKey, []string{"1.0.0"})
	seedStore.SetInstalled(clearCacheInstalledKey, store.InstalledEntry{
		InstallPath:    filepath.Join("ansible_collections", "acme", "installed"),
		Source:         "https://example.test",
		ArtifactSHA256: sha,
		InstalledAt:    time.Now().UTC(),
	})
	seedStore.SetWarmed(clearCacheWarmedKey, sha)
	if err := seed.SaveStore(ctx, seedStore); err != nil {
		t.Fatalf("seed SaveStore: %v", err)
	}
	if err := seed.Close(ctx); err != nil {
		t.Fatalf("seed Close: %v", err)
	}
	return sha
}

// assertFileRemoved reports a t.Errorf naming label unless path no longer
// exists. Factored out (rather than inlined per call site) to keep
// TestInitInstallClearCacheWipesArtifactsAndMetadataCaches's own cyclomatic
// complexity down: each call below is one statement, not a branch, in the
// caller's own body.
func assertFileRemoved(t *testing.T, label, path string) {
	t.Helper()
	if _, statErr := os.Stat(path); statErr == nil || !os.IsNotExist(statErr) {
		t.Errorf("%s survived, stat err = %v", label, statErr)
	}
}

// assertFileKept reports a t.Errorf naming label unless path still exists.
func assertFileKept(t *testing.T, label, path string) {
	t.Helper()
	if _, statErr := os.Stat(path); statErr != nil {
		t.Errorf("%s removed: %v", label, statErr)
	}
}

// assertCacheEvicted reports a t.Errorf naming label when ok is true, i.e.
// when a Get* lookup on a bucket --clear-cache is supposed to empty still
// found an entry.
func assertCacheEvicted(t *testing.T, label string, ok bool) {
	t.Helper()
	if ok {
		t.Errorf("%s survived --clear-cache", label)
	}
}

// assertCacheKept reports a t.Errorf naming label when ok is false, i.e.
// when a Get* lookup on a bucket --clear-cache must never touch came back
// empty.
func assertCacheKept(t *testing.T, label string, ok bool) {
	t.Helper()
	if !ok {
		t.Errorf("%s was wiped by --clear-cache", label)
	}
}

// clearCacheWipeFixture bundles the on-disk paths
// TestInitInstallClearCacheWipesArtifactsAndMetadataCaches asserts against
// after a real (non-dry-run) --clear-cache run, alongside the config that
// drives it.
type clearCacheWipeFixture struct {
	cfg          *config.Config
	cacheDir     string
	artifactPath string
	sidecarPath  string
	markerPath   string
}

// buildClearCacheWipeFixture seeds a persisted store plus a cached artifact,
// its sha256 sidecar, and an extracted tree (complete with its ready
// marker) under one cacheDir, and returns the paths needed to assert on
// their survival alongside a real (non-dry-run) --clear-cache config.
// Factored out of the test itself to keep that function's own statement
// count under the project's funlen budget.
func buildClearCacheWipeFixture(t *testing.T) clearCacheWipeFixture {
	t.Helper()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}

	sha := seedClearCacheStore(t, cacheDir)

	// A cached artifact and its sha256 sidecar, placed at the exact path the
	// real artifact store would use for this server/filename pair.
	artifactPath := filepath.Join(cacheDir, helpers.ArtifactKey("https://example.test", "acme-app-1.0.0.tar.gz"))
	mustWriteFile(t, artifactPath, []byte("tarball bytes"))
	sidecarPath := artifactPath + helpers.ArtifactSHASidecarSuffix
	mustWriteFile(t, sidecarPath, []byte(sha))

	// An extracted tree keyed by sha, the way extracted.Store.Ensure would
	// have left it, complete with its ready marker.
	extractedDir := filepath.Join(cacheDir, extracted.RootDirName, sha)
	if err := os.MkdirAll(extractedDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir extractedDir: %v", err)
	}
	markerPath := filepath.Join(extractedDir, extracted.ReadyMarker)
	mustWriteFile(t, markerPath, []byte(extracted.ReadyMarkerPayload))

	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections: []\n"))

	return clearCacheWipeFixture{
		cacheDir:     cacheDir,
		artifactPath: artifactPath,
		sidecarPath:  sidecarPath,
		markerPath:   markerPath,
		cfg: &config.Config{
			CacheDir:         cacheDir,
			RequirementsFile: reqPath,
			DownloadPath:     filepath.Join(root, "install"),
			ClearCache:       true,
			Workers:          1,
		},
	}
}

// TestInitInstallClearCacheWipesArtifactsAndMetadataCaches proves
// --clear-cache's real (non-dry-run) branch inside initInstall actually
// wipes the in-memory API/deps/versions caches and the cached artifact
// files on disk, while leaving the installed and warmed bookkeeping - and
// the content-addressable extracted tree they protect - untouched:
// --clear-cache clears the server-response caches and downloaded
// artifacts, never the installed or warmed records, the extracted store,
// or the Bolt database this run had already loaded from. The lock file's
// survival is that same keep policy one layer down and is asserted where
// the policy lives (internal/galaxy/store/cache_dir_test.go), not again
// here; the Bolt database is asserted here as wiring evidence - that the
// destructive call did not take the snapshot this very run is holding. It
// is the positive control TestInitInstallDryRunSkipsClearCache names in
// its own doc comment: that test only proves a dry run refuses to reach
// this branch, which by itself proves nothing about what the branch does
// once it actually runs.
func TestInitInstallClearCacheWipesArtifactsAndMetadataCaches(t *testing.T) {
	t.Parallel()
	fx := buildClearCacheWipeFixture(t)

	printer := &capturingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)

	state, err := initInstall(context.Background(), fx.cfg, runtime)
	if err != nil {
		t.Fatalf("initInstall: %v", err)
	}
	t.Cleanup(func() {
		if state.release != nil {
			_ = state.release()
		}
		_ = state.backend.Close(context.Background())
	})

	// The assertions below are mutually independent - none depends on
	// another having passed - so each reports through t.Errorf (via the
	// assert* helpers below, which call t.Helper() so a failure attributes
	// to this line), never t.Fatalf: a single run reports every line a
	// mutation broke instead of stopping at the first.
	assertFileRemoved(t, "artifact", fx.artifactPath)
	assertFileRemoved(t, "sidecar", fx.sidecarPath)
	assertFileKept(t, "bolt db", filepath.Join(fx.cacheDir, helpers.StoreDBLocal))
	assertFileKept(t, "extracted tree", fx.markerPath)

	_, apiOK := state.store.GetAPICache(clearCacheAPIKey)
	_, depsOK := state.store.GetDepsCache(clearCacheDepsKey)
	_, versionsOK := state.store.GetVersionsCache(clearCacheVersionsKey)
	_, installedOK := state.store.GetInstalled(clearCacheInstalledKey)
	assertCacheEvicted(t, "APICache", apiOK)
	assertCacheEvicted(t, "DepsCache", depsOK)
	assertCacheEvicted(t, "Versions", versionsOK)
	assertCacheKept(t, "Installed entry", installedOK)

	if warmed := state.store.WarmedArtifactSHAByKey(); len(warmed) != 1 {
		t.Errorf("Warmed set = %v, want 1 entry", warmed)
	}
	if printer.hasWarnContaining("--clear-cache") {
		t.Errorf("unexpected --clear-cache warning in a real (non-dry-run) run: %v", printer.warns)
	}
}

// clearCacheFixture builds a cacheDir seeded with a persisted store (via
// seedClearCacheStore, so initInstall's LoadStore has something real to
// load) plus one cached tarball, and a config wired for a real (non-dry-run)
// --clear-cache run against it. When readOnly is true, cacheDir's
// permissions are stripped to read+execute only, after the lock file has
// already been created: ClearFiles's later os.Remove of the tarball then
// fails with a permission error while everything upstream of it (Lock,
// LoadStore) still succeeds, since opening an already-existing file for
// read/write needs no write permission on its parent directory.
func clearCacheFixture(t *testing.T, readOnly bool) (string, *config.Config) {
	t.Helper()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}

	seedClearCacheStore(t, cacheDir)

	// Materialize the lock file before the chmod below. Without this, the
	// read-only variant would fail inside Lock itself (which creates the
	// lock file with O_CREATE, requiring directory write permission) instead
	// of inside ClearFiles, and would prove nothing about the arm under
	// test.
	rel, err := store.AcquireLock(cacheDir)
	if err != nil {
		t.Fatalf("materialize lock file: %v", err)
	}
	if err := rel(); err != nil {
		t.Fatalf("release materializing lock: %v", err)
	}

	mustWriteFile(t, filepath.Join(cacheDir, "sentinel.acme-app-1.0.0.tar.gz"), []byte("cached bytes"))

	if readOnly {
		//nolint:gosec // G302: intentionally read-only (no write bit) to force ClearFiles's os.Remove to fail with a permission error.
		if err := os.Chmod(cacheDir, 0o555); err != nil {
			t.Fatalf("chmod cacheDir read-only: %v", err)
		}
		t.Cleanup(func() {
			if err := os.Chmod(cacheDir, helpers.DirMod); err != nil {
				t.Errorf("restore cacheDir perms: %v", err)
			}
		})
	}

	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections: []\n"))

	cfg := &config.Config{
		CacheDir:         cacheDir,
		RequirementsFile: reqPath,
		DownloadPath:     filepath.Join(root, "install"),
		ClearCache:       true,
		Workers:          1,
	}
	return cacheDir, cfg
}

// TestInitInstallClearCacheFailureReleasesLock proves initInstall's
// clearCacheIfRequested failure arm releases the exclusive backend lock and
// closes the backend before returning the error, so a ClearFiles failure -
// here, a cache directory whose permissions refuse the deletion it
// requires - does not leak the lock and block out every subsequent run
// sharing this cache. Both subtests share one fixture helper
// (clearCacheFixture) so the read-only subtest's refusal cannot be checked
// without the writable subtest demonstrating, on the identical fixture,
// that a real --clear-cache run does succeed.
func TestInitInstallClearCacheFailureReleasesLock(t *testing.T) {
	t.Parallel()

	// "writable cache dir" is the positive control: it demonstrates the
	// shared fixture is capable of reaching and passing ClearFiles, not
	// itself the assertion under test.
	t.Run("writable cache dir", func(t *testing.T) {
		t.Parallel()
		cacheDir, cfg := clearCacheFixture(t, false)
		printer := &capturingPrinter{}
		runtime := infra.New(printer, http.DefaultClient)

		state, err := initInstall(context.Background(), cfg, runtime)
		if err != nil {
			t.Fatalf("initInstall: %v", err)
		}
		t.Cleanup(func() {
			if state.release != nil {
				_ = state.release()
			}
			_ = state.backend.Close(context.Background())
		})

		tarballPath := filepath.Join(cacheDir, "sentinel.acme-app-1.0.0.tar.gz")
		if _, statErr := os.Stat(tarballPath); statErr == nil || !os.IsNotExist(statErr) {
			t.Errorf("expected the tarball to be removed by a real --clear-cache run, stat err = %v", statErr)
		}
	})

	t.Run("read-only cache dir", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root; permission-based removal guard cannot be tested")
		}
		t.Parallel()
		cacheDir, cfg := clearCacheFixture(t, true)
		runtime := infra.New(&capturingPrinter{}, http.DefaultClient)

		_, err := initInstall(context.Background(), cfg, runtime)
		if err == nil {
			t.Fatalf("expected initInstall to fail against a read-only cache dir")
		}
		// Pins that the failure arm propagates the underlying cause rather
		// than swallowing or replacing it. Deliberately not an exit-class or
		// message assertion, so this stays true if the local backend is
		// ever taught to wrap the cause into helpers.ErrCacheBackendUnavailable
		// with %w.
		if !errors.Is(err, fs.ErrPermission) {
			t.Errorf("expected errors.Is(err, fs.ErrPermission), got %v", err)
		}

		// The lock must be released even though the run failed: a fresh
		// acquisition against the same (still read-only) cacheDir must
		// succeed. This is checked before clearCacheFixture's own
		// chmod-restore cleanup runs, so success here proves the release
		// itself - not a later permissions restore - is what unblocks it.
		rel, lockErr := store.AcquireLock(cacheDir)
		if lockErr != nil {
			t.Errorf("lock not released by initInstall: %v", lockErr)
			return
		}
		if err := rel(); err != nil {
			t.Errorf("release re-acquired lock: %v", err)
		}
	})
}
