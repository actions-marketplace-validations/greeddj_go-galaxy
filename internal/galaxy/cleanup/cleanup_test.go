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

// newTestRuntime builds an Infra wired with a no-op printer and the default
// HTTP client, matching the pattern used by the collections package's own
// tests for constructing an Infra without caring about rendered output.
func newTestRuntime() *infra.Infra {
	return infra.New(noopPrinter{}, http.DefaultClient)
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

// TestStartFailsOnMissingRequirements proves the same fail-safe as
// TestStartFailsOnUnreadableRequirementsCorrupt for a requirements file that
// does not exist at all, for a project whose workspace is otherwise present
// on disk. A missing file for a present workspace is a load failure exactly
// like corrupt content - it must abort the run rather than skip past it,
// since openProjectWorkspace has already confirmed the workspace exists and
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
// reachability must also drive C08.3's snapshot-derived extracted-store
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
// legacy-keyed artifact in the first place.
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
	if !printer.hasPrintContaining("legacy artifact") {
		t.Fatalf("expected a would-sweep report mentioning the legacy artifact, got prints: %v", printer.prints)
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
