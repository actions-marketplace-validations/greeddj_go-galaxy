// Package output declares Printer, the operator-output interface every
// subsystem takes instead of depending on a concrete renderer. Its methods
// fall into three tiers that behave differently under --quiet and --verbose:
// Printf is transient progress, PersistentPrintf/Okf/OkVersionf/Updatef/
// Errorf/ErrorVersionf/Warnf are results that always emit, and
// Debugf/DebugSincef appear only in verbose mode. internal/progress holds
// the implementation;
// keeping the interface in a package that imports only time is what lets a
// test substitute a recorder without pulling the renderer in with it.
package output

import "time"

// Printer defines the progress output interface.
//
// OkVersionf and ErrorVersionf take the exact version their subject settled
// on as a parameter rather than leaving a caller to format it into its own
// message: the renderer colors that version, and a message is sanitized
// before it is decorated, so an escape sequence spelled into a format string
// would never reach the terminal. ErrorVersionf takes its cause apart from
// the message for the same structural reason - a failure line ends with what
// went wrong, so the version has to be placed before it rather than appended.
type Printer interface {
	Printf(format string, args ...any)
	PersistentPrintf(format string, args ...any)
	Okf(format string, args ...any)
	OkVersionf(version, format string, args ...any)
	// Updatef is the third verdict a report can reach beside Okf and Errorf:
	// the subject is intact and something newer exists. It is its own tier
	// rather than either neighbor because a success mark on it would say
	// there is nothing to do and a failure mark would say something broke,
	// and it lands on stdout, where the rest of the report is.
	Updatef(format string, args ...any)
	Errorf(format string, args ...any)
	ErrorVersionf(version, cause, format string, args ...any)
	Warnf(format string, args ...any)
	Debugf(format string, args ...any)
	DebugSincef(startTime time.Time, format string, args ...any)
}
