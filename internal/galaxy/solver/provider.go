// Package solver implements a PubGrub-style version solver: a pure,
// in-memory, deterministic algorithm for choosing one version per package
// that satisfies every declared constraint, or proving that no such
// selection exists. It performs no I/O of its own; all package metadata is
// obtained through the Provider seam, which the caller supplies.
package solver

import (
	"context"

	"github.com/Masterminds/semver/v3"
)

// rootPkg is the name of the synthetic root package. It is not a valid
// "ns.name" fully qualified collection name, so it can never collide with a
// real package, and its leading NUL byte sorts it before every real fqdn
// under byte-wise ordering, which the decision heuristic and root-requirement
// processing both rely on.
const rootPkg = "\x00root"

// rootVersionString is the single version of the synthetic root package.
const rootVersionString = "0.0.0"

// rootVersion is the root package's sole version, built once at package
// init.
//
//nolint:gochecknoglobals // a canonical, immutable singleton of the algorithm itself, not mutable shared state
var rootVersion = mustNewVersion(rootVersionString)

// mustNewVersion parses raw as a Version or panics. It exists only for the
// constant root version string above, which is guaranteed to parse.
// errSolverBug's own doc comment (solver.go) is the home of the
// panic-vs-error rule this function's own panic falls under.
func mustNewVersion(raw string) Version {
	v, err := NewVersion(raw)
	if err != nil {
		panic("solver: invalid constant version " + raw + ": " + err.Error())
	}
	return v
}

// Requirement is one root requirement to resolve: a package name paired with
// its constraint expression. The caller is responsible for validating and
// deduplicating requirements before calling Solve; the core does not sort,
// dedupe, or normalize reqs itself beyond processing them in Package order.
type Requirement struct {
	Package    string
	Constraint string
}

// Resolution maps a resolved package name to the original registry version
// string of the version Solve chose for it.
type Resolution map[string]string

// Constraint is a canonical constraint expression: the output of
// helpers.NormalizeConstraint, where the empty string means unconstrained
// (both raw "*" and raw empty normalize to ""). The core parses it with
// Masterminds/semver; a parse failure at that point is a provider contract
// violation and aborts the solve.
type Constraint = string

// Result is the successful outcome of Solve.
type Result struct {
	// Versions holds every decided package except the synthetic root,
	// mapped to the original registry string of its chosen version.
	Versions Resolution
	// Graph holds, for every package in Versions, the sorted list of its
	// dependency package names at the decided version. Every edge target is
	// itself a key in Versions.
	Graph map[string][]string
}

// Version pairs a parsed semver version with the original registry string it
// came from. Providers build Versions via NewVersion; the core never
// constructs one from raw user input except for the synthetic root version.
type Version struct {
	parsed   *semver.Version
	original string
}

// NewVersion parses raw as a semver version. Providers use this to build the
// Versions they hand to the core.
func NewVersion(raw string) (Version, error) {
	parsed, err := semver.NewVersion(raw)
	if err != nil {
		return Version{}, err
	}
	return Version{parsed: parsed, original: raw}, nil
}

// Original returns the original registry version string v was parsed from.
// This is what ends up in Resolution and in user-facing proof text.
func (v Version) Original() string {
	return v.original
}

// sv returns the parsed *semver.Version backing v, for in-package use
// (membership checks and precedence comparisons). It is the sole accessor
// the rest of the package uses to reach into a Version.
func (v Version) sv() *semver.Version {
	return v.parsed
}

// Provider is the seam between the solver core and package metadata. All
// three methods must be safe for concurrent use if the caller drives
// multiple solves concurrently, though a single Solve call drives the
// provider from one goroutine only. An implementation must carry the supplied
// ctx into every I/O it performs rather than substituting one of its own, and
// must surface a cancellation observed there as an error whose tree still
// satisfies errors.Is(err, ctx.Err()). A call answered entirely from an
// in-memory cache performs no I/O and is under no obligation to check ctx
// itself: Solve's own per-iteration check is what bounds that case.
type Provider interface {
	// Highest returns the registry-reported highest version of pkg, with NO
	// constraint checking performed by the provider - the core checks
	// membership itself. ok reports whether the package is known and a
	// highest version is available; when ok is false (or err is non-nil),
	// the core falls back to Universe.
	Highest(ctx context.Context, pkg string) (Version, bool, error)

	// Universe returns every published version of pkg, deduplicated by
	// original string. The core re-sorts the result into its own total
	// order regardless of what order Universe returns, so the provider
	// contract states an order only as a defense-in-depth convention, not a
	// correctness requirement. An unknown package returns an empty slice
	// and a nil error.
	Universe(ctx context.Context, pkg string) ([]Version, error)

	// Dependencies returns the validated dependency map of pkg@v: dependency
	// fqdn mapped to its canonical Constraint. Key and constraint validation
	// happens inside Dependencies itself; a malformed dependency key or
	// constraint is a provider contract violation and must be surfaced as an
	// error from this method, never guessed at by the core.
	Dependencies(ctx context.Context, pkg string, v Version) (map[string]Constraint, error)
}
