package collections

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	"github.com/greeddj/go-galaxy/internal/cache/local"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// A dry run still writes one thing: the resolve-side metadata caches, through
// saveDryRunSnapshotIfPersisted. That write can fail, and until these tests
// nothing confirmed any of the three dry-run commands surfaced the failure at
// all, let alone with the exit class the same failure carries on the real
// path. The gap was uniform across install, warm and lock, so the coverage is
// too: fixing it for one command would have implied the other two already had
// it.
//
// The classified sentinel (helpers.ErrCacheBackendUnavailable, the shape a
// dead bucket's SaveStore produces) is used rather than an opaque one on
// purpose. An opaque sentinel classifies as the generic exit code no matter
// what the code under test does with it, so an exit-code assertion on it
// would hold even if the error were rewrapped into something unrecognizable -
// it is precisely the assertion that cannot fail. This one can.
//
// KILLING MUTATION, run and reverted: making saveDryRunSnapshotIfPersisted
// return nil unconditionally - the shape a dry run would have if it stopped
// saving, or stopped reporting the outcome of saving - fails all three rows on
// the same line, one per command:
//
//	dry_run_save_failure_test.go:115: lock --dry-run swallowed its tail save failure: err = <nil>
//	dry_run_save_failure_test.go:115: install --dry-run swallowed its tail save failure: err = <nil>
//	dry_run_save_failure_test.go:115: warm --dry-run swallowed its tail save failure: err = <nil>

// seedPersistedSnapshot writes an empty snapshot to cacheDir through a real
// local backend and closes it again, so a state built afterwards loads a store
// whose WasPersisted() is true. Without it every test here would take
// saveDryRunSnapshotIfPersisted's other branch - the one that skips the save
// entirely - and prove nothing about a save failure.
func seedPersistedSnapshot(t *testing.T, cacheDir string) {
	t.Helper()

	backend := local.New(cacheDir)
	ctx := context.Background()
	if err := backend.Open(ctx); err != nil {
		t.Fatalf("seeding backend.Open: %v", err)
	}
	if err := backend.SaveStore(ctx, store.New()); err != nil {
		t.Fatalf("seeding SaveStore: %v", err)
	}
	if err := backend.Close(ctx); err != nil {
		t.Fatalf("seeding backend.Close: %v", err)
	}
}

// newDryRunSaveFailureFixture builds a cache directory holding an already
// persisted snapshot, an empty requirements file, and a config in dry-run
// mode, then returns the config and a state whose SaveStore always fails with
// the classified sentinel.
func newDryRunSaveFailureFixture(t *testing.T) (*config.Config, *installState) {
	t.Helper()

	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}
	seedPersistedSnapshot(t, cacheDir)

	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections: []\n"))

	cfg := &config.Config{
		CacheDir:         cacheDir,
		DownloadPath:     filepath.Join(root, "install"),
		RequirementsFile: reqPath,
		LockFile:         filepath.Join(root, "requirements.lock.yml"),
		Workers:          1,
		Offline:          true,
		DryRun:           true,
	}
	state := newCacheBackendUnavailableSaveFailState(t, cfg)
	if !state.store.WasPersisted() {
		t.Fatal("fixture did not produce a persisted snapshot, so the save under test would be skipped")
	}
	return cfg, state
}

// TestDryRunSurfacesItsTailSaveFailure runs all three dry-run commands against
// the identical fixture: a cache that already holds a persisted snapshot, so
// the dry run does attempt its metadata-cache save, and a backend whose
// SaveStore always fails. Each command must return that failure and classify
// it as the network exit class, exactly as its real path does for the same
// backend failure.
func TestDryRunSurfacesItsTailSaveFailure(t *testing.T) {
	t.Parallel()

	for _, tc := range dryRunSaveFailureCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg, state := newDryRunSaveFailureFixture(t)
			runtime := infra.New(noopPrinter{}, nil)

			err := tc.run(context.Background(), cfg, runtime, state)
			if !errors.Is(err, helpers.ErrCacheBackendUnavailable) {
				t.Fatalf("%s --dry-run swallowed its tail save failure: err = %v", tc.name, err)
			}
			if got := exitcode.FromError(err); got != exitcode.ExitNetwork {
				t.Errorf("exitcode.FromError(err) = %d, want ExitNetwork (%d)", got, exitcode.ExitNetwork)
			}
			// The lockfile assertion belongs to every row, not just lock's:
			// a dry run that wrote one would be a different defect, and the
			// save failure must not be the reason it did not.
			if _, statErr := os.Stat(cfg.LockFile); !os.IsNotExist(statErr) {
				t.Errorf("expected no lockfile from a dry run, stat error = %v", statErr)
			}
		})
	}
}

// dryRunSaveFailureCase is one row of TestDryRunSurfacesItsTailSaveFailure:
// the command entry point to run under --dry-run.
type dryRunSaveFailureCase struct {
	run  func(ctx context.Context, cfg *config.Config, runtime *infra.Infra, state *installState) error
	name string
}

// dryRunSaveFailureCases returns one row per command that saves through
// saveDryRunSnapshotIfPersisted.
func dryRunSaveFailureCases() []dryRunSaveFailureCase {
	return []dryRunSaveFailureCase{
		{
			name: "install",
			run: func(ctx context.Context, cfg *config.Config, runtime *infra.Infra, state *installState) error {
				return installWithState(ctx, cfg, runtime, state, time.Now())
			},
		},
		{
			name: "warm",
			run: func(ctx context.Context, cfg *config.Config, runtime *infra.Infra, state *installState) error {
				return warmWithState(ctx, cfg, runtime, state, time.Now())
			},
		},
		{
			name: "lock",
			run: func(ctx context.Context, cfg *config.Config, runtime *infra.Infra, state *installState) error {
				return lockWithState(ctx, cfg, runtime, state, time.Now())
			},
		},
	}
}
