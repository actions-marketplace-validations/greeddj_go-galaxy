package cleanup

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"

	"github.com/Masterminds/semver/v3"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/output"
	"github.com/greeddj/go-galaxy/internal/galaxy/requirements"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// buildReachable computes, for every project recorded in the registry, the
// set of installed collection keys (ns.name@version) reachable from that
// project's requirements.yml, transitively through each installed
// collection's own declared dependencies, plus installedByKey - the full
// index of every on-disk copy that removeUnused and sweepLegacyArtifacts
// iterate afterward.
//
// This runs as two full passes over one sorted project-path list
// (projectPaths), never interleaved per project:
//
//   - Phase 1 scans every recorded project's on-disk workspace into the
//     shared installedIndex/installedByKey/depsByKey maps, before any
//     project's requirements are resolved against them.
//   - Phase 2 resolves every recorded project's requirements roots
//     (projectRequirementRoots) against that now-complete index.
//
// The split is load-bearing, not cosmetic. A single-phase implementation
// that scans one project's workspace and immediately resolves that same
// project's roots against the index built so far would only ever see
// collections scanned by projects processed earlier: project A's
// requirements.yml naming a collection only project B installs would find
// nothing whenever B happens to be scanned after A - purely a function of
// iteration order, not of what either project actually declares. Splitting
// into two full passes removes that ordering dependency entirely: every
// root, from every project, is resolved against every project's scanned
// collections, regardless of which project the registry yields first.
//
// projectPaths is sorted (slices.Sorted over the registry's map keys) for
// three reasons, all load-bearing:
//
//   - it makes a phase-2 abort deterministic - the same unreadable project's
//     requirements file names itself in the returned error on every run,
//     rather than whichever project the map's randomized iteration order
//     happened to yield first;
//   - it makes warning order deterministic for the identical reason; and
//   - it lets a test fixture pin a genuine ordering bug with a single run
//     instead of a statistical loop that only sometimes iterates the
//     registry in the order that exposes it.
func buildReachable(runtime *infra.Infra, registry *store.ProjectRegistry) (map[string]bool, map[string][]installedCollection, error) {
	reachable := make(map[string]bool)
	installedIndex := make(map[string][]installedCollection)
	depsByKey := make(map[string]map[string]string)
	// installedByKey accumulates every on-disk copy of a given key
	// (ns.name@version): the same collection can be installed under more
	// than one project's collections path, and every copy must be found so
	// removeUnused can remove all of them in a single run rather than
	// overwriting earlier copies and only shedding one per run.
	installedByKey := make(map[string][]installedCollection)
	// constraints caches each raw requirement constraint string's parsed
	// *semver.Constraints (or nil for an unparseable one) across the whole
	// BFS below, so the same constraint evaluated on many edges is parsed
	// at most once instead of once per edge.
	constraints := make(map[string]*semver.Constraints)

	projectPaths := slices.Sorted(maps.Keys(registry.Projects))

	// Phase 1: every project's workspace into the shared index, before any
	// root resolves against it.
	for _, projectPath := range projectPaths {
		if err := scanProjectWorkspace(
			runtime.Output, projectPath, registry.Projects[projectPath], installedIndex, installedByKey, depsByKey,
		); err != nil {
			return nil, nil, err
		}
	}

	// Phase 2: every recorded project's roots, against the complete index.
	for _, projectPath := range projectPaths {
		roots, err := projectRequirementRoots(runtime.Output, projectPath, registry.Projects[projectPath])
		if err != nil {
			return nil, nil, err
		}
		for _, root := range roots {
			fqdn := fmt.Sprintf("%s.%s", root.Namespace, root.Name)
			for _, inst := range selectInstalled(installedIndex, constraints, fqdn, root.Version) {
				markReachable(inst.Key, reachable, depsByKey, installedIndex, constraints)
			}
		}
	}
	return reachable, installedByKey, nil
}

// projectRequirementRoots loads projectPath's requirements roots for
// reachability. It runs for every recorded project in phase 2, regardless of
// whether that project's own workspace was scanned in phase 1: phase 1 and
// phase 2 are deliberately independent (see buildReachable's own doc
// comment), so a project skipped in phase 1 - an absent or escaping
// workspace - still contributes whatever roots its own requirements.yml
// declares here, potentially keeping another project's on-disk copy alive.
//
// Only two states about a project's requirements are honest, and this
// function distinguishes exactly those two by testing the load failure
// against fs.ErrNotExist: "this project declares nothing" (loadRequirements
// failed with a plain fs.ErrNotExist - zero roots, no error) and "this
// project's declarations are unknown" (loadRequirements failed with
// anything else - the roots it would have contributed cannot be
// determined). The first is a single warning naming the project,
// contributing no roots and letting the run proceed; the second aborts the
// whole run before removeUnused deletes anything, since an unknown set of
// roots could have been protecting any project's on-disk copies, not only
// this one's.
func projectRequirementRoots(
	out output.Printer, projectPath string, project store.ProjectRecord,
) ([]requirements.CollectionRequirement, error) {
	roots, err := loadRequirements(project.RequirementsFile, "")
	if err == nil {
		return roots, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		out.Warnf("project %q: requirements file %q no longer exists; it contributes no reachability roots this run",
			projectPath, project.RequirementsFile)
		return nil, nil
	}
	return nil, fmt.Errorf("%w: %s: %w", helpers.ErrProjectRequirementsUnreadable, project.RequirementsFile, err)
}

// selectInstalled filters installed collections by constraint. constraints
// caches each raw constraint string's parsed *semver.Constraints (or nil for
// an unparseable one) across the whole BFS, so the same requirement string
// evaluated against many edges is parsed at most once. A cache miss is
// distinguished from a cached-nil (parse failure) via the two-value map
// read, so an unparseable constraint is itself parsed only once too.
func selectInstalled(
	index map[string][]installedCollection,
	constraints map[string]*semver.Constraints,
	fqdn, constraint string,
) []installedCollection {
	items := index[fqdn]
	if len(items) == 0 {
		return nil
	}
	normalized := helpers.NormalizeConstraint(constraint)
	if normalized == "" {
		return items
	}
	c, ok := constraints[normalized]
	if !ok {
		c, _ = semver.NewConstraint(normalized)
		constraints[normalized] = c
	}
	if c == nil {
		return items
	}
	out := make([]installedCollection, 0, len(items))
	for _, item := range items {
		if item.Parsed == nil {
			continue
		}
		if c.Check(item.Parsed) {
			out = append(out, item)
		}
	}
	return out
}

// markReachable marks all reachable dependencies starting at key.
func markReachable(
	key string,
	reachable map[string]bool,
	deps map[string]map[string]string,
	index map[string][]installedCollection,
	constraints map[string]*semver.Constraints,
) {
	queue := []string{key}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if reachable[current] {
			continue
		}
		reachable[current] = true
		for depFQDN, constraint := range deps[current] {
			for _, inst := range selectInstalled(index, constraints, depFQDN, constraint) {
				if !reachable[inst.Key] {
					queue = append(queue, inst.Key)
				}
			}
		}
	}
}
