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
func (p *lineCapturingPrinter) Okf(format string, args ...any)    { p.recordf(format, args...) }
func (p *lineCapturingPrinter) Errorf(format string, args ...any) { p.recordf(format, args...) }
func (p *lineCapturingPrinter) Warnf(format string, args ...any)  { p.recordf(format, args...) }
func (p *lineCapturingPrinter) Debugf(format string, args ...any) { p.recordf(format, args...) }
func (p *lineCapturingPrinter) DebugSincef(_ time.Time, format string, args ...any) {
	p.recordf(format, args...)
}

func (p *lineCapturingPrinter) recordf(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lines = append(p.lines, fmt.Sprintf(format, args...))
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
