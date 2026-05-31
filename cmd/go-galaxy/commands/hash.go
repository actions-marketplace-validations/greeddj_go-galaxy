package commands

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"

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
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "requirements-file",
				Aliases: []string{"r"},
				Usage:   "Path to requirements.yml",
				Sources: cli.EnvVars("GO_GALAXY_REQUIREMENTS_FILE", "ANSIBLE_GALAXY_REQUIREMENTS_FILE"),
			},
			&cli.StringFlag{
				Name:    "lock-file",
				Usage:   "Path to lockfile (default: requirements.lock.yml beside requirements file)",
				Sources: cli.EnvVars("GO_GALAXY_LOCK_FILE"),
			},
		},
		Action: func(_ context.Context, c *cli.Command) error {
			req := c.String("requirements-file")
			if req == "" {
				req = "requirements.yml"
			}
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

func computeHash(requirementsFile, lockPath string) (string, error) {
	if lf, err := lockfile.Load(lockPath); err == nil {
		hash, err := lf.Hash()
		if err != nil {
			return "", err
		}
		return "sha256:" + hash, nil
	}
	//nolint:gosec // requirementsFile is user-provided.
	data, err := os.ReadFile(requirementsFile)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
