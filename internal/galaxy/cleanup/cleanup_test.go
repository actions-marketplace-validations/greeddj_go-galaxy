package cleanup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	cacheBackend "github.com/greeddj/go-galaxy/internal/cache"
	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/extracted"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/psvmcc/hub/pkg/types"
)

// noopPrinter is a minimal output.Printer stub for tests that need an Infra
// but do not care about the rendered progress output. output.Printer
// implementations are package-private test doubles rather than a shared
// helper, so this duplicates the identical type already used by
// internal/galaxy/collections/lock_pin_test.go instead of trying to share it
// across packages.
type noopPrinter struct{}

func (noopPrinter) Printf(string, ...any)                 {}
func (noopPrinter) PersistentPrintf(string, ...any)       {}
func (noopPrinter) Okf(string, ...any)                    {}
func (noopPrinter) Errorf(string, ...any)                 {}
func (noopPrinter) Warnf(string, ...any)                  {}
func (noopPrinter) Debugf(string, ...any)                 {}
func (noopPrinter) DebugSincef(time.Time, string, ...any) {}

// recordingPrinter is an output.Printer stub that records Warnf, Printf, and
// Errorf calls so tests can assert that a rejection surfaced a warning, that
// a dry-run reported a transient-tier line, or that a non-fatal failure was
// still surfaced as an error-tier line, rather than any of these being
// silently swallowed. It embeds noopPrinter for the other Printer methods
// and is safe for concurrent use since scanInstalledCollections may be
// called from goroutines in other packages, though cleanup itself scans
// serially.
type recordingPrinter struct {
	noopPrinter

	warnings []string
	prints   []string
	errs     []string
	mu       sync.Mutex
}

func (p *recordingPrinter) Warnf(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.warnings = append(p.warnings, fmt.Sprintf(format, args...))
}

func (p *recordingPrinter) Printf(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prints = append(p.prints, fmt.Sprintf(format, args...))
}

func (p *recordingPrinter) Errorf(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.errs = append(p.errs, fmt.Sprintf(format, args...))
}

// hasWarningContaining reports whether any recorded warning contains substr.
func (p *recordingPrinter) hasWarningContaining(substr string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, w := range p.warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

// hasPrintContaining reports whether any recorded Printf line contains substr.
func (p *recordingPrinter) hasPrintContaining(substr string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, line := range p.prints {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}

// hasErrorContaining reports whether any recorded Errorf line contains substr.
func (p *recordingPrinter) hasErrorContaining(substr string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, line := range p.errs {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}

// testManifestJSON is a minimal, valid MANIFEST.json body for ns.name@1.0.0.
// scanInstalledCollections/buildInstalledRecord only read collection_info's
// namespace, name, and version to build an installedCollection record, so
// the other manifest fields are omitted.
const testManifestJSON = `{
	"collection_info": {
		"namespace": "ns",
		"name": "name",
		"version": "1.0.0"
	}
}`

// TestStartAbortsOnCorruptRegistryBeforeDeletion proves that a project
// registry file which fails to decode aborts cleanup.Start with a wrapped
// helpers.ErrCorruptProjectRegistry, through the real local cache backend,
// lock, and Bolt store - not just at the LoadProjectRegistry unit level. It
// also proves the error this real pipeline returns classifies as
// exitcode.ExitCacheCorrupt: exitcode.FromError is exercised against the
// genuine wrap chain Start builds here, rather than a synthetic sentinel
// constructed directly in a test.
//
// A registry file has to be unparseable JSON to exercise this path at all,
// so it cannot also encode a valid project entry pointing at the seeded
// install tree in the same file: initCleanup calls
// backend.LoadProjectRegistry and returns its error immediately, before
// state.registry is ever read by Start, so buildReachable/removeUnused are
// structurally unreachable here regardless of what the install tree
// contains - there is no code path between a corrupt-registry error and any
// deletion logic. The install tree is still seeded so "it still exists
// after Start" is a real assertion against on-disk state (not a vacuous
// one), and so this test proves the full production pipeline - cache
// backend selection, lock acquisition, store load, project registry load,
// error propagation - reports the failure through Start's return value
// rather than silently continuing to delete anything.
func TestStartAbortsOnCorruptRegistryBeforeDeletion(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()
	installDir := seedInstallTree(t, downloadPath)
	writeCorruptRegistry(t, cacheDir)

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	err := Start(t.Context(), cfg, runtime)
	if !errors.Is(err, helpers.ErrCorruptProjectRegistry) {
		t.Fatalf("expected ErrCorruptProjectRegistry, got %v", err)
	}
	if got := exitcode.FromError(err); got != exitcode.ExitCacheCorrupt {
		t.Fatalf("exitcode.FromError(err) = %d, want ExitCacheCorrupt (%d)", got, exitcode.ExitCacheCorrupt)
	}
	assertManifestSurvives(t, installDir)
}

// TestStartTakesNoOpPathOnEmptyRegistry proves that a missing (equivalently,
// empty) project registry takes cleanup.Start's explicit no-op branch
// (len(state.registry.Projects) == 0) rather than attempting any
// reachability computation or deletion, again through the real local cache
// backend rather than a unit-level check.
//
// As with the corrupt-registry case, zero recorded projects means
// buildReachable/removeUnused never run regardless of what is on disk, so
// this proves the guard fires rather than that a specific collection was
// individually preserved by reachability logic. It additionally asserts that
// the no-op branch releases the instance lock: a fresh backend can acquire
// the same cache dir's lock right after Start returns, which only holds if
// the early return released it rather than leaking it.
func TestStartTakesNoOpPathOnEmptyRegistry(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()
	installDir := seedInstallTree(t, downloadPath)

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected nil error for an empty registry, got %v", err)
	}
	assertManifestSurvives(t, installDir)
	assertLockIsFree(t, cfg, runtime)
}

// assertLockIsFree proves the cache dir's instance lock is acquirable, which
// it is only if a prior Start released it. It opens a fresh backend, acquires
// the lock, and releases it again.
func assertLockIsFree(t *testing.T, cfg *config.Config, runtime *infra.Infra) {
	t.Helper()
	backend, err := cacheBackend.New(cfg, runtime)
	if err != nil {
		t.Fatalf("failed to build a fresh backend: %v", err)
	}
	if err := backend.Open(t.Context()); err != nil {
		t.Fatalf("failed to open a fresh backend: %v", err)
	}
	defer func() {
		if err := backend.Close(t.Context()); err != nil {
			t.Errorf("failed to close the fresh backend: %v", err)
		}
	}()
	release, err := backend.Lock(t.Context())
	if err != nil {
		t.Fatalf("expected the lock to be free after an empty-registry cleanup, got %v", err)
	}
	if err := release(); err != nil {
		t.Errorf("failed to release the re-acquired lock: %v", err)
	}
}

// TestInitCleanupCacheBackendNewFailure pins initCleanup's first lifecycle
// arm: cacheBackend.New itself failing (here, a nil *config.Config) before
// anything is opened, locked, or loaded. Start must simply propagate the
// error; nothing was ever acquired, so there is nothing beyond the non-nil
// error itself to assert. Exit code is deliberately not asserted for this
// arm: it exits 1 (unclassified) like every arm but the lock one below, and
// asserting that here would cement a classification this codebase has not
// settled elsewhere.
func TestInitCleanupCacheBackendNewFailure(t *testing.T) {
	t.Parallel()
	runtime := newTestRuntime()
	if err := Start(t.Context(), nil, runtime); err == nil {
		t.Fatal("expected Start to fail when cfg is nil")
	}
}

// TestInitCleanupOpenFailure pins initCleanup's second lifecycle arm:
// backend.Open failing because the cache dir's parent path is itself a
// regular file, so os.MkdirAll cannot create the cache dir under it. Open
// runs before Lock, so nothing is acquired by the time this fails - this
// pins the branch being taken, not a release - and the blocker file itself
// must survive untouched, since nothing on this path ever removes or
// replaces it.
func TestInitCleanupOpenFailure(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	blocker := filepath.Join(tmp, "afile")
	if err := os.WriteFile(blocker, []byte("x"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write blocker file: %v", err)
	}

	cfg := &config.Config{CacheDir: filepath.Join(blocker, "sub"), DryRun: false}
	runtime := newTestRuntime()

	if err := Start(t.Context(), cfg, runtime); err == nil {
		t.Fatal("expected Start to fail when the cache dir's parent is a regular file")
	}
	info, statErr := os.Stat(blocker)
	if statErr != nil {
		t.Fatalf("expected the blocker file to survive, stat error: %v", statErr)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("expected the blocker to remain a regular file, got mode %v", info.Mode())
	}
}

// TestInitCleanupLockFailure pins initCleanup's third lifecycle arm: the
// instance lock already held by another acquirer on the same cache dir.
// flock(2) conflicts between file descriptions, not processes, so acquiring
// it a second time from this same test process already reproduces
// EWOULDBLOCK - no subprocess, no goroutine, no timing dependency needed.
// This is also the one arm that carries a classified exit code:
// helpers.ErrAnotherInstanceIsRunning classifies as exitcode.ExitCacheBusy,
// asserted here alongside errors.Is.
//
// The positive control releases the held lock, closes the holding backend,
// and re-runs Start on the identical cache dir: it must succeed, proving the
// failure above was the lock specifically, not some other property of the
// cache dir.
func TestInitCleanupLockFailure(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	holder, err := cacheBackend.New(cfg, runtime)
	if err != nil {
		t.Fatalf("failed to build the lock-holding backend: %v", err)
	}
	if err := holder.Open(t.Context()); err != nil {
		t.Fatalf("failed to open the lock-holding backend: %v", err)
	}
	release, err := holder.Lock(t.Context())
	if err != nil {
		t.Fatalf("failed to acquire the holding lock: %v", err)
	}

	startErr := Start(t.Context(), cfg, runtime)
	if !errors.Is(startErr, helpers.ErrAnotherInstanceIsRunning) {
		t.Fatalf("expected ErrAnotherInstanceIsRunning, got %v", startErr)
	}
	if got := exitcode.FromError(startErr); got != exitcode.ExitCacheBusy {
		t.Fatalf("exitcode.FromError(startErr) = %d, want ExitCacheBusy (%d)", got, exitcode.ExitCacheBusy)
	}

	if err := release(); err != nil {
		t.Fatalf("failed to release the holding lock: %v", err)
	}
	if err := holder.Close(t.Context()); err != nil {
		t.Fatalf("failed to close the lock-holding backend: %v", err)
	}

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed once the lock is released, got %v", err)
	}
}

// TestInitCleanupLoadStoreFailure pins initCleanup's fourth lifecycle arm:
// backend.LoadStore itself failing because the local Bolt file at
// helpers.StoreDBLocal holds garbage instead of a valid database. Unlike the
// other three lifecycle arms in this file, this one asserts a classified
// exit code: openBolt's corruption arm must surface through Start as
// helpers.ErrCorruptSnapshotStore and classify as exitcode.ExitCacheCorrupt,
// since garbage bytes at this path satisfy that class's own predicate (the
// persisted cache state cannot be interpreted by anyone) exactly as a
// corrupt project registry already does. It also keeps the one other
// genuinely observable discipline property this arm has: the lock acquired
// just before LoadStore must still be released on this failure path, which
// assertLockIsFree verifies by re-acquiring it through a fresh backend.
//
// The mandatory positive control deletes the corrupt file and re-runs Start
// against the identical cache dir: it must succeed, proving this fixture
// genuinely reaches LoadStore - rather than failing some earlier step for an
// unrelated reason - and that the abort above is specific to the damaged
// bytes, not to the cache dir itself.
func TestInitCleanupLoadStoreFailure(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("failed to create cache dir: %v", err)
	}
	dbPath := filepath.Join(cacheDir, helpers.StoreDBLocal)
	if err := os.WriteFile(dbPath, []byte("not a bolt database"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write a garbage store db: %v", err)
	}

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	err := Start(t.Context(), cfg, runtime)
	if err == nil {
		t.Fatal("expected Start to fail when the local store db is corrupt")
	}
	if !errors.Is(err, helpers.ErrCorruptSnapshotStore) {
		t.Fatalf("expected ErrCorruptSnapshotStore, got %v", err)
	}
	if got := exitcode.FromError(err); got != exitcode.ExitCacheCorrupt {
		t.Fatalf("exitcode.FromError(err) = %d, want ExitCacheCorrupt (%d)", got, exitcode.ExitCacheCorrupt)
	}
	assertLockIsFree(t, cfg, runtime)

	if err := os.Remove(dbPath); err != nil {
		t.Fatalf("failed to remove the corrupt store db: %v", err)
	}
	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed once the corrupt store db is removed, got %v", err)
	}
}

// newTestRuntime builds an Infra wired with a no-op printer and the default
// HTTP client, matching the pattern used by the collections package's own
// tests for constructing an Infra without caring about rendered output.
func newTestRuntime() *infra.Infra {
	return newTestRuntimeWith(nil)
}

// newTestRuntimeWith builds an Infra wired with printer and the default HTTP
// client, for a test that needs to assert on recorded output (a warning, a
// dry-run report line, or a non-fatal error) rather than only on Start's
// return value or on-disk state. A nil printer falls back to noopPrinter{},
// so newTestRuntime can share this single constructor instead of duplicating
// infra.New's wiring.
func newTestRuntimeWith(printer *recordingPrinter) *infra.Infra {
	if printer == nil {
		return infra.New(noopPrinter{}, http.DefaultClient)
	}
	return infra.New(printer, http.DefaultClient)
}

// openTestWorkspace opens collectionsPath's os.Root the same way
// openProjectWorkspace does once it finds a usable candidate, for tests that
// call the rooted scan functions (scanInstalledCollections, scanNamespaceDir)
// directly rather than only indirectly through Start. The caller owns the
// returned workspace's root and must close it.
func openTestWorkspace(t *testing.T, collectionsPath string) workspace {
	t.Helper()
	root, err := os.OpenRoot(collectionsPath)
	if err != nil {
		t.Fatalf("failed to open root at %s: %v", collectionsPath, err)
	}
	return workspace{root: root, fsys: root.FS(), path: collectionsPath}
}

// seedInstallTree creates a MANIFEST.json for ns.name@1.0.0 under
// <downloadPath>/ansible_collections/ns/name, mirroring the on-disk layout
// scanInstalledCollections walks and removeInstalled deletes. It returns
// the manifest's directory (the install path a real deletion would remove).
func seedInstallTree(t *testing.T, downloadPath string) string {
	t.Helper()
	installDir := filepath.Join(downloadPath, "ansible_collections", "ns", "name")
	if err := os.MkdirAll(installDir, helpers.DirMod); err != nil {
		t.Fatalf("failed to create install dir: %v", err)
	}
	manifestPath := filepath.Join(installDir, "MANIFEST.json")
	if err := os.WriteFile(manifestPath, []byte(testManifestJSON), helpers.FileMod); err != nil {
		t.Fatalf("failed to write MANIFEST.json: %v", err)
	}
	return installDir
}

// writeCorruptRegistry writes unparseable bytes directly at the project
// registry path, bypassing store.RecordProject (which only ever round-trips
// valid JSON) so the file is corrupt from the very first read.
func writeCorruptRegistry(t *testing.T, cacheDir string) {
	t.Helper()
	path := filepath.Join(cacheDir, helpers.StoreDBProjects)
	if err := os.WriteFile(path, []byte("{invalid"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write corrupt registry: %v", err)
	}
}

// assertManifestSurvives fails the test unless installDir's MANIFEST.json
// is still present, proving no deletion touched it.
func assertManifestSurvives(t *testing.T, installDir string) {
	t.Helper()
	manifestPath := filepath.Join(installDir, "MANIFEST.json")
	if _, err := os.Stat(manifestPath); err != nil {
		t.Fatalf("expected the pre-seeded install tree to survive, stat error: %v", err)
	}
}

// writeProjectRegistry writes a valid project registry JSON directly at
// cacheDir, bypassing store.RecordProject so the test controls the exact
// CollectionsPath and RequirementsFile without needing a real project
// directory layout.
func writeProjectRegistry(t *testing.T, cacheDir string, registry *store.ProjectRegistry) {
	t.Helper()
	data, err := json.Marshal(registry)
	if err != nil {
		t.Fatalf("failed to marshal project registry: %v", err)
	}
	path := filepath.Join(cacheDir, helpers.StoreDBProjects)
	if err := os.WriteFile(path, data, helpers.FileMod); err != nil {
		t.Fatalf("failed to write project registry: %v", err)
	}
}

// writeOutsideSentinel creates a file at outsideRoot/name/keep.txt,
// simulating a target that a successful path-traversal exploit could reach
// if it escaped the collections tree, and returns the file's path.
func writeOutsideSentinel(t *testing.T, outsideRoot, name string) string {
	t.Helper()
	sentinelDir := filepath.Join(outsideRoot, name)
	if err := os.MkdirAll(sentinelDir, helpers.DirMod); err != nil {
		t.Fatalf("failed to create outside sentinel dir: %v", err)
	}
	sentinelFile := filepath.Join(sentinelDir, "keep.txt")
	if err := os.WriteFile(sentinelFile, []byte("keep me"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write outside sentinel file: %v", err)
	}
	return sentinelFile
}

// writeMaliciousManifestTree seeds a MANIFEST.json under downloadPath whose
// version is a path-traversal payload, and returns the manifest's path.
func writeMaliciousManifestTree(t *testing.T, downloadPath string) string {
	t.Helper()
	installDir := filepath.Join(downloadPath, "ansible_collections", "ns", "name")
	if err := os.MkdirAll(installDir, helpers.DirMod); err != nil {
		t.Fatalf("failed to create install dir: %v", err)
	}
	maliciousManifest := `{
		"collection_info": {
			"namespace": "ns",
			"name": "name",
			"version": "../../../../pwn"
		}
	}`
	manifestPath := filepath.Join(installDir, "MANIFEST.json")
	if err := os.WriteFile(manifestPath, []byte(maliciousManifest), helpers.FileMod); err != nil {
		t.Fatalf("failed to write malicious MANIFEST.json: %v", err)
	}
	return manifestPath
}

// registerCleanupProject writes a project registry entry pointing at
// downloadPath with an empty requirements file (nothing reachable), so that
// any indexed collection would be a removal candidate.
func registerCleanupProject(t *testing.T, cacheDir, downloadPath string) {
	t.Helper()
	reqPath := filepath.Join(t.TempDir(), "requirements.yml")
	if err := os.WriteFile(reqPath, []byte("collections: []\n"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write requirements file: %v", err)
	}
	registerCleanupProjectAt(t, cacheDir, downloadPath, reqPath)
}

// registerCleanupProjectAt writes a project registry entry pointing at
// downloadPath whose requirements file is reqPath. Unlike
// registerCleanupProject, the caller fully controls reqPath's existence and
// content - including deliberately leaving it nonexistent or writing
// unparseable content - so this is used to test requirements-load failures.
func registerCleanupProjectAt(t *testing.T, cacheDir, downloadPath, reqPath string) {
	t.Helper()
	writeProjectRegistry(t, cacheDir, &store.ProjectRegistry{
		Projects: map[string]store.ProjectRecord{
			"proj": {
				RequirementsFile: reqPath,
				CollectionsPath:  downloadPath,
				LastRun:          time.Now().UTC(),
			},
		},
	})
}

// TestRemoveInstalledRejectsTraversalVersion proves the path-traversal fix
// end to end through Start: a MANIFEST.json whose version is a traversal
// payload must never reach removeInstalled at all, because
// buildInstalledRecord rejects it at ingestion. The collection's own
// on-disk directory and an unrelated sentinel file outside the whole
// collections tree must both survive, and a warning must be recorded
// instead of the run aborting or silently deleting anything.
func TestRemoveInstalledRejectsTraversalVersion(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()

	sentinelFile := writeOutsideSentinel(t, t.TempDir(), "pwn")
	manifestPath := writeMaliciousManifestTree(t, downloadPath)
	registerCleanupProject(t, cacheDir, downloadPath)

	printer := &recordingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)
	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}

	if _, err := os.Stat(sentinelFile); err != nil {
		t.Fatalf("expected outside sentinel to survive, stat error: %v", err)
	}
	if _, err := os.Stat(manifestPath); err != nil {
		t.Fatalf("expected the collection's own install dir to survive since ingestion rejected it, stat error: %v", err)
	}
	if !printer.hasWarningContaining("unsafe identifier") {
		t.Fatalf("expected a warning about an unsafe identifier, got: %v", printer.warnings)
	}
}

// TestRemoveUnusedCannotForgeAReportLine proves, end to end through Start,
// that a MANIFEST.json version carrying a newline can never inject an extra
// plain-text line into cleanup's own report output. The survival check
// below is what actually closes the defect: buildInstalledRecord's
// helpers.IsPathElement check on version rejects the newline at ingestion
// (see IsPathElement's own doc comment for the structural fix this pins),
// so a rejected identifier is never indexed and nothing about the hostile
// collection - including a report line naming it - is ever printed at all.
// The newline scan is checked first, ahead of that survival assertion and
// the warning assertion after it, so that a reachable regression fails on
// the injected line's own content rather than one step later on a
// same-cause symptom (a missing file) that does not by itself say why.
//
// Its mandatory positive control is a second, ordinary collection seeded
// under the identical project and fixture: with an empty requirements.yml,
// both collections are unreferenced, so the ordinary one is a genuine
// removal candidate and must actually be removed, with its own "removed"
// line appearing in the report exactly as removeUnused always renders it
// (helpers.IsPathElement-validated components, printed with a bare %s - see
// removeUnused's own comment on that line). Without this control, the
// hostile collection surviving would be indistinguishable from "cleanup
// never reached the scan at all" rather than "the scan correctly refused
// it".
//
// Confirmed killing mutation (disabling IsPathElement's control-character
// rejection, restoring the pre-fix behavior that only checked for path
// separators and "."/".."): running `go test ./internal/galaxy/cleanup/...
// -run TestRemoveUnusedCannotForgeAReportLine -v` against that mutation
// produced:
//
//	cleanup_test.go:635: recorded output line contains a raw newline,
//	forged-line defect is not closed: "🧹 removed ns.hostile@1.0.0
//	forged plain-text line"
//	--- FAIL: TestRemoveUnusedCannotForgeAReportLine (0.01s)
//
// under the mutation, the forged version passes buildInstalledRecord's
// IsPathElement check same as before this fix, so the hostile collection is
// indexed and genuinely removed by removeUnused, whose bare-%s "removed"
// line carries the forged newline and trailing text straight through -
// exactly the injected line this test exists to catch. The scan is what
// fails first under this mutation; the survival and warning checks below it
// never run at all, so only the quoted line above is a mutation-pinned
// claim - the positive control's own eventual behavior under this same
// mutation is not what this mutation is cited for.
func TestRemoveUnusedCannotForgeAReportLine(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()

	const forgedVersion = "1.0.0\nforged plain-text line"
	seedManifestAt(t, downloadPath, "ns", "hostile", forgedVersion)
	seedManifestAt(t, downloadPath, "ns", "ordinary", "1.0.0")
	registerCleanupProject(t, cacheDir, downloadPath)

	printer := &recordingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)
	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}

	// Checked first, ahead of the survival and warning assertions below: no
	// recorded warning or print line may contain a raw, unescaped newline,
	// since that is exactly what would let a hostile value fold a second,
	// attacker-chosen plain-text line into this program's own output.
	for _, line := range append(append([]string{}, printer.warnings...), printer.prints...) {
		if strings.Contains(line, "\n") {
			t.Fatalf("recorded output line contains a raw newline, forged-line defect is not closed: %q", line)
		}
	}

	// This is what actually closes the defect: the hostile collection was
	// never indexed, so its own on-disk tree was never a removal candidate
	// and survives untouched.
	hostileManifest := filepath.Join(downloadPath, "ansible_collections", "ns", "hostile", "MANIFEST.json")
	if _, err := os.Stat(hostileManifest); err != nil {
		t.Fatalf("expected the hostile collection's install dir to survive since ingestion rejected it, stat error: %v", err)
	}
	if !printer.hasWarningContaining("unsafe identifier") {
		t.Fatalf("expected a warning about an unsafe identifier, got: %v", printer.warnings)
	}

	// Positive control: the ordinary, unreferenced sibling collection is a
	// genuine removal candidate and must actually be removed, with its
	// report line rendered normally.
	ordinaryManifest := filepath.Join(downloadPath, "ansible_collections", "ns", "ordinary", "MANIFEST.json")
	if _, err := os.Stat(ordinaryManifest); !os.IsNotExist(err) {
		t.Fatalf("expected the ordinary, unreferenced collection to be removed, stat error: %v", err)
	}
	if !printer.hasPrintContaining("removed ns.ordinary@1.0.0") {
		t.Fatalf("expected a removal report line for ns.ordinary@1.0.0, got prints: %v", printer.prints)
	}
}

// TestRemoveInstalledContainmentGuard exercises removeInstalled directly
// with a crafted installedCollection whose Version is a traversal payload
// containing "/". This trips the IsPathElement check on Version - the
// first guard in removeInstalled - not the WithinDir containment check
// further down: a Version containing "/" can never reach WithinDir at all,
// since removeInstalled rejects it before computing any path to check for
// containment. See TestRemoveInstalledRejectsInstallPathEscape below for a
// test that actually exercises the WithinDir install-path guard, with
// ns/name/Version all valid path elements but InstallPath itself pointing
// outside CollectionsDir. Both guards return ErrUnsafeRemovalPath and never
// call os.RemoveAll on anything, so an unrelated outside sentinel survives.
func TestRemoveInstalledContainmentGuard(t *testing.T) {
	t.Parallel()
	collectionsDir := t.TempDir()
	sentinelFile := writeOutsideSentinel(t, t.TempDir(), "evil")

	inst := installedCollection{
		Key:            "ns.name@../../evil",
		FQDN:           "ns.name",
		Namespace:      "ns",
		Name:           "name",
		Version:        "../../evil",
		InstallPath:    filepath.Join(collectionsDir, "ansible_collections", "ns", "name"),
		CollectionsDir: collectionsDir,
	}

	err := removeInstalled(t.Context(), inst, nil, "")
	if !errors.Is(err, helpers.ErrUnsafeRemovalPath) {
		t.Fatalf("expected ErrUnsafeRemovalPath, got %v", err)
	}
	if _, statErr := os.Stat(sentinelFile); statErr != nil {
		t.Fatalf("expected outside sentinel to survive, stat error: %v", statErr)
	}
}

// TestRemoveInstalledRejectsInstallPathEscape proves the WithinDir
// install-path containment guard fires independently: ns, name, and
// Version are all valid, safe path elements (they pass IsPathElement), but
// InstallPath itself points entirely outside CollectionsDir - as it might
// for a stale or corrupted in-memory record rather than one built by the
// normal scan. removeInstalled must still refuse to call os.RemoveAll on
// it, so the unrelated outside sentinel survives.
func TestRemoveInstalledRejectsInstallPathEscape(t *testing.T) {
	t.Parallel()
	collectionsDir := t.TempDir()
	sentinelFile := writeOutsideSentinel(t, t.TempDir(), "victim")

	inst := installedCollection{
		Key:            "ns.name@1.0.0",
		FQDN:           "ns.name",
		Namespace:      "ns",
		Name:           "name",
		Version:        "1.0.0",
		InstallPath:    filepath.Dir(sentinelFile),
		CollectionsDir: collectionsDir,
	}

	err := removeInstalled(t.Context(), inst, nil, "")
	if !errors.Is(err, helpers.ErrUnsafeRemovalPath) {
		t.Fatalf("expected ErrUnsafeRemovalPath, got %v", err)
	}
	if _, statErr := os.Stat(sentinelFile); statErr != nil {
		t.Fatalf("expected outside sentinel to survive, stat error: %v", statErr)
	}
}

// TestBuildInstalledRecordRejectsSeparators table-tests buildInstalledRecord
// across namespace/name/version combinations: unsafe path elements must be
// rejected with helpers.ErrUnsafeCollectionIdentifier, incomplete manifests
// remain a benign skip (ok=false, err=nil), and valid identifiers - including
// a semver value with both a prerelease and a build metadata segment - must
// succeed. ns and name are buildInstalledRecord's own arguments (the shape
// scanCollectionDir feeds it, taken from the walked
// ansible_collections/<ns>/<name> directory pair), so this table is what
// actually exercises the namespace/name IsPathElement arm: the real scan
// can never hand buildInstalledRecord a namespace or name containing "/" or
// "..", since fs.ReadDir never yields
// such an entry, but a direct call like this one still can.
func TestBuildInstalledRecordRejectsSeparators(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name              string
		ns, coll, version string
		wantOK            bool
		wantErr           bool
	}{
		{"valid identifiers", "ns", "name", "1.0.0", true, false},
		{"valid semver with prerelease and build", "ns", "name", "1.0.0-rc.1+build", true, false},
		{"namespace is traversal", "..", "name", "1.0.0", false, true},
		{"name contains slash", "ns", "a/b", "1.0.0", false, true},
		{"version is traversal", "ns", "name", "../../../../pwn", false, true},
		{"version is absolute path", "ns", "name", "/etc/passwd", false, true},
		// A version carrying a newline is what a hostile manifest would use
		// to try to forge an extra plain-text line into a report line this
		// package later prints verbatim (e.g. removeUnused's "removed %s" -
		// see IsPathElement's own doc comment for the structural fix this
		// pins). Rejected here, at ingestion, before any such value can ever
		// reach a printed key.
		{"version carries a newline", "ns", "name", "1.0.0\nforged plain-text line", false, true},
		{"namespace is empty", "", "name", "1.0.0", false, false},
		{"name is empty", "ns", "", "1.0.0", false, false},
		{"version is empty", "ns", "name", "", false, false},
		{"version is dot", "ns", "name", ".", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var manifest types.GalaxyCollectionVersionInfoManifest
			manifest.CollectionInfo.Version = tc.version

			record, key, ok, err := buildInstalledRecord(
				"/collections", "/collections/ansible_collections/x/MANIFEST.json", tc.ns, tc.coll, manifest,
			)

			if tc.wantErr {
				if !errors.Is(err, helpers.ErrUnsafeCollectionIdentifier) {
					t.Fatalf("expected ErrUnsafeCollectionIdentifier, got %v (ok=%v)", err, ok)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
			if ok != tc.wantOK {
				t.Fatalf("expected ok=%v, got %v (record=%+v key=%q)", tc.wantOK, ok, record, key)
			}
		})
	}
}

// TestStartFailsOnUnreadableRequirementsCorrupt proves that a recorded
// project whose workspace is present on disk (its collections tree is
// seeded and found by openProjectWorkspace), but whose requirements file
// contains unparseable content, aborts the whole run with
// helpers.ErrProjectRequirementsUnreadable instead of silently contributing
// zero reachability roots and letting the seeded install become an
// unreachable deletion candidate.
func TestStartFailsOnUnreadableRequirementsCorrupt(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()
	installDir := seedInstallTree(t, downloadPath)

	reqPath := filepath.Join(t.TempDir(), "requirements.yml")
	if err := os.WriteFile(reqPath, []byte("{invalid"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write corrupt requirements file: %v", err)
	}
	registerCleanupProjectAt(t, cacheDir, downloadPath, reqPath)

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	err := Start(t.Context(), cfg, runtime)
	if !errors.Is(err, helpers.ErrProjectRequirementsUnreadable) {
		t.Fatalf("expected ErrProjectRequirementsUnreadable, got %v", err)
	}
	assertManifestSurvives(t, installDir)
}

// TestStartToleratesMissingRequirementsAsStaleEntry proves a recorded
// project's requirements file that no longer exists is tolerated as a stale
// registry entry, not treated the same way as
// TestStartFailsOnUnreadableRequirementsCorrupt's unparseable one: a missing
// file is the one state projectRequirementRoots can actually know the
// answer for - "this project declares nothing" - as opposed to a
// present-but-unreadable-or-unparseable file, where the roots it would have
// contributed are genuinely unknown and could have been protecting any
// project's on-disk copies, which is what still aborts the whole run.
func TestStartToleratesMissingRequirementsAsStaleEntry(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()
	installDir := seedInstallTree(t, downloadPath)

	reqPath := filepath.Join(t.TempDir(), "does-not-exist.yml")
	registerCleanupProjectAt(t, cacheDir, downloadPath, reqPath)

	printer := &recordingPrinter{}
	runtime := newTestRuntimeWith(printer)
	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed with a stale (missing) requirements file, got %v", err)
	}
	if !printer.hasWarningContaining("no longer exists") {
		t.Fatalf("expected a warning about the missing requirements file, got: %v", printer.warnings)
	}
	manifestPath := filepath.Join(installDir, "MANIFEST.json")
	if _, statErr := os.Stat(manifestPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected the unreferenced install to still be removed despite the stale entry, stat error: %v", statErr)
	}
}

// TestStartDeletesUnreferencedWithValidRequirements is the control for
// TestStartFailsOnUnreadableRequirementsCorrupt above: a project with a
// valid requirements file that references nothing must still let cleanup
// proceed normally and delete the unreferenced installed collection,
// proving the abort triggers only on an actual read-or-parse failure, not
// on every non-matching, empty, or missing requirements file.
func TestStartDeletesUnreferencedWithValidRequirements(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()
	installDir := seedInstallTree(t, downloadPath)
	registerCleanupProject(t, cacheDir, downloadPath)

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed with a valid requirements file, got %v", err)
	}
	manifestPath := filepath.Join(installDir, "MANIFEST.json")
	if _, statErr := os.Stat(manifestPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected the unreferenced collection to be deleted, stat error: %v", statErr)
	}
}

// buildNonRegularRequirementsFixture builds the two-project fixture
// TestStartAbortsOnFifoRequirementsFile and its positive control share: a
// "gated-project" whose requirements file is shaped by buildReqFile (a
// named pipe for the refusal case, a regular file for the control), and a
// healthy "other-project" holding one unreferenced install of its own, so a
// survival/removal assertion on it always has something real to check
// regardless of what happens to the gated project's requirements file.
// buildReqFile may call t.Skipf itself (mirroring buildManifestAsNamedPipe's
// own contract) when the shape it needs is unavailable on this platform.
func buildNonRegularRequirementsFixture(
	t *testing.T,
	cacheDir string,
	buildReqFile func(t *testing.T, reqPath string),
) (string, string) {
	t.Helper()
	gatedDownloadPath := t.TempDir()
	gatedInstallDir := seedInstallTree(t, gatedDownloadPath)
	gatedReqPath := filepath.Join(t.TempDir(), "requirements.yml")
	buildReqFile(t, gatedReqPath)

	otherDownloadPath := t.TempDir()
	otherInstallDir := seedInstallTree(t, otherDownloadPath)
	otherReqPath := filepath.Join(t.TempDir(), "requirements-other.yml")
	if err := os.WriteFile(otherReqPath, []byte("collections: []\n"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write other project's requirements file: %v", err)
	}

	writeProjectRegistry(t, cacheDir, &store.ProjectRegistry{
		Projects: map[string]store.ProjectRecord{
			"gated-project": {RequirementsFile: gatedReqPath, CollectionsPath: gatedDownloadPath, LastRun: time.Now().UTC()},
			"other-project": {RequirementsFile: otherReqPath, CollectionsPath: otherDownloadPath, LastRun: time.Now().UTC()},
		},
	})
	return gatedInstallDir, otherInstallDir
}

// TestStartAbortsOnFifoRequirementsFile proves loadRequirements's Stat gate
// end to end through Start: a recorded project's requirements file that is a
// named pipe rather than a real file must abort the whole run with
// helpers.ErrProjectRequirementsUnreadable rather than blocking forever in
// requirements.LoadCollections's own os.ReadFile open() call - see
// loadRequirements's own doc comment (requirements.go) for the full hazard,
// including the S3 backend's lock-heartbeat consequence. The bound
// (startBoundedErr) exists for the identical reason
// nonRegularManifestStartBound does: if a regression ever removes the gate,
// this test must fail fast rather than hang. A second, healthy project's own
// unreferenced install must survive the abort untouched.
func TestStartAbortsOnFifoRequirementsFile(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	_, otherInstallDir := buildNonRegularRequirementsFixture(t, cacheDir, func(t *testing.T, reqPath string) {
		t.Helper()
		if err := syscall.Mkfifo(reqPath, 0o644); err != nil {
			t.Skipf("named pipes unavailable on this platform: %v", err)
		}
	})

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	err := startBoundedErr(t, cfg, runtime)
	if !errors.Is(err, helpers.ErrProjectRequirementsUnreadable) {
		t.Fatalf("expected ErrProjectRequirementsUnreadable, got %v", err)
	}

	otherManifest := filepath.Join(otherInstallDir, "MANIFEST.json")
	if _, statErr := os.Stat(otherManifest); statErr != nil {
		t.Fatalf("expected the other project's install to survive the abort untouched, stat error: %v", statErr)
	}
}

// TestStartAbortsOnFifoRequirementsFilePositiveControl is the mandatory
// positive control for TestStartAbortsOnFifoRequirementsFile on the
// identical fixture shape: swapping the named pipe for a regular, valid
// requirements file lets Start succeed and removes both projects'
// unreferenced installs, proving the abort above is specific to the
// non-regular shape, not to some other property of this fixture.
func TestStartAbortsOnFifoRequirementsFilePositiveControl(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	gatedInstallDir, otherInstallDir := buildNonRegularRequirementsFixture(t, cacheDir, func(t *testing.T, reqPath string) {
		t.Helper()
		if err := os.WriteFile(reqPath, []byte("collections: []\n"), helpers.FileMod); err != nil {
			t.Fatalf("failed to write requirements file: %v", err)
		}
	})

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	if err := startBoundedErr(t, cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}

	gatedManifest := filepath.Join(gatedInstallDir, "MANIFEST.json")
	if _, statErr := os.Stat(gatedManifest); !os.IsNotExist(statErr) {
		t.Fatalf("expected the formerly-gated project's unreferenced install to be removed, stat error: %v", statErr)
	}
	otherManifest := filepath.Join(otherInstallDir, "MANIFEST.json")
	if _, statErr := os.Stat(otherManifest); !os.IsNotExist(statErr) {
		t.Fatalf("expected the other project's unreferenced install to be removed, stat error: %v", statErr)
	}
}

// TestStartRemovesLegacyAndScopedArtifactKeys is the regression guard for
// the one-time artifact-cache migration from the pre-multi-server flat key
// shape to the current server-scoped one: a cached tarball can exist under
// the pre-multi-server flat key (legacyArtifactKey) and/or the current,
// server-scoped key
// (helpers.ArtifactKey), and a single Start run against an unreferenced
// collection must remove both - the scoped key via removeUnused's own purge
// (driven by the persisted InstalledEntry.Source), the legacy key via
// sweepLegacyArtifacts, which runs unconditionally rather than only for
// collections being removed.
func TestStartRemovesLegacyAndScopedArtifactKeys(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()
	seedInstallTree(t, downloadPath)
	registerCleanupProject(t, cacheDir, downloadPath)

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	const source = "https://galaxy.example.com/api"
	seedSnapshotInstalled(t, cfg, runtime, map[string]store.InstalledEntry{
		"ns.name@1.0.0": {Source: source, ArtifactSHA256: "deadbeef"},
	})

	const filename = "ns-name-1.0.0.tar.gz"
	legacyPath := filepath.Join(cacheDir, legacyArtifactKey("ns", "name", "1.0.0"))
	scopedPath := filepath.Join(cacheDir, helpers.ArtifactKey(source, filename))
	if err := os.WriteFile(legacyPath, []byte("legacy-bytes"), helpers.FileMod); err != nil {
		t.Fatalf("failed to seed legacy artifact: %v", err)
	}
	if err := os.WriteFile(scopedPath, []byte("scoped-bytes"), helpers.FileMod); err != nil {
		t.Fatalf("failed to seed scoped artifact: %v", err)
	}

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}

	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("expected the legacy-keyed artifact to be removed, stat error: %v", err)
	}
	if _, err := os.Stat(scopedPath); !os.IsNotExist(err) {
		t.Fatalf("expected the scoped artifact to be removed, stat error: %v", err)
	}
}

// TestStartSweepsLegacyArtifactForReachableCollection proves
// sweepLegacyArtifacts runs independent of reachability: a collection that
// remains referenced by a project's requirements keeps its workspace and its
// current, server-scoped artifact cache entry, but its legacy-keyed tarball -
// which nothing will ever look up again regardless of reachability - is
// still reclaimed.
func TestStartSweepsLegacyArtifactForReachableCollection(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()
	seedInstallTree(t, downloadPath)

	reqPath := filepath.Join(t.TempDir(), "requirements.yml")
	if err := os.WriteFile(reqPath, []byte("collections:\n  - name: ns.name\n"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write requirements file: %v", err)
	}
	registerCleanupProjectAt(t, cacheDir, downloadPath, reqPath)

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	const source = "https://galaxy.example.com/api"
	seedSnapshotInstalled(t, cfg, runtime, map[string]store.InstalledEntry{
		"ns.name@1.0.0": {Source: source, ArtifactSHA256: "deadbeef"},
	})

	const filename = "ns-name-1.0.0.tar.gz"
	legacyPath := filepath.Join(cacheDir, legacyArtifactKey("ns", "name", "1.0.0"))
	scopedPath := filepath.Join(cacheDir, helpers.ArtifactKey(source, filename))
	if err := os.WriteFile(legacyPath, []byte("legacy-bytes"), helpers.FileMod); err != nil {
		t.Fatalf("failed to seed legacy artifact: %v", err)
	}
	if err := os.WriteFile(scopedPath, []byte("scoped-bytes"), helpers.FileMod); err != nil {
		t.Fatalf("failed to seed scoped artifact: %v", err)
	}

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}

	manifestPath := filepath.Join(downloadPath, "ansible_collections", "ns", "name", "MANIFEST.json")
	if _, err := os.Stat(manifestPath); err != nil {
		t.Fatalf("expected the still-referenced collection's workspace to survive, stat error: %v", err)
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("expected the legacy-keyed artifact to be swept even though the collection is reachable, stat error: %v", err)
	}
	if _, err := os.Stat(scopedPath); err != nil {
		t.Fatalf("expected the scoped artifact of a reachable collection to survive, stat error: %v", err)
	}
}

// TestSweepLegacyArtifactsSkipsForgedScopedCollision proves
// sweepLegacyArtifacts's guard against the artifact-key collision a walked
// namespace directory can forge: legacyArtifactKey's url.QueryEscape-based
// shape leaves "." and "-" unescaped, so a hostile namespace directory named
// "<fp>.acme" - fp being the leading helpers.ArtifactKeyFingerprintLen hex
// characters of a real, current helpers.ArtifactKey scoped to some server -
// produces a legacy key byte-identical to that genuine, scoped cache entry.
// Without the guard, sweepLegacyArtifacts would delete it unconditionally:
// this pass has no reachability check and no Source requirement, purely
// because a scanned tree happened to contain a "." in a namespace component.
//
// A second, ordinary project with a genuinely legacy-keyed artifact is the
// mandatory positive control, exercised in the same Start run: it proves the
// guard skips only the forged collision rather than disabling the sweep
// outright.
func TestSweepLegacyArtifactsSkipsForgedScopedCollision(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()

	const source = "https://galaxy.ansible.com/api"
	const filename = "acme-app-1.0.0.tar.gz"
	scopedKey := helpers.ArtifactKey(source, filename)
	fp := scopedKey[:helpers.ArtifactKeyFingerprintLen]
	hostileNS := fp + ".acme"

	hostileDownloadPath := t.TempDir()
	seedManifestAt(t, hostileDownloadPath, hostileNS, "app", "1.0.0")
	hostileReqPath := filepath.Join(t.TempDir(), "requirements.yml")
	if err := os.WriteFile(hostileReqPath, []byte("collections: []\n"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write hostile project's requirements file: %v", err)
	}

	// forgedLegacyKey is what legacyArtifactKey builds for the hostile
	// namespace/name/version. If this fails, the fixture itself no longer
	// demonstrates the collision this test exists to guard against.
	forgedLegacyKey := legacyArtifactKey(hostileNS, "app", "1.0.0")
	if forgedLegacyKey != scopedKey {
		t.Fatalf("fixture assumption broken: forged legacy key %q does not equal scoped key %q", forgedLegacyKey, scopedKey)
	}
	scopedArtifactPath := filepath.Join(cacheDir, scopedKey)
	if err := os.WriteFile(scopedArtifactPath, []byte("scoped-bytes"), helpers.FileMod); err != nil {
		t.Fatalf("failed to seed the scoped artifact: %v", err)
	}

	// Positive control: an ordinary project with a genuinely legacy-keyed
	// artifact.
	controlDownloadPath := t.TempDir()
	seedManifestAt(t, controlDownloadPath, "ctrl", "coll", "1.0.0")
	controlReqPath := filepath.Join(t.TempDir(), "requirements.yml")
	if err := os.WriteFile(controlReqPath, []byte("collections: []\n"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write control project's requirements file: %v", err)
	}
	controlLegacyKey := legacyArtifactKey("ctrl", "coll", "1.0.0")
	controlLegacyPath := filepath.Join(cacheDir, controlLegacyKey)
	if err := os.WriteFile(controlLegacyPath, []byte("legacy-bytes"), helpers.FileMod); err != nil {
		t.Fatalf("failed to seed the control project's legacy artifact: %v", err)
	}

	writeProjectRegistry(t, cacheDir, &store.ProjectRegistry{
		Projects: map[string]store.ProjectRecord{
			"hostile-project": {RequirementsFile: hostileReqPath, CollectionsPath: hostileDownloadPath, LastRun: time.Now().UTC()},
			"control-project": {RequirementsFile: controlReqPath, CollectionsPath: controlDownloadPath, LastRun: time.Now().UTC()},
		},
	})

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}

	if _, err := os.Stat(scopedArtifactPath); err != nil {
		t.Fatalf("expected the scoped artifact to survive the legacy sweep, stat error: %v", err)
	}
	if _, err := os.Stat(controlLegacyPath); !os.IsNotExist(err) {
		t.Fatalf("expected the genuinely legacy-keyed control artifact to be swept, stat error: %v", err)
	}
}

// seedSnapshotInstalled saves a store snapshot at cfg.CacheDir whose
// Installed set is exactly entries, via the real local backend (SaveStore),
// mirroring how a prior install run would have persisted it.
func seedSnapshotInstalled(t *testing.T, cfg *config.Config, runtime *infra.Infra, entries map[string]store.InstalledEntry) {
	t.Helper()
	backend, err := cacheBackend.New(cfg, runtime)
	if err != nil {
		t.Fatalf("failed to build backend for seeding: %v", err)
	}
	if err := backend.Open(t.Context()); err != nil {
		t.Fatalf("failed to open backend for seeding: %v", err)
	}
	defer func() {
		if err := backend.Close(t.Context()); err != nil {
			t.Errorf("failed to close seeding backend: %v", err)
		}
	}()
	st := store.New()
	for key, entry := range entries {
		st.SetInstalled(key, entry)
	}
	if err := backend.SaveStore(t.Context(), st); err != nil {
		t.Fatalf("failed to save seeded store: %v", err)
	}
}

// seedSnapshotWarmed saves a store snapshot at cfg.CacheDir whose Warmed set
// is exactly entries, via the real local backend (SaveStore), mirroring how a
// prior warm run would have persisted it. Entries are written directly into
// the exported Warmed map rather than through Store.SetWarmed, so a caller
// can seed a deliberately stale WarmedAt (SetWarmed always stamps the current
// time and cannot produce that).
func seedSnapshotWarmed(t *testing.T, cfg *config.Config, runtime *infra.Infra, entries map[string]store.WarmedEntry) {
	t.Helper()
	backend, err := cacheBackend.New(cfg, runtime)
	if err != nil {
		t.Fatalf("failed to build backend for seeding: %v", err)
	}
	if err := backend.Open(t.Context()); err != nil {
		t.Fatalf("failed to open backend for seeding: %v", err)
	}
	defer func() {
		if err := backend.Close(t.Context()); err != nil {
			t.Errorf("failed to close seeding backend: %v", err)
		}
	}()
	st := store.New()
	maps.Copy(st.Warmed, entries)
	if err := backend.SaveStore(t.Context(), st); err != nil {
		t.Fatalf("failed to save seeded store: %v", err)
	}
}

// seedExtractedDir creates <cacheDir>/extracted/<sha>/ with a ready marker,
// mirroring the on-disk layout extracted.Store.Ensure/Promote produce, so
// Sweep sees a real completed entry rather than an empty directory.
func seedExtractedDir(t *testing.T, cacheDir, sha string) {
	t.Helper()
	dir := filepath.Join(cacheDir, extracted.RootDirName, sha)
	if err := os.MkdirAll(dir, helpers.DirMod); err != nil {
		t.Fatalf("failed to create extracted dir for %s: %v", sha, err)
	}
	if err := os.WriteFile(filepath.Join(dir, extracted.ReadyMarker), []byte("ok"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write ready marker for %s: %v", sha, err)
	}
}

// recordAbsentWorkspaceProject records a project pointing at downloadPath,
// which deliberately has no ansible_collections subdirectory. This makes
// scanProjectWorkspace skip the project entirely, so its installed snapshot
// entries are never scanned, never pruned, and never contribute to
// installedByKey - the normal ephemeral-CI state where a fresh runner has no
// local ansible_collections workspace at all.
func recordAbsentWorkspaceProject(t *testing.T, cfg *config.Config, runtime *infra.Infra, downloadPath string) {
	t.Helper()
	reqPath := filepath.Join(t.TempDir(), "requirements.yml")
	backend, err := cacheBackend.New(cfg, runtime)
	if err != nil {
		t.Fatalf("failed to build backend to record project: %v", err)
	}
	if err := backend.Open(t.Context()); err != nil {
		t.Fatalf("failed to open backend to record project: %v", err)
	}
	defer func() {
		if err := backend.Close(t.Context()); err != nil {
			t.Errorf("failed to close backend after recording project: %v", err)
		}
	}()
	if err := backend.RecordProject(t.Context(), reqPath, downloadPath); err != nil {
		t.Fatalf("failed to record project: %v", err)
	}
}

// TestSweepKeepsCacheWhenWorkspaceAbsent is the core regression guard for
// deriving the extracted-store keep set from the persisted snapshot instead
// of from on-disk workspaces: a project whose ansible_collections workspace
// is absent contributes nothing to installedByKey/reachable, but its
// snapshot Installed entries survive untouched (removeUnused only prunes
// entries it actually iterated), so their extracted artifact trees must
// still be kept. Under the old on-disk-derived keep set, an empty
// installedByKey meant Sweep(empty) wiped the entire extracted cache.
func TestSweepKeepsCacheWhenWorkspaceAbsent(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	entries := map[string]store.InstalledEntry{
		"ns.name@1.0.0":  {ArtifactSHA256: "sha-keep-1"},
		"ns.other@2.0.0": {ArtifactSHA256: "sha-keep-2"},
	}
	seedSnapshotInstalled(t, cfg, runtime, entries)
	for _, entry := range entries {
		seedExtractedDir(t, cacheDir, entry.ArtifactSHA256)
	}
	recordAbsentWorkspaceProject(t, cfg, runtime, downloadPath)

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}

	for _, entry := range entries {
		dir := filepath.Join(cacheDir, extracted.RootDirName, entry.ArtifactSHA256)
		if _, statErr := os.Stat(dir); statErr != nil {
			t.Fatalf("expected extracted dir %s to survive an absent workspace, stat error: %v", entry.ArtifactSHA256, statErr)
		}
	}
}

// seedSnapshotInstalledAndWarmed saves a store snapshot at cfg.CacheDir whose
// Installed and Warmed sets are exactly installed and warmed, in a single
// SaveStore call. Calling seedSnapshotInstalled and seedSnapshotWarmed back
// to back would not work for a test needing both: each builds its own
// store.New() and Save fully replaces every bucket from what it is given, so
// the second, separate seeding call would silently wipe the bucket the first
// one just wrote.
func seedSnapshotInstalledAndWarmed(
	t *testing.T,
	cfg *config.Config,
	runtime *infra.Infra,
	installed map[string]store.InstalledEntry,
	warmed map[string]store.WarmedEntry,
) {
	t.Helper()
	backend, err := cacheBackend.New(cfg, runtime)
	if err != nil {
		t.Fatalf("failed to build backend for seeding: %v", err)
	}
	if err := backend.Open(t.Context()); err != nil {
		t.Fatalf("failed to open backend for seeding: %v", err)
	}
	defer func() {
		if err := backend.Close(t.Context()); err != nil {
			t.Errorf("failed to close seeding backend: %v", err)
		}
	}()
	st := store.New()
	for key, entry := range installed {
		st.SetInstalled(key, entry)
	}
	maps.Copy(st.Warmed, warmed)
	if err := backend.SaveStore(t.Context(), st); err != nil {
		t.Fatalf("failed to save seeded store: %v", err)
	}
}

// TestSweepKeepsWarmedShaWithNoInstalledEntry pins the invariant that a
// warm-only machine's snapshot - which has no Installed entries at all,
// since warm never calls recordInstall, only a Warmed entry - still keeps
// its extracted tree during a sweep. Its recorded project's workspace does
// not exist (scanProjectWorkspace returns scanned=false and the project is
// skipped entirely, exactly like TestSweepKeepsCacheWhenWorkspaceAbsent's
// install-side scenario), so extractedKeepSet must also consult the
// Warmed set's sha, not only InstalledArtifactSHAByKey, or Sweep would wipe
// the extracted tree warm had just materialized.
func TestSweepKeepsWarmedShaWithNoInstalledEntry(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	seedSnapshotWarmed(t, cfg, runtime, map[string]store.WarmedEntry{
		"ns.name@1.0.0": {WarmedAt: time.Now().UTC(), ArtifactSHA256: "sha-warmed"},
	})
	seedExtractedDir(t, cacheDir, "sha-warmed")
	recordAbsentWorkspaceProject(t, cfg, runtime, downloadPath)

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}

	assertExtractedDirsSurvive(t, cacheDir, "sha-warmed")
}

// TestSweepPrunesStaleWarmedSha proves a warmed entry only protects its
// extracted tree while it is fresh: one seeded with a WarmedAt older than
// helpers.WarmedEntryMaxAge no longer contributes to the keep set, so its
// extracted tree is swept exactly like a genuine orphan would be.
//
// The stale entry never reaches cleanup's keep set: the persist-time prune
// inside seedSnapshotWarmed's own SaveStore (snapshotData/copyFreshWarmed)
// already drops it, so it is absent from the snapshot cleanup loads. This
// test therefore pins the end-to-end retention outcome and stays green if
// either retention site survives alone; WarmedArtifactSHAByKey's own
// read-side filter - the one that matters when an entry goes stale between
// its last save and a later cleanup run - is pinned separately by
// TestWarmedArtifactSHAByKeyExcludesStaleEntry.
func TestSweepPrunesStaleWarmedSha(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	staleAt := time.Now().UTC().Add(-40 * 24 * time.Hour)
	seedSnapshotWarmed(t, cfg, runtime, map[string]store.WarmedEntry{
		"ns.name@1.0.0": {WarmedAt: staleAt, ArtifactSHA256: "sha-stale-warmed"},
	})
	seedExtractedDir(t, cacheDir, "sha-stale-warmed")
	recordAbsentWorkspaceProject(t, cfg, runtime, downloadPath)

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}

	dir := filepath.Join(cacheDir, extracted.RootDirName, "sha-stale-warmed")
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Fatalf("expected the stale warmed entry's extracted dir to be swept, stat error: %v", statErr)
	}
}

// TestSweepKeepsWarmedShaWhenInstalledEntryRemovedAsUnreachable proves
// the warmed half of extractedKeepSet is never filtered by the installed
// half's wouldRemove exclusion: a key that is
// both installed (and gets removed as unreachable in this very run) and
// warmed must still have its extracted tree kept, since warm's intent is
// independent of install reachability. seedInstallTree/registerCleanupProject
// give ns.name@1.0.0 a real, unreferenced on-disk install, so removeUnused
// actually deletes its InstalledEntry in this run - the exact case where the
// two signals disagree and the warmed union must win.
func TestSweepKeepsWarmedShaWhenInstalledEntryRemovedAsUnreachable(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()
	installDir := seedInstallTree(t, downloadPath)
	registerCleanupProject(t, cacheDir, downloadPath)

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	const sha = "sha-shared"
	seedSnapshotInstalledAndWarmed(t, cfg, runtime,
		map[string]store.InstalledEntry{"ns.name@1.0.0": {ArtifactSHA256: sha}},
		map[string]store.WarmedEntry{"ns.name@1.0.0": {WarmedAt: time.Now().UTC(), ArtifactSHA256: sha}},
	)
	seedExtractedDir(t, cacheDir, sha)

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}

	manifestPath := filepath.Join(installDir, "MANIFEST.json")
	if _, statErr := os.Stat(manifestPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected the unreferenced collection's install to be removed, stat error: %v", statErr)
	}
	assertExtractedDirsSurvive(t, cacheDir, sha)
}

// TestSweepDropsUnreferencedSha proves the snapshot-derived keep set still
// drops a genuinely orphaned extracted entry: one referenced by an installed
// snapshot entry survives, one with no referencing entry at all is removed.
func TestSweepDropsUnreferencedSha(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	seedSnapshotInstalled(t, cfg, runtime, map[string]store.InstalledEntry{
		"ns.name@1.0.0": {ArtifactSHA256: "sha-keep"},
	})
	seedExtractedDir(t, cacheDir, "sha-keep")
	seedExtractedDir(t, cacheDir, "sha-orphan")
	recordAbsentWorkspaceProject(t, cfg, runtime, downloadPath)

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}

	keepDir := filepath.Join(cacheDir, extracted.RootDirName, "sha-keep")
	if _, statErr := os.Stat(keepDir); statErr != nil {
		t.Fatalf("expected referenced sha dir to survive, stat error: %v", statErr)
	}
	orphanDir := filepath.Join(cacheDir, extracted.RootDirName, "sha-orphan")
	if _, statErr := os.Stat(orphanDir); !os.IsNotExist(statErr) {
		t.Fatalf("expected unreferenced orphan sha dir to be removed, stat error: %v", statErr)
	}
}

// TestScanIgnoresNestedManifest proves scanInstalledCollections only ever
// looks at the fixed <ansible_collections>/<ns>/<name>/MANIFEST.json depth:
// a MANIFEST.json nested deeper - as a collection's own test fixtures might
// ship one - must never be mistaken for an installed collection. The nested
// manifest here declares a different, unreferenced collection identity, so
// under the old full-tree walk it would have been discovered as its own
// installedCollection and, being unreferenced, deleted by removeUnused. The
// top-level collection is kept reachable via requirements.yml so its
// directory is never a deletion candidate either way, isolating the
// assertion to whether the nested manifest alone was (wrongly) discovered.
func TestScanIgnoresNestedManifest(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()
	installDir := seedInstallTree(t, downloadPath)

	nestedDir := filepath.Join(installDir, "tests", "fixtures")
	if err := os.MkdirAll(nestedDir, helpers.DirMod); err != nil {
		t.Fatalf("failed to create nested fixture dir: %v", err)
	}
	nestedManifest := `{
		"collection_info": {
			"namespace": "phantom",
			"name": "fixture",
			"version": "9.9.9"
		}
	}`
	nestedManifestPath := filepath.Join(nestedDir, "MANIFEST.json")
	if err := os.WriteFile(nestedManifestPath, []byte(nestedManifest), helpers.FileMod); err != nil {
		t.Fatalf("failed to write nested manifest: %v", err)
	}

	reqPath := filepath.Join(t.TempDir(), "requirements.yml")
	if err := os.WriteFile(reqPath, []byte("collections:\n  - ns.name\n"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write requirements file: %v", err)
	}
	registerCleanupProjectAt(t, cacheDir, downloadPath, reqPath)

	runtime := newTestRuntime()
	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}

	assertManifestSurvives(t, installDir)
	if _, statErr := os.Stat(nestedManifestPath); statErr != nil {
		t.Fatalf("expected the nested phantom manifest to survive since it is never scanned, stat error: %v", statErr)
	}
}

// TestScanSkipsDirWithoutManifest proves a <ns>/<name> directory with no
// MANIFEST.json at all (e.g. a partially cleaned or interrupted install) is
// silently skipped rather than treated as an IO error that aborts the scan.
func TestScanSkipsDirWithoutManifest(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()

	emptyDir := filepath.Join(downloadPath, "ansible_collections", "ns", "empty")
	if err := os.MkdirAll(emptyDir, helpers.DirMod); err != nil {
		t.Fatalf("failed to create manifest-less dir: %v", err)
	}
	registerCleanupProject(t, cacheDir, downloadPath)

	runtime := newTestRuntime()
	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed with a manifest-less directory present, got %v", err)
	}
}

// TestRemovesAllCopiesInOneRun proves that when the same ns.name@version is
// installed under two distinct projects' collections paths, both on-disk
// copies are removed in a single Start run rather than one copy per run.
// installedByKey is keyed by ns.name@version but holds every project's copy
// for that key (map[string][]installedCollection), so a second project's
// scan appends to, rather than overwrites, the first project's record, and
// removeUnused sees and removes every recorded copy, not just the last one
// scanned.
func TestRemovesAllCopiesInOneRun(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPathA := t.TempDir()
	downloadPathB := t.TempDir()
	installDirA := seedInstallTree(t, downloadPathA)
	installDirB := seedInstallTree(t, downloadPathB)

	// Both projects need a present workspace (checked above via
	// seedInstallTree) and a readable, valid requirements file so the run
	// reaches removeUnused rather than aborting via the requirements-load
	// guard. Neither references ns.name, so the key is unreachable from
	// both projects.
	reqPathA := filepath.Join(t.TempDir(), "requirements-a.yml")
	if err := os.WriteFile(reqPathA, []byte("collections: []\n"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write requirements file for project A: %v", err)
	}
	reqPathB := filepath.Join(t.TempDir(), "requirements-b.yml")
	if err := os.WriteFile(reqPathB, []byte("collections: []\n"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write requirements file for project B: %v", err)
	}
	writeProjectRegistry(t, cacheDir, &store.ProjectRegistry{
		Projects: map[string]store.ProjectRecord{
			"proj-a": {
				RequirementsFile: reqPathA,
				CollectionsPath:  downloadPathA,
				LastRun:          time.Now().UTC(),
			},
			"proj-b": {
				RequirementsFile: reqPathB,
				CollectionsPath:  downloadPathB,
				LastRun:          time.Now().UTC(),
			},
		},
	})

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}

	manifestA := filepath.Join(installDirA, "MANIFEST.json")
	if _, statErr := os.Stat(manifestA); !os.IsNotExist(statErr) {
		t.Fatalf("expected project A's copy to be removed in this single run, stat error: %v", statErr)
	}
	manifestB := filepath.Join(installDirB, "MANIFEST.json")
	if _, statErr := os.Stat(manifestB); !os.IsNotExist(statErr) {
		t.Fatalf("expected project B's copy to be removed in this single run, stat error: %v", statErr)
	}
}

// TestReportsCorruptManifest proves a MANIFEST.json that exists but fails to
// parse as JSON is reported via a warning rather than silently disappearing,
// while the fail-safe invariant holds: an unidentifiable install is neither
// a reachability source nor a deletion candidate, so its on-disk tree
// survives. A separate, valid installed collection alongside it is still
// processed normally (it is unreferenced by requirements, so it is removed),
// proving the corrupt manifest does not disrupt the rest of the scan, and
// Start does not return an error - a corrupt manifest is reported, not
// fatal.
func TestReportsCorruptManifest(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()
	validInstallDir := seedInstallTree(t, downloadPath)

	corruptDir := filepath.Join(downloadPath, "ansible_collections", "corruptns", "corruptname")
	if err := os.MkdirAll(corruptDir, helpers.DirMod); err != nil {
		t.Fatalf("failed to create corrupt manifest dir: %v", err)
	}
	corruptManifestPath := filepath.Join(corruptDir, "MANIFEST.json")
	if err := os.WriteFile(corruptManifestPath, []byte("{invalid"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write corrupt manifest: %v", err)
	}

	reqPath := filepath.Join(t.TempDir(), "requirements.yml")
	if err := os.WriteFile(reqPath, []byte("collections: []\n"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write requirements file: %v", err)
	}
	registerCleanupProjectAt(t, cacheDir, downloadPath, reqPath)

	printer := &recordingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)
	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected a corrupt manifest to be reported rather than fatal, got error: %v", err)
	}

	if !printer.hasWarningContaining("corrupt manifest") {
		t.Fatalf("expected a warning about a corrupt manifest, got: %v", printer.warnings)
	}
	if _, statErr := os.Stat(corruptManifestPath); statErr != nil {
		t.Fatalf("expected the corrupt manifest's tree to survive, stat error: %v", statErr)
	}
	validManifestPath := filepath.Join(validInstallDir, "MANIFEST.json")
	if _, statErr := os.Stat(validManifestPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected the unreferenced valid collection to still be removed normally, stat error: %v", statErr)
	}
}

// seedManifestAt writes a MANIFEST.json for ns.name@version under
// <root>/ansible_collections/<ns>/<name>, for tests that need more than one
// distinct installed collection under the same collections root.
func seedManifestAt(t *testing.T, root, ns, name, version string) {
	t.Helper()
	seedManifestWithDeps(t, root, ns, name, version, nil)
}

// manifestJSON builds a MANIFEST.json body for ns.name@version. A non-empty
// deps map is rendered as collection_info.dependencies (raw FQDN -> raw
// constraint string, exactly the shape extractDeps reads), matching a real
// manifest's declared dependencies; a nil/empty map omits the field
// entirely, matching a manifest with no declared dependencies.
func manifestJSON(ns, name, version string, deps map[string]string) string {
	fields := fmt.Sprintf(`"namespace": %q, "name": %q, "version": %q`, ns, name, version)
	if len(deps) > 0 {
		keys := make([]string, 0, len(deps))
		for depFQDN := range deps {
			keys = append(keys, depFQDN)
		}
		sort.Strings(keys)
		pairs := make([]string, 0, len(deps))
		for _, depFQDN := range keys {
			pairs = append(pairs, fmt.Sprintf("%q: %q", depFQDN, deps[depFQDN]))
		}
		fields += fmt.Sprintf(`, "dependencies": {%s}`, strings.Join(pairs, ", "))
	}
	return fmt.Sprintf(`{"collection_info": {%s}}`, fields)
}

// seedManifestWithDeps writes a MANIFEST.json for ns.name@version, with an
// optional declared dependencies map, under
// <root>/ansible_collections/<ns>/<name>.
func seedManifestWithDeps(t *testing.T, root, ns, name, version string, deps map[string]string) {
	t.Helper()
	installDir := filepath.Join(root, "ansible_collections", ns, name)
	if err := os.MkdirAll(installDir, helpers.DirMod); err != nil {
		t.Fatalf("failed to create install dir for %s.%s: %v", ns, name, err)
	}
	manifest := manifestJSON(ns, name, version, deps)
	if err := os.WriteFile(filepath.Join(installDir, "MANIFEST.json"), []byte(manifest), helpers.FileMod); err != nil {
		t.Fatalf("failed to write manifest for %s.%s: %v", ns, name, err)
	}
}

// assertManifestPresentAt fails the test unless the MANIFEST.json for
// ns.name still exists under root/ansible_collections.
func assertManifestPresentAt(t *testing.T, root, ns, name string) {
	t.Helper()
	path := filepath.Join(root, "ansible_collections", ns, name, "MANIFEST.json")
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("expected manifest for %s.%s to survive, stat error: %v", ns, name, statErr)
	}
}

// assertManifestAbsentAt fails the test unless the MANIFEST.json for
// ns.name has been removed from under root/ansible_collections.
func assertManifestAbsentAt(t *testing.T, root, ns, name string) {
	t.Helper()
	path := filepath.Join(root, "ansible_collections", ns, name, "MANIFEST.json")
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("expected manifest for %s.%s to be removed, stat error: %v", ns, name, statErr)
	}
}

// assertExtractedDirGone fails the test unless the named extracted entry
// under cacheDir/extracted has been removed from disk.
func assertExtractedDirGone(t *testing.T, cacheDir, sha string) {
	t.Helper()
	dir := filepath.Join(cacheDir, extracted.RootDirName, sha)
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Fatalf("expected extracted dir %s to be swept, stat error: %v", sha, statErr)
	}
}

// TestMarkReachableFollowsTransitiveDependency proves the core reachability
// safety guarantee: a collection that is not itself a root requirement, but
// is a declared dependency of one, must survive cleanup, and this
// reachability must also drive the snapshot-derived extracted-store
// sweep. ns.a is the sole root (via requirements.yml); ns.a declares
// ns.b as a dependency in its manifest (exercising extractDeps's non-nil
// branch and markReachable's deps[current] walk with a real, parsed
// constraint via selectInstalled); ns.c is undeclared and unreferenced,
// serving as the control that proves the run still removes what is
// genuinely unreachable rather than becoming a blanket keep-everything.
func TestMarkReachableFollowsTransitiveDependency(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()

	seedManifestWithDeps(t, downloadPath, "ns", "a", "1.0.0", map[string]string{"ns.b": ">=1.0.0"})
	seedManifestWithDeps(t, downloadPath, "ns", "b", "1.0.0", nil)
	seedManifestWithDeps(t, downloadPath, "ns", "c", "1.0.0", nil)

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	seedSnapshotInstalled(t, cfg, newTestRuntime(), map[string]store.InstalledEntry{
		"ns.b@1.0.0": {ArtifactSHA256: "sha-b"},
		"ns.c@1.0.0": {ArtifactSHA256: "sha-c"},
	})
	seedExtractedDir(t, cacheDir, "sha-b")
	seedExtractedDir(t, cacheDir, "sha-c")

	reqPath := filepath.Join(t.TempDir(), "requirements.yml")
	if err := os.WriteFile(reqPath, []byte("collections:\n  - ns.a\n"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write requirements file: %v", err)
	}
	registerCleanupProjectAt(t, cacheDir, downloadPath, reqPath)

	runtime := newTestRuntime()
	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}

	assertManifestPresentAt(t, downloadPath, "ns", "a")
	assertManifestPresentAt(t, downloadPath, "ns", "b")
	assertManifestAbsentAt(t, downloadPath, "ns", "c")

	assertExtractedDirsSurvive(t, cacheDir, "sha-b")
	assertExtractedDirGone(t, cacheDir, "sha-c")
}

// assertExtractedDirsSurvive fails the test unless every named extracted
// entry under cacheDir/extracted is still present on disk.
func assertExtractedDirsSurvive(t *testing.T, cacheDir string, shas ...string) {
	t.Helper()
	for _, sha := range shas {
		dir := filepath.Join(cacheDir, extracted.RootDirName, sha)
		if _, statErr := os.Stat(dir); statErr != nil {
			t.Fatalf("expected extracted dir %s to survive, stat error: %v", sha, statErr)
		}
	}
}

// TestDryRunReportsExtractedSweep proves a dry run reports the extracted
// cache entries it would sweep instead of silently returning early. The
// snapshot references two keys: one reachable (kept) and one unreachable
// (would be removed), plus a third extracted entry with no referencing key
// at all (a genuine orphan). Because dry-run does not prune the snapshot,
// the would-be-removed key's SHA must be excluded from keep explicitly (via
// reachable/installedByKey) for the report to match what a real run would
// actually sweep. The non-negotiable assertion is that dry-run deletes
// nothing at all; the reported plan is asserted as a secondary check.
func TestDryRunReportsExtractedSweep(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()

	seedManifestAt(t, downloadPath, "ns", "name", "1.0.0")
	seedManifestAt(t, downloadPath, "orphan", "name", "2.0.0")

	cfg := &config.Config{CacheDir: cacheDir, DryRun: true}
	seedSnapshotInstalled(t, cfg, newTestRuntime(), map[string]store.InstalledEntry{
		"ns.name@1.0.0":     {ArtifactSHA256: "sha-keep"},
		"orphan.name@2.0.0": {ArtifactSHA256: "sha-would-remove"},
	})
	for _, sha := range []string{"sha-keep", "sha-would-remove", "sha-orphan"} {
		seedExtractedDir(t, cacheDir, sha)
	}

	reqPath := filepath.Join(t.TempDir(), "requirements.yml")
	if err := os.WriteFile(reqPath, []byte("collections:\n  - ns.name\n"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write requirements file: %v", err)
	}
	registerCleanupProjectAt(t, cacheDir, downloadPath, reqPath)

	printer := &recordingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected dry-run Start to succeed, got %v", err)
	}

	// Primary, non-negotiable assertion: dry-run deletes nothing at all.
	assertExtractedDirsSurvive(t, cacheDir, "sha-keep", "sha-would-remove", "sha-orphan")

	// Secondary assertion: the report accurately reflects what a real run
	// would sweep - both the unreachable key's SHA and the true orphan, but
	// not the reachable, kept key's SHA.
	if !printer.hasPrintContaining("sha-would-remove") {
		t.Fatalf("expected a would-sweep report for sha-would-remove, got prints: %v", printer.prints)
	}
	if !printer.hasPrintContaining("sha-orphan") {
		t.Fatalf("expected a would-sweep report for sha-orphan, got prints: %v", printer.prints)
	}
	if printer.hasPrintContaining("sha-keep") {
		t.Fatalf("expected no would-sweep report for the reachable, kept sha-keep, got prints: %v", printer.prints)
	}
}

// TestDryRunReportsExtractedSweepExcludesWarmedSha is the dry-run mirror of
// TestDryRunReportsExtractedSweep for a warmed entry: a warmed sha must never
// appear in the would-sweep report (the same as a reachable installed sha
// would not), while an unrelated true orphan is still reported, and dry-run
// deletes nothing either way.
func TestDryRunReportsExtractedSweepExcludesWarmedSha(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()

	cfg := &config.Config{CacheDir: cacheDir, DryRun: true}
	seedRuntime := newTestRuntime()
	seedSnapshotWarmed(t, cfg, seedRuntime, map[string]store.WarmedEntry{
		"ns.name@1.0.0": {WarmedAt: time.Now().UTC(), ArtifactSHA256: "sha-warmed"},
	})
	seedExtractedDir(t, cacheDir, "sha-warmed")
	seedExtractedDir(t, cacheDir, "sha-orphan")
	recordAbsentWorkspaceProject(t, cfg, seedRuntime, downloadPath)

	printer := &recordingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected dry-run Start to succeed, got %v", err)
	}

	// Primary, non-negotiable assertion: dry-run deletes nothing at all.
	assertExtractedDirsSurvive(t, cacheDir, "sha-warmed", "sha-orphan")

	// Secondary assertion: the report never names a warmed, kept sha, but
	// still names the true orphan.
	if printer.hasPrintContaining("sha-warmed") {
		t.Fatalf("expected no would-sweep report for the warmed sha-warmed, got prints: %v", printer.prints)
	}
	if !printer.hasPrintContaining("sha-orphan") {
		t.Fatalf("expected a would-sweep report for sha-orphan, got prints: %v", printer.prints)
	}
}

// TestDryRunReportsLegacyArtifactSweep proves sweepLegacyArtifacts' dry-run
// path: it reports only a legacy-keyed entry that actually exists on disk,
// deletes nothing, and does not report a collection that never had a
// legacy-keyed artifact in the first place. ns.other is seeded with no
// legacy artifact specifically to pin that last property: the assertion
// checks for its own legacyArtifactKey value's absence from the report, not
// merely a loose "legacy artifact" substring, since the substring alone
// would still pass even if reportLegacyArtifactSweepCandidate's own
// has == false early return (a Has probe against a key that was never
// written) silently regressed into reporting every scanned collection
// regardless of whether it ever had a legacy-keyed artifact.
func TestDryRunReportsLegacyArtifactSweep(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()
	seedManifestAt(t, downloadPath, "ns", "name", "1.0.0")
	seedManifestAt(t, downloadPath, "ns", "other", "1.0.0")

	reqPath := filepath.Join(t.TempDir(), "requirements.yml")
	if err := os.WriteFile(reqPath, []byte("collections:\n  - ns.name\n  - ns.other\n"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write requirements file: %v", err)
	}
	registerCleanupProjectAt(t, cacheDir, downloadPath, reqPath)

	legacyPath := filepath.Join(cacheDir, legacyArtifactKey("ns", "name", "1.0.0"))
	if err := os.WriteFile(legacyPath, []byte("legacy-bytes"), helpers.FileMod); err != nil {
		t.Fatalf("failed to seed legacy artifact: %v", err)
	}

	cfg := &config.Config{CacheDir: cacheDir, DryRun: true}
	printer := &recordingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected dry-run Start to succeed, got %v", err)
	}

	if _, err := os.Stat(legacyPath); err != nil {
		t.Fatalf("expected dry-run to delete nothing, but the legacy artifact is gone: %v", err)
	}
	legacyKey := legacyArtifactKey("ns", "name", "1.0.0")
	if !printer.hasPrintContaining(legacyKey) {
		t.Fatalf("expected a would-sweep report naming the legacy key %q, got prints: %v", legacyKey, printer.prints)
	}
	otherLegacyKey := legacyArtifactKey("ns", "other", "1.0.0")
	if printer.hasPrintContaining(otherLegacyKey) {
		t.Fatalf("expected no would-sweep report for ns.other's own legacy key %q, got prints: %v", otherLegacyKey, printer.prints)
	}
}

// assertSelectedKeys fails the test unless the Key of every item in got is
// exactly the set of want, ignoring order.
func assertSelectedKeys(t *testing.T, got []installedCollection, want ...string) {
	t.Helper()
	gotKeys := make([]string, 0, len(got))
	for _, item := range got {
		gotKeys = append(gotKeys, item.Key)
	}
	sort.Strings(gotKeys)
	wantSorted := append([]string(nil), want...)
	sort.Strings(wantSorted)
	if len(gotKeys) != len(wantSorted) {
		t.Fatalf("selected keys = %v, want %v", gotKeys, wantSorted)
	}
	for i, k := range gotKeys {
		if k != wantSorted[i] {
			t.Fatalf("selected keys = %v, want %v", gotKeys, wantSorted)
		}
	}
}

// TestSelectInstalledConstraintCache proves the constraint cache introduced
// to hoist semver parsing out of the BFS edge loop preserves selectInstalled's
// exact prior behavior: a valid constraint filters correctly (including
// skipping an item whose version failed to parse), an unparseable or empty
// constraint is match-all, and repeated calls with the same constraint string
// hit the cache and still yield identical results.
func TestSelectInstalledConstraintCache(t *testing.T) {
	t.Parallel()
	v15, err := semver.NewVersion("1.5.0")
	if err != nil {
		t.Fatalf("failed to parse 1.5.0: %v", err)
	}
	v20, err := semver.NewVersion("2.0.0")
	if err != nil {
		t.Fatalf("failed to parse 2.0.0: %v", err)
	}
	index := map[string][]installedCollection{
		"ns.name": {
			{Key: "ns.name@1.5.0", Version: "1.5.0", Parsed: v15},
			{Key: "ns.name@2.0.0", Version: "2.0.0", Parsed: v20},
			// An item whose version failed to parse (e.g. a git ref that
			// still passed the path-element safety check): Parsed is nil,
			// exactly as buildInstalledRecord would leave it.
			{Key: "ns.name@git-ref", Version: "git-ref", Parsed: nil},
		},
	}
	constraints := make(map[string]*semver.Constraints)

	// A valid constraint selects the matching parsed version and skips
	// both the out-of-range version and the unparseable-version item.
	selected := selectInstalled(index, constraints, "ns.name", ">=1.0.0,<2.0.0")
	assertSelectedKeys(t, selected, "ns.name@1.5.0")
	if len(constraints) != 1 {
		t.Fatalf("expected the constraint to be cached after first use, got %d entries", len(constraints))
	}

	// Calling again with the identical constraint string must hit the
	// cache (no new entry) and still yield the identical result set.
	selectedAgain := selectInstalled(index, constraints, "ns.name", ">=1.0.0,<2.0.0")
	assertSelectedKeys(t, selectedAgain, "ns.name@1.5.0")
	if len(constraints) != 1 {
		t.Fatalf("expected no new cache entry on a repeated constraint, got %d entries", len(constraints))
	}

	// An empty/"*" constraint is match-all without ever touching the cache.
	all := selectInstalled(index, constraints, "ns.name", "")
	assertSelectedKeys(t, all, "ns.name@1.5.0", "ns.name@2.0.0", "ns.name@git-ref")
	if len(constraints) != 1 {
		t.Fatalf("expected the match-all fast path not to populate the cache, got %d entries", len(constraints))
	}

	// An unparseable constraint is also match-all (matching
	// semver.NewConstraint's existing failure behavior), and gets cached as
	// nil so a repeated use of the same unparseable string costs only one
	// parse attempt.
	unparseable := selectInstalled(index, constraints, "ns.name", "not a constraint !!")
	assertSelectedKeys(t, unparseable, "ns.name@1.5.0", "ns.name@2.0.0", "ns.name@git-ref")
	c, ok := constraints["not a constraint !!"]
	if !ok || c != nil {
		t.Fatalf("expected the unparseable constraint to be cached as nil, got ok=%v c=%v", ok, c)
	}
}

// TestRemoveUnusedAbortsOnRemoveAllError proves an os.RemoveAll failure
// while removing an unreferenced install's on-disk tree propagates as a
// real error out of Start (via removeUnused's mid-loop abort) instead of
// being swallowed. Removing the "name" directory itself requires write
// permission on its parent ("ns"), which this test strips, so os.RemoveAll
// can still delete MANIFEST.json inside "name" (write permission on "name"
// itself is untouched) but fails on the final rmdir of "name" against its
// now-read-only parent: the install is left partially, not fully, removed,
// and the failed removal must not report success. Skipped when running as
// root, since root bypasses the permission bits this test relies on to
// force the failure.
func TestRemoveUnusedAbortsOnRemoveAllError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission-based removal guard cannot be tested")
	}
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()
	installDir := seedInstallTree(t, downloadPath)

	namespaceDir := filepath.Join(downloadPath, "ansible_collections", "ns")
	//nolint:gosec // G302: intentionally read-only (no write bit) to force os.RemoveAll to fail with a permission error.
	if err := os.Chmod(namespaceDir, 0o555); err != nil {
		t.Fatalf("failed to chmod namespace dir read-only: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(namespaceDir, helpers.DirMod); err != nil {
			t.Errorf("failed to restore namespace dir perms: %v", err)
		}
	})

	registerCleanupProject(t, cacheDir, downloadPath)

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	if err := Start(t.Context(), cfg, runtime); err == nil {
		t.Fatalf("expected a non-nil error when os.RemoveAll fails, got nil")
	}
	// The "name" directory entry itself is still present under its
	// now-read-only parent, since the final rmdir is exactly what failed -
	// even though its MANIFEST.json content was already removed as part of
	// the same (failed) os.RemoveAll call.
	if _, statErr := os.Stat(installDir); statErr != nil {
		t.Fatalf("expected the install directory to still be present after a failed removal, stat error: %v", statErr)
	}
}

// TestRemoveInstalledRemovesInfoDir proves the happy-path .info directory
// removal actually deletes it, rather than the WithinDir guard change
// leaving it as a silent no-op. The .info directory name mirrors exactly
// what removeInstalled itself constructs: fmt.Sprintf("%s.%s-%s.info", ns,
// name, version) joined under <collectionsDir>/ansible_collections. Both
// fixture trees also carry a read-only (0444) file, mirroring what a
// hardened extraction now produces, proving removeInstallPath and
// removeInfoDir still clear them: removal only needs write permission on
// the containing directory, never on the target file's own mode.
func TestRemoveInstalledRemovesInfoDir(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()
	installDir := seedInstallTree(t, downloadPath) // ns.name@1.0.0, unreferenced below

	// The install's MANIFEST.json is exactly the kind of file a hardened
	// extraction now produces read-only: removeInstallPath must still be
	// able to remove the directory it lives in even though the file itself
	// carries no write bit, since removal only needs write permission on the
	// containing directory, never on the target file's own mode.
	manifestPath := filepath.Join(installDir, "MANIFEST.json")
	//nolint:gosec // read-only (not writable) is exactly the fixture this test needs, not a leak risk.
	if err := os.Chmod(manifestPath, 0o444); err != nil {
		t.Fatalf("failed to chmod MANIFEST.json read-only: %v", err)
	}

	infoDir := filepath.Join(downloadPath, "ansible_collections", "ns.name-1.0.0.info")
	if err := os.MkdirAll(filepath.Join(infoDir, "marker"), helpers.DirMod); err != nil {
		t.Fatalf("failed to create .info dir: %v", err)
	}
	// .info sidecar content is never CAS-backed, but removeInfoDir must be
	// equally indifferent to a read-only file underneath it.
	infoFile := filepath.Join(infoDir, "marker", "GALAXY.yml")
	//nolint:gosec // read-only (not writable) is exactly the fixture this test needs, not a leak risk.
	if err := os.WriteFile(infoFile, []byte("collection_info: {}\n"), 0o444); err != nil {
		t.Fatalf("failed to write read-only .info file: %v", err)
	}

	registerCleanupProject(t, cacheDir, downloadPath)

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}
	if _, statErr := os.Stat(infoDir); !os.IsNotExist(statErr) {
		t.Fatalf("expected the .info directory to be removed, stat error: %v", statErr)
	}
	if _, statErr := os.Stat(installDir); !os.IsNotExist(statErr) {
		t.Fatalf("expected the install directory (with its read-only MANIFEST.json) to be removed, stat error: %v", statErr)
	}
}

// TestRemoveInstalledRefusesNamespaceSymlinkSwap proves the symlink-swap
// TOCTOU fix: if the namespace directory scanInstalledCollections indexed is
// replaced by a symlink pointing outside collections_path before
// removeInstalled runs - exactly the race a concurrent, locally-writable
// attacker could win against the old, purely lexical os.RemoveAll(
// inst.InstallPath) call - removeInstalled must not follow it. The lexical
// WithinDir check alone cannot catch this: the joined InstallPath is still
// textually inside CollectionsDir even though "ns" now resolves elsewhere on
// disk. os.Root is what actually closes it: RemoveAll refuses to traverse a
// symlink that escapes its root, so the outside decoy's sentinel file must
// survive untouched.
func TestRemoveInstalledRefusesNamespaceSymlinkSwap(t *testing.T) {
	t.Parallel()
	collectionsDir := t.TempDir()
	seedInstallTree(t, collectionsDir) // real ansible_collections/ns/name/MANIFEST.json

	outsideRoot := t.TempDir()
	sentinelFile := writeOutsideSentinel(t, outsideRoot, "ns-decoy")

	// Simulate the scan-to-removal race: the real "ns" directory the scan
	// saw is gone by the time removeInstalled runs, replaced by a symlink to
	// a directory entirely outside collectionsDir.
	nsDir := filepath.Join(collectionsDir, "ansible_collections", "ns")
	if err := os.RemoveAll(nsDir); err != nil {
		t.Fatalf("failed to remove the real ns dir before swapping it: %v", err)
	}
	if err := os.Symlink(filepath.Join(outsideRoot, "ns-decoy"), nsDir); err != nil {
		t.Fatalf("failed to symlink ns to the outside decoy: %v", err)
	}

	inst := installedCollection{
		Key:            "ns.name@1.0.0",
		FQDN:           "ns.name",
		Namespace:      "ns",
		Name:           "name",
		Version:        "1.0.0",
		InstallPath:    filepath.Join(nsDir, "name"),
		CollectionsDir: collectionsDir,
	}

	if err := removeInstalled(t.Context(), inst, nil, ""); err == nil {
		t.Fatalf("expected removeInstalled to refuse to follow the swapped ns symlink, got nil error")
	}
	if _, statErr := os.Stat(sentinelFile); statErr != nil {
		t.Fatalf("expected the outside decoy's sentinel file to survive, stat error: %v", statErr)
	}
}

// TestRemoveInstalledRefusesAnsibleCollectionsSymlinkSwap is the sibling of
// TestRemoveInstalledRefusesNamespaceSymlinkSwap for the other symlink-swap
// point that matters: the ansible_collections directory itself. Rooting
// os.Root at inst.CollectionsDir - not at its ansible_collections
// subdirectory - is what catches this case too: a Root opened one level
// deeper would never even see this swap, since the escape happens before
// reaching that deeper root.
func TestRemoveInstalledRefusesAnsibleCollectionsSymlinkSwap(t *testing.T) {
	t.Parallel()
	collectionsDir := t.TempDir()

	outsideRoot := t.TempDir()
	sentinelFile := writeOutsideSentinel(t, outsideRoot, "ac-decoy")

	// ansible_collections itself never exists as a real directory here - it
	// is a symlink to a directory entirely outside collectionsDir from the
	// start, standing in for the moment right after an attacker's swap.
	acDir := filepath.Join(collectionsDir, "ansible_collections")
	if err := os.Symlink(filepath.Join(outsideRoot, "ac-decoy"), acDir); err != nil {
		t.Fatalf("failed to symlink ansible_collections to the outside decoy: %v", err)
	}

	inst := installedCollection{
		Key:            "ns.name@1.0.0",
		FQDN:           "ns.name",
		Namespace:      "ns",
		Name:           "name",
		Version:        "1.0.0",
		InstallPath:    filepath.Join(acDir, "ns", "name"),
		CollectionsDir: collectionsDir,
	}

	if err := removeInstalled(t.Context(), inst, nil, ""); err == nil {
		t.Fatalf("expected removeInstalled to refuse to follow the swapped ansible_collections symlink, got nil error")
	}
	if _, statErr := os.Stat(sentinelFile); statErr != nil {
		t.Fatalf("expected the outside decoy's sentinel file to survive, stat error: %v", statErr)
	}
}

// errArtifactStoreStubNotImplemented is returned by recordingArtifactStore
// methods that TestRemoveInstalledDeletesArtifactWhenWorkspaceAbsent never
// exercises, so an unexpected call fails the test loudly instead of
// returning a misleadingly successful zero value.
var errArtifactStoreStubNotImplemented = errors.New("stub: method not implemented")

// recordingArtifactStore is a minimal cacheManager.ArtifactStore test double
// that records every key passed to Delete. removeInstalled only ever calls
// Delete on an ArtifactStore, so the other methods are never exercised here.
type recordingArtifactStore struct {
	deleted []string
}

func (a *recordingArtifactStore) Has(context.Context, string) (bool, error) {
	return false, errArtifactStoreStubNotImplemented
}

func (a *recordingArtifactStore) Meta(context.Context, string) (map[string]string, bool, error) {
	return nil, false, errArtifactStoreStubNotImplemented
}

func (a *recordingArtifactStore) Fetch(context.Context, string) (cacheManager.ArtifactFile, error) {
	return cacheManager.ArtifactFile{}, errArtifactStoreStubNotImplemented
}

func (a *recordingArtifactStore) TempFile(context.Context, string) (*os.File, func(), error) {
	return nil, nil, errArtifactStoreStubNotImplemented
}

func (a *recordingArtifactStore) Commit(
	context.Context, string, string, map[string]string,
) (cacheManager.ArtifactFile, error) {
	return cacheManager.ArtifactFile{}, errArtifactStoreStubNotImplemented
}

// Delete records key and always succeeds, matching a real ArtifactStore's
// best-effort use from removeInstalled (its error is ignored there).
func (a *recordingArtifactStore) Delete(_ context.Context, key string) error {
	a.deleted = append(a.deleted, key)
	return nil
}

// TestRemoveInstalledDeletesArtifactWhenWorkspaceAbsent proves the artifact
// purge is not gated on the on-disk workspace's presence: when
// inst.CollectionsDir does not exist at all - e.g. its project's workspace
// was already removed, or never existed this run - removeInstalled must
// still call artifacts.Delete with the collection's current, server-scoped
// artifact key rather than returning early before ever reaching the purge.
// This is the regression a naive "os.IsNotExist -> return nil" placed
// directly in removeInstalled (instead of in the workspace-only helper)
// would reintroduce: pre-os.Root, os.RemoveAll on an absent path was itself a
// silent no-op, so the artifact purge always ran regardless of workspace
// presence.
func TestRemoveInstalledDeletesArtifactWhenWorkspaceAbsent(t *testing.T) {
	t.Parallel()
	collectionsDir := filepath.Join(t.TempDir(), "does-not-exist")
	inst := installedCollection{
		Key:            "ns.name@1.0.0",
		FQDN:           "ns.name",
		Namespace:      "ns",
		Name:           "name",
		Version:        "1.0.0",
		InstallPath:    filepath.Join(collectionsDir, "ansible_collections", "ns", "name"),
		CollectionsDir: collectionsDir,
	}
	artifacts := &recordingArtifactStore{}
	const source = "https://galaxy.example.com/api"

	if err := removeInstalled(t.Context(), inst, artifacts, source); err != nil {
		t.Fatalf("expected nil error for an absent workspace, got %v", err)
	}

	wantKey := helpers.ArtifactKey(source, "ns-name-1.0.0.tar.gz")
	if len(artifacts.deleted) != 1 || artifacts.deleted[0] != wantKey {
		t.Fatalf("expected Delete to be called once with key %q, got %v", wantKey, artifacts.deleted)
	}
}

// TestRemoveInstalledSkipsArtifactPurgeWhenSourceUnknown proves removeInstalled
// never guesses an artifact key when source is unknown (""): purging the
// legacy flat key is sweepLegacyArtifacts' job now, not removeInstalled's, so
// an empty source - e.g. an InstalledEntry that predates the multi-server
// work, or was never recorded - means no Delete call at all, rather than one
// built from an empty, meaningless server fingerprint.
func TestRemoveInstalledSkipsArtifactPurgeWhenSourceUnknown(t *testing.T) {
	t.Parallel()
	collectionsDir := filepath.Join(t.TempDir(), "does-not-exist")
	inst := installedCollection{
		Key:            "ns.name@1.0.0",
		FQDN:           "ns.name",
		Namespace:      "ns",
		Name:           "name",
		Version:        "1.0.0",
		InstallPath:    filepath.Join(collectionsDir, "ansible_collections", "ns", "name"),
		CollectionsDir: collectionsDir,
	}
	artifacts := &recordingArtifactStore{}

	if err := removeInstalled(t.Context(), inst, artifacts, ""); err != nil {
		t.Fatalf("expected nil error for an absent workspace, got %v", err)
	}
	if len(artifacts.deleted) != 0 {
		t.Fatalf("expected no Delete call when source is unknown, got %v", artifacts.deleted)
	}
}

// TestScanAbortsOnRealIOErrorReadingManifest proves a genuine (non-
// os.IsNotExist) IO error reading a MANIFEST.json - e.g. a permission
// error - propagates as an abort out of Start rather than being treated as
// a benign skip or a corrupt-manifest warning, and that nothing is deleted
// before the abort. Skipped when running as root, since root bypasses the
// permission bits this test relies on to force the read failure.
func TestScanAbortsOnRealIOErrorReadingManifest(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission-based read guard cannot be tested")
	}
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()
	installDir := seedInstallTree(t, downloadPath)

	manifestPath := filepath.Join(installDir, "MANIFEST.json")
	if err := os.Chmod(manifestPath, 0o000); err != nil {
		t.Fatalf("failed to chmod manifest unreadable: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(manifestPath, helpers.FileMod); err != nil {
			t.Errorf("failed to restore manifest perms: %v", err)
		}
	})

	registerCleanupProject(t, cacheDir, downloadPath)

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	if err := Start(t.Context(), cfg, runtime); err == nil {
		t.Fatalf("expected Start to abort with a non-nil error on a real manifest read error, got nil")
	}
	// os.Stat only needs traversal permission on parent directories, not
	// read permission on the file itself, so this still proves the entry
	// survives untouched despite the file being unreadable.
	assertManifestSurvives(t, installDir)
}

// TestScanAbortsOnUnreadableCollectionDir proves the IO-error abort branch
// inside manifestIsRegularFile's own gate, the sibling of
// TestScanAbortsOnRealIOErrorReadingManifest's abort past the gate: a
// genuine Lstat failure on ansible_collections/<ns>/<name>/MANIFEST.json -
// as opposed to the entry simply not existing - still aborts the whole run
// rather than being silently skipped.
//
// chmod 0o000 on the collection's own <ns>/<name> directory (not the
// manifest file, and not ansible_collections itself) is what forces this
// specific failure and no other: the parent namespace listing
// (scanNamespaceDir's fs.ReadDir on ansible_collections/ns) only needs
// read+execute on ansible_collections/ns, which stays untouched, so it
// still succeeds and reaches this collection's own directory entry - but
// Lstat-ing MANIFEST.json inside that directory needs search permission on
// the directory itself, which is exactly what is now missing.
//
// The error is asserted to contain "statat", not just the collections path:
// that is what tells this abort apart from
// TestScanAbortsOnRealIOErrorReadingManifest's. That test's mode-0 target
// is the manifest FILE itself, which passes this gate cleanly (Lstat needs
// only search permission on the parent, not any permission on the file
// itself) and fails later, at readManifest's own ReadFile ("openat").
// Without asserting on "statat" specifically, both tests would only prove
// "Start aborted with the collections path in the message", which does not
// tell the two branches inside scanCollectionDir apart.
//
// Skipped when running as root, since root bypasses the permission bits
// this test relies on to force the failure.
func TestScanAbortsOnUnreadableCollectionDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission-based read guard cannot be tested")
	}
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()
	installDir := seedInstallTree(t, downloadPath)

	if err := os.Chmod(installDir, 0o000); err != nil {
		t.Fatalf("failed to chmod collection dir unreadable: %v", err)
	}
	restored := false
	restore := func() {
		if restored {
			return
		}
		if err := os.Chmod(installDir, helpers.DirMod); err != nil {
			t.Errorf("failed to restore collection dir perms: %v", err)
		}
		restored = true
	}
	t.Cleanup(restore)

	registerCleanupProject(t, cacheDir, downloadPath)
	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	err := Start(t.Context(), cfg, runtime)
	if err == nil {
		t.Fatalf("expected Start to abort on an unreadable collection directory, got nil")
	}
	if !strings.Contains(err.Error(), downloadPath) {
		t.Fatalf("expected the error to name the collections path %s, got %v", downloadPath, err)
	}
	if !strings.Contains(err.Error(), "statat") {
		t.Fatalf("expected the error to name the failing statat call, distinguishing this abort from a ReadFile (openat) one, got %v", err)
	}

	// The abort happens before removeUnused ever runs, so nothing could
	// have been deleted; the survival check runs only after restore(), not
	// right here, since a mode-0 collection directory blocks traversal to
	// the manifest for this test's own os.Stat call too.
	restore()
	assertManifestSurvives(t, installDir)

	// Positive control: the identical fixture with the collection directory
	// readable again must complete and remove the unreferenced install,
	// proving the abort above pins the permission failure specifically and
	// not some other property of this fixture.
	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed once the collection directory is readable again, got %v", err)
	}
	manifestPath := filepath.Join(installDir, "MANIFEST.json")
	if _, statErr := os.Stat(manifestPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected the unreferenced install to be removed once the read succeeds, stat error: %v", statErr)
	}
}

// TestDryRunReportsSweepPlanError proves reportExtractedSweepPlan's error
// branch: when SweepPlan itself fails (e.g. a real IO error reading the
// extracted store root), a dry run still completes successfully rather
// than aborting the whole cleanup over a read-only reporting step, but the
// failure is surfaced via Errorf rather than silently dropped. Skipped when
// running as root, since root bypasses the permission bits this test
// relies on to force the read failure.
func TestDryRunReportsSweepPlanError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission-based read guard cannot be tested")
	}
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()
	seedManifestAt(t, downloadPath, "ns", "name", "1.0.0")

	cfg := &config.Config{CacheDir: cacheDir, DryRun: true}
	seedSnapshotInstalled(t, cfg, newTestRuntime(), map[string]store.InstalledEntry{
		"ns.name@1.0.0": {ArtifactSHA256: "sha-keep"},
	})

	extractedRoot := filepath.Join(cacheDir, extracted.RootDirName)
	if err := os.MkdirAll(extractedRoot, helpers.DirMod); err != nil {
		t.Fatalf("failed to create extracted root: %v", err)
	}
	if err := os.Chmod(extractedRoot, 0o000); err != nil {
		t.Fatalf("failed to chmod extracted root unreadable: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(extractedRoot, helpers.DirMod); err != nil {
			t.Errorf("failed to restore extracted root perms: %v", err)
		}
	})

	reqPath := filepath.Join(t.TempDir(), "requirements.yml")
	if err := os.WriteFile(reqPath, []byte("collections:\n  - ns.name\n"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write requirements file: %v", err)
	}
	registerCleanupProjectAt(t, cacheDir, downloadPath, reqPath)

	printer := &recordingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected dry-run Start to still succeed despite a sweep-plan error, got %v", err)
	}
	if !printer.hasErrorContaining("failed to plan extracted cache sweep") {
		t.Fatalf("expected an Errorf about the failed sweep plan, got: %v", printer.errs)
	}
}

// TestScanInstalledCollectionsMissingRootIsNotAnError proves
// scanInstalledCollections treats a missing ansible_collections root as a
// benign no-op (nil error, no entries added) rather than an error, called
// directly rather than only indirectly through Start.
//
// This is not the ordinary absent-workspace path: openProjectWorkspace
// handles that at its own probe, before a workspace ever reaches
// scanInstalledCollections - an absent ansible_collections entry there just
// moves to the next candidate, and when no candidate is usable at all,
// produces the ordinary (workspace{}, nil) skip; only a non-fs.ErrNotExist
// outcome is refused. What this test pins instead is narrower: a race guard
// for ansible_collections disappearing between that probe and this read
// (e.g. a concurrent cleanup or install run removing it in between), not the
// everyday "this project has no workspace" case.
func TestScanInstalledCollectionsMissingRootIsNotAnError(t *testing.T) {
	t.Parallel()
	collectionsPath := t.TempDir() // no ansible_collections subdirectory created
	ws := openTestWorkspace(t, collectionsPath)
	defer func() { _ = ws.root.Close() }()
	index := make(map[string][]installedCollection)
	byKey := make(map[string][]installedCollection)
	deps := make(map[string]map[string]string)

	if err := scanInstalledCollections(noopPrinter{}, ws, index, byKey, deps); err != nil {
		t.Fatalf("expected nil error for a missing ansible_collections root, got %v", err)
	}
	if len(index) != 0 || len(byKey) != 0 || len(deps) != 0 {
		t.Fatalf("expected no entries to be added, got index=%v byKey=%v deps=%v", index, byKey, deps)
	}
}

// TestScanNamespaceDirVanishedIsNotAnError proves scanNamespaceDir treats a
// vanished namespace directory (present root, missing ns subdirectory) as a
// benign no-op (nil error, no entries added), called directly rather than
// only indirectly through Start.
func TestScanNamespaceDirVanishedIsNotAnError(t *testing.T) {
	t.Parallel()
	collectionsPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(collectionsPath, "ansible_collections"), helpers.DirMod); err != nil {
		t.Fatalf("failed to create ansible_collections root: %v", err)
	}
	ws := openTestWorkspace(t, collectionsPath) // root exists; the "does-not-exist" ns subdir under it does not
	defer func() { _ = ws.root.Close() }()
	index := make(map[string][]installedCollection)
	byKey := make(map[string][]installedCollection)
	deps := make(map[string]map[string]string)

	if err := scanNamespaceDir(noopPrinter{}, ws, "does-not-exist", index, byKey, deps); err != nil {
		t.Fatalf("expected nil error for a vanished namespace dir, got %v", err)
	}
	if len(index) != 0 || len(byKey) != 0 || len(deps) != 0 {
		t.Fatalf("expected no entries to be added, got index=%v byKey=%v deps=%v", index, byKey, deps)
	}
}

// seedEscapingWorkspaceFixture creates a fresh collections path whose
// ansible_collections entry is an absolute symlink escaping that path,
// pointing at a directory holding a real outside.coll@1.0.0 MANIFEST.json.
// It returns a store.ProjectRecord for that collections path (sharing reqPath
// as its requirements file) plus the outside manifest's on-disk path, so a
// caller can assert on its survival. It skips the calling test outright when
// os.Symlink is unavailable on this platform.
func seedEscapingWorkspaceFixture(t *testing.T, reqPath string) (store.ProjectRecord, string) {
	t.Helper()
	escapingCollectionsPath := t.TempDir()
	outsideDir := t.TempDir()
	outsideManifestDir := filepath.Join(outsideDir, "outside", "coll")
	if err := os.MkdirAll(outsideManifestDir, helpers.DirMod); err != nil {
		t.Fatalf("failed to create outside manifest dir: %v", err)
	}
	outsideManifest := `{
		"collection_info": {
			"namespace": "outside",
			"name": "coll",
			"version": "1.0.0"
		}
	}`
	outsideManifestPath := filepath.Join(outsideManifestDir, "MANIFEST.json")
	if err := os.WriteFile(outsideManifestPath, []byte(outsideManifest), helpers.FileMod); err != nil {
		t.Fatalf("failed to write outside manifest: %v", err)
	}

	acLink := filepath.Join(escapingCollectionsPath, "ansible_collections")
	if err := os.Symlink(outsideDir, acLink); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}

	return store.ProjectRecord{
		RequirementsFile: reqPath,
		CollectionsPath:  escapingCollectionsPath,
		LastRun:          time.Now().UTC(),
	}, outsideManifestPath
}

// TestStartSkipsProjectWhoseWorkspaceEscapes is the regression test for the
// defect this package's rooted scan closes: a project registered against a
// collections path whose ansible_collections entry is an absolute symlink
// escaping that path must be skipped with a warning, rather than making
// Start abort the whole run - taking every other, legitimate project's
// cleanup down with it - once the already-rooted removeWorkspaceFiles
// refuses to delete what the old, unrooted scan had indexed from outside the
// tree.
func TestStartSkipsProjectWhoseWorkspaceEscapes(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()

	reqPath := filepath.Join(t.TempDir(), "requirements.yml")
	if err := os.WriteFile(reqPath, []byte("collections: []\n"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write shared requirements file: %v", err)
	}
	escapingProject, outsideManifestPath := seedEscapingWorkspaceFixture(t, reqPath)

	healthyDownloadPath := t.TempDir()
	installDir := seedInstallTree(t, healthyDownloadPath)

	writeProjectRegistry(t, cacheDir, &store.ProjectRegistry{
		Projects: map[string]store.ProjectRecord{
			"escaping-project": escapingProject,
			"healthy-project": {
				RequirementsFile: reqPath,
				CollectionsPath:  healthyDownloadPath,
				LastRun:          time.Now().UTC(),
			},
		},
	})

	printer := &recordingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)
	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}

	// Confirmed killing mutation (reverting the probe in openProjectWorkspace
	// from root.Stat("ansible_collections") back to
	// os.Stat(filepath.Join(candidate, "ansible_collections"))): "expected
	// Start to finish despite the escaping workspace, got failed to scan
	// \"/var/.../003\": openat ansible_collections: path escapes from
	// parent". The plain os.Stat follows the escaping symlink and reports a
	// normal directory, so openProjectWorkspace treats the candidate as
	// usable instead of refusing it, and the subsequent rooted read
	// (scanInstalledCollections, still bound by root.FS()) then fails with a
	// path-escape error that aborts Start instead of being skipped.
	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to finish despite the escaping workspace, got %v", err)
	}
	// Confirmed killing mutation (deleting scanProjectWorkspace's out.Warnf
	// call): "expected a warning naming the skipped project, got: [no
	// persisted snapshot was found; skipping the extracted-cache sweep and
	// leaving the snapshot untouched this run]" - no other warning names the
	// skipped project.
	if !printer.hasWarningContaining(`skipping project "escaping-project"`) {
		t.Fatalf("expected a warning naming the skipped project, got: %v", printer.warnings)
	}
	// Documentary, not pinned: no single-guard mutation produces "run
	// succeeded, warned, and still deleted the outside tree", since
	// removeWorkspaceFiles refuses that removal independently of this
	// package's scan-side rooting.
	// TestDryRunReportsNoCandidatesFromEscapingWorkspace carries the pinned
	// form of this property.
	if _, statErr := os.Stat(outsideManifestPath); statErr != nil {
		t.Fatalf("expected the outside manifest to survive, stat error: %v", statErr)
	}
	// Positive control on the identical run: proves the fixture is capable
	// of a real removal, so the nil error above means the run actually did
	// its job rather than merely failing to crash.
	manifestPath := filepath.Join(installDir, "MANIFEST.json")
	if _, statErr := os.Stat(manifestPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected the healthy project's installed collection to be removed, stat error: %v", statErr)
	}
}

// TestStartScansWorkspaceBehindRelativeSymlink is the positive control for
// TestStartSkipsProjectWhoseWorkspaceEscapes, with the unsafe element
// replaced by a safe one: ansible_collections is a relative, in-root symlink
// to a real sibling directory. This must be scanned and cleaned up normally,
// which is what stops a future implementer from "hardening" the probe in
// openProjectWorkspace into a blanket rejection of any symlinked
// ansible_collections, safe or not.
func TestStartScansWorkspaceBehindRelativeSymlink(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	collectionsPath := t.TempDir()

	installDir := filepath.Join(collectionsPath, "real_ac", "ns", "name")
	if err := os.MkdirAll(installDir, helpers.DirMod); err != nil {
		t.Fatalf("failed to create real_ac install dir: %v", err)
	}
	manifestPath := filepath.Join(installDir, "MANIFEST.json")
	if err := os.WriteFile(manifestPath, []byte(testManifestJSON), helpers.FileMod); err != nil {
		t.Fatalf("failed to write MANIFEST.json: %v", err)
	}

	acLink := filepath.Join(collectionsPath, "ansible_collections")
	if err := os.Symlink("real_ac", acLink); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}

	registerCleanupProject(t, cacheDir, collectionsPath)

	printer := &recordingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)
	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed scanning through a relative in-root symlink, got %v", err)
	}
	if printer.hasWarningContaining("skipping project") {
		t.Fatalf("expected no skip warning for a relative in-root symlink, got: %v", printer.warnings)
	}
	if _, statErr := os.Stat(manifestPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected the collection behind the relative symlink to be removed, stat error: %v", statErr)
	}
}

// TestStartScansThroughSymlinkedCollectionsPath proves the drop-in
// compatibility case: the registered CollectionsPath itself - not a
// component beneath it - is a symlink to a real directory holding a normal
// ansible_collections tree. This is a different hop than
// TestStartScansWorkspaceBehindRelativeSymlink exercises: that test covers a
// component below the established root, this one covers root establishment
// itself, and each breaks under a different wrong implementation - this one
// specifically breaks if a future implementer moves the root down to
// ansible_collections instead of the collections path.
func TestStartScansThroughSymlinkedCollectionsPath(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	realCollectionsDir := t.TempDir()
	installDir := seedInstallTree(t, realCollectionsDir)

	collectionsPathLink := filepath.Join(t.TempDir(), "collections-link")
	if err := os.Symlink(realCollectionsDir, collectionsPathLink); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}

	registerCleanupProject(t, cacheDir, collectionsPathLink)

	printer := &recordingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)
	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed through a symlinked collections path, got %v", err)
	}
	if printer.hasWarningContaining("skipping project") {
		t.Fatalf("expected no skip warning for a symlinked collections path, got: %v", printer.warnings)
	}
	manifestPath := filepath.Join(installDir, "MANIFEST.json")
	if _, statErr := os.Stat(manifestPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected the collection behind the symlinked collections path to be removed, stat error: %v", statErr)
	}
}

// TestDryRunReportsNoCandidatesFromEscapingWorkspace proves a dry run against
// TestStartSkipsProjectWhoseWorkspaceEscapes's escaping fixture, alone,
// reports the skip warning but never previews a removal of the collection
// living outside the collections tree.
func TestDryRunReportsNoCandidatesFromEscapingWorkspace(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()

	reqPath := filepath.Join(t.TempDir(), "requirements.yml")
	if err := os.WriteFile(reqPath, []byte("collections: []\n"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write requirements file: %v", err)
	}
	escapingProject, outsideManifestPath := seedEscapingWorkspaceFixture(t, reqPath)

	writeProjectRegistry(t, cacheDir, &store.ProjectRegistry{
		Projects: map[string]store.ProjectRecord{
			"escaping-project": escapingProject,
		},
	})

	printer := &recordingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)
	cfg := &config.Config{CacheDir: cacheDir, DryRun: true}

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected dry-run Start to finish despite the escaping workspace, got %v", err)
	}
	// Confirmed killing mutation: this test run against the pre-change
	// implementation of this package printed "would remove
	// outside.coll@1.0.0" and Start still exited with a nil error - a
	// preview of a deletion the real run could never actually perform, since
	// removeWorkspaceFiles refuses it independently.
	if printer.hasPrintContaining("would remove outside.coll@1.0.0") {
		t.Fatalf("expected no dry-run candidate for a collection outside the collections tree, got prints: %v", printer.prints)
	}
	// Confirmed killing mutation (deleting scanProjectWorkspace's out.Warnf
	// call): "expected a warning naming the skipped project, got: [no
	// persisted snapshot was found; skipping the extracted-cache sweep and
	// leaving the snapshot untouched this run]" - no other warning names the
	// skipped project.
	if !printer.hasWarningContaining(`skipping project "escaping-project"`) {
		t.Fatalf("expected a warning naming the skipped project, got: %v", printer.warnings)
	}
	if _, statErr := os.Stat(outsideManifestPath); statErr != nil {
		t.Fatalf("expected the outside manifest to survive, stat error: %v", statErr)
	}
}

// TestScanAbortsOnUnreadableWorkspaceRoot proves the boundary between the two
// new classifications openProjectWorkspace/scanProjectWorkspace draw: a
// static symlink escape is skipped with a warning (see
// TestStartSkipsProjectWhoseWorkspaceEscapes), but a genuine IO error reading
// an otherwise-valid ansible_collections directory still aborts the whole
// run, wrapped with the collections path that raw root-relative error
// otherwise carries no indication of. Skipped when running as root, since
// root bypasses the permission bits this test relies on to force the read
// failure - matching TestScanAbortsOnRealIOErrorReadingManifest's own guard.
func TestScanAbortsOnUnreadableWorkspaceRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission-based read guard cannot be tested")
	}
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()
	installDir := seedInstallTree(t, downloadPath)
	acDir := filepath.Join(downloadPath, "ansible_collections")

	if err := os.Chmod(acDir, 0o000); err != nil {
		t.Fatalf("failed to chmod ansible_collections unreadable: %v", err)
	}
	restored := false
	restore := func() {
		if restored {
			return
		}
		if err := os.Chmod(acDir, helpers.DirMod); err != nil {
			t.Errorf("failed to restore ansible_collections perms: %v", err)
		}
		restored = true
	}
	t.Cleanup(restore)

	registerCleanupProject(t, cacheDir, downloadPath)
	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	err := Start(t.Context(), cfg, runtime)
	if err == nil {
		t.Fatalf("expected Start to abort on an unreadable ansible_collections root, got nil")
	}
	if !strings.Contains(err.Error(), downloadPath) {
		t.Fatalf("expected the error to name the collections path %s, got %v", downloadPath, err)
	}
	// The abort happens in buildReachable, before removeUnused ever runs, so
	// nothing could have been deleted by this aborted run. The survival
	// check below runs only after restore(), not right here: a mode-0
	// ansible_collections blocks traversal to the manifest for this test's
	// own os.Stat call, exactly as it did for the scan that just aborted.
	restore()
	assertManifestSurvives(t, installDir)

	// Positive control: the identical fixture with ansible_collections
	// readable again must complete and remove the unreferenced install,
	// proving the abort above pins the permission failure specifically and
	// not some other property of this fixture.
	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed once ansible_collections is readable again, got %v", err)
	}
	manifestPath := filepath.Join(installDir, "MANIFEST.json")
	if _, statErr := os.Stat(manifestPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected the unreferenced install to be removed once the read succeeds, stat error: %v", statErr)
	}
}

// buildNamespaceSymlinkFixture creates a collections path with an
// ns.name@1.0.0 MANIFEST.json reachable either through a real
// ansible_collections/ns directory (safe=true) or through a relative,
// in-root symlink from ansible_collections/ns to a sibling hidden_ns
// directory that lives inside collectionsPath but outside
// ansible_collections (safe=false) - a target os.Root would follow if the
// scan ever asked it to. It registers the project against cacheDir with an
// empty requirements file (nothing reachable) and returns the manifest's
// real on-disk path.
func buildNamespaceSymlinkFixture(t *testing.T, cacheDir string, safe bool) string {
	t.Helper()
	collectionsPath := t.TempDir()
	hiddenNsDir := filepath.Join(collectionsPath, "hidden_ns")
	hiddenInstallDir := filepath.Join(hiddenNsDir, "name")
	if err := os.MkdirAll(hiddenInstallDir, helpers.DirMod); err != nil {
		t.Fatalf("failed to create hidden namespace install dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(hiddenInstallDir, "MANIFEST.json"), []byte(testManifestJSON), helpers.FileMod); err != nil {
		t.Fatalf("failed to write MANIFEST.json: %v", err)
	}

	acDir := filepath.Join(collectionsPath, "ansible_collections")
	if err := os.MkdirAll(acDir, helpers.DirMod); err != nil {
		t.Fatalf("failed to create ansible_collections dir: %v", err)
	}
	nsPath := filepath.Join(acDir, "ns")

	var manifestPath string
	if safe {
		if err := os.Rename(hiddenNsDir, nsPath); err != nil {
			t.Fatalf("failed to move the namespace dir into place: %v", err)
		}
		manifestPath = filepath.Join(nsPath, "name", "MANIFEST.json")
	} else {
		if err := os.Symlink("../hidden_ns", nsPath); err != nil {
			t.Skipf("symlinks unavailable on this platform: %v", err)
		}
		manifestPath = filepath.Join(hiddenNsDir, "name", "MANIFEST.json")
	}

	registerCleanupProject(t, cacheDir, collectionsPath)
	return manifestPath
}

// TestScanSkipsSymlinkedNamespaceEntry pins the one containment property in
// this package's rooted scan that the rooting itself does not provide: a
// symlinked namespace entry is never indexed because fs.ReadDir's
// DirEntry.IsDir() reports false for a symlink regardless of its target, and
// scanInstalledCollections already skips any non-directory entry - true
// before and after this package's rooting change, not something the rooting
// itself adds. The safe case is the positive control on the identical
// fixture shape: a real namespace directory in the same place is indexed and
// removed normally.
func TestScanSkipsSymlinkedNamespaceEntry(t *testing.T) {
	t.Parallel()

	t.Run("symlinked namespace entry is not indexed", func(t *testing.T) {
		t.Parallel()
		cacheDir := t.TempDir()
		manifestPath := buildNamespaceSymlinkFixture(t, cacheDir, false)

		cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
		runtime := newTestRuntime()

		if err := Start(t.Context(), cfg, runtime); err != nil {
			t.Fatalf("expected Start to succeed, got %v", err)
		}
		if _, statErr := os.Stat(manifestPath); statErr != nil {
			t.Fatalf("expected the manifest behind the symlinked namespace entry to survive unindexed, stat error: %v", statErr)
		}
	})

	t.Run("real namespace directory is indexed and removed", func(t *testing.T) {
		t.Parallel()
		cacheDir := t.TempDir()
		manifestPath := buildNamespaceSymlinkFixture(t, cacheDir, true)

		cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
		runtime := newTestRuntime()

		if err := Start(t.Context(), cfg, runtime); err != nil {
			t.Fatalf("expected Start to succeed, got %v", err)
		}
		if _, statErr := os.Stat(manifestPath); !os.IsNotExist(statErr) {
			t.Fatalf("expected the manifest behind a real namespace directory to be removed, stat error: %v", statErr)
		}
	})
}

// buildNameSymlinkFixture creates a collections path with an ns.name@1.0.0
// MANIFEST.json reachable either through a real ansible_collections/ns/name
// directory (safe=true) or through a relative, in-root symlink from
// ansible_collections/ns/name to a sibling hidden_name directory that lives
// inside collectionsPath but outside ansible_collections (safe=false) - a
// target os.Root would follow on a read. Unlike buildNamespaceSymlinkFixture,
// the symlink here sits at the leaf of the walk (the name entry itself), not
// at an intermediate component: ansible_collections/ns is always a real
// directory in both cases. It registers the project against cacheDir with an
// empty requirements file (nothing reachable) and returns the manifest's
// real on-disk path plus the ansible_collections/ns/name entry's own path (a
// symlink in the unsafe case, a real directory in the safe case).
func buildNameSymlinkFixture(t *testing.T, cacheDir string, safe bool) (string, string) {
	t.Helper()
	collectionsPath := t.TempDir()
	hiddenNameDir := filepath.Join(collectionsPath, "hidden_name")
	if err := os.MkdirAll(hiddenNameDir, helpers.DirMod); err != nil {
		t.Fatalf("failed to create hidden name install dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(hiddenNameDir, "MANIFEST.json"), []byte(testManifestJSON), helpers.FileMod); err != nil {
		t.Fatalf("failed to write MANIFEST.json: %v", err)
	}

	nsDir := filepath.Join(collectionsPath, "ansible_collections", "ns")
	if err := os.MkdirAll(nsDir, helpers.DirMod); err != nil {
		t.Fatalf("failed to create ansible_collections/ns dir: %v", err)
	}
	namePath := filepath.Join(nsDir, "name")

	var manifestPath string
	if safe {
		if err := os.Rename(hiddenNameDir, namePath); err != nil {
			t.Fatalf("failed to move the name dir into place: %v", err)
		}
		manifestPath = filepath.Join(namePath, "MANIFEST.json")
	} else {
		if err := os.Symlink("../../hidden_name", namePath); err != nil {
			t.Skipf("symlinks unavailable on this platform: %v", err)
		}
		manifestPath = filepath.Join(hiddenNameDir, "MANIFEST.json")
	}

	registerCleanupProject(t, cacheDir, collectionsPath)
	return manifestPath, namePath
}

// TestScanSkipsSymlinkedNameEntry is the name-level sibling of
// TestScanSkipsSymlinkedNamespaceEntry: an ansible_collections/<ns>/<name>
// entry that is not a real directory is not an installed collection, so it
// is neither indexed nor reported as removed.
//
// This is a correctness property, not a containment one - nothing escapes
// either way, unlike the namespace-level guard. Disabling
// scanNamespaceDir's own "if !nameEntry.IsDir() { continue }" guard removes
// the symlink entry itself and reports the collection removed, while the
// directory it points at is left untouched. The reason the two levels
// differ: at the namespace level the symlink is an intermediate component of
// removeInstalled's rooted RemoveAll("ansible_collections/<ns>/<name>"), so
// os.Root resolves through it and destroys content inside the target - which
// is why TestScanSkipsSymlinkedNamespaceEntry's guard is a containment
// guard. At the name level, exercised here, that same symlink is the leaf of
// that same RemoveAll, and RemoveAll unlinks a symlink rather than following
// it. The safe case is the positive control on the identical fixture shape:
// a real name directory in the same place is indexed and removed normally.
func TestScanSkipsSymlinkedNameEntry(t *testing.T) {
	t.Parallel()

	t.Run("symlinked name entry is not indexed", func(t *testing.T) {
		t.Parallel()
		cacheDir := t.TempDir()
		manifestPath, entryPath := buildNameSymlinkFixture(t, cacheDir, false)

		printer := &recordingPrinter{}
		runtime := infra.New(printer, http.DefaultClient)
		cfg := &config.Config{CacheDir: cacheDir, DryRun: false}

		if err := Start(t.Context(), cfg, runtime); err != nil {
			t.Fatalf("expected Start to succeed, got %v", err)
		}
		// Confirmed killing mutation (deleting scanNamespaceDir's
		// "if !nameEntry.IsDir() { continue }" guard): Start prints
		// "removed ns.name@1.0.0" and the symlink entry itself is unlinked
		// ("lstat .../ansible_collections/ns/name: no such file or
		// directory"), even though nothing behind it is ever touched. This
		// is the pinned assertion: newTestRuntime's no-op printer cannot see
		// it, which is why this subtest builds its own recordingPrinter.
		if printer.hasPrintContaining("removed ns.name@1.0.0") {
			t.Fatalf("expected the symlinked name entry not to be reported as removed, got prints: %v", printer.prints)
		}
		// Documentary, not pinned: the hasPrintContaining check above already
		// fails first under the identical mutation, so this assertion can
		// never be the first failing line - it observes that same mutation
		// directly, on the entry itself rather than on the printed line
		// describing it.
		if _, statErr := os.Lstat(entryPath); statErr != nil {
			t.Fatalf("expected the symlink entry itself to survive, lstat error: %v", statErr)
		}
		// Documentary, not pinned: the target manifest survives under the
		// killing mutation too, since that entry is the leaf of
		// removeInstalled's rooted RemoveAll, and RemoveAll unlinks a
		// symlink rather than following it into the directory it points at.
		if _, statErr := os.Stat(manifestPath); statErr != nil {
			t.Fatalf("expected the manifest behind the symlinked name entry to survive, stat error: %v", statErr)
		}
	})

	t.Run("real name directory is indexed and removed", func(t *testing.T) {
		t.Parallel()
		cacheDir := t.TempDir()
		manifestPath, _ := buildNameSymlinkFixture(t, cacheDir, true)

		cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
		runtime := newTestRuntime()

		if err := Start(t.Context(), cfg, runtime); err != nil {
			t.Fatalf("expected Start to succeed, got %v", err)
		}
		if _, statErr := os.Stat(manifestPath); !os.IsNotExist(statErr) {
			t.Fatalf("expected the manifest behind a real name directory to be removed, stat error: %v", statErr)
		}
	})
}

// TestScanSkipsNonRegularManifest is the regression test for the "one bad
// project kills every project" defect at the manifest leaf: a MANIFEST.json
// entry that is not a regular file - a symlink of any shape, or a directory
// - must be warned about and skipped rather than aborting the whole run, the
// same way a corrupt manifest already is. Each case registers a hostile
// project (whose ansible_collections/ns/name/MANIFEST.json is built into the
// shape under test) alongside a healthy project holding one unreferenced,
// otherwise-ordinary ns.name@1.0.0 install, both sharing one empty
// requirements file, so every subtest proves the whole property - the run
// finishes AND the healthy project's cleanup actually happens - not merely
// that no error was returned.
//
// The three symlink shapes are included specifically because Lstat, not the
// symlink's target, is what manifestIsRegularFile classifies on: an absolute
// target outside the root, an absolute target that geometrically resolves
// back inside the root, and a relative in-root target all report the same
// ModeSymlink regardless of where they point, so all three converge on the
// identical warn-and-skip outcome the directory shape gets too. The relative
// in-root case is the one shape where manifestIsRegularFile's own predicate,
// not os.Root's separate escape refusal, is what does the rejecting: an
// absolute symlink target, inside or outside the root, is refused by os.Root
// itself regardless of this gate, since os.Root never consults the absolute
// filesystem namespace, but a relative in-root target is exactly what
// os.Root is documented to follow instead - the same behavior
// TestStartScansWorkspaceBehindRelativeSymlink and
// TestScanSkipsSymlinkedNameEntry rely on for a legitimate symlink elsewhere
// in this walk. Without this gate's own mode check, that one shape would be
// silently followed and read as the manifest rather than rejected - the
// deliberate cost of making the rule uniform ("a regular file, or nothing,
// with a warning") instead of depending on whether os.Root happens to follow
// a given symlink.
//
// The named-pipe case pins the one shape where the gate is load-bearing for
// more than correctness: Lstat reports a fifo's mode instantly, but a blind
// ReadFile on the same path blocks in the open() syscall until a writer
// appears - a writer this test never provides. Every case, this one
// included, runs Start under a hard wall-clock bound rather than the plain
// call every other subtest in this package uses, specifically so that a
// regression reintroducing an unguarded read fails this test instead of
// hanging the whole package until the test binary's own panic timeout.
//
// The positive control - a sixth case with a genuinely regular MANIFEST.json
// at the identical path - proves the gate does not indiscriminately reject
// every leaf: it is itself indexed and removed as an ordinary unreferenced
// collection, exactly like the healthy project's.
func TestScanSkipsNonRegularManifest(t *testing.T) {
	t.Parallel()

	cases := []struct {
		// build creates the hostile project's ansible_collections/ns/name/
		// MANIFEST.json entry under nameDir (already created as a real
		// directory by the caller) in the shape under test, and returns the
		// manifest entry's own on-disk path plus - for a symlink shape only
		// - its target file's path, so the caller can assert the target
		// survives untouched. hostileCollectionsPath is the project's
		// registered CollectionsPath, needed by the in-root shapes to place
		// their target under it.
		build func(t *testing.T, hostileCollectionsPath, nameDir string) (manifestPath, targetPath string)
		name  string
		// wantIndexed marks the positive control: a regular MANIFEST.json is
		// expected to be indexed and removed rather than warned about.
		wantIndexed bool
	}{
		{name: "absolute symlink targeting outside the root", build: buildManifestAbsoluteSymlinkOutsideRoot},
		{name: "absolute symlink resolving back inside the root", build: buildManifestAbsoluteSymlinkInsideRoot},
		{name: "relative in-root symlink", build: buildManifestRelativeInRootSymlink},
		// The directory shape is deliberately not skipped when symlinks are
		// unavailable: it needs no symlink support at all, and is the one
		// that must run on every platform.
		{name: "directory named MANIFEST.json", build: buildManifestAsDirectory},
		{name: "named pipe", build: buildManifestAsNamedPipe},
		{name: "positive control: regular file", build: buildManifestAsRegularFile, wantIndexed: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runNonRegularManifestCase(t, tc.build, tc.wantIndexed)
		})
	}
}

// buildManifestAbsoluteSymlinkOutsideRoot makes ansible_collections/ns/name/
// MANIFEST.json an absolute symlink to a real manifest file entirely outside
// the hostile project's own collections path.
func buildManifestAbsoluteSymlinkOutsideRoot(t *testing.T, _, nameDir string) (string, string) {
	t.Helper()
	outsideDir := t.TempDir()
	target := filepath.Join(outsideDir, "MANIFEST.json")
	if err := os.WriteFile(target, []byte(testManifestJSON), helpers.FileMod); err != nil {
		t.Fatalf("failed to write outside target manifest: %v", err)
	}
	manifestPath := filepath.Join(nameDir, "MANIFEST.json")
	if err := os.Symlink(target, manifestPath); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}
	return manifestPath, target
}

// buildManifestAbsoluteSymlinkInsideRoot makes
// ansible_collections/ns/name/MANIFEST.json an absolute symlink whose target
// is an absolute path string even though it geometrically resolves back
// inside hostileCollectionsPath: os.Root never consults the absolute
// filesystem namespace, so this is refused exactly like the outside-root
// case - but that refusal is not what manifestIsRegularFile's gate relies
// on, since Lstat never resolves the target at all.
func buildManifestAbsoluteSymlinkInsideRoot(t *testing.T, hostileCollectionsPath, nameDir string) (string, string) {
	t.Helper()
	targetDir := filepath.Join(hostileCollectionsPath, "geometric-target")
	if err := os.MkdirAll(targetDir, helpers.DirMod); err != nil {
		t.Fatalf("failed to create geometric target dir: %v", err)
	}
	target := filepath.Join(targetDir, "MANIFEST.json")
	if err := os.WriteFile(target, []byte(testManifestJSON), helpers.FileMod); err != nil {
		t.Fatalf("failed to write geometrically-inside target manifest: %v", err)
	}
	manifestPath := filepath.Join(nameDir, "MANIFEST.json")
	if err := os.Symlink(target, manifestPath); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}
	return manifestPath, target
}

// buildManifestRelativeInRootSymlink makes
// ansible_collections/ns/name/MANIFEST.json a relative, in-root symlink -
// the one shape os.Root follows on an unguarded read, and so the one shape
// whose behavior this gate actually changes.
func buildManifestRelativeInRootSymlink(t *testing.T, hostileCollectionsPath, nameDir string) (string, string) {
	t.Helper()
	targetDir := filepath.Join(hostileCollectionsPath, "relative-target")
	if err := os.MkdirAll(targetDir, helpers.DirMod); err != nil {
		t.Fatalf("failed to create relative target dir: %v", err)
	}
	target := filepath.Join(targetDir, "MANIFEST.json")
	if err := os.WriteFile(target, []byte(testManifestJSON), helpers.FileMod); err != nil {
		t.Fatalf("failed to write relative target manifest: %v", err)
	}
	manifestPath := filepath.Join(nameDir, "MANIFEST.json")
	rel, err := filepath.Rel(nameDir, target)
	if err != nil {
		t.Fatalf("failed to compute relative target: %v", err)
	}
	if err := os.Symlink(rel, manifestPath); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}
	return manifestPath, target
}

// buildManifestAsDirectory makes ansible_collections/ns/name/MANIFEST.json a
// directory instead of a file - the shape that needs no symlink support at
// all.
func buildManifestAsDirectory(t *testing.T, _, nameDir string) (string, string) {
	t.Helper()
	manifestPath := filepath.Join(nameDir, "MANIFEST.json")
	if err := os.MkdirAll(manifestPath, helpers.DirMod); err != nil {
		t.Fatalf("failed to create directory-shaped manifest: %v", err)
	}
	return manifestPath, ""
}

// buildManifestAsNamedPipe makes ansible_collections/ns/name/MANIFEST.json a
// named pipe (fifo) instead of a file - the shape that makes an unguarded
// ReadFile block in open() until a writer appears, a writer this test never
// provides. Lstat reports a fifo's mode instantly regardless, which is why
// manifestIsRegularFile's gate must never fall through to opening a leaf it
// has not already classified as regular.
func buildManifestAsNamedPipe(t *testing.T, _, nameDir string) (string, string) {
	t.Helper()
	manifestPath := filepath.Join(nameDir, "MANIFEST.json")
	if err := syscall.Mkfifo(manifestPath, 0o644); err != nil {
		t.Skipf("named pipes unavailable on this platform: %v", err)
	}
	return manifestPath, ""
}

// buildManifestAsRegularFile writes a genuine, valid MANIFEST.json at
// ansible_collections/ns/name/MANIFEST.json - the positive control every
// non-regular shape above is contrasted against.
func buildManifestAsRegularFile(t *testing.T, _, nameDir string) (string, string) {
	t.Helper()
	manifestPath := filepath.Join(nameDir, "MANIFEST.json")
	if err := os.WriteFile(manifestPath, []byte(testManifestJSON), helpers.FileMod); err != nil {
		t.Fatalf("failed to write regular manifest: %v", err)
	}
	return manifestPath, ""
}

// nonRegularManifestStartBound bounds each TestScanSkipsNonRegularManifest
// case's call to Start via runStartBounded. Every case actually returns in
// well under a second, so this is generous headroom, not a tight budget: its
// only job is to turn a regression that lets an unguarded read reach the
// named-pipe case's blocking open() into a fast, named test failure instead
// of a whole-package hang until the test binary's own panic timeout.
const nonRegularManifestStartBound = 10 * time.Second

// runStartBounded runs Start against cfg/runtime on its own goroutine and
// waits for it under nonRegularManifestStartBound, rather than calling it
// inline the way every other test in this package does. That bound is
// specifically for the named-pipe case: manifestIsRegularFile's Lstat gate
// is the only thing standing between this call and root.ReadFile's blocking
// open() on a fifo with no writer, so if a regression ever removes that
// gate, this call must fail the test rather than hang it - a plain,
// unbounded Start call would hang the whole package until the test binary's
// own panic timeout instead. errCh is buffered so the goroutine's send never
// blocks when Start returns normally, and a goroutine still blocked in
// open() when the bound fires is abandoned rather than waited on; it dies
// with the test binary at the end of the run, since nothing else waits on
// it.
func runStartBounded(t *testing.T, cfg *config.Config, runtime *infra.Infra) {
	t.Helper()
	errCh := make(chan error, 1)
	go func() {
		errCh <- Start(t.Context(), cfg, runtime)
	}()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("expected Start to succeed, got %v", err)
		}
	case <-time.After(nonRegularManifestStartBound):
		t.Fatalf(
			"Start did not return within %s: the manifest gate did not reject this shape, so the read blocked on open",
			nonRegularManifestStartBound,
		)
	}
}

// requirementsFifoStartBound bounds startBoundedErr's call to Start in
// TestStartAbortsOnFifoRequirementsFile and its positive control, the same
// way nonRegularManifestStartBound bounds runStartBounded above: the only
// job of this bound is to turn a regression that lets an unguarded
// requirements-file read reach a named pipe's blocking open() into a fast,
// named test failure instead of a whole-package hang.
const requirementsFifoStartBound = 10 * time.Second

// startBoundedErr runs Start against cfg/runtime on its own goroutine and
// returns its error once Start completes within requirementsFifoStartBound,
// failing the test instead if the bound elapses first. It is
// runStartBounded's sibling for a caller that needs to inspect Start's
// returned error rather than only assert success, so runStartBounded's own
// existing call sites do not need to change shape. A goroutine still blocked
// in open() when the bound fires is abandoned rather than waited on, exactly
// as runStartBounded's own doc comment describes.
func startBoundedErr(t *testing.T, cfg *config.Config, runtime *infra.Infra) error {
	t.Helper()
	errCh := make(chan error, 1)
	go func() {
		errCh <- Start(t.Context(), cfg, runtime)
	}()
	select {
	case err := <-errCh:
		return err
	case <-time.After(requirementsFifoStartBound):
		t.Fatalf(
			"Start did not return within %s: the requirements-file gate did not reject this shape, so the read blocked on open",
			requirementsFifoStartBound,
		)
		return nil // unreachable: t.Fatalf stops this goroutine before returning.
	}
}

// runNonRegularManifestCase builds a hostile project (its
// ansible_collections/ns/name/MANIFEST.json shaped by build) alongside a
// healthy project holding one unreferenced, otherwise-ordinary
// ns.name@1.0.0 install, both sharing one empty requirements file, then
// runs Start and asserts the shared property every TestScanSkipsNonRegularManifest
// case needs: the run finishes, and the healthy project's cleanup actually
// happens despite whatever the hostile project's manifest leaf looks like.
// wantIndexed selects the positive-control assertions (the hostile
// project's own manifest is indexed and removed, no warning) instead of the
// non-regular-shape ones (a warning names the hostile manifest, and its
// symlink target - where the shape has one - survives untouched).
func runNonRegularManifestCase(
	t *testing.T,
	build func(t *testing.T, hostileCollectionsPath, nameDir string) (manifestPath, targetPath string),
	wantIndexed bool,
) {
	t.Helper()
	cacheDir := t.TempDir()

	hostileCollectionsPath := t.TempDir()
	nameDir := filepath.Join(hostileCollectionsPath, "ansible_collections", "ns", "name")
	if err := os.MkdirAll(nameDir, helpers.DirMod); err != nil {
		t.Fatalf("failed to create hostile name dir: %v", err)
	}
	manifestPath, targetPath := build(t, hostileCollectionsPath, nameDir)

	healthyDownloadPath := t.TempDir()
	healthyInstallDir := seedInstallTree(t, healthyDownloadPath)

	reqPath := filepath.Join(t.TempDir(), "requirements.yml")
	if err := os.WriteFile(reqPath, []byte("collections: []\n"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write shared requirements file: %v", err)
	}
	writeProjectRegistry(t, cacheDir, &store.ProjectRegistry{
		Projects: map[string]store.ProjectRecord{
			"hostile-project": {
				RequirementsFile: reqPath,
				CollectionsPath:  hostileCollectionsPath,
				LastRun:          time.Now().UTC(),
			},
			"healthy-project": {
				RequirementsFile: reqPath,
				CollectionsPath:  healthyDownloadPath,
				LastRun:          time.Now().UTC(),
			},
		},
	})

	printer := &recordingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)
	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}

	runStartBounded(t, cfg, runtime)

	// This is what proves the run actually did its job rather than merely
	// not crashing: the healthy project's own unreferenced collection must
	// still be removed despite the hostile project's manifest leaf.
	healthyManifestPath := filepath.Join(healthyInstallDir, "MANIFEST.json")
	if _, statErr := os.Stat(healthyManifestPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected the healthy project's collection to be removed, stat error: %v", statErr)
	}

	if wantIndexed {
		assertHostileManifestIndexed(t, printer, manifestPath)
		return
	}
	assertHostileManifestWarnedAndSkipped(t, printer, manifestPath, targetPath)
}

// assertHostileManifestIndexed asserts the positive-control outcome: no
// non-regular-manifest warning was recorded, and the hostile project's own
// regular manifest was indexed and removed like any other unreferenced
// collection.
func assertHostileManifestIndexed(t *testing.T, printer *recordingPrinter, manifestPath string) {
	t.Helper()
	if printer.hasWarningContaining("skipping non-regular manifest") {
		t.Fatalf("expected no non-regular-manifest warning for a regular manifest, got: %v", printer.warnings)
	}
	if _, statErr := os.Stat(manifestPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected the hostile project's own regular manifest to be indexed and removed, stat error: %v", statErr)
	}
}

// assertHostileManifestWarnedAndSkipped asserts the non-regular-shape
// outcome: a warning names the hostile manifest path, and - documentary,
// not pinned, since the gate never opens the entry at all - a symlink
// shape's target (targetPath == "" for a shape with none) survives
// untouched. Nothing in this run could make the target disappear regardless
// of whether the gate is even present.
func assertHostileManifestWarnedAndSkipped(t *testing.T, printer *recordingPrinter, manifestPath, targetPath string) {
	t.Helper()
	if !printer.hasWarningContaining(manifestPath) {
		t.Fatalf("expected a warning naming the hostile manifest path, got: %v", printer.warnings)
	}
	if targetPath == "" {
		return
	}
	if _, statErr := os.Stat(targetPath); statErr != nil {
		t.Fatalf("expected the symlink target to survive, stat error: %v", statErr)
	}
}

// TestSweepExtractedStoreNoopWhenCacheDirEmpty proves sweepExtractedStore's
// empty-CacheDir guard returns immediately without panicking or touching
// anything, called directly with an empty cfg.CacheDir.
func TestSweepExtractedStoreNoopWhenCacheDirEmpty(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{CacheDir: "", DryRun: false}
	runtime := newTestRuntime()
	st := store.New()

	sweepExtractedStore(cfg, runtime, st, map[string]bool{}, map[string][]installedCollection{})
}

// TestStartLeavesExtractedCacheWhenNoSnapshotPersisted is THE regression test
// for the WasPersisted guard: an absent workspace plus a project registered
// for GC plus NO persisted snapshot at all (a fresh cache dir - no
// seedSnapshotInstalled, no seedSnapshotWarmed) produces an empty keep set
// that is indistinguishable from "nothing is installed or warmed anywhere".
// Without the WasPersisted guard, sweepExtractedStore would wipe the entire
// extracted store on the strength of having no evidence at all; the guard
// instead skips the sweep, so the extracted dir must survive.
func TestStartLeavesExtractedCacheWhenNoSnapshotPersisted(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	seedExtractedDir(t, cacheDir, "sha-orphaned-by-no-snapshot")
	recordAbsentWorkspaceProject(t, cfg, runtime, downloadPath)

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}

	assertExtractedDirsSurvive(t, cacheDir, "sha-orphaned-by-no-snapshot")
}

// TestStartDoesNotFabricatePersistedSnapshot proves the write-side guard:
// when Start runs with no persisted snapshot to begin with, it must not call
// SaveStore at all, since doing so would stamp Meta.LastSnapshot for the
// first time and fabricate a persisted-and-empty snapshot the next run would
// read as positive evidence that nothing is installed or warmed anywhere.
// Reloading through a fresh backend after Start must still report
// WasPersisted() == false.
func TestStartDoesNotFabricatePersistedSnapshot(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	seedExtractedDir(t, cacheDir, "sha-orphaned-by-no-snapshot")
	recordAbsentWorkspaceProject(t, cfg, runtime, downloadPath)

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}

	reloaded := reloadStoreThroughFreshBackend(t, cfg, runtime)
	if reloaded.WasPersisted() {
		t.Fatal("expected no snapshot to have been persisted by a cleanup run that never loaded one")
	}
}

// reloadStoreThroughFreshBackend opens a brand-new backend against cfg and
// loads its store, mirroring what the next real run would see on disk -
// as opposed to inspecting the *store.Store instance Start itself used,
// which would not prove anything actually reached the backend.
func reloadStoreThroughFreshBackend(t *testing.T, cfg *config.Config, runtime *infra.Infra) *store.Store {
	t.Helper()
	backend, err := cacheBackend.New(cfg, runtime)
	if err != nil {
		t.Fatalf("failed to build a fresh backend: %v", err)
	}
	if err := backend.Open(t.Context()); err != nil {
		t.Fatalf("failed to open a fresh backend: %v", err)
	}
	defer func() {
		if err := backend.Close(t.Context()); err != nil {
			t.Errorf("failed to close the fresh backend: %v", err)
		}
	}()
	st, err := backend.LoadStore(t.Context())
	if err != nil {
		t.Fatalf("failed to load store through a fresh backend: %v", err)
	}
	return st
}

// TestStartTwiceWithNoSnapshotLeavesExtractedCacheIntact proves the guard
// holds across repeated runs, not just a single one: two consecutive Start
// calls against a cache dir that never gains a persisted snapshot must both
// leave the extracted store untouched. This only passes when both halves of
// the fix are in place together - if the read-side guard alone landed
// without the write-side guard, the first run would still fabricate a
// persisted-and-empty snapshot that the second run would then read as real
// evidence and wipe the extracted dir on.
func TestStartTwiceWithNoSnapshotLeavesExtractedCacheIntact(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	seedExtractedDir(t, cacheDir, "sha-orphaned-by-no-snapshot")
	recordAbsentWorkspaceProject(t, cfg, runtime, downloadPath)

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected first Start to succeed, got %v", err)
	}
	assertExtractedDirsSurvive(t, cacheDir, "sha-orphaned-by-no-snapshot")

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected second Start to succeed, got %v", err)
	}
	assertExtractedDirsSurvive(t, cacheDir, "sha-orphaned-by-no-snapshot")
}

// TestStartSweepsExtractedCacheWhenSnapshotIsPersistedButEmpty is the
// narrowness guard on the read side: a snapshot that was actually persisted -
// even one whose Installed/Warmed maps are both empty - is real evidence
// ("nothing is referenced"), unlike an absent snapshot ("unknown"), and the
// guard must not treat the two the same. An orphaned extracted dir must
// still be swept in this case.
func TestStartSweepsExtractedCacheWhenSnapshotIsPersistedButEmpty(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	// An empty but persisted snapshot: SaveStore still stamps Meta.LastSnapshot
	// even though the Installed map given to it is empty.
	seedSnapshotInstalled(t, cfg, runtime, map[string]store.InstalledEntry{})
	seedExtractedDir(t, cacheDir, "sha-orphan-with-persisted-empty-snapshot")
	recordAbsentWorkspaceProject(t, cfg, runtime, downloadPath)

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}

	assertExtractedDirGone(t, cacheDir, "sha-orphan-with-persisted-empty-snapshot")
}

// TestStartRemovesUnreferencedInstallWithNoPersistedSnapshot is the
// narrowness guard confirming removeUnused stays unconditional: it is driven
// by the on-disk workspace scan plus each project's requirements.yml, not by
// the snapshot, so it must stay authoritative regardless of whether any
// snapshot was ever persisted. A present workspace holding an unreferenced
// collection must still have its on-disk install tree removed even with no
// snapshot seeded at all.
func TestStartRemovesUnreferencedInstallWithNoPersistedSnapshot(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()
	installDir := seedInstallTree(t, downloadPath)
	registerCleanupProject(t, cacheDir, downloadPath)

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}

	manifestPath := filepath.Join(installDir, "MANIFEST.json")
	if _, statErr := os.Stat(manifestPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected the unreferenced install to be removed even with no persisted snapshot, stat error: %v", statErr)
	}
}

// TestDryRunReportsNoExtractedSweepWithNoPersistedSnapshot proves the guard
// in sweepExtractedStore runs before the cfg.DryRun branch: a dry run against
// a cache dir with no persisted snapshot must not print any "would sweep
// extracted" line, matching what a real run would (not) do in the same
// situation.
func TestDryRunReportsNoExtractedSweepWithNoPersistedSnapshot(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()

	cfg := &config.Config{CacheDir: cacheDir, DryRun: true}
	runtime := newTestRuntime()

	seedExtractedDir(t, cacheDir, "sha-orphaned-by-no-snapshot")
	recordAbsentWorkspaceProject(t, cfg, runtime, downloadPath)

	printer := &recordingPrinter{}
	dryRunRuntime := infra.New(printer, http.DefaultClient)

	if err := Start(t.Context(), cfg, dryRunRuntime); err != nil {
		t.Fatalf("expected dry-run Start to succeed, got %v", err)
	}

	if printer.hasPrintContaining("would sweep extracted") {
		t.Fatalf("expected no would-sweep-extracted report with no persisted snapshot, got prints: %v", printer.prints)
	}
	assertExtractedDirsSurvive(t, cacheDir, "sha-orphaned-by-no-snapshot")
}

// ---------------------------------------------------------------------------
// Identity-from-walked-directory coverage: a scanned collection's namespace
// and name come from the ansible_collections/<ns>/<name> pair the scan
// walked through, never from the manifest's own declared fields. The tests
// below (prefixed TestScanIdentity.../TestRemoveInstalledArtifactAndSidecar)
// pin that invariant; the tests further down (prefixed
// TestBuildReachable.../TestStaleRegistryEntry.../TestUnparseableRequirements...)
// pin buildReachable's two-phase, sorted-project reachability computation.
// ---------------------------------------------------------------------------

// seedManifestWithIdentity writes a MANIFEST.json at
// <root>/ansible_collections/<dirNs>/<dirName> whose own JSON content
// declares collection_info.namespace=jsonNs and collection_info.name=jsonName -
// independently of the directory pair it is written under - for tests
// proving a scanned collection's identity comes from the walked directory
// rather than from this content.
func seedManifestWithIdentity(t *testing.T, root, dirNs, dirName, jsonNs, jsonName, version string) {
	t.Helper()
	installDir := filepath.Join(root, "ansible_collections", dirNs, dirName)
	if err := os.MkdirAll(installDir, helpers.DirMod); err != nil {
		t.Fatalf("failed to create install dir for %s/%s: %v", dirNs, dirName, err)
	}
	manifest := manifestJSON(jsonNs, jsonName, version, nil)
	if err := os.WriteFile(filepath.Join(installDir, "MANIFEST.json"), []byte(manifest), helpers.FileMod); err != nil {
		t.Fatalf("failed to write manifest for %s/%s: %v", dirNs, dirName, err)
	}
}

// TestScanIdentityComesFromWalkedDirectoryNotManifest proves the governing
// identity invariant end to end through Start: a hostile manifest at
// ansible_collections/evil/pkg/MANIFEST.json whose own JSON content falsely
// declares victim.collection@9.9.9 - the exact ns.name a legitimate,
// requirements-pinned victim.collection@1.0.0 install sits at, under
// ansible_collections/victim/collection - must never let that content
// redirect reachability or deletion at the legitimate collection. A
// requirements file pinning victim.collection ==1.0.0 keeps only the real
// victim.collection@1.0.0 reachable (its key includes the pin, so version
// 9.9.9 could never satisfy it even if the hostile record's identity were
// trusted); the hostile record itself is unreferenced by anything and must
// be removed from its own, real directory (evil/pkg), not from victim's.
//
// Confirmed killing mutation (reverting scanCollectionDir's call to
// buildInstalledRecord to pass manifest.CollectionInfo.Namespace/.Name
// instead of the walked ns/name, restoring the pre-fix behavior): running
// `go test ./internal/galaxy/cleanup/... -v` against that mutation produces
// exactly two top-level failures - this test and, independently,
// TestRemoveInstalledArtifactAndSidecarFollowWalkedIdentity (see that
// test's own doc comment for why). This test's own share of that run:
//
//	cleanup_test.go:3730: expected victim.collection to survive, stat error: stat .../MANIFEST.json: no such file or directory
//	--- FAIL: TestScanIdentityComesFromWalkedDirectoryNotManifest (0.00s)
//	    --- PASS: TestScanIdentityComesFromWalkedDirectoryNotManifest/positive_control:_evil.pkg_survives_its_own_requirement (0.02s)
//	    --- FAIL: TestScanIdentityComesFromWalkedDirectoryNotManifest/hostile_manifest_cannot_redirect_deletion (0.02s)
//
// matching the disaster this invariant exists to prevent: the hostile
// record's manifest-derived identity (victim.collection) let removeUnused
// target the real victim's directory for deletion. The subtest's second
// assertion (the hostile fixture's own directory being removed) never
// actually ran under this mutation - t.Fatalf halted the subtest at the
// first failing check - so only the quoted line above is a mutation-pinned
// claim; the positive-control subtest's own pass under this same mutation
// is coincidental and is not itself what this mutation is cited for. What
// actually happens there: under the mutation both records' Namespace/Name
// become manifest-derived ("victim"/"collection"), so the two distinct
// installedByKey entries this fixture produces - victim.collection@1.0.0
// (the real install) and victim.collection@9.9.9 (the hostile one) -
// resolve to the identical removal target path
// (ansible_collections/victim/collection), and neither record's FQDN is
// ever "evil.pkg" any more, so the positive control's own evil.pkg root
// matches nothing in the index and both keys stay unreachable. removeUnused
// still removes ansible_collections/victim/collection (once, then again as
// a harmless no-op for the second key), but evil/pkg's own physical
// directory - the hostile record's true, on-disk location - is never
// targeted at all, so it survives coincidentally rather than because
// "evil.pkg" was ever resolved as reachable.
func TestScanIdentityComesFromWalkedDirectoryNotManifest(t *testing.T) {
	t.Parallel()

	t.Run("hostile manifest cannot redirect deletion", func(t *testing.T) {
		t.Parallel()
		cacheDir := t.TempDir()
		downloadPath := t.TempDir()
		seedManifestAt(t, downloadPath, "victim", "collection", "1.0.0")
		seedManifestWithIdentity(t, downloadPath, "evil", "pkg", "victim", "collection", "9.9.9")

		reqPath := filepath.Join(t.TempDir(), "requirements.yml")
		reqYAML := "collections:\n  - name: victim.collection\n    version: \"==1.0.0\"\n"
		if err := os.WriteFile(reqPath, []byte(reqYAML), helpers.FileMod); err != nil {
			t.Fatalf("failed to write requirements file: %v", err)
		}
		registerCleanupProjectAt(t, cacheDir, downloadPath, reqPath)

		cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
		runtime := newTestRuntime()

		if err := Start(t.Context(), cfg, runtime); err != nil {
			t.Fatalf("expected Start to succeed, got %v", err)
		}

		if _, statErr := os.Stat(filepath.Join(downloadPath, "ansible_collections", "victim", "collection", "MANIFEST.json")); statErr != nil {
			t.Fatalf("expected victim.collection to survive, stat error: %v", statErr)
		}
		if _, statErr := os.Stat(filepath.Join(downloadPath, "ansible_collections", "evil", "pkg", "MANIFEST.json")); !os.IsNotExist(statErr) {
			t.Fatalf("expected the hostile fixture's own directory (evil/pkg) to be removed, stat error: %v", statErr)
		}
	})

	// Positive control on the identical on-disk fixture: proves the fixture
	// can be accepted, and that the scanned record's real identity really is
	// evil.pkg - not victim.collection, which its own manifest falsely
	// claims - by requiring evil.pkg directly and observing it survive.
	t.Run("positive control: evil.pkg survives its own requirement", func(t *testing.T) {
		t.Parallel()
		cacheDir := t.TempDir()
		downloadPath := t.TempDir()
		seedManifestAt(t, downloadPath, "victim", "collection", "1.0.0")
		seedManifestWithIdentity(t, downloadPath, "evil", "pkg", "victim", "collection", "9.9.9")

		reqPath := filepath.Join(t.TempDir(), "requirements.yml")
		if err := os.WriteFile(reqPath, []byte("collections:\n  - evil.pkg\n"), helpers.FileMod); err != nil {
			t.Fatalf("failed to write requirements file: %v", err)
		}
		registerCleanupProjectAt(t, cacheDir, downloadPath, reqPath)

		cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
		runtime := newTestRuntime()

		if err := Start(t.Context(), cfg, runtime); err != nil {
			t.Fatalf("expected Start to succeed, got %v", err)
		}

		assertManifestPresentAt(t, downloadPath, "evil", "pkg")
		assertManifestAbsentAt(t, downloadPath, "victim", "collection")
	})
}

// scanHostileEvilPkgRecord scans downloadPath's evil/pkg fixture (seeded by
// seedManifestWithIdentity) through the real scanCollectionDir and returns
// the resulting installedCollection record, failing the test unless exactly
// one record was produced under the walked identity evil.pkg.
func scanHostileEvilPkgRecord(t *testing.T, downloadPath string) installedCollection {
	t.Helper()
	ws := openTestWorkspace(t, downloadPath)
	defer func() { _ = ws.root.Close() }()
	index := make(map[string][]installedCollection)
	byKey := make(map[string][]installedCollection)
	deps := make(map[string]map[string]string)
	if err := scanCollectionDir(noopPrinter{}, ws, "evil", "pkg", index, byKey, deps); err != nil {
		t.Fatalf("failed to scan the hostile fixture: %v", err)
	}
	insts := byKey["evil.pkg@9.9.9"]
	if len(insts) != 1 {
		t.Fatalf("expected exactly one scanned record keyed evil.pkg@9.9.9, got %d: %+v", len(insts), insts)
	}
	inst := insts[0]
	if inst.Namespace != "evil" || inst.Name != "pkg" {
		t.Fatalf("expected the scanned record's identity to be evil/pkg (the walked directory), got ns=%q name=%q", inst.Namespace, inst.Name)
	}
	return inst
}

// seedEmptyDirs creates each of dirs as an empty directory, failing the test
// on any MkdirAll error.
func seedEmptyDirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, helpers.DirMod); err != nil {
			t.Fatalf("failed to create dir %s: %v", dir, err)
		}
	}
}

// TestRemoveInstalledArtifactAndSidecarFollowWalkedIdentity proves the
// artifact-cache purge and the .info sidecar removal both key off the same
// walked identity buildInstalledRecord assigns - never off anything the
// manifest declares. It scans the identical hostile fixture
// TestScanIdentityComesFromWalkedDirectoryNotManifest uses (evil/pkg on
// disk, claiming victim.collection@9.9.9 in its own JSON) through the real
// scanCollectionDir, so the installedCollection record under test is exactly
// what production code would build for it, then drives removeInstalled
// directly with a recordingArtifactStore so the deleted artifact key can be
// asserted precisely.
//
// This test dies independently under the identical mutation
// TestScanIdentityComesFromWalkedDirectoryNotManifest's own doc comment
// confirms (reverting scanCollectionDir's call to buildInstalledRecord to
// pass manifest.CollectionInfo.Namespace/.Name instead of the walked
// ns/name): under that mutation, scanCollectionDir("evil", "pkg", ...) keys
// the resulting record under "victim.collection@9.9.9" instead of
// "evil.pkg@9.9.9", so this test's own lookup below
// (byKey["evil.pkg@9.9.9"]) finds nothing at all rather than the one record
// it expects:
//
//	cleanup_test.go:3829: expected exactly one scanned record keyed evil.pkg@9.9.9, got 0: []
//	--- FAIL: TestRemoveInstalledArtifactAndSidecarFollowWalkedIdentity (0.00s)
func TestRemoveInstalledArtifactAndSidecarFollowWalkedIdentity(t *testing.T) {
	t.Parallel()
	downloadPath := t.TempDir()
	seedManifestWithIdentity(t, downloadPath, "evil", "pkg", "victim", "collection", "9.9.9")
	inst := scanHostileEvilPkgRecord(t, downloadPath)

	// Two sidecar directories: one under the record's real, walked identity
	// (evil.pkg-9.9.9.info) and one under the identity its own manifest
	// falsely claims (victim.collection-9.9.9.info). Only the former must be
	// removed.
	realSidecar := filepath.Join(downloadPath, "ansible_collections", "evil.pkg-9.9.9.info")
	falseSidecar := filepath.Join(downloadPath, "ansible_collections", "victim.collection-9.9.9.info")
	seedEmptyDirs(t, realSidecar, falseSidecar)

	st := store.New()
	const source = "https://galaxy.example.com/api"
	st.SetInstalled(inst.Key, store.InstalledEntry{Source: source, ArtifactSHA256: "deadbeef"})
	resolvedSource := installedSource(st, inst.Key)

	artifacts := &recordingArtifactStore{}
	if err := removeInstalled(t.Context(), inst, artifacts, resolvedSource); err != nil {
		t.Fatalf("expected removeInstalled to succeed, got %v", err)
	}

	wantKey := helpers.ArtifactKey(source, "evil-pkg-9.9.9.tar.gz")
	if len(artifacts.deleted) != 1 || artifacts.deleted[0] != wantKey {
		t.Fatalf("expected Delete to be called once with key %q, got %v", wantKey, artifacts.deleted)
	}
	if _, statErr := os.Stat(realSidecar); !os.IsNotExist(statErr) {
		t.Fatalf("expected the walked-identity sidecar to be removed, stat error: %v", statErr)
	}
	if _, statErr := os.Stat(falseSidecar); statErr != nil {
		t.Fatalf("expected the manifest-claimed sidecar to survive untouched, stat error: %v", statErr)
	}
}

// TestBuildReachablePhase2ReachesDirectCrossProjectRequirement proves
// buildReachable's two-phase split: project "proj-a" requires foo.bar
// >=1.0.0 but holds only
// an unrelated other.thing install; project "proj-b" holds the only
// foo.bar@1.0.0 install and requires nothing. "proj-a" sorts before
// "proj-b", so under a single-phase implementation that scans and resolves
// one project at a time, project A's own root would be resolved before
// project B is ever scanned - foo.bar would not yet be in the index, and
// project B's only copy would be removed as unreferenced. The two-phase
// split scans every project's workspace first, so this fixture alone
// deterministically distinguishes it from a single-phase implementation;
// no statistical loop is needed.
//
// Confirmed killing mutation (collapsing buildReachable's two phases back
// into one loop that scans and resolves each project in turn, unconditionally
// calling projectRequirementRoots right after scanProjectWorkspace for each
// project instead of over two full passes): running
// `go test ./internal/galaxy/cleanup/... -v` against that mutation produces
// exactly two top-level failures - this test and
// TestBuildReachablePhase2FollowsTransitiveDependencyEdge below:
//
//	cleanup_test.go:3931: expected foo.bar to survive via project A's
//	cross-project requirement, stat .../MANIFEST.json: no such file or directory
//	cleanup_test.go:3986: expected dep.leaf to survive via top.level's
//	transitive dependency, stat .../MANIFEST.json: no such file or directory
//	--- FAIL: TestBuildReachablePhase2ReachesDirectCrossProjectRequirement (0.01s)
//	--- FAIL: TestBuildReachablePhase2FollowsTransitiveDependencyEdge (0.01s)
//
// The same mutation run against TestBuildReachableSkippedProjectStillContributesRoots,
// TestStaleRegistryEntryToleratedWithOtherProjectCleanup,
// TestStaleRegistryEntryPositiveControlUnparseableAborts, and
// TestUnparseableRequirementsAbortsEvenForUnscannedProject left all four
// passing: this mutation removes the phase separation, not the "a skipped
// project still contributes its roots" property those four pin, which is a
// distinct fix confirmed by a separate mutation on
// TestBuildReachableSkippedProjectStillContributesRoots's own doc comment.
func TestBuildReachablePhase2ReachesDirectCrossProjectRequirement(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPathA := t.TempDir()
	downloadPathB := t.TempDir()

	seedManifestAt(t, downloadPathA, "other", "thing", "1.0.0")
	seedManifestAt(t, downloadPathB, "foo", "bar", "1.0.0")

	reqPathA := filepath.Join(t.TempDir(), "requirements-a.yml")
	reqA := "collections:\n  - name: foo.bar\n    version: \">=1.0.0\"\n"
	if err := os.WriteFile(reqPathA, []byte(reqA), helpers.FileMod); err != nil {
		t.Fatalf("failed to write requirements file for project A: %v", err)
	}
	reqPathB := filepath.Join(t.TempDir(), "requirements-b.yml")
	if err := os.WriteFile(reqPathB, []byte("collections: []\n"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write requirements file for project B: %v", err)
	}

	writeProjectRegistry(t, cacheDir, &store.ProjectRegistry{
		Projects: map[string]store.ProjectRecord{
			"proj-a": {RequirementsFile: reqPathA, CollectionsPath: downloadPathA, LastRun: time.Now().UTC()},
			"proj-b": {RequirementsFile: reqPathB, CollectionsPath: downloadPathB, LastRun: time.Now().UTC()},
		},
	})

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}

	if _, statErr := os.Stat(filepath.Join(downloadPathB, "ansible_collections", "foo", "bar", "MANIFEST.json")); statErr != nil {
		t.Fatalf("expected foo.bar to survive via project A's cross-project requirement, stat error: %v", statErr)
	}
	assertManifestAbsentAt(t, downloadPathA, "other", "thing")
}

// TestBuildReachablePhase2FollowsTransitiveDependencyEdge is
// TestBuildReachablePhase2ReachesDirectCrossProjectRequirement's sibling for
// a transitive MANIFEST dependency edge rather than a direct requirements.yml
// root: project A holds top.level, whose own manifest declares
// dep.leaf >=1.0.0; project B holds the only dep.leaf@1.0.0; only top.level
// is required. The same sorted-project-order trick applies (project A must
// sort first), and the same mutation kills both tests together - see
// TestBuildReachablePhase2ReachesDirectCrossProjectRequirement's own doc
// comment for the quoted failure output covering both.
//
// unrelated.extra is seeded under project B and asserted removed as the
// removal control: it proves removeUnused actually ran against B's own
// tree in this run, rather than dep.leaf surviving for some reason
// unrelated to reachability (e.g. B's workspace never being scanned at
// all).
func TestBuildReachablePhase2FollowsTransitiveDependencyEdge(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPathA := t.TempDir()
	downloadPathB := t.TempDir()

	seedManifestWithDeps(t, downloadPathA, "top", "level", "1.0.0", map[string]string{"dep.leaf": ">=1.0.0"})
	seedManifestAt(t, downloadPathB, "dep", "leaf", "1.0.0")
	seedManifestAt(t, downloadPathB, "unrelated", "extra", "1.0.0")

	reqPathA := filepath.Join(t.TempDir(), "requirements-a.yml")
	if err := os.WriteFile(reqPathA, []byte("collections:\n  - top.level\n"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write requirements file for project A: %v", err)
	}
	reqPathB := filepath.Join(t.TempDir(), "requirements-b.yml")
	if err := os.WriteFile(reqPathB, []byte("collections: []\n"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write requirements file for project B: %v", err)
	}

	writeProjectRegistry(t, cacheDir, &store.ProjectRegistry{
		Projects: map[string]store.ProjectRecord{
			"proj-a": {RequirementsFile: reqPathA, CollectionsPath: downloadPathA, LastRun: time.Now().UTC()},
			"proj-b": {RequirementsFile: reqPathB, CollectionsPath: downloadPathB, LastRun: time.Now().UTC()},
		},
	})

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}

	assertManifestPresentAt(t, downloadPathA, "top", "level")
	if _, statErr := os.Stat(filepath.Join(downloadPathB, "ansible_collections", "dep", "leaf", "MANIFEST.json")); statErr != nil {
		t.Fatalf("expected dep.leaf to survive via top.level's transitive dependency, stat error: %v", statErr)
	}
	assertManifestAbsentAt(t, downloadPathB, "unrelated", "extra")
}

// buildSkippedProjectRootsFixture builds the two-project fixture the tests
// below share: "aa-holder" holds the only foo.bar@1.0.0, and
// "zz-requires-only" has an absent workspace (no ansible_collections
// subdirectory at all, so scanProjectWorkspace skips it entirely without
// ever reading its own requirements file in phase 1) but records a
// requirements file requiring foo.bar when requireFooBar is true, or
// referencing nothing when it is false. The project keys are deliberately
// chosen so the skipped project sorts LAST: under the pre-fix single loop,
// foo.bar would already have been indexed (aa-holder having already been
// scanned earlier in that same loop) by the time the loop reached the
// skipped project, so ordering alone could never explain a survival here -
// only whether the skipped project's own roots are actually consulted
// despite never having been scanned.
func buildSkippedProjectRootsFixture(t *testing.T, cacheDir string, requireFooBar bool) string {
	t.Helper()
	holderPath := t.TempDir()
	seedManifestAt(t, holderPath, "foo", "bar", "1.0.0")

	holderReqPath := filepath.Join(t.TempDir(), "requirements-holder.yml")
	if err := os.WriteFile(holderReqPath, []byte("collections: []\n"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write holder requirements file: %v", err)
	}

	skippedCollectionsPath := t.TempDir()
	skippedReqPath := filepath.Join(t.TempDir(), "requirements-skipped.yml")
	skippedReqYAML := "collections: []\n"
	if requireFooBar {
		skippedReqYAML = "collections:\n  - foo.bar\n"
	}
	if err := os.WriteFile(skippedReqPath, []byte(skippedReqYAML), helpers.FileMod); err != nil {
		t.Fatalf("failed to write skipped project's requirements file: %v", err)
	}

	writeProjectRegistry(t, cacheDir, &store.ProjectRegistry{
		Projects: map[string]store.ProjectRecord{
			"aa-holder":        {RequirementsFile: holderReqPath, CollectionsPath: holderPath, LastRun: time.Now().UTC()},
			"zz-requires-only": {RequirementsFile: skippedReqPath, CollectionsPath: skippedCollectionsPath, LastRun: time.Now().UTC()},
		},
	})
	return holderPath
}

// TestBuildReachableSkippedProjectStillContributesRoots proves a project
// skipped in phase 1 (its own workspace is absent, so nothing under it is
// ever scanned) still contributes its own requirements.yml roots in phase
// 2, keeping another project's on-disk copy alive.
//
// Confirmed killing mutation (restoring the pre-fix behavior that tracks,
// per project, whether phase 1's openProjectWorkspace found a usable
// workspace, and skips phase 2's root resolution entirely - `if !scanned {
// continue }` - for a project it did not): running
// `go test ./internal/galaxy/cleanup/... -v` against that mutation produces
// exactly two top-level failures - this test and
// TestUnparseableRequirementsAbortsEvenForUnscannedProject, which pins the
// identical "phase 2 still consults a phase-1-skipped project's
// requirements" property from the opposite direction (an unparseable file
// must still abort, rather than a valid file still being consulted):
//
//	cleanup_test.go:4074: expected foo.bar to survive via the skipped
//	project's own requirement, stat .../MANIFEST.json: no such file or directory
//	--- FAIL: TestBuildReachableSkippedProjectStillContributesRoots (0.01s)
//
// Every other test in this package - including
// TestBuildReachablePhase2ReachesDirectCrossProjectRequirement,
// TestBuildReachablePhase2FollowsTransitiveDependencyEdge,
// TestStaleRegistryEntryToleratedWithOtherProjectCleanup, and
// TestStaleRegistryEntryPositiveControlUnparseableAborts - keeps passing:
// their own fixtures give every project a real, scanned workspace and so
// never reach this gate at all. TestBuildReachableSkippedProjectRootsControl
// (below) is this test's own positive control on the identical fixture
// shape.
func TestBuildReachableSkippedProjectStillContributesRoots(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	holderPath := buildSkippedProjectRootsFixture(t, cacheDir, true)

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(holderPath, "ansible_collections", "foo", "bar", "MANIFEST.json")); statErr != nil {
		t.Fatalf("expected foo.bar to survive via the skipped project's own requirement, stat error: %v", statErr)
	}
}

// TestBuildReachableSkippedProjectRootsControl is the positive control for
// TestBuildReachableSkippedProjectStillContributesRoots on the identical
// fixture shape: when the skipped project's own requirements do not name
// foo.bar, the holder's unreferenced copy is removed normally, proving the
// survival above comes from the skipped project's root specifically, not
// from some blanket "a skipped project protects everything installed
// anywhere" behavior.
func TestBuildReachableSkippedProjectRootsControl(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	holderPath := buildSkippedProjectRootsFixture(t, cacheDir, false)

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}
	assertManifestAbsentAt(t, holderPath, "foo", "bar")
}

// buildStaleRegistryFixture builds the two-project fixture the tests below
// share: a "stale-project" whose requirements file is written with
// staleContent (nil
// leaves the file entirely unwritten, i.e. genuinely absent), and a healthy
// "other-project" holding one unreferenced install of its own, so a
// survival/removal assertion on the healthy project's copy always has
// something real to check regardless of what happens to the stale one.
func buildStaleRegistryFixture(t *testing.T, cacheDir string, staleContent []byte) (string, string) {
	t.Helper()
	staleDownloadPath := t.TempDir()
	seedManifestAt(t, staleDownloadPath, "stale", "coll", "1.0.0")
	staleReqPath := filepath.Join(t.TempDir(), "stale-requirements.yml")
	if staleContent != nil {
		if err := os.WriteFile(staleReqPath, staleContent, helpers.FileMod); err != nil {
			t.Fatalf("failed to write stale project's requirements file: %v", err)
		}
	}

	otherDownloadPath := t.TempDir()
	otherInstallDir := seedInstallTree(t, otherDownloadPath)
	otherReqPath := filepath.Join(t.TempDir(), "requirements-other.yml")
	if err := os.WriteFile(otherReqPath, []byte("collections: []\n"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write other project's requirements file: %v", err)
	}

	writeProjectRegistry(t, cacheDir, &store.ProjectRegistry{
		Projects: map[string]store.ProjectRecord{
			"stale-project": {RequirementsFile: staleReqPath, CollectionsPath: staleDownloadPath, LastRun: time.Now().UTC()},
			"other-project": {RequirementsFile: otherReqPath, CollectionsPath: otherDownloadPath, LastRun: time.Now().UTC()},
		},
	})
	return staleDownloadPath, otherInstallDir
}

// TestStaleRegistryEntryToleratedWithOtherProjectCleanup proves a
// recorded requirements file that no longer exists at all is tolerated as a
// stale registry entry - a warning, not a load failure - and every other
// recorded project's cleanup still proceeds normally around it.
func TestStaleRegistryEntryToleratedWithOtherProjectCleanup(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	_, otherInstallDir := buildStaleRegistryFixture(t, cacheDir, nil)

	printer := &recordingPrinter{}
	runtime := newTestRuntimeWith(printer)
	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}
	if !printer.hasWarningContaining("no longer exists") {
		t.Fatalf("expected a warning about the missing requirements file, got: %v", printer.warnings)
	}
	manifestPath := filepath.Join(otherInstallDir, "MANIFEST.json")
	if _, statErr := os.Stat(manifestPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected the other project's unreferenced copy to still be removed, stat error: %v", statErr)
	}
}

// TestStaleRegistryEntryPositiveControlUnparseableAborts is the mandatory
// positive control for TestStaleRegistryEntryToleratedWithOtherProjectCleanup
// on the identical two-project fixture: replacing the missing file with an
// unparseable one flips the outcome from tolerated to aborted, and nothing -
// including the other, healthy project's own unreferenced copy - is
// deleted, proving the tolerant behavior above is specific to a genuinely
// absent file, not to "cleanup never touches the other project's install
// anyway".
func TestStaleRegistryEntryPositiveControlUnparseableAborts(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	staleDownloadPath, otherInstallDir := buildStaleRegistryFixture(t, cacheDir, []byte("{invalid"))

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	err := Start(t.Context(), cfg, runtime)
	if !errors.Is(err, helpers.ErrProjectRequirementsUnreadable) {
		t.Fatalf("expected ErrProjectRequirementsUnreadable, got %v", err)
	}

	staleManifest := filepath.Join(staleDownloadPath, "ansible_collections", "stale", "coll", "MANIFEST.json")
	if _, statErr := os.Stat(staleManifest); statErr != nil {
		t.Fatalf("expected the stale project's own install to survive the abort, stat error: %v", statErr)
	}
	otherManifest := filepath.Join(otherInstallDir, "MANIFEST.json")
	if _, statErr := os.Stat(otherManifest); statErr != nil {
		t.Fatalf("expected the other project's install to survive the abort untouched, stat error: %v", statErr)
	}
}

// buildUnscannedProjectFixture builds the two-project fixture
// TestUnparseableRequirementsAbortsEvenForUnscannedProject and its positive
// control share, mirroring buildStaleRegistryFixture's own shape: an
// "absent-workspace-project" whose collections path has no
// ansible_collections subdirectory at all (so phase 1 skips it without ever
// reading its own requirements file) and whose requirements file is written
// with reqContent, and a healthy "other-project" holding one unreferenced
// install of its own. The project keys are kept exactly as the inline
// fixture this replaces used them, so sorted project order - load-bearing
// for buildReachable's own determinism (see its doc comment) - stays
// unchanged. It returns the absent-workspace project's own requirements file
// path (needed to pin which project's own path an abort error names) and the
// other project's install directory.
func buildUnscannedProjectFixture(t *testing.T, cacheDir string, reqContent []byte) (string, string) {
	t.Helper()
	absentCollectionsPath := t.TempDir()
	reqPath := filepath.Join(t.TempDir(), "unparseable.yml")
	if err := os.WriteFile(reqPath, reqContent, helpers.FileMod); err != nil {
		t.Fatalf("failed to write the absent-workspace project's requirements file: %v", err)
	}

	otherDownloadPath := t.TempDir()
	otherInstallDir := seedInstallTree(t, otherDownloadPath)
	otherReqPath := filepath.Join(t.TempDir(), "requirements-other.yml")
	if err := os.WriteFile(otherReqPath, []byte("collections: []\n"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write other project's requirements file: %v", err)
	}

	writeProjectRegistry(t, cacheDir, &store.ProjectRegistry{
		Projects: map[string]store.ProjectRecord{
			"absent-workspace-project": {RequirementsFile: reqPath, CollectionsPath: absentCollectionsPath, LastRun: time.Now().UTC()},
			"other-project":            {RequirementsFile: otherReqPath, CollectionsPath: otherDownloadPath, LastRun: time.Now().UTC()},
		},
	})
	return reqPath, otherInstallDir
}

// TestUnparseableRequirementsAbortsEvenForUnscannedProject proves the
// abort on an unreadable requirements file is not conditioned on that same
// project's own workspace having been scanned: a project whose collections
// path has no ansible_collections subdirectory at all (so phase 1 skips it
// without ever reading its requirements file) still aborts the whole run in
// phase 2 once that same requirements file turns out to be unparseable, and
// another, healthy project's install survives the abort untouched. Asserting
// that the error names unparseableReqPath - not just errors.Is - is what
// pins WHICH project's requirements file aborted the run: projectRequirementRoots
// wraps the underlying load failure as "%w: %s: %w" carrying
// project.RequirementsFile, so a regression that aborted for the wrong
// recorded project would still satisfy errors.Is here but fail this
// substring check.
//
// Confirmed killing mutation: the same `if !scanned { continue }` mutation
// documented on TestBuildReachableSkippedProjectStillContributesRoots's own
// doc comment kills this test too - phase 2 never reaches this project's
// requirements file at all once it is gated on phase 1 having scanned
// something, so the unparseable content is never read and Start returns nil
// instead of aborting:
//
//	cleanup_test.go:4259: expected ErrProjectRequirementsUnreadable, got <nil>
//	--- FAIL: TestUnparseableRequirementsAbortsEvenForUnscannedProject (0.01s)
func TestUnparseableRequirementsAbortsEvenForUnscannedProject(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	unparseableReqPath, otherInstallDir := buildUnscannedProjectFixture(t, cacheDir, []byte("{invalid"))

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	err := Start(t.Context(), cfg, runtime)
	if !errors.Is(err, helpers.ErrProjectRequirementsUnreadable) {
		t.Fatalf("expected ErrProjectRequirementsUnreadable, got %v", err)
	}
	if !strings.Contains(err.Error(), unparseableReqPath) {
		t.Fatalf("expected the error to name the failing requirements file %q, got %v", unparseableReqPath, err)
	}

	otherManifest := filepath.Join(otherInstallDir, "MANIFEST.json")
	if _, statErr := os.Stat(otherManifest); statErr != nil {
		t.Fatalf("expected the other project's install to survive the abort untouched, stat error: %v", statErr)
	}
}

// TestUnparseableRequirementsAbortsEvenForUnscannedProjectPositiveControl is
// the mandatory positive control for
// TestUnparseableRequirementsAbortsEvenForUnscannedProject on the identical
// fixture shape: a valid, empty requirements file at the absent-workspace
// project lets Start succeed and removes the other project's unreferenced
// install, proving the abort above is specific to the unparseable content,
// not to some other property of an absent-workspace project reaching phase
// 2.
func TestUnparseableRequirementsAbortsEvenForUnscannedProjectPositiveControl(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	_, otherInstallDir := buildUnscannedProjectFixture(t, cacheDir, []byte("collections: []\n"))

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}
	otherManifest := filepath.Join(otherInstallDir, "MANIFEST.json")
	if _, statErr := os.Stat(otherManifest); !os.IsNotExist(statErr) {
		t.Fatalf("expected the other project's unreferenced install to be removed, stat error: %v", statErr)
	}
}

// TestScanNamespaceDirAbortsOnUnreadableNamespaceDir proves
// scanNamespaceDir's own non-ENOENT read failure aborts the whole run:
// chmod 0o000 on ansible_collections/ns itself - not the manifest, and not
// ansible_collections - blocks fs.ReadDir from listing that namespace's own
// <name> entries, which is a different failure site than either existing
// manifest-level abort test (TestScanAbortsOnRealIOErrorReadingManifest's
// ReadFile "openat" on the manifest file itself, and
// TestScanAbortsOnUnreadableCollectionDir's Lstat "statat" on the manifest
// path). Asserting the error contains "openat ansible_collections/ns:" -
// with the trailing colon - is what pins this specific branch: neither
// sibling test's error string can ever contain that substring, since both
// name a deeper MANIFEST.json path instead.
//
// Skipped when running as root, since root bypasses the permission bits
// this test relies on to force the read failure, matching the two sibling
// tests' own guard.
func TestScanNamespaceDirAbortsOnUnreadableNamespaceDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission-based read guard cannot be tested")
	}
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()
	installDir := seedInstallTree(t, downloadPath)

	nsDir := filepath.Join(downloadPath, "ansible_collections", "ns")
	if err := os.Chmod(nsDir, 0o000); err != nil {
		t.Fatalf("failed to chmod namespace dir unreadable: %v", err)
	}
	restored := false
	restore := func() {
		if restored {
			return
		}
		if err := os.Chmod(nsDir, helpers.DirMod); err != nil {
			t.Errorf("failed to restore namespace dir perms: %v", err)
		}
		restored = true
	}
	t.Cleanup(restore)

	registerCleanupProject(t, cacheDir, downloadPath)
	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	err := Start(t.Context(), cfg, runtime)
	if err == nil {
		t.Fatalf("expected Start to abort on an unreadable namespace directory, got nil")
	}
	if !strings.Contains(err.Error(), "openat ansible_collections/ns:") {
		t.Fatalf("expected the error to name the failing openat call on ansible_collections/ns, got %v", err)
	}

	// The abort happens in buildReachable, before removeUnused ever runs, so
	// nothing could have been deleted. The survival check runs only after
	// restore(), not right here: a mode-0 namespace directory blocks this
	// test's own os.Stat traversal to the manifest too.
	restore()
	assertManifestSurvives(t, installDir)

	// Positive control: the identical fixture with the namespace directory
	// readable again must complete and remove the unreferenced install,
	// proving the abort above pins the permission failure specifically and
	// not some other property of this fixture.
	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed once the namespace directory is readable again, got %v", err)
	}
	manifestPath := filepath.Join(installDir, "MANIFEST.json")
	if _, statErr := os.Stat(manifestPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected the unreferenced install to be removed once the read succeeds, stat error: %v", statErr)
	}
}

// TestScanCollectionDirManifestMissingVersionIsBenignSkip proves a
// manifest with a namespace and name but no version is buildInstalledRecord's
// benign skip (ok=false, err=nil): the collection is neither indexed nor
// reported, and its on-disk tree survives untouched. The assertion is
// deliberately !hasWarningContaining("unsafe identifier") rather than
// asserting zero warnings outright: this fixture seeds no snapshot, so
// warnIfSnapshotNotPersisted's own, unrelated warning fires regardless, and
// asserting on the total warning count would make this test fail for a
// reason that has nothing to do with the missing-version skip it exists to
// pin.
func TestScanCollectionDirManifestMissingVersionIsBenignSkip(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()

	installDir := filepath.Join(downloadPath, "ansible_collections", "ns", "name")
	if err := os.MkdirAll(installDir, helpers.DirMod); err != nil {
		t.Fatalf("failed to create install dir: %v", err)
	}
	manifestNoVersion := `{"collection_info": {"namespace": "ns", "name": "name"}}`
	manifestPath := filepath.Join(installDir, "MANIFEST.json")
	if err := os.WriteFile(manifestPath, []byte(manifestNoVersion), helpers.FileMod); err != nil {
		t.Fatalf("failed to write manifest without a version: %v", err)
	}
	registerCleanupProject(t, cacheDir, downloadPath)

	printer := &recordingPrinter{}
	runtime := newTestRuntimeWith(printer)
	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}
	if _, statErr := os.Stat(manifestPath); statErr != nil {
		t.Fatalf("expected the version-less install to survive as a benign skip, stat error: %v", statErr)
	}
	if printer.hasWarningContaining("unsafe identifier") {
		t.Fatalf("expected no unsafe-identifier warning for a manifest missing only its version, got: %v", printer.warnings)
	}
	if printer.hasWarningContaining("corrupt manifest") {
		t.Fatalf("expected no corrupt-manifest warning for a manifest missing only its version, got: %v", printer.warnings)
	}
}

// TestScanCollectionDirManifestMissingVersionPositiveControl is the positive
// control for TestScanCollectionDirManifestMissingVersionIsBenignSkip on the
// identical fixture shape: the same manifest with a version present is
// indexed normally and, being unreferenced, removed - proving the survival
// above is buildInstalledRecord's benign skip specifically, not some
// unrelated reason the install tree was left alone.
func TestScanCollectionDirManifestMissingVersionPositiveControl(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()
	installDir := seedInstallTree(t, downloadPath) // ns.name@1.0.0, version present
	registerCleanupProject(t, cacheDir, downloadPath)

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}
	manifestPath := filepath.Join(installDir, "MANIFEST.json")
	if _, statErr := os.Stat(manifestPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected the version-complete install to be indexed and removed, stat error: %v", statErr)
	}
}

// buildCollectionsPathFallbackFixture creates a real project directory -
// used directly as the registry's map key, exactly as store.RecordProject
// would key it from a requirements file's own directory - with an
// ansible_collections tree seeded under a fallbackDirName (".collections" or
// "collections") subdirectory, and registers a project record whose
// CollectionsPath is deliberately empty so collectionsPathCandidates falls
// back to that project-relative candidate. It returns the seeded install's
// directory; the project directory itself (the registry key) is not needed
// by any caller.
func buildCollectionsPathFallbackFixture(t *testing.T, cacheDir, fallbackDirName string) string {
	t.Helper()
	projectDir := t.TempDir()
	fallbackRoot := filepath.Join(projectDir, fallbackDirName)
	installDir := seedInstallTree(t, fallbackRoot)

	reqPath := filepath.Join(projectDir, "requirements.yml")
	if err := os.WriteFile(reqPath, []byte("collections: []\n"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write requirements file: %v", err)
	}
	writeProjectRegistry(t, cacheDir, &store.ProjectRegistry{
		Projects: map[string]store.ProjectRecord{
			projectDir: {RequirementsFile: reqPath, CollectionsPath: "", LastRun: time.Now().UTC()},
		},
	})
	return installDir
}

// TestOpenProjectWorkspaceFallsBackToDotCollections proves that an
// empty recorded CollectionsPath falls back to a project-relative
// ".collections" directory: the workspace under it is scanned and its
// unreferenced install cleaned up normally.
func TestOpenProjectWorkspaceFallsBackToDotCollections(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	installDir := buildCollectionsPathFallbackFixture(t, cacheDir, ".collections")

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed scanning the .collections fallback, got %v", err)
	}
	manifestPath := filepath.Join(installDir, "MANIFEST.json")
	if _, statErr := os.Stat(manifestPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected the unreferenced install under .collections to be removed, stat error: %v", statErr)
	}
}

// TestOpenProjectWorkspaceFallsBackToCollections is
// TestOpenProjectWorkspaceFallsBackToDotCollections's sibling for the second,
// non-dotfile fallback candidate ("collections").
func TestOpenProjectWorkspaceFallsBackToCollections(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	installDir := buildCollectionsPathFallbackFixture(t, cacheDir, "collections")

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed scanning the collections fallback, got %v", err)
	}
	manifestPath := filepath.Join(installDir, "MANIFEST.json")
	if _, statErr := os.Stat(manifestPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected the unreferenced install under collections to be removed, stat error: %v", statErr)
	}
}

// TestOpenProjectWorkspacePrefersRecordedCollectionsPathOverFallback is the
// positive control proving collectionsPathCandidates' order, not merely its
// reachability: a project with a non-empty, real CollectionsPath plus an
// existing ".collections" sibling holding a completely different,
// unreferenced collection. Only the recorded path is ever opened -
// openProjectWorkspace returns at its first usable candidate - so the
// sibling collection under ".collections" must survive untouched even
// though it too is unreferenced by anything.
//
// Confirmed killing mutation (reversing collectionsPathCandidates' two
// append blocks, so the ".collections"/"collections" fallbacks are tried
// before the recorded CollectionsPath): running
// `go test -run TestOpenProjectWorkspacePrefersRecordedCollectionsPathOverFallback -v`
// against that mutation produced:
//
//	cleanup_test.go:4567: expected the recorded CollectionsPath's unreferenced install to be removed, stat error: <nil>
//	--- FAIL: TestOpenProjectWorkspacePrefersRecordedCollectionsPathOverFallback (0.01s)
//
// under the mutation, the fallback candidate wins the race instead: it is a
// real, usable workspace holding only the sibling collection, so
// openProjectWorkspace returns there first and never even opens
// recordedPath, leaving its own unreferenced install untouched (a nil stat
// error) instead of removed.
func TestOpenProjectWorkspacePrefersRecordedCollectionsPathOverFallback(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	projectDir := t.TempDir()

	recordedPath := t.TempDir()
	recordedInstallDir := seedInstallTree(t, recordedPath) // ns.name@1.0.0

	fallbackRoot := filepath.Join(projectDir, ".collections")
	fallbackInstallDir := filepath.Join(fallbackRoot, "ansible_collections", "sibling", "other")
	if err := os.MkdirAll(fallbackInstallDir, helpers.DirMod); err != nil {
		t.Fatalf("failed to create sibling fallback install dir: %v", err)
	}
	sibling := `{"collection_info": {"namespace": "sibling", "name": "other", "version": "1.0.0"}}`
	fallbackManifestPath := filepath.Join(fallbackInstallDir, "MANIFEST.json")
	if err := os.WriteFile(fallbackManifestPath, []byte(sibling), helpers.FileMod); err != nil {
		t.Fatalf("failed to write sibling manifest: %v", err)
	}

	reqPath := filepath.Join(projectDir, "requirements.yml")
	if err := os.WriteFile(reqPath, []byte("collections: []\n"), helpers.FileMod); err != nil {
		t.Fatalf("failed to write requirements file: %v", err)
	}
	writeProjectRegistry(t, cacheDir, &store.ProjectRegistry{
		Projects: map[string]store.ProjectRecord{
			projectDir: {RequirementsFile: reqPath, CollectionsPath: recordedPath, LastRun: time.Now().UTC()},
		},
	})

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	if err := Start(t.Context(), cfg, runtime); err != nil {
		t.Fatalf("expected Start to succeed, got %v", err)
	}

	recordedManifest := filepath.Join(recordedInstallDir, "MANIFEST.json")
	if _, statErr := os.Stat(recordedManifest); !os.IsNotExist(statErr) {
		t.Fatalf("expected the recorded CollectionsPath's unreferenced install to be removed, stat error: %v", statErr)
	}
	if _, statErr := os.Stat(fallbackManifestPath); statErr != nil {
		t.Fatalf("expected the .collections sibling to survive untouched, stat error: %v", statErr)
	}
}
