package commands

import (
	"context"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/urfave/cli/v3"
)

// Warm returns the CLI command that downloads and extracts collections
// into the cache without populating the install path. Intended for
// CI image bake: subsequent install runs hardlink instantly.
func Warm() *cli.Command {
	flags := helpers.CollectionFlags()
	flags = append(flags, helpers.S3Flags()...)

	return &cli.Command{
		Name:    "warm",
		Aliases: []string{"w"},
		Usage:   "Download and extract collections into cache without installing",
		Flags:   flags,
		Action: func(ctx context.Context, c *cli.Command) error {
			return runCollectionCommand(ctx, c, func(ctx context.Context, cfg *config.Config, runtime *infra.Infra) error {
				cfg.WarmOnly = true
				return collections.Warm(ctx, cfg, runtime)
			})
		},
	}
}
