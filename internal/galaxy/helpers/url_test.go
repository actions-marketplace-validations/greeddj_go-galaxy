package helpers

import "testing"

// TestWithoutQuery covers the cut itself and, in the same table, the shapes
// that carry no query at all: the function is applied to values this tool did
// not author - a server's download URL, a requirements file's signature
// source - so "leaves an ordinary URL alone" is as much a property worth
// pinning as "removes a presigned capability".
func TestWithoutQuery(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"empty", "", ""},
		{"no query", "https://h/x/artifact.tar.gz", "https://h/x/artifact.tar.gz"},
		{"presigned capability", "https://h/x?X-Amz-Signature=deadbeef", "https://h/x"},
		{"multi-parameter query", "https://h/x?a=1&b=2", "https://h/x"},
		{"empty query", "https://h/x?", "https://h/x"},
		{"question mark in a later parameter", "https://h/x?a=b?c", "https://h/x"},
		{"bare question mark", "?", ""},
		{"query on a file url", "file:///tmp/sig.asc?a=b", "file:///tmp/sig.asc"},
		{"fragment is left alone", "https://h/x#frag", "https://h/x#frag"},
		// The cut is textual, so a value url.Parse would refuse is stripped
		// exactly like one it accepts. That is the whole reason the cut is
		// textual, and this row is what says so.
		{"unparseable url", "http://%zz/x?token=t", "http://%zz/x"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := WithoutQuery(tc.raw); got != tc.want {
				t.Errorf("WithoutQuery(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// TestWithoutFragment covers the cut and, in the same table, every shape that
// carries no fragment at all. The second set is the larger one for the same
// reason WithoutQuery's is: the function runs over values this tool did not
// author, so "leaves an ordinary URL alone" is the property most of its inputs
// depend on.
//
// The file row is the one the cut exists for beyond hygiene: url.Parse splits a
// fragment off before the Path a file:// source is opened by, so a value that
// keeps its fragment names a file nothing read.
func TestWithoutFragment(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"empty", "", ""},
		{"no fragment", "https://h/x/sig.asc", "https://h/x/sig.asc"},
		{"fragment", "https://h/x#frag", "https://h/x"},
		{"empty fragment", "https://h/x#", "https://h/x"},
		{"second hash inside the fragment", "https://h/x#a#b", "https://h/x"},
		{"bare hash", "#", ""},
		{"fragment on a file url", "file:///tmp/a#b.asc", "file:///tmp/a"},
		{"query is left for WithoutQuery", "https://h/x?a=b", "https://h/x?a=b"},
		{"hash after a query", "https://h/x?a=b#c", "https://h/x?a=b"},
		// The cut is textual, so a value url.Parse would refuse is stripped
		// exactly like one it accepts - the same property WithoutQuery's own
		// table records, for the same reason.
		{"unparseable url", "http://%zz/x#f", "http://%zz/x"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := WithoutFragment(tc.raw); got != tc.want {
				t.Errorf("WithoutFragment(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// urlCase is one raw value and what WithoutUserinfo must make of it.
type urlCase struct {
	name string
	raw  string
	want string
}

// withoutUserinfoCases is the table two tests below share: the first asserts
// the cut itself, the second asserts that composing it with the other two cuts
// gives the same answer in any of their six orders, which is a claim about
// every one of these values rather than about a chosen few.
//
// It is a function rather than a package-level var so the shared table cannot
// be mutated by whichever test runs first.
func withoutUserinfoCases() []urlCase {
	return []urlCase{
		// The values that are cut. The last two are why the scan is written by
		// hand instead of over a *url.URL: url.Parse reports "http:u:p@h/x" as
		// an opaque URL with no authority to redact, and two "@" in one
		// authority is what separates the last "@" from the first.
		{"credentialed https url", "https://u:p@h/x", "https://h/x"},
		{"scheme-relative url", "//u:p@h/x", "//h/x"},
		{"opaque url", "http:u:p@h/x", "http:h/x"},
		{"two at signs in the authority", "https://u@p@h/x", "https://h/x"},
		{"query is left for WithoutQuery", "https://u:p@h/x?a=b", "https://h/x?a=b"},
		// The values that are not. Every one of them carries something that
		// resembles the cut - an "@" in a path, a ":" that is not a scheme, a
		// scheme with no authority at all - and none of them carries a
		// credential.
		{"no userinfo", "https://h/x", "https://h/x"},
		{"at sign in the path", "https://h/x@y", "https://h/x@y"},
		{"authority ends at the query", "https://h?a=b", "https://h?a=b"},
		{"at sign in a file path", "file:///tmp/a@b.asc", "file:///tmp/a@b.asc"},
		{"file url with a single slash", "file:/abs/p", "file:/abs/p"},
		{"bare absolute path", "/bare/abs/path", "/bare/abs/path"},
		{"relative path", "relative/path", "relative/path"},
		{"data url", "data:text/plain;base64,aGk=", "data:text/plain;base64,aGk="},
		{"unparseable url", "http://%zz/x", "http://%zz/x"},
		{"empty", "", ""},
		// The residual, in the two spellings that are not the "?" one: a
		// delimiter sitting inside what was meant as a userinfo ends the
		// authority scan, so no "@" is left inside it and this function hands
		// the value back whole. What a message ends up carrying is then decided
		// by the cuts composed alongside this one, which is what
		// TestDisplayCutsOnADelimiterInsideUserinfo below pins.
		{"hash inside the userinfo", "https://user:pa#55w0rd@h/x", "https://user:pa#55w0rd@h/x"},
		{"slash inside the userinfo", "https://user:pa/55w0rd@h/x", "https://user:pa/55w0rd@h/x"},
		// The one legitimate value the cut rewrites, recorded as a property
		// rather than left to be discovered: a mailto is not a fetchable
		// location for any caller here, and erring toward removing too much is
		// the deliberate direction.
		{"mailto", "mailto:u@e.com", "mailto:e.com"},
	}
}

// TestWithoutUserinfo covers the cut and, in the same table, every shape that
// must survive it untouched. The second set is the larger one on purpose: this
// runs over values this tool did not author, so "leaves an ordinary URL alone"
// is the property most of its inputs depend on.
func TestWithoutUserinfo(t *testing.T) {
	t.Parallel()

	for _, tc := range withoutUserinfoCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Two mutations, applied through go test -overlay so no production
			// file is edited.
			//
			// Replacing strings.LastIndex with strings.Index fails the
			// two-at-signs row, which is the only value that can tell them
			// apart:
			//
			//	url_test.go:167: WithoutUserinfo("https://u@p@h/x") = "https://p@h/x", want "https://h/x"
			//
			// Dropping the rest[i] == ':' guard on the scheme scan - so any
			// first delimiter is taken for a scheme - fails the
			// scheme-relative row, whose first delimiter is the "/" of its own
			// "//":
			//
			//	url_test.go:167: WithoutUserinfo("//u:p@h/x") = "//u:p@h/x", want "//h/x"
			if got := WithoutUserinfo(tc.raw); got != tc.want {
				t.Errorf("WithoutUserinfo(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// TestTheThreeCutsComposeInAnyOrder pins the claim all three cuts' doc comments
// make and that signature.FetchRequirementSource relies on when it composes
// them in one expression: none can reach across another's boundary, so the
// order they are applied in is immaterial.
//
// It is asserted over the whole table rather than over a hand-picked value,
// since the claim is about the functions and not about any one input, and over
// all six orders rather than a chosen pair, since three cuts have six.
func TestTheThreeCutsComposeInAnyOrder(t *testing.T) {
	t.Parallel()

	// Bound to one-letter names so each composition below fits on one line and
	// reads as the order its own name spells out.
	q, f, u := WithoutQuery, WithoutFragment, WithoutUserinfo
	orders := []struct {
		apply func(string) string
		name  string
	}{
		{name: "query, fragment, userinfo", apply: func(raw string) string { return u(f(q(raw))) }},
		{name: "query, userinfo, fragment", apply: func(raw string) string { return f(u(q(raw))) }},
		{name: "fragment, query, userinfo", apply: func(raw string) string { return u(q(f(raw))) }},
		{name: "fragment, userinfo, query", apply: func(raw string) string { return q(u(f(raw))) }},
		{name: "userinfo, query, fragment", apply: func(raw string) string { return f(q(u(raw))) }},
		{name: "userinfo, fragment, query", apply: func(raw string) string { return q(f(u(raw))) }},
	}

	// A userinfo carrying a literal "?" is one spelling of the disclosed
	// residual, and it composes identically too - every order truncates at that
	// "?" rather than disagreeing about it. The other two spellings of it are
	// rows in the table above.
	cases := withoutUserinfoCases()
	raws := make([]string, 0, len(cases)+1)
	raws = append(raws, "https://user:pa?55w0rd@h/x")
	for _, tc := range cases {
		raws = append(raws, tc.raw)
	}

	for _, raw := range raws {
		want := orders[0].apply(raw)
		for _, order := range orders[1:] {
			if got := order.apply(raw); got != want {
				t.Errorf("composing on %q: %s = %q, %s = %q", raw, order.name, got, orders[0].name, want)
			}
		}
	}
}

// TestDisplayCutsOnADelimiterInsideUserinfo records the residual
// WithoutUserinfo discloses as a measured outcome rather than only as prose,
// and it runs the exact composition signature.FetchRequirementSource renders
// every one of its messages from.
//
// A value whose intended userinfo contains one of the authority delimiters
// leaves no "@" inside the authority the scan reads, so the cut never fires and
// what an operator sees is whatever the other two cuts leave behind. url.Parse
// refuses all three of these values, so no request is composed from any of
// them: this is a claim about what reaches a message, never about what reaches
// a host.
//
// The slash row is the weak one and is here to say so out loud: nothing cuts at
// "/", so that value is rendered whole, credential and all. It cannot be cut
// without truncating every ordinary URL at its authority - "https://h/x@y" is
// the legitimate shape it is textually indistinguishable from, and that one is
// a row in the table above.
//
// The first row is the positive control the other three need: the same
// composition on a userinfo carrying no delimiter removes the credential
// outright, so the rows below are about the delimiter rather than about a cut
// that never works.
func TestDisplayCutsOnADelimiterInsideUserinfo(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"positive control: no delimiter inside the userinfo", "https://user:pa55w0rd@h/x", "https://h/x"},
		{"question mark", "https://user:pa?55w0rd@h/x", "https://user:pa"},
		{"hash", "https://user:pa#55w0rd@h/x", "https://user:pa"},
		{"slash", "https://user:pa/55w0rd@h/x", "https://user:pa/55w0rd@h/x"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Making WithoutFragment a no-op - its body replaced by
			// `return raw` - applied through go test -overlay so no production
			// file is edited, fails the hash row of this table alone, on the
			// whole credential it then renders (TestWithoutFragment fails too):
			//
			//	url_test.go:268: display cuts on "https://user:pa#55w0rd@h/x" = "https://user:pa#55w0rd@h/x", want "https://user:pa"
			got := WithoutUserinfo(WithoutFragment(WithoutQuery(tc.raw)))
			if got != tc.want {
				t.Errorf("display cuts on %q = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}
