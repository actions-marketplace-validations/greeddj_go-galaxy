package collections

import (
	"context"
	"fmt"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
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
) (*lockfile.File, error) {
	cfg := deps.cfg
	entries := make([]lockfile.Entry, 0, len(resolved))
	for fqdn, col := range resolved {
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
		// break every such server.
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
		SchemaVersion: lockfile.SchemaVersion,
		Server:        cfg.Server,
		Collections:   entries,
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
	prep *rootPreparation,
) (map[string]collection, map[string][]string, error) {
	byFQDN, err := indexLockfile(lf, cfg)
	if err != nil {
		return nil, nil, err
	}
	if err := verifyRootsAgainstLockfile(prep, byFQDN); err != nil {
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
		if entry.Source == "" {
			entry.Source = cfg.Server
		}
		out[e.Name] = entry
	}
	return out, nil
}

func verifyRootsAgainstLockfile(prep *rootPreparation, byFQDN map[string]lockfile.Entry) error {
	for _, root := range prep.AllRoots {
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
		resolved[fqdn] = col
		graph[col.key()] = lockfileDepsToKeys(e.Deps, byFQDN)
	}
	return resolved, graph, nil
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
