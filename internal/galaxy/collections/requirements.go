package collections

import (
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/requirements"
)

// loadRequirements parses the requirements file, turning its collection
// entries into internal structs and handing its role entries and parse
// warnings back as parsed.
func loadRequirements(path, defaultSource string) ([]collection, requirements.File, error) {
	file, err := requirements.Load(path, defaultSource)
	if err != nil {
		return nil, requirements.File{}, err
	}
	collections := make([]collection, 0, len(file.Collections))
	for _, req := range file.Collections {
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
	return collections, file, nil
}
