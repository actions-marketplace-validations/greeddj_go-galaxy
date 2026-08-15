package signature

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"

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
	// reported back verbatim. Fetcher.FetchRequirementSource fills it with the
	// source it was handed under the cuts helpers makes - the query string
	// (helpers.WithoutQuery), the userinfo (helpers.WithoutUserinfo) and the
	// fragment (helpers.WithoutFragment) - so neither a presigned source's
	// capability nor an embedded credential rides into a message this value is
	// rendered into, and a file source names the path that was opened.
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
// worker. That is the contract this function is written to; it is not a claim
// that a parallel run has been observed under the race detector.
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
// each inflating the count. go-crypto returns the verifying entity on success,
// so this is a fail-safe direction for a shape it does not produce, not a case
// with a fixture behind it.
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

// verificationError builds the failure a negative verdict returns: a headline
// naming which clause of Policy.verdict fired, with every non-ignored Failure
// joined behind it so errors.Is still reaches each underlying cause.
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

	return errors.Join(causes...)
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
	if err := checkPacketFraming(packets); err != nil {
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
