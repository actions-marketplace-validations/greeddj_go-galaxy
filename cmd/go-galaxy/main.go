package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/commands"
	"github.com/greeddj/go-galaxy/cmd/go-galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/progress"
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

	// cmdErr captures action/before/after/flag-action errors via ExitErrHandler.
	// Flag-parse "Incorrect Usage" errors are printed by urfave itself and never
	// reach this handler, so capturing here keeps the print to a single seam.
	var cmdErr error
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
		ExitErrHandler: func(_ context.Context, _ *cli.Command, err error) { cmdErr = err },
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	defer stop()

	runErr := app.Run(ctx, os.Args)
	code, printErr := handleResult(runErr, cmdErr)
	if printErr != nil {
		progress.Errorf("%s", printErr.Error())
	}
	return code
}

// handleResult decides the process exit code and the error, if any, that
// still needs printing. capturedErr (from ExitErrHandler) takes precedence
// since it carries the actual error value; a bare runErr with no capturedErr
// means urfave already printed a flag-parse usage error itself.
func handleResult(runErr, capturedErr error) (int, error) {
	if capturedErr != nil {
		return 1, capturedErr
	}
	if runErr != nil {
		return 1, nil
	}
	return 0, nil
}
