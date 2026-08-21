package collectionbuild

import "github.com/greeddj/go-galaxy/internal/galaxy/treearchive"

// defaultPatternCount is how many patterns newIgnoreRules puts ahead of
// build_ignore.
const defaultPatternCount = 9

// ignoreDirNames are the directory basenames ansible prunes at every depth.
func ignoreDirNames() map[string]struct{} {
	return map[string]struct{}{
		"CVS": {}, ".bzr": {}, ".hg": {}, ".git": {}, ".svn": {}, "__pycache__": {}, ".tox": {},
	}
}

// newIgnoreRules is the build's exclusion list: ansible's defaults, then
// build_ignore in the order written, each matched with treearchive.Fnmatch
// against the "/"-joined path relative to the collection root - so
// "galaxy.yml" and "tests/output" match at the root only - plus the basename
// prune list that applies to directories alone.
func newIgnoreRules(namespace, name string, buildIgnore []string) treearchive.Rules {
	patterns := make([]string, 0, defaultPatternCount+len(buildIgnore))
	patterns = append(patterns,
		"MANIFEST.json",
		"FILES.json",
		"galaxy.yml",
		"galaxy.yaml",
		".git",
		"*.pyc",
		"*.retry",
		"tests/output",
		namespace+"-"+name+"-*.tar.gz",
	)
	patterns = append(patterns, buildIgnore...)
	return treearchive.Rules{Patterns: patterns, DirNames: ignoreDirNames()}
}
