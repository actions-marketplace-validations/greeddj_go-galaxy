// Package gitfetch is the one production package that talks to a git remote.
// It implements gitsource.Client on top of go-git's plumbing: one advertised
// references round trip per acquisition, a pack fetched straight through that
// same upload-pack session into a bare on-disk object store, the requested
// commit's tree read object by object, and - the one step where Acquire and
// AcquireRole differ - the collections it holds handed to
// internal/galaxy/collectionbuild, or the role its root is handed to
// internal/galaxy/rolebuild, to become artifacts.
//
// The fetch is driven at the session level rather than through go-git's
// Remote, for three reasons that are each a boundary of this package. The
// advertisement is read once and decides everything - which commit a ref
// names, whether the remote allows a shallow fetch (a deepen sent to a remote
// that did not advertise shallow is a protocol error) and whether it serves a
// commit by hash - so no second round trip re-asks it. The wants are hashes
// learned from that advertisement, never names, so a ref moved between the
// advertisement and the fetch cannot substitute content: the pin is the
// commit. And the transport a session runs on is chosen here per Fetcher,
// never through go-git's process-global protocol registry for http(s), so a
// run's own HTTP client - with its stall watchdog and its refusal to follow a
// redirect off the origin - is the only one a credential ever travels over.
//
// Two hardening steps do touch go-git's globals, exactly once per process:
// the file and git transports are removed from the registry (the first execs
// git-upload-pack, the second is plaintext TCP with no authentication), and
// go-git's reading of ~/.ssh/config is switched off so a developer's Hostname
// or Port rewrite cannot redirect a CI run. URL scheme validation in gitsource
// refuses both transports before go-git ever sees a URL; the registry edit is
// the second lock on the same door.
//
// What a remote can do to this process is bounded on disk rather than in
// flight: the object store sits on a counting filesystem that refuses writes
// past helpers.GitPackMaxSize, which covers https and ssh alike since neither
// transport exposes a byte counter of its own. Time is bounded by the context
// the caller derives from its git deadline. Nothing is ever checked out: no
// worktree, no submodule, no hook, no .gitattributes; a tree entry is read by
// name and refused when its name is not a safe path element, so the archive
// the builder writes is one the extractor will accept.
package gitfetch
