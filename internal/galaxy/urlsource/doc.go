// Package urlsource is the grammar of a url collection or role source: a
// direct http(s) URL to a tar.gz artifact. It parses and canonicalizes a
// tarball URL, formats and parses the locator string that identifies a
// resolved url source everywhere the program persists one, and parses the
// credential-binding prefix a GO_GALAXY_URL_* variable declares.
//
// Nothing here performs I/O, and the package imports only helpers: the same
// boundary gitsource draws for git sources. requirements, lockfile, store,
// cleanup and the CLI's read-only commands all need this grammar, and none of
// them should link an HTTP client to get it. The download itself runs in the
// install pipeline over the dedicated client internal/galaxy/fetch builds for
// url sources.
//
// A tarball URL is repository content and is judged accordingly. It may carry
// no credential (ErrURLRequirementUserinfo): a url credential comes from the
// environment, bound to an origin and an optional path prefix by the
// operator. It may carry no "#" at all: a fragment is never sent to a server,
// and the locator grammar below relies on the URL part containing none. Its
// path is refused when it carries a dot or empty segment, because a server
// resolves those while the credential match reads the path as written, so a
// dot segment would let a requirements file spend a credential bound to one
// path prefix on any path of the host. One structural form is admitted beyond
// plain segments: the path may embed an absolute http(s) URL - the
// caching-proxy shape http://front/<upstream-url>, where a front host reads
// the rest of the request path as the URL it fetches and caches - and the
// embedded URL must itself be a url source in its canonical spelling, judged
// by this same grammar, so the only empty segment such a path carries is the
// embedded scheme's "//" separator (see checkSourcePath). A query string is
// allowed and is part of the source's identity; every message renders the URL
// through helpers.URLForMessage, which cuts it.
//
// The locator, url+<url>#sha256:<hex>, always carries its "#" even when the
// pin is absent, so the grammar parses left to right without guessing: the
// URL part never contains "#", and the pin, when present, is the sha256 of
// the artifact the URL served. A locator without a pin names a requirement
// before discovery; one with a pin names a resolved source, and is what flows
// into the artifact cache key, the installed record, the resolved snapshot
// and the lockfile. The pin is a real content digest - unlike a git
// collection, whose artifact is rebuilt from a commit, a url artifact is the
// origin's own bytes, so the same digest holds on every refetch.
package urlsource
