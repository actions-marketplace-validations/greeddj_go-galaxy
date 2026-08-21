package treearchive

import (
	"errors"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

func TestChargeSizeTotalBudget(t *testing.T) {
	t.Parallel()
	p := &Plan{}
	for p.total < helpers.ArchiveMaxTotalSize {
		if err := p.chargeSize("a", helpers.ArchiveMaxEntrySize); err != nil {
			t.Fatalf("charge at %d: %v", p.total, err)
		}
	}
	if err := p.chargeSize("b", 1); !errors.Is(err, helpers.ErrArchiveExceedsMaxSize) {
		t.Fatalf("charge past the cap: %v", err)
	}
}

func TestRelativeLink(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ from, to, want string }{
		{".", "README.md", "README.md"},
		{".", "d/f", "d/f"},
		{"a", "d/f", "../d/f"},
		{"a/b", "a/c", "../c"},
		{"a/b", "a", ".."},
		{"a/b", "a/b/c", "c"},
		{"d/sub", "d/f", "../f"},
	} {
		if got := relativeLink(tt.from, tt.to); got != tt.want {
			t.Errorf("relativeLink(%q, %q) = %q, want %q", tt.from, tt.to, got, tt.want)
		}
	}
}
