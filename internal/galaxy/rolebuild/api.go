package rolebuild

import (
	"context"
	"os"

	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
)

// Meta is what a role's metadata tells the installer: the dependencies of
// meta/main.yml with those of meta/requirements.yml appended behind them,
// each as written and unjudged; the galaxy_info.role_name when the meta
// carries one ("" when absent); and the warnings the parse raised - an empty
// meta file, a galaxy_info that is not a mapping or a role_name that is not a
// scalar, a name: overridden by a role: in the same spec.
type Meta struct {
	Dependencies []gitsource.RoleDependency
	RoleName     string
	Warnings     []string
}

// Built is one artifact Build produced: the metadata read from the tree, the
// temp path of the tar.gz, its sha256 computed while writing, the warnings
// raised along the way (the walk's skipped links and submodules, then
// Meta.Warnings), and a Cleanup that removes the temp file and is idempotent.
type Built struct {
	Cleanup      func()
	Meta         Meta
	ArtifactPath string
	SHA256       string
	Warnings     []string
}

// TempFileFunc hands Build the file it writes the artifact into; the cleanup
// removes it. See gitsource.TempFileFunc for why the caller supplies it.
type TempFileFunc func(ctx context.Context) (*os.File, func(), error)
