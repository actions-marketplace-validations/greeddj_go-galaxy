package collections

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/greeddj/go-galaxy/internal/cache/local"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/psvmcc/hub/pkg/types"
)

// noopPrinter is a minimal output.Printer stub for tests that need an Infra
// but do not care about the rendered progress output.
type noopPrinter struct{}

func (noopPrinter) Printf(string, ...any)                 {}
func (noopPrinter) PersistentPrintf(string, ...any)       {}
func (noopPrinter) Okf(string, ...any)                    {}
func (noopPrinter) Errorf(string, ...any)                 {}
func (noopPrinter) Warnf(string, ...any)                  {}
func (noopPrinter) Debugf(string, ...any)                 {}
func (noopPrinter) DebugSincef(time.Time, string, ...any) {}

func TestVerifyPinnedSHA(t *testing.T) {
	t.Parallel()
	col := collection{Namespace: "ns", Name: "name", Version: "1.0.0"}

	tests := []struct {
		name    string
		pin     string
		actual  string
		wantErr bool
	}{
		{name: "empty pin is a no-op", pin: "", actual: "deadbeef", wantErr: false},
		{name: "matching pin passes", pin: "abc123", actual: "abc123", wantErr: false},
		{name: "mismatched pin fails", pin: "abc123", actual: "def456", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := col
			c.SHA256 = tt.pin
			err := verifyPinnedSHA(c, tt.actual)
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("expected nil error, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error, got nil")
			}
			if !errors.Is(err, helpers.ErrSHA256Mismatch) {
				t.Fatalf("expected errors.Is ErrSHA256Mismatch, got %v", err)
			}
			if !strings.Contains(err.Error(), c.key()) {
				t.Fatalf("expected error to contain key %q, got %v", c.key(), err)
			}
		})
	}
}

// newTestInstallDeps builds installDeps rooted at t.TempDir subdirectories,
// wired with a no-op printer and the default HTTP client. It constructs the
// struct directly (rather than via newInstallDeps) so this test does not add
// another always-nil call site for the db parameter, which unparam would
// otherwise flag.
func newTestInstallDeps(t *testing.T, cfg *config.Config) installDeps {
	t.Helper()
	runtime := infra.New(noopPrinter{}, http.DefaultClient)
	st := store.New()
	artifacts := local.NewArtifacts(cfg.CacheDir)
	return installDeps{
		collectionDeps: newCollectionDeps(cfg, runtime, st),
		artifacts:      artifacts,
	}
}

func TestInstallCollectionCacheHitPinMismatch(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	downloadPath := filepath.Join(root, "install")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}

	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0", SHA256: "not-the-real-hash"}
	artifactPath := filepath.Join(cacheDir, artifactKey(col))
	content := []byte("arbitrary tarball bytes for cache-hit test")
	if err := os.WriteFile(artifactPath, content, helpers.FileMod); err != nil {
		t.Fatalf("seed cached artifact: %v", err)
	}
	if realSHA := sha256Hex(content); realSHA == col.SHA256 {
		t.Fatalf("test setup bug: pin accidentally matches real hash")
	}

	cfg := &config.Config{
		CacheDir:     cacheDir,
		DownloadPath: downloadPath,
		Workers:      1,
		NoDeps:       true,
		Offline:      true,
	}
	deps := newTestInstallDeps(t, cfg)

	err := installCollection(context.Background(), col, deps, nil, nil)
	if err == nil {
		t.Fatalf("expected pin mismatch error, got nil")
	}
	if !errors.Is(err, helpers.ErrSHA256Mismatch) {
		t.Fatalf("expected errors.Is ErrSHA256Mismatch, got %v", err)
	}

	installPath := filepath.Join(downloadPath, "ansible_collections", col.Namespace, col.Name)
	if _, statErr := os.Stat(installPath); statErr == nil {
		t.Fatalf("install path %s was created despite the pin mismatch", installPath)
	} else if !os.IsNotExist(statErr) {
		t.Fatalf("unexpected stat error on install path: %v", statErr)
	}
}

func TestInstallCollectionFreshDownloadPinMismatch(t *testing.T) {
	t.Parallel()
	content := []byte("bytes served fresh over http for download-path test")
	correctSHA := sha256Hex(content)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(content)
	}))
	defer server.Close()

	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	downloadPath := filepath.Join(root, "install")
	if err := os.MkdirAll(cacheDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir cacheDir: %v", err)
	}

	col := collection{Namespace: "acme", Name: "gadgets", Version: "2.0.0", SHA256: "wrong-pin-value"}

	meta := &types.GalaxyCollectionVersionInfo{DownloadURL: server.URL}
	meta.Artifact.Sha256 = correctSHA

	cfg := &config.Config{
		CacheDir:     cacheDir,
		DownloadPath: downloadPath,
		Workers:      1,
		NoDeps:       true,
		Offline:      false,
		NoCache:      false,
	}
	deps := newTestInstallDeps(t, cfg)

	err := installCollection(context.Background(), col, deps, nil, meta)
	if err == nil {
		t.Fatalf("expected pin mismatch error, got nil")
	}
	if !errors.Is(err, helpers.ErrSHA256Mismatch) {
		t.Fatalf("expected errors.Is ErrSHA256Mismatch, got %v", err)
	}

	installPath := filepath.Join(downloadPath, "ansible_collections", col.Namespace, col.Name)
	if _, statErr := os.Stat(installPath); statErr == nil {
		t.Fatalf("install path %s was created despite the pin mismatch", installPath)
	} else if !os.IsNotExist(statErr) {
		t.Fatalf("unexpected stat error on install path: %v", statErr)
	}
}

func TestCanSkipInstallPinGate(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	downloadPath := filepath.Join(root, "install")

	col := collection{Namespace: "acme", Name: "widgets", Version: "1.0.0"}
	installPath := filepath.Join(downloadPath, "ansible_collections", col.Namespace, col.Name)
	const installedSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	if err := os.MkdirAll(installPath, helpers.DirMod); err != nil {
		t.Fatalf("mkdir installPath: %v", err)
	}
	marker := filepath.Join(installPath, ".extract-done."+installedSHA)
	if err := os.WriteFile(marker, []byte("ok"), helpers.FileMod); err != nil {
		t.Fatalf("write extract marker: %v", err)
	}
	infoDir := filepath.Join(downloadPath, "ansible_collections", col.Namespace+"."+col.Name+"-"+col.Version+".info")
	if err := os.MkdirAll(infoDir, helpers.DirMod); err != nil {
		t.Fatalf("mkdir infoDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(infoDir, "GALAXY.yml"), []byte("format_version: 1.0.0\n"), helpers.FileMod); err != nil {
		t.Fatalf("write GALAXY.yml: %v", err)
	}

	cfg := &config.Config{DownloadPath: downloadPath}
	st := store.New()
	st.SetInstalled(col.key(), store.InstalledEntry{
		InstallPath:    installPath,
		ArtifactSHA256: installedSHA,
		InstalledAt:    time.Now().UTC(),
	})

	matching := col
	matching.SHA256 = installedSHA
	if !canSkipInstall(cfg, matching, installPath, st) {
		t.Fatalf("expected canSkipInstall to return true when the pin matches the installed SHA")
	}

	mismatched := col
	mismatched.SHA256 = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if canSkipInstall(cfg, mismatched, installPath, st) {
		t.Fatalf("expected canSkipInstall to return false when the pin does not match the installed SHA")
	}
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
