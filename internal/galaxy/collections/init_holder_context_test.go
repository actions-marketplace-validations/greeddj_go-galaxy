package collections

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
)

// newLoadStoreFailureFixture builds a config whose cache directory holds
// garbage where the local Bolt snapshot belongs, so initInstall reaches its
// LoadStore arm and fails there rather than at any earlier step. The path is
// returned so the positive control can delete it and re-run against the
// identical cache directory.
func newLoadStoreFailureFixture(t *testing.T) (*config.Config, string) {
	t.Helper()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}
	dbPath := filepath.Join(cacheDir, helpers.StoreDBLocal)
	mustWriteFile(t, dbPath, []byte("not a bolt database"))

	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections: []\n"))

	return &config.Config{
		CacheDir:         cacheDir,
		RequirementsFile: reqPath,
		DownloadPath:     filepath.Join(root, "install"),
		Workers:          1,
	}, dbPath
}

// TestInitInstallLoadStoreFailureReturnsHolderContext pins what initInstall
// hands back from its LoadStore arm - the CONTEXT, not only the error.
//
// The error half is already covered elsewhere; the context half is not
// covered by anything, and it is the half the lock-loss verdict depends on.
// initInstall's LoadStore arm runs with the backend's exclusive lock already
// held, which is exactly when a heartbeat can find another acquirer's token
// on the lock object and cancel the holder context underneath this run - so a
// LoadStore failure here can be a symptom of the lock being stolen rather
// than of the snapshot being bad. runInstall judges that by passing this
// returned context to cacheManager.LockLostError, and LockLostError returns
// the run's error untouched when the context it is handed is nil. An arm
// rewritten to hand back a nil context alongside that same error is
// therefore invisible to every error-shaped assertion, and silently reports
// a stolen lock as an ordinary corrupt-snapshot failure.
//
// The local backend's Lock returns the caller's own context unchanged, so
// identity against the ctx passed in is the whole assertion - and the reason
// this can be checked at all without an S3 fixture.
//
// KILLING MUTATION, run and reverted: rewriting that arm to discard the
// holder context - a nil in its place, the error unchanged - leaves every
// error-shaped assertion above it green and fails exactly one line, which is
// the point of asserting the context at all:
//
//	init_holder_context_test.go:90: initInstall returned holder context <nil>, want the ctx it was handed
//
// The positive control deletes the garbage and re-runs against the identical
// cache directory: initInstall must then succeed, which is what proves this
// fixture genuinely reaches LoadStore rather than failing at some earlier
// step for an unrelated reason.
func TestInitInstallLoadStoreFailureReturnsHolderContext(t *testing.T) {
	t.Parallel()
	cfg, dbPath := newLoadStoreFailureFixture(t)
	runtime := infra.New(noopPrinter{}, http.DefaultClient)

	ctx := t.Context()
	lockCtx, state, err := initInstall(ctx, cfg, runtime)
	if err == nil {
		t.Fatalf("expected initInstall to fail against a corrupt snapshot store")
	}
	if !errors.Is(err, helpers.ErrCorruptSnapshotStore) {
		t.Fatalf("initInstall error = %v, want errors.Is helpers.ErrCorruptSnapshotStore", err)
	}
	if state != nil {
		t.Fatalf("initInstall returned a state alongside its failure: %+v", state)
	}
	if lockCtx != ctx {
		t.Fatalf("initInstall returned holder context %v, want the ctx it was handed", lockCtx)
	}

	assertInitInstallSucceedsOnce(t, cfg, runtime, dbPath)
}

// assertInitInstallSucceedsOnce is the positive control: with the garbage
// snapshot removed, the same cache directory must let initInstall through -
// which also proves the failing run released the lock it had taken, since a
// leaked lock would block this acquisition instead. It reads t.Context()
// itself rather than taking one, so *testing.T stays the first parameter
// without tripping revive's context-as-argument rule. Split out of the test
// body purely to stay under the funlen budget.
func assertInitInstallSucceedsOnce(t *testing.T, cfg *config.Config, runtime *infra.Infra, dbPath string) {
	t.Helper()
	ctx := t.Context()
	if err := os.Remove(dbPath); err != nil {
		t.Fatalf("remove the corrupt snapshot store: %v", err)
	}
	lockCtx, state, err := initInstall(ctx, cfg, runtime)
	if err != nil {
		t.Fatalf("initInstall against a clean cache dir: %v", err)
	}
	t.Cleanup(func() {
		if state.release != nil {
			_ = state.release()
		}
		_ = state.backend.Close(context.Background())
	})
	if lockCtx != ctx {
		t.Fatalf("initInstall returned holder context %v on success, want the ctx it was handed", lockCtx)
	}
}
