package gitfetch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"

	"github.com/greeddj/go-galaxy/internal/galaxy/archive"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/testing/fakegit"
)

// roleRepo is the role fixture: role "app" depending on acme.base on main
// (HEAD), tagged v1, and a second commit on dev with one more dependency
// and no role name.
type roleRepo struct {
	repo  *fakegit.Repo
	first plumbing.Hash
	dev   plumbing.Hash
}

func newRoleRepo(t *testing.T) roleRepo {
	t.Helper()
	r := fakegit.NewRepo(t)
	r.AddRole([]string{"acme.base"}, "app")
	first := r.Commit("role app")
	r.Branch("main", first)
	r.SetHEAD("main")
	r.Tag("v1", first)
	r.AddRole([]string{"acme.base", "acme.extra,1.0.0"}, "")
	dev := r.Commit("role app dev")
	r.Branch("dev", dev)
	r.Branch("main", first)
	return roleRepo{repo: r, first: first, dev: dev}
}

func acquireRole(t *testing.T, f *Fetcher, req gitsource.RoleRequest) (gitsource.RoleResult, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()
	if req.TempFile == nil {
		req.TempFile = tempFileIn(t.TempDir())
	}
	res, err := f.AcquireRole(ctx, req)
	if res.Cleanup != nil {
		t.Cleanup(res.Cleanup)
	}
	return res, err
}

// assertRoleArtifact checks the artifact is a real file whose sha is well
// formed and whose entries extract at the top level of a directory.
func assertRoleArtifact(t *testing.T, res gitsource.RoleResult) {
	t.Helper()
	if !helpers.IsSHA256Hex(res.ArtifactSHA) {
		t.Fatalf("ArtifactSHA = %q", res.ArtifactSHA)
	}
	if res.BytesFetched <= 0 {
		t.Fatalf("BytesFetched = %d", res.BytesFetched)
	}
	dst := t.TempDir()
	if err := archive.ExtractTarGz(t.Context(), res.ArtifactPath, dst); err != nil {
		t.Fatalf("ExtractTarGz: %v", err)
	}
	for _, name := range []string{"meta/main.yml", "tasks/main.yml", "defaults/main.yml"} {
		if _, err := os.Stat(filepath.Join(dst, filepath.FromSlash(name))); err != nil {
			t.Fatalf("%s not extracted at the top level: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, "MANIFEST.json")); err == nil {
		t.Fatalf("a role artifact carries a MANIFEST.json")
	}
}

type roleRefCase struct {
	name        string
	ref         string
	commit      string
	wantRefName string
	wantRole    string
	wantDeps    []gitsource.RoleDependency
	wantCommit  plumbing.Hash
}

func roleRefCases(role roleRepo) []roleRefCase {
	base := []gitsource.RoleDependency{{Src: "acme.base"}}
	devDeps := []gitsource.RoleDependency{{Src: "acme.base"}, {Src: "acme.extra,1.0.0"}}
	return []roleRefCase{
		{name: "HEAD", ref: "", wantRefName: "refs/heads/main", wantRole: "app", wantDeps: base, wantCommit: role.first},
		{name: "tag", ref: "v1", wantRefName: "refs/tags/v1", wantRole: "app", wantDeps: base, wantCommit: role.first},
		{name: "branch", ref: "dev", wantRefName: "refs/heads/dev", wantDeps: devDeps, wantCommit: role.dev},
		{name: "pinned commit", ref: "main", commit: role.dev.String(), wantRefName: "main", wantDeps: devDeps, wantCommit: role.dev},
		{name: "commit ref", ref: role.first.String(), wantRefName: role.first.String(), wantRole: "app", wantDeps: base, wantCommit: role.first},
	}
}

// TestAcquireRoleOverHTTP drives every ref shape through the real smart-HTTP
// transport against fakegit and checks the metadata and the artifact each
// time.
func TestAcquireRoleOverHTTP(t *testing.T) {
	t.Parallel()
	role := newRoleRepo(t)
	for _, tc := range roleRefCases(role) {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := fakegit.New(t)
			srv.Add("role", role.repo)
			res, err := acquireRole(t, newFetcher(t), gitsource.RoleRequest{
				URL:    mustURL(t, srv.RepoURL("role")),
				Ref:    mustRef(t, tc.ref),
				Commit: tc.commit,
			})
			if err != nil {
				t.Fatalf("AcquireRole: %v", err)
			}
			if res.RefName != tc.wantRefName || res.Commit != tc.wantCommit.String() {
				t.Fatalf("resolved to (%s, %s), want (%s, %s)", res.RefName, res.Commit, tc.wantRefName, tc.wantCommit)
			}
			if res.GalaxyRoleName != tc.wantRole {
				t.Fatalf("GalaxyRoleName = %q, want %q", res.GalaxyRoleName, tc.wantRole)
			}
			if !reflect.DeepEqual(res.Dependencies, tc.wantDeps) {
				t.Fatalf("Dependencies = %#v, want %#v", res.Dependencies, tc.wantDeps)
			}
			if len(res.Warnings) != 0 {
				t.Fatalf("warnings = %q", res.Warnings)
			}
			assertRoleArtifact(t, res)
			if srv.Count(fakegit.EndpointInfoRefs) != 1 {
				t.Fatalf("info/refs requested %d times, want 1", srv.Count(fakegit.EndpointInfoRefs))
			}
		})
	}
}

// TestAcquireRoleCleanupRemovesTheArtifact proves Cleanup removes the temp
// file and is safe to call twice, and that the storage directory is gone
// once the acquisition returns.
func TestAcquireRoleCleanupRemovesTheArtifact(t *testing.T) {
	t.Parallel()
	role := newRoleRepo(t)
	srv := fakegit.New(t)
	srv.Add("role", role.repo)
	tempDir := t.TempDir()
	f := New(newFetcher(t).httpClient, func() string { return tempDir })
	res, err := acquireRole(t, f, gitsource.RoleRequest{URL: mustURL(t, srv.RepoURL("role")), Ref: mustRef(t, "")})
	if err != nil {
		t.Fatalf("AcquireRole: %v", err)
	}
	if entries, _ := os.ReadDir(tempDir); len(entries) != 0 {
		t.Fatalf("storage left under the temp dir: %v", entries)
	}
	if _, err := os.Stat(res.ArtifactPath); err != nil {
		t.Fatalf("artifact missing before cleanup: %v", err)
	}
	res.Cleanup()
	res.Cleanup()
	if _, err := os.Stat(res.ArtifactPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("artifact after cleanup: %v", err)
	}
}

// TestAcquireRoleRefusals pins the refusals: a repository that is not a role
// (the collection fixture has a meta directory but no main.yml), a missing
// ref, and a request without a temp file supplier.
func TestAcquireRoleRefusals(t *testing.T) {
	t.Parallel()
	role := newRoleRepo(t)
	app := newAppRepo(t)
	srv := fakegit.New(t)
	srv.Add("role", role.repo)
	srv.Add("app", app.repo)
	f := newFetcher(t)
	cases := []struct {
		wantErr error
		name    string
		repo    string
		ref     string
	}{
		{name: "not a role", repo: "app", ref: "", wantErr: helpers.ErrRoleMetaNotFound},
		{name: "missing ref", repo: "role", ref: "nosuch", wantErr: helpers.ErrGitRefNotFound},
		{name: "missing repository", repo: "missing", ref: "", wantErr: helpers.ErrGitTransportFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := acquireRole(t, f, gitsource.RoleRequest{URL: mustURL(t, srv.RepoURL(tc.repo)), Ref: mustRef(t, tc.ref)})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("AcquireRole error = %v, want %v", err, tc.wantErr)
			}
		})
	}
	t.Run("nil temp file supplier", func(t *testing.T) {
		t.Parallel()
		// Its own server, so the sibling subtests' requests cannot be
		// mistaken for one this request made.
		own := fakegit.New(t)
		own.Add("role", role.repo)
		_, err := f.AcquireRole(t.Context(), gitsource.RoleRequest{URL: mustURL(t, own.RepoURL("role")), Ref: mustRef(t, "")})
		if !errors.Is(err, helpers.ErrConfigIsNil) {
			t.Fatalf("nil TempFile: %v, want ErrConfigIsNil", err)
		}
		if own.Total() != 0 {
			t.Fatalf("a request without a temp file supplier reached the remote")
		}
	})
}
