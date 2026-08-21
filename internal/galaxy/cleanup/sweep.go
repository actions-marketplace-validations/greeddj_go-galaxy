package cleanup

import (
	"context"
	"fmt"
	"maps"
	"net/url"
	"slices"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/extracted"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
)

// legacyArtifactKey builds the pre-multi-server-scoped artifact cache key:
// the flat, percent-encoded-filename-only shape every artifactKey in this
// codebase used to build before it gained a server-fingerprint prefix (see
// helpers.ArtifactKey). No code path ever builds this shape for a fresh
// install anymore - collections.artifactKey and this package's own current
// key both go through helpers.ArtifactKey now - so a cache entry still
// living under this key can never again be reached by a cache-hit lookup,
// regardless of whether its collection is still reachable. sweepLegacyArtifacts
// is the only remaining caller, purging it unconditionally as a one-time
// migration cleanup rather than leaving it as permanent orphaned disk usage.
//
// The filename literal here deliberately does not call
// helpers.ArtifactFilename: this key must keep matching the bytes old
// versions actually wrote, so it stays frozen even if the live filename
// contract ever changes.
func legacyArtifactKey(namespace, name, version string) string {
	filename := fmt.Sprintf("%s-%s-%s.tar.gz", namespace, name, version)
	return url.QueryEscape(filename)
}

// sweepLegacyArtifacts removes every discovered installed collection's
// artifact cached under the pre-multi-server flat key shape
// (legacyArtifactKey), independent of reachability: a still-reachable
// collection keeps its workspace and its current, server-scoped artifact
// cache entry (helpers.ArtifactKey) untouched, but any copy still cached
// under the retired flat key is unconditionally dead weight, since nothing
// will ever look it up again. removeUnused's own artifact purge only ever
// runs for a collection it is also removing, so without this separate pass a
// still-reachable collection's legacy-keyed tarball would never be reclaimed
// by any code path. In a dry run this only reports candidates that actually
// exist on disk, matching sweepExtractedStore's own dry-run reporting
// convention.
func sweepLegacyArtifacts(
	ctx context.Context,
	cfg *config.Config,
	runtime *infra.Infra,
	backend cacheManager.Backend,
	installedByKey map[string][]installedCollection,
) {
	artifacts := backend.Artifacts()
	if artifacts == nil {
		return
	}
	// Sorted rather than ranged directly, mirroring removeUnused's own
	// ordering fix: a Delete failure is ignored below rather than propagated,
	// so map order cannot randomize which entries a completed pass sweeps -
	// only the order dry-run report lines are printed in, and the order a
	// test fixture asserting on those lines would otherwise have to tolerate.
	// A pass cut short by the context check below stops on a prefix of that
	// same order rather than on an arbitrary subset.
	for _, mapKey := range slices.Sorted(maps.Keys(installedByKey)) {
		// Stopping silently suffices for a pass that is void by design: on
		// the common path the caller has already returned an error
		// (cleanupWithState reads the same context immediately before
		// calling this), and a context that ends inside this loop instead is
		// still judged once the work returns - runCleanup hands
		// cleanupWithState's outcome to cacheManager.LockLostError, which
		// turns even a nil into the lock-loss verdict whenever the holder
		// context ended because another holder took the cache. A plain
		// cancellation costs at most a deferred purge: what this pass
		// reclaims is disk under a key shape nothing can look up again,
		// never correctness.
		if ctx.Err() != nil {
			return
		}
		insts := installedByKey[mapKey]
		if len(insts) == 0 {
			continue
		}
		// Every on-disk copy of the same key shares the same namespace/name/
		// version, and therefore the same legacy key, regardless of which
		// project installed it - so only the first copy needs inspecting.
		inst := insts[0]
		legacyKey := legacyArtifactKey(inst.Namespace, inst.Name, inst.Version)
		// A walked namespace directory may legally contain "." (nothing in
		// this codebase's scan rejects it - see buildInstalledRecord's
		// IsPathElement check, which permits it), and legacyArtifactKey's
		// url.QueryEscape leaves both "." and "-" unescaped. A namespace like
		// "<12-hex>.acme" therefore forges a legacyArtifactKey byte-identical
		// to a genuine, current helpers.ArtifactKey entry scoped to a server
		// whose fingerprint happens to be that same 12-hex prefix - a
		// collision this pass has no reachability check or Source
		// requirement to catch, since sweepLegacyArtifacts runs
		// unconditionally over every scanned collection. Skipping any
		// candidate that already carries ArtifactKey's own fingerprint prefix
		// closes that: this pass only ever purges a key that could not also
		// be a live, server-scoped cache slot.
		if helpers.IsScopedArtifactKey(legacyKey) {
			continue
		}
		if cfg.DryRun {
			reportLegacyArtifactSweepCandidate(ctx, runtime, artifacts, legacyKey)
			continue
		}
		_ = artifacts.Delete(ctx, legacyKey)
	}
}

// reportLegacyArtifactSweepCandidate prints a dry-run sweep line for key only
// when it actually exists, so a dry-run report is not flooded with a line for
// every installed collection regardless of whether it ever had a
// legacy-keyed cache entry in the first place. A Has error is treated as "not
// present" - conservative for a report that must never claim more than it
// can verify.
func reportLegacyArtifactSweepCandidate(ctx context.Context, runtime *infra.Infra, artifacts cacheManager.ArtifactStore, key string) {
	has, err := artifacts.Has(ctx, key)
	if err != nil || !has {
		return
	}
	// key needs no quoting here: legacyArtifactKey builds it via
	// url.QueryEscape(filename), which percent-encodes every control byte
	// (measured: url.QueryEscape("a\nb.tar.gz") == "a%0Ab.tar.gz") on top of
	// namespace/name/version already being IsPathElement-validated by
	// buildInstalledRecord before a key is ever built from them. A future
	// author must not "fix" this into %q to match the extracted-entry line
	// below: that line's name comes from a raw directory listing with no
	// escaping step of its own, which is exactly what makes it different.
	runtime.Output.Printf("🧹 would sweep legacy artifact %s", key)
}

// sweepExtractedStore drops content-addressable extracted entries whose SHA
// is not referenced by any entry in the persisted snapshot's Installed set,
// nor by a still-fresh entry in its Warmed set. The snapshot - not an on-disk
// workspace scan - is the correct source of truth here: in a real run,
// removeUnused has already pruned it down to installed entries that are
// either still reachable or belong to a project whose workspace was absent
// this run (and so was never scanned or pruned at all). An on-disk scan
// would see an empty keep set for every absent workspace, which is the
// normal ephemeral-CI state, and would wipe the entire extracted cache. A
// warmed entry is the only evidence a warm-only machine - no project
// workspace exists at all, so scanProjectWorkspace skips the project entirely
// and it never contributes to installedByKey/reachable either - still wants
// its extracted trees; it expires purely by age (helpers.WarmedEntryMaxAge).
//
// In a dry run, removeUnused does not prune the snapshot (it only reports
// what it would remove), so the snapshot still contains the about-to-be-
// removed keys. To report the sweep accurately, their SHAs are excluded from
// keep here via reachable/installedByKey - the same two values removeUnused
// used to decide what it would remove - so the reported plan matches what a
// real run would actually do. This exclusion is a no-op in a real run, since
// those keys are already absent from the snapshot by the time this runs.
//
// The !st.WasPersisted() guard lives here, inside the function, rather than
// at the Start call site, so that a future second caller of sweepExtractedStore
// inherits it automatically instead of having to remember to repeat it. It
// runs before the cfg.DryRun branch so a dry run reports no sweep candidates
// that a real run would not act on either. Without it, an absent workspace
// plus a never-loaded (or dropped/expired) snapshot hands this function an
// empty keep set indistinguishable from "nothing is installed or warmed
// anywhere", which would wipe the entire extracted store on the strength of
// having no evidence at all.
func sweepExtractedStore(
	ctx context.Context,
	cfg *config.Config,
	runtime *infra.Infra,
	st *store.Store,
	reachable map[string]bool,
	installedByKey map[string][]installedCollection,
	roles roleReachability,
) {
	if cfg == nil || cfg.CacheDir == "" || st == nil || !st.HasRecordedContent() {
		return
	}
	extractedStore := extracted.NewStore(cfg.CacheDir)
	if extractedStore == nil {
		return
	}
	keep := extractedKeepSet(st, reachable, installedByKey)
	maps.Copy(keep, roleKeepSHAs(st, roles.reachable, roles.byName))

	if cfg.DryRun {
		reportExtractedSweepPlan(runtime, extractedStore, keep)
		return
	}
	// Reported, not discarded, and still not fatal. The sweep reclaims disk in
	// a rebuildable layer, so one unreadable entry must not fail a cleanup run
	// that has already done its real work; but the same return also carries
	// the containment root's refusal when the store directory itself leads out
	// of the cache directory, and ctx's own error when this run stopped owning
	// the cache partway through the entries - and silence for either would
	// leave an operator with a run that reports success while reclaiming
	// nothing.
	if err := extractedStore.Sweep(ctx, keep); err != nil {
		runtime.Output.Errorf("failed to sweep the extracted cache: %v", err)
	}
}

// extractedKeepSet builds the set of extracted-store SHAs to keep: installed-
// and-still-referenced union warmed-and-still-fresh. The installed half
// excludes any key that removeUnused would remove (or already removed, in a
// real run) so a dry-run report matches what a real run would actually
// sweep. The warmed half is unioned in unconditionally: it is deliberately not
// subject to the installed half's wouldRemove exclusion, since a key that is
// both installed-unreachable and warmed must still keep its tree - warm's
// intent is independent of install reachability. The two loops below are pure
// additions to the same set, so their relative order is irrelevant; what is
// load-bearing is that the warmed loop never consults wouldRemove.
func extractedKeepSet(
	st *store.Store,
	reachable map[string]bool,
	installedByKey map[string][]installedCollection,
) map[string]bool {
	wouldRemove := make(map[string]bool, len(installedByKey))
	for key := range installedByKey {
		if !reachable[key] {
			wouldRemove[key] = true
		}
	}

	shaByKey := st.InstalledArtifactSHAByKey()
	warmedByKey := st.WarmedArtifactSHAByKey()
	keep := make(map[string]bool, len(shaByKey)+len(warmedByKey))
	for key, sha := range shaByKey {
		if wouldRemove[key] {
			continue
		}
		keep[sha] = true
	}
	for _, sha := range warmedByKey {
		keep[sha] = true
	}
	return keep
}

// reportExtractedSweepPlan prints, without deleting anything, the extracted
// entries a real run would sweep given keep.
func reportExtractedSweepPlan(runtime *infra.Infra, extractedStore *extracted.Store, keep map[string]bool) {
	plan, err := extractedStore.SweepPlan(keep)
	if err != nil {
		runtime.Output.Errorf("failed to plan extracted cache sweep: %v", err)
		return
	}
	for _, name := range plan {
		// name is entry.Name() from a raw directory listing of the extracted
		// store root (extracted.Store.SweepPlan) - content this program did
		// not itself validate, unlike a legacy artifact key (see
		// reportLegacyArtifactSweepCandidate) or an install key (see
		// removeUnused), both of which are assembled solely from
		// IsPathElement-validated components. A local writer able to plant a
		// directory there controls this string outright, so it is rendered
		// %q rather than %s.
		runtime.Output.Printf("🧹 would sweep extracted %q", name)
	}
}
