// Package signature verifies detached OpenPGP signatures over a collection's
// MANIFEST.json.
//
// Three boundaries bind everything in it, and each is a property of the package
// rather than of any one function.
//
// It does no IO into the collections tree. It opens two kinds of path, both
// read-only, and they differ in who chose them. The keyring is the operator's
// own, named by a flag, an environment value, or ansible.cfg. A signature
// source is repository content: a requirements file's entry, authored by
// whoever can commit to the repository rather than by the operator running the
// install, so a file URL there has this process read a local path a value it
// did not author selected - a provenance Fetcher.FetchRequirementSource's name
// is the rule for, and a residual its fetchFile records in place. What that
// rule still excludes is a value arriving across one of this project's other
// trust boundaries, a Galaxy server's version metadata and the persisted
// snapshot alike. It creates, replaces and deletes nothing anywhere, so no
// verification decision can touch a byte under a project's ansible_collections;
// the install pipeline's own rooted writes stay the only writer there, and a
// reviewer auditing what may touch that tree never has to read this package.
//
// No request it makes can carry a credential or inherit a relaxed TLS policy.
// Fetcher builds its own client rather than accepting one, and it builds it
// from fetch.NewUnauthenticated, which is handed no server configuration at
// all - so neither the token-attaching layer nor the per-origin TLS dispatch
// has an origin it could match, whatever a signature source names. Both are
// closed by construction rather than by a caller remembering to pass nothing:
// see fetch.NewUnauthenticated and Fetcher for the arguments themselves.
//
// It derives no state that is ever persisted. A keyring's entities and the
// verdicts drawn from them live for the run that computed them and reach
// neither the snapshot store nor the lockfile, so a later run never inherits a
// verification decision an earlier one made: every run answers the question
// against the key material it was pointed at itself. That matters because the
// snapshot is a documented trust boundary of this project - a principal who
// can write it influences what a run installs - and a cached "this verified"
// would hand that same principal the verdict as well as the input.
package signature
