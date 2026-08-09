package signature

import (
	"fmt"
	"slices"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// Status is one gpg status code, spelled the way gpg's own status output and
// ansible's GPG_ERROR_MAP spell it. The sixteen constants below are that map's
// whole key set, frozen from ansible-core 2.21.2.
//
// A value outside the sixteen is refused, which is parity rather than
// strictness: such a value is an error to ansible's own argument parser too.
//
// Which of the sixteen a verdict can ever carry belongs to the vocabulary
// rather than to whatever classifies one, because the ignore set is configured
// against it: ignoring a code nothing can produce is a no-op that reads like
// protection. The three const groups below are that split, and it is
// exhaustive: eight produced, two that are only gpg's other name for a produced
// one, six inert. So a reader can tell from the group a code sits in whether it
// can appear in a verdict at all, and whatever classifies a verification
// failure is bound to this split rather than free to widen it.
type Status string

// The eight status codes a verdict from this tool can carry, one per class of
// OpenPGP verification failure it can conclude: a signature that does not
// verify against the manifest (BADSIG), one whose failure has no more specific
// name (ERRSIG), one made by a key the keyring does not hold (NO_PUBKEY), one
// made by an expired (EXPKEYSIG) or revoked (REVKEYSIG) key, one that has
// itself expired (EXPSIG), a blob carrying no OpenPGP data at all (NODATA), and
// one whose armor does not decode (BADARMOR).
const (
	StatusBadSig    Status = "BADSIG"
	StatusErrSig    Status = "ERRSIG"
	StatusNoPubKey  Status = "NO_PUBKEY"
	StatusExpKeySig Status = "EXPKEYSIG"
	StatusRevKeySig Status = "REVKEYSIG"
	StatusExpSig    Status = "EXPSIG"
	StatusNoData    Status = "NODATA"
	StatusBadArmor  Status = "BADARMOR"
)

// The two status codes that are gpg's second name for a condition the eight
// above already name: KEYEXPIRED for EXPKEYSIG, KEYREVOKED for REVKEYSIG. No
// verdict carries one, and configuring one still works, because statusSynonyms
// makes either spelling cover both.
const (
	StatusKeyExpired Status = "KEYEXPIRED"
	StatusKeyRevoked Status = "KEYREVOKED"
)

// The six status codes accepted and inert. Three of them name secret key
// material and the business of unlocking it, which a verifier never has:
// LoadKeyring refuses a keyring carrying any. The other three are a gpg process
// reporting on itself, and this tool runs none - it verifies in pure Go.
//
// They are accepted so that an ansible-shaped CI environment block naming one
// parses rather than failing the run over a value that would change nothing
// either way. Refusing them instead would turn an operator's existing,
// harmless configuration into a usage error for asking to tolerate a failure
// that cannot occur.
const (
	StatusMissingPassphrase Status = "MISSING_PASSPHRASE"
	StatusBadPassphrase     Status = "BAD_PASSPHRASE"
	StatusNoSecKey          Status = "NO_SECKEY"
	StatusUnexpected        Status = "UNEXPECTED"
	StatusError             Status = "ERROR"
	StatusFailure           Status = "FAILURE"
)

// statusCodes is the vocabulary itself: every Status an operator may configure,
// in the order the groups above declare them, which is also the order a refusal
// lists them in. It is the single membership table, so a code declared as a
// constant and left out here is refused rather than silently half-supported.
//
//nolint:gochecknoglobals // a fixed, immutable vocabulary, not mutable shared state.
var statusCodes = []Status{
	StatusBadSig,
	StatusErrSig,
	StatusNoPubKey,
	StatusExpKeySig,
	StatusRevKeySig,
	StatusExpSig,
	StatusNoData,
	StatusBadArmor,
	StatusKeyExpired,
	StatusKeyRevoked,
	StatusMissingPassphrase,
	StatusBadPassphrase,
	StatusNoSecKey,
	StatusUnexpected,
	StatusError,
	StatusFailure,
}

// statusSynonyms maps each status code gpg spells two ways to its other
// spelling, in both directions.
//
// This table is the reason Ignores is not a plain set lookup. gpg emits both
// names for one underlying condition - KEYEXPIRED and EXPKEYSIG are the same
// expired signing key, KEYREVOKED and REVKEYSIG the same revoked one - while
// this tool reports one code per condition. So an operator who configured
// "ignore EXPKEYSIG" against a membership test alone would get nothing whenever
// the verdict carried KEYEXPIRED instead, and would get it silently: an ignore
// that covers one name of a condition and not the other ignores nothing, since
// the run keeps failing on exactly the failure the operator said to tolerate.
// An ignore works only if every name for the condition is ignorable.
//
// Both directions are written out rather than derived from a list of pairs, so
// that a lookup is one step and the symmetry is visible where the table is
// declared instead of being asserted somewhere else.
//
//nolint:gochecknoglobals // a fixed, immutable lookup table, not mutable shared state.
var statusSynonyms = map[Status]Status{
	StatusKeyExpired: StatusExpKeySig,
	StatusExpKeySig:  StatusKeyExpired,
	StatusKeyRevoked: StatusRevKeySig,
	StatusRevKeySig:  StatusKeyRevoked,
}

// StatusSet is a set of status codes an operator configured this run to
// tolerate. Its zero value is usable and tolerates nothing, which is what a run
// that configured no ignore list carries.
type StatusSet map[Status]struct{}

// Ignores reports whether s tolerates code, consulting statusSynonyms so that
// configuring either spelling of a condition covers both. See that table for
// why a membership test on its own would silently under-deliver.
func (s StatusSet) Ignores(code Status) bool {
	if _, ok := s[code]; ok {
		return true
	}

	synonym, ok := statusSynonyms[code]
	if !ok {
		return false
	}
	_, ok = s[synonym]

	return ok
}

// ParseStatusCodes turns the status code names an operator configured into a
// StatusSet. Each value is trimmed and upper-cased, so the whitespace a
// comma-separated list leaves around an element, and the lower-case spelling
// gpg's own documentation uses in prose, are absorbed rather than refused.
//
// A value outside the vocabulary is refused with
// helpers.ErrUnknownSignatureStatusCode naming it and listing what is accepted,
// and that refusal is the whole point of the sentinel: a code nothing
// recognizes would ignore nothing, so swallowing a typo would read to the
// operator as "this failure is being tolerated" while the run kept failing on
// exactly that failure. The accepted list is part of the message because the
// operator who mistyped one needs to see the alternatives, and a single value
// this small has no second place to look them up.
//
// An element that is empty once trimmed is refused the same way rather than
// skipped, for that same reason: whoever composed the list meant something by
// it. A repeated code is not a refusal - a set holds one of each, and asking
// twice asks for what asking once did.
func ParseStatusCodes(values []string) (StatusSet, error) {
	set := make(StatusSet, len(values))
	for _, value := range values {
		code := Status(strings.ToUpper(strings.TrimSpace(value)))
		if !slices.Contains(statusCodes, code) {
			return nil, fmt.Errorf("%w: %q; accepted codes are %s",
				helpers.ErrUnknownSignatureStatusCode, value, statusCodeList())
		}
		set[code] = struct{}{}
	}

	return set, nil
}

// statusCodeList renders the vocabulary for a refusal message. It allocates per
// call rather than being precomputed, because the only caller is a refusal path
// that ends the run.
func statusCodeList() string {
	names := make([]string, len(statusCodes))
	for i, code := range statusCodes {
		names[i] = string(code)
	}

	return strings.Join(names, ", ")
}
