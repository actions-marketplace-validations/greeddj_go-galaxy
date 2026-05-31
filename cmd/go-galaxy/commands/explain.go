package commands

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"

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
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "requirements-file",
				Aliases: []string{"r"},
				Usage:   "Path to " + requirementsYAML,
				Sources: cli.EnvVars("GO_GALAXY_REQUIREMENTS_FILE", "ANSIBLE_GALAXY_REQUIREMENTS_FILE"),
			},
			&cli.StringFlag{
				Name:    "lock-file",
				Usage:   "Path to lockfile",
				Sources: cli.EnvVars("GO_GALAXY_LOCK_FILE"),
			},
		},
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
			return printExplain(lf, target, rootSet)
		},
	}
}

func printExplain(lf *lockfile.File, target string, roots map[string]bool) error {
	entry, rdeps, found := findExplainTarget(lf, target)
	if !found {
		return fmt.Errorf("%w: %s", errExplainNotFound, target)
	}
	printEntryHeader(entry)
	printRequiredBy(target, rdeps, roots)
	printDepends(entry)
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

func printEntryHeader(entry lockfile.Entry) {
	fmt.Printf("%s %s\n", entry.Name, entry.Version) //nolint:forbidigo
	if entry.Source != "" {
		fmt.Printf("  source : %s\n", entry.Source) //nolint:forbidigo
	}
	if entry.SHA256 != "" {
		fmt.Printf("  sha256 : %s\n", entry.SHA256) //nolint:forbidigo
	}
}

func printRequiredBy(target string, rdeps []lockfile.Entry, roots map[string]bool) {
	fmt.Println("  required by:") //nolint:forbidigo
	if roots[target] {
		fmt.Println("    - " + requirementsYAML + " (root)") //nolint:forbidigo
	}
	sort.Slice(rdeps, func(i, j int) bool { return rdeps[i].Name < rdeps[j].Name })
	for _, r := range rdeps {
		fmt.Printf("    - %s %s\n", r.Name, r.Version) //nolint:forbidigo
	}
	if !roots[target] && len(rdeps) == 0 {
		fmt.Println("    - (no parents — orphan in lockfile)") //nolint:forbidigo
	}
}

func printDepends(entry lockfile.Entry) {
	if len(entry.Deps) == 0 {
		return
	}
	deps := make([]string, len(entry.Deps))
	copy(deps, entry.Deps)
	sort.Strings(deps)
	fmt.Println("  depends on:") //nolint:forbidigo
	for _, d := range deps {
		fmt.Printf("    - %s\n", d) //nolint:forbidigo
	}
}
