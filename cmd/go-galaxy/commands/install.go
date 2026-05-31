package commands

import (
	"context"
	"net/http"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/fetch"
	"github.com/urfave/cli/v3"
)

// Install returns the CLI command that installs collections from requirements.
func Install() *cli.Command {
	flags := helpers.CollectionFlags()
	flags = append(flags, helpers.S3Flags()...)

	return &cli.Command{
		Name:    "install",
		Aliases: []string{"i"},
		Usage:   "Install collections from requirements file",
		Flags:   flags,
		Action: func(ctx context.Context, c *cli.Command) error {
			return runCollectionCommand(ctx, c, collections.Start)
		},
	}
}

// newHTTPClient builds an HTTP client honoring offline mode.
func newHTTPClient(cfg *config.Config) *http.Client {
	if cfg != nil && cfg.Offline {
		return fetch.NewOffline(cfg.Timeout)
	}
	return fetch.New(cfg.Timeout)
}
