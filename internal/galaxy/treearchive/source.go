package treearchive

import (
	"io"
	"time"
)

// EntryKind classifies one entry of a source tree.
type EntryKind uint8

const (
	// EntryFile is a regular, non-executable file.
	EntryFile EntryKind = iota + 1
	// EntryExecutable is a regular file with the executable bit set.
	EntryExecutable
	// EntryDir is a directory.
	EntryDir
	// EntrySymlink is a symbolic link; Open returns its target as the blob.
	EntrySymlink
	// EntrySubmodule is a git submodule entry, which the builder skips with a
	// warning: nothing is ever fetched for it.
	EntrySubmodule
)

// Entry is one directory entry of a source tree. Name is a single, already
// validated path element; Size is the blob size for a file, executable or
// symlink and zero otherwise.
type Entry struct {
	Name string
	Kind EntryKind
	Size int64
}

// Source is a read-only view of a source tree at one commit. Paths
// are "/"-joined and relative to the repository root, "" being the root.
// ReadDir returns the entries of a directory in the tree's own order with
// every name already validated by the implementation (an invalid name is an
// error, never a skipped entry); Open streams a file's or a symlink's blob,
// capped by the implementation at the per-entry archive size; CommitTime is
// the committer time of the commit, which the builder stamps on every
// archive entry so two builds of one commit are byte-identical under one
// toolchain.
type Source interface {
	ReadDir(path string) ([]Entry, error)
	Open(path string) (io.ReadCloser, error)
	CommitTime() time.Time
}
