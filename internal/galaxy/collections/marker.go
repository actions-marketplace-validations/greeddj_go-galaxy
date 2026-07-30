package collections

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/output"
)

// treeTally is a cheap fingerprint of an installed collection's file tree: a
// count of directories, a count of everything else (regular files, symlinks,
// or anything unusual a tarball might contain), and the sum of their lstat
// sizes. It is not a hash - two different trees can share a tally - but it is
// strong enough to catch the failure mode this guards against (a file added,
// removed, or resized after extraction) at the cost of one fs.WalkDir pass
// instead of a full re-hash of every byte. See verifyExtractMarker for
// exactly what it does and does not detect.
type treeTally struct {
	Entries int64 // non-directory entries (regular files, symlinks, anything else)
	Dirs    int64 // directories, excluding the root
	Bytes   int64 // sum of lstat sizes over non-directory entries
}

// extractMarkerVersion is the literal format tag every marker this package
// writes begins with. A marker written by a future format change (a new
// field, a changed unit) would carry a different tag, so an old-format
// marker from a prior binary is rejected as unparseable - safe, since that
// just falls through to a re-extraction - rather than misread against a
// shape it no longer matches.
const extractMarkerVersion = "go-galaxy-extract-1"

// extractMarkerFieldCount is the exact number of whitespace-separated fields
// a well-formed marker line has: the version tag plus entries=, dirs=, and
// bytes=, in that fixed order.
const extractMarkerFieldCount = 4

// extractMarkerMaxReadSize bounds the read of a marker file. A well-formed
// marker is well under 100 bytes, so anything longer than this is either
// corrupt or hostile, and is rejected outright without the caller ever
// needing to learn the file's real length.
const extractMarkerMaxReadSize = 256

// scanTree computes a treeTally over target's install directory with exactly
// one fs.WalkDir pass against target.root.FS(), rooted at target.rel. This
// walks through target.root the same way every other read in this file does,
// so a symlink swap of an ancestor component (ansible_collections, or the
// namespace/name pair) cannot redirect the scan outside target.rel itself
// (see checkExtractMarker's callers for what enforces that at the boundary).
//
// fs.WalkDir uses Lstat semantics and never follows symlinks, so a symlink -
// including one that would otherwise form a loop or point outside
// target.rel - counts as a single non-directory entry carrying its own lstat
// size and is never descended into; this makes the walk immune to a
// symlink-loop hang and to a symlink being used to inflate the tally by
// aliasing a large subtree it does not actually contain.
//
// target.rel itself is excluded from the tally, and so is any top-level entry
// (a direct child of target.rel) whose name starts with
// helpers.ExtractMarkerPrefix - this is what stops the marker file from
// counting itself, and what stops a tarball that happens to ship a
// same-prefixed file from skewing the tally it is itself compared against.
func scanTree(target installTarget) (treeTally, error) {
	var tally treeTally
	err := fs.WalkDir(target.root.FS(), target.rel, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == target.rel {
			return nil
		}
		if path.Dir(p) == target.rel && strings.HasPrefix(d.Name(), helpers.ExtractMarkerPrefix) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			tally.Dirs++
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return infoErr
		}
		tally.Entries++
		tally.Bytes += info.Size()
		return nil
	})
	if err != nil {
		return treeTally{}, err
	}
	return tally, nil
}

// formatExtractMarker renders tally into the single-line marker format this
// package both writes and parses:
//
//	go-galaxy-extract-1 entries=<n> dirs=<n> bytes=<n>\n
func formatExtractMarker(tally treeTally) string {
	return fmt.Sprintf("%s entries=%d dirs=%d bytes=%d\n", extractMarkerVersion, tally.Entries, tally.Dirs, tally.Bytes)
}

// parseExtractMarker strictly parses the single-line format formatExtractMarker
// writes. It is hand-rolled rather than built on fmt.Sscanf deliberately:
// Sscanf matches a prefix of its input and silently ignores trailing garbage,
// which would let a truncated or appended-to marker parse "successfully"
// with the wrong tally. This instead requires exactly extractMarkerFieldCount
// space-separated fields, the literal version tag first, and the three fixed
// keys in their fixed order, with strconv.ParseInt rejecting any trailing
// non-digit character in a value. Any mismatch reports (treeTally{}, false),
// which every caller treats identically to "marker absent": unverifiable,
// not corrupt.
func parseExtractMarker(content string) (treeTally, bool) {
	content = strings.TrimSuffix(content, "\n")
	fields := strings.Split(content, " ")
	if len(fields) != extractMarkerFieldCount || fields[0] != extractMarkerVersion {
		return treeTally{}, false
	}
	entries, ok := parseExtractMarkerField(fields[1], "entries=")
	if !ok {
		return treeTally{}, false
	}
	dirs, ok := parseExtractMarkerField(fields[2], "dirs=")
	if !ok {
		return treeTally{}, false
	}
	bytesCount, ok := parseExtractMarkerField(fields[3], "bytes=")
	if !ok {
		return treeTally{}, false
	}
	return treeTally{Entries: entries, Dirs: dirs, Bytes: bytesCount}, true
}

// parseExtractMarkerField requires field to start with the literal key and
// the remainder to be a valid non-negative base-10 int64 with no trailing
// garbage - strconv.ParseInt itself already rejects any character past the
// last digit, which is the property fmt.Sscanf does not give us.
func parseExtractMarkerField(field, key string) (int64, bool) {
	rest, ok := strings.CutPrefix(field, key)
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// readExtractMarker reads target's marker at rel (root-relative, already
// computed by the caller via markerRel) with a hard cap of
// extractMarkerMaxReadSize bytes, through target.root rather than a plain
// os.Open - the same symlink-swap containment scanTree and every other read
// in this file gets. It reports ok=false for a missing/unreadable file and
// for a file longer than the cap - in the latter case, the read stops at the
// cap and the true length is never learned, since anything past a
// well-formed marker's size is already invalid regardless of what it
// contains.
func readExtractMarker(target installTarget, rel string) (string, bool) {
	f, err := target.root.Open(rel)
	if err != nil {
		return "", false
	}
	defer func() {
		_ = f.Close()
	}()

	buf := make([]byte, extractMarkerMaxReadSize+1)
	n, err := io.ReadFull(f, buf)
	switch {
	case err == nil:
		// A full extractMarkerMaxReadSize+1 bytes were read, meaning the file
		// is at least one byte over the cap: reject as oversized.
		return "", false
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return string(buf[:n]), true
	default:
		return "", false
	}
}

// writeExtractMarker computes target's current tree tally via scanTree and
// persists it as sha's extract-done marker, so a later verifyExtractMarker
// call has something authoritative to compare against. The write always
// removes any existing marker under sha's name first (ignoring
// fs.ErrNotExist): extractCollection always wipes target.path with
// target.root.RemoveAll before a real extraction, so a leftover marker here
// would only happen if this were ever called a second time against the same
// sha without that wipe in between - remove-then-write keeps that case
// correct too rather than relying on target.root.WriteFile's own
// truncate-on-write, which is already what happens but is worth making an
// explicit step rather than an implicit side effect.
func writeExtractMarker(target installTarget, sha string) error {
	rel, ok := markerRel(target, sha)
	if !ok {
		// Hard error here, unlike checkExtractMarker/verifyExtractMarker: this
		// runs only after a successful extraction, so sha has already passed
		// resolveArtifactSHA's own validation - reaching here with an unsafe
		// value means that guard was bypassed. Returning nil while writing
		// nothing would leave a real extracted tree with no marker at all,
		// which re-triggers a full re-extraction on every future run rather
		// than just this one; failing loudly is strictly better than that.
		return fmt.Errorf("%w: %q", helpers.ErrMalformedArtifactSHA256, sha)
	}
	tally, err := scanTree(target)
	if err != nil {
		return err
	}
	if err := target.root.Remove(rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return target.root.WriteFile(rel, []byte(formatExtractMarker(tally)), helpers.FileMod)
}

// extractMarkerStatus discriminates why checkExtractMarker did or did not
// find target's extract-done marker still valid.
type extractMarkerStatus int

const (
	// extractMarkerUnknown is the zero value of extractMarkerStatus. It
	// exists so a zero-value extractMarkerOutcome{} can never satisfy
	// matches(): every real construction site below sets status explicitly
	// (by field name, never positionally), so this value is never produced
	// by checkExtractMarker itself, but a future error path that forgets to
	// set status - or simply returns extractMarkerOutcome{} - fails closed
	// (treated as invalid, triggering re-extraction) instead of silently
	// reporting a valid marker.
	extractMarkerUnknown extractMarkerStatus = iota
	// extractMarkerMatches means the marker is present, well-formed, and its
	// recorded tally equals what scanTree observes right now.
	extractMarkerMatches
	// extractMarkerMissing means the marker file is absent or unreadable.
	extractMarkerMissing
	// extractMarkerMalformed means the marker exists but does not parse as
	// the current format (including the legacy "ok" sentinel it replaced).
	extractMarkerMalformed
	// extractMarkerScanFailed means scanTree itself returned an error.
	extractMarkerScanFailed
	// extractMarkerDrifted means the marker parsed fine, the scan succeeded,
	// but the two tallies disagree.
	extractMarkerDrifted
	// extractMarkerUnsafeSHA means sha itself is not helpers.IsSHA256Hex, so
	// no marker path was even computed - see markerRel's doc comment for why
	// the sha, not the joined path, is what gets rejected.
	extractMarkerUnsafeSHA
)

// extractMarkerOutcome is checkExtractMarker's result: a status plus
// whatever detail that status carries. want/got are only meaningful when
// status is extractMarkerDrifted; scanErr is only set when status is
// extractMarkerScanFailed.
type extractMarkerOutcome struct {
	scanErr error
	want    treeTally
	got     treeTally
	status  extractMarkerStatus
}

// matches reports whether the marker was found valid - the single bit a
// caller that only cares about the yes/no answer (installDryRunProbe) needs,
// without inspecting status itself.
func (o extractMarkerOutcome) matches() bool {
	return o.status == extractMarkerMatches
}

// markerRel returns the root-relative path (slash-separated, suitable for
// target.root and target.root.FS()) of target's extract-done marker for sha,
// and whether sha was safe to use at all - the single validating chokepoint
// every construction of this path must go through, so it cannot be computed
// two different ways, one of them unguarded.
//
// sha must be helpers.IsSHA256Hex before it is ever joined: path.Join fuses a
// leading ".." in sha into the constant helpers.ExtractMarkerPrefix element
// (".extract-done."), turning it into ".extract-done.." - a real "up one
// directory" element - so the join absorbs sha's first ".." for free and
// every later ".." in sha pops a real path component off target.rel. A
// leading "./" makes this unbounded rather than capped at target.rel's own
// depth: "./../../../../../../etc/passwd" walks all the way past target.rel's
// components before the extra ".." tokens run out, rather than stopping once
// they are exhausted. Validating the joined result after the fact cannot
// close this - by the time a path exists to inspect, the escape has already
// happened - so sha itself is what is validated, before any join is
// computed.
//
// target.root is a real backstop against the same class of value now, not
// the only defense the way installPath-based joins once were: even a
// (hypothetical, guard-bypassing) traversal sha that survived this check
// would still have to pass through target.root, which refuses to resolve a
// path component escaping cfg.DownloadPath. This guard is kept anyway,
// unconditionally, for the same reason writeExtractMarker's own guard is not
// made redundant by any caller's check - defense in depth, not a single
// point of failure.
func markerRel(target installTarget, sha string) (string, bool) {
	if !helpers.IsSHA256Hex(sha) {
		return "", false
	}
	return path.Join(target.rel, helpers.ExtractMarkerPrefix+sha), true
}

// checkExtractMarker reports whether target's on-disk extract-done marker
// for sha is present, well-formed, and its recorded tally still matches what
// scanTree observes for target right now. It is a pure read: no logging, no
// marker deletion, no side effects at all - a caller that only wants a
// read-only answer can use this directly, currently installDryRunProbe (for
// the dry-run preview), though nothing about this predicate ties it to that
// one caller. verifyExtractMarker below is the install-path wrapper that adds
// logging and the best-effort cleanup on a negative outcome; see its own doc
// comment for what this predicate does and does not catch, and why cleanup
// lives in the wrapper rather than here.
func checkExtractMarker(target installTarget, sha string) extractMarkerOutcome {
	rel, ok := markerRel(target, sha)
	if !ok {
		return extractMarkerOutcome{status: extractMarkerUnsafeSHA}
	}

	content, ok := readExtractMarker(target, rel)
	if !ok {
		return extractMarkerOutcome{status: extractMarkerMissing}
	}
	want, ok := parseExtractMarker(content)
	if !ok {
		return extractMarkerOutcome{status: extractMarkerMalformed}
	}
	got, err := scanTree(target)
	if err != nil {
		return extractMarkerOutcome{status: extractMarkerScanFailed, scanErr: err}
	}
	if got != want {
		return extractMarkerOutcome{status: extractMarkerDrifted, want: want, got: got}
	}
	return extractMarkerOutcome{status: extractMarkerMatches}
}

// verifyExtractMarker reports whether target's on-disk extract-done marker
// for sha is present, well-formed, and its recorded tally still matches what
// scanTree observes for target right now - checkExtractMarker answers that
// question; this wrapper adds the install path's own logging and
// best-effort cleanup around it, so those two concerns (the pure comparison,
// and what an install does in reaction to it) cannot drift apart from a
// caller that only wants the former.
//
// What this catches: any file or directory added or removed under target's
// install directory since extraction, and any file whose size changed -
// which is the dominant real-world shape of an in-place edit, since installed
// files are hard links into the shared extracted-store cache, so an edit
// corrupts that cache for every future install sharing the same content, not
// just this one.
//
// What this deliberately does NOT catch: an in-place edit that preserves the
// edited file's exact byte length. Closing that hole needs a full re-hash (or
// an equally expensive per-file sha comparison against FILES.json) - a cost
// this cheap tally exists specifically to avoid paying on every warm install.
// TestExtractMarkerMissesEqualSizeEdit pins this limit so it is never
// mistaken for a stronger guarantee than it actually is.
//
// On any negative outcome that reaches a real marker path - missing or
// unreadable, unparseable (including the legacy "ok" sentinel this format
// replaces), or a tally mismatch - that marker is removed on a best-effort
// basis before returning false. That is what avoids a second tree walk:
// extractCollection's own presence check runs after this and finds nothing,
// so it goes straight to re-extraction at the cost of one unlink rather than
// a second traversal. It is also self-healing: a rejected marker never
// survives the run that rejected it, so the next run starts clean instead of
// re-detecting the same drift forever.
//
// Severity is deliberately asymmetric. A marker that is absent, in the
// legacy format, or otherwise unparseable is logged at Debugf only: every
// user upgrading past the marker-format change would otherwise see a
// one-time warning storm for a condition that is expected and harmless. A
// marker that IS in the current format but whose tally does not match, or a
// sha that fails the digest-shape check entirely, is logged at Warnf,
// mirroring warnIfOffServerDownloadHost: both are live integrity signals, so
// they must reach stderr and survive --quiet rather than risk being silenced
// in the very CI runs where they matter most.
//
// This never fails the run: a detected drift, and an unsafe sha, are both
// recovered by re-extraction, so turning either signal into a hard error
// would convert a recoverable event into a CI outage for no benefit.
func verifyExtractMarker(out output.Printer, target installTarget, sha string) bool {
	rel, ok := markerRel(target, sha)
	if !ok {
		// An unsafe sha is never expected the way a missing or legacy-format
		// marker is - it means either a corrupt snapshot or a lying server -
		// so unlike those, it is reported at Warnf, not Debugf, matching
		// extractMarkerDrifted and warnIfOffServerDownloadHost: it must reach
		// stderr and survive --quiet. %q (never %s) guards against a
		// newline or control byte in sha forging a log line. There is no
		// marker path to log or remove - markerRel refused to construct one -
		// and no Remove is reachable on this arm at all.
		out.Warnf("refusing to use unsafe artifact sha256 %q as an extract marker for %s, will re-extract", sha, target.path)
		return false
	}
	// markerDisplay renders the same location rel refers to as a plain,
	// OS-native absolute path, purely for these log lines: every actual
	// filesystem operation below still goes through target.root and rel, this
	// is display-only.
	markerDisplay := filepath.Join(target.path, helpers.ExtractMarkerPrefix+sha)
	outcome := checkExtractMarker(target, sha)

	switch outcome.status {
	case extractMarkerUnknown:
		// Never actually produced by checkExtractMarker (every construction
		// site sets status explicitly) - this exists purely so the zero value
		// falls through to the same remove-and-fail-closed tail every other
		// non-match status does, rather than being silently treated as valid.
	case extractMarkerUnsafeSHA:
		// Unreachable from here: the ok check above already returned before
		// checkExtractMarker could be called with this same sha, so it can
		// never hand back extractMarkerUnsafeSHA to this switch. Listed only
		// so this switch stays exhaustive over extractMarkerStatus.
	case extractMarkerMatches:
		return true
	case extractMarkerMissing:
		out.Debugf("extract marker missing or unreadable at %s, will re-extract", markerDisplay)
	case extractMarkerMalformed:
		out.Debugf("extract marker at %s is not in the current format (legacy or corrupt), will re-extract", markerDisplay)
	case extractMarkerScanFailed:
		out.Debugf("failed to scan %s to verify its extract marker: %v, will re-extract", target.path, outcome.scanErr)
	case extractMarkerDrifted:
		out.Warnf(
			"installed tree %s no longer matches its extract marker (recorded entries=%d dirs=%d bytes=%d, "+
				"found entries=%d dirs=%d bytes=%d): the cache may have been modified after extraction; re-extracting",
			target.path, outcome.want.Entries, outcome.want.Dirs, outcome.want.Bytes, outcome.got.Entries, outcome.got.Dirs, outcome.got.Bytes,
		)
	}
	_ = target.root.Remove(rel)
	return false
}
