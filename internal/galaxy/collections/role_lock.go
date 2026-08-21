package collections

import (
	"fmt"
	"slices"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/lockfile"
	"github.com/greeddj/go-galaxy/internal/galaxy/requirements"
)

// roleLockfileEntries renders every resolved role as a lockfile entry, in
// install-name order. A Galaxy role records the server whose v1 API answered
// for it as its source (the run's default when a replayed pin predates that
// record) and the repository the server pointed at; a git role
// records the repository as its source. Both record the ref asked for and
// the commit it resolved to, and no sha256 (see lockfile.RoleEntry). The
// locator is taken apart so the file a human reviews names the repository,
// not an internal key.
func roleLockfileEntries(cfg *config.Config, roles roleResolution) ([]lockfile.RoleEntry, error) {
	if len(roles.roles) == 0 {
		return nil, nil
	}
	names := slices.Sorted(func(yield func(string) bool) {
		for name := range roles.roles {
			if !yield(name) {
				return
			}
		}
	})
	entries := make([]lockfile.RoleEntry, 0, len(names))
	for _, name := range names {
		entry, err := roleLockfileEntry(cfg, roles.roles[name])
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func roleLockfileEntry(cfg *config.Config, r resolvedRole) (lockfile.RoleEntry, error) {
	loc, err := gitsource.ParseLocator(r.Source)
	if err != nil {
		return lockfile.RoleEntry{}, fmt.Errorf("lockfile: role %s: %w", r.Name, err)
	}
	if !loc.Pinned() {
		return lockfile.RoleEntry{}, fmt.Errorf("lockfile: role %s: %w: role is not pinned to a commit", r.Name, helpers.ErrInvalidGitLocator)
	}
	entry := lockfile.RoleEntry{
		Name:    r.Name,
		Version: r.Version,
		Ref:     r.Ref,
		Commit:  loc.Commit,
		Deps:    slices.Clone(r.Deps),
	}
	if r.Kind == requirements.TypeGalaxy {
		entry.Type = lockfile.RoleTypeGalaxy
		entry.Galaxy = r.GalaxyName
		entry.Source = r.Server
		if entry.Source == "" {
			entry.Source = cfg.Server
		}
		entry.Repository = loc.URL
		return entry, nil
	}
	entry.Type = lockfile.RoleTypeGit
	entry.Source = loc.URL
	return entry, nil
}

// resolveRolesFromLockfile is resolveFromLockfile for the roles list: every
// requirement must be locked as it is written, and every role entry becomes
// a resolved role whose artifact the install fetches from the cache or, on
// a miss, by the pinned commit - exactly as a git collection is materialized.
// No network is touched here.
func resolveRolesFromLockfile(lf *lockfile.File, roots []requirements.RoleRequirement) (roleResolution, error) {
	byName := make(map[string]lockfile.RoleEntry, len(lf.Roles))
	for _, e := range lf.Roles {
		byName[e.Name] = e
	}
	for _, root := range roots {
		if err := verifyRoleRootAgainstLockfile(root, byName); err != nil {
			return roleResolution{}, err
		}
	}
	res := roleResolution{roles: make(map[string]resolvedRole, len(lf.Roles)), order: make([]string, 0, len(lf.Roles))}
	for _, e := range lf.Roles {
		kind := requirements.TypeGit
		if !e.IsGit() {
			kind = requirements.TypeGalaxy
		}
		role := resolvedRole{
			Deps:       slices.Clone(e.Deps),
			Name:       e.Name,
			Source:     gitsource.Locator{URL: e.RepositoryURL(), Commit: e.Commit}.String(),
			Repository: e.RepositoryURL(),
			Ref:        e.Ref,
			Version:    e.Version,
			GalaxyName: e.Galaxy,
			Kind:       kind,
		}
		if !e.IsGit() {
			role.Server = e.Source
		}
		res.roles[e.Name] = role
		res.order = append(res.order, e.Name)
	}
	return res, nil
}

// verifyRoleRootAgainstLockfile checks one roles: entry, as written, against
// the lockfile: the install name must be locked, from the same source - the
// Galaxy name for a Galaxy role, the repository for a git role - and from
// the same ref; for a Galaxy role with a version asked for, that version. A
// ref change is a mismatch even when the commit happens to be the same, for
// the reason verifyGitRootAgainstLockfile gives.
func verifyRoleRootAgainstLockfile(root requirements.RoleRequirement, byName map[string]lockfile.RoleEntry) error {
	entry, ok := byName[root.Name]
	if !ok {
		return fmt.Errorf("%w: role %s missing", helpers.ErrLockfileMismatch, root.Name)
	}
	if root.IsGit() {
		if !entry.IsGit() || entry.Source != root.Src {
			return fmt.Errorf("%w: role %s locked from %s, requirements ask for %s",
				helpers.ErrLockfileMismatch, root.Name, roleEntryOrigin(entry), helpers.URLForMessage(root.Src))
		}
		if entry.Ref != root.Version {
			return fmt.Errorf("%w: role %s locked from ref %q, requirements ask for %q",
				helpers.ErrLockfileMismatch, root.Name, entry.Ref, root.Version)
		}
		return nil
	}
	if entry.IsGit() || entry.Galaxy != root.Src {
		return fmt.Errorf("%w: role %s locked from %s, requirements ask for Galaxy role %s",
			helpers.ErrLockfileMismatch, root.Name, roleEntryOrigin(entry), root.Src)
	}
	if root.Version != "" && entry.Version != root.Version {
		return fmt.Errorf("%w: role %s locked at %q, requirements ask for %q",
			helpers.ErrLockfileMismatch, root.Name, entry.Version, root.Version)
	}
	return nil
}

// roleEntryOrigin renders where a locked role came from, for a message.
func roleEntryOrigin(e lockfile.RoleEntry) string {
	if e.IsGit() {
		return helpers.URLForMessage(e.Source)
	}
	return "Galaxy role " + e.Galaxy
}
