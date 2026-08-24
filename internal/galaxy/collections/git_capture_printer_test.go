package collections_test

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// lineCapturingPrinter records every line on every tier, so a test can
// assert both on what a report said (the outdated lines) and on what no line
// at all may contain (a git password), including the Debugf tier --verbose
// turns on. It is safe for the concurrent use the download-worker pool makes
// of it.
type lineCapturingPrinter struct {
	lines []string
	mu    sync.Mutex
}

func (p *lineCapturingPrinter) Printf(format string, args ...any) { p.recordf(format, args...) }
func (p *lineCapturingPrinter) PersistentPrintf(format string, args ...any) {
	p.recordf(format, args...)
}
func (p *lineCapturingPrinter) Okf(format string, args ...any) { p.recordf(format, args...) }
func (p *lineCapturingPrinter) OkVersionf(version, format string, args ...any) {
	p.record(renderVersionLine(version, "", format, args...))
}
func (p *lineCapturingPrinter) Updatef(format string, args ...any) { p.recordf(format, args...) }
func (p *lineCapturingPrinter) Errorf(format string, args ...any)  { p.recordf(format, args...) }
func (p *lineCapturingPrinter) ErrorVersionf(version, cause, format string, args ...any) {
	p.record(renderVersionLine(version, cause, format, args...))
}
func (p *lineCapturingPrinter) Warnf(format string, args ...any)  { p.recordf(format, args...) }
func (p *lineCapturingPrinter) Debugf(format string, args ...any) { p.recordf(format, args...) }
func (p *lineCapturingPrinter) DebugSincef(_ time.Time, format string, args ...any) {
	p.recordf(format, args...)
}

func (p *lineCapturingPrinter) recordf(format string, args ...any) {
	p.record(fmt.Sprintf(format, args...))
}

func (p *lineCapturingPrinter) record(line string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lines = append(p.lines, line)
}

// snapshot returns a copy of every recorded line, in order.
func (p *lineCapturingPrinter) snapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.lines...)
}

// hasLineContaining reports whether any recorded line, on any tier, contains
// substr.
func (p *lineCapturingPrinter) hasLineContaining(substr string) bool {
	for _, line := range p.snapshot() {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}

// renderVersionLine is what this double stores for an OkVersionf or
// ErrorVersionf call: the same message, version tag and cause an operator
// would read, minus the color internal/progress adds. Recording only the
// format and its args would drop both the version and the cause, and the
// assertions this printer exists for - that a line contains what a report
// promised, and that no line contains a git password - would then be reading
// half a line. The collections package's own internal test doubles carry an
// identical helper, unreachable from this external test package.
func renderVersionLine(version, cause, format string, args ...any) string {
	line := fmt.Sprintf(format, args...)
	if version != "" {
		line += " == " + version
	}
	if cause != "" {
		line += " " + cause
	}
	return line
}
