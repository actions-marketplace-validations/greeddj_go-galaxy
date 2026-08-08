package commands

import (
	"context"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/cliflags"
	"github.com/greeddj/go-galaxy/internal/galaxy/cleanup"
	"github.com/urfave/cli/v3"
)

// Cleanup returns the CLI command that removes unused cached collections.
func Cleanup() *cli.Command {
	return &cli.Command{
		Name:    "cleanup",
		Aliases: []string{"c"},
		Usage:   "Cleanup unused cached collections across all projects",
		Flags:   cliflags.S3Flags(),
		Action: func(ctx context.Context, c *cli.Command) error {
			return runCollectionCommand(ctx, c, cleanup.Start)
		},
	}
}
