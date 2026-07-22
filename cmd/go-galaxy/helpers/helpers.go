package helpers

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
)

// Version returns the formatted version string for the application. version,
// commit, and date are normally injected by the Justfile's ldflags; on a
// plain `go build`/`go run` (dev build) they are empty, and fillFromBuildInfo
// recovers what it can from the Go module/VCS build info instead of making a
// network call.
func Version(version, commit, date, builtBy string) string {
	if version == "" || commit == "" || date == "" {
		version, commit, date = fillFromBuildInfo(version, commit, date)
	}
	if version == "" {
		version = defaultVersion
	}

	if builtBy == "" {
		builtBy = defaultBuilder
	}

	return formatVersion(version, commit, date, builtBy)
}

// formatVersion renders the already-resolved version fields into the final
// display string. Split out from Version so the four format branches can be
// exercised deterministically in tests, independent of fillFromBuildInfo's
// environment-dependent fallback.
func formatVersion(version, commit, date, builtBy string) string {
	switch {
	case date != "" && commit != "":
		return fmt.Sprintf("%s (commit %s, built by %s @ %s) // %s", version, commit, builtBy, date, runtime.Version())
	case date == "" && commit != "":
		return fmt.Sprintf("%s (commit %s, built by %s) // %s", version, commit, builtBy, runtime.Version())
	case date != "" && commit == "":
		return fmt.Sprintf("%s (built by %s @ %s) // %s", version, builtBy, date, runtime.Version())
	default:
		return fmt.Sprintf("%s (built by %s) // %s", version, builtBy, runtime.Version())
	}
}

// fillFromBuildInfo fills any of version, commit, date that are empty from
// runtime/debug.ReadBuildInfo, leaving already-set (ldflags-provided) values
// untouched. It is a pure, network-free fallback for dev builds.
func fillFromBuildInfo(version, commit, date string) (string, string, string) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return version, commit, date
	}

	if version == "" {
		version = info.Main.Version
	}
	if commit == "" || date == "" {
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				if commit == "" {
					commit = setting.Value
				}
			case "vcs.time":
				if date == "" {
					date = setting.Value
				}
			}
		}
	}
	return version, commit, date
}

// defaultCacheDir returns the default cache directory path.
func defaultCacheDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(defaultHomeDir, dirSuffix)
	}
	return filepath.Join(home, dirSuffix)
}
