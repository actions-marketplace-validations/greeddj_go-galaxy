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

// Client is the seam between the install pipeline and a git remote. Advertise
// answers which commit a ref currently points at with one advertisement round
// trip and no pack transfer; Acquire does the whole acquisition. Every error
// wraps one of the helpers git sentinels so the caller can classify it, and
// both honor ctx cancellation and deadlines.
type Client interface {
	Advertise(ctx context.Context, u URL, ref Ref, auth Credential) (commit, refName string, err error)
	Acquire(ctx context.Context, req Request) (Result, error)
}
