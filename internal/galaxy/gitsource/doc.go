// Package gitsource is the grammar of a git collection source and the seam the
// rest of the program reaches one through. It parses and canonicalizes a
// repository URL, classifies a ref, validates a #subdir fragment, splits
// ansible's combined "url#subdir,ref" spelling in ansible's own order, formats
// and parses the locator string that identifies a resolved git collection
// everywhere the program persists a source, matches a requirement URL against
// the operator's host-bound credentials, and declares the Client interface the
// install pipeline calls to turn a request into built artifacts.
//
// Nothing here imports go-git, and that is a boundary rather than a
// convenience: requirements, lockfile, store, cleanup and the CLI's read-only
// commands all need this grammar, and none of them should link a transport to
// get it. The one production implementation of Client lives in
// internal/galaxy/gitfetch, which is the only package that imports go-git.
//
// Two of the values parsed here are repository content and are judged
// accordingly. A URL may carry no credential at all (ErrGitURLUserinfo):
// credentials come from the environment, bound to a host by the operator, and
// MatchCredential is the only way one is paired with a URL. A path is refused
// when it could be read as an argument by the remote's upload-pack (a leading
// "-" in its first segment) or break out of the single quotes go-git wraps it
// in over ssh (a quote, a NUL, a control rune) - the remote is a host the
// operator trusts with a credential, so a requirements file must not be able
// to hand that host an argument.
//
// The locator, git+<url>#<subdir>@<commit>, always carries its "#" even when
// the subdir is empty, so the grammar parses left to right without guessing:
// a canonical URL never contains "#", and the commit, when present, is the
// suffix after the last "@" and is exactly forty lowercase hex digits. A
// locator without a commit names a requirement before discovery; one with a
// commit names a resolved collection, and is what flows into the artifact
// cache key, the installed record, the resolved snapshot and GALAXY.yml.
package gitsource
