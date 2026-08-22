package exitcode

import (
	"fmt"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// urlExitCases pins the exit class of every url-source sentinel, one row per
// sentinel, in the same closed-table shape gitExitCases keeps for git.
//
//nolint:gochecknoglobals // a fixed table consumed by one test, not mutable shared state
var urlExitCases = []exitCase{
	{name: "invalid url requirement", err: helpers.ErrInvalidURLRequirement, wantCode: ExitUsage},
	{name: "url userinfo", err: helpers.ErrURLRequirementUserinfo, wantCode: ExitUsage},
	{name: "invalid url locator", err: helpers.ErrInvalidURLLocator, wantCode: ExitUsage},
	{name: "url credential invalid", err: helpers.ErrURLCredentialInvalid, wantCode: ExitUsage},
	{name: "role tarball layout", err: helpers.ErrRoleTarballLayout, wantCode: ExitUsage},
	{name: "role tarball entry invalid", err: helpers.ErrRoleTarballEntryInvalid, wantCode: ExitUsage},
	{name: "version assert mismatch", err: helpers.ErrURLCollectionVersionMismatch, wantCode: ExitResolution},
	{name: "artifact identity mismatch", err: helpers.ErrURLArtifactIdentityMismatch, wantCode: ExitIntegrity},
	{name: "role artifact sha mismatch", err: helpers.ErrURLArtifactSHA256Mismatch, wantCode: ExitIntegrity},
}

// TestURLSentinelsAreAllClassified checks every url sentinel both bare and
// wrapped, and refuses the generic fallback for any of them: a url sentinel
// that reaches ExitError is one this package forgot to place.
func TestURLSentinelsAreAllClassified(t *testing.T) {
	t.Parallel()
	for _, tt := range urlExitCases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := FromError(tt.err); got != tt.wantCode {
				t.Fatalf("FromError(bare) = %d, want %d", got, tt.wantCode)
			}
			wrapped := fmt.Errorf("context: %w", tt.err)
			if got := FromError(wrapped); got != tt.wantCode {
				t.Fatalf("FromError(wrapped) = %d, want %d", got, tt.wantCode)
			}
			if tt.wantCode == ExitError {
				t.Fatalf("a url sentinel must never classify as the generic fallback")
			}
		})
	}
}
