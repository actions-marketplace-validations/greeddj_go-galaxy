// Package output declares Printer, the operator-output interface every
// subsystem takes instead of depending on a concrete renderer. Its methods
// fall into three tiers that behave differently under --quiet and --verbose:
// Printf is transient progress, PersistentPrintf/Okf/Errorf/Warnf are results
// that always emit, and Debugf/DebugSincef appear only in verbose mode.
// internal/progress holds the implementation; keeping the interface in a
// package that imports only time is what lets a test substitute a recorder
// without pulling the renderer in with it.
package output

import "time"

// Printer defines the progress output interface.
type Printer interface {
	Printf(format string, args ...any)
	PersistentPrintf(format string, args ...any)
	Okf(format string, args ...any)
	Errorf(format string, args ...any)
	Warnf(format string, args ...any)
	Debugf(format string, args ...any)
	DebugSincef(startTime time.Time, format string, args ...any)
}
