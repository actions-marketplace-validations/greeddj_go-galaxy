package output

import "time"

// Printer defines the progress output interface.
type Printer interface {
	Printf(format string, args ...any)
	PersistentPrintf(format string, args ...any)
	Okf(format string, args ...any)
	Errorf(format string, args ...any)
	Debugf(format string, args ...any)
	DebugSincef(startTime time.Time, format string, args ...any)
}
