package collections

import (
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/requirements"
)

// loadRequirements parses collection requirements into internal structs.
func loadRequirements(path, defaultSource string) ([]collection, bool, error) {
	reqs, rolesFound, err := requirements.LoadCollections(path, defaultSource)
	if err != nil {
		return nil, false, err
	}
	collections := make([]collection, 0, len(reqs))
	for _, req := range reqs {
		if req.IsGit() {
			// The locator carries no commit yet; expandGitRoots pins it.
			// Constraint holds the ref so the requirements signature and the
			// lockfile check both see what the file asked for.
			collections = append(collections, collection{
				Namespace:  req.Namespace,
				Name:       req.Name,
				Source:     gitsource.Locator{URL: req.Source, Subdir: req.Subdir}.String(),
				Constraint: req.Ref,
				Type:       typeGit,
				Ref:        req.Ref,
			})
			continue
		}
		collections = append(collections, collection{
			Namespace:  req.Namespace,
			Name:       req.Name,
			Version:    req.Version,
			Source:     req.Source,
			Signatures: req.Signatures,
			Constraint: req.Version,
			Type:       req.Type,
		})
	}
	return collections, rolesFound, nil
}
