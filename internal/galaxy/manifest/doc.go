// Package manifest reads and cross-checks the metadata documents a collection
// artifact carries about itself.
//
// It writes nothing, and that is a boundary rather than an implementation
// detail: an entry point here takes a path to a file this run already
// downloaded or produced and hands back bytes, so nothing in this package can
// create, replace or delete a byte anywhere - not under a project's collections
// tree, not under the cache, not even a temporary file of its own. The
// documents read here decide whether an install proceeds, so the code reading
// them is deliberately unable to change what is installed, the same separation
// internal/galaxy/signature draws around the verdicts it computes.
//
// Nothing here resolves a path through an os.Root, and nothing needs to: the
// caller always names an absolute path to a file it produced itself, never one
// walked out of an archive or read back from a directory listing.
package manifest
