// Package proseaudit gates this repository's own written text: comments
// against the code they cite, and committed text against the house style.
//
// It holds no production code and nothing imports it: its entire content is
// tests, which is the load-bearing choice rather than an accident of
// packaging. `go test ./...` already runs them, so the gates need no Justfile
// target, no CI step, no new dependency, and no depguard allow-list entry. A
// standalone script would have needed all four and would still have run only
// when someone remembered to run it - and the author who breaks either rule is
// precisely the one not thinking about it, since an unrelated feature commit
// shifts line numbers in files it never opens, and a dash arrives by paste
// rather than by decision.
//
// Two rules are enforced today.
//
// A comment anywhere in the module may cite a test file's line
// (`foo_test.go:NNN`, with NNN a real number) only when that line is one `go
// test` could attribute a failure to, and may not cite a production file's
// line at all. This package's own prose spells such an example with a
// placeholder rather than digits, so that the gate needs no exemption for the
// files describing it. See refs_test.go for the exact predicate, for why a
// citation into production code is answered with an identifier instead, and
// for what the gate deliberately does not check.
//
// No committed file may carry an em dash (U+2014) or an en dash (U+2013);
// hyphen-minus is the only dash this repository writes, in prose, code,
// comments and commit text alike. See dashes_test.go, which enumerates
// through git rather than by walking the filesystem, and for why the one test
// that legitimately needs an em dash spells it as an escape instead of
// earning an exemption.
package proseaudit
