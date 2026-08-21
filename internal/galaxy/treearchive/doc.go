// Package treearchive turns a source tree into a deterministic tar.gz. It is
// the one production tar writer in this module: internal/galaxy/collectionbuild
// and internal/galaxy/rolebuild decide what a tree means - which documents
// lead the archive, which paths the build ignores, what identity the result
// carries - and hand the walking and the writing to this package, so a
// collection artifact and a role artifact share one byte shape, one set of
// budgets and one symlink policy.
//
// The tree is read through Source, never from the filesystem, so nothing here
// opens a path, follows a real symlink or execs anything: a git tree at one
// commit is the production Source, and every name it hands over is already
// validated by that implementation. The package's own boundaries are the
// archive caps declared in internal/galaxy/helpers - entry count, per-entry
// size, total declared size, name length, tree depth - charged during the
// walk so an oversized tree is refused before its first byte is written, and
// re-checked by the extractor on the way out.
//
// A build is two passes over the tree. PlanTree decides every entry in the
// tree's own order, parents before children, applies the caller's Rules,
// charges the budgets, resolves every symlink and, when asked, hashes every
// blob so the caller can list the tree ahead of it; nothing is written. Write
// then streams the caller's lead documents and the planned entries into a
// file, digesting the compressed bytes as they go. Two passes rather than one
// because the documents that lead a collection artifact describe the tree
// behind them, and the one place the two orders can be reconciled is before
// the first byte.
//
// The artifact is deterministic: every entry carries the commit's committer
// time, fixed modes and no owner, the gzip header is left zeroed, and entries
// follow the tree's own order behind the lead documents, so two builds of one
// commit produce one byte sequence and one sha256. A symlink is written
// pointing at the final entry its chain resolves to, relative to the link's
// own directory, because internal/galaxy/manifest resolves a link against the
// archive in one lookup per hop over real entries and the extractor refuses a
// path whose component is a link. A link whose target the artifact will not
// carry - outside the tree, through a submodule, under an ignored path, its
// own directory - is skipped with a warning, as ansible skips a link leaving
// a collection; a dangling or looping one is refused.
package treearchive
