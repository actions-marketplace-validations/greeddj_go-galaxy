package collections

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"sort"
	"strings"
	"sync"

	"github.com/Masterminds/semver/v3"
	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/psvmcc/hub/pkg/types"
)

// resolveTask describes a dependency resolution task.
type resolveTask struct {
	FQDN              string
	Namespace         string
	Name              string
	Source            string
	Constraints       []string
	ConstraintSources []constraintSource
}

// resolveResult captures the outcome of resolving one collection.
type resolveResult struct {
	Err       error
	Deps      map[string]string
	FQDN      string
	Namespace string
	Name      string
	Source    string
	Version   string
}

// resolveCollectionsInternal resolves versions and dependencies for roots.
func resolveCollectionsInternal(
	ctx context.Context,
	deps collectionDeps,
	roots []collection,
	allowSnapshot bool,
	record bool,
) (map[string]collection, map[string][]string, error) {
	cfg := deps.cfg
	st := deps.st
	if cfg.NoDeps {
		return resolveWithoutDeps(ctx, deps, roots, record)
	}

	reqSpec := buildRequirementsSpec(cfg, roots)
	reqHash := requirementsSignatureFromSpec(reqSpec)

	snapshotAllowed := allowSnapshot && st != nil
	if snapshotAllowed {
		resolvedSnap, graphSnap, ok, err := resolveFromSnapshots(ctx, deps, roots, reqSpec, reqHash)
		if shouldReturnSnapshot(ok, err) {
			return resolvedSnap, graphSnap, err
		}
	}

	state, err := newResolverState(cfg, roots)
	if err != nil {
		return nil, nil, err
	}
	if err := state.resolveQueue(ctx, deps); err != nil {
		return nil, nil, err
	}
	resolved, graph, err := state.buildGraph(roots)
	if err != nil {
		return nil, nil, err
	}
	recordResolutionIfNeeded(st, record, resolved, graph, reqHash, cfg.Server, reqSpec)
	return resolved, graph, nil
}

func shouldReturnSnapshot(ok bool, err error) bool {
	return ok || err != nil
}

func recordResolutionIfNeeded(
	st *store.Store,
	record bool,
	resolved map[string]collection,
	graph map[string][]string,
	reqHash string,
	server string,
	reqSpec map[string]requirementSpec,
) {
	if !record || st == nil {
		return
	}
	recordResolution(st, resolved, graph, reqHash, server, reqSpec)
}

type resolverState struct {
	cfg            *config.Config
	resolved       map[string]collection
	depsByParent   map[string]map[string]string
	depConstraints map[string]map[string]string
	sourceByFQDN   map[string]string
	queued         map[string]bool
	queue          []string
}

// resolveWithoutDeps resolves every root to a single concrete version without
// following any dependency edges - the --no-deps fast path. Each root is
// resolved independently and concurrently via resolveTaskVersion: an
// already-pinned root returns immediately with zero I/O, while an unpinned or
// ranged root goes through the same root-metadata/version-selection helpers
// the normal (deps-following) resolver uses. This keeps the resulting
// version - and therefore the artifact cache key, the extracted-store key,
// and any lockfile entry derived from it - a concrete version rather than a
// literal "*" (or other non-exact) constraint.
func resolveWithoutDeps(
	ctx context.Context,
	deps collectionDeps,
	roots []collection,
	record bool,
) (map[string]collection, map[string][]string, error) {
	cfg := deps.cfg
	st := deps.st

	// versions/errs are pre-sized and index-aligned with roots: each slot is
	// written by exactly one goroutine (its own root's index), so no mutex is
	// needed despite the slices being shared across the worker pool.
	versions := make([]string, len(roots))
	errs := make([]error, len(roots))

	workers := max(cfg.Workers, 1)
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for i, root := range roots {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			versions[i], errs[i] = resolveRootVersion(ctx, deps, root)
		})
	}
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			return nil, nil, err
		}
	}

	resolved := make(map[string]collection, len(roots))
	graph := make(map[string][]string, len(roots))
	for i, root := range roots {
		fqdn := fmt.Sprintf("%s.%s", root.Namespace, root.Name)
		source := root.Source
		if source == "" {
			source = cfg.Server
		}
		col := collection{Namespace: root.Namespace, Name: root.Name, Version: versions[i], Source: source}
		resolved[fqdn] = col
		graph[col.key()] = nil
	}

	if record && st != nil {
		spec := buildRequirementsSpec(cfg, roots)
		recordResolution(st, resolved, graph, requirementsSignatureFromSpec(spec), cfg.Server, spec)
	}
	return resolved, graph, nil
}

// resolveRootVersion builds a single-constraint resolveTask for root and
// resolves it to a concrete version via resolveTaskVersion.
func resolveRootVersion(ctx context.Context, deps collectionDeps, root collection) (string, error) {
	fqdn := fmt.Sprintf("%s.%s", root.Namespace, root.Name)
	source := root.Source
	if source == "" {
		source = deps.cfg.Server
	}
	constraint := root.Constraint
	if constraint == "" {
		constraint = root.Version
	}
	task := resolveTask{
		FQDN:              fqdn,
		Namespace:         root.Namespace,
		Name:              root.Name,
		Source:            source,
		Constraints:       []string{constraint},
		ConstraintSources: []constraintSource{{Constraint: constraint, Source: "root"}},
	}
	col := collection{Namespace: root.Namespace, Name: root.Name, Source: source}
	return resolveTaskVersion(ctx, deps, task, col)
}

// resolveTaskVersion resolves task's constraint(s) to a single concrete
// version, without resolving or caching any dependencies - the piece of
// resolveOne's work that --no-deps actually needs. An exact pin
// (exactVersionFromConstraints) short-circuits before any I/O; otherwise it
// composes the same root-metadata and version-selection helpers resolveOne
// uses (cachePolicyForConstraint, resolveRootMetadata, resolveFinalVersion),
// so an unpinned or ranged root is resolved consistently with the normal
// resolver. resolveOne itself is left untouched: this is a new, narrower
// function built from the same lower-level pieces, not an extraction out of
// it.
func resolveTaskVersion(ctx context.Context, deps collectionDeps, task resolveTask, col collection) (string, error) {
	version, exact, err := exactVersionFromConstraints(task.Constraints)
	if err != nil {
		return "", err
	}
	if exact {
		return version, nil
	}

	policy := cachePolicyForConstraint(deps.cfg, exact)
	rootMeta, versionsURL, err := resolveRootMetadata(ctx, deps, col, policy, task.FQDN)
	if err != nil {
		return "", err
	}
	return resolveFinalVersion(ctx, deps, task, policy, version, exact, rootMeta, versionsURL)
}

func resolveFromSnapshots(
	ctx context.Context,
	deps collectionDeps,
	roots []collection,
	reqSpec map[string]requirementSpec,
	reqHash string,
) (map[string]collection, map[string][]string, bool, error) {
	cfg := deps.cfg
	st := deps.st

	if resolved, graph, ok := loadResolvedFromSnapshot(cfg, st, roots, reqHash); ok {
		return resolved, graph, true, nil
	}
	resolved, graph, ok, err := tryIncrementalResolve(ctx, deps, roots, reqSpec, reqHash)
	if err != nil {
		return nil, nil, false, err
	}
	return resolved, graph, ok, nil
}

func recordResolution(
	st *store.Store,
	resolved map[string]collection,
	graph map[string][]string,
	reqHash string,
	server string,
	reqSpec map[string]requirementSpec,
) {
	setResolvedAll(st, resolved)
	st.SetGraphSnapshot(graph)
	st.SetMetaRequirements(reqHash, server)
	st.SetRequirements(reqSpec)
}

func newResolverState(cfg *config.Config, roots []collection) (*resolverState, error) {
	state := &resolverState{
		cfg:            cfg,
		resolved:       make(map[string]collection),
		depsByParent:   make(map[string]map[string]string),
		depConstraints: make(map[string]map[string]string),
		sourceByFQDN:   make(map[string]string),
		queue:          make([]string, 0, len(roots)),
		queued:         make(map[string]bool),
	}
	if err := state.enqueueRoots(roots); err != nil {
		return nil, err
	}
	return state, nil
}

func (r *resolverState) enqueueRoots(roots []collection) error {
	for _, root := range roots {
		fqdn := fmt.Sprintf("%s.%s", root.Namespace, root.Name)
		source := root.Source
		if source == "" {
			source = r.cfg.Server
		}
		r.sourceByFQDN[fqdn] = source
		constraint := root.Constraint
		if constraint == "" {
			constraint = root.Version
		}
		if err := addRootConstraint(r.depConstraints, fqdn, constraint); err != nil {
			return err
		}
		if !r.queued[fqdn] {
			r.queue = append(r.queue, fqdn)
			r.queued[fqdn] = true
		}
	}
	return nil
}

// resolveQueue drives the resolver in a pipelined fashion: tasks are
// dispatched to a worker pool as soon as new fqdns are discovered, and
// results are folded back into state by a single owner goroutine. This
// removes the wave barrier that the previous wave-based loop imposed —
// deeper dependency chains no longer wait for the slowest task in the
// current wave to finish before starting the next level.
func (r *resolverState) resolveQueue(ctx context.Context, deps collectionDeps) error {
	workers := max(deps.cfg.Workers, 1)
	workCh := make(chan resolveTask)
	resultCh := make(chan resolveResult)

	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() { runResolveWorker(ctx, deps, workCh, resultCh) })
	}
	go func() {
		wg.Wait()
		close(resultCh)
	}()

	closeOnce := sync.Once{}
	closeWork := func() { closeOnce.Do(func() { close(workCh) }) }
	drain := func() {
		//nolint:revive // intentional drain to let workers complete pending sends.
		for range resultCh {
		}
	}

	seed, err := r.drainQueueToTasks()
	if err != nil {
		closeWork()
		drain()
		return err
	}
	return r.pumpResolveLoop(workCh, resultCh, seed, closeWork, drain)
}

func runResolveWorker(ctx context.Context, deps collectionDeps, in <-chan resolveTask, out chan<- resolveResult) {
	for task := range in {
		out <- resolveOne(ctx, deps, task)
	}
}

func (r *resolverState) drainQueueToTasks() ([]resolveTask, error) {
	tasks, err := r.buildTasks()
	if err != nil {
		return nil, err
	}
	r.resetQueue()
	return tasks, nil
}

func (r *resolverState) pumpResolveLoop(
	workCh chan<- resolveTask,
	resultCh <-chan resolveResult,
	seed []resolveTask,
	closeWork func(),
	drain func(),
) error {
	pending := 0
	queued := seed
	for {
		if pending == 0 && len(queued) == 0 {
			closeWork()
			drain()
			return nil
		}
		var sendCh chan<- resolveTask
		var next resolveTask
		if len(queued) > 0 {
			sendCh = workCh
			next = queued[0]
		}
		select {
		case sendCh <- next:
			queued = queued[1:]
			pending++
		case res := <-resultCh:
			pending--
			if res.Err != nil {
				closeWork()
				drain()
				return res.Err
			}
			r.applyResult(res)
			more, err := r.drainQueueToTasks()
			if err != nil {
				closeWork()
				drain()
				return err
			}
			queued = append(queued, more...)
		}
	}
}

func (r *resolverState) buildTasks() ([]resolveTask, error) {
	tasks := make([]resolveTask, 0, len(r.queue))
	for _, fqdn := range r.queue {
		namespace, name, ok := helpers.SplitFQDN(fqdn)
		if !ok {
			return nil, fmt.Errorf("%w: %q", helpers.ErrInvalidCollectionName, fqdn)
		}
		sources := constraintSourcesFor(r.depConstraints, fqdn)
		constraints := constraintStringsFromSources(sources)
		source := r.sourceByFQDN[fqdn]
		if source == "" {
			source = r.cfg.Server
		}
		tasks = append(tasks, resolveTask{
			FQDN:              fqdn,
			Namespace:         namespace,
			Name:              name,
			Constraints:       constraints,
			ConstraintSources: sources,
			Source:            source,
		})
	}
	return tasks, nil
}

func (r *resolverState) resetQueue() {
	r.queue = r.queue[:0]
	r.queued = make(map[string]bool)
}

func (r *resolverState) applyResult(res resolveResult) {
	parentFQDN := res.FQDN
	previous, ok := r.resolved[parentFQDN]
	if !ok || previous.Version != res.Version {
		r.resolved[parentFQDN] = collection{
			Namespace: res.Namespace,
			Name:      res.Name,
			Version:   res.Version,
			Source:    res.Source,
		}
	}

	changedDeps := applyDependencyConstraints(parentFQDN, res.Deps, r.depConstraints, r.depsByParent)
	for depFQDN := range res.Deps {
		if _, ok := r.sourceByFQDN[depFQDN]; !ok {
			r.sourceByFQDN[depFQDN] = r.cfg.Server
		}
	}
	r.enqueueChanges(changedDeps)
}

func (r *resolverState) enqueueChanges(changedDeps map[string]bool) {
	for depFQDN := range changedDeps {
		if !r.queued[depFQDN] {
			r.queue = append(r.queue, depFQDN)
			r.queued[depFQDN] = true
		}
	}
}

func (r *resolverState) buildGraph(roots []collection) (map[string]collection, map[string][]string, error) {
	r.pruneUnreachable(roots)
	graph, err := buildGraphFromDeps(r.resolved, r.depsByParent)
	if err != nil {
		return nil, nil, err
	}
	ensureGraphNodes(r.resolved, graph)
	return r.resolved, graph, nil
}

func (r *resolverState) pruneUnreachable(roots []collection) {
	reachable := collectReachable(roots, r.depsByParent)
	for fqdn := range r.resolved {
		if !reachable[fqdn] {
			delete(r.resolved, fqdn)
		}
	}
	for parent, deps := range r.depsByParent {
		if !reachable[parent] {
			delete(r.depsByParent, parent)
			continue
		}
		for dep := range deps {
			if !reachable[dep] {
				delete(deps, dep)
			}
		}
	}
}

func buildGraphFromDeps(resolved map[string]collection, depsByParent map[string]map[string]string) (map[string][]string, error) {
	graph := make(map[string][]string)
	for parentFQDN, deps := range depsByParent {
		parentCol, ok := resolved[parentFQDN]
		if !ok {
			return nil, fmt.Errorf("%w: %s", helpers.ErrMissingResolvedParent, parentFQDN)
		}
		parentKey := parentCol.key()
		depKeys := make([]string, 0, len(deps))
		for depFQDN := range deps {
			depCol, ok := resolved[depFQDN]
			if !ok {
				return nil, fmt.Errorf("%w: %s", helpers.ErrMissingResolvedDependency, depFQDN)
			}
			depKeys = append(depKeys, depCol.key())
		}
		graph[parentKey] = depKeys
	}
	return graph, nil
}

func ensureGraphNodes(resolved map[string]collection, graph map[string][]string) {
	for _, col := range resolved {
		key := col.key()
		if _, ok := graph[key]; !ok {
			graph[key] = nil
		}
	}
}

// resolveOne resolves a single collection version and dependencies.
func resolveOne(ctx context.Context, deps collectionDeps, task resolveTask) resolveResult {
	cfg := deps.cfg
	st := deps.st

	col := collection{
		Namespace: task.Namespace,
		Name:      task.Name,
		Source:    task.Source,
	}

	version, exact, err := exactVersionFromConstraints(task.Constraints)
	if err != nil {
		return resolveResult{FQDN: task.FQDN, Namespace: task.Namespace, Name: task.Name, Err: err}
	}
	policy := cachePolicyForConstraint(cfg, exact)
	if exact {
		if res, ok := cachedResult(task, version, st, policy); ok {
			return res
		}
	}

	rootMeta, versionsURL, err := resolveRootMetadata(ctx, deps, col, policy, task.FQDN)
	if err != nil {
		return resolveResult{FQDN: task.FQDN, Namespace: task.Namespace, Name: task.Name, Err: err}
	}

	version, err = resolveFinalVersion(ctx, deps, task, policy, version, exact, rootMeta, versionsURL)
	if err != nil {
		return resolveResult{FQDN: task.FQDN, Namespace: task.Namespace, Name: task.Name, Err: err}
	}

	if res, ok := cachedResult(task, version, st, policy); ok {
		return res
	}

	versionInfo, err := fetchVersionMetadataCached(ctx, deps, col.Source, versionsURL, version, policy)
	if err != nil {
		return resolveResult{FQDN: task.FQDN, Namespace: task.Namespace, Name: task.Name, Err: err}
	}

	depMap, err := parseDependencies(extractDependencies(versionInfo))
	if err != nil {
		return resolveResult{
			FQDN:      task.FQDN,
			Namespace: task.Namespace,
			Name:      task.Name,
			Err:       err,
		}
	}

	cacheKey := fmt.Sprintf("%s.%s@%s", task.Namespace, task.Name, version)
	cacheDeps(st, policy, cacheKey, depMap)
	return buildResolveResult(task, version, depMap)
}

func resolveFinalVersion(
	ctx context.Context,
	deps collectionDeps,
	task resolveTask,
	policy cacheManager.Policy,
	version string,
	exact bool,
	rootMeta *types.GalaxyCollection,
	versionsURL string,
) (string, error) {
	if exact {
		return version, nil
	}
	return resolveNonExactVersion(ctx, deps, task, policy, version, rootMeta, versionsURL)
}

func cachedResult(task resolveTask, version string, st *store.Store, policy cacheManager.Policy) (resolveResult, bool) {
	cacheKey := fmt.Sprintf("%s.%s@%s", task.Namespace, task.Name, version)
	deps, ok := cachedDeps(st, policy, cacheKey)
	if !ok {
		return resolveResult{}, false
	}
	return buildResolveResult(task, version, deps), true
}

func cacheDeps(st *store.Store, policy cacheManager.Policy, cacheKey string, deps map[string]string) {
	if st == nil || !policy.Write {
		return
	}
	st.SetDepsCache(cacheKey, deps)
}

func buildResolveResult(task resolveTask, version string, deps map[string]string) resolveResult {
	return resolveResult{
		FQDN:      task.FQDN,
		Namespace: task.Namespace,
		Name:      task.Name,
		Source:    task.Source,
		Version:   version,
		Deps:      deps,
	}
}

func cachedDeps(st *store.Store, policy cacheManager.Policy, cacheKey string) (map[string]string, bool) {
	if st == nil || !policy.Read {
		return nil, false
	}
	deps, ok := st.GetDepsCache(cacheKey)
	return deps, ok
}

func resolveRootMetadata(
	ctx context.Context,
	deps collectionDeps,
	col collection,
	policy cacheManager.Policy,
	label string,
) (*types.GalaxyCollection, string, error) {
	runtime := deps.runtime
	versionsURL := collectionVersionsURL(col)
	rootMeta, err := loadRootMetadataCached(ctx, deps, col, policy)
	if err != nil {
		return nil, "", err
	}
	if rootMeta != nil && rootMeta.VersionsURL != "" {
		versionsURL = normalizeVersionsURL(col.Source, rootMeta.VersionsURL)
		runtime.Output.Debugf("versions URL for %s: %s", label, versionsURL)
	}
	return rootMeta, versionsURL, nil
}

func extractDependencies(info *types.GalaxyCollectionVersionInfo) map[string]string {
	if len(info.Metadata.Dependencies) > 0 {
		return info.Metadata.Dependencies
	}
	return info.Manifest.CollectionInfo.Dependencies
}

func parseDependencies(deps map[string]string) (map[string]string, error) {
	parsedDeps := make(map[string]string, len(deps))
	for dep, constraint := range deps {
		if _, _, ok := helpers.SplitFQDN(dep); !ok {
			return nil, fmt.Errorf("%w: %q", helpers.ErrInvalidDependencyKey, dep)
		}
		parsedDeps[dep] = strings.TrimSpace(constraint)
	}
	return parsedDeps, nil
}

type versionCandidate struct {
	Semver  *semver.Version
	Version string
}

// selectionMode describes how selectVersion should behave on conflict.
type selectionMode struct {
	Lenient   bool
	Backtrack bool
}

// selectVersion picks the highest version that satisfies constraints.
//
// Strict mode (default): returns conflictError when no version matches.
//
// Backtrack mode: tries to drop exactly one constraint (the "blamed" one)
// and re-check satisfiability; if a single-blame solution exists, return
// it. Otherwise fall through to lenient.
//
// Lenient mode: pick the highest version satisfying the largest subset of
// constraints, log which constraints were ignored.
func selectVersion(
	runtime *infra.Infra,
	fqdn string,
	versions []string,
	sources []constraintSource,
	mode selectionMode,
) (string, error) {
	candidates := buildCandidates(versions)
	if len(candidates) == 0 {
		return "", helpers.ErrNoSemverCandidates
	}
	parsedConstraints, err := parseConstraints(constraintStringsFromSources(sources))
	if err != nil {
		return "", err
	}
	if v, ok := selectStrict(candidates, parsedConstraints); ok {
		return v, nil
	}
	if mode.Backtrack {
		if v, blamed, ok := selectBacktrackOne(candidates, parsedConstraints, sources); ok {
			logBlamed(runtime, fqdn, v, blamed)
			return v, nil
		}
	}
	if mode.Lenient {
		return selectLenient(runtime, fqdn, candidates, parsedConstraints, sources)
	}
	return "", &conflictError{FQDN: fqdn, Sources: sources}
}

// selectBacktrackOne tries every (constraints − one) subset and returns the
// highest version satisfying any such subset, plus the dropped constraint
// (the single "blamed" source). If multiple subsets succeed, the result
// with the highest version wins.
func selectBacktrackOne(
	candidates []versionCandidate,
	parsed []*semver.Constraints,
	sources []constraintSource,
) (string, constraintSource, bool) {
	if len(parsed) != len(sources) || len(parsed) < 2 {
		return "", constraintSource{}, false
	}
	bestIdx := -1
	var bestVersion string
	var bestBlamed constraintSource
	for skip := range parsed {
		subset := make([]*semver.Constraints, 0, len(parsed)-1)
		for i, c := range parsed {
			if i == skip {
				continue
			}
			subset = append(subset, c)
		}
		v, ok := selectStrict(candidates, subset)
		if !ok {
			continue
		}
		idx := candidateIndex(candidates, v)
		if bestIdx < 0 || idx < bestIdx {
			bestIdx = idx
			bestVersion = v
			bestBlamed = sources[skip]
		}
	}
	if bestIdx < 0 {
		return "", constraintSource{}, false
	}
	return bestVersion, bestBlamed, true
}

func candidateIndex(candidates []versionCandidate, version string) int {
	for i, c := range candidates {
		if c.Version == version {
			return i
		}
	}
	return -1
}

func logBlamed(runtime *infra.Infra, fqdn, version string, blamed constraintSource) {
	if runtime == nil || runtime.Output == nil {
		return
	}
	runtime.Output.Printf("⚠️ backtrack: %s @ %s — ignored constraint %s (required by %s)",
		fqdn, version, blamed.Constraint, prettySource(blamed.Source))
}

func buildCandidates(versions []string) []versionCandidate {
	candidates := make([]versionCandidate, 0, len(versions))
	for _, v := range versions {
		parsed, err := semver.NewVersion(v)
		if err != nil {
			continue
		}
		candidates = append(candidates, versionCandidate{Version: v, Semver: parsed})
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Semver.GreaterThan(candidates[j].Semver)
	})
	return candidates
}

func selectStrict(candidates []versionCandidate, constraints []*semver.Constraints) (string, bool) {
	for _, c := range candidates {
		ok := true
		for _, constraint := range constraints {
			if !constraint.Check(c.Semver) {
				ok = false
				break
			}
		}
		if ok {
			return c.Version, true
		}
	}
	return "", false
}

// selectLenient picks the candidate satisfying the largest number of
// constraints (tie-break: highest version, since candidates are sorted desc).
// It reports which constraints were ignored via the runtime printer.
func selectLenient(
	runtime *infra.Infra,
	fqdn string,
	candidates []versionCandidate,
	parsed []*semver.Constraints,
	sources []constraintSource,
) (string, error) {
	bestIdx := -1
	bestSatisfied := -1
	for i, c := range candidates {
		satisfied := 0
		for _, constraint := range parsed {
			if constraint.Check(c.Semver) {
				satisfied++
			}
		}
		if satisfied > bestSatisfied {
			bestSatisfied = satisfied
			bestIdx = i
		}
	}
	if bestIdx < 0 {
		return "", &conflictError{FQDN: fqdn, Sources: sources}
	}
	chosen := candidates[bestIdx]
	if runtime != nil && runtime.Output != nil {
		ignored := ignoredConstraints(chosen.Semver, parsed, sources)
		runtime.Output.Printf("⚠️ lenient: %s @ %s (ignored %d constraints)", fqdn, chosen.Version, len(ignored))
		for _, s := range ignored {
			runtime.Output.Printf("    · %s (required by %s)", s.Constraint, prettySource(s.Source))
		}
	}
	return chosen.Version, nil
}

func ignoredConstraints(version *semver.Version, parsed []*semver.Constraints, sources []constraintSource) []constraintSource {
	if len(parsed) != len(sources) {
		return nil
	}
	out := make([]constraintSource, 0, len(sources))
	for i, c := range parsed {
		if !c.Check(version) {
			out = append(out, sources[i])
		}
	}
	return out
}

// constraintsSatisfiedByVersion reports whether version matches constraints.
func constraintsSatisfiedByVersion(version string, constraints []string) (bool, error) {
	if len(constraints) == 0 {
		return true, nil
	}
	parsed, err := parseConstraints(constraints)
	if err != nil {
		return false, err
	}
	if len(parsed) == 0 {
		return true, nil
	}
	parsedVersion, err := semver.NewVersion(version)
	if err != nil {
		return false, fmt.Errorf("invalid version %q: %w", version, err)
	}
	for _, constraint := range parsed {
		if !constraint.Check(parsedVersion) {
			return false, nil
		}
	}
	return true, nil
}

// loadVersionsListCached loads the available versions list with caching,
// paging through the upstream API in bounded offset increments of
// versionLimit entries per request. Each page still flows through
// fetchJSONWithCachePolicy, so per-page ETag/cache behavior is unchanged.
//
// Pagination is bounded by maxVersionPages: a server that keeps reporting a
// growing total forever (or lies about it) makes the loop fail hard via
// helpers.ErrVersionsPagingExceeded instead of looping unboundedly or
// silently truncating the list a caller then resolves constraints against.
func loadVersionsListCached(
	ctx context.Context,
	deps collectionDeps,
	versionsURL string,
	policy cacheManager.Policy,
) ([]string, error) {
	if versions, ok := cachedVersionsList(deps.st, policy, versionsURL); ok {
		return versions, nil
	}

	var all []string
	offset := 0
	for page := 0; ; page++ {
		versions, total, err := fetchVersionsPage(ctx, deps, policy, versionsURL, versionLimit, offset)
		if err != nil {
			return nil, err
		}
		if page == 0 {
			// Pre-size once, right after the first page is in: use the
			// server's declared total when it reports one, so the common
			// case (a handful of pages) never triggers append's internal
			// growth; otherwise fall back to a single page's worth.
			capacity := versionLimit
			if total > 0 {
				// Clamp to what the loop could ever collect before the
				// maxVersionPages ceiling stops it: the server-declared total
				// is untrusted, so a hostile or broken meta.count must not be
				// allowed to drive the allocation (a huge value would panic
				// make with "cap out of range" long before the ceiling fires).
				capacity = min(total, maxVersionPages*versionLimit)
			}
			all = make([]string, 0, capacity)
		}
		all = append(all, versions...)
		if len(versions) < versionLimit {
			break
		}
		offset += versionLimit
		if total > 0 && offset >= total {
			break
		}
		if page+1 >= maxVersionPages {
			return nil, fmt.Errorf("%w: %s", helpers.ErrVersionsPagingExceeded, versionsURL)
		}
	}

	cacheVersionsList(deps.st, policy, versionsURL, all)
	return all, nil
}

func cachedVersionsList(st *store.Store, policy cacheManager.Policy, versionsURL string) ([]string, bool) {
	if st == nil || !policy.Read || policy.TTL != 0 {
		return nil, false
	}
	versions, ok := st.GetVersionsCache(versionsURL)
	if !ok || len(versions) == 0 {
		return nil, false
	}
	return versions, true
}

// fetchVersionsPage fetches one limit/offset page of the versions list and
// returns its version strings alongside the server's declared total (from
// meta.count or count, depending on payload shape), so the caller can decide
// whether more pages remain.
func fetchVersionsPage(
	ctx context.Context,
	deps collectionDeps,
	policy cacheManager.Policy,
	versionsURL string,
	limit, offset int,
) ([]string, int, error) {
	url := fmt.Sprintf("%s?limit=%d&offset=%d", versionsURL, limit, offset)
	var payload map[string]any
	if err := fetchJSONWithCachePolicy(ctx, deps.runtime.HTTP, url, deps.st, &payload, policy); err != nil {
		return nil, 0, err
	}
	return parseVersionsPayload(payload)
}

func cacheVersionsList(st *store.Store, policy cacheManager.Policy, versionsURL string, versions []string) {
	if st == nil || !policy.Write || policy.TTL != 0 {
		return
	}
	st.SetVersionsCache(versionsURL, versions)
}

// resolveNonExactVersion selects a version when constraints are not exact.
func resolveNonExactVersion(
	ctx context.Context,
	deps collectionDeps,
	task resolveTask,
	policy cacheManager.Policy,
	version string,
	rootMeta *types.GalaxyCollection,
	versionsURL string,
) (string, error) {
	runtime := deps.runtime

	if rootMeta != nil && rootMeta.HighestVersion.Version != "" {
		ok, err := constraintsSatisfiedByVersion(rootMeta.HighestVersion.Version, task.Constraints)
		if err != nil {
			return "", err
		}
		if ok {
			runtime.Output.Debugf("highest_version selected for %s: %s", task.FQDN, rootMeta.HighestVersion.Version)
			return rootMeta.HighestVersion.Version, nil
		}
	}
	if version != "" {
		return version, nil
	}
	runtime.Output.Debugf("resolving versions list for %s", task.FQDN)
	versionsMeta, err := loadVersionsListCached(ctx, deps, versionsURL, policy)
	if err != nil {
		return "", err
	}
	mode := selectionMode{Lenient: deps.cfg.IsLenient(), Backtrack: deps.cfg.IsBacktrack()}
	return selectVersion(deps.runtime, task.FQDN, versionsMeta, task.ConstraintSources, mode)
}

// parseConstraints parses version constraints into semver constraints.
func parseConstraints(list []string) ([]*semver.Constraints, error) {
	result := make([]*semver.Constraints, 0, len(list))
	for _, raw := range list {
		normalized := helpers.NormalizeConstraint(raw)
		if normalized == "" {
			continue
		}
		c, err := semver.NewConstraint(normalized)
		if err != nil {
			return nil, fmt.Errorf("invalid constraint %q: %w", normalized, err)
		}
		result = append(result, c)
	}
	return result, nil
}

// addRootConstraint records a constraint for a root collection.
func addRootConstraint(depConstraints map[string]map[string]string, fqdn, version string) error {
	constraint := helpers.NormalizeConstraint(version)
	if constraint == "" {
		return nil
	}
	if _, ok := depConstraints[fqdn]; !ok {
		depConstraints[fqdn] = make(map[string]string)
	}
	existing, ok := depConstraints[fqdn]["root"]
	if ok && existing != constraint {
		return fmt.Errorf("%w for %s: %q vs %q", helpers.ErrConflictingRootConstraints, fqdn, existing, constraint)
	}
	depConstraints[fqdn]["root"] = constraint
	return nil
}

// applyDependencyConstraints merges dependency constraints and reports changes.
func applyDependencyConstraints(
	parentFQDN string,
	newDeps map[string]string,
	depConstraints map[string]map[string]string,
	depsByParent map[string]map[string]string,
) map[string]bool {
	changed := make(map[string]bool)
	oldDeps := depsByParent[parentFQDN]
	if oldDeps == nil {
		oldDeps = make(map[string]string)
	}

	for dep := range oldDeps {
		if _, ok := newDeps[dep]; !ok {
			if removeConstraint(depConstraints, dep, parentFQDN) {
				changed[dep] = true
			}
		}
	}

	for dep, constraint := range newDeps {
		if setConstraint(depConstraints, dep, parentFQDN, constraint) {
			changed[dep] = true
		}
	}

	depsByParent[parentFQDN] = newDeps
	return changed
}

// setConstraint records a constraint for a dependency and reports changes.
func setConstraint(depConstraints map[string]map[string]string, dep, source, constraint string) bool {
	if _, ok := depConstraints[dep]; !ok {
		depConstraints[dep] = make(map[string]string)
	}
	current, ok := depConstraints[dep][source]
	if ok && current == constraint {
		return false
	}
	depConstraints[dep][source] = constraint
	return true
}

// removeConstraint removes a constraint and reports whether it changed.
func removeConstraint(depConstraints map[string]map[string]string, dep, source string) bool {
	if _, ok := depConstraints[dep]; !ok {
		return false
	}
	if _, ok := depConstraints[dep][source]; !ok {
		return false
	}
	delete(depConstraints[dep], source)
	if len(depConstraints[dep]) == 0 {
		delete(depConstraints, dep)
	}
	return true
}

// collectionVersionsURL builds the versions API URL for a collection.
func collectionVersionsURL(col collection) string {
	base := strings.TrimRight(col.Source, "/")
	return fmt.Sprintf("%s/api/v3/collections/%s/%s/versions/", base, col.Namespace, col.Name)
}

// normalizeSignatures trims, sorts, and filters signatures.
func normalizeSignatures(signatures []string) []string {
	if len(signatures) == 0 {
		return nil
	}
	out := make([]string, 0, len(signatures))
	for _, value := range signatures {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		out = append(out, trimmed)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

// normalizeRequirementConstraint normalizes a constraint for hashing.
func normalizeRequirementConstraint(value string) string {
	normalized := helpers.NormalizeConstraint(value)
	if normalized == "" {
		return "*"
	}
	return normalized
}

// requirementSpecEqual reports whether two requirement specs are equal.
func requirementSpecEqual(a, b requirementSpec) bool {
	if a.Constraint != b.Constraint || a.Source != b.Source || a.Type != b.Type {
		return false
	}
	left := normalizeSignatures(a.Signatures)
	right := normalizeSignatures(b.Signatures)
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

// exactVersionFromConstraints returns a single exact version if specified.
//
// Classification is delegated to the semver library rather than a
// hand-maintained character guard: a constraint is exact only if it parses
// as a bare semver.Version once a single leading "=" is stripped. Anything
// that fails as a version but succeeds as semver.NewConstraint is a genuine
// range or wildcard (including ansible's "1.x" / "1.2.x" x-ranges, which a
// char guard cannot recognize) and contributes no exact pin. A string that
// is neither a valid version nor a valid constraint is malformed.
func exactVersionFromConstraints(constraints []string) (string, bool, error) {
	exact := ""
	for _, raw := range constraints {
		normalized := helpers.NormalizeConstraint(raw)
		if normalized == "" {
			continue
		}
		candidate := normalized
		if after, hasPrefix := strings.CutPrefix(normalized, "="); hasPrefix {
			candidate = strings.TrimSpace(after)
		}
		if _, err := semver.NewVersion(candidate); err != nil {
			if _, cErr := semver.NewConstraint(normalized); cErr != nil {
				return "", false, fmt.Errorf("invalid version constraint %q: %w", raw, cErr)
			}
			// A valid range/wildcard constraint (e.g. ">=1.0.0" or "1.x") is
			// non-exact by definition; it contributes no pin.
			continue
		}
		if exact == "" {
			exact = candidate
			continue
		}
		if exact != candidate {
			return "", false, fmt.Errorf("%w: %s vs %s", helpers.ErrConflictingExactVersions, exact, candidate)
		}
	}
	if exact == "" {
		return "", false, nil
	}
	return exact, true, nil
}

// collectReachable returns the set of reachable collections from roots.
func collectReachable(roots []collection, depsByParent map[string]map[string]string) map[string]bool {
	reachable := make(map[string]bool)
	queue := make([]string, 0, len(roots))

	for _, root := range roots {
		fqdn := fmt.Sprintf("%s.%s", root.Namespace, root.Name)
		if !reachable[fqdn] {
			reachable[fqdn] = true
			queue = append(queue, fqdn)
		}
	}

	for len(queue) > 0 {
		fqdn := queue[0]
		queue = queue[1:]
		deps := depsByParent[fqdn]
		for dep := range deps {
			if !reachable[dep] {
				reachable[dep] = true
				queue = append(queue, dep)
			}
		}
	}

	return reachable
}

// splitCollectionKey splits a key of the form "ns.name@version".
func splitCollectionKey(key string) (string, string, error) {
	parts := strings.SplitN(key, "@", helpers.CollectionNameParts)
	if len(parts) != helpers.CollectionNameParts || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("%w: %q", helpers.ErrInvalidCollectionKey, key)
	}
	return parts[0], parts[1], nil
}

// collectGraphKeysFromKeys walks the graph starting from root keys.
func collectGraphKeysFromKeys(graph map[string][]string, roots []string) map[string]bool {
	visited := make(map[string]bool)
	queue := make([]string, 0, len(roots))
	for _, key := range roots {
		if !visited[key] {
			visited[key] = true
			queue = append(queue, key)
		}
	}

	for len(queue) > 0 {
		key := queue[0]
		queue = queue[1:]
		deps := graph[key]
		for _, dep := range deps {
			if !visited[dep] {
				visited[dep] = true
				queue = append(queue, dep)
			}
		}
	}
	return visited
}

// sameDeps reports whether two dependency slices contain the same items.
func sameDeps(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[string]int, len(a))
	for _, v := range a {
		counts[v]++
	}
	for _, v := range b {
		if counts[v] == 0 {
			return false
		}
		counts[v]--
	}
	return true
}

// tryIncrementalResolve reuses snapshot data when only some roots changed.
func tryIncrementalResolve(
	ctx context.Context,
	deps collectionDeps,
	roots []collection,
	currentSpec map[string]requirementSpec,
	reqHash string,
) (map[string]collection, map[string][]string, bool, error) {
	prevSpec := deps.st.RequirementsSnapshot()
	if len(prevSpec) == 0 {
		return nil, nil, false, nil
	}

	unchangedRoots, changedRoots := splitRootsByChange(roots, currentSpec, prevSpec)
	if len(unchangedRoots) == 0 || len(changedRoots) == 0 {
		return nil, nil, false, nil
	}

	return tryIncrementalResolveWithSnapshot(ctx, deps, unchangedRoots, changedRoots, currentSpec, reqHash)
}

func tryIncrementalResolveWithSnapshot(
	ctx context.Context,
	deps collectionDeps,
	unchangedRoots []collection,
	changedRoots []collection,
	currentSpec map[string]requirementSpec,
	reqHash string,
) (map[string]collection, map[string][]string, bool, error) {
	resolvedSnap, graphSnap, ok := loadSnapshotData(deps.st)
	if !ok {
		return nil, nil, false, nil
	}

	preservedResolved, preservedGraph, ok := buildPreservedSnapshot(deps.cfg, unchangedRoots, resolvedSnap, graphSnap)
	if !ok {
		return nil, nil, false, nil
	}

	resolvedNew, graphNew, err := resolveCollectionsInternal(ctx, deps, changedRoots, false, false)
	if err != nil {
		return nil, nil, false, err
	}

	mergedResolved, mergedGraph, ok := mergeResolvedGraphs(preservedResolved, preservedGraph, resolvedNew, graphNew)
	if !ok {
		return nil, nil, false, nil
	}

	if !expandGraphFromSnapshot(deps.cfg, mergedResolved, mergedGraph, resolvedSnap, graphSnap) {
		return nil, nil, false, nil
	}
	if !validateMergedGraph(mergedResolved, mergedGraph) {
		return nil, nil, false, nil
	}

	if deps.st != nil {
		recordResolution(deps.st, mergedResolved, mergedGraph, reqHash, deps.cfg.Server, currentSpec)
	}

	return mergedResolved, mergedGraph, true, nil
}

func splitRootsByChange(roots []collection, currentSpec, prevSpec map[string]requirementSpec) ([]collection, []collection) {
	unchangedRoots := make([]collection, 0, len(roots))
	changedRoots := make([]collection, 0, len(roots))
	for _, root := range roots {
		fqdn := fmt.Sprintf("%s.%s", root.Namespace, root.Name)
		current, ok := currentSpec[fqdn]
		if ok {
			if prev, ok := prevSpec[fqdn]; ok && requirementSpecEqual(current, prev) {
				unchangedRoots = append(unchangedRoots, root)
				continue
			}
		}
		changedRoots = append(changedRoots, root)
	}
	return unchangedRoots, changedRoots
}

func loadSnapshotData(st *store.Store) (map[string]store.ResolvedEntry, map[string][]string, bool) {
	resolvedSnap := st.ResolvedSnapshot()
	graphSnap := st.GraphSnapshot()
	if len(resolvedSnap) == 0 || len(graphSnap) == 0 {
		return nil, nil, false
	}
	return resolvedSnap, graphSnap, true
}

func buildPreservedSnapshot(
	cfg *config.Config,
	unchangedRoots []collection,
	resolvedSnap map[string]store.ResolvedEntry,
	graphSnap map[string][]string,
) (map[string]collection, map[string][]string, bool) {
	rootKeys, ok := preservedRootKeys(unchangedRoots, resolvedSnap)
	if !ok {
		return nil, nil, false
	}
	preservedKeys := collectGraphKeysFromKeys(graphSnap, rootKeys)
	preservedGraph := make(map[string][]string, len(preservedKeys))
	preservedResolved := make(map[string]collection)
	for key := range preservedKeys {
		deps, ok := graphSnap[key]
		if !ok {
			return nil, nil, false
		}
		preservedGraph[key] = deps
		if !addPreservedEntry(cfg, preservedResolved, resolvedSnap, key) {
			return nil, nil, false
		}
	}
	return preservedResolved, preservedGraph, true
}

func preservedRootKeys(unchangedRoots []collection, resolvedSnap map[string]store.ResolvedEntry) ([]string, bool) {
	rootKeys := make([]string, 0, len(unchangedRoots))
	for _, root := range unchangedRoots {
		fqdn := fmt.Sprintf("%s.%s", root.Namespace, root.Name)
		entry, ok := resolvedSnap[fqdn]
		if !ok || entry.Version == "" {
			return nil, false
		}
		rootKeys = append(rootKeys, fmt.Sprintf("%s@%s", fqdn, entry.Version))
	}
	return rootKeys, true
}

func addPreservedEntry(
	cfg *config.Config,
	preservedResolved map[string]collection,
	resolvedSnap map[string]store.ResolvedEntry,
	key string,
) bool {
	fqdn, version, err := splitCollectionKey(key)
	if err != nil {
		return false
	}
	entry, ok := resolvedSnap[fqdn]
	if !ok || entry.Version != version {
		return false
	}
	namespace, name, ok := helpers.SplitFQDN(fqdn)
	if !ok {
		return false
	}
	source := entry.Source
	if source == "" {
		source = cfg.Server
	}
	preservedResolved[fqdn] = collection{
		Namespace: namespace,
		Name:      name,
		Version:   version,
		Source:    source,
	}
	return true
}

func mergeResolvedGraphs(
	preservedResolved map[string]collection,
	preservedGraph map[string][]string,
	resolvedNew map[string]collection,
	graphNew map[string][]string,
) (map[string]collection, map[string][]string, bool) {
	mergedResolved := make(map[string]collection, len(preservedResolved)+len(resolvedNew))
	maps.Copy(mergedResolved, preservedResolved)
	for fqdn, col := range resolvedNew {
		if existing, ok := mergedResolved[fqdn]; ok && existing.Version != col.Version {
			return nil, nil, false
		}
		mergedResolved[fqdn] = col
	}

	mergedGraph := make(map[string][]string, len(preservedGraph)+len(graphNew))
	maps.Copy(mergedGraph, preservedGraph)
	for key, deps := range graphNew {
		if existing, ok := mergedGraph[key]; ok {
			if !sameDeps(existing, deps) {
				return nil, nil, false
			}
			continue
		}
		mergedGraph[key] = deps
	}
	return mergedResolved, mergedGraph, true
}

func expandGraphFromSnapshot(
	cfg *config.Config,
	mergedResolved map[string]collection,
	mergedGraph map[string][]string,
	resolvedSnap map[string]store.ResolvedEntry,
	graphSnap map[string][]string,
) bool {
	queue := make([]string, 0, len(mergedGraph))
	for key := range mergedGraph {
		queue = append(queue, key)
	}
	for len(queue) > 0 {
		key := queue[0]
		queue = queue[1:]
		deps := mergedGraph[key]
		for _, dep := range deps {
			if _, ok := mergedGraph[dep]; ok {
				continue
			}
			depDeps, ok := graphSnap[dep]
			if !ok {
				return false
			}
			mergedGraph[dep] = depDeps
			queue = append(queue, dep)
			if !ensureResolvedFromSnapshot(cfg, mergedResolved, resolvedSnap, dep) {
				return false
			}
		}
	}
	return true
}

func ensureResolvedFromSnapshot(
	cfg *config.Config,
	mergedResolved map[string]collection,
	resolvedSnap map[string]store.ResolvedEntry,
	key string,
) bool {
	fqdn, version, err := splitCollectionKey(key)
	if err != nil {
		return false
	}
	if existing, ok := mergedResolved[fqdn]; ok {
		return existing.Version == version
	}
	entry, ok := resolvedSnap[fqdn]
	if !ok || entry.Version != version {
		return false
	}
	namespace, name, ok := helpers.SplitFQDN(fqdn)
	if !ok {
		return false
	}
	source := entry.Source
	if source == "" {
		source = cfg.Server
	}
	mergedResolved[fqdn] = collection{
		Namespace: namespace,
		Name:      name,
		Version:   version,
		Source:    source,
	}
	return true
}

func validateMergedGraph(mergedResolved map[string]collection, mergedGraph map[string][]string) bool {
	for key := range mergedGraph {
		fqdn, version, err := splitCollectionKey(key)
		if err != nil {
			return false
		}
		entry, ok := mergedResolved[fqdn]
		if !ok || entry.Version != version {
			return false
		}
	}
	return true
}

// buildRequirementsSpec builds a normalized requirement spec map.
func buildRequirementsSpec(cfg *config.Config, roots []collection) map[string]requirementSpec {
	spec := make(map[string]requirementSpec, len(roots))
	for _, root := range roots {
		fqdn := fmt.Sprintf("%s.%s", root.Namespace, root.Name)
		source := root.Source
		if source == "" {
			source = cfg.Server
		}
		constraint := root.Constraint
		if constraint == "" {
			constraint = root.Version
		}
		constraint = normalizeRequirementConstraint(constraint)
		spec[fqdn] = requirementSpec{
			Constraint: constraint,
			Source:     source,
			Type:       root.Type,
			Signatures: normalizeSignatures(root.Signatures),
		}
	}
	return spec
}

// requirementsSignatureFromSpec returns a stable signature of requirements.
func requirementsSignatureFromSpec(spec map[string]requirementSpec) string {
	parts := make([]string, 0, len(spec))
	for fqdn, entry := range spec {
		constraint := entry.Constraint
		if constraint == "" {
			constraint = "*"
		}
		signatureKey := strings.Join(normalizeSignatures(entry.Signatures), ",")
		parts = append(parts, fmt.Sprintf("%s|%s|%s|%s|%s", fqdn, constraint, entry.Source, entry.Type, signatureKey))
	}
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:])
}

// loadResolvedFromSnapshot loads resolved data when requirements match.
func loadResolvedFromSnapshot(
	cfg *config.Config,
	st *store.Store,
	roots []collection,
	reqHash string,
) (map[string]collection, map[string][]string, bool) {
	if !snapshotMatchesRequirements(st, reqHash) {
		return nil, nil, false
	}
	resolvedSnapshot, graphSnapshot, ok := loadSnapshotData(st)
	if !ok {
		return nil, nil, false
	}
	resolved, ok := buildResolvedSnapshot(cfg, resolvedSnapshot)
	if !ok {
		return nil, nil, false
	}
	if !rootsMatchSnapshot(roots, resolved, graphSnapshot) {
		return nil, nil, false
	}
	filtered := filterGraphSnapshot(graphSnapshot, resolved)
	return resolved, filtered, true
}

func snapshotMatchesRequirements(st *store.Store, reqHash string) bool {
	meta := st.MetaSnapshot()
	return meta.RequirementsHash != "" && meta.RequirementsHash == reqHash
}

func buildResolvedSnapshot(cfg *config.Config, resolvedSnapshot map[string]store.ResolvedEntry) (map[string]collection, bool) {
	resolved := make(map[string]collection, len(resolvedSnapshot))
	for fqdn, entry := range resolvedSnapshot {
		if entry.Version == "" {
			return nil, false
		}
		namespace, name, ok := helpers.SplitFQDN(fqdn)
		if !ok {
			return nil, false
		}
		source := entry.Source
		if source == "" {
			source = cfg.Server
		}
		resolved[fqdn] = collection{
			Namespace: namespace,
			Name:      name,
			Version:   entry.Version,
			Source:    source,
		}
	}
	return resolved, true
}

func rootsMatchSnapshot(roots []collection, resolved map[string]collection, graphSnapshot map[string][]string) bool {
	for _, root := range roots {
		if !isGalaxyType(root.Type) {
			return false
		}
		fqdn := fmt.Sprintf("%s.%s", root.Namespace, root.Name)
		col, ok := resolved[fqdn]
		if !ok {
			return false
		}
		constraint := root.Constraint
		if constraint == "" {
			constraint = root.Version
		}
		ok, err := constraintSatisfied(col.Version, constraint)
		if err != nil || !ok {
			return false
		}
		if _, ok := graphSnapshot[col.key()]; !ok {
			return false
		}
	}
	return true
}

func filterGraphSnapshot(graphSnapshot map[string][]string, resolved map[string]collection) map[string][]string {
	validKeys := make(map[string]bool, len(resolved))
	for _, col := range resolved {
		validKeys[col.key()] = true
	}
	filtered := make(map[string][]string, len(graphSnapshot))
	for key, deps := range graphSnapshot {
		if !validKeys[key] {
			continue
		}
		out := make([]string, 0, len(deps))
		for _, dep := range deps {
			if validKeys[dep] {
				out = append(out, dep)
			}
		}
		filtered[key] = out
	}
	return filtered
}

// constraintSatisfied reports whether version satisfies constraint.
func constraintSatisfied(version, constraint string) (bool, error) {
	normalized := helpers.NormalizeConstraint(constraint)
	if normalized == "" {
		return true, nil
	}
	v, err := semver.NewVersion(version)
	if err != nil {
		return false, fmt.Errorf("invalid version %q: %w", version, err)
	}
	c, err := semver.NewConstraint(normalized)
	if err != nil {
		return false, fmt.Errorf("invalid constraint %q: %w", normalized, err)
	}
	return c.Check(v), nil
}

// buildInstallLevels topologically groups nodes for installation order.
func buildInstallLevels(graph map[string][]string) ([][]string, error) {
	indegree, reverse := buildDependencyIndex(graph)
	return topologicalLevels(indegree, reverse)
}

func buildDependencyIndex(graph map[string][]string) (map[string]int, map[string][]string) {
	indegree := make(map[string]int)
	reverse := make(map[string][]string)
	for node, deps := range graph {
		if _, ok := indegree[node]; !ok {
			indegree[node] = 0
		}
		for _, dep := range deps {
			indegree[node]++
			reverse[dep] = append(reverse[dep], node)
			if _, ok := indegree[dep]; !ok {
				indegree[dep] = 0
			}
		}
	}
	return indegree, reverse
}

func topologicalLevels(indegree map[string]int, reverse map[string][]string) ([][]string, error) {
	levels := make([][]string, 0)
	for len(indegree) > 0 {
		level := nextLevel(indegree)
		if len(level) == 0 {
			return nil, helpers.ErrDependencyGraphHasACycle
		}
		levels = append(levels, level)
		applyLevel(indegree, reverse, level)
	}
	return levels, nil
}

func nextLevel(indegree map[string]int) []string {
	level := make([]string, 0)
	for node, deg := range indegree {
		if deg == 0 {
			level = append(level, node)
		}
	}
	return level
}

func applyLevel(indegree map[string]int, reverse map[string][]string, level []string) {
	for _, node := range level {
		delete(indegree, node)
		for _, child := range reverse[node] {
			indegree[child]--
		}
	}
}
