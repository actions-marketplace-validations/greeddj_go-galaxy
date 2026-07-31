package collections

import (
	"context"
	"fmt"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
)

// buildLockfile assembles a lockfile from a resolved set and its graph.
// Artifact SHAs come from version metadata which the resolver has already
// cached, so this only does a metadata fetch (likely a 304 ETag hit) per
// collection rather than downloading tarballs.
func buildLockfile(
	ctx context.Context,
	deps collectionDeps,
	resolved map[string]collection,
	graph map[string][]string,
) (*lockfile.File, error) {
	cfg := deps.cfg
	entries := make([]lockfile.Entry, 0, len(resolved))
	for fqdn, col := range resolved {
		meta, err := loadCollectionMetadata(ctx, deps, col)
		if err != nil {
			return nil, fmt.Errorf("lockfile: %s: %w", fqdn, err)
		}
		// A non-empty sha here is raw Galaxy API JSON a server controls, the
		// same trust boundary resolveArtifactSHA validates - and lock is the
		// command that manufactures the pin every later --frozen install
		// trusts. Rejecting a non-canonical value here, before it is ever
		// written to the lockfile, means a poisoned or lying server can
		// never get its bad digest committed to version control in the
		// first place; catching it only at install time would still be
		// fail-closed, but with the wrong error class for what actually
		// went wrong. An empty sha is left alone: verifyPinnedSHA treats an
		// empty pin as no pin at all, which is what keeps a server that
		// does not publish digests usable, and weakening that here would
		// break every such server.
		sha := strings.TrimSpace(meta.Artifact.Sha256)
		if sha != "" && !helpers.IsSHA256Hex(sha) {
			return nil, fmt.Errorf("lockfile: %s: %w: %q", fqdn, helpers.ErrMalformedArtifactSHA256, sha)
		}
		entries = append(entries, lockfile.Entry{
			Name:    fqdn,
			Version: col.Version,
			Source:  col.Source,
			SHA256:  sha,
			Deps:    lockfileDepsFromGraph(graph, col.key()),
		})
	}
	return &lockfile.File{
		SchemaVersion: lockfile.SchemaVersion,
		Server:        cfg.Server,
		Collections:   entries,
	}, nil
}

func lockfileDepsFromGraph(graph map[string][]string, key string) []string {
	deps := graph[key]
	if len(deps) == 0 {
		return nil
	}
	out := make([]string, 0, len(deps))
	for _, dep := range deps {
		fqdn, _, err := splitCollectionKey(dep)
		if err != nil {
			continue
		}
		out = append(out, fqdn)
	}
	return out
}

// resolveFromLockfile builds resolved/graph maps from a lockfile and
// validates that every requested root is present and constraint-satisfied.
// No HTTP calls are made - this is the offline / --frozen fast path.
func resolveFromLockfile(
	cfg *config.Config,
	lf *lockfile.File,
	prep *rootPreparation,
) (map[string]collection, map[string][]string, error) {
	byFQDN, err := indexLockfile(lf, cfg)
	if err != nil {
		return nil, nil, err
	}
	if err := verifyRootsAgainstLockfile(prep, byFQDN); err != nil {
		return nil, nil, err
	}
	return materializeLockfile(byFQDN)
}

func indexLockfile(lf *lockfile.File, cfg *config.Config) (map[string]lockfile.Entry, error) {
	if lf == nil {
		return nil, fmt.Errorf("%w: nil lockfile", helpers.ErrLockfileInvalid)
	}
	out := make(map[string]lockfile.Entry, len(lf.Collections))
	for _, e := range lf.Collections {
		if _, _, ok := helpers.SplitFQDN(e.Name); !ok {
			return nil, fmt.Errorf("%w: invalid name %q", helpers.ErrLockfileInvalid, e.Name)
		}
		entry := e
		if entry.Source == "" {
			entry.Source = cfg.Server
		}
		out[e.Name] = entry
	}
	return out, nil
}

func verifyRootsAgainstLockfile(prep *rootPreparation, byFQDN map[string]lockfile.Entry) error {
	for _, root := range prep.AllRoots {
		fqdn := fmt.Sprintf("%s.%s", root.Namespace, root.Name)
		entry, ok := byFQDN[fqdn]
		if !ok {
			return fmt.Errorf("%w: root %s missing", helpers.ErrLockfileMismatch, fqdn)
		}
		constraint := root.Constraint
		if constraint == "" {
			constraint = root.Version
		}
		ok, err := constraintSatisfied(entry.Version, constraint)
		if err != nil {
			return fmt.Errorf("%w: root %s: %s", helpers.ErrLockfileMismatch, fqdn, err.Error())
		}
		if !ok {
			return fmt.Errorf("%w: root %s constraint %q not satisfied by lockfile %s",
				helpers.ErrLockfileMismatch, fqdn, constraint, entry.Version)
		}
	}
	return nil
}

func materializeLockfile(byFQDN map[string]lockfile.Entry) (map[string]collection, map[string][]string, error) {
	resolved := make(map[string]collection, len(byFQDN))
	graph := make(map[string][]string, len(byFQDN))
	for fqdn, e := range byFQDN {
		ns, name, ok := helpers.SplitFQDN(fqdn)
		if !ok {
			return nil, nil, fmt.Errorf("%w: %s", helpers.ErrLockfileInvalid, fqdn)
		}
		col := collection{Namespace: ns, Name: name, Version: e.Version, Source: e.Source, SHA256: e.SHA256}
		resolved[fqdn] = col
		graph[col.key()] = lockfileDepsToKeys(e.Deps, byFQDN)
	}
	return resolved, graph, nil
}

func lockfileDepsToKeys(deps []string, byFQDN map[string]lockfile.Entry) []string {
	if len(deps) == 0 {
		return nil
	}
	out := make([]string, 0, len(deps))
	for _, dep := range deps {
		entry, ok := byFQDN[dep]
		if !ok {
			continue
		}
		out = append(out, fmt.Sprintf("%s@%s", dep, entry.Version))
	}
	return out
}

// lockDryRunBaseline loads the lockfile already on disk at path, to serve as
// the "before" side of the diff a dry run reports against a fresh resolve. It
// returns nil - "no baseline" - both when the file does not exist yet and
// when it exists but cannot be loaded, and never fails the run either way.
//
// An absent file is silent: "no lockfile yet" is the ordinary first-lock
// case, and the resulting all-Added report already says so on its own.
//
// A present but unusable file - a bad schema version, unparseable YAML,
// duplicate names, or any other lockfile.Load failure - is warned about
// before being treated as no baseline, because a silent all-Added report
// against a corrupt file would be indistinguishable from one against a
// project that genuinely has no lockfile yet; the warning is what keeps that
// report honest. It is not, however, a reason to fail the preview: a real
// `lock` run never reads this file at all, it only ever overwrites it, so a
// preview refusing to proceed here would invent a failure class the command
// it previews does not have. Warnf, not Printf: it must survive --quiet and
// reach stderr, matching every other dry-run disclosure in this package.
//
// This is `lock`'s own preview policy, specific to a command whose product is
// to replace this file wholesale - it is deliberately not shared with a
// future command that instead consumes the lockfile: that caller has to call
// lockfile.Load itself and fail closed on an unusable file, the same way
// resolveOrLoadLockfile already does for --frozen.
func lockDryRunBaseline(runtime *infra.Infra, path string) *lockfile.File {
	lf, err := lockfile.Load(path)
	if err == nil {
		return lf
	}
	if !lockfile.IsNotExist(err) {
		runtime.Output.Warnf("existing lockfile %s cannot be read (%v); reporting every collection as added", path, err)
	}
	return nil
}

// reportLockfileDiff prints what `lock` would write for lf at path: the
// file-level server line first (if any), then one line per changed
// collection, then a single trailing summary - and nothing else on a diff
// with no changes at all.
//
// Every per-entry line goes through Okf, matching classifyDryRun's own
// register: a server change, an add, an update, and a removal are all work
// the command would do, not a failure - Errorf would misstate that. The
// trailing summary goes through PersistentPrintf, exactly like
// classifyDryRun's own "Dry run: ..." line, with the same counts-first shape
// so the two commands' summaries are eyeball-comparable. Both tiers survive
// --quiet, which is what keeps the preview usable from a job that otherwise
// silences ordinary output.
//
// The summary's verdict term is derived from diff.Empty() itself, never from
// the counts: "lockfile would change" unless diff.Empty() is true, in which
// case "lockfile is up to date". This is deliberately not "any count is
// nonzero", because diff.Empty() also covers diff.Server - a change to lf's
// file-level Server field with every collection otherwise untouched leaves
// every count at its unchanged value, and a verdict built from the counts
// alone would report "up to date" for a run that would still rewrite the
// file. Deriving the verdict from Empty() instead means any future field
// Diff gains cannot reintroduce that bug: whatever makes Empty() false also
// flips the verdict, by construction, with no second place to remember to
// update.
//
// The unchanged count is derived - len(lf.Collections) minus the added and
// updated counts - rather than carried on Diff itself. That subtraction is
// exact only because lf is always buildLockfile's own output: its entries
// come from an fqdn-keyed map, so a name can never repeat within lf, and
// every one of lf's entries is therefore counted by Compare as exactly one of
// Added, Updated, or neither (unchanged) - never more than once.
func reportLockfileDiff(runtime *infra.Infra, lf *lockfile.File, path string, diff lockfile.Diff) {
	if diff.Server != nil {
		runtime.Output.Okf("Would change: %s", renderFieldChange(*diff.Server))
	}
	for _, e := range diff.Added {
		runtime.Output.Okf("Would add: %s@%s", e.Name, e.Version)
	}
	for _, c := range diff.Updated {
		runtime.Output.Okf("Would update: %s (%s)", c.To.Name, renderFieldChanges(c.Fields()))
	}
	for _, e := range diff.Removed {
		runtime.Output.Okf("Would remove: %s@%s", e.Name, e.Version)
	}
	unchanged := len(lf.Collections) - len(diff.Added) - len(diff.Updated)
	verdict := "lockfile would change"
	if diff.Empty() {
		verdict = "lockfile is up to date"
	}
	runtime.Output.PersistentPrintf(
		"Dry run: %s; %d would be added, %d would be updated, %d would be removed, %d unchanged (%s)",
		verdict, len(diff.Added), len(diff.Updated), len(diff.Removed), unchanged, path,
	)
}

// renderFieldChange renders one FieldChange as "version 0.9.0 -> 1.0.0".
func renderFieldChange(f lockfile.FieldChange) string {
	return fmt.Sprintf("%s %s -> %s", f.Field, quoteEmpty(f.From), quoteEmpty(f.To))
}

// renderFieldChanges renders one updated entry's changed fields as
// "version 0.9.0 -> 1.0.0; sha256 (none) -> c101ba4c...". Values are never
// abbreviated, unlike that example: a truncated sha256 can make two different
// digests look identical, which is exactly the difference this report exists
// to surface.
func renderFieldChanges(fields []lockfile.FieldChange) string {
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		parts = append(parts, renderFieldChange(f))
	}
	return strings.Join(parts, "; ")
}

// quoteEmpty renders an empty field value as "(none)" so a line reporting a
// value gained or lost does not read as though it changed into nothing
// visible at all.
func quoteEmpty(v string) string {
	if v == "" {
		return "(none)"
	}
	return v
}
