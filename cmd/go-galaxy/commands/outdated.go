package commands

import (
	"context"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/cliflags"
	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/urfave/cli/v3"
)

// Outdated returns the CLI command that lists collections in the lockfile
// whose latest available Galaxy version differs from the locked version.
func Outdated() *cli.Command {
	flags := cliflags.CollectionFlags()
	flags = append(flags, cliflags.S3Flags()...)

	return &cli.Command{
		Name:    "outdated",
		Aliases: []string{"o"},
		Usage:   "Compare lockfile entries against the latest versions on Galaxy",
		Flags:   flags,
		Action: func(ctx context.Context, c *cli.Command) error {
			return runCollectionCommand(ctx, c, collections.Outdated)
		},
	}
}
