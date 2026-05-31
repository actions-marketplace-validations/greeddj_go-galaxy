package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/commands"
	"github.com/greeddj/go-galaxy/cmd/go-galaxy/helpers"
	"github.com/urfave/cli/v3"
)

//nolint:gochecknoglobals
var (
	Version string
	Commit  string
	Date    string
	BuiltBy string
)

// main is the CLI entry point.
func main() {
	os.Exit(run())
}

// run configures and executes the CLI, returning the exit code.
func run() int {
	// Customize the version printer to show only the version.
	cli.VersionPrinter = func(c *cli.Command) {
		_, _ = fmt.Fprintln(c.Writer, Version)
	}
	app := &cli.Command{
		Name:                   "go-galaxy",
		Usage:                  "Galaxy Collection Manager for CI",
		HideHelpCommand:        true,
		UseShortOptionHandling: true,
		DefaultCommand:         "install",
		Version:                helpers.Version(Version, Commit, Date, BuiltBy),
		Flags:                  helpers.CommonFlags(),
		Commands: []*cli.Command{
			commands.Install(),
			commands.Cleanup(),
			commands.Lock(),
			commands.Warm(),
			commands.Hash(),
			commands.Tree(),
			commands.Explain(),
			commands.Outdated(),
		},
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	defer stop()

	if err := app.Run(ctx, os.Args); err != nil {
		return 1
	}
	return 0
}
