package collections

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/galaxy/urlsource"
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
	roles roleResolution,
) (*lockfile.File, error) {
	cfg := deps.cfg
	roleEntries, err := roleLockfileEntries(cfg, roles)
	if err != nil {
		return nil, err
	}
	entries := make([]lockfile.Entry, 0, len(resolved))
	for fqdn, col := range resolved {
		// col.Version crosses the identical snapshot trust boundary the sha
		// guard below does: it comes from a resolved map that a fresh solve
		// always fills with an exact version, but can also come from a
		// persisted snapshot's ResolvedEntry (see buildResolvedSnapshot,
		// deliberately left lenient) reused across a run with an unchanged
		// requirements hash. Rejecting a non-exact value here, before it is
		// ever written to the lockfile, means a poisoned snapshot can never
		// get a constraint string like "*" committed as a pin every later
		// --frozen install would then treat as an unresolved collection and
		// silently install the server's highest version instead. Checked
		// before loadCollectionMetadata, not after: buildCollectionsMap's own
		// validate-before-you-act ordering applies here too, since col.Version
		// is already known and does not need a network round trip to check -
		// a poisoned snapshot entry buys no metadata fetches, under the
		// backend's whole-run exclusive lock, before this fails closed.
		if !helpers.IsExactVersion(col.Version) {
			return nil, fmt.Errorf("lockfile: %s: %w: %q", fqdn, helpers.ErrInvalidCollectionVersion, col.Version)
		}
		if entry, handled, err := sourceLockfileEntry(fqdn, col, graph); handled {
			if err != nil {
				return nil, err
			}
			entries = append(entries, entry)
			continue
		}
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
		// break every such server. This guard cannot be hoisted above
		// loadCollectionMetadata the way the version guard above was: sha
		// comes from meta, which the network call itself produces.
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
		SchemaVersion: lockfile.SchemaVersionFor(entries, roleEntries),
		Server:        cfg.Server,
		Collections:   entries,
		Roles:         roleEntries,
	}, nil
}

// sourceLockfileEntry renders the pin of a collection whose Source is a
// locator - a git or url entry - and reports handled=false for a Galaxy
// collection, whose entry buildLockfile itself renders from server metadata.
func sourceLockfileEntry(fqdn string, col collection, graph map[string][]string) (lockfile.Entry, bool, error) {
	switch {
	case col.isGit():
		entry, err := gitLockfileEntry(fqdn, col, graph)
		return entry, true, err
	case col.isURL():
		entry, err := urlLockfileEntry(fqdn, col, graph)
		return entry, true, err
	default:
		return lockfile.Entry{}, false, nil
	}
}

// gitLockfileEntry renders a git collection's pin: the repository URL as its
// source, the ref the requirements file asked for, the commit it resolved to
// and the subdir it was built from, and no sha256 (see lockfile.Entry for
// why). The locator is taken apart rather than written as-is so the file a
// human reviews names the repository, not an internal key. No metadata fetch
// happens: everything a git entry records was settled at discovery.
func gitLockfileEntry(fqdn string, col collection, graph map[string][]string) (lockfile.Entry, error) {
	loc, err := col.gitLocator()
	if err != nil {
		return lockfile.Entry{}, fmt.Errorf("lockfile: %s: %w", fqdn, err)
	}
	if !loc.Pinned() {
		return lockfile.Entry{}, fmt.Errorf("lockfile: %s: %w: git source is not pinned to a commit", fqdn, helpers.ErrInvalidGitLocator)
	}
	ref := col.Ref
	if ref == "" {
		// A resolved collection that lost its ref on the way here (a snapshot
		// written before refs were recorded) still pins correctly by commit.
		ref = loc.Commit
	}
	return lockfile.Entry{
		Name:    fqdn,
		Type:    lockfile.TypeGit,
		Version: col.Version,
		Source:  loc.URL,
		Ref:     ref,
		Commit:  loc.Commit,
		Subdir:  loc.Subdir,
		Deps:    lockfileDepsFromGraph(graph, col.key()),
	}, nil
}

// urlLockfileEntry renders a url collection's pin: the tarball URL as its
// source and the origin bytes' sha256 - a real digest, required where a git
// entry's is refused (see lockfile.Entry). The locator is taken apart rather
// than written as-is so the file a human reviews names the URL, not an
// internal key. No metadata fetch happens: everything a url entry records
// was settled at discovery.
func urlLockfileEntry(fqdn string, col collection, graph map[string][]string) (lockfile.Entry, error) {
	loc, err := urlSourceOf(col)
	if err != nil {
		return lockfile.Entry{}, fmt.Errorf("lockfile: %s: %w", fqdn, err)
	}
	return lockfile.Entry{
		Name:    fqdn,
		Type:    lockfile.TypeURL,
		Version: col.Version,
		Source:  loc.URL,
		SHA256:  loc.SHA256,
		Deps:    lockfileDepsFromGraph(graph, col.key()),
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
	roots []collection,
) (map[string]collection, map[string][]string, error) {
	byFQDN, err := indexLockfile(lf, cfg)
	if err != nil {
		return nil, nil, err
	}
	if err := verifyRootsAgainstLockfile(roots, byFQDN); err != nil {
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
		if entry.Source == "" && !entry.IsGit() && !entry.IsURL() {
			entry.Source = cfg.Server
		}
		out[e.Name] = entry
	}
	return out, nil
}

func verifyRootsAgainstLockfile(roots []collection, byFQDN map[string]lockfile.Entry) error {
	for _, root := range roots {
		if root.isGit() {
			if err := verifyGitRootAgainstLockfile(root, byFQDN); err != nil {
				return err
			}
			continue
		}
		if root.isURL() {
			if err := verifyURLRootAgainstLockfile(root, byFQDN); err != nil {
				return err
			}
			continue
		}
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
		switch {
		case e.IsGit():
			// The pin is the commit: the locator rebuilt here is what keys the
			// artifact cache and the installed record, and SHA256 stays empty
			// so verifyPinnedSHA has nothing to compare (see lockfile.Entry).
			col.Source = gitsource.Locator{URL: e.Source, Subdir: e.Subdir, Commit: e.Commit}.String()
			col.Type = typeGit
			col.Ref = e.Ref
		case e.IsURL():
			// The pin is the sha256, carried in both the locator (the artifact
			// key) and col.SHA256 (what verifyPinnedSHA compares bytes to).
			col.Source = urlsource.Locator{URL: e.Source, SHA256: e.SHA256}.String()
			col.Type = typeURL
		}
		resolved[fqdn] = col
		graph[col.key()] = lockfileDepsToKeys(e.Deps, byFQDN)
	}
	return resolved, graph, nil
}

// verifyGitRootAgainstLockfile checks one git root, as the requirements file
// wrote it, against the lockfile: at least one git entry must come from the
// same repository URL under the root's subdir (the root's own directory, or
// an immediate child of it, which is how a multi-collection repository
// expands), every such entry must have been locked from the same ref, and a
// root that names its collection must find that very fqdn among them. A ref
// change is a mismatch even when the commit happens to be the same: the
// lockfile records what was asked for, and --frozen means "what was asked for
// has not changed".
func verifyGitRootAgainstLockfile(root collection, byFQDN map[string]lockfile.Entry) error {
	loc, err := root.gitLocator()
	if err != nil {
		return err
	}
	display := helpers.URLForMessage(loc.URL)
	matched := 0
	for fqdn, entry := range byFQDN {
		if !lockedFromGitRoot(entry, loc) {
			continue
		}
		if entry.Ref != root.Ref {
			return fmt.Errorf("%w: git root %s locked from ref %q, requirements ask for %q",
				helpers.ErrLockfileMismatch, display, entry.Ref, root.Ref)
		}
		matched++
		if root.Namespace != "" && fqdn == root.Namespace+"."+root.Name {
			return nil
		}
	}
	return gitRootUnmatched(root, display, matched)
}

// lockedFromGitRoot reports whether entry is a git entry locked from loc's
// repository under loc's subdir (itself or an immediate child).
func lockedFromGitRoot(entry lockfile.Entry, loc gitsource.Locator) bool {
	return entry.IsGit() && entry.Source == loc.URL && subdirWithin(entry.Subdir, loc.Subdir)
}

// gitRootUnmatched is verifyGitRootAgainstLockfile's verdict once the scan
// found no entry naming the root's own fqdn: no entry at all from the
// repository is a mismatch, so is a named root whose fqdn is absent, and an
// unnamed root is satisfied by any matched entry.
func gitRootUnmatched(root collection, display string, matched int) error {
	switch {
	case matched == 0:
		return fmt.Errorf("%w: git root %s has no lockfile entry", helpers.ErrLockfileMismatch, display)
	case root.Namespace != "":
		return fmt.Errorf("%w: git root %s has no lockfile entry for %s.%s",
			helpers.ErrLockfileMismatch, display, root.Namespace, root.Name)
	default:
		return nil
	}
}

// verifyURLRootAgainstLockfile checks one url root, as the requirements file
// wrote it, against the lockfile: exactly the entry locked from the same URL
// must be present, and a version: the root asserted must be the version it
// was locked as - --frozen means "what was asked for has not changed".
func verifyURLRootAgainstLockfile(root collection, byFQDN map[string]lockfile.Entry) error {
	loc, err := root.urlLocator()
	if err != nil {
		return err
	}
	display := helpers.URLForMessage(loc.URL)
	for _, entry := range byFQDN {
		if !entry.IsURL() || entry.Source != loc.URL {
			continue
		}
		if root.Constraint != "" && entry.Version != root.Constraint {
			return fmt.Errorf("%w: url root %s locked as version %q, requirements ask for %q",
				helpers.ErrLockfileMismatch, display, entry.Version, root.Constraint)
		}
		return nil
	}
	return fmt.Errorf("%w: url root %s has no lockfile entry", helpers.ErrLockfileMismatch, display)
}

// subdirWithin reports whether a locked entry's subdir is the root's own
// subdir or an immediate child of it.
func subdirWithin(entrySubdir, rootSubdir string) bool {
	if entrySubdir == rootSubdir {
		return true
	}
	parent := path.Dir(entrySubdir)
	if parent == "." {
		parent = ""
	}
	return parent == rootSubdir
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
// to replace this file wholesale - it is deliberately not shared with
// lockFrozen, which consumes this same on-disk file instead of previewing a
// replacement for it: a command that only ever overwrites the lockfile can
// treat an unusable one as "no baseline yet" without misleading anyone, while
// a command whose entire verdict depends on what is already there cannot -
// see lockFrozen's own doc comment for why it fails closed on the identical
// failure this function forgives.
func lockDryRunBaseline(runtime *infra.Infra, path string) *lockfile.File {
	lf, err := lockfile.Load(path)
	if err == nil {
		return lf
	}
	if !lockfile.IsNotExist(err) {
		runtime.Output.Warnf("Existing lockfile %s cannot be read (%v); reporting every collection as added", path, err)
	}
	return nil
}

// dryRunDiffPrefix and frozenDiffPrefix are reportLockfileDiff's two callers'
// own leading terms for its trailing summary: "Dry run: lockfile would
// change; ..." for lockDryRun's preview, "Frozen: lockfile would change; ..."
// for lockFrozen's drift gate. Declared once here rather than left as bare
// literals at each call site, since both name the same summary line under a
// different mode.
const (
	dryRunDiffPrefix = "Dry run"
	frozenDiffPrefix = "Frozen"
)

// reportLockfileDiff prints what `lock` would write for lf at path: the
// file-level server line first (if any), then one line per changed
// collection, then a single trailing summary carrying the caller-supplied
// prefix - and nothing else on a diff with no changes at all.
//
// prefix names only the trailing summary's leading term, never a per-entry
// line: every "Would add/update/remove/change" line above it reads
// identically regardless of which caller is asking, because the fact each
// one states - what a real `lock` run would do to this collection - is the
// same fact in both registers. Only the summary's own framing differs:
// lockDryRun's "Dry run: ..." states what a real run would additionally do
// on top of this one, while lockFrozen's "Frozen: ..." states what makes
// this run itself fail. Baking the prefix into a shared per-entry line would
// make it counterfactual in one of the two registers.
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
// update. This is also why lockFrozen's own drift error carries no counts of
// its own: a server-only change leaves every count at zero, which the counts
// alone would misreport as "nothing changed".
//
// The unchanged count is derived - len(lf.Collections) minus the added and
// updated counts - rather than carried on Diff itself. That subtraction is
// exact only because lf is always buildLockfile's own output: its entries
// come from an fqdn-keyed map, so a name can never repeat within lf, and
// every one of lf's entries is therefore counted by Compare as exactly one of
// Added, Updated, or neither (unchanged) - never more than once.
func reportLockfileDiff(runtime *infra.Infra, lf *lockfile.File, path, prefix string, diff lockfile.Diff) {
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
	reportRoleDiff(runtime, lf, prefix, diff)
	unchanged := len(lf.Collections) - len(diff.Added) - len(diff.Updated)
	verdict := "lockfile would change"
	if diff.Empty() {
		verdict = "lockfile is up to date"
	}
	runtime.Output.PersistentPrintf(
		"%s: %s; %d would be added, %d would be updated, %d would be removed, %d unchanged (%s)",
		prefix, verdict, len(diff.Added), len(diff.Updated), len(diff.Removed), unchanged, path,
	)
}

// reportRoleDiff renders the roles half of a diff - one line per role, and
// a roles summary - only when the file or the diff has a role, so a
// collections-only run reads exactly as it did before roles existed. The
// unchanged count derives the way the collection count does: lf's roles
// come from a name-keyed map, so each is counted once.
func reportRoleDiff(runtime *infra.Infra, lf *lockfile.File, prefix string, diff lockfile.Diff) {
	if len(lf.Roles) == 0 && !diff.HasRoles() {
		return
	}
	for _, e := range diff.RolesAdded {
		runtime.Output.Okf("Would add role: %s@%s", e.Name, e.Version)
	}
	for _, c := range diff.RolesUpdated {
		runtime.Output.Okf("Would update role: %s (%s)", c.To.Name, renderFieldChanges(c.Fields()))
	}
	for _, e := range diff.RolesRemoved {
		runtime.Output.Okf("Would remove role: %s@%s", e.Name, e.Version)
	}
	unchanged := len(lf.Roles) - len(diff.RolesAdded) - len(diff.RolesUpdated)
	runtime.Output.PersistentPrintf(
		"%s: roles: %d would be added, %d would be updated, %d would be removed, %d unchanged",
		prefix, len(diff.RolesAdded), len(diff.RolesUpdated), len(diff.RolesRemoved), unchanged,
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
