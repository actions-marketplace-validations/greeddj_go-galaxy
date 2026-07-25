package collections

import (
	"fmt"
	"sort"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// constraintSource pairs a version constraint with the parent that imposed it
// (a parent FQDN like "amazon.aws", or "root" for the requirements file).
type constraintSource struct {
	Constraint string
	Source     string
}

// conflictError describes a collection that cannot be resolved because no
// available version satisfies the union of its constraints. It enumerates
// every (constraint, parent) pair so users can see which dependencies
// conflict — the equivalent of pip's "ResolutionImpossible" message.
type conflictError struct {
	FQDN    string
	Sources []constraintSource
}

func (e *conflictError) Error() string {
	if len(e.Sources) == 0 {
		return e.FQDN + ": no version satisfies constraints"
	}
	sorted := make([]constraintSource, len(e.Sources))
	copy(sorted, e.Sources)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Source != sorted[j].Source {
			return sorted[i].Source < sorted[j].Source
		}
		return sorted[i].Constraint < sorted[j].Constraint
	})

	var b strings.Builder
	fmt.Fprintf(&b, "no version of %s satisfies all constraints:", e.FQDN)
	for _, s := range sorted {
		fmt.Fprintf(&b, "\n  - %s (required by %s)", s.Constraint, prettySource(s.Source))
	}
	return b.String()
}

// Is reports that this is a constraint-conflict error so callers can
// errors.Is-check against the sentinel.
func (e *conflictError) Is(target error) bool {
	return target == helpers.ErrNoVersionSatisfiesConstraints
}

// prettySource humanizes the source label for the error message.
func prettySource(source string) string {
	switch source {
	case "", "root":
		return "requirements.yml"
	default:
		return source
	}
}

// constraintSourcesFor returns the (constraint, source) pairs for a fqdn.
// The result mirrors constraintsFor() but preserves which parent imposed
// each constraint.
func constraintSourcesFor(depConstraints map[string]map[string]string, fqdn string) []constraintSource {
	sources := depConstraints[fqdn]
	if len(sources) == 0 {
		return nil
	}
	out := make([]constraintSource, 0, len(sources))
	for source, c := range sources {
		normalized := helpers.NormalizeConstraint(c)
		if normalized == "" {
			continue
		}
		out = append(out, constraintSource{Constraint: normalized, Source: source})
	}
	return out
}

// constraintStringsFromSources returns just the constraint strings.
func constraintStringsFromSources(sources []constraintSource) []string {
	if len(sources) == 0 {
		return nil
	}
	out := make([]string, 0, len(sources))
	for _, s := range sources {
		out = append(out, s.Constraint)
	}
	return out
}
