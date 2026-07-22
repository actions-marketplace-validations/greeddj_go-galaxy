package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/urfave/cli/v3"
)

const requirementsYAML = "requirements.yml"

var (
	errExplainNoTarget = errors.New("explain: collection name (ns.name) is required")
	errExplainNotFound = errors.New("collection not found in lockfile")
)

// Explain returns the CLI command that prints why a particular collection
// version was chosen and which other collections depend on it.
func Explain() *cli.Command {
	return &cli.Command{
		Name:      "explain",
		Aliases:   []string{"why"},
		Usage:     "Explain why a collection was resolved to its locked version",
		ArgsUsage: "<namespace.name>",
		Flags:     helpers.LockInspectFlags(),
		Action: func(_ context.Context, c *cli.Command) error {
			if c.NArg() < 1 {
				return errExplainNoTarget
			}
			target := c.Args().First()
			reqPath := c.String("requirements-file")
			if reqPath == "" {
				reqPath = requirementsYAML
			}
			lockPath := lockfile.ResolveDefaultPath(reqPath, c.String("lock-file"))
			lf, err := lockfile.Load(lockPath)
			if err != nil {
				return fmt.Errorf("load lockfile %s: %w", lockPath, err)
			}
			roots, _ := loadRootFQDNs(reqPath)
			rootSet := make(map[string]bool, len(roots))
			for _, r := range roots {
				rootSet[r] = true
			}
			return printExplain(os.Stdout, lf, target, rootSet)
		},
	}
}

func printExplain(w io.Writer, lf *lockfile.File, target string, roots map[string]bool) error {
	entry, rdeps, found := findExplainTarget(lf, target)
	if !found {
		return fmt.Errorf("%w: %s", errExplainNotFound, target)
	}
	printEntryHeader(w, entry)
	printRequiredBy(w, target, rdeps, roots)
	printDepends(w, entry)
	return nil
}

func findExplainTarget(lf *lockfile.File, target string) (lockfile.Entry, []lockfile.Entry, bool) {
	var entry lockfile.Entry
	found := false
	rdeps := make([]lockfile.Entry, 0)
	for _, e := range lf.Collections {
		if e.Name == target {
			entry = e
			found = true
		}
		if slices.Contains(e.Deps, target) {
			rdeps = append(rdeps, e)
		}
	}
	return entry, rdeps, found
}

func printEntryHeader(w io.Writer, entry lockfile.Entry) {
	_, _ = fmt.Fprintf(w, "%s %s\n", entry.Name, entry.Version)
	if entry.Source != "" {
		_, _ = fmt.Fprintf(w, "  source : %s\n", entry.Source)
	}
	if entry.SHA256 != "" {
		_, _ = fmt.Fprintf(w, "  sha256 : %s\n", entry.SHA256)
	}
}

func printRequiredBy(w io.Writer, target string, rdeps []lockfile.Entry, roots map[string]bool) {
	_, _ = fmt.Fprintln(w, "  required by:")
	if roots[target] {
		_, _ = fmt.Fprintln(w, "    - "+requirementsYAML+" (root)")
	}
	sort.Slice(rdeps, func(i, j int) bool { return rdeps[i].Name < rdeps[j].Name })
	for _, r := range rdeps {
		_, _ = fmt.Fprintf(w, "    - %s %s\n", r.Name, r.Version)
	}
	if !roots[target] && len(rdeps) == 0 {
		_, _ = fmt.Fprintln(w, "    - (no parents - orphan in lockfile)")
	}
}

func printDepends(w io.Writer, entry lockfile.Entry) {
	if len(entry.Deps) == 0 {
		return
	}
	deps := make([]string, len(entry.Deps))
	copy(deps, entry.Deps)
	sort.Strings(deps)
	_, _ = fmt.Fprintln(w, "  depends on:")
	for _, d := range deps {
		_, _ = fmt.Fprintf(w, "    - %s\n", d)
	}
}
