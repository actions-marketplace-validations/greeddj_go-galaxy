package commands

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/urfave/cli/v3"
)

// A missing lockfile used to mean different things to different commands: the
// lockfile exit class for the three that take --frozen, the environment-usage
// class for tree, explain and outdated, where a bare fs.ErrNotExist reached
// the usage classifier, and nothing at all for hash. The first two are the
// same fact - the lockfile a command was told to read is not there - and this
// file pins that they now classify the same, with hash's fallback pinned
// beside them so the exception stays a decision rather than a forgotten
// corner.
//
// outdated is covered by its own package's test rather than here: it is not a
// cmd/go-galaxy/commands entry point at all. What is shared, and what makes
// these pins hold together, is lockfile.LoadRequired.

// missingLockfileFixture writes a requirements file into a fresh directory
// and returns that directory, with no lockfile beside it.
func missingLockfileFixture(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	reqPath := filepath.Join(dir, "requirements.yml")
	body := []byte("collections:\n  - name: acme.widgets\n    version: \"*\"\n")
	if err := os.WriteFile(reqPath, body, helpers.FileMod); err != nil {
		t.Fatalf("write requirements: %v", err)
	}
	return dir
}

// runCommandInDir runs cmd with args from within dir. Running from the
// fixture directory is what makes the commands resolve their default lockfile
// path to a file that is genuinely absent, rather than to one that may exist
// beside the test binary. t.Chdir restores the previous directory itself and
// makes the test non-parallel, which is why nothing in this file calls
// t.Parallel.
func runCommandInDir(t *testing.T, dir string, cmd *cli.Command, args ...string) error {
	t.Helper()

	t.Chdir(dir)
	return cmd.Run(context.Background(), append([]string{cmd.Name}, args...))
}

// TestMissingLockfileClassifiesAsLockfileError pins the unified verdict for
// the two commands here that require a lockfile. Both the sentinel and the
// exit code are asserted: the sentinel is what the rest of the program reads,
// the exit code is what CI branches on, and only the pair together rules out
// a sentinel that no longer maps where it should.
func TestMissingLockfileClassifiesAsLockfileError(t *testing.T) {
	for _, tc := range missingLockfileCases() {
		t.Run(tc.name, func(t *testing.T) {
			dir := missingLockfileFixture(t)

			err := runCommandInDir(t, dir, tc.command(), tc.args...)
			if !errors.Is(err, helpers.ErrLockfileMissing) {
				t.Fatalf("%s with no lockfile: err = %v, want errors.Is helpers.ErrLockfileMissing", tc.name, err)
			}
			if got := exitcode.FromError(err); got != exitcode.ExitLock {
				t.Errorf("exitcode.FromError(err) = %d, want ExitLock (%d)", got, exitcode.ExitLock)
			}
			// The path belongs in the message: "not found" without naming
			// what was looked for leaves an operator guessing which of the
			// default and the --lock-file override was in effect.
			if !strings.Contains(err.Error(), lockfile.DefaultName) {
				t.Errorf("error does not name the lockfile path it looked for: %v", err)
			}
		})
	}
}

// missingLockfileCase is one row of TestMissingLockfileClassifiesAsLockfileError.
type missingLockfileCase struct {
	command func() *cli.Command
	name    string
	args    []string
}

// missingLockfileCases returns one row per command in this package that
// requires a lockfile to exist.
func missingLockfileCases() []missingLockfileCase {
	return []missingLockfileCase{
		{name: "tree", command: Tree},
		{name: "explain", command: Explain, args: []string{"acme.widgets"}},
	}
}

// TestMissingLockfileLeavesHashFallingBack is the documented exception, and
// it is pinned rather than described: hash must still succeed against a
// repository that does not lock, hashing the requirements file instead. It is
// also the control for the rows above - it proves the fixture really is a
// directory with a readable requirements file and no lockfile, so their
// failures are the absence of the lockfile and not a broken fixture.
func TestMissingLockfileLeavesHashFallingBack(t *testing.T) {
	dir := missingLockfileFixture(t)

	got, err := computeHash(filepath.Join(dir, "requirements.yml"), filepath.Join(dir, lockfile.DefaultName))
	if err != nil {
		t.Fatalf("computeHash with no lockfile: %v", err)
	}
	if !strings.HasPrefix(got, "sha256:") {
		t.Errorf("computeHash = %q, want a sha256: prefixed digest of the requirements file", got)
	}
}
