package progress

import (
	"bytes"
	"strings"
	"testing"
)

// The fixtures the version tiers are exercised with. benignVersion and
// benignCause are what a real install hands OkVersionf and ErrorVersionf;
// hostileVersion and hostileCause are their untrusted counterparts, each
// carrying the same control characters hostileCallerText does, so a missing
// Clean call on either parameter shows up as raw escape bytes in the line
// rather than as replacement runes.
const (
	benignVersion       = "1.0.0"
	benignCause         = "error: boom"
	hostileVersion      = "1.0.0\x1b[31m\rowned"
	hostileVersionClean = "1.0.0�[31m�owned"
	hostileCause        = "error: \x1b[31mred\rgone"
	hostileCauseClean   = "error: �[31mred�gone"
)

// colorVersionTag is the colored tag benignVersion renders as, built through
// the same function production builds it with rather than spelled out as a
// literal, for the reason okMark and its siblings are built that way: a test
// that restates the escape sequence would pass while the two disagreed.
func colorVersionTag() string { return versionTag(benignVersion, true) }

// TestVersionTagFollowsItsDestination pins the per-destination rule for the
// one decoration that is not a marker: colored where the stream accepts
// color, plain text where it does not. Both halves are asserted on the same
// Progress - stdout redirected, stderr still a terminal - so neither result
// can be the fixture, exactly as TestMarkersFollowTheirOwnDestination does
// for the markers themselves.
//
// The plain half is the load-bearing one: it is what `go-galaxy install >
// install.log` and NO_COLOR both produce, and it is what makes `grep '== '`
// find the version in a redirected log.
func TestVersionTagFollowsItsDestination(t *testing.T) {
	t.Parallel()

	var out, errOut bytes.Buffer
	p := newStreamProgress(false, false, false,
		stream{w: &out, color: false}, stream{w: &errOut, color: true})
	defer p.Close()

	p.OkVersionf(benignVersion, "Installed: acme.app")
	p.ErrorVersionf(benignVersion, benignCause, "Failed: acme.app")

	assertBuf(t, &out, okGlyph+" Installed: acme.app == "+benignVersion+"\n")
	want := failMark() + "Failed: acme.app" + colorVersionTag() + " " + benignCause + "\n"
	if got := errOut.String(); got != want {
		t.Errorf("errOut = %q, want %q", got, want)
	}
	if strings.Contains(out.String(), "\x1b") {
		t.Errorf("a plain destination received escape bytes: %q", out.String())
	}
}

// TestVersionIsPlacedBeforeTheCause pins the shape a failure line has to
// have: the version belongs to the subject, so it comes before the cause and
// not after it. Appending it instead would produce
// "Failed: acme.app error: boom == 1.0.0", where the version reads as part
// of the error text - which is the whole reason ErrorVersionf takes its
// cause as a parameter rather than letting a caller format it into the
// message.
func TestVersionIsPlacedBeforeTheCause(t *testing.T) {
	t.Parallel()

	p, _, errOut := stateB()
	defer p.Close()
	p.ErrorVersionf(benignVersion, benignCause, "Failed: acme.app")

	got := errOut.String()
	versionAt := strings.Index(got, versionMark)
	causeAt := strings.Index(got, benignCause)
	if versionAt < 0 || causeAt < 0 {
		t.Fatalf("line %q is missing the version tag or the cause", got)
	}
	if versionAt > causeAt {
		t.Fatalf("line %q puts the version behind the cause", got)
	}
}

// TestEmptyVersionRendersTheUndecoratedLine pins the fallback the two
// version tiers promise: with nothing to report they print what Okf and
// Errorf print, byte for byte, rather than a bare "== ". That is what lets a
// call site pass whatever version it has without first deciding whether it
// has one, and it is the only path a collection or role with no resolved
// version can take.
func TestEmptyVersionRendersTheUndecoratedLine(t *testing.T) {
	t.Parallel()

	t.Run("OkVersionf", func(t *testing.T) {
		t.Parallel()
		plain, out, _ := stateB()
		defer plain.Close()
		plain.Okf("Installed: acme.app")

		tagged, taggedOut, _ := stateB()
		defer tagged.Close()
		tagged.OkVersionf("", "Installed: acme.app")

		assertBuf(t, taggedOut, out.String())
	})

	t.Run("ErrorVersionf", func(t *testing.T) {
		t.Parallel()
		plain, _, errOut := stateB()
		defer plain.Close()
		plain.Errorf("Failed: acme.app %s", benignCause)

		tagged, _, taggedErr := stateB()
		defer tagged.Close()
		tagged.ErrorVersionf("", benignCause, "Failed: acme.app")

		assertBuf(t, taggedErr, errOut.String())
	})
}

// TestEmptyCauseLeavesNoTrailingSpace pins the other half of the optional
// parameters: a failure line with a version and nothing to say about the
// cause ends at the version, not at a space. A trailing space is invisible
// to a reader and would survive into every log this line reaches.
func TestEmptyCauseLeavesNoTrailingSpace(t *testing.T) {
	t.Parallel()

	p, _, errOut := stateB()
	defer p.Close()
	p.ErrorVersionf(benignVersion, "", "Failed: acme.app")

	want := failMark() + "Failed: acme.app" + colorVersionTag() + "\n"
	if got := errOut.String(); got != want {
		t.Fatalf("line = %q, want %q", got, want)
	}
}

// TestVersionAndCauseAreSanitizedIndependently pins that both parameters a
// version tier takes beyond its format are payload, not decoration: each is
// cleaned on its own, and the escapes this package puts around the version
// survive between them. A version and a cause are as untrusted as a message
// - a version reaches this package from a manifest or a git ref, a cause
// from a server's error text - so a tier that cleaned only its format string
// would hand a terminal exactly what this package exists to stop.
//
// Asserting the whole line rather than each half separately is what makes
// the third failure mode reachable: escapes stripped from the tag itself,
// which no per-half substring check would catch.
func TestVersionAndCauseAreSanitizedIndependently(t *testing.T) {
	t.Parallel()

	p, _, errOut := stateB()
	defer p.Close()
	p.ErrorVersionf(hostileVersion, hostileCause, "Failed: acme.app")

	want := failMark() + "Failed: acme.app" +
		" " + ansiGray + versionMark + hostileVersionClean + ansiReset +
		" " + hostileCauseClean + "\n"
	if got := errOut.String(); got != want {
		t.Fatalf("line = %q, want %q", got, want)
	}
}

// TestVersionTiersEmitInEveryState pins that the two version tiers are
// result tiers like the ones they extend: they emit in all four output
// states - with a spinner running, in verbose, in quiet, and on a non-TTY -
// and each lands on the stream its undecorated sibling lands on. Quiet is
// the row that matters most: a CI run with --quiet still has to say what it
// installed and at which version.
func TestVersionTiersEmitInEveryState(t *testing.T) {
	states := []struct {
		build func() (*Progress, *bytes.Buffer, *bytes.Buffer)
		name  string
		tag   string
	}{
		{name: "A spinner", build: stateA, tag: colorVersionTag()},
		{name: "B verbose", build: stateB, tag: colorVersionTag()},
		{name: "C quiet", build: stateC, tag: colorVersionTag()},
		{name: "D non-TTY", build: stateD, tag: " " + versionMark + benignVersion},
	}

	for _, st := range states {
		t.Run(st.name+"/OkVersionf", func(t *testing.T) {
			p, out, errOut := st.build()
			defer p.Close()
			p.OkVersionf(benignVersion, "Installed: acme.app")
			assertBuf(t, out, okPrefixFor(st.name)+"Installed: acme.app"+st.tag+"\n")
			assertEmpty(t, errOut)
		})

		t.Run(st.name+"/ErrorVersionf", func(t *testing.T) {
			p, out, errOut := st.build()
			defer p.Close()
			p.ErrorVersionf(benignVersion, benignCause, "Failed: acme.app")
			assertBuf(t, errOut, failPrefixFor(st.name)+"Failed: acme.app"+st.tag+" "+benignCause+"\n")
			assertEmpty(t, out)
		})
	}
}

// okPrefixFor and failPrefixFor return the marker the named state renders:
// colored for states A through C, whose destinations are terminals, and
// plain for state D, which is the non-TTY case.
func okPrefixFor(state string) string {
	if strings.HasPrefix(state, "D") {
		return okGlyph + " "
	}
	return okMark()
}

func failPrefixFor(state string) string {
	if strings.HasPrefix(state, "D") {
		return failGlyph + " "
	}
	return failMark()
}
