package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/commands"
	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Deliberately omit syscall.SIGQUIT from this notify set: leaving it
	// unhandled restores Go's runtime default of dumping all goroutine stacks,
	// which is the standard way to diagnose a hung CI run.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigCh)

	// caught records the signal (if any) that triggered cancellation, so run()
	// can report a signal-specific exit code even though ctx itself only carries
	// context.Canceled.
	var caught atomic.Pointer[os.Signal]
	done := make(chan struct{})
	go func() {
		select {
		case s := <-sigCh:
			caught.Store(&s)
			cancel()
		case <-done:
		}
	}()

	runErr := app.Run(ctx, os.Args)
	close(done)

	var sig os.Signal
	if p := caught.Load(); p != nil {
		sig = *p
	}

	code, printErr := handleResult(runErr, cmdErr, sig)
	if printErr != nil {
		progress.Errorf("%s", printErr.Error())
	}
	return code
}

// handleResult decides the process exit code and the error, if any, that
// still needs printing. A caught signal takes precedence over everything
// else (the process is being asked to stop, not to report a business
// error); otherwise capturedErr (from ExitErrHandler) wins since it carries
// the actual error value, classified via exitcode.FromError. A bare runErr
// with no capturedErr means urfave already printed a flag-parse usage error
// itself, so only the exit code is reported.
func handleResult(runErr, capturedErr error, sig os.Signal) (int, error) {
	if sig != nil {
		return exitcode.FromSignal(sig), nil
	}
	if capturedErr != nil {
		return exitcode.FromError(capturedErr), capturedErr
	}
	if runErr != nil {
		return exitcode.ExitUsage, nil
	}
	return exitcode.ExitOK, nil
}
