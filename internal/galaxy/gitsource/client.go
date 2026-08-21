package gitsource

import (
	"context"
	"os"
)

// Identity is the namespace, name and exact version a built collection
// declares in its galaxy.yml.
type Identity struct {
	Namespace string
	Name      string
	Version   string
}

// TempFileFunc hands the builder a temporary file to write an artifact into,
// with a cleanup that removes it. The pipeline supplies the artifact store's
// own TempFile so a committed artifact is a rename, never a copy, and so a
// temp file left by a killed run is swept by the same prefix rule every
// download temp is.
type TempFileFunc func(ctx context.Context) (*os.File, func(), error)

// Request is one acquisition. With Commit empty, Ref is resolved against the
// remote's advertised refs; with Commit set, exactly that commit is fetched
// and Ref is only a hint for the cheapest way to reach it (the lockfile knows
// which ref the commit came from). Only, when set, restricts the build to that
// one collection of the repository - the install-time rebuild of a pinned
// collection - and its absence, or a different identity under its subdir, is
// an error rather than a silent substitution.
type Request struct {
	Only     *Identity
	TempFile TempFileFunc
	URL      URL
	Ref      Ref
	Subdir   string
	Commit   string
	Auth     Credential
}

// Collection is one built artifact: its identity, the subdir it was built
// from (relative to the repository root, "" for the root), the raw galaxy.yml
// dependencies for the caller to validate, the temp path of the tar.gz and the
// sha256 computed while writing it. Cleanup removes the temp file and is the
// caller's duty on every path.
type Collection struct {
	Cleanup      func()
	Dependencies map[string]string
	Namespace    string
	Name         string
	Version      string
	Subdir       string
	ArtifactPath string
	ArtifactSHA  string
}

// Identity returns the collection's identity triple.
func (c Collection) Identity() Identity {
	return Identity{Namespace: c.Namespace, Name: c.Name, Version: c.Version}
}

// Result is what an acquisition produced: the commit the request resolved to,
// the full ref name it was reached through ("HEAD" when HEAD was asked for
// and the remote did not say what it points at), the built collections, the
// warnings the build raised (skipped submodules and out-of-tree symlinks,
// unknown galaxy.yml keys, an ambiguous name) for the caller to print, and
// the bytes the fetch wrote to disk for the caller to count.
type Result struct {
	Commit       string
	RefName      string
	Collections  []Collection
	Warnings     []string
	BytesFetched int64
}

// RoleRequest is one role acquisition. Commit and Ref behave as in Request;
// a role has no subdir and no identity filter: the repository root is the
// role, and the install name is the caller's to choose, as it is in ansible,
// so nothing in the tree is compared against it.
type RoleRequest struct {
	TempFile TempFileFunc
	URL      URL
	Ref      Ref
	Commit   string
	Auth     Credential
}

// RoleDependency is one dependency a role's meta declares, normalized to
// ansible's spec keys and otherwise unjudged: the caller validates it
// through the requirements grammar exactly as it validates
// Collection.Dependencies through the constraint grammar. Src is the Galaxy
// name or the repository URL as written, Scm the scm when one was spelled,
// Version the version as written and Name the install name when one was
// given; an empty field was not written.
type RoleDependency struct {
	Src     string
	Scm     string
	Version string
	Name    string
}

// RoleResult is the built role artifact: the commit the request resolved
// to, the full ref name it was reached through (as Result.RefName), the
// dependencies its meta declares, the temp path of the tar.gz and the sha256
// computed while writing it, the warnings the build raised for the caller to
// print, and the bytes the fetch wrote to disk for the caller to count.
// GalaxyRoleName is the galaxy_info.role_name the meta carries, when it
// does; it is informational, since the install directory is the
// requirement's name, as in ansible. Cleanup removes the temp file and is
// the caller's duty on every path.
type RoleResult struct {
	Cleanup        func()
	Dependencies   []RoleDependency
	Commit         string
	RefName        string
	GalaxyRoleName string
	ArtifactPath   string
	ArtifactSHA    string
	Warnings       []string
	BytesFetched   int64
}

// Client is the seam between the install pipeline and a git remote. Advertise
// answers which commit a ref currently points at with one advertisement round
// trip and no pack transfer; Acquire does the whole acquisition of a
// repository's collections; AcquireRole does the same for the role a
// repository's root is. Every error wraps one of the helpers git sentinels so
// the caller can classify it, and all three honor ctx cancellation and
// deadlines.
type Client interface {
	Advertise(ctx context.Context, u URL, ref Ref, auth Credential) (commit, refName string, err error)
	Acquire(ctx context.Context, req Request) (Result, error)
	AcquireRole(ctx context.Context, req RoleRequest) (RoleResult, error)
}
