package commands

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/cliflags"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/urfave/cli/v3"
)

// Hash returns the CLI command that prints a deterministic CI cache key.
//
// The key is the canonical lockfile hash when a lockfile is present
// (preferred for reproducible CI), otherwise it falls back to the SHA256
// of the requirements file. Output goes to stdout as `sha256:<hex>` on a
// single line, suitable for capture in CI: `KEY=$(go-galaxy hash)`.
func Hash() *cli.Command {
	return &cli.Command{
		Name:    "hash",
		Aliases: []string{"h"},
		Usage:   "Print a deterministic cache key for CI (sha256 of lockfile or requirements)",
		Flags:   cliflags.LockInspectFlags(),
		Action: func(_ context.Context, c *cli.Command) error {
			req := c.String("requirements-file")
			lockPath := lockfile.ResolveDefaultPath(req, c.String("lock-file"))
			key, err := computeHash(req, lockPath)
			if err != nil {
				return err
			}
			fmt.Println(key) //nolint:forbidigo // hash is the command's only output.
			return nil
		},
	}
}

// computeHash prefers the lockfile's canonical hash when one is present. A
// missing lockfile falls back to hashing the requirements file (the historic
// behavior, kept for repos that do not lock). Any other lockfile.Load
// failure - the file exists but cannot be parsed or validated, whatever the
// specific cause - is surfaced as an error instead of silently falling back,
// since that would hide a broken lockfile behind a hash that looks fine.
func computeHash(requirementsFile, lockPath string) (string, error) {
	lf, err := lockfile.Load(lockPath)
	switch {
	case err == nil:
		hash, hashErr := lf.Hash()
		if hashErr != nil {
			return "", hashErr
		}
		return "sha256:" + hash, nil
	case lockfile.IsNotExist(err):
		// No lockfile: fall through to hashing the requirements file below.
	default:
		return "", err
	}

	//nolint:gosec // requirementsFile is user-provided.
	data, err := os.ReadFile(requirementsFile)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
