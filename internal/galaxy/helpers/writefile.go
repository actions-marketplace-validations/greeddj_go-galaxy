package helpers

import (
	"os"
	"path/filepath"
)

// WriteFileAtomic writes data to a temp file in path's own directory and
// renames it onto path. Rename within a single directory is atomic, so a
// reader sees either the previous file or the fully written new one, never a
// truncated one, and a crashed or concurrent run leaves no half-written file
// behind. The parent tree is created with DirMod if missing - the temp file
// MUST live in path's own directory for the rename to be atomic, so directory
// existence is this function's own precondition, not the caller's. The temp
// file is removed on any error. Rename replaces the entry at path rather than
// following it, so a symlink pre-planted at path is replaced instead of
// having its target truncated, and os.CreateTemp's O_EXCL refuses to open
// through a symlink at the temp name; O_NOFOLLOW/O_EXCL on path itself is
// deliberately NOT used because it would break the legitimate
// overwrite-on-rerun case. A directory fsync is intentionally omitted:
// rename atomicity plus the content Sync deliver the no-truncated-file
// guarantee, and a dir-open would add a portability wrinkle for no benefit
// here. The file is stamped FileMod exactly, without umask filtering,
// because both current callers write operator-facing output a CI consumer
// must be able to read; a caller that wants a tighter cache-internal mode
// (the project registry, which inherits CreateTemp's 0600) must not use this
// helper as-is.
//
//nolint:nonamedreturns // the named err return lets the deferred cleanup see the final error and remove the temp file only on failure.
func WriteFileAtomic(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	if err = os.MkdirAll(dir, DirMod); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Chmod(FileMod); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	err = os.Rename(tmpName, path)
	return err
}
