package treearchive

import (
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/safeout"
)

// RootName is the tree-relative name of the root itself, as FILES.json
// spells it; every other tree-relative path is "/"-joined without a leading
// "./".
const RootName = "."

// JoinPath joins two "/"-separated repository path pieces, either of which
// may be "" (the repository root).
func JoinPath(dir, name string) string {
	switch {
	case dir == "":
		return name
	case name == "":
		return dir
	default:
		return dir + "/" + name
	}
}

// DisplayPath renders a repository path for a message: the root gets a name,
// and anything else is cleaned of control runes and bounded, since the name
// is repository content.
func DisplayPath(p string) string {
	if p == "" {
		return "the repository root"
	}
	return helpers.TruncateForMessage(string(safeout.Clean(p)))
}

// relJoin joins a tree-relative directory (RootName for the root) and a
// name.
func relJoin(dir, name string) string {
	if dir == RootName {
		return name
	}
	return dir + "/" + name
}
