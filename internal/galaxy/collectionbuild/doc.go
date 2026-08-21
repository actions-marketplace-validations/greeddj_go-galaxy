// Package collectionbuild turns a collection source tree into the tar.gz
// artifact the rest of the pipeline already knows how to verify, cache and
// extract. It owns the build half of what `ansible-galaxy collection build`
// does, and reproduces it where a reader of the result can tell: the
// galaxy.yml schema and its mandatory keys, the MANIFEST.json-only fallback
// for a tree that ships a built collection, the one-level discovery rule for
// a repository holding several collections, the default ignore list with
// build_ignore appended and matched with Python's fnmatch semantics, and the
// MANIFEST.json and FILES.json row shapes. It diverges from ansible where
// ansible's choice would leave this tool without a usable identity or
// artifact, and each divergence is a named refusal rather than a silent
// substitute: a version that is not exact (ansible installs "*"), a manifest:
// directive block (needs distlib; build_ignore is the supported way), a
// directory carrying both galaxy.yml and MANIFEST.json (ansible's dir
// classification refuses it too, but only for a path source), and a symlink
// the extractor would refuse or the chain check could not follow.
//
// The tree is read through Source, never from the filesystem, so nothing here
// opens a path, follows a real symlink or execs anything: a git tree at one
// commit is the production Source, and every name it hands over is already
// validated by that implementation. The builder's own boundaries are the
// archive caps declared in internal/galaxy/helpers - entry count, per-entry
// size, total declared size, name length, tree depth - applied during the
// walk so an oversized tree is refused before its first byte is written, and
// re-checked by the extractor on the way out.
//
// The artifact is deterministic: every entry carries the commit's committer
// time, fixed modes and no owner, the gzip header is left zeroed, and entries
// follow the tree's own order behind MANIFEST.json and FILES.json, so two
// builds of one commit produce one byte sequence and one sha256. A symlink is
// written pointing at the final entry its chain resolves to, relative to the
// link's own directory, because internal/galaxy/manifest resolves a link
// against the archive in one lookup per hop over real entries and the
// extractor refuses a path whose component is a link. Every artifact is read
// back through manifest.ReadFromTarGz and manifest.VerifyChain before it is
// handed over; a failure there is a defect of this package, reported under
// its own sentinel with the cause as text so it never classifies as an
// integrity failure of the remote.
package collectionbuild
