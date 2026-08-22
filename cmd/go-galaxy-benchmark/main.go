// Package main is the go-galaxy-benchmark executable. It times
// `ansible-galaxy collection install` against `go-galaxy install` over the
// requirements files in testing/, records every run's wall clock in a JSON
// report, and renders that report as a table or an SVG chart.
//
// Two entry points, and the split is the point: `run` performs the
// measurement and prints the table, `show` re-renders a report already on
// disk and touches neither the network nor the measured binaries. A chart can
// therefore be redrawn without paying for the measurement again.
//
// Wall clock is the only thing recorded. Peak memory, bytes downloaded and
// disk footprint are deliberately absent: they cannot be read off a stopwatch
// around a child process, and inferring them would make the report claim more
// than it measured.
//
// Three measurement rules are worth stating, because each exists for a
// reason that is not obvious from the code alone. Every measured command
// gets a closed stdin, because a tool that decides to prompt is otherwise
// indistinguishable from one that has hung. A failing run is recorded and
// left out of the mean rather than ending the series, so one flaky download
// does not discard the four runs around it. Each tool gets its own cache, its
// own install target and its own temporary directory inside a single working
// tree, so neither tool can warm the other and both do their temporary work
// on the same filesystem.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/greeddj/go-galaxy/internal/progress"
	"github.com/urfave/cli/v3"
)

// Sentinel errors, matched with errors.Is by the caller and by the tests.
var (
	errBinaryMissing       = errors.New("binary not found or not executable")
	errRequirementsMissing = errors.New("requirements file not found")
	errWorkDirRequired     = errors.New("--work-dir is required")
	errSizeInvalid         = errors.New("size must be a positive integer")
	errScenarioUnknown     = errors.New("unknown scenario")
	errFormatUnknown       = errors.New("unknown format")
	errReportEmpty         = errors.New("report contains no results")
)

// main is the CLI entry point.
func main() {
	os.Exit(run())
}

// run assembles the command tree and turns what comes back into an exit code.
// Two codes only: this is a measurement harness, and the caller's question is
// whether it produced a report, not which layer refused.
func run() int {
	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	defer stop()

	app := &cli.Command{
		Name:  "go-galaxy-benchmark",
		Usage: "Time ansible-galaxy against go-galaxy over the testing/ requirements files",
		Commands: []*cli.Command{
			runCommand(),
			showCommand(),
		},
		HideHelpCommand: true,
	}

	if err := app.Run(ctx, os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)

		return 1
	}

	return 0
}

// runCommand declares `run`: measure, write the report, print the table.
func runCommand() *cli.Command {
	return &cli.Command{
		Name:  "run",
		Usage: "Measure both tools and write a JSON report",
		Flags: runFlags(),
		Action: func(ctx context.Context, cmd *cli.Command) error {
			opts, err := optionsFromCLI(cmd)
			if err != nil {
				return err
			}

			report, err := runMeasurement(ctx, opts)
			if err != nil {
				return err
			}

			return renderTable(os.Stdout, report)
		},
	}
}

// runMeasurement owns the progress printer for exactly as long as the
// measurement lasts, and that scope is the point rather than a detail.
//
// The printer draws a spinner on a terminal and repaints its line until it is
// closed. Every line the printer itself writes stops and restarts the spinner
// around the write, so those survive - but the table is written straight to
// stdout and never passes through the printer, so nothing would stop the
// spinner for it. Closing here, before the caller renders anything, is what
// keeps the final spinner frame from being drawn into the table's first line.
func runMeasurement(ctx context.Context, opts options) (*Report, error) {
	out := progress.New(opts.verbose, opts.quiet)
	defer out.Close()

	report, err := measure(ctx, out, opts)
	if err != nil {
		return nil, err
	}

	if err := saveReport(opts.reportPath, report); err != nil {
		return nil, err
	}

	out.Okf("report written to %s", opts.reportPath)

	return report, nil
}

// showCommand declares `show`: render a report that already exists.
func showCommand() *cli.Command {
	return &cli.Command{
		Name:  "show",
		Usage: "Render an existing JSON report as a table or an SVG chart",
		Flags: showFlags(),
		Action: func(_ context.Context, cmd *cli.Command) error {
			report, err := loadReport(cmd.String("report"))
			if err != nil {
				return err
			}

			return renderReport(report, cmd.String("format"), cmd.String("scenario"), cmd.String("out"))
		},
	}
}

// renderReport dispatches on the requested format. The table goes to stdout
// because that is where a shell pipeline expects it; the chart goes to a file
// because a terminal cannot show it.
func renderReport(report *Report, format, scenario, out string) error {
	switch format {
	case formatTable:
		return renderTable(os.Stdout, report)
	case formatSVG:
		return writeSVG(report, scenario, out)
	default:
		return fmt.Errorf("%w: %q (want %s or %s)", errFormatUnknown, format, formatTable, formatSVG)
	}
}
