// Package tartree is the one place a foreign tar.gz becomes a
// treearchive.Source: it turns the tarball a url role source serves into the
// tree internal/galaxy/rolebuild repacks into this tool's canonical role
// artifact. A url tarball is the one artifact class whose layout the origin
// chose - a role built by `ansible-galaxy role import` sits at the archive
// root, a GitHub-generated archive wraps everything in one release directory
// - so this package owns the rule that reconciles both: the role root is the
// archive root when meta/main.yml (or meta/main.yaml, counted only as a
// regular file) sits there, else the single top-level directory carrying one;
// no such directory, or more than one, is helpers.ErrRoleTarballLayout. That
// is ansible's shortest-parent-of-meta/main.yml scan, held to depth two.
//
// The bytes are untrusted, and nothing here reads them directly: Load hands
// the tarball to internal/galaxy/archive's extractor, which owns every
// boundary an untrusted archive crosses - the size, count and name caps, the
// path and symlink refusals, the decompression budget, gzip opened only
// through internal/gzipstream - and extracts into a private directory under
// the run's temp dir. The Tree then serves that directory through an os.Root,
// so no name it resolves can reach outside it, entries are listed in byte
// order (the extractor flattened whatever order the tar held, so byte order
// is the one deterministic choice left), a symlink's blob is its own target
// string read with Readlink rather than followed, and a name the extractor
// tolerated on disk but this tool's own extractor would refuse on the way
// back out - a control rune, a backslash - is
// helpers.ErrRoleTarballEntryInvalid at the read.
//
// CommitTime is the Unix epoch for every tree: a url tarball names no commit,
// and a fixed stamp is what keeps the repack deterministic - one origin
// artifact yields one repacked artifact under one toolchain, whatever
// mtimes the extraction left on disk.
package tartree
