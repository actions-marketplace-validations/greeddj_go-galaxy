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
//
// A git root may have no identity yet (its repository's galaxy.yml supplies
// one at discovery), so its duplicate check is by locator rather than by
// name; a git root that does name its collection is checked by name as well,
// the same way a Galaxy root is. The invariant that a root is a git root
// exactly when its Source is a locator is asserted here because everything
// downstream dispatches on the locator prefix alone.
func prepareRoots(roots []collection) ([]collection, error) {
	prepared := make([]collection, 0, len(roots))
	seen := make(map[string]collection)
	addRoot := func(key string, col collection) error {
		if existing, ok := seen[key]; ok {
			return fmt.Errorf("%w for %s (type %s vs %s)", helpers.ErrDuplicateCollectionRequirement, key, existing.Type, col.Type)
		}
		seen[key] = col
		return nil
	}

	for _, root := range roots {
		if err := normalizeRootType(&root); err != nil {
			return nil, err
		}
		if root.isGit() {
			if err := addRoot(root.Source, root); err != nil {
				return nil, err
			}
			if root.Namespace == "" && root.Name == "" {
				prepared = append(prepared, root)
				continue
			}
		}
		if err := splitRootName(&root); err != nil {
			return nil, err
		}
		if err := addRoot(fmt.Sprintf("%s.%s", root.Namespace, root.Name), root); err != nil {
			return nil, err
		}
		prepared = append(prepared, root)
	}

	return prepared, nil
}

// splitRootName fills a root's namespace and name from a dotted Name when
// either half is missing; a Name that is not a valid fqdn is refused.
func splitRootName(root *collection) error {
	if root.Namespace != "" && root.Name != "" {
		return nil
	}
	namespace, name, ok := helpers.SplitFQDN(root.Name)
	if !ok {
		return fmt.Errorf("%w: %q", helpers.ErrInvalidCollectionName, root.Name)
	}
	root.Namespace = namespace
	root.Name = name
	return nil
}

// normalizeRootType canonicalizes root's type (empty means galaxy), refuses
// a type other than galaxy or git, and enforces that a root is typed git
// exactly when its Source is a git locator.
func normalizeRootType(root *collection) error {
	root.Type = normalizeType(root.Type)
	if root.Type == "" {
		root.Type = typeGalaxy
	}
	if !isSupportedType(root.Type) {
		return fmt.Errorf("%w: %q (only galaxy and git are supported)", helpers.ErrUnsupportedCollectionType, root.Type)
	}
	if (root.Type == typeGit) != root.isGit() {
		return fmt.Errorf("%w: type %q does not match source %q", helpers.ErrInvalidCollectionEntry,
			root.Type, helpers.URLForMessage(root.Source))
	}
	return nil
}

// normalizeType normalizes a collection type string.
func normalizeType(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

// isGalaxyType reports whether the type is a Galaxy type (the empty type
// is Galaxy).
func isGalaxyType(value string) bool {
	normalized := normalizeType(value)
	return normalized == "" || normalized == typeGalaxy
}

// isSupportedType reports whether the type is one this tool resolves: Galaxy
// or git.
func isSupportedType(value string) bool {
	return isGalaxyType(value) || normalizeType(value) == typeGit
}
