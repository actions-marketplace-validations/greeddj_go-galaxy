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

// newRootCommand builds the root command. onErr, when non-nil, receives what
// urfave hands ExitErrHandler - an action, before or after failure. A nil
// onErr captures nothing, which is what a caller inspecting only the
// command's shape wants; run passes a real one.
//
// Usage and Description carry two facts a CI author cannot learn anywhere
// else from the binary itself. The first is that a bare go-galaxy installs:
// DefaultCommand makes it so, and nothing printed said as much, which is a
// surprising amount of work for a command someone ran to see what it does.
// The second is the exit-code classes. They exist to be branched on, so a
// pipeline author is exactly who needs them, and until now they appeared only
// in the README. Description is the one field that reaches --help with them,
// since urfave renders a DESCRIPTION block whenever it is non-empty.
//
// Each phrase below is the leading phrase of the matching row in README's
// Exit codes table rather than a fresh wording, so the two cannot come to
// describe the same number differently. The README rows carry the full
// qualifications; this list is the index, not a replacement.
func newRootCommand(onErr func(error)) *cli.Command {
	return &cli.Command{
		Name:  "go-galaxy",
		Usage: "Galaxy Collection Manager for CI; with no command it runs install",
		Description: "With no command, go-galaxy runs install.\n" +
			"\n" +
			"Exit codes:\n" +
			"  0    Success\n" +
			"  1    Generic failure\n" +
			"  2    Usage or configuration error\n" +
			"  3    Dependency resolution failure\n" +
			"  4    Network or Galaxy API failure\n" +
			"  5    Install-time failure\n" +
			"  6    Lockfile error\n" +
			"  7    Artifact-integrity failure\n" +
			"  8    Cache contention\n" +
			"  9    Persisted cache state is corrupt or oversized\n" +
			"  130  Interrupted\n",
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
		ExitErrHandler: func(_ context.Context, _ *cli.Command, err error) {
			if onErr != nil {
				onErr(err)
			}
		},
	}
}

// run configures and executes the CLI, returning the exit code.
func run() int {
	// Customize the version printer to show only the formatted version
	// string (c.Root().Version, set below via helpers.Version). The raw
	// Version global can be empty on dev builds; the formatted string never is.
	cli.VersionPrinter = func(c *cli.Command) {
		_, _ = fmt.Fprintln(c.Writer, c.Root().Version)
	}

	// cmdErr captures action/before/after/flag-action errors via ExitErrHandler.
	// Flag-parse "Incorrect Usage" errors are printed by urfave itself and never
	// reach this handler, so capturing here keeps the print to a single seam.
	var cmdErr error
	app := newRootCommand(func(err error) { cmdErr = err })

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
