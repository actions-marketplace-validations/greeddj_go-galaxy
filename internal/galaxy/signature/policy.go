package signature

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

const (
	// strictPrefix is the leading marker that makes a count spec strict. It is
	// a marker rather than a sign: the number it precedes is still a plain
	// non-negative count.
	strictPrefix = "+"

	// allSpelling is the one non-numeric count spec, and it is lower case
	// because that is the literal ansible's own grammar accepts.
	allSpelling = "all"

	// negativeAllSpelling is the value ansible's install-parser help text
	// claims means every signature. It is refused by name so that the operator
	// who read that help gets told what to write instead; see ParseCountSpec
	// for why reproducing the claim was not an option.
	negativeAllSpelling = "-1"
)

// CountSpec is a parsed required-valid-signature-count policy: how many
// signatures must verify, and whether verifying nothing at all is tolerated on
// top of that count.
//
// The two halves are independent, and what each spelling means binds whoever
// writes the decision function:
//
//   - bare N requires N valid signatures. A signature that failed is tolerated
//     as long as N others verified.
//   - +N requires N valid signatures AND that at least one signature verified.
//     It does NOT make a checked-and-failed signature fatal - "all" is the
//     spelling for that - so +N with two failures beside one success passes
//     whenever N is one. The leading + is the strict marker, never part of the
//     number.
//   - all, with or without the +, requires every signature checked to verify.
//     Count carries no floor there, and ParseCountSpec never sets All and a
//     non-zero Count together.
//   - Count 0 is a floor of zero, so an empty signature list satisfies it. It
//     is an operator asking for no floor at all, and it is the only spelling
//     under which the rule below has nothing to refuse.
//
// The strict marker contributes exactly one clause, and making it contribute
// the stronger one instead is not a local edit: a counted policy stops
// checking at its Count-th success, so a signature sitting after that point is
// never examined and could not be found to have failed. "Any checked signature
// that failed is fatal" would therefore mean something different depending on
// where in the list the failure sat, unless the early stop went too - which
// would also cost every run the public-key operations that stop exists to
// avoid.
//
// The trap this doc exists for: a non-strict spelling is satisfied VACUOUSLY
// when nothing was gathered. Bare N passes on zero signatures, and so does all,
// because neither asks for a floor that an empty set can fail - only the strict
// marker requires at least one signature to have been checked. So a hostile
// server or a poisoned snapshot that serves an empty signature list satisfies
// the default policy while nothing was verified at all.
//
// That verdict is deliberate and must not be "improved" here: it is what
// ansible-galaxy does, an operator who needs it closed closes it with +, and a
// tool that silently exits differently from the one it stands in for is worse
// than one that shares a documented weakness. What is NOT inherited is the
// silence. A run with verification enabled that gathers nothing for a
// collection has to say so on stderr, naming the collection and the spelling
// that would have required a signature, so a vacuous pass is visible in a log
// rather than indistinguishable from a real one. The verdict is parity; the
// silence is not.
//
// Separately, and with no such tension: a source that could not be fetched must
// never fold into "there were no signatures". That is
// helpers.ErrSignatureSourceUnavailable, which says the material was never in
// hand - a run that cannot reach a signature has learned nothing about the
// collection, which is a different fact from a policy having been met.
type CountSpec struct {
	// Count is the number of signatures that must verify. It is meaningful
	// only when All is false.
	Count int
	// All requires every signature checked to verify, in place of a count.
	All bool
	// Strict requires that at least one signature verified, on top of
	// whichever of Count or All applies. It is what closes the vacuous pass
	// described above, and it is not a rule about failures - see this type's
	// own doc comment for why the stronger reading is "all" rather than this.
	Strict bool
}

// ParseCountSpec parses the required-valid-signature-count value: an optional
// leading strictPrefix, then either allSpelling or a non-negative decimal
// count. The grammar is ansible's own, frozen from ansible-core 2.21.2, and it
// is matched with strings rather than a regexp because a two-token grammar does
// not need one and this path carries no regexp dependency.
//
// The whole value must match, whitespace included: " 1" and "+ 1" are refused
// rather than trimmed into shape, which is what ansible's own anchored pattern
// does. "ALL" is refused for the same reason - the token is a literal of that
// grammar - and accepting a spelling ansible refuses would let a value work
// here and fail under ansible-galaxy, which is the one direction of divergence
// that costs an operator something.
//
// negativeAllSpelling is refused by name. ansible's install-parser help text
// claims it means every signature while ansible's own grammar accepts no
// negative number at all, so the value reaches this parser from operators who
// read that help; reproducing the claim would mean inventing a spelling with no
// grammar behind it, and refusing it generically would leave those operators
// with a message that never names the spelling they wanted.
//
// Every refusal wraps helpers.ErrInvalidSignatureCount and names the offending
// value.
func ParseCountSpec(value string) (CountSpec, error) {
	if value == negativeAllSpelling {
		return CountSpec{}, fmt.Errorf("%w: %q does not mean every signature; write %q instead",
			helpers.ErrInvalidSignatureCount, value, allSpelling)
	}

	var spec CountSpec

	rest := value
	if after, found := strings.CutPrefix(rest, strictPrefix); found {
		spec.Strict = true
		rest = after
	}

	if rest == allSpelling {
		spec.All = true

		return spec, nil
	}

	if !isDecimalDigits(rest) {
		return CountSpec{}, invalidCountSpec(value)
	}

	count, err := strconv.Atoi(rest)
	if err != nil {
		// The only way a string of nothing but digits fails to convert is a
		// range error, so the message names the size rather than the shape.
		return CountSpec{}, fmt.Errorf("%w: %q is larger than a signature count can be", helpers.ErrInvalidSignatureCount, value)
	}
	spec.Count = count

	return spec, nil
}

// invalidCountSpec is the refusal for a value that does not match the grammar
// at all. It spells out what would have been accepted, in the literals
// themselves, because the whole value is a handful of characters and the
// message is the only place an operator learns which ones they were.
func invalidCountSpec(value string) error {
	return fmt.Errorf("%w: %q; write a non-negative count or %q, optionally prefixed with %q for strict",
		helpers.ErrInvalidSignatureCount, value, allSpelling, strictPrefix)
}

// isDecimalDigits reports whether s is one or more ASCII decimal digits.
//
// The loop is over bytes rather than runes deliberately: the grammar is ASCII
// decimal, and a rune loop paired with unicode.IsDigit would accept a
// non-ASCII digit that strconv.Atoi then refuses, turning a grammar refusal
// into a conversion failure with a worse message.
func isDecimalDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}

	return true
}

// Policy is one run's signature verification policy: where the key material
// lives, how many signatures have to verify, and which failure statuses are
// tolerated.
//
// It holds no key material and no file handle, and nothing here mutates one
// after NewPolicy returns it, so a single Policy can be read from every worker
// without synchronization.
type Policy struct {
	// Ignore is the set of statuses a failed signature may carry without
	// counting as a failure.
	Ignore StatusSet
	// KeyringPath is the keyring location exactly as this package received it;
	// nothing here expands or resolves it. See NewPolicy for why that belongs
	// to the caller.
	KeyringPath string
	// Required is the parsed required-valid-signature-count spec.
	Required CountSpec
	// Disabled records that verification was switched off outright, which is
	// separate from no keyring having been configured.
	Disabled bool
}

// NewPolicy assembles a Policy from the raw configured values.
//
// It takes the keyring PATH rather than a loaded *Keyring, and that is what
// lets Enabled be answered before any file is read: a run that verifies nothing
// must not pay for a keyring read to discover it, and the key material is
// passed to the one operation that needs it, so a policy carrying a second copy
// could only disagree with that one.
//
// It expands nothing and touches no filesystem: the path is stored exactly as
// configured. Resolving "~" or a relative path belongs to the config layer, and
// doing it here would give this package a second reason to touch the disk
// beyond the keyring read its package doc scopes it to.
//
// A parse failure is returned as it came. It already wraps its own sentinel and
// names the offending value, so wrapping it again would only repeat it.
func NewPolicy(keyringPath, requiredCount string, ignoreCodes []string, disabled bool) (Policy, error) {
	required, err := ParseCountSpec(requiredCount)
	if err != nil {
		return Policy{}, err
	}

	ignore, err := ParseStatusCodes(ignoreCodes)
	if err != nil {
		return Policy{}, err
	}

	return Policy{
		KeyringPath: keyringPath,
		Ignore:      ignore,
		Required:    required,
		Disabled:    disabled,
	}, nil
}

// Enabled reports whether this run verifies signatures at all: key material
// must be configured and verification must not be switched off.
//
// The two inputs stay separate rather than collapsing into one because they
// arrive from different places and say different things - an absent keyring is
// a configuration that never asked for verification, while Disabled is an
// operator switching off a configuration that did.
func (p Policy) Enabled() bool {
	return VerificationEnabled(p.KeyringPath, p.Disabled)
}

// VerificationEnabled answers Enabled's question over the raw configured
// values, before any Policy has been built.
//
// It exists so that a caller who has to decide whether to build a policy at all
// - internal/galaxy/collections' newVerifyContext, which must not turn a run
// that verifies nothing into a refusal over a count spec nothing will read -
// asks this package rather than copying the predicate. A copy would be a second
// place to update, and a term added here later would leave it disagreeing in
// the direction that silently switches verification OFF.
//
// Both spellings stay: Enabled is what a holder of a Policy asks, this is what
// a holder of the two values asks, and the first is written in terms of the
// second so the two can never answer differently.
func VerificationEnabled(keyringPath string, disabled bool) bool {
	return keyringPath != "" && !disabled
}
