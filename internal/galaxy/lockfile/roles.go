package lockfile

import (
	"fmt"
	"slices"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// SchemaVersionRoles is the schema of a file carrying at least one role
// entry. It differs from SchemaVersionGit only in that the file has a roles
// list; SchemaVersionFor picks it from the entries.
const SchemaVersionRoles = 3

// Role entry types. A role always has one: a Galaxy role was looked up by
// name on a Galaxy server, a git role was named by its repository.
const (
	RoleTypeGalaxy = "galaxy"
	RoleTypeGit    = "git"
)

// RoleEntry is a single pinned role in the lockfile. Name is the directory
// the role installs into under roles_path. Type is galaxy or git. Version is
// the concrete version installed - a tag, a branch name, or the commit the
// requirement spelled - and is not an exact version in the semver sense,
// since a branch is a legal role version; the commit is the pin. For a
// Galaxy role, Galaxy is the Galaxy name (owner.role), Source the Galaxy
// server it was looked up on and Repository the git repository that server
// pointed at; for a git role, Source is the repository URL and Repository
// and Galaxy are empty. Ref is the ref the requirement asked for and Commit
// the commit it resolved to, which is what a frozen install fetches when the
// artifact is not cached. Deps are the install names of the roles this
// role's meta depends on that the run installed. A role entry carries no
// sha256 for the reason Entry gives for a git entry: its artifact is rebuilt
// from the commit, and the bytes of a rebuild depend on the toolchain.
type RoleEntry struct {
	Name       string   `yaml:"name"`
	Type       string   `yaml:"type"`
	Version    string   `yaml:"version"`
	Galaxy     string   `yaml:"galaxy,omitempty"`
	Source     string   `yaml:"source"`
	Repository string   `yaml:"repository,omitempty"`
	Ref        string   `yaml:"ref"`
	Commit     string   `yaml:"commit"`
	Deps       []string `yaml:"deps,omitempty"`
}

// IsGit reports whether the role was named by its repository rather than
// looked up on a Galaxy server.
func (e RoleEntry) IsGit() bool { return e.Type == RoleTypeGit }

// RepositoryURL is the one repository a frozen install fetches the role
// from: Repository for a Galaxy role, Source for a git role.
func (e RoleEntry) RepositoryURL() string {
	if e.IsGit() {
		return e.Source
	}
	return e.Repository
}

// validateRoles judges every role entry: a unique install name in the role
// alphabet, a type, a version in the persisted-version shape, a canonical
// ref, a full commit, a canonical repository URL where one belongs and none
// where it does not, no userinfo in the source, dependency names in the
// alphabet, and a file whose schema admits roles at all - a schema-1 or
// schema-2 file carrying a role has been edited by hand, since the schema
// is what tells an older binary to stop.
func (f *File) validateRoles() error {
	seen := make(map[string]struct{}, len(f.Roles))
	for _, e := range f.Roles {
		if !helpers.IsRoleInstallName(e.Name) {
			return fmt.Errorf("%w: role name %q is not a role install name", helpers.ErrLockfileInvalid, e.Name)
		}
		if _, dup := seen[e.Name]; dup {
			return fmt.Errorf("%w: duplicate role name %s", helpers.ErrLockfileInvalid, e.Name)
		}
		seen[e.Name] = struct{}{}
		if f.SchemaVersion != SchemaVersionRoles {
			return fmt.Errorf("%w: role %s: a role entry requires schema_version %d", helpers.ErrLockfileInvalid, e.Name, SchemaVersionRoles)
		}
		if reason := roleEntryProblem(e); reason != "" {
			return fmt.Errorf("%w: role %s: %s", helpers.ErrLockfileInvalid, e.Name, reason)
		}
	}
	return nil
}

// roleEntryProblem returns why a role entry is refused, or "" when every
// field is canonical. The source and repository are re-parsed rather than
// trusted, since a lockfile is repository content: anything a later run
// connects to has to pass the grammar a requirements entry does.
func roleEntryProblem(e RoleEntry) string {
	if !helpers.IsRoleVersion(e.Version) {
		return fmt.Sprintf("version %q is not a role version", e.Version)
	}
	if ref, err := gitsource.ParseRef(e.Ref); err != nil || e.Ref == "" || ref.Name != e.Ref {
		return fmt.Sprintf("ref %q is not a canonical git ref", e.Ref)
	}
	if !gitsource.IsCommitHash(e.Commit) {
		return fmt.Sprintf("commit %q is not a lowercase 40-hex commit", e.Commit)
	}
	if dep, ok := firstBadDep(e.Deps); !ok {
		return fmt.Sprintf("dependency %q is not a role install name", dep)
	}
	switch e.Type {
	case RoleTypeGit:
		return gitRoleEntryProblem(e)
	case RoleTypeGalaxy:
		return galaxyRoleEntryProblem(e)
	default:
		return fmt.Sprintf("unsupported role type %q", e.Type)
	}
}

// firstBadDep returns the first dependency name outside the role install
// alphabet, or ok when every one is inside it.
func firstBadDep(deps []string) (string, bool) {
	for _, dep := range deps {
		if !helpers.IsRoleInstallName(dep) {
			return dep, false
		}
	}
	return "", true
}

func gitRoleEntryProblem(e RoleEntry) string {
	if u, err := gitsource.ParseURL(e.Source); err != nil || u.String() != e.Source {
		return "source is not a canonical git repository URL"
	}
	if e.Galaxy != "" || e.Repository != "" {
		return "galaxy and repository belong to a galaxy role"
	}
	return ""
}

func galaxyRoleEntryProblem(e RoleEntry) string {
	if !helpers.IsRoleName(e.Galaxy) {
		return fmt.Sprintf("galaxy %q is not owner.role", e.Galaxy)
	}
	if u, err := gitsource.ParseURL(e.Repository); err != nil || u.String() != e.Repository {
		return "repository is not a canonical git repository URL"
	}
	if sourceHasUserinfo(e.Source) {
		return helpers.ErrGalaxyServerURLUserinfo.Error()
	}
	return ""
}

// cloneRoles deep-copies the roles list and each entry's Deps, for
// canonicalClone.
func cloneRoles(roles []RoleEntry) []RoleEntry {
	if roles == nil {
		return nil
	}
	out := make([]RoleEntry, len(roles))
	copy(out, roles)
	for i := range out {
		if len(roles[i].Deps) == 0 {
			continue
		}
		out[i].Deps = slices.Clone(roles[i].Deps)
	}
	return out
}

// canonicalizeRoles sorts the roles by name and each entry's Deps.
func canonicalizeRoles(roles []RoleEntry) {
	slices.SortFunc(roles, func(a, b RoleEntry) int { return strings.Compare(a.Name, b.Name) })
	for i := range roles {
		slices.Sort(roles[i].Deps)
	}
}

// RoleChange is one role present in both before and after whose pinned
// fields differ; see Change.
type RoleChange struct {
	From RoleEntry
	To   RoleEntry
}

// comparedRoleFieldCount is the number of per-role fields Fields can report.
const comparedRoleFieldCount = 8

// Fields reports which of c's pinned fields differ between From and To, in
// a fixed order (type, version, galaxy, source, repository, ref, commit,
// deps); see Change.Fields.
func (c RoleChange) Fields() []FieldChange {
	fields := make([]FieldChange, 0, comparedRoleFieldCount)
	add := func(name, from, to string) {
		if from != to {
			fields = append(fields, FieldChange{Field: name, From: from, To: to})
		}
	}
	add(fieldType, c.From.Type, c.To.Type)
	add(fieldVersion, c.From.Version, c.To.Version)
	add(fieldGalaxy, c.From.Galaxy, c.To.Galaxy)
	add(fieldSource, c.From.Source, c.To.Source)
	add(fieldRepository, c.From.Repository, c.To.Repository)
	add(fieldRef, c.From.Ref, c.To.Ref)
	add(fieldCommit, c.From.Commit, c.To.Commit)
	if !sameDeps(c.From.Deps, c.To.Deps) {
		fields = append(fields, FieldChange{Field: fieldDeps, From: renderDeps(c.From.Deps), To: renderDeps(c.To.Deps)})
	}
	return fields
}

// sameRoleEntry reports whether a and b pin the same role; Name is not
// compared, as in sameEntry.
func sameRoleEntry(a, b RoleEntry) bool {
	return a.Type == b.Type && a.Version == b.Version && a.Galaxy == b.Galaxy && a.Source == b.Source &&
		a.Repository == b.Repository && a.Ref == b.Ref && a.Commit == b.Commit && sameDeps(a.Deps, b.Deps)
}

// indexRolesByName builds a name-keyed index of f's roles; a nil f yields
// an empty map, as indexByName does.
func indexRolesByName(f *File) map[string]RoleEntry {
	if f == nil {
		return map[string]RoleEntry{}
	}
	idx := make(map[string]RoleEntry, len(f.Roles))
	for _, e := range f.Roles {
		idx[e.Name] = e
	}
	return idx
}

// diffRoles builds RolesAdded/RolesUpdated/RolesRemoved over the sorted
// name union of the two indexes, as diffIndexes does for collections.
func diffRoles(beforeIdx, afterIdx map[string]RoleEntry) ([]RoleEntry, []RoleChange, []RoleEntry) {
	names := unionSortedRoleNames(beforeIdx, afterIdx)
	var added, removed []RoleEntry
	var updated []RoleChange
	for _, name := range names {
		a, inAfter := afterIdx[name]
		b, inBefore := beforeIdx[name]
		switch {
		case inAfter && !inBefore:
			added = append(added, a)
		case inAfter && inBefore:
			if !sameRoleEntry(a, b) {
				updated = append(updated, RoleChange{From: b, To: a})
			}
		case !inAfter && inBefore:
			removed = append(removed, b)
		}
	}
	return added, updated, removed
}

// unionSortedRoleNames is unionSortedNames for the role indexes.
func unionSortedRoleNames(beforeIdx, afterIdx map[string]RoleEntry) []string {
	names := make([]string, 0, len(beforeIdx)+len(afterIdx))
	for name := range afterIdx {
		names = append(names, name)
	}
	for name := range beforeIdx {
		if _, ok := afterIdx[name]; !ok {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}
