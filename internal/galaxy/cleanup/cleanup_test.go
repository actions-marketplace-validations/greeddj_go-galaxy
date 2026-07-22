package cleanup

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	cacheBackend "github.com/greeddj/go-galaxy/internal/cache"
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

// recordingPrinter is an output.Printer stub that records Warnf calls so
// tests can assert that a rejection surfaced a warning rather than being
// silently swallowed. It embeds noopPrinter for the other Printer methods and
// is safe for concurrent use since scanInstalledCollections may be called
// from goroutines in other packages, though cleanup itself scans serially.
type recordingPrinter struct {
	noopPrinter

	warnings []string
	mu       sync.Mutex
}

func (p *recordingPrinter) Warnf(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.warnings = append(p.warnings, fmt.Sprintf(format, args...))
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
// lock, and Bolt store - not just at the LoadProjectRegistry unit level.
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

// newTestRuntime builds an Infra wired with a no-op printer and the default
// HTTP client, matching the pattern used by the collections package's own
// tests for constructing an Infra without caring about rendered output.
func newTestRuntime() *infra.Infra {
	return infra.New(noopPrinter{}, http.DefaultClient)
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

// TestRemoveInstalledContainmentGuard exercises removeInstalled directly
// with a crafted installedCollection whose Version is a traversal payload,
// proving the removal-time guard rejects it with ErrUnsafeRemovalPath
// (defense in depth, independent of the ingestion-time rejection covered by
// TestRemoveInstalledRejectsTraversalVersion) and never calls os.RemoveAll
// on anything, so an unrelated outside sentinel survives.
func TestRemoveInstalledContainmentGuard(t *testing.T) {
	t.Parallel()
	collectionsDir := t.TempDir()
	sentinelFile := writeOutsideSentinel(t, t.TempDir(), "evil")

	inst := installedCollection{
		Key:            "ns.name@../../evil",
		FQDN:           "ns.name",
		Version:        "../../evil",
		InstallPath:    filepath.Join(collectionsDir, "ansible_collections", "ns", "name"),
		CollectionsDir: collectionsDir,
	}

	err := removeInstalled(t.Context(), inst, nil)
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
// succeed.
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
		{"namespace is empty", "", "name", "1.0.0", false, false},
		{"name is empty", "ns", "", "1.0.0", false, false},
		{"version is empty", "ns", "name", "", false, false},
		{"version is dot", "ns", "name", ".", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var manifest types.GalaxyCollectionVersionInfoManifest
			manifest.CollectionInfo.Namespace = tc.ns
			manifest.CollectionInfo.Name = tc.coll
			manifest.CollectionInfo.Version = tc.version

			record, key, ok, err := buildInstalledRecord("/collections", "/collections/ansible_collections/x/MANIFEST.json", manifest)

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
// seeded and found by pickCollectionsPath), but whose requirements file
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

// TestStartFailsOnMissingRequirements proves the same fail-safe as
// TestStartFailsOnUnreadableRequirementsCorrupt for a requirements file that
// does not exist at all, for a project whose workspace is otherwise present
// on disk. A missing file for a present workspace is a load failure exactly
// like corrupt content - it must abort the run rather than skip past it,
// since pickCollectionsPath has already confirmed the workspace exists and
// scanInstalledCollections has already populated deletion candidates for it.
func TestStartFailsOnMissingRequirements(t *testing.T) {
	t.Parallel()
	cacheDir := t.TempDir()
	downloadPath := t.TempDir()
	installDir := seedInstallTree(t, downloadPath)

	reqPath := filepath.Join(t.TempDir(), "does-not-exist.yml")
	registerCleanupProjectAt(t, cacheDir, downloadPath, reqPath)

	cfg := &config.Config{CacheDir: cacheDir, DryRun: false}
	runtime := newTestRuntime()

	err := Start(t.Context(), cfg, runtime)
	if !errors.Is(err, helpers.ErrProjectRequirementsUnreadable) {
		t.Fatalf("expected ErrProjectRequirementsUnreadable, got %v", err)
	}
	assertManifestSurvives(t, installDir)
}

// TestStartDeletesUnreferencedWithValidRequirements is the control for the
// two requirements-load-failure tests above: a project with a valid
// requirements file that references nothing must still let cleanup proceed
// normally and delete the unreferenced installed collection, proving the
// new fail-the-run behavior triggers only on an actual load failure and not
// on every non-matching or empty requirements file.
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
// pickCollectionsPath skip the project entirely, so its installed snapshot
// entries are never scanned, never pruned, and never contribute to
// installedByKey - the normal ephemeral-CI state this commit's fix targets.
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
// Before this fix, installedByKey was map[string]installedCollection: the
// second project's scan silently overwrote the first project's record for
// the same key, so removeUnused only ever saw and removed the last copy
// scanned, leaving the other to survive until a later run.
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
