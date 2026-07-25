package solver

import (
	"fmt"
	"slices"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// Attribution pairs a constraint with the parent that imposed it, mirroring
// the existing "which parent wants what" block the install pipeline already
// renders for a plain version conflict.
type Attribution struct {
	Parent     string
	Constraint string
}

// ConflictError reports that no selection of versions satisfies every
// requirement. It carries a human-readable proof (the derivation graph
// walked into numbered prose), a set of conditional hints, and the flat
// per-parent attributions the existing conflict UX renders.
type ConflictError struct {
	proofLines   []string
	hints        []string
	attributions []Attribution
}

// Error renders the proof followed by any hints, one per line.
func (e *ConflictError) Error() string {
	var b strings.Builder
	for i, l := range e.proofLines {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(l)
	}
	for _, h := range e.hints {
		b.WriteString("\nhint: ")
		b.WriteString(h)
	}
	return b.String()
}

// Is reports that a ConflictError is a version-resolution failure, so
// callers can errors.Is-check against the shared sentinel exactly as the
// pre-existing collections conflict error does.
func (e *ConflictError) Is(target error) bool {
	return target == helpers.ErrNoVersionSatisfiesConstraints
}

// ProofLines returns the rendered derivation-graph proof, one entry per
// line (a blank entry marks a paragraph break).
func (e *ConflictError) ProofLines() []string {
	return e.proofLines
}

// Hints returns the deterministic, package-name-ordered hint texts.
func (e *ConflictError) Hints() []string {
	return e.hints
}

// Attributions returns every (parent, constraint) pair contributing a
// dependency edge to the proof, sorted by (Parent, Constraint).
func (e *ConflictError) Attributions() []Attribution {
	return e.attributions
}

// isDerivedInc reports whether inc is a conflict-resolution-derived
// incompatibility (as opposed to an external one).
func isDerivedInc(inc *incompatibility) bool {
	_, ok := inc.Cause.(causeConflict)
	return ok
}

// causedByTwoExternals reports whether inc is derived and both of its own
// causes are external - the "simple" case the numbered rendering algorithm
// prefers to inline without a line number.
func causedByTwoExternals(inc *incompatibility) bool {
	cc, ok := inc.Cause.(causeConflict)
	if !ok {
		return false
	}
	return !isDerivedInc(cc.Left) && !isDerivedInc(cc.Right)
}

// pickSimple returns, of cause1 and cause2, whichever one (if exactly one)
// is caused by two externals, alongside the other (the "compound" one).
func pickSimple(cause1, cause2 *incompatibility) (*incompatibility, *incompatibility, bool) {
	c1 := causedByTwoExternals(cause1)
	c2 := causedByTwoExternals(cause2)
	switch {
	case c1:
		return cause1, cause2, true
	case c2:
		return cause2, cause1, true
	default:
		return nil, nil, false
	}
}

// splitOneDerived reports whether exactly one of cc's two causes is itself
// derived, returning (derivedCause, externalCause, true) if so.
func splitOneDerived(cc causeConflict) (*incompatibility, *incompatibility, bool) {
	l, r := isDerivedInc(cc.Left), isDerivedInc(cc.Right)
	switch {
	case l && !r:
		return cc.Left, cc.Right, true
	case r && !l:
		return cc.Right, cc.Left, true
	default:
		return nil, nil, false
	}
}

// countOutgoing walks inc's derivation graph once (memoized via visited)
// and records, for every incompatibility, how many distinct parents cause
// it - the pre-pass the numbered rendering algorithm requires so it knows
// up front which nodes will need to be referred back to.
func countOutgoing(inc *incompatibility, outgoing map[*incompatibility]int, visited map[*incompatibility]bool) {
	if visited[inc] {
		return
	}
	visited[inc] = true
	cc, ok := inc.Cause.(causeConflict)
	if !ok {
		return
	}
	outgoing[cc.Left]++
	outgoing[cc.Right]++
	countOutgoing(cc.Left, outgoing, visited)
	countOutgoing(cc.Right, outgoing, visited)
}

// reportBuilder accumulates the numbered proof as it walks a derivation
// graph. lineOf holds the line number assigned to a node that has one (a
// node referenced by two or more parents always earns one; the partial-
// satisfier merge case can also force one early to allow a back-reference).
type reportBuilder struct {
	lineOf   map[*incompatibility]int
	rendered map[*incompatibility]bool
	outgoing map[*incompatibility]int
	lines    []string
	nextLine int
}

// hasLine reports whether inc has already been assigned a line number.
func (b *reportBuilder) hasLine(inc *incompatibility) bool {
	_, ok := b.lineOf[inc]
	return ok
}

// ensureRendered renders inc if it has not been rendered yet (a node
// referenced by only one parent is rendered exactly once, at its sole
// reference point). Every call site only needs this side effect - inc being
// rendered and, if applicable, assigned a line number - never a reference
// text, so this returns nothing.
func (b *reportBuilder) ensureRendered(s *solveState, inc *incompatibility) {
	if !b.rendered[inc] {
		b.renderNode(s, inc, false)
	}
}

// forceLineNumber assigns inc the next line number if it does not already
// have one, retrofitting the number onto the last line written (which, by
// the invariant this is only ever called right after ensureRendered(inc)
// for a first-time, single-parent node, is guaranteed to be inc's own
// concluding line).
func (b *reportBuilder) forceLineNumber(inc *incompatibility) int {
	if ln, ok := b.lineOf[inc]; ok {
		return ln
	}
	b.nextLine++
	b.lineOf[inc] = b.nextLine
	if n := len(b.lines); n > 0 {
		b.lines[n-1] = fmt.Sprintf("%s (%d)", b.lines[n-1], b.nextLine)
	}
	return b.nextLine
}

// renderNode renders inc's own explanatory line (recursing into its causes
// as the numbered algorithm requires), appends it to b.lines, and assigns it
// a line number if it is referenced by two or more parents. final marks the
// single outermost call (the terminal incompatibility itself), which
// rewrites the line's leading connective to "So," and its trailing
// description to "version solving failed".
func (b *reportBuilder) renderNode(s *solveState, inc *incompatibility, final bool) {
	cc, ok := inc.Cause.(causeConflict)
	if !ok {
		// renderNode is only ever called (by buildConflictError or
		// ensureRendered) on a derived incompatibility, so a non-causeConflict
		// cause here is a solver invariant violation, not reachable input.
		panic("solver: renderNode called on a non-derived incompatibility; this is a bug")
	}
	ext1, ext2 := !isDerivedInc(cc.Left), !isDerivedInc(cc.Right)

	var line string
	switch {
	case !ext1 && !ext2:
		line = b.renderBothDerived(s, inc, cc.Left, cc.Right)
	case ext1 != ext2:
		derived, external := cc.Right, cc.Left
		if ext2 {
			derived, external = cc.Left, cc.Right
		}
		line = b.renderOneDerived(s, inc, derived, external)
	default:
		line = renderBothExternal(s, cc, inc)
	}

	b.rendered[inc] = true
	if final {
		line = finalizeLine(line, s.describe(inc))
	}
	b.lines = append(b.lines, line)
	if !final && b.outgoing[inc] >= 2 {
		b.nextLine++
		b.lineOf[inc] = b.nextLine
		b.lines[len(b.lines)-1] = fmt.Sprintf("%s (%d)", line, b.nextLine)
	}
}

// renderBothExternal renders inc's line when both of cc's causes are
// external: the ordinary two-cause conjunction, or - when cc.Left and
// cc.Right are the very same incompatibility (a degenerate self-resolution,
// e.g. an unknown-package leaf whose own derived negation re-conflicts with
// it) - the single cause once, instead of "<X> and <X>". This relies on
// incompatStore's content dedup making two content-identical
// incompatibilities the same pointer, so the equality check is exact
// pointer identity, never describe-string equality.
func renderBothExternal(s *solveState, cc causeConflict, inc *incompatibility) string {
	if cc.Left == cc.Right {
		return fmt.Sprintf("Because %s, %s.", s.describe(cc.Left), s.describe(inc))
	}
	return fmt.Sprintf("Because %s and %s, %s.", s.describe(cc.Left), s.describe(cc.Right), s.describe(inc))
}

// renderBothDerived implements the numbered algorithm's case 1: inc is
// caused by two other derived incompatibilities.
func (b *reportBuilder) renderBothDerived(s *solveState, inc, cause1, cause2 *incompatibility) string {
	ln1, has1 := b.lineOf[cause1]
	ln2, has2 := b.lineOf[cause2]

	switch {
	case has1 && has2:
		return fmt.Sprintf("Because %s (%d) and %s (%d), %s.", s.describe(cause1), ln1, s.describe(cause2), ln2, s.describe(inc))
	case has1 != has2:
		withLine, without, ln := cause1, cause2, ln1
		if has2 {
			withLine, without, ln = cause2, cause1, ln2
		}
		b.ensureRendered(s, without)
		return fmt.Sprintf("And because %s (%d), %s.", s.describe(withLine), ln, s.describe(inc))
	default:
		if simple, compound, ok := pickSimple(cause1, cause2); ok {
			b.ensureRendered(s, compound)
			b.ensureRendered(s, simple)
			return fmt.Sprintf("Thus, %s.", s.describe(inc))
		}
		b.ensureRendered(s, cause1)
		ln := b.forceLineNumber(cause1)
		b.lines = append(b.lines, "")
		b.ensureRendered(s, cause2)
		return fmt.Sprintf("And because %s (%d), %s.", s.describe(cause1), ln, s.describe(inc))
	}
}

// renderOneDerived implements the numbered algorithm's case 2: inc is
// caused by exactly one derived incompatibility and one external one.
func (b *reportBuilder) renderOneDerived(s *solveState, inc, derived, external *incompatibility) string {
	if ln, ok := b.lineOf[derived]; ok {
		return fmt.Sprintf("Because %s and %s (%d), %s.", s.describe(external), s.describe(derived), ln, s.describe(inc))
	}
	if cc, ok := derived.Cause.(causeConflict); ok {
		if priorDerived, priorExternal, ok2 := splitOneDerived(cc); ok2 && !b.hasLine(priorDerived) {
			b.ensureRendered(s, priorDerived)
			return fmt.Sprintf("And because %s and %s, %s.", s.describe(priorExternal), s.describe(external), s.describe(inc))
		}
	}
	b.ensureRendered(s, derived)
	return fmt.Sprintf("And because %s, %s.", s.describe(external), s.describe(inc))
}

// finalizeLine rewrites the outermost proof line: its trailing ", {desc}."
// clause becomes ", version solving failed.", and its leading connective
// becomes "So,", per the reference algorithm's special-cased final line.
func finalizeLine(line, desc string) string {
	suffix := ", " + desc + "."
	if trimmed, ok := strings.CutSuffix(line, suffix); ok {
		line = trimmed + ", version solving failed."
	}
	switch {
	case strings.HasPrefix(line, "And because "):
		line = "So, because " + strings.TrimPrefix(line, "And because ")
	case strings.HasPrefix(line, "Because "):
		line = "So, because " + strings.TrimPrefix(line, "Because ")
	case strings.HasPrefix(line, "Thus, "):
		line = "So, " + strings.TrimPrefix(line, "Thus, ")
	}
	return line
}

// buildConflictError renders the full proof for inc (the terminal
// incompatibility resolveConflict produced) and constructs the ConflictError
// returned to the caller. It runs while every package materialized during
// the solve is still live, since the hint conditions inspect universes.
func (s *solveState) buildConflictError(inc *incompatibility) *ConflictError {
	b := &reportBuilder{
		lineOf:   make(map[*incompatibility]int),
		rendered: make(map[*incompatibility]bool),
		outgoing: make(map[*incompatibility]int),
	}
	countOutgoing(inc, b.outgoing, make(map[*incompatibility]bool))

	if isDerivedInc(inc) {
		b.renderNode(s, inc, true)
	} else {
		b.lines = append(b.lines, finalizeLine(s.describe(inc), s.describe(inc)))
	}

	return &ConflictError{
		proofLines:   b.lines,
		hints:        s.collectHints(inc),
		attributions: s.collectAttributions(inc),
	}
}

// collectAttributions walks inc's derivation graph and returns every
// distinct (Parent, Constraint) pair contributed by a causeDependency leaf,
// sorted by (Parent, Constraint).
func (s *solveState) collectAttributions(inc *incompatibility) []Attribution {
	seen := make(map[Attribution]bool)
	visited := make(map[*incompatibility]bool)
	var walk func(*incompatibility)
	walk = func(n *incompatibility) {
		if visited[n] {
			return
		}
		visited[n] = true
		switch cause := n.Cause.(type) {
		case causeConflict:
			walk(cause.Left)
			walk(cause.Right)
		case causeDependency:
			seen[Attribution{Parent: cause.Parent, Constraint: cause.Constraint}] = true
		}
	}
	walk(inc)

	out := make([]Attribution, 0, len(seen))
	for a := range seen {
		out = append(out, a)
	}
	slices.SortFunc(out, func(a, b Attribution) int {
		if a.Parent != b.Parent {
			return strings.Compare(a.Parent, b.Parent)
		}
		return strings.Compare(a.Constraint, b.Constraint)
	})
	return out
}

// collectHints walks inc's derivation graph for causeNoVersions leaves and
// renders the deterministic, conditional hints of section 10.4 for each
// distinct package, ordered by package name ascending.
func (s *solveState) collectHints(inc *incompatibility) []string {
	type entry struct{ pkg, text string }
	var entries []entry
	pkgsSeen := make(map[string]bool)
	visited := make(map[*incompatibility]bool)

	var walk func(*incompatibility)
	walk = func(n *incompatibility) {
		if visited[n] {
			return
		}
		visited[n] = true
		switch cause := n.Cause.(type) {
		case causeConflict:
			walk(cause.Left)
			walk(cause.Right)
		case causeNoVersions:
			pkg := cause.term.Package
			if pkgsSeen[pkg] {
				return
			}
			if text, ok := s.prereleaseHint(pkg); ok {
				pkgsSeen[pkg] = true
				entries = append(entries, entry{pkg: pkg, text: text})
			}
		}
	}
	walk(inc)

	slices.SortFunc(entries, func(a, b entry) int { return strings.Compare(a.pkg, b.pkg) })
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.text
	}
	return out
}

// prereleaseHint implements the two prerelease-related hint conditions of
// section 10.4 for pkg's materialized universe.
func (s *solveState) prereleaseHint(pkg string) (string, bool) {
	u := s.uniFor(pkg)
	if len(u.versions) == 0 {
		return "", false
	}
	allPrerelease := true
	anyPrerelease := false
	for _, v := range u.versions {
		if v.sv().Prerelease() != "" {
			anyPrerelease = true
		} else {
			allPrerelease = false
		}
	}
	switch {
	case allPrerelease:
		return fmt.Sprintf(
			"%s publishes only pre-release versions, which plain constraints exclude; "+
				"if a pre-release is acceptable, pin one exactly or use a >=X.Y.Z-0 floor - "+
				"otherwise no published version can satisfy this requirement", pkg,
		), true
	case anyPrerelease:
		return fmt.Sprintf(
			"pre-release versions of %s exist and are excluded by plain constraints; "+
				"if you intended to allow them, use a >=X.Y.Z-0 floor or an exact pin", pkg,
		), true
	default:
		return "", false
	}
}

// describe renders inc's own conclusion text: a specific phrasing for each
// external cause kind, or a generic term-based phrasing for a derived (or
// root) incompatibility.
func (s *solveState) describe(inc *incompatibility) string {
	switch cause := inc.Cause.(type) {
	case causeDependency:
		return s.describeDependency(cause)
	case causeNoVersions:
		return fmt.Sprintf("no version of %s matches %s", cause.term.Package, cause.term.Set.display)
	case causeUnknownPackage:
		return cause.Package + " has no published versions"
	default:
		return s.describeGeneric(inc)
	}
}

// describeDependency renders "Parent[@Version] depends on Dep Constraint",
// omitting root's version per the reference algorithm's root special case.
func (s *solveState) describeDependency(c causeDependency) string {
	parentLabel := "root"
	if c.Parent != rootPkg {
		parentLabel = fmt.Sprintf("%s %s", c.Parent, c.ParentVersion.Original())
	}
	constraint := c.Constraint
	if constraint == "" {
		constraint = "*"
	}
	return fmt.Sprintf("%s depends on %s %s", parentLabel, c.Dep, constraint)
}

// dependencyPairTermCount is the term count that renders as "{depender}
// requires {dependency}" (one positive term for the depender's own version,
// one negative term for the forbidden dependency range).
const dependencyPairTermCount = 2

// describeGeneric renders a root or conflict-resolution-derived
// incompatibility's terms generically: a single negative term reads
// "{X} is forbidden", a positive/negative pair reads "{depender} requires
// {dependency}", and the (rare, success-path-only) general case joins every
// term's own single-term phrasing.
func (s *solveState) describeGeneric(inc *incompatibility) string {
	switch len(inc.Terms) {
	case 0:
		return "version solving failed"
	case 1:
		return s.describeSingleTerm(inc.Terms[0])
	case dependencyPairTermCount:
		return s.describeTwoTerm(inc.Terms[0], inc.Terms[1])
	default:
		parts := make([]string, len(inc.Terms))
		for i, t := range inc.Terms {
			parts[i] = s.describeSingleTerm(t)
		}
		return strings.Join(parts, " and ")
	}
}

func (s *solveState) describeSingleTerm(t term) string {
	if t.Positive && t.Package == rootPkg {
		return "version solving failed"
	}
	label := s.termLabel(t.Package, t.Set)
	if t.Positive {
		return label + " is required"
	}
	return label + " is forbidden"
}

func (s *solveState) describeTwoTerm(a, b term) string {
	pos, neg := a, b
	if !pos.Positive {
		pos, neg = b, a
	}
	return fmt.Sprintf("%s requires %s", s.termLabel(pos.Package, pos.Set), s.termLabel(neg.Package, neg.Set))
}

// termLabel renders a human label for a (package, set) pair: "root" for the
// synthetic root (its single version is never shown), "every version of X"
// for a set covering the whole universe, "X <version>" for a singleton, and
// "X <display>" otherwise.
func (s *solveState) termLabel(pkg string, set versionSet) string {
	if pkg == rootPkg {
		return "root"
	}
	if set.isSingleton {
		return fmt.Sprintf("%s %s", pkg, set.singleton.Original())
	}
	if set.isAny() {
		return "every version of " + pkg
	}
	if set.kind == setBitset {
		u := s.uniFor(pkg)
		if len(u.versions) > 0 && popcount(set.bits) == len(u.versions) {
			return "every version of " + pkg
		}
	}
	if set.display != "" {
		return fmt.Sprintf("%s %s", pkg, set.display)
	}
	return pkg
}
