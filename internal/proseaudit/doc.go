// Package proseaudit gates this repository's comments against the code they
// cite.
//
// It holds no production code and nothing imports it: its entire content is
// one test, which is the load-bearing choice rather than an accident of
// packaging. `go test ./...` already runs it, so the gate needs no Justfile
// target, no CI step, no new dependency, and no depguard allow-list entry. A
// standalone script would have needed all four and would still have run only
// when someone remembered to run it - and the author who breaks these
// citations is precisely the one not thinking about them, since an unrelated
// feature commit shifts line numbers in files it never opens.
//
// The rule enforced today: a comment anywhere in the module may cite a test
// file's line (`foo_test.go:NNN`, with NNN a real number) only when that line
// is one `go test` could attribute a failure to, and may not cite a
// production file's line at all. This package's own prose spells such an
// example with a placeholder rather than digits, so that the gate needs no
// exemption for the files describing it.
// See refs_test.go for the exact predicate, for why a citation into
// production code is answered with an identifier instead, and for what the
// gate deliberately does not check.
package proseaudit
