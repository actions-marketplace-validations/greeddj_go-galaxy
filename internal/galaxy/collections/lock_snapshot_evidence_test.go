package collections

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/cleanup"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/extracted"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/testing/fakegalaxy"
)

// TestLockOnAColdCacheLeavesNoEvidenceForCleanupToActOn is the end-to-end half
// of the defect: the cleanup package's own guard test pins what the guard does
// with each snapshot shape, and this pins that a real `lock` run produces the
// shape that must be left alone.
//
// The scenario is the reachable one. A machine has extracted trees on disk; a
// schema bump drops its snapshot but not those trees; `lock` runs, saving a
// snapshot that records nothing about on-disk content because lock never looks
// at any; then `cleanup` runs. Before, that last step wiped the whole
// content-addressable store on the strength of a snapshot that had never been
// told anything.
//
// The lockfile itself is asserted, not incidental: it is what proves the run
// did its real work rather than failing early into a no-op that would leave
// the tree alone for the wrong reason. The recorded project is load-bearing
// for the same reason: without it cleanup returns before the sweep, and the
// tree survives having never been considered.
//
// KILLING MUTATION, run and reverted: putting sweepExtractedStore's guard back
// on WasPersisted - the predicate that reads a written snapshot as evidence
// regardless of whether anything was ever recorded in it - fails this test
// with the whole tree gone:
//
//	lock_snapshot_evidence_test.go:111: expected the extracted tree to survive a
//	cleanup following a cold-cache lock, stat error: stat
//	.../cache/extracted/sha-survived-the-schema-bump: no such file or directory
func TestLockOnAColdCacheLeavesNoEvidenceForCleanupToActOn(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}
	reqPath := filepath.Join(root, "requirements.yml")
	mustWriteFile(t, reqPath, []byte("collections:\n  - name: acme.widgets\n    version: \"*\"\n"))
	lockPath := filepath.Join(root, "galaxy.lock")

	srv := fakegalaxy.New(t)
	srv.AddVersion("acme", "widgets", "1.0.0", nil)

	// A tree that survived the snapshot drop, named by a sha nothing in the
	// cold snapshot can possibly reference.
	const survivor = "sha-survived-the-schema-bump"
	survivorDir := filepath.Join(cacheDir, extracted.RootDirName, survivor)
	if err := os.MkdirAll(survivorDir, helpers.DirMod); err != nil {
		t.Fatalf("seed extracted tree: %v", err)
	}
	marker := filepath.Join(survivorDir, extracted.ReadyMarker)
	if err := os.WriteFile(marker, []byte(extracted.ReadyMarkerPayload), helpers.FileMod); err != nil {
		t.Fatalf("seed ready marker: %v", err)
	}

	cfg := &config.Config{
		Server:           srv.URL(),
		CacheDir:         cacheDir,
		DownloadPath:     filepath.Join(root, "install"),
		RequirementsFile: reqPath,
		LockFile:         lockPath,
		Workers:          1,
	}
	state := newLocalState(t, cfg)
	runtime := infra.New(noopPrinter{}, srv.Client())

	if err := lockWithState(context.Background(), cfg, runtime, state, time.Now()); err != nil {
		t.Fatalf("lockWithState: %v", err)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("expected lock to have written its lockfile: %v", err)
	}

	// cleanup walks the project registry, so without a recorded project it
	// would return before the extracted-store sweep is even reached and the
	// tree below would survive for the wrong reason. The recorded project
	// points at a DownloadPath that was never created, which is the ordinary
	// warm-only shape: its workspace is skipped, it contributes no installed
	// key, and the sweep therefore runs with an empty keep set - the exact
	// state that decides whether the snapshot's silence is read as evidence.
	if err := state.backend.RecordProject(context.Background(), reqPath, cfg.DownloadPath, ""); err != nil {
		t.Fatalf("RecordProject: %v", err)
	}

	// cleanup opens its own backend against the same cache directory, and Bolt
	// admits one holder at a time, so the lock run's backend has to be closed
	// first - exactly as a real run does when its process exits.
	if err := state.backend.Close(context.Background()); err != nil {
		t.Fatalf("closing the lock run's backend: %v", err)
	}

	if err := cleanup.Start(context.Background(), cfg, infra.New(noopPrinter{}, srv.Client())); err != nil {
		t.Fatalf("cleanup.Start: %v", err)
	}
	if _, err := os.Stat(survivorDir); err != nil {
		t.Fatalf("expected the extracted tree to survive a cleanup following a cold-cache lock, stat error: %v", err)
	}
}
