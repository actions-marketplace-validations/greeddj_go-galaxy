package signature

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/ProtonMail/go-crypto/openpgp"
	pgperrors "github.com/ProtonMail/go-crypto/openpgp/errors"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

const (
	// keyringBlobEquivalents is how many signature blobs' worth of bytes a
	// keyring file may occupy. helpers.SignatureMaxSize is sized for a single
	// detached signature of a few hundred bytes, and a keyring is a different
	// kind of object: one public key, its subkeys, its user ids and every
	// certification signature over them, repeated once per key an operator
	// trusts. The multiple is what turns a per-blob ceiling into one an
	// ordinary multi-key keyring fits inside with room to spare while still
	// bounding what a hostile or broken file can buffer into memory. It is
	// deliberately not tied to helpers.MaxSignaturesPerCollection, which
	// happens to carry the same number and counts something else entirely.
	keyringBlobEquivalents = 64

	// keyringMaxSize caps the bytes LoadKeyring reads from a keyring file.
	//
	// Crossing it is a refusal rather than a truncation, and that is the whole
	// reason the ceiling is checked instead of merely applied: a keyring read
	// short still parses, yielding a keyring silently missing exactly the keys
	// past the cut, and the run would then report a correctly signed artifact
	// as one nobody it trusts had signed. helpers.ErrKeyringUnreadable naming
	// the file is the answer an operator can act on.
	//
	// That argument is only worth making because the ceiling is the sole place
	// a configured key can drop out of the trust set: readArmoredKeyRing reads
	// every armor block in the file rather than the first, so no key goes
	// missing merely for being written after another one. What keeps that a
	// property of the file rather than of how it was assembled is
	// errArmorBlockStartMidLine, which turns the one concatenation the block cut
	// cannot see into a refusal instead of a silent short read.
	//
	// helpers.NewSizeLimitedReader is the module's own reader for this shape
	// and is deliberately not used here: it raises helpers.ErrResponseTooLarge,
	// which cmd/go-galaxy/exitcode classifies as a transport failure, so a
	// keyring too large to read would exit as a network error rather than as
	// the configuration error it is.
	keyringMaxSize = helpers.SignatureMaxSize * keyringBlobEquivalents

	// keyboxMagicOffset is where a GnuPG keybox carries keyboxMagic: a keybox
	// opens with a header blob whose first eight bytes are a 4-byte blob
	// length, a 1-byte blob type, a 1-byte version and 2 bytes of flags.
	keyboxMagicOffset = 8

	// keyboxMagic is the four-byte magic identifying a GnuPG keybox container.
	keyboxMagic = "KBXf"

	// publicKeyArmorHeader opens an ASCII-armored OpenPGP public key block,
	// which is what `gpg --export --armor` writes.
	publicKeyArmorHeader = "-----BEGIN PGP PUBLIC KEY BLOCK-----"

	// privateKeyArmorHeader opens an ASCII-armored OpenPGP private key block.
	// It is a routing needle, never an accepted shape: a file carrying one is
	// read as armor so that the secret-material refusal is what it earns, rather
	// than the binary reader failing on base64 text and reporting a malformed
	// packet an operator cannot act on. What produces that refusal is the packet
	// walk's own secret-key arm, on the block's decoded bytes.
	//
	// #nosec G101 -- this is the public delimiter line that introduces such a
	// block, not key material: it is the needle that gets a file holding one
	// refused, and no secret this program has read or could read.
	privateKeyArmorHeader = "-----BEGIN PGP PRIVATE KEY BLOCK-----"

	// armorBlockStart is the prefix every armored OpenPGP block's opening line
	// carries, whatever the block's type.
	armorBlockStart = "-----BEGIN "

	// armorBlockStartMinLen mirrors the length test armor.Decode applies to a
	// candidate opening line: the prefix, the trailing "-----", and at least
	// one byte of type between them.
	armorBlockStartMinLen = len(armorBlockStart) + len("-----") + 1

	// armorHeaderMaxSize bounds one armor block's header section: the bytes
	// between its opening line and the blank line that ends the section.
	//
	// The bound is on the section rather than on any one line in it, because the
	// cost it exists to remove is quadratic in the section's length. armor.Decode
	// reads its input through a bufio.Reader of 100 bytes and accumulates a
	// header line longer than one buffer a chunk at a time, appending each chunk
	// to the value it has built so far, so an N-byte header line is copied N/100
	// times and the header loop spends about N*N/200 bytes of allocation.
	//
	// What the bound pins is one execution of that header loop, which is the unit
	// the decoder restarts rather than the input's own total: a header line
	// carrying no colon sends it looking for the next opening line, and the loop
	// starts again on whatever it finds. One section at exactly the bound measures
	// 102960 bytes under a one-character header name and 102560 under a
	// seven-character one, the spread being size-class rounding on the string
	// being grown, against 83886 from the N*N/200 model. Across an input the term
	// therefore stays linear, measured flat at 23.9 times the input's own length
	// on blobs of 16, 64 and 259 such sections.
	//
	// The residual that leaves is on the blob path, and it is bounded rather than
	// closed: a blob filling helpers.SignatureMaxSize with those sections spends
	// about 25 MB, and walkBlobs is a sequential loop bounded by
	// helpers.MaxSignaturesPerCollection, so about 1.6 GB per collection, spent
	// one blob at a time rather than held at once, against 5.5 GB for a single
	// ungated blob. The keyring path carries no such residual, and owes that to
	// the shape of its own reader rather than to this bound: measured at 2.2 times
	// the file's length on a 1047396-byte file of those same sections, which is
	// essentially the io.ReadAll, because readArmoredKeyRing cuts a block at every
	// opening line and readKeyArmorBlock refuses the first cut that is not key
	// material, so a multi-section file pays for one section.
	//
	// What that spends is CPU and allocator churn rather than peak memory - every
	// copy is garbage the moment the next one is made - so the ceiling is chosen
	// to be far out of the way of real armor rather than tight against it: 4096
	// holds more Version, Comment and Charset lines than any producer writes,
	// while gpg writes an opening line and a blank line with nothing between them
	// at all.
	//
	// A block's body needs no bound of its own here, which is what makes this
	// cheap: armor.Decode's own lineReader already refuses a body line it could
	// not read whole and any body line over 96 bytes, so the part of an armored
	// blob that carries real length is already capped by the library, and this
	// bound costs it nothing.
	armorHeaderMaxSize = 4096
)

// errArmorBlockStartMidLine is the verdict for a keyring file carrying an armor
// block's opening prefix somewhere other than the start of a line.
//
// The shape it exists for is one concatenation produces, and it is measured
// rather than argued: a block armor.Encode writes ends on the final "-" of its
// end line, with no terminator behind it, so `cat a.asc b.asc` puts b's opening
// line on the end of a's last line. Three such blocks concatenated present ONE
// opening line to the cut; measured, they load as 1 entity with a nil error
// where the same three blocks separated by newlines load as 3, and 5000 of them
// load as 1 and drop 4999 in the same silence. A file gpg wrote is not this
// shape, since `gpg --export --armor` ends its output with a newline, which is
// what keeps the committed two-keys.asc loading as two keys.
// TestGluedArmorBlocksAreRefused is where all of that is a measurement rather
// than a claim.
//
// It is refused rather than cut apart at the second start, and that direction
// is the point. Teaching the cut to find an opening line mid-line would widen
// what counts as a block to a shape neither armor.Encode nor gpg writes, and it
// is the direction that can merge two blocks rather than turn the file away;
// this one costs a single scan over bytes the size ceiling has already put in
// memory, and it names the remedy in the message.
//
// It is an acceptance change on the keyring path, and disclosed as one: a file
// carrying that sequence anywhere but at the start of a line stops loading,
// where before it loaded and silently dropped every key behind the first block.
// What it costs is availability on a file the loader could not have read
// correctly anyway.
//
// The blob path is left as it is, and the asymmetry is a property of what each
// kind of file is for rather than an oversight: checkOne decodes the first armor
// block of a signature blob and reads nothing behind it, so a glued blob
// verifies its first signature and the remaining bytes reach neither the gate
// nor the verifier - they are granted nothing, and one blob per source is the
// model that path is built on. A keyring is the opposite: every block in it is
// material the operator meant this run to trust, so a block the cut cannot see
// is a configured key the run silently does not hold. Hence the keyring path
// refuses what the blob path ignores.
var errArmorBlockStartMidLine = errors.New("an armored block start does not begin its line")

// Keyring is the OpenPGP key material of one keyring file, together with the
// path it was read from. Nothing in this package mutates one after LoadKeyring
// returns it.
type Keyring struct {
	path     string
	entities openpgp.EntityList
}

// Path returns the file this keyring was read from. It is the value to name in
// an operator-facing message about the keyring, so a run that verified against
// the wrong file still says which file that was.
func (k *Keyring) Path() string {
	return k.path
}

// Len returns how many OpenPGP entities the keyring holds. It is never zero
// for a *Keyring LoadKeyring returned, which is what lets a caller treat a
// loaded keyring as usable without re-checking that it holds anything.
func (k *Keyring) Len() int {
	return len(k.entities)
}

// LoadKeyring reads path as an OpenPGP keyring.
//
// The format is decided from the bytes rather than from the file name: an
// operator names a keyring by path and nothing constrains its extension, so a
// name is evidence of nothing. Three shapes are distinguished, in this order:
// a GnuPG keybox, refused with helpers.ErrKeyringIsKeybox; armored key blocks,
// read with readArmoredKeyRing; and anything else, read as a binary packet
// stream with openpgp.ReadKeyRing. The keybox check comes first because a
// keybox is neither of the other two and would otherwise be reported through
// the generic sentinel, costing an operator the one remedy worth naming - a
// default GnuPG installation writes that container, so it is the file an
// operator is most likely to point at.
//
// An armored file is read whole: every block in it must be key material and
// every one of them contributes its keys, so the
// `cat teamA.asc teamB.asc > keyring.asc` an operator builds a multi-key
// keyring with holds both teams rather than the first. That concatenation has
// one requirement, and a file failing it is refused with the remedy named
// rather than read short: each export must end with a newline, so that the next
// export's opening line begins a line of its own. errArmorBlockStartMidLine
// holds why a file where one does not is refused rather than cut apart anyway.
//
// Only public key material is accepted. A file holding a secret key is refused
// with helpers.ErrKeyringUnreadable naming the export that would have been
// right, in both encodings alike, so the refusal is this tool's policy rather
// than a side effect of which reader the bytes happened to reach.
//
// Every other failure is helpers.ErrKeyringUnreadable naming path: the file
// could not be opened, it is larger than this tool reads, its bytes did not
// parse, or they parsed to no entities at all. That last shape is not a
// failure to the library - openpgp.ReadKeyRing reports an empty packet stream
// as an empty list and a nil error - so it is refused here rather than handed
// back, since an empty keyring verifies nothing while reporting nothing wrong.
func LoadKeyring(path string) (*Keyring, error) {
	return loadKeyring(path, keyringMaxSize)
}

// loadKeyring is LoadKeyring with the size ceiling as a parameter, so that
// crossing it can be exercised without a fixture the size of the real one.
func loadKeyring(path string, limit int64) (*Keyring, error) {
	// #nosec G304 -- path is the keyring location the operator configured;
	// reading the file they named is the entire operation.
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %q: %w", helpers.ErrKeyringUnreadable, path, err)
	}
	defer func() { _ = f.Close() }()

	// Reading limit+1 is what makes the ceiling exact rather than off by one:
	// stopping at limit leaves a file of exactly limit bytes indistinguishable
	// from a longer one truncated there, so it would have to be refused too.
	data, err := io.ReadAll(&io.LimitedReader{R: f, N: limit + 1})
	if err != nil {
		return nil, fmt.Errorf("%w: %q: %w", helpers.ErrKeyringUnreadable, path, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%w: %q is larger than the %d bytes this tool reads", helpers.ErrKeyringUnreadable, path, limit)
	}

	if looksLikeKeybox(data) {
		return nil, fmt.Errorf("%w: %q", helpers.ErrKeyringIsKeybox, path)
	}

	entities, err := readEntities(data)
	if err != nil {
		if errors.Is(err, errSecretKeyPacket) {
			return nil, secretKeyMaterialError(path)
		}

		return nil, fmt.Errorf("%w: %q: %w", helpers.ErrKeyringUnreadable, path, err)
	}
	if len(entities) == 0 {
		return nil, fmt.Errorf("%w: %q holds no OpenPGP keys", helpers.ErrKeyringUnreadable, path)
	}
	if holdsSecretKey(entities) {
		// Unreachable from any file, and deliberately kept: readEntities passes
		// the walk that refuses the two secret-key tags before openpgp.ReadKeyRing
		// sees them, so no entity built here can carry private material. What is
		// uncovered is this branch rather than the predicate -
		// TestHoldsSecretKeyReportsMaterialInEitherPlace drives that directly -
		// and what its removal would cost is a reader that reached the library
		// without the walk in front of it having no refusal at all.
		return nil, secretKeyMaterialError(path)
	}

	return &Keyring{path: path, entities: entities}, nil
}

// secretKeyMaterialError is the one refusal a keyring holding secret material
// earns, wherever that material was noticed.
//
// The packet walk refuses the two secret-key tags before anything parses them,
// and holdsSecretKey stands behind it over entities that did parse; rendering
// both through one function is what keeps the operator-facing text and the exit
// class a property of the policy rather than of which check happened to run.
// Naming the export to make instead is the whole of what the message is for: the
// file is one an operator did not mean to point a build at, and the remedy is
// one command.
//
// errSecretKeyPacket is rendered into the sentence rather than named beside it,
// which is what keeps this refusal reachable through errors.Is whichever of the
// two checks produced it while leaving the operator-facing text one sentence.
func secretKeyMaterialError(path string) error {
	return fmt.Errorf(
		"%w: %q holds %w; verifying needs only the public half, "+
			"so export that instead: gpg --export --armor > keyring.asc",
		helpers.ErrKeyringUnreadable, path, errSecretKeyPacket)
}

// readEntities reads data as key material, choosing the encoding from the bytes
// and putting the packet framing gate in front of the parser in either case.
//
// bytes.Contains rather than a prefix test: openpgp's own armor.Decode scans
// lines for an armor header and ignores whatever precedes it, so a file
// carrying one after a leading comment is a file the armored reader can read,
// and a prefix test would route it to the binary reader and refuse a keyring
// that is fine. The converse misroute - a binary keyring whose bytes happen to
// contain one of these exact header lines, sent to the armored reader and
// refused - is accepted rather than closed: it takes a deliberately built file
// to hit, while a leading comment line is something ordinary tooling writes.
//
// The needle is the two block types openpgp.ReadArmoredKeyRing itself accepts,
// never the bare armorBlockStart prefix, so this test is exactly as selective
// as the reader behind it: a PEM certificate or any other armored object routes
// to the binary reader and is refused there.
//
// The binary branch is already decoded, so it is gated here; the armored branch
// gates each block's decoded bytes inside readKeyArmorBlock, where the decode
// happens. Between them no path reaches go-crypto's packet parser ungated - see
// checkPacketFraming for what that buys and why a keyring needs it too.
func readEntities(data []byte) (openpgp.EntityList, error) {
	if bytes.Contains(data, []byte(publicKeyArmorHeader)) || bytes.Contains(data, []byte(privateKeyArmorHeader)) {
		return readArmoredKeyRing(data)
	}
	if err := checkPacketFraming(data, keyringProfile()); err != nil {
		return nil, err
	}

	return openpgp.ReadKeyRing(bytes.NewReader(data))
}

// readArmoredKeyRing reads the entities of every armor block in data, in file
// order, rather than of the first one alone.
//
// openpgp.ReadArmoredKeyRing calls armor.Decode exactly once, and one call
// reads one block, so handing it a whole `cat teamA.asc teamB.asc` file yields
// team A's keys and a nil error - a trust set silently smaller than the one
// configured, which is the failure keyringMaxSize's own refusal exists to
// avoid. Concatenation is how an operator builds a multi-key keyring, so the
// answer is to read all of it rather than to refuse the shape.
//
// The blocks are cut apart here rather than by calling armor.Decode in a loop
// because armor.Decode documents its input as unusable afterwards: it wraps
// the reader in a bufio.Reader of its own and may consume an arbitrary amount
// past the block's end line, so a second call resumes from wherever that
// buffer happened to stop. Cutting the bytes is available precisely because
// the whole file is already in memory, which the size ceiling above
// guarantees.
//
// A block that is not key material is a refusal, not something skipped:
// readKeyArmorBlock names the type it found, and skipping would put the loader
// back in the business of using part of a file quietly. The cut itself errs the
// same way. isArmorBlockStart finds an opening line only at the start of a
// line, which is where armor.Encode and gpg both write one; it can additionally
// start a block at a line the decoder would have skipped as garbage, and the
// worst that costs is a slice holding nothing the decoder recognizes, which is
// refused.
//
// What guarantees that no two blocks are ever merged into one that quietly
// drops a key is checkArmorBlockStartsAtLineStart rather than that recognizer:
// a file carrying the opening prefix anywhere but at the start of a line does
// not load at all. Refusing that shape is what lets the cut stay a line-start
// test - errArmorBlockStartMidLine holds the measurement and the argument.
//
// The packet ceiling is carried across the blocks rather than applied to each,
// which is the one thing reading a file block by block would otherwise give
// away: the ceiling is stated over a keyring file, and a per-block count bounds
// nothing a file cannot buy more of by carrying another block. spent is that
// carry, and walkPacketFraming's own doc comment holds what a file measured
// without it did.
func readArmoredKeyRing(data []byte) (openpgp.EntityList, error) {
	if err := checkArmorBlockStartsAtLineStart(data); err != nil {
		return nil, err
	}

	var entities openpgp.EntityList

	// blockStart is where the block currently being accumulated begins, or -1
	// before the first opening line has been seen; spent is how much of this
	// file's own ceiling the blocks already read used, their own
	// armorBlockPacketCost included.
	blockStart := -1
	spent := 0
	readBlock := func(end int) error {
		if blockStart < 0 {
			return nil
		}
		block, packets, err := readKeyArmorBlock(data[blockStart:end], spent)
		if err != nil {
			return err
		}
		spent += packets
		entities = append(entities, block...)

		return nil
	}

	for off := 0; off < len(data); {
		line, next := nextLine(data, off)
		if isArmorBlockStart(line) {
			// The previous block ends where this one starts. Whatever sits
			// between its end line and here the decoder never reads, since it
			// stops at that end line.
			if err := readBlock(off); err != nil {
				return nil, err
			}
			blockStart = off
		}
		off = next
	}

	if err := readBlock(len(data)); err != nil {
		return nil, err
	}

	return entities, nil
}

// readKeyArmorBlock decodes one armored block and reads the key material out of
// it, with the packet framing gate standing between the two steps, and reports
// what the block cost against the file's packet budget.
//
// spent is how much of the ceiling the file's earlier blocks already used, and
// what this block cost comes back so the caller can keep that sum: the ceiling
// belongs to the file, and this function sees one block of it.
//
// The block itself costs one packet of that budget, charged before the decode
// rather than after it, and that placement is the whole of what the charge
// buys: a block decoded and then charged would have already spent the armor
// decode, an openpgp.ReadKeyRing call and their buffers on a block the budget
// had no room for. keyringMaxPackets' own doc comment holds what charging it
// costs a real keyring and what leaving it uncharged cost a hostile one.
//
// It stands in for openpgp.ReadArmoredKeyRing, which performs both steps in one
// call with nothing in between, and reproduces that function's own two refusals
// so that a failure keeps the shape a caller already gets: input holding no
// armor block at all, and a block whose type is neither half of a key. The
// substitution exists only for the gate - see checkPacketFraming for what an
// ungated decoded stream costs, and note that it costs it here on the
// operator's own file, which is why this closes with the blob path rather than
// after it.
func readKeyArmorBlock(data []byte, spent int) (openpgp.EntityList, int, error) {
	profile := keyringProfile()
	spent += armorBlockPacketCost
	if spent > profile.maxPackets {
		return nil, 0, armorBlockBudgetError(profile.maxPackets)
	}

	decoded, blockType, err := decodeArmorBlock(data)
	if armorDecodeFoundNothing(err) {
		return nil, 0, pgperrors.InvalidArgumentError("no armored data found")
	}
	if err != nil {
		return nil, 0, err
	}
	if blockType != openpgp.PublicKeyType && blockType != openpgp.PrivateKeyType {
		return nil, 0, pgperrors.InvalidArgumentError("expected public or private key block, got: " + blockType)
	}
	packets, err := walkPacketFraming(decoded, profile, spent)
	if err != nil {
		return nil, 0, err
	}

	entities, err := openpgp.ReadKeyRing(bytes.NewReader(decoded))

	return entities, packets + armorBlockPacketCost, err
}

// nextLine returns the line of data beginning at off, without its terminator,
// together with the offset the following line begins at. A final line carrying
// no terminator is returned whole, and its follower is the end of data.
func nextLine(data []byte, off int) ([]byte, int) {
	if i := bytes.IndexByte(data[off:], '\n'); i >= 0 {
		return data[off : off+i], off + i + 1
	}

	return data[off:], len(data)
}

// checkArmorBlockStartsAtLineStart refuses data carrying armorBlockStart
// anywhere but at the start of a line, which is the one shape the cut above
// cannot see and therefore the one that silently merges two blocks into one.
// errArmorBlockStartMidLine holds why that is refused rather than cut apart.
//
// What may precede the prefix is what isArmorBlockStart itself accepts before
// it - whitespace and nothing else - so the two agree on where a block may
// open. The length test that recognizer also applies is deliberately not
// repeated here: a line too short to be an opening line is one neither the cut
// nor armor.Decode opens a block at, so it merges nothing and is left alone.
//
// Every occurrence in a line is judged rather than the first alone: two
// prefixes on one line is precisely the concatenated shape, and the second of
// them is the one carrying keys nobody would read.
//
// One pass over the file, in the bytes loadKeyring has already read: each line
// is scanned once, and a line carrying a second prefix is refused at it rather
// than walked further.
func checkArmorBlockStartsAtLineStart(data []byte) error {
	needle := []byte(armorBlockStart)

	for off := 0; off < len(data); {
		line, next := nextLine(data, off)
		for at := 0; ; {
			i := bytes.Index(line[at:], needle)
			if i < 0 {
				break
			}
			i += at
			if len(bytes.TrimSpace(line[:i])) != 0 {
				// The offset is this walk's own arithmetic and the prefix is a
				// package constant, so the message carries no part of the input.
				return fmt.Errorf("%w: %q at byte %d; separate concatenated key exports with a newline",
					errArmorBlockStartMidLine, armorBlockStart, off+i)
			}
			at = i + len(needle)
		}
		off = next
	}

	return nil
}

// isArmorBlockStart reports whether line opens an armor block.
//
// The test mirrors armor.Decode's own - the prefix on a whitespace-trimmed
// line of at least armorBlockStartMinLen bytes - because the split points have
// to include every block start the decoder will itself recognize; readArmored-
// KeyRing's own doc comment holds what recognizing one more than that costs.
// Trimming is what covers a CRLF file, whose lines carry a trailing carriage
// return here.
func isArmorBlockStart(line []byte) bool {
	line = bytes.TrimSpace(line)

	return len(line) >= armorBlockStartMinLen && bytes.HasPrefix(line, []byte(armorBlockStart))
}

// holdsSecretKey reports whether any entity carries private key material, in
// its primary key or in a subkey.
//
// Verification needs public keys and nothing else, so a keyring holding a
// secret key is refused rather than used. Both arms are checked because the
// policy is about the material present, not about which packet a particular
// exporter chose to put it in.
//
// It is a backstop rather than the refusal: no entity reaching it can carry
// private material today, since the only packets that set those two fields are
// the ones secretKeyPacketTags refuses before any parse, and every path into
// openpgp.ReadKeyRing passes that walk. So it can only answer true for an
// entity built some other way - which is what makes it worth keeping and worth
// testing directly, as TestHoldsSecretKeyReportsMaterialInEitherPlace does over
// hand-built entities rather than over a file. Removing it would leave a future
// reader that reaches the library without the walk in front of it with no
// refusal at all.
func holdsSecretKey(entities openpgp.EntityList) bool {
	for _, entity := range entities {
		if entity.PrivateKey != nil {
			return true
		}
		for i := range entity.Subkeys {
			if entity.Subkeys[i].PrivateKey != nil {
				return true
			}
		}
	}

	return false
}

// looksLikeKeybox reports whether b opens with a GnuPG keybox header blob.
//
// The magic at keyboxMagicOffset is the whole check. The eight bytes before it
// are ordinary numbers a binary packet stream could also begin with, so the
// offset is part of the identification rather than a place to start searching
// from: a file merely containing "KBXf" somewhere is not a keybox, and reading
// it as one would refuse it with a remedy that does not apply.
func looksLikeKeybox(b []byte) bool {
	end := keyboxMagicOffset + len(keyboxMagic)

	return len(b) >= end && string(b[keyboxMagicOffset:end]) == keyboxMagic
}
