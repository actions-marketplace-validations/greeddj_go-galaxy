package signature

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	pgperrors "github.com/ProtonMail/go-crypto/openpgp/errors"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// signatureArmorHeader opens an ASCII-armored detached signature, which is what
// `gpg --detach-sign --armor` writes. It is the needle checkOne routes on, and
// it is the whole opening line rather than the bare armorBlockStart prefix for
// the reason readEntities' own routing test states: the needle has to be
// exactly as selective as the reader behind it, which here accepts the
// "PGP SIGNATURE" block type and nothing else.
const signatureArmorHeader = "-----BEGIN PGP SIGNATURE-----"

// errNoSignatureData is the verdict for a blob carrying no OpenPGP data at all,
// whether it arrived with no bytes worth reading or with an armor envelope that
// decodes to none.
//
// It is raised by checkOne rather than left to the library, because
// go-crypto answers an empty packet stream with ErrUnknownIssuer - the same
// error a signature from a key outside the keyring produces. Passing that
// through would report an empty blob as NO_PUBKEY, a status an operator may
// legitimately have configured as ignorable, so a source that returned nothing
// at all would be tolerated by an ignore list that was never meant to cover it.
//
// Which of the two shapes a source sends is not the operator's choice, which is
// why the check that decides emptiness sits after the envelope rather than
// before it: a blank blob is what a broken source produces, and a well-formed
// empty armor envelope is what someone who read this comment would send
// instead.
var errNoSignatureData = errors.New("signature blob carries no OpenPGP data")

// Blob is one detached signature as it was gathered, together with the source
// it came from.
type Blob struct {
	// Origin names where this blob came from - a URL, a file path, or a
	// server's own signature entry. Nothing here parses or validates it; it is
	// carried so a Failure can say which source produced it, and it is
	// reported back verbatim.
	//
	// Reported back verbatim is why every filler cuts before it fills, and
	// there are two. Fetcher.FetchRequirementSource fills it with the source it
	// was handed under three cuts - the query string (helpers.WithoutQuery),
	// the userinfo (helpers.WithoutUserinfo) and the fragment
	// (helpers.WithoutFragment) - so a file source names the path that was
	// opened. collections.serverSignatureBlobs is the other, filling it from
	// the version-metadata document's own href under helpers.WithoutCredentials
	// - the first two of those three cuts, named once - and a length cap; the
	// fragment survives there because nothing was opened for it to have to name
	// exactly. Between them, neither a presigned source's capability nor an
	// embedded credential rides into a message this value is rendered into.
	Origin string
	// Data is the raw signature, armored or binary. Which of the two it
	// carries is decided from these bytes, never from Origin.
	Data []byte
}

// Failure is one signature that was checked, did not verify, and carried a
// status this run does not tolerate. A failure whose status is in the policy's
// ignore set never becomes one of these.
//
// Err leads the fields for alignment rather than for emphasis: every field here
// holds a pointer, so the interface has to sit first for the struct's pointer
// span to be the smallest one possible.
type Failure struct {
	// Err is the underlying verification error, kept so a caller can inspect
	// it rather than parse a rendered message.
	Err error
	// Origin is the Blob.Origin of the signature that failed.
	Origin string
	// Status is the gpg status code the failure classifies as, drawn from the
	// eight status.go declares a verdict can carry.
	Status Status
}

// Result is one collection's verification outcome. Verify returns a fully
// populated Result on a negative verdict as well as a positive one, and a
// partly populated one when the caller's own pull failed, so a caller that
// reports what happened never has to reconstruct it from the error.
type Result struct {
	// Failures holds every signature that was checked, failed, and was not
	// ignored, in the order the pull yielded them.
	Failures []Failure
	// Verified is how many DISTINCT keys verified a signature. It is what a
	// caller applies the chain rule with: the MANIFEST.json -> FILES.json ->
	// content check runs only when at least one signature verified, since
	// nothing authenticates the chain's first link otherwise.
	//
	// It is not "how many of the blobs are good", for two separate reasons. A
	// non-strict counted policy stops checking the moment its count is
	// reached, so blobs after that point are never looked at; and two blobs
	// that verify against one key count once, so a signature supplied twice
	// cannot stand in for a second signer. Verify's own doc comment holds the
	// argument for that second one.
	Verified int
	// VacuousPass records that the policy was satisfied while not one
	// signature verified - the verdict is a pass and Verified is zero.
	//
	// The verdict itself is ansible's, deliberately: a non-strict spelling is
	// satisfied by an empty set, so a bare count and "all" alike pass when
	// nothing was gathered, and "all" passes just as well when every blob it
	// was handed failed with a status this run tolerates. A hostile server, or
	// a poisoned snapshot, that serves no signatures therefore satisfies the
	// default policy while nothing was verified at all. Keeping that verdict is
	// the point - a tool that silently exits differently from the one it stands
	// in for is worse than one that shares a documented weakness, and an
	// operator who needs the hole closed closes it with the strict marker,
	// which can never produce a vacuous pass because strict fails outright on a
	// zero count.
	//
	// What is not inherited is the silence, and this field is how it is broken.
	// The warning belongs to a collection, which this package does not know
	// about and must not learn, so the fact is handed back for whoever knows
	// the collection to name it on stderr. A caller that ignores this field
	// reproduces exactly the silence this field exists to prevent.
	//
	// It is reported rather than left to be derived, because the condition is
	// not "Verified == 0 and the policy looked lenient": it also covers a
	// policy that was handed blobs and tolerated every failure they carried,
	// which a caller could only see by recomputing the whole verdict.
	VacuousPass bool
}

// NextBlob yields one gathered signature at a time, in the caller's own order.
//
// It returns the next blob and true; false once the caller has no more to
// supply; or an error, which Verify surfaces as it came without checking
// anything further. The error is read first, so a call reporting one has its
// other two results ignored - and what it reports is a gather failure, never a
// verdict about any signature.
//
// It is a pull rather than a slice so a caller can fetch, hand over and
// release one source at a time. helpers.SignatureMaxSize bounds one blob at
// 1 MiB and helpers.MaxSignaturesPerCollection allows 64 of them, so a
// materialized slice is 64 MiB resident before the first is checked, once per
// collection and therefore once per worker; pulling makes that one blob. The
// second thing it buys is ordering: a source that cannot be fetched fails at
// the point in the list where it sits, instead of aborting the whole gather
// before any signature has been looked at.
type NextBlob func() (Blob, bool, error)

// Verify checks the blobs next yields as detached signatures over manifest and
// returns the collection's verdict.
//
// The algorithm is ansible-core 2.21.2's verify_file_signatures, frozen: a
// failure whose status this run ignores is invisible - neither a success nor a
// failure - a success counts, and a non-"all" policy stops the moment its count
// is reached, so signatures after that point are never checked and cannot fail
// the run. The verdict is then three clauses, in Policy.verdict.
//
// Two things are done differently, and only the second can move a verdict.
//
// The order: ansible iterates a set, so which signatures it checks before a
// counted policy stops is undefined; this walks the caller's own order, which
// makes the checked set and the Failures order the same on every run over the
// same input.
//
// The unit counted: a success counts a KEY, not a blob, so one signature
// supplied twice satisfies a count of one and not a count of two. Counting
// blobs would let a replayed signature stand in for a second signer, which
// costs an attacker nothing - the blob list arrives from a Galaxy server or a
// persisted snapshot, both documented trust boundaries of this project, so the
// same bytes can be offered under two origins - while an operator who writes 2
// is asking for two signers. This is strictly stricter than counting blobs: it
// can refuse a run a blob count would have passed, never the reverse. A
// signature that verifies against a key already counted is not a failure
// either, since it verified; it simply moves no counter.
//
// Result.VacuousPass is likewise an addition to what is reported rather than a
// change to what is decided.
//
// A negative verdict returns the Result as well as an error, which names the
// clause that fired and joins every non-ignored Failure behind
// helpers.ErrSignatureVerificationFailed. A gather error returns the Result
// built so far alongside that error, so a caller can still report what was
// checked before the list ran out.
//
// kr is read and never written, here or anywhere else in this package, so one
// *Keyring is shared across every worker of a run rather than cloned per
// worker. That is the contract this function is written to, and it has now been
// exercised as well as argued: collections.TestVerifyContextIsSafeForConcurrentUse
// drives concurrent verifications through one shared keyring, policy and
// fetcher under -race.
func Verify(manifest []byte, next NextBlob, kr *Keyring, p Policy) (Result, error) {
	// Fail closed rather than dereference a nil keyring, and fail as the
	// configuration error it is: a caller asking for verification with no key
	// material has learned nothing about the collection, so this must never
	// look like a verdict a signature produced.
	if kr == nil || kr.Len() == 0 {
		return Result{}, fmt.Errorf("%w: nothing to verify signatures against", helpers.ErrKeyringRequired)
	}

	walk, err := walkBlobs(manifest, next, kr, p)
	verified := len(walk.signers)
	result := Result{Failures: walk.failures, Verified: verified}
	if err != nil {
		return result, err
	}

	passed := p.verdict(verified, len(walk.failures), walk.gathered)
	result.VacuousPass = passed && verified == 0
	if passed {
		return result, nil
	}

	return result, verificationError(p, verified, walk.failures)
}

// blobWalk is what one pass over a caller's pull produced, before any clause of
// the policy has been applied to it.
type blobWalk struct {
	// failures holds every signature that failed with a status this run does
	// not tolerate, in the order the pull yielded them.
	failures []Failure
	// signers holds one entry per distinct key that verified a signature,
	// keyed as signerKey describes.
	signers []string
	// gathered is how many blobs the pull yielded before the walk stopped,
	// which is the term Policy.verdict's vacuous clause reads.
	gathered int
}

// walkBlobs pulls and checks blobs until the pull runs out, the required count
// is reached, or the pull reports an error, which it returns as it came.
func walkBlobs(manifest []byte, next NextBlob, kr *Keyring, p Policy) (blobWalk, error) {
	// Both of walk's slices start nil because the overwhelmingly common outcome
	// is one signature that verifies and nothing that fails, and a nil slice
	// appends into an allocation only once there is something to hold. signers
	// is a slice rather than a set because helpers.MaxSignaturesPerCollection
	// bounds it at 64 and a real collection carries one or two, so a linear
	// scan beats hashing and, unlike a map, costs nothing when it stays empty.
	var walk blobWalk

	for {
		blob, ok, err := next()
		if err != nil {
			return walk, err
		}
		if !ok {
			return walk, nil
		}
		walk.gathered++

		signer, checkErr := checkOne(manifest, blob.Data, kr)
		if checkErr != nil {
			walk.failures = recordFailure(walk.failures, blob.Origin, checkErr, p.Ignore)

			continue
		}

		walk.signers = recordSigner(walk.signers, signer)
		// An "all" policy checks every blob; a counted one stops here, which is
		// what makes a bare N mean "at least N" and leaves any blob after the
		// Nth distinct signer unexamined.
		if !p.Required.All && len(walk.signers) == p.Required.Count {
			return walk, nil
		}
	}
}

// recordSigner adds the key that verified a signature to signers, unless a
// signature from that same key already counted.
func recordSigner(signers []string, signer *openpgp.Entity) []string {
	key := signerKey(signer)
	if slices.Contains(signers, key) {
		return signers
	}

	return append(signers, key)
}

// recordFailure classifies one check failure and records it, unless its status
// is one this run tolerates - in which case it is invisible to both counters,
// which is what makes an ignored failure neither a success nor a failure.
func recordFailure(failures []Failure, origin string, err error, ignore StatusSet) []Failure {
	status := classify(err)
	if ignore.Ignores(status) {
		return failures
	}

	return append(failures, Failure{Origin: origin, Status: status, Err: err})
}

// signerKey identifies the entity a signature verified against, so that two
// signatures by one key count once.
//
// The identifier is the primary key's fingerprint, held as raw bytes in a
// string rather than hex-encoded, since nothing prints it. A signature made by
// a subkey reports its primary entity here, which is the answer that makes
// "two signers" mean two independent keys rather than two packets of one key's
// material.
//
// A signature that verified while naming no identifiable entity keys on the
// empty string, so every such signature collapses onto one another rather than
// each inflating the count.
//
// That guard is deliberately uncovered, and the reason is the fixture rather
// than the seam: this function is unexported and takes the entity as a
// parameter, so a test could hand it a nil one in a line, but reaching it the
// way a run would means an openpgp.CheckDetachedSignature that returns a nil
// error and no entity - which no committed material produces and no shape of
// blob has been found to produce. Removing the guard costs a nil dereference
// inside a verification walk, on an input this package would then have no
// verdict for at all; what the empty string buys on top of not panicking is that
// two such signatures collapse onto one key rather than reading as two distinct
// signers, which is how a required-count policy would be satisfied by one blob.
func signerKey(signer *openpgp.Entity) string {
	if signer == nil || signer.PrimaryKey == nil {
		return ""
	}

	return string(signer.PrimaryKey.Fingerprint)
}

// verdict is ansible's three-clause decision, and the whole of it.
//
// gathered is how many blobs Verify pulled, not how many of them moved a
// counter: the vacuous clause asks whether any signature existed to check,
// which an ignored failure does not change. That is what separates "nothing was
// gathered" - a pass - from "one blob was gathered, failed, and its failure was
// tolerated", which passes only under "all", where a tolerated failure leaves
// nothing recorded against the collection.
//
// Pulled is the same number as supplied wherever this clause can act on it.
// Verify stops pulling early only once verified has reached Count, which
// satisfies the clause's second term outright, so a run that left blobs
// unpulled never reaches the first term with a value it would answer
// differently.
//
// The clauses are ordered, not independent: strict is answered first, so a
// strict policy that verified nothing fails whatever the other two would have
// said. verdictReason mirrors this order exactly and must keep mirroring it, or
// a run's message would name a clause other than the one that fired.
func (p Policy) verdict(verified, failed, gathered int) bool {
	if p.Required.Strict && verified == 0 {
		return false
	}
	if p.Required.All {
		return failed == 0
	}

	// The comparison is equality rather than "at least", because the loop
	// breaks at the Count-th success and so can never overshoot it - except
	// under a Count of zero, where an operator asked for no floor and a
	// signature that verified anyway leaves the two unequal. Both are
	// ansible's, and the second is the reason this is not written as >=.
	return gathered == 0 || verified == p.Required.Count
}

// maxRenderedFailures bounds how many per-signature causes a negative
// verdict's own MESSAGE renders. It does not bound how many signatures this
// run checked, and it does not bound how many causes stay reachable through
// errors.Is once the message has been built - verdictError.Unwrap returns
// every cause regardless; see that type's own doc comment for why the two
// must not be the same knob.
//
// The number is derived rather than picked: it is the count of distinct
// statuses a verdict can carry - the eight status.go declares in its first
// const group - which is the count of distinct remedies a failure set can
// point an operator at, so a message at the cap can still exhibit every shape
// the vocabulary has. It cannot claim the converse: causes render in the
// pull's own order, so a message at the cap is not guaranteed to show every
// status actually present in a failure set, only that it could.
//
// It completes a bound the item side already had and the count side never
// did, phrased as count x per-item. helpers.MessageValueMaxLen already bounds
// the item - Blob.Origin, at both of its producers, serverBlobOrigin's href
// and parseRequirementSource's display form - and a bound on one item is not
// a bound on a message built from many of them. That framing is a bound on
// the rendered MESSAGE alone - what reaches stderr, and what a %w wrap
// re-materializes when this error crosses another layer - and not on what
// the process allocates to build the error in the first place; the two
// measured paragraphs below the table give each its own figure rather than
// one number standing in for both. The count is the gather's to pick, not
// this package's: whoever supplied the blob list chooses it - a requirements
// file's author, up to the load-time ceiling checkSignatureSources enforces,
// or a Galaxy server / a persisted snapshot, through serverSignatureBlobs.
// Measured for a single collection's whole gather - the verdict headline plus
// every one of MaxSignaturesPerCollection (64) non-ignored failures, one line
// per failure, as an uncapped render would - on the fixture that produces
// these three rows exactly: verdictReason under a required count of 1 with 0
// verified (the 93 B headline), and per cause a NO_PUBKEY failure whose Err
// is pgperrors.ErrUnknownIssuer, whose status and error text sum to the 50
// bytes all three rows imply. That sum is not unique to this pair: BADARMOR
// (8) plus a base64.CorruptInputError at an eight-digit offset - "illegal
// base64 data at input byte 10000000" (42 B) - sums to the identical 50, and
// classify routes it to StatusBadArmor too; ERRSIG (6) plus any 44-byte
// error text sums to 50 as well, through classify's own default arm, which
// accepts text this package never chooses at all. What actually generates
// the table below, and what a reader needs to re-derive it, is the
// arithmetic rather than a uniqueness claim: a cause renders as status +
// " from " + %q(origin) + ": " + err text, so per cause is 60 B plus the
// origin's rendered length - 50 B for this pair's status+text, 10 B for the
// fixed punctuation - which is exactly 179/591/2,127 B at 119/531/2,067
// rendered origin bytes. Among the six pairs whose text this package fixes
// rather than a library choosing it, the measured status+text sums are
// NODATA 44, NO_PUBKEY 50, REVKEYSIG 47, EXPKEYSIG 29, EXPSIG 32,
// BADARMOR/ArmorCorrupt 44; NO_PUBKEY is the only one of those six landing
// on 50, but BADSIG and ERRSIG both carry library-chosen text with no fixed
// sum at all, and BADARMOR carries a second, variable-offset shape - the
// base64 arm above - beside its fixed ArmorCorrupt one, so no uniqueness
// claim survives the whole eight-status vocabulary. The realistic row's href
// length is the galaxy.ansible.com v3 figure
// helpers.MessageValueMaxLen's own doc already records:
//
//	realistic (119 B href): 179 B per cause, 11,613 B at 64 causes
//	at the MessageValueMaxLen ceiling (531 B): 591 B per cause, 37,981 B at 64
//	that ceiling in control bytes, which %q escapes fourfold: 2,127 B per cause, 136,285 B at 64
//
// How many times a value at this scale is materialized, and where each
// materialization lands, is helpers.MessageValueMaxLen's own doc comment to
// state; that doc now names a third site the two paragraphs below measure.
//
// verificationError builds every one of those 64 causes with fmt.Errorf
// before this constant gets any say, and fmt.Errorf renders its string
// immediately rather than lazily, so a negative verdict pays for the whole
// uncapped table above the moment Verify returns it - whether or not anything
// ever calls Error(). That cost is the table's own figures minus the 64
// newline separators strings.Join would add across the table's 65 parts (the
// headline plus 64 causes), measured on the same three rows at 11,549 B,
// 37,917 B and 136,221 B. maxRenderedFailures does not bound any of it: the
// constant governs what Error() joins afterward, never what construction
// already allocated before Error() is ever reached.
//
// What Error() actually emits for that same fixture - the headline, the
// eight rendered causes, and the footer together - is smaller again, and this
// is the figure that reaches stderr and that a %w wrap re-materializes:
// 1,613 B, 4,909 B and 17,197 B on the same three rows. Each includes the
// footer's own hidden-status addition; Error()'s and hiddenStatuses' own doc
// comments hold the argument for what that addition is and why it stops
// short of recovering an origin.
//
// What the cap withholds is chosen by whoever supplied the blob list, not by
// this constant. The gather's own order decides which non-ignored failures
// occupy the render's first maxRenderedFailures slots, and a repeated line -
// a failure set whose causes share one origin and one status, which is what a
// server offering a long list of one bad signature produces - consumes a
// slot exactly like a distinct one costs. Measured on a fixture built for it:
// eight decoy sources, each declaring its own origin, each classifying
// NO_PUBKEY, ahead of a ninth failure classifying BADSIG, fill the whole
// render with the eight decoys and never reach the ninth - whatever remedy
// that ninth failure alone would have pointed an operator at.
//
// A renderer keyed on the distinct (status, origin) pair, in place of the
// gather's own order, was considered and rejected on that same fixture: the
// eight decoys carry eight DISTINCT pairs, one origin apiece, so deduping on
// the pair changes nothing about which lines fill the budget - it still
// renders the same eight NO_PUBKEY lines and still never reaches the BADSIG
// behind them. hiddenStatuses is the shape that actually answers what a
// pair-keyed renderer could not: rather than changing which causes occupy the
// render's slots, Error()'s own footer separately names the distinct statuses
// among what those slots excluded, so a status buried behind a wall of
// decoys still reaches the message even though the decoys' own lines still
// fill the budget. What it deliberately does not recover is the origin of a
// hidden failure: an origin is an unbounded, input-chosen value, and putting
// one back into the footer is exactly what this cap exists to keep out of a
// negative verdict's message.
const maxRenderedFailures = 8

// verdictError is the error a negative Verify verdict returns: a headline
// naming which clause of Policy.verdict fired, followed by every non-ignored
// Failure the walk recorded, bounded at render time by maxRenderedFailures.
//
// Every cause stays reachable through errors.Is and only the rendering is
// bounded, because a constant that governs how much is printed must not
// govern what an error matches. Dropping the unrendered causes would make
// errors.Is - and therefore exitcode.FromError, which is a pure function of
// the error tree - depend on a cause's position in a list this program does
// not own: a Galaxy server, a persisted snapshot and a requirements file all
// choose that order. Presentation would then be part of the error's
// identity, and changing a message-length constant would move an exit code.
//
// This is what separates it from the install side's own aggregate, which
// renders none of its causes: there, each cause was already printed to
// stderr in real time by the worker that hit it, while nothing prints
// Result.Failures - verifyCollectionSignatures drops the Result on the error
// path - so these causes are rendered because the message is the only place
// they appear.
//
// The construction invariant every constructor of this type has to hold, in
// full: causes is non-empty, carries no nil element, and its index 0 is the
// headline - never a per-signature failure. verificationError is the sole
// constructor and establishes all three. Nothing here checks any of them at
// construction or render time, so a second constructor that violates one
// breaks silently rather than loudly, and each violation's cost is its own
// shape: an empty causes renders Error() as the empty string, since shown is
// then 0 and strings.Join of no parts is ""; a nil element panics inside
// Error(), since the loop calls cause.Error() on it directly; and an index 0
// that is not the headline makes the footer's len(e.causes)-1 count one
// failure too FEW, since that arithmetic assumes the first slot is spent on
// something that is not itself a Failure. Measured against ten failure
// causes with no headline: Error() renders nine failure lines - one more
// than the footer's own claim - and a footer reading "showing the first 8
// of 9 signature failures; 1 not shown, carrying: BADSIG" against a true
// total of ten failures, while notShown itself stays correct at 1. The
// defect is quiet below ten causes: at nine or fewer with no headline,
// notShown is 0 and no footer fires at all, so nothing about the miscount
// is ever visible.
//
// hidden holds the distinct statuses, in first-seen order, that a failure
// past the render cap carries - see hiddenStatuses, verificationError's own
// producer of it. It is nil whenever nothing was hidden, and Error()'s footer
// branch already guards on that before reading it.
type verdictError struct {
	causes []error
	hidden []Status
}

// Error renders the headline and up to maxRenderedFailures of the failures
// behind it, one line per failure, joined by "\n". A strings.Contains
// assertion against this message still finds a cause the cap keeps rendered;
// one matching a cause the cap excludes - the (maxRenderedFailures+1)-th
// failure or later - is exactly what stops matching, since Unwrap still
// returns that cause but Error no longer renders it.
//
// shown reserves the headline's own slot: causes[0] is always the headline
// verificationError built, so maxRenderedFailures+1 is the render budget for
// "the headline plus that many failures", never "that many causes total".
//
// The footer, when it fires, names two things about what it withheld rather
// than only how many: the count of failures not shown, and - through
// e.hidden, when it is non-empty - the distinct statuses among them. That
// second part exists because a status is what an operator's ignore list and
// remedy are keyed on, and the render order is the gather's order rather than
// a ranking by severity: a wall of decoys sharing one tolerated status can
// fill every rendered slot and push the one failure carrying the strongest
// evidence - a BADSIG among eight NO_PUBKEY - past the cap with nothing in
// the visible lines saying so. Naming its status in the footer is what
// recovers that fact without recovering the failure's own origin, which stays
// out of the footer deliberately - see hiddenStatuses' own doc comment for
// why.
func (e *verdictError) Error() string {
	shown := min(len(e.causes), maxRenderedFailures+1) // the headline occupies the first slot
	parts := make([]string, 0, shown+1)
	for _, cause := range e.causes[:shown] {
		parts = append(parts, cause.Error())
	}
	if notShown := len(e.causes) - shown; notShown > 0 {
		footer := fmt.Sprintf("showing the first %d of %d signature failures", maxRenderedFailures, len(e.causes)-1)
		if len(e.hidden) > 0 {
			names := make([]string, len(e.hidden))
			for i, status := range e.hidden {
				names[i] = string(status)
			}
			footer += fmt.Sprintf("; %d not shown, carrying: %s", notShown, strings.Join(names, ", "))
		}
		parts = append(parts, footer)
	}

	return strings.Join(parts, "\n")
}

// Unwrap returns every cause, rendered or not, which is what lets errors.Is
// keep reaching a cause the message itself left out. No copy: the standard
// library's own errors.Join makes the identical choice on its *joinError -
// its Unwrap also returns its backing slice directly. What keeps that safe
// here is not a guarantee this type enforces against a caller it does not
// control - a caller holding the returned slice could still write through it
// - but a fact about this package's own callers instead: causes is built once
// by verificationError and nothing in this package retains a reference to it
// or writes through one afterward.
func (e *verdictError) Unwrap() []error {
	return e.causes
}

// hiddenStatuses returns the distinct statuses, in first-seen order, carried
// by the failures a negative verdict's own message does not render - those at
// and past index maxRenderedFailures, the same boundary Error()'s own shown
// computation draws. It returns nil whenever nothing is hidden, which is the
// common case and costs nothing: a collection whose gather produced at most
// maxRenderedFailures non-ignored failures is the ordinary run.
//
// The dedupe is what keeps the footer naming a shape rather than a count: a
// gather dominated by many failures of one status past the cap would
// otherwise repeat that status once per hidden failure, drowning a rarer one
// among them exactly as the render cap itself drowns a rarer FAILURE among
// many rendered ones. slices.Contains over at most eight distinct statuses is
// cheap enough that a map buys nothing here.
func hiddenStatuses(failures []Failure) []Status {
	if len(failures) <= maxRenderedFailures {
		return nil
	}
	var hidden []Status
	for _, failure := range failures[maxRenderedFailures:] {
		if !slices.Contains(hidden, failure.Status) {
			hidden = append(hidden, failure.Status)
		}
	}

	return hidden
}

// verificationError builds the failure a negative verdict returns: a headline
// naming which clause of Policy.verdict fired, with every non-ignored Failure
// joined behind it into a *verdictError - see that type's own doc comment for
// why the join it performs is what still lets errors.Is reach each underlying
// cause, and for the construction invariant this function alone is
// responsible for establishing.
//
// Each cause is rendered with its origin and status rather than passed through
// bare, because the library's own message names neither: "openpgp: invalid
// signature: EdDSA verification failure" says nothing about which source
// supplied the blob. The origin is quoted, since it is a value this package
// receives and never validates.
func verificationError(p Policy, verified int, failures []Failure) error {
	causes := make([]error, 0, len(failures)+1)
	causes = append(causes, fmt.Errorf("%w: %s", helpers.ErrSignatureVerificationFailed, verdictReason(p, verified)))
	for i := range failures {
		causes = append(causes, fmt.Errorf("%s from %q: %w", failures[i].Status, failures[i].Origin, failures[i].Err))
	}

	return &verdictError{causes: causes, hidden: hiddenStatuses(failures)}
}

// verdictReason names the clause of Policy.verdict that refused the run. Its
// cases are in that function's order for the reason stated there.
//
// The count clause is rendered two ways because one wording cannot serve both
// halves of it. A floor of zero is refused by the same equality every other
// count is, but "fewer valid signatures than required: got 1, need 0" describes
// nothing an operator can act on: one is not fewer than zero, and the run
// failed because a signature verified rather than because too few did. The
// verdict itself is ansible's and stays exactly as it is - see Policy.verdict
// for why that clause is an equality - so what changes here is only what the
// operator is told about it.
func verdictReason(p Policy, verified int) string {
	switch {
	case p.Required.Strict && verified == 0:
		return "no valid signature"
	case p.Required.All:
		return "some signatures failed"
	case p.Required.Count == 0:
		return fmt.Sprintf("a required count of 0 is satisfied only while nothing verifies; %d did", verified)
	default:
		return fmt.Sprintf("fewer valid signatures than required: got %d, need %d", verified, p.Required.Count)
	}
}

// checkOne verifies a single blob against the manifest, returning the entity
// whose key verified it and a nil error, or a nil entity and the reason it did
// not verify.
//
// The encoding is decided from the bytes, not from a source's name: a run that
// gathers signatures from several sources can hold both at once, so this is a
// per-blob decision rather than a per-run one. An armored blob is decoded here
// rather than by the library's armored entry point, because the packet framing
// gate below has to see the DECODED bytes - armor is base64 text and carries no
// framing to walk - and the library's armored entry point hands its decoded
// stream straight to the packet parser with nothing in between. The block type
// is checked exactly as that entry point checks it, so an armored object of
// another kind is still refused rather than fed to the signature reader.
//
// Every path reaches openpgp.CheckDetachedSignature and none reaches it without
// passing checkPacketFraming first. That ordering is the whole protection: the
// gate is what keeps a ten-byte blob from sizing a 4 GiB buffer inside the
// packet parser, and it is worth nothing if a later encoding branch routes
// around it.
func checkOne(manifest, blob []byte, kr *Keyring) (*openpgp.Entity, error) {
	// TrimSpace returns a subslice and allocates nothing. A source that
	// answered with a newline and nothing else is as empty as one that answered
	// with no bytes at all. This is the cheap half of the emptiness test; the
	// half that decides it sits below the envelope.
	if len(bytes.TrimSpace(blob)) == 0 {
		return nil, errNoSignatureData
	}

	packets := blob
	if bytes.Contains(blob, []byte(signatureArmorHeader)) {
		decoded, blockType, err := decodeArmorBlock(blob)
		if err != nil {
			return nil, err
		}
		if blockType != openpgp.SignatureType {
			return nil, pgperrors.InvalidArgumentError("expected '" + openpgp.SignatureType + "', got: " + blockType)
		}
		packets = decoded
	}

	// Emptiness is decided after the envelope rather than before it. A
	// well-formed but empty armor block - an opening line, a blank line and a
	// closing line, with or without a checksum line - survives the pre-check
	// above, decodes to zero bytes, and would otherwise reach the library as an
	// empty packet stream and come back as NO_PUBKEY: exactly the status
	// errNoSignatureData exists to keep an empty source out of, reachable by an
	// attacker choosing the shape.
	if len(packets) == 0 {
		return nil, errNoSignatureData
	}
	if err := checkPacketFraming(packets, signatureBlobProfile()); err != nil {
		return nil, err
	}

	return openpgp.CheckDetachedSignature(kr.entities, bytes.NewReader(manifest), bytes.NewReader(packets), nil)
}

// classify maps one verification failure onto the status vocabulary, which is
// what the ignore set is configured against.
//
// Every arm below names a condition go-crypto reports distinctly, and an error
// matching none of them becomes StatusErrSig - the vocabulary's own "this
// failed and has no more specific name". Falling back to a failure status
// rather than to a success is the property this function exists for: a
// go-crypto release that adds an error shape nobody here anticipated makes a
// signature unverifiable, never verified.
//
// The two armor arms are one condition with two shapes: a line the decoder
// refuses outright (armor.ArmorCorrupt, which is what a block whose line breaks
// were lost produces) and a body that is not valid base64. Both say the
// envelope did not decode, which is what BADARMOR names.
func classify(err error) Status {
	var corruptBase64 base64.CorruptInputError

	switch {
	case errors.Is(err, errNoSignatureData):
		return StatusNoData
	case errors.Is(err, pgperrors.ErrUnknownIssuer):
		return StatusNoPubKey
	case errors.Is(err, pgperrors.ErrKeyRevoked):
		return StatusRevKeySig
	case errors.Is(err, pgperrors.ErrKeyExpired):
		return StatusExpKeySig
	case errors.Is(err, pgperrors.ErrSignatureExpired):
		return StatusExpSig
	case errors.Is(err, armor.ArmorCorrupt), errors.As(err, &corruptBase64):
		return StatusBadArmor
	case isSignatureError(err):
		return StatusBadSig
	default:
		return StatusErrSig
	}
}

// isSignatureError reports whether err is go-crypto's own "a syntactically
// valid signature failed to validate", which is BADSIG.
//
// It is a type test rather than a value comparison because that error carries
// the algorithm that failed in its text, so there is no single value to compare
// against. It sits below the sentinel arms in classify: ErrUnknownIssuer and
// the two key-state errors are separate values rather than instances of this
// type, but ordering the sentinels first keeps that a property of classify
// rather than of what go-crypto happens to declare.
func isSignatureError(err error) bool {
	var sigErr pgperrors.SignatureError

	return errors.As(err, &sigErr)
}
