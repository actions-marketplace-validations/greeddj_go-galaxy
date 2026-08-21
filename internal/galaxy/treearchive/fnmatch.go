package treearchive

import (
	"path"
	"slices"
	"strings"
)

// Fnmatch reports whether name matches pattern under the semantics of
// Python's fnmatch.fnmatch on a POSIX system, which is what ansible applies
// build_ignore with: "*" matches any run of bytes including "/", "?"
// matches exactly one byte including "/", "[seq]" matches one byte of the
// set and "[!seq]" one outside it, a range inside a set compares byte
// values, a "]" first in a set is a literal, an unterminated "[" is a literal
// "[", a reversed range matches nothing, there is no escape character, and
// the match is anchored at both ends. Matching is case-sensitive, since
// os.path.normcase is the identity on POSIX.
//
// The unit is the byte, not the rune: ansible hands fnmatch the pattern and
// the path as bytes (to_bytes), so one "?" consumes one byte of the UTF-8
// encoding, a two-byte rune needs "??", and a set or range never matches a
// multi-byte rune. "*" is unaffected, a run of bytes is a run of runes.
//
// The matcher is a memoized backtracking walk over bytes rather than a
// translation to a regular expression, so its cost is bounded by the product
// of the two lengths whatever the pattern: a chain of stars cannot go
// exponential, which matters because the pattern comes from the repository.
func Fnmatch(pattern, name string) bool {
	m := &matcher{toks: compilePattern(pattern), bytes: name}
	m.width = len(m.bytes) + 1
	m.failed = make([]bool, (len(m.toks)+1)*m.width)
	return m.walk(0, 0)
}

// matcher is one match in progress. failed[pi*width+ni] marks a (token,
// byte) state already proven not to match; a state that matches ends the
// walk, so only failures are worth remembering.
type matcher struct {
	toks   []token
	bytes  string
	failed []bool
	width  int
}

func (m *matcher) walk(pi, ni int) bool {
	if pi == len(m.toks) {
		return ni == len(m.bytes)
	}
	if m.failed[pi*m.width+ni] {
		return false
	}
	matched := m.step(pi, ni)
	if !matched {
		m.failed[pi*m.width+ni] = true
	}
	return matched
}

// step tries the token at pi against the input at ni.
func (m *matcher) step(pi, ni int) bool {
	tok := m.toks[pi]
	if tok.kind == tokStar {
		for k := ni; k <= len(m.bytes); k++ {
			if m.walk(pi+1, k) {
				return true
			}
		}
		return false
	}
	if ni >= len(m.bytes) || !tok.accepts(m.bytes[ni]) {
		return false
	}
	return m.walk(pi+1, ni+1)
}

// accepts reports whether a single-byte token matches r.
func (t token) accepts(r byte) bool {
	switch t.kind {
	case tokAny:
		return true
	case tokLiteral:
		return r == t.lit
	case tokSet:
		return t.set.contains(r)
	case tokStar:
		return false
	default:
		return false
	}
}

type tokenKind uint8

const (
	tokLiteral tokenKind = iota + 1
	tokStar
	tokAny
	tokSet
)

type token struct {
	set  *charSet
	lit  byte
	kind tokenKind
}

// charSet is one bracket expression: single bytes and inclusive ranges.
type charSet struct {
	singles []byte
	ranges  [][2]byte
	negated bool
}

func (s *charSet) contains(r byte) bool {
	in := slices.Contains(s.singles, r)
	for i := 0; i < len(s.ranges) && !in; i++ {
		in = r >= s.ranges[i][0] && r <= s.ranges[i][1]
	}
	return in != s.negated
}

// compilePattern tokenizes pattern the way fnmatch.translate scans it. A
// run of consecutive stars collapses into one, which changes nothing in what
// matches and keeps the state table small.
func compilePattern(pattern string) []token {
	p := pattern
	toks := make([]token, 0, len(p))
	for i := 0; i < len(p); {
		switch p[i] {
		case '*':
			if len(toks) == 0 || toks[len(toks)-1].kind != tokStar {
				toks = append(toks, token{kind: tokStar})
			}
			i++
		case '?':
			toks = append(toks, token{kind: tokAny})
			i++
		case '[':
			set, next, ok := compileSet(p, i)
			if !ok {
				toks = append(toks, token{kind: tokLiteral, lit: '['})
				i++
				continue
			}
			toks = append(toks, token{kind: tokSet, set: set})
			i = next
		default:
			toks = append(toks, token{kind: tokLiteral, lit: p[i]})
			i++
		}
	}
	return toks
}

// compileSet parses the bracket expression opening at p[start]. It returns
// the index past the closing bracket, or ok false when there is none, in
// which case the caller treats the bracket as a literal. The closing bracket
// is searched for from past an optional "!" and past a first "]", both of
// which fnmatch.translate steps over before it looks.
func compileSet(p string, start int) (*charSet, int, bool) {
	j := start + 1
	negated := false
	if j < len(p) && p[j] == '!' {
		negated = true
		j++
	}
	if j < len(p) && p[j] == ']' {
		j++
	}
	end := strings.IndexByte(p[min(j, len(p)):], ']')
	if end < 0 {
		return nil, 0, false
	}
	end += j
	body := p[start+1 : end]
	if negated {
		body = body[1:]
	}
	set := parseSetBody(body)
	set.negated = negated
	return set, end + 1, true
}

// parseSetBody reads the bytes and ranges of a bracket expression. A "-"
// first or last is a literal; a reversed range is dropped, so it matches
// nothing, as fnmatch.translate drops it.
func parseSetBody(body string) *charSet {
	set := &charSet{}
	for i := 0; i < len(body); i++ {
		if i+2 < len(body) && body[i+1] == '-' {
			lo, hi := body[i], body[i+2]
			if lo <= hi {
				set.ranges = append(set.ranges, [2]byte{lo, hi})
			}
			i += 2
			continue
		}
		set.singles = append(set.singles, body[i])
	}
	return set
}

// Rules is a build's exclusion list: Patterns, each matched with Fnmatch
// against the "/"-joined path relative to the tree root the build walks from
// - so a pattern without a slash matches at the root only - and DirNames, the
// directory basenames pruned at every depth. A zero Rules excludes nothing.
type Rules struct {
	DirNames map[string]struct{}
	Patterns []string
}

// Skip reports whether the entry at rel, a clean "/"-joined path relative to
// the tree root, is left out of the build.
func (r Rules) Skip(rel string, isDir bool) bool {
	if isDir {
		if _, ok := r.DirNames[path.Base(rel)]; ok {
			return true
		}
	}
	for _, p := range r.Patterns {
		if Fnmatch(p, rel) {
			return true
		}
	}
	return false
}

// Excludes reports whether rel, or any directory above it, is left out of
// the build: a link may only point at what the artifact carries.
func (r Rules) Excludes(rel string, isDir bool) bool {
	parts := strings.Split(rel, "/")
	for i := 1; i < len(parts); i++ {
		if r.Skip(strings.Join(parts[:i], "/"), true) {
			return true
		}
	}
	return r.Skip(rel, isDir)
}
