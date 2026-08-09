// Package signature verifies detached OpenPGP signatures over a collection's
// MANIFEST.json.
//
// Two boundaries bind everything in it, and both are properties of the package
// rather than of any one function.
//
// It does no IO into the collections tree. The only path anything here opens
// is the keyring the operator configured, so no verification decision can
// create, replace, or delete a byte under a project's ansible_collections -
// the install pipeline's own rooted writes stay the only writer there, and a
// reviewer auditing what may touch that tree never has to read this package.
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
