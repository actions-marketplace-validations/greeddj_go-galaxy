package collections

import "testing"

// TestClassifyOutdated checks the locked-vs-latest comparison, including the
// two failure modes where either side does not parse as semver: these must
// be reported as Failed rather than silently treated as up-to-date.
func TestClassifyOutdated(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		locked     string
		latest     string
		wantNewer  bool
		wantFailed bool
	}{
		{name: "latest is newer", locked: "1.0.0", latest: "2.0.0", wantNewer: true, wantFailed: false},
		{name: "locked equals latest", locked: "2.0.0", latest: "2.0.0", wantNewer: false, wantFailed: false},
		{name: "locked is newer than latest", locked: "2.0.0", latest: "1.0.0", wantNewer: false, wantFailed: false},
		{name: "locked does not parse as semver", locked: "garbage", latest: "2.0.0", wantNewer: false, wantFailed: true},
		{name: "latest does not parse as semver", locked: "1.0.0", latest: "garbage", wantNewer: false, wantFailed: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			entry := classifyOutdated("ns.name", tt.locked, tt.latest)
			if entry.Newer != tt.wantNewer {
				t.Errorf("classifyOutdated(%q, %q).Newer = %v, want %v", tt.locked, tt.latest, entry.Newer, tt.wantNewer)
			}
			if entry.Failed != tt.wantFailed {
				t.Errorf("classifyOutdated(%q, %q).Failed = %v, want %v", tt.locked, tt.latest, entry.Failed, tt.wantFailed)
			}
			if tt.wantFailed && entry.Message == "" {
				t.Errorf("classifyOutdated(%q, %q).Message is empty, want a parse-failure message", tt.locked, tt.latest)
			}
			if !tt.wantFailed && entry.Message != "" {
				t.Errorf("classifyOutdated(%q, %q).Message = %q, want empty on success", tt.locked, tt.latest, entry.Message)
			}
		})
	}
}
