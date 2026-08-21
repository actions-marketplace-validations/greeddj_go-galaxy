package rolebuild

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"

	"github.com/greeddj/go-galaxy/internal/galaxy/archive"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/treearchive"
)

// gitDirName is the one directory name the build excludes at every depth.
const gitDirName = ".git"

// installInfoPattern is the one file the build leaves out of the artifact:
// ansible-galaxy's own install record at the role root, which a repository
// commits by accident often enough. The install writes its own record after
// materialization, and a committed one in the artifact would be the
// read-only hard link that write has to replace.
const installInfoPattern = "meta/.galaxy_install_info"

// Build turns the role at the root of src into a tar.gz artifact written
// into the file tempFile supplies. The metadata is read first, so a tree
// that is not a role is refused before the walk; the tree is then planned
// and written with no lead documents. The result's Cleanup removes the file
// and is idempotent; on any error the file is already gone.
func Build(ctx context.Context, src treearchive.Source, tempFile TempFileFunc) (Built, error) {
	if tempFile == nil {
		return Built{}, fmt.Errorf("%w: no temp file supplier", helpers.ErrConfigIsNil)
	}
	meta, err := readMeta(src)
	if err != nil {
		return Built{}, err
	}
	plan, err := treearchive.PlanTree(ctx, src, treearchive.Options{
		Subject: "the role",
		Rules:   treearchive.Rules{DirNames: map[string]struct{}{gitDirName: {}}, Patterns: []string{installInfoPattern}},
	})
	if err != nil {
		return Built{}, err
	}

	file, cleanup, err := tempFile(ctx)
	if err != nil {
		return Built{}, err
	}
	artifactPath := file.Name()
	digest, err := plan.Write(ctx, file, nil)
	if err != nil {
		_ = file.Close()
		cleanup()
		return Built{}, err
	}
	if err := file.Close(); err != nil {
		cleanup()
		return Built{}, fmt.Errorf("closing %s: %w", artifactPath, err)
	}
	if err := selfCheck(ctx, artifactPath); err != nil {
		cleanup()
		return Built{}, err
	}

	var once sync.Once
	return Built{
		Cleanup:      func() { once.Do(cleanup) },
		Meta:         meta,
		ArtifactPath: artifactPath,
		SHA256:       digest,
		Warnings:     append(slices.Clone(plan.Warnings()), meta.Warnings...),
	}, nil
}

// selfCheck probes the artifact's outer shape the way the extractor will.
// Any failure is this package's defect, rendered with %v rather than %w so
// it never classifies as an integrity failure of the remote's bytes - see
// helpers.ErrGitArtifactSelfCheck. The caller's own cancellation, which the
// probe returns unchanged, is passed through unchanged too: it is not a
// defect of the artifact.
func selfCheck(ctx context.Context, artifactPath string) error {
	err := archive.ProbeTarGz(ctx, artifactPath)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	default:
		//nolint:errorlint // deliberately %v, not %w: see the doc comment above.
		return fmt.Errorf("%w: %v", helpers.ErrGitArtifactSelfCheck, err)
	}
}

// readMeta locates and parses the role's metadata: meta/main.yml (else
// meta/main.yaml) for the dependencies and role name, then
// meta/requirements.yml (else .yaml) whose list is appended behind them. A
// meta directory that is absent, or that lists neither main file as a regular
// file, makes the tree not a role.
func readMeta(src treearchive.Source) (Meta, error) {
	root, err := src.ReadDir("")
	if err != nil {
		return Meta{}, err
	}
	if !listsKind(root, metaDirName, treearchive.EntryDir) {
		return Meta{}, fmt.Errorf("%w: the repository root has no %s directory", helpers.ErrRoleMetaNotFound, metaDirName)
	}
	entries, err := src.ReadDir(metaDirName)
	if err != nil {
		return Meta{}, err
	}
	meta, err := readMain(src, entries)
	if err != nil {
		return Meta{}, err
	}
	deps, warnings, err := readRequirements(src, entries)
	if err != nil {
		return Meta{}, err
	}
	meta.Dependencies = append(meta.Dependencies, deps...)
	meta.Warnings = append(meta.Warnings, warnings...)
	return meta, nil
}

// readMain reads whichever main spelling the meta directory lists.
func readMain(src treearchive.Source, entries []treearchive.Entry) (Meta, error) {
	name, err := pickOne(entries, mainYMLName, mainYAMLName)
	if err != nil {
		return Meta{}, err
	}
	if name == "" {
		return Meta{}, fmt.Errorf("%w: %s carries neither %s nor %s", helpers.ErrRoleMetaNotFound, metaDirName, mainYMLName, mainYAMLName)
	}
	p := treearchive.JoinPath(metaDirName, name)
	data, err := readMetadataFile(src, p)
	if err != nil {
		return Meta{}, err
	}
	return parseMetaMain(data, p)
}

// readRequirements reads whichever requirements spelling the meta directory
// lists; neither is an empty list.
func readRequirements(src treearchive.Source, entries []treearchive.Entry) ([]gitsource.RoleDependency, []string, error) {
	name, err := pickOne(entries, requirementsYMLName, requirementsYAMLName)
	if err != nil || name == "" {
		return nil, nil, err
	}
	p := treearchive.JoinPath(metaDirName, name)
	data, err := readMetadataFile(src, p)
	if err != nil {
		return nil, nil, err
	}
	return parseMetaRequirements(data, p)
}

// pickOne returns whichever of the two spellings entries list as a regular
// file, "" when neither is listed, and a refusal when both are: ansible would
// read the first and ignore the second, and a role whose two spellings
// disagree has no one answer.
func pickOne(entries []treearchive.Entry, yml, yaml string) (string, error) {
	hasYML := listsFile(entries, yml)
	hasYAML := listsFile(entries, yaml)
	switch {
	case hasYML && hasYAML:
		return "", fmt.Errorf("%w: %s carries both %s and %s", helpers.ErrRoleMetaInvalid, metaDirName, yml, yaml)
	case hasYML:
		return yml, nil
	case hasYAML:
		return yaml, nil
	default:
		return "", nil
	}
}

// listsFile reports whether entries carry name as a regular file. A link
// under the name is not metadata: following it would mean reading a blob the
// walk's own symlink rules may later skip.
func listsFile(entries []treearchive.Entry, name string) bool {
	return listsKind(entries, name, treearchive.EntryFile) || listsKind(entries, name, treearchive.EntryExecutable)
}

func listsKind(entries []treearchive.Entry, name string, kind treearchive.EntryKind) bool {
	for _, e := range entries {
		if e.Name == name && e.Kind == kind {
			return true
		}
	}
	return false
}

// readMetadataFile reads one metadata file under the metadata cap. The
// reader stops one byte past the cap so the refusal is decided here rather
// than by the Source's larger per-entry cap.
func readMetadataFile(src treearchive.Source, p string) ([]byte, error) {
	r, err := src.Open(p)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	data, err := io.ReadAll(io.LimitReader(r, metadataMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", treearchive.DisplayPath(p), err)
	}
	if len(data) > metadataMaxBytes {
		return nil, fmt.Errorf("%w: %s is larger than %d bytes", helpers.ErrRoleMetaInvalid, treearchive.DisplayPath(p), metadataMaxBytes)
	}
	return data, nil
}
