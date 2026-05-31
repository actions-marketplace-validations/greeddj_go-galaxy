package commands

import (
	"context"
	"fmt"
	"sort"

	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/galaxy/requirements"
	"github.com/urfave/cli/v3"
)

// Tree returns the CLI command that prints the dep tree from a lockfile.
//
// Output layout (similar to `cargo tree` / `pip show --tree`):
//
//	requirements.yml
//	├── community.general 11.5.0
//	│   └── ansible.posix 2.0.0
//	└── ansible.utils 6.0.2
func Tree() *cli.Command {
	return &cli.Command{
		Name:    "tree",
		Aliases: []string{"t"},
		Usage:   "Print the resolved dependency tree from the lockfile",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "requirements-file",
				Aliases: []string{"r"},
				Usage:   "Path to requirements.yml",
				Sources: cli.EnvVars("GO_GALAXY_REQUIREMENTS_FILE", "ANSIBLE_GALAXY_REQUIREMENTS_FILE"),
			},
			&cli.StringFlag{
				Name:    "lock-file",
				Usage:   "Path to lockfile (default: requirements.lock.yml beside requirements file)",
				Sources: cli.EnvVars("GO_GALAXY_LOCK_FILE"),
			},
		},
		Action: func(_ context.Context, c *cli.Command) error {
			reqPath := c.String("requirements-file")
			if reqPath == "" {
				reqPath = "requirements.yml"
			}
			lockPath := lockfile.ResolveDefaultPath(reqPath, c.String("lock-file"))
			lf, err := lockfile.Load(lockPath)
			if err != nil {
				return fmt.Errorf("load lockfile %s: %w", lockPath, err)
			}
			roots, err := loadRootFQDNs(reqPath)
			if err != nil {
				return err
			}
			printTree(lf, roots)
			return nil
		},
	}
}

func loadRootFQDNs(reqPath string) ([]string, error) {
	reqs, _, err := requirements.LoadCollections(reqPath, "")
	if err != nil {
		return nil, fmt.Errorf("load requirements %s: %w", reqPath, err)
	}
	out := make([]string, 0, len(reqs))
	for _, r := range reqs {
		out = append(out, fmt.Sprintf("%s.%s", r.Namespace, r.Name))
	}
	return out, nil
}

func printTree(lf *lockfile.File, roots []string) {
	byFQDN := make(map[string]lockfile.Entry, len(lf.Collections))
	for _, e := range lf.Collections {
		byFQDN[e.Name] = e
	}
	sortedRoots := make([]string, len(roots))
	copy(sortedRoots, roots)
	sort.Strings(sortedRoots)

	fmt.Println("requirements.yml") //nolint:forbidigo // command output
	for i, root := range sortedRoots {
		isLast := i == len(sortedRoots)-1
		walkTree(byFQDN, root, "", isLast, make(map[string]bool))
	}
}

func walkTree(by map[string]lockfile.Entry, fqdn, prefix string, isLast bool, seen map[string]bool) {
	branch := "├── "
	cont := "│   "
	if isLast {
		branch = "└── "
		cont = "    "
	}
	entry, ok := by[fqdn]
	if !ok {
		fmt.Printf("%s%s%s (missing in lockfile)\n", prefix, branch, fqdn) //nolint:forbidigo
		return
	}
	if seen[fqdn] {
		fmt.Printf("%s%s%s %s (*)\n", prefix, branch, fqdn, entry.Version) //nolint:forbidigo
		return
	}
	seen[fqdn] = true
	fmt.Printf("%s%s%s %s\n", prefix, branch, fqdn, entry.Version) //nolint:forbidigo

	deps := make([]string, len(entry.Deps))
	copy(deps, entry.Deps)
	sort.Strings(deps)
	for i, dep := range deps {
		walkTree(by, dep, prefix+cont, i == len(deps)-1, seen)
	}
}
