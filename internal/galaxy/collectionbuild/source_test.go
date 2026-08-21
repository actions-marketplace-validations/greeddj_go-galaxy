package collectionbuild

import (
	"context"
	"os"
	"testing"
)

// tempFileIn returns a TempFileFunc creating files under dir.
func tempFileIn(dir string) TempFileFunc {
	return func(_ context.Context) (*os.File, func(), error) {
		f, err := os.CreateTemp(dir, ".download-*")
		if err != nil {
			return nil, nil, err
		}
		name := f.Name()
		return f, func() { _ = os.Remove(name) }, nil
	}
}

// minimalGalaxyYML is a galaxy.yml every build fixture starts from.
const minimalGalaxyYML = "namespace: acme\nname: app\nversion: 1.2.3\nreadme: README.md\nauthors:\n  - A. Author\n"

// candidateFor discovers the one candidate under subdir or fails the test.
func candidateFor(t *testing.T, src Source, subdir string) Candidate {
	t.Helper()
	cands, _, err := Discover(src, subdir)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("Discover returned %d candidates, want 1", len(cands))
	}
	return cands[0]
}
