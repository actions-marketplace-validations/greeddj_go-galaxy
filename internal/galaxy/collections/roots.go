package collections

import (
	"fmt"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// prepareRoots normalizes and validates root requirements, returning the
// normalized roots. A root's empty Source is left as-is: unpinned is now a
// distinct, stable value that lets the root walk the configured server list,
// rather than being nailed to a single default server here.
func prepareRoots(roots []collection) ([]collection, error) {
	prepared := make([]collection, 0, len(roots))
	seen := make(map[string]collection)
	addRoot := func(col collection) error {
		fqdn := fmt.Sprintf("%s.%s", col.Namespace, col.Name)
		if existing, ok := seen[fqdn]; ok {
			return fmt.Errorf("%w for %s (type %s vs %s)", helpers.ErrDuplicateCollectionRequirement, fqdn, existing.Type, col.Type)
		}
		seen[fqdn] = col
		return nil
	}

	for _, root := range roots {
		root.Type = normalizeType(root.Type)
		if root.Type == "" {
			root.Type = "galaxy"
		}
		if !isGalaxyType(root.Type) {
			return nil, fmt.Errorf("%w: %q (only galaxy is supported)", helpers.ErrUnsupportedCollectionType, root.Type)
		}
		if root.Namespace == "" || root.Name == "" {
			namespace, name, ok := helpers.SplitFQDN(root.Name)
			if !ok {
				return nil, fmt.Errorf("%w: %q", helpers.ErrInvalidCollectionName, root.Name)
			}
			root.Namespace = namespace
			root.Name = name
		}
		if err := addRoot(root); err != nil {
			return nil, err
		}
		prepared = append(prepared, root)
	}

	return prepared, nil
}

// normalizeType normalizes a collection type string.
func normalizeType(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

// isGalaxyType reports whether the type is a supported galaxy type.
func isGalaxyType(value string) bool {
	normalized := normalizeType(value)
	return normalized == "" || normalized == "galaxy"
}
