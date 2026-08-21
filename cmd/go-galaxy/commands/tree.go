package commands

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"slices"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/cliflags"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/galaxy/requirements"
	"github.com/greeddj/go-galaxy/internal/safeout"
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
		Flags:   cliflags.LockInspectFlags(),
		Action: func(_ context.Context, c *cli.Command) error {
			reqPath := c.String("requirements-file")
			lockPath := lockfile.ResolveDefaultPath(reqPath, c.String("lock-file"))
			lf, err := lockfile.LoadRequired(lockPath)
			if err != nil {
				return err
			}
			roots, err := loadRootFQDNs(reqPath, lf)
			if err != nil {
				return err
			}
			printTree(os.Stdout, reqPath, lf, roots)
			return nil
		},
	}
}

// loadRootFQDNs lists the collections the requirements file names as roots.
// A Galaxy entry names one directly. A git entry names whatever its
// repository held, which only the lockfile knows: every git entry locked from
// the same repository under the entry's subdir (or an immediate child of it,
// the multi-collection shape) is a root, or just the one the entry named
// explicitly. A git entry the lockfile holds nothing for is reported under
// its locator text, so the tree shows it as missing rather than dropping it.
func loadRootFQDNs(reqPath string, lf *lockfile.File) ([]string, error) {
	reqs, _, err := requirements.LoadCollections(reqPath, "")
	if err != nil {
		return nil, fmt.Errorf("load requirements %s: %w", reqPath, err)
	}
	out := make([]string, 0, len(reqs))
	for _, r := range reqs {
		if r.IsGit() {
			out = append(out, gitRootFQDNs(r, lf)...)
			continue
		}
		out = append(out, fmt.Sprintf("%s.%s", r.Namespace, r.Name))
	}
	return out, nil
}

func gitRootFQDNs(r requirements.CollectionRequirement, lf *lockfile.File) []string {
	var out []string
	if lf != nil {
		for _, e := range lf.Collections {
			if !e.IsGit() || e.Source != r.Source || !gitSubdirWithin(e.Subdir, r.Subdir) {
				continue
			}
			if r.Name != "" && e.Name != r.Namespace+"."+r.Name {
				continue
			}
			out = append(out, e.Name)
		}
	}
	if len(out) == 0 {
		if r.Name != "" {
			return []string{r.Namespace + "." + r.Name}
		}
		return []string{gitsource.Locator{URL: r.Source, Subdir: r.Subdir}.String()}
	}
	return out
}

// gitSubdirWithin reports whether a locked entry's subdir is the requirement's
// own subdir or an immediate child of it.
func gitSubdirWithin(entrySubdir, rootSubdir string) bool {
	if entrySubdir == rootSubdir {
		return true
	}
	parent := path.Dir(entrySubdir)
	if parent == "." {
		parent = ""
	}
	return parent == rootSubdir
}

// printTree writes the header line (the actual requirements path passed in,
// not a hardcoded name) followed by the dependency tree rooted at each entry
// in roots.
//
// w is wrapped in safeout.NewWriter as the first statement so every write
// this function and the helpers it calls (walkTree) make is sanitized,
// regardless of what a lockfile entry's Name/Version/Deps contain - a
// lockfile can be edited by hand or reach this command from an untrusted
// source, and its fields are otherwise printed verbatim.
func printTree(w io.Writer, reqPath string, lf *lockfile.File, roots []string) {
	w = safeout.NewWriter(w)
	byFQDN := make(map[string]lockfile.Entry, len(lf.Collections))
	for _, e := range lf.Collections {
		byFQDN[e.Name] = e
	}
	sortedRoots := slices.Sorted(slices.Values(roots))

	_, _ = fmt.Fprintln(w, reqPath)
	for i, root := range sortedRoots {
		isLast := i == len(sortedRoots)-1
		walkTree(w, byFQDN, root, "", isLast, make(map[string]bool))
	}
}

func walkTree(w io.Writer, by map[string]lockfile.Entry, fqdn, prefix string, isLast bool, seen map[string]bool) {
	branch := "├── "
	cont := "│   "
	if isLast {
		branch = "└── "
		cont = "    "
	}
	entry, ok := by[fqdn]
	if !ok {
		_, _ = fmt.Fprintf(w, "%s%s%s (missing in lockfile)\n", prefix, branch, fqdn)
		return
	}
	if seen[fqdn] {
		_, _ = fmt.Fprintf(w, "%s%s%s %s (*)\n", prefix, branch, fqdn, entry.Version)
		return
	}
	seen[fqdn] = true
	_, _ = fmt.Fprintf(w, "%s%s%s %s%s\n", prefix, branch, fqdn, entry.Version, gitOrigin(entry))

	deps := slices.Sorted(slices.Values(entry.Deps))
	for i, dep := range deps {
		walkTree(w, by, dep, prefix+cont, i == len(deps)-1, seen)
	}
}

// gitOrigin renders a git entry's provenance for the tree: the repository,
// the subdir when one is set, and the commit. Empty for a Galaxy entry.
func gitOrigin(entry lockfile.Entry) string {
	if !entry.IsGit() {
		return ""
	}
	subdir := ""
	if entry.Subdir != "" {
		subdir = "#" + entry.Subdir
	}
	return fmt.Sprintf(" (git %s%s @%s)", entry.Source, subdir, entry.Commit)
}
