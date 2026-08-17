package signature

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

const (
	// packetHeaderMSB is the bit every OpenPGP packet header octet sets. A
	// byte without it is not a packet header at all.
	packetHeaderMSB = 0x80
	// packetNewFormatBit distinguishes the new header format from the old one.
	packetNewFormatBit = 0x40
	// packetTagMask covers the six low bits a new-format header spends on the
	// tag, and the six the old format spends on the tag plus its length type.
	packetTagMask = 0x3f
	// oldFormatTagShift is how far an old-format tag sits above the two length
	// type bits below it.
	oldFormatTagShift = 2
	// oldFormatLengthTypeMask covers those two bits.
	oldFormatLengthTypeMask = 0x03

	// Three of the four old-format length types, each naming a body length
	// spelled in that many octets. The fourth is the switch's default rather
	// than a constant, because it names no length at all: the body runs to the
	// end of the input.
	oldFormatOneOctet  = 0
	oldFormatTwoOctet  = 1
	oldFormatFourOctet = 2

	// tagOctet is the single header octet carrying the format bit and the tag.
	// Every header length below is that octet plus its length field, so a
	// header length is written as the sum rather than as a number.
	tagOctet = 1
	// The length field widths, in octets. lenFieldFive is the new format's
	// five-octet form: a marker octet followed by four length octets.
	lenFieldOne  = 1
	lenFieldTwo  = 2
	lenFieldFour = 4
	lenFieldFive = 5

	// octetRange is how many values one octet holds, which is the multiplier
	// the new format's two-octet length is built with.
	octetRange = 256

	// newFormatOneOctetMax, newFormatPartialMin and newFormatFiveOctetMarker
	// are the boundaries a new-format length's first octet is read against: a
	// value below the first is the whole length, one below the second adds a
	// second octet, the marker introduces a four-octet length, and anything
	// between the second and the marker is a partial (chunked) length that
	// declares no total at all.
	newFormatOneOctetMax     = 192
	newFormatPartialMin      = 224
	newFormatFiveOctetMarker = 255

	// packetTagSignature is the tag of a signature packet, in both header
	// formats.
	packetTagSignature = 2
	// The other tags the sets below name: the ones keyringPacketTags admits,
	// whose own doc comment holds why each of them belongs in a keyring, and the
	// two secretKeyPacketTags refuses.
	packetTagPrivateKey    = 5
	packetTagPublicKey     = 6
	packetTagPrivateSubkey = 7
	packetTagTrust         = 12
	packetTagUserID        = 13
	packetTagPublicSubkey  = 14
	packetTagUserAttribute = 17
	packetTagPadding       = 21

	// The three signature versions whose bodies carry a subpacket area length at
	// all. v6 is the one whose two lengths are four octets wide; the other two
	// spell both of them in two. checkSignatureBodyFraming leaves every other
	// version alone, and its own doc comment holds what stands behind that.
	signatureVersionV4 = 4
	signatureVersionV5 = 5
	signatureVersionV6 = 6
	// signatureHashedLenOffset is where a signature packet body carries the
	// length of its hashed subpackets: after the version, the signature type,
	// the public key algorithm and the hash algorithm. The offset is the same
	// in every version; only the width of the field sitting there changes.
	signatureHashedLenOffset = 4
	// subpacketLenSizeV6 and subpacketLenSizeV4 are those two widths, hashed
	// and unhashed alike. The second name is the v4 spelling of a width every
	// version but v6 uses.
	subpacketLenSizeV6 = 4
	subpacketLenSizeV4 = 2

	// subpacketOneOctetMax and subpacketFiveOctetMarker are the boundaries a
	// signature subpacket's own length field is read against: a value below the
	// first is the whole length, the marker introduces a four-octet length
	// behind it, and anything between the two adds a second octet.
	//
	// They are deliberately not the packet-length boundaries above, which they
	// only partly resemble: a subpacket length has no partial form, so 224 is
	// no boundary here and every value from 192 to 254 takes the two-octet
	// reading.
	subpacketOneOctetMax     = 192
	subpacketFiveOctetMarker = 255
	// subpacketLenFieldTwo and subpacketLenFieldFive are the two wider length
	// field widths, in octets, marker octet included.
	subpacketLenFieldTwo  = 2
	subpacketLenFieldFive = 5
	// subpacketTypeOctet is the one octet of a subpacket's contents that names
	// its type; the rest is the subpacket's own body.
	subpacketTypeOctet = 1
	// subpacketTypeMask strips the critical bit from that octet, leaving the
	// type. The high bit is the format's own critical flag (RFC 4880 5.2.3.1,
	// RFC 9580 5.2.3.7), so masking it is what keeps an embedded signature
	// recognizable whichever way its producer set that bit.
	subpacketTypeMask = 0x7f
	// embeddedSignatureSubpacketType is the subpacket type whose body is a
	// whole signature packet body of its own, which is what makes the walk
	// below recursive.
	embeddedSignatureSubpacketType = 32

	// maxEmbeddedSignatureDepth is how many levels of embedded signature the
	// walk descends before refusing the packet.
	//
	// The cap is required rather than cosmetic. An embedded signature costs
	// about ten bytes of input per level, so without a cap the walk's own
	// recursion depth would be a function of the input, and keyringMaxSize
	// admits 64 MiB of it - millions of levels, which is a stack overflow this
	// walk would reach on its way to protecting the parser behind it.
	//
	// Four is headroom rather than a measurement. The subpacket's only defined
	// use is the cross-certification a signing subkey's binding signature
	// carries, which is exactly one level deep, so the cap leaves room for a
	// producer this project has not seen while keeping the walk's depth a
	// constant.
	maxEmbeddedSignatureDepth = 4

	// framingIndeterminate and framingPartial name the two header shapes that
	// introduce a body this walk cannot bound, spelled as the refusal prints
	// them.
	framingIndeterminate = "an old-format indeterminate length"
	framingPartial       = "a new-format partial length"
)

// packetTagSet is a set of OpenPGP packet tags, one bit per tag.
type packetTagSet uint64

// has reports whether tag is in the set.
//
// There is no bound check on tag, and none is needed: both header readers mask
// it to six bits - packetTagMask in the new format, and the same mask above the
// two length type bits in the old one - so every tag a walk can produce is in
// [0,63] by construction. Go defines a shift wider than the operand as zero
// anyway, so a tag from somewhere else would read as absent rather than as a
// neighbour's bit.
func (s packetTagSet) has(tag int) bool {
	return s&(packetTagSet(1)<<tag) != 0
}

// signatureBlobPacketTags is what a detached signature blob may hold: signature
// packets and nothing else. It is the set the blob path passes, which is the
// path whose input is not the operator's own.
const signatureBlobPacketTags = packetTagSet(1) << packetTagSignature

// keyringPacketTags is what a keyring file may hold: the packets a transferable
// PUBLIC key is made of, plus the two a real exporter writes alongside them.
//
// The set is an allow-list rather than a deny-list because the tag is six bits
// of an octet the input itself writes, so a rule naming what may not appear is
// one an attacker steps out of by rewriting them. What the list buys is that
// every packet reaching the parser behind this gate is one of these, at a count
// the profile bounds - and every one of them is driven, so no admission rests on
// an argument about where go-crypto stops reading.
//
// Two admissions are worth their own sentence, since neither belongs to a key.
// Tag 12 is GnuPG's ring-trust packet, which sits in a raw pubring.gpg an
// operator can legitimately point --keyring at. Tag 21 is RFC 9580 padding,
// permitted inside a transferable public key. Admitting either costs what
// TestEveryAdmittedTagIsReadToItsEnd and TestDriveAllocationStaysLinearInTheSegment
// measure, and neither rests on an argument about how the reader behind this
// gate treats the tag.
//
// Tag 10, the marker packet, is deliberately absent from every set here. It has
// no place in a keyring or a detached signature, and it is exactly what a
// carrier packet ahead of real material is written as.
const keyringPacketTags = packetTagSet(1)<<packetTagSignature |
	packetTagSet(1)<<packetTagPublicKey |
	packetTagSet(1)<<packetTagTrust |
	packetTagSet(1)<<packetTagUserID |
	packetTagSet(1)<<packetTagPublicSubkey |
	packetTagSet(1)<<packetTagUserAttribute |
	packetTagSet(1)<<packetTagPadding

// secretKeyPacketTags is the pair carrying secret key material: a secret key
// packet and a secret subkey packet.
//
// They are refused ahead of the allow-list rather than merely left out of it,
// because the refusal has something to say that "no place in this stream" does
// not - loadKeyring renders the export an operator should have made instead from
// it, which is the message a secret export used to earn after being parsed.
//
// Refusing them at the header buys two things a parse cannot. PrivateKey.parse
// returns at a GNU-dummy S2K, before the read that would consume the rest, so a
// tag 5 packet can parse with a NIL error and leave its declared body unread
// while its own framing is honest - the desync driveParser exists to catch,
// arriving without a re-framed header to notice it by; and parsePrivateKey,
// which packet.Read reaches only through these two tags, ends in RSA key
// validation over big.Ints the packet's own MPI lengths size, at 11.936526375s
// for ONE 16409-byte packet whose modulus is 65520 bits - just under the 65535
// an MPI's two-octet bit length can name - which is a packet a keyring file may
// hold 4089 copies of inside keyringMaxSize. Both are gone once the parser never
// sees the packet: measured, that same 16409-byte packet is refused in
// 111.333µs. TestSecretKeyPacketsAreRefusedAtTheGate holds both measurements.
const secretKeyPacketTags = packetTagSet(1)<<packetTagPrivateKey |
	packetTagSet(1)<<packetTagPrivateSubkey

const (
	// signatureBlobMaxPackets is how many packets a detached signature blob may
	// hold.
	//
	// A blob carries one signature packet per signer, and every committed
	// signature fixture carries exactly one, so this is the number of distinct
	// signers a single blob would have to name to reach the ceiling - beyond
	// anything a collection is signed by. What the ceiling buys is the second
	// term of the drive's bound: the tag set closes which parsers may run, and
	// this closes how many times any of them may.
	//
	// It shares nothing with helpers.MaxSignaturesPerCollection and is not
	// derived from it. The two carry the same number today by coincidence and
	// count different things - that one is how many blobs a collection may be
	// handed, this one is how many packets one of those blobs may hold - so a
	// reader who assumes kinship will move one when the other moves.
	//
	// What it costs is a blob whose signature packets are more numerous than
	// this, which is refused whole rather than checked in part, for the reason
	// checkPacketFraming's own last paragraph gives.
	signatureBlobMaxPackets = 64

	// keyringMaxPackets is how many packets a keyring file may hold.
	//
	// go-galaxy installs collections rather than distribution packages, so the
	// keyring it undertakes to load is a collection-publisher keyring: a key
	// packet per key with its subkeys, user ids and certification signatures,
	// which is several hundred keys' worth against a committed keyring.asc of
	// 13 packets. A distribution-scale keyring is outside what this tool
	// promises to read.
	//
	// So the disclosure is a stated limit rather than a defect: a keyring FILE
	// past this many packets stops loading - counted across every armor block in
	// it rather than per block, and counting each of those blocks as one, which
	// is walkPacketFraming's whole reason for taking a count already spent - with
	// helpers.ErrKeyringUnreadable naming the file, and the remedy is to point
	// --keyring at the keys this build actually verifies against. TestArmorBlocksShareOnePacketBudget is what makes that
	// sentence a measurement rather than a claim. Refusing it whole rather than
	// reading part of it is keyringMaxSize's own principle, which the file-size
	// ceiling states.
	//
	// The budget bounds two things rather than one, and a reader tempted to
	// simplify the accounting back needs the second: an armor block costs
	// armorBlockPacketCost against it as well as its own packets, so a file's
	// BLOCK count is bounded by this number too. Without that charge a block
	// decoding to no packets at all cost nothing, and the block count was
	// bounded by file size alone - measured, 20000 empty blocks and one real one
	// is 1600449 bytes of keyring that loads with a nil error and one key, for
	// 70500184 bytes of allocation, 44.1 times the file.
	//
	// What the charge costs the reach is a stated consequence rather than a
	// silent one. A minimal gpg key export is 3 packets, so one armored block
	// holding N of them costs 3N+1 and a file built by concatenating N
	// single-key exports costs 4N: measured, one block of 1365 keys loads and
	// one of 1366 is refused, while 1024 single-key blocks load and 1025 are
	// refused. Both figures are far above the several hundred this bound is
	// stated for. The per-block cost they are arithmetic on is what
	// TestArmorBlocksAreChargedAgainstThePacketBudget pins, at exactly the
	// ceiling and one block past it.
	keyringMaxPackets = 4096

	// armorBlockPacketCost is what one armor block costs its file's packet
	// budget, on top of the packets inside it.
	//
	// One is the smallest charge that bounds the block count at all, and it is
	// enough: a block over the budget is refused before it is decoded, so the
	// count of blocks a file may present is this ceiling divided by this cost.
	// The cost is the block's own - an armor decode, its buffers, and an
	// openpgp.ReadKeyRing call - which no packet inside it pays for.
	armorBlockPacketCost = 1
)

// packetProfile is the whole vocabulary of one caller's input: which packet tags
// may appear in it, and how many packets it may hold.
//
// The unit both terms are stated over is that whole input - the blob a source
// answered with, or the keyring file an operator configured - rather than one
// packet stream inside it. The distinction is only visible on an armored
// keyring, which is one file cut into several streams, and it is why
// walkPacketFraming takes the count already spent rather than starting each
// stream at zero.
//
// The two terms travel together because they answer the same question about the
// same input, and because the drive's cost is the product of them: the tag set
// closes which of go-crypto's parsers may run, and maxPackets closes how many
// times any of them may. A caller holding a third kind of input adds a profile
// of its own; it never widens one of these to fit.
type packetProfile struct {
	tags       packetTagSet
	maxPackets int
}

// keyringProfile is the vocabulary of a keyring file, which is the operator's
// own: the packets a transferable key is made of, and a ceiling sized for a
// collection-publisher keyring.
func keyringProfile() packetProfile {
	return packetProfile{tags: keyringPacketTags, maxPackets: keyringMaxPackets}
}

// signatureBlobProfile is the vocabulary of a detached signature blob, which is
// the input this package does not trust: signature packets and nothing else, and
// a ceiling sized for the signers one blob can plausibly name.
func signatureBlobProfile() packetProfile {
	return packetProfile{tags: signatureBlobPacketTags, maxPackets: signatureBlobMaxPackets}
}

// errMalformedSignaturePacket is the verdict for a packet stream this walk
// refuses: one whose framing declares more bytes than the stream holds, one
// whose framing declares no body length at all, one carrying a packet tag the
// caller's stream has no place for, one carrying secret key material, one
// holding more packets than that caller's stream may, one carrying bytes that
// are not a packet header this walk can read, and one whose packet the parser
// does not read to the end of.
//
// It is deliberately absent from classify's arms, so it lands on the default
// one and becomes ERRSIG. The ignore set an operator configures is keyed on the
// status, so where a refusal lands decides which existing configuration
// silently covers it.
//
// Every arm changes a verdict rather than only a cost, since each of them
// refuses a file go-crypto reads: what the narrowing costs is files that
// loaded and no longer do. Measured on this tree, openpgp.ReadKeyRing answers one
// entity and a nil error for a real public key export carrying a marker packet
// ahead of it (the tag arm), for one carrying an unbounded-framed packet behind
// it in either spelling (the framing arm), for one whose key packet is re-framed
// to declare five bytes more than the parser reads with a marker packet in the
// five (the drive arm), for one whose key packet declares a 5000 byte body with
// 271 present (the length arm) - that last one because a declared length sizes
// go-crypto's span reader and is never compared against the bytes behind it -
// and for one carrying a LEADING byte that is not a packet header, which its own
// reader scans past (the unreadable arm). That arm's other half is the exception
// and is measured rather than assumed: the same export with one trailing 0x00
// answers zero entities and an error of its own, so a trailing byte is a file
// this refusal costs nothing. The packet-count arm is the one whose refused file
// go-crypto reads without any recovery at all: a keyring simply larger than this
// tool undertakes to load.
//
// The secret-key arm changes a verdict too, though not for either of the two
// inputs it was built for. A secret export reaching LoadKeyring earns the identical
// sentinel and the identical message it earned when holdsSecretKey produced them
// after the parse, since loadKeyring renders that message from errSecretKeyPacket
// instead; a tag 5 or 7 packet reaching the blob path was already outside
// signatureBlobPacketTags, so it moves from one wording of this same sentinel to
// another and lands on the status it already landed on. What the arm changes for
// those two is what was spent getting there, which secretKeyPacketTags' own doc
// comment states and TestSecretKeyPacketsAreRefusedAtTheGate measures.
//
// A third input is neither, and this arm refuses a file that used to load: a
// keyring holding a good public key plus a tag 5 packet whose own
// PublicKey.parse fails - an algorithm no build knows, or a v3 key packet, the
// PGP 2.x shape that survives on raw keyrings. Measured, openpgp.ReadKeyRing
// answers such a file with 1 entity and a nil error, and holdsSecretKey reports
// false over it, because the old path skipped that packet as unsupported and
// built no entity for the backstop to see; the drive does not catch it either,
// since the parse consumes the whole frame. The cost is availability alone - the
// file is refused whole rather than read short, and the single entity it used to
// yield was the public key it also carried - and the policy is deliberate rather
// than incidental: what may sit in a keyring is decided by the tag the file
// itself writes, never by whether this build happens to be able to parse the
// material behind it.
//
// Where each of them lands is not universal and must not be restated as if it
// were. Two arms move a status rather than only a message: a blob carrying
// nothing but a marker packet answered NO_PUBKEY before the tag arm existed and
// answers ERRSIG now, and a blob carrying a marker ahead of a signature that
// verifies moved from a pass to ERRSIG. The other arms refuse inputs go-crypto
// itself answers with an unexpected EOF, which is already ERRSIG, so for those
// the refusal lands where the library's own failure landed.
//
// The tag arm is the one whose refused shape a real producer writes: a marker, a
// ring-trust packet, or any non-critical unknown tag ahead of real material is
// skipped by go-crypto's packet.Reader.Next and the material behind it read.
// keyringPacketTags is where that costs least, since the ring-trust packet gpg
// writes into a raw pubring.gpg is admitted there by name.
var errMalformedSignaturePacket = errors.New("malformed OpenPGP packet framing")

// errSecretKeyPacket is the second identity the secret-key arm's refusal
// carries, alongside errMalformedSignaturePacket.
//
// It exists so that loadKeyring can render one message rather than two for one
// policy: a keyring holding secret material is refused with the export an
// operator should have made instead, whether that material was found by this
// walk or by holdsSecretKey behind it. Nothing else branches on it, and it names
// the material rather than a remedy, since the remedy belongs to the caller that
// knows the file is a keyring.
var errSecretKeyPacket = errors.New("secret key material")

// errOversizedArmorHeader is the verdict for armored input carrying a block
// whose header section no blank line ends within armorHeaderMaxSize bytes.
//
// Like errMalformedSignaturePacket it is deliberately absent from classify's
// arms, so it lands on the default one and becomes ERRSIG, and for the same
// reason: the ignore set an operator configures is keyed on the status, so where
// a refusal lands decides which existing configuration silently covers it.
//
// The shape it covers is a verdict this gate adds rather than one the decoder
// would have reached on its own, and ERRSIG is where such a verdict belongs:
// BADARMOR names an envelope the decoder itself refused, which is the accident
// an operator plausibly tolerates, and routing a refusal of this tool's own into
// it would let an existing ignore entry cover a shape that entry was never
// written for.
var errOversizedArmorHeader = errors.New("oversized OpenPGP armor header section")

// checkPacketFraming walks data's OpenPGP packet framing once and refuses seven
// things: a stream that declares more bytes than it holds, a packet whose
// framing declares no body length at all, a packet whose tag is outside the
// caller's profile, a packet carrying secret key material, a stream holding more
// packets than that profile allows, bytes that are not a packet header this walk
// can read, and a packet go-crypto's own parser does not read to the end of.
//
// It is the one-shot form of walkPacketFraming, for a caller whose file is one
// packet stream. A caller holding several - readArmoredKeyRing, whose blocks are
// one file - carries the packet budget across them itself.
//
// It exists because go-crypto sizes buffers from octets the stream itself
// supplies and only afterwards discovers the stream is short. A signature
// packet spells its hashed and unhashed subpacket lengths in the stream, and
// the signature packet parser allocates each with make([]byte, n) before the
// read that fails: ten bytes of blob - a new-format signature header declaring
// an eight-octet body, a v6 version octet, and a hashed subpacket length of
// 0xffffffff - allocate 4.00 GiB, an amplification of 4.3e8.
// MaxSignaturesPerCollection of them is 640 bytes of input.
//
// SignatureMaxSize bounds none of that, because the worst case is a tiny blob,
// and the blobs are not this program's own: they arrive from a Galaxy server
// or from a persisted snapshot, both documented trust boundaries of this
// project. A keyring is the operator's own file and so a weaker case, but it
// reaches the identical parser through the identical packets, so both callers
// pass through here rather than one.
//
// profile is the caller's own packet vocabulary - signatureBlobProfile for a
// detached signature, keyringProfile for a keyring - and it is a parameter
// rather than one shared value because the two callers hold different files: a
// keyring is made of key packets and a signature blob is not, so a single set
// would have to be their union and would admit a key packet into a blob, and the
// two ceilings differ by orders of magnitude for that same reason.
//
// A nil return means "this stream is made of packets this caller may hold, none
// of them carrying secret key material, there are no more of them than it may
// hold, each declares an allocation its own bytes back, go-crypto's own parser
// read each of them to exactly the end this walk computed for it, and the stream
// ends on a packet boundary". It is not a verdict about a signature, which is
// the next reader's business.
//
// The parser clause is a measurement this walk took, never an argument about
// where go-crypto stops: driveParser runs over every packet the profile admits,
// so a tag whose parse leaves a byte behind is refused rather than reasoned
// about. That is the whole of why no tag is exempt from it.
//
// That last clause is an arm rather than an accident. A header shape this walk
// cannot read - an octet without the header MSB, or a length field running off
// the end of the input - ends the walk with a refusal naming how many bytes are
// left, because ending it with a nil error is the gate's own bypass: go-crypto
// recovers past bytes it cannot read when they sit ahead of key material,
// scanning forward for the next key packet in readToNextPublicKey, so the reader
// behind the gate does not stop where this walk had to. Measured with the walk
// ending on any frame it cannot bound, which is the shape this arm replaced, one
// 0x00 octet ahead of the amplifying blob leaves it returning nil and
// readEntities allocating 4294969288 bytes for eleven bytes of input. Both
// shapes are ones go-crypto refuses cheaply in a stream it does not recover in,
// and the arm costs the committed corpus nothing: every fixture and every armor
// block consumes to its last byte.
//
// The other two shapes stop this walk for a different reason and are not safe
// to wave through on that reasoning. An old-format indeterminate length and a
// new-format partial length are both headers go-crypto reads happily and then
// parses a body behind - the first running to the end of the input, the second
// chunked - while neither declares a total. A header declaring no total leaves
// this walk unable to locate the end of that packet, and so unable to locate
// anything behind it: waving one through hands every remaining byte to the
// parser with nothing having looked at it, and the parser does not give up
// where this walk did. So it is refused whatever tag carries it, because the
// tag is a value the input chose - a rule scoped to one tag is a rule an
// attacker steps out of by rewriting the tag bits of one octet, which is no
// bound on an attacker at all.
//
// The cost of refusing all of them is bounded by what may legitimately carry an
// unbounded framing, which is a narrow set. A partial body length is permitted
// on the data packets alone (RFC 4880 4.2.2.4, RFC 9580 4.2.1.4), and in this
// module's own copy of the library only serializeStreamHeader writes one, from
// literal.go, compressed.go and symmetrically_encrypted.go - those same data
// packets, none of which a keyring or a detached signature is made of. An
// old-format indeterminate length is written by nothing here at all, since
// serializeType spells every header this library emits in the new format. What
// this one arm refuses that go-crypto would have accepted is therefore real
// material with an unbounded-framed packet appended behind it, in either
// spelling; errMalformedSignaturePacket's own doc comment holds the same
// question answered for every arm, since each of them refuses some shape
// go-crypto reads.
//
// Every declared subpacket length is judged, at every version and at every
// nesting level, rather than only a v6 signature's own two. The narrower rule
// that judged v6 alone rested on the largest buffer a two-octet field can name
// being 65535 bytes, which is a small number and not a bound: measured, ten
// bytes declaring a v4 signature with a 0xffff hashed length allocate 67360,
// and an eighteen-byte v4 signature carrying an embedded v6 signature in its
// hashed area reaches 4294969944, because a subpacket may hold a whole
// signature body of its own and the version of that inner body is a value the
// input picks. checkSignatureBodyFraming is the recursion that closes both.
//
// What the drive costs takes both of the caller's ceilings to bound, and
// neither of them does it alone. profile.maxPackets bounds how many packets an
// input may hold and says nothing about how large one may be; the caller's size
// ceiling bounds the bytes and says nothing about how many packets they are cut
// into. So the bound has the shape maxPackets times a per-packet constant, plus
// a ratio times that size ceiling, and a figure measured at one packet size is
// that size's figure rather than the mechanism's - 4096 packets of 16 KiB is a
// keyring file of exactly keyringMaxSize, which this ceiling permits as readily
// as it permits 4096 packets of 20 bytes.
//
// Measured at the keyring ceiling, which is 4096 packets whose total is
// keyringMaxSize: the worst is a v6 signature whose hashed area is packed with
// the smallest subpackets this walk accepts - a one-octet length of 2 naming a
// type octet and one octet of body - at 3571847440 bytes for 4096 packets of
// 16383 out of 67104768 bytes of input, 53.2 times the file and in the gate
// alone. A user attribute of that same count and size comes next, at 3207829880
// (47.8x), packed with a subpacket one octet denser: a one-octet length of 1
// naming a type octet and NO body. That shape is available to it and not to the
// signature because only a signature body's subpackets are walked here, and this
// walk refuses exactly that shape in one; packed with the signature's own
// three-octet subpacket the same attribute measures 2219541392 (33.1x). The tags
// carrying no structure of their own stay one to three orders below, at the same
// count and the same size: 222167216 for user ids, 4325424 for ring-trust
// packets, 272304 for padding. The blob ceiling is the same ratio over a smaller
// input, 55811968 for 64 packets of 16383.
//
// What that costs against the reader behind the gate is the number that says
// whether driving every admitted tag was worth it, and it is 2.00x. driveParser
// runs packet.Read once per packet, which is once more than the file would have
// cost without it, so where the reader behind the gate reads the same stream to
// its end the total is twice what that reader spends alone: measured at 4325424
// against 4325616 on the ring-trust stream above, 222167216 against 222167312
// on the user id one, and 1.51x on 1365 real key exports, where the reader
// additionally builds the entities. The two worst figures above are the other
// case - a stream of signature or user attribute packets is not a transferable
// key, so that reader stops almost at once, 1744288 bytes against the gate's
// 3571847440, and the gate pays for the whole stream by itself. There the ratio
// says nothing and the absolute figure is the bound.
//
// A secondary figure, on the committed fixtures rather than on the bound: the
// whole gate costs 1792 bytes over sig-a-valid.asc's single packet and 33456
// over keyring.asc's thirteen, read block by block as readArmoredKeyRing reads
// them, inside the 72384 readEntities spends on that file and the 77536
// LoadKeyring spends end to end.
//
// Refusing a packet refuses the whole file rather than that packet. That is
// inherited from keyringMaxSize's own stated principle rather than decided
// again here: a keyring silently missing exactly the keys behind some rejected
// packet is the failure this package refuses to produce, and the same reasoning
// covers a signature blob whose first packet is fine and whose second is not.
func checkPacketFraming(data []byte, profile packetProfile) error {
	_, err := walkPacketFraming(data, profile, 0)

	return err
}

// walkPacketFraming is checkPacketFraming with the packet count as a value the
// caller carries: spent is how much of this profile's ceiling the SAME FILE has
// already used, and the returned count is how many packets this walk itself
// found.
//
// The two numbers are not the same currency, and the caller owns the
// difference: what a keyring file spends includes armorBlockPacketCost for
// every armor block, this one included, which is a charge no packet in the
// stream accounts for. readKeyArmorBlock is where that is added and where the
// returned count is turned back into a charge.
//
// The two exist separately because the ceiling is a property of the file and an
// armored keyring is several packet streams in one file. Counting per stream
// bounds each block and nothing else, so a file's total is bounded only by
// keyringMaxSize divided by whatever a block costs: measured before the budget
// was carried, one armored block of 2049 packets loads, and a file of two such
// blocks - 4098 packets against a ceiling of 4096 - loads too, 1366 entities and
// a nil error out of 505152 bytes.
//
// spent is added to this walk's own count rather than subtracted from the
// profile, so that the refusal keeps naming the ceiling an operator configured
// their file against rather than whatever was left of it when the crossing block
// began.
func walkPacketFraming(data []byte, profile packetProfile, spent int) (int, error) {
	// One reader for the whole walk, Reset per frame rather than built per
	// frame: the drive below needs an io.Reader over each packet's own bytes,
	// and a reader per packet would cost one allocation per packet of a keyring
	// for nothing.
	var rd bytes.Reader

	// Counted here rather than derived afterwards: the ceiling has to refuse the
	// packet that would cross it before that packet is judged or driven, or it
	// would report a cost instead of bounding one.
	packets := 0

	for len(data) > 0 {
		frame, status := readPacketFrame(data)
		if err := profile.judgeHeader(frame, status, len(data)); err != nil {
			return packets, err
		}
		packets++
		if spent+packets > profile.maxPackets {
			// The ceiling is this profile's own number, so the message carries
			// no part of the input. The arm sits here, immediately behind the
			// header's own judgment and ahead of everything below, because a
			// ceiling that refused the packet after judging and driving it would
			// report a cost rather than bound one.
			//
			// It says "the whole input" rather than "one stream" because the
			// budget spans a keyring file's armor blocks: a message naming a
			// stream sends an operator looking for a single oversized block in a
			// file whose blocks are all ordinary.
			return packets, fmt.Errorf("%w: more than %d packets across the whole input",
				errMalformedSignaturePacket, profile.maxPackets)
		}
		if err := judgePacket(&rd, data, frame); err != nil {
			return packets, err
		}

		data = data[int64(frame.headerLen)+frame.bodyLen:]
	}

	return packets, nil
}

// armorBlockBudgetError is the refusal for an armored keyring whose blocks
// alone exhaust the packet budget, which is the one crossing walkPacketFraming
// cannot report: a block decoding to no packets never enters that loop.
//
// It names the same ceiling in the same words the packet arm does, and adds the
// one thing an operator would otherwise have to guess at - that an armor block
// is counted like a packet. maxPackets is the profile's own number, so the
// message carries no part of the input.
func armorBlockBudgetError(maxPackets int) error {
	return fmt.Errorf("%w: more than %d packets across the whole input, counting each armor block as one",
		errMalformedSignaturePacket, maxPackets)
}

// judgeHeader judges one packet header against this profile: the two framings
// this walk cannot follow, and then the tag.
//
// remaining is how many bytes of the stream are left, which is what the
// unreadable arm reports; it is this walk's own arithmetic over the input's
// LENGTH, never a copy of the input's own bytes, so the message can carry no
// character an attacker chose - it can neither forge an extra printed line
// nor drive a terminal. That is a claim about injection, not about
// disclosure: remaining is still a fact ABOUT the input, and for a blob whose
// framing this walk cannot read at all it is exactly the blob's own length -
// source.go's fetchFile doc comment states what a caller reading local
// content this program did not author can already infer from a number of
// that shape. Both readers mask a tag to at most six bits, so it too renders
// as a number below 64 and can carry no character able to forge a line or
// drive a terminal - the tag has no comparable disclosure to caveat, since
// six bits of framing say nothing about a blob's size or content.
//
// The order of the arms is the whole content of this function and is pinned by
// TestUnboundedFramingIsJudgedBeforeTheTag. A framing this walk cannot bound is
// refused whatever tag carries it, because the tag of a packet whose end cannot
// be found decides nothing; and an unreadable header carries no tag at all, only
// a zero value, so judging one would be judging a field that was never read.
//
// The secret-key arm sits ahead of the allow-list for a reason of message rather
// than of verdict: no profile admits those two tags, so the allow-list would
// refuse them anyway, and what would be lost is the one message an operator can
// act on - which loadKeyring renders from errSecretKeyPacket.
//
// Refusing both of those states is also what makes the caller's loop advance,
// since only a bounded frame has a header length to advance past: the other two
// carry a zero one, so a caller that walked on from either would re-read the
// same bytes until some other bound stopped it.
func (p packetProfile) judgeHeader(frame packetFrame, status frameStatus, remaining int) error {
	switch {
	case status == frameUnbounded:
		return fmt.Errorf("%w: a tag %d packet carries %s, which declares no body length at all",
			errMalformedSignaturePacket, frame.tag, frame.framing)
	case status == frameUnreadable:
		// An empty input never enters the walk, and a walk that consumed the
		// last packet exactly leaves it by its own loop condition, so neither
		// reaches this arm.
		return fmt.Errorf("%w: %d bytes are not a packet header this walk can read",
			errMalformedSignaturePacket, remaining)
	case secretKeyPacketTags.has(frame.tag):
		return fmt.Errorf("%w: a tag %d packet carries %w",
			errMalformedSignaturePacket, frame.tag, errSecretKeyPacket)
	case !p.tags.has(frame.tag):
		return fmt.Errorf("%w: a tag %d packet has no place in this stream",
			errMalformedSignaturePacket, frame.tag)
	default:
		return nil
	}
}

// judgePacket judges one bounded packet, whose header the profile has already
// accepted: that the body it declares is backed by bytes actually present, that
// a signature body's own declared lengths are, and that go-crypto's parser reads
// the packet to exactly the end this walk computed for it.
//
// The drive runs on every packet that gets here rather than on a set of tags,
// which is what leaves no admitted tag resting on an argument about where
// go-crypto stops reading. driveParser's own doc comment holds what that costs.
//
// data starts at the packet's own header and runs to the end of the stream, so
// the returned error is the caller's whole verdict and the caller advances past
// this packet only once this function has agreed the bytes are there.
//
// The subpacket check is what makes the drive below it safe, so it has to stay
// ahead of it, and it carries two properties rather than one: it bounds every
// data-derived allocation Signature.parse makes on this body, and it keeps a
// body go-crypto panics on away from that parser - a hashed subpacket carrying a
// type and no body indexes an empty slice inside parseSignatureSubpacket.
// Nothing else here looks at a subpacket at all, so reordering the two turns the
// gate into the amplifier and re-opens the panic in one move.
func judgePacket(rd *bytes.Reader, data []byte, frame packetFrame) error {
	body := data[frame.headerLen:]
	if frame.bodyLen > int64(len(body)) {
		return fmt.Errorf("%w: a packet declares a %d byte body with %d present",
			errMalformedSignaturePacket, frame.bodyLen, len(body))
	}
	if frame.tag == packetTagSignature {
		if err := checkSignatureBodyFraming(body[:frame.bodyLen], 0); err != nil {
			return err
		}
	}

	return driveParser(rd, data[:int64(frame.headerLen)+frame.bodyLen], frame.tag)
}

// driveParser runs go-crypto's own packet parser over one packet's bytes and
// refuses a packet the parser leaves unread bytes inside.
//
// It exists because this walk's packet boundaries are not automatically the
// parser's. packet.Read consumes a packet's remaining body only on its error
// paths, so a parse that SUCCEEDS while reading less than the header declared
// leaves the shared reader positioned inside that body, and every packet the
// consumer reads afterwards is cut at an offset the input chose rather than at
// the one this walk judged. Measured: 67 bytes made of a real 51-byte public
// key body re-framed to declare 61, with the ten-byte amplifying signature
// packet in the ten bytes nobody reads, walk clean under a check of declared
// lengths alone and allocate 4294975376 through go-crypto's own keyring reader.
// Requiring residual 0 is what makes the two agree.
//
// What is judged is that residual, so this arm refuses shapes go-crypto reads
// without complaint as well as the carriers it was built for. Measured, a real
// public key export whose key packet is re-framed to declare five bytes more
// than the parser reads, those five being a marker packet, is one
// openpgp.ReadKeyRing answers with one entity and a nil error; this refuses it,
// naming the five unread bytes.
//
// The parse error is deliberately discarded, and judging the residual instead is
// what makes that safe rather than lenient: a parse that failed without
// consuming its frame is refused here exactly as a parse that succeeded without
// consuming it is. So the error tells this walk nothing the residual does not,
// while judging it would refuse material go-crypto itself skips and reads on
// from - a packet whose key algorithm this build does not know, which
// ReadKeyRing answers by dropping that entity and keeping the rest. What a failed
// parse costs the corpus is a measurement rather than an argument:
// TestEveryAdmittedTagIsReadToItsEnd walks every tag a keyring may hold over
// bodies built to fail, and every one of them comes back with nothing unread.
//
// Every packet the caller's profile admits is driven, with no tag exempt, and
// that is the point rather than an implementation detail. The narrower set this
// replaced excluded six tags on the argument that each consumes its whole frame
// however its parse ends - an argument about go-crypto's control flow that this
// package cannot enforce, and one that was wrong: PrivateKey.parse returns at a
// GNU-dummy S2K, ahead of the read that would have consumed the body, so a
// 36-byte tag 5 packet parses with a NIL error and ten bytes left unread.
// Measured against the walk that trusted the enumeration, that packet with the
// amplifying signature in its ten unread bytes walked clean and left
// readEntities allocating 4294980800 bytes. Those two tags are refused outright
// now (secretKeyPacketTags holds why), and the rest are driven, so no admitted
// tag rests on a claim about where a parser stops.
//
// What driving everything costs is a per-packet term and a count, and both are
// bounded by things this package holds. The count is profile.maxPackets. The
// term is what one packet's parse allocates, which is a constant of go-crypto's
// making plus an amount proportional to that packet's own bytes - measured for
// every admitted tag across four segment sizes by
// TestDriveAllocationStaysLinearInTheSegment, which is where the ratio lives
// rather than in this comment, since a dependency bump moves it.
//
// Two of that term's parts are load-bearing enough to be pinned on their own,
// because each is a number rather than a shape. An MPI is sized from a two-octet
// bit length, so one read is at most 8 KiB whatever the segment's length
// (TestMPIStaysBoundedByItsTwoOctetLength); the per-packet total is a constant
// only while the COUNT of MPI reads stays fixed per algorithm, so an algorithm
// reading a data-derived number of them would break it. And a v5 signature
// allocates nothing because packet.V5Disabled is set under a `!v5` build
// constraint go-crypto's own config file carries, so a build with -tags v5
// changes the answer; nothing in this repository's Justfile or CI passes it, and
// TestV5ParsingStaysDisabled is what says so at test time rather than in prose.
//
// A signature packet's own two subpacket areas are the one part of the term this
// package bounds directly rather than measures: checkSignatureBodyFraming judges
// every declared subpacket length, at every version and every nesting level,
// against the bytes actually present, which is why it runs ahead of the drive.
// Signature.parse's own signature MPIs sit past where that walk looks and are
// left to the two-octet bound above; extending the walk over them would be the
// shadow parser this design refuses to become.
func driveParser(rd *bytes.Reader, segment []byte, tag int) error {
	rd.Reset(segment)
	_, _ = packet.Read(rd)
	if unread := rd.Len(); unread != 0 {
		// Both the tag and the counts are numbers this walk computed from the
		// input's own SHAPE, never a copy of its bytes, so the message carries
		// no character an attacker chose and needs no sanitizing on that
		// account. That is an injection claim, not a disclosure one: unread
		// and len(segment) still say something about the packet this walk
		// refused - source.go's fetchFile doc comment states the wider version
		// of what a number derived from local content can tell a repository
		// that chose the path.
		return fmt.Errorf("%w: the parser left %d of a tag %d packet's %d bytes unread",
			errMalformedSignaturePacket, unread, tag, len(segment))
	}

	return nil
}

// frameStatus is what readPacketFrame made of the octets at the front of the
// walk's remaining bytes.
type frameStatus int

const (
	// frameUnreadable means the octets are not a header this walk can read, so
	// there is nothing here to judge and nothing behind it to reach - which is
	// why the walk refuses the stream rather than ending on it.
	frameUnreadable frameStatus = iota
	// frameBounded means the header declares how long its body is, which is the
	// only state whose bodyLen and headerLen mean anything.
	frameBounded
	// frameUnbounded means the header is one go-crypto reads and parses a body
	// behind while declaring no total for it, so this walk can find neither the
	// end of that body nor whatever follows it. Only tag and framing are set.
	frameUnbounded
)

// packetFrame is one packet header as the walk read it: where its body starts,
// how long the header says that body is, and which packet type it introduces.
//
// framing is set only alongside frameUnbounded, where it names the shape that
// left the body unbounded so the refusal can say which one it was; the other
// two states leave it empty.
type packetFrame struct {
	framing   string
	bodyLen   int64
	headerLen int
	tag       int
}

// readPacketFrame reads the packet header at the front of data, reporting which
// of the three states it landed in. checkPacketFraming's own doc comment holds
// which header shapes reach frameUnreadable and frameUnbounded, and why neither
// of the two is safe to end the walk on with a nil error.
func readPacketFrame(data []byte) (packetFrame, frameStatus) {
	first := data[0]
	if first&packetHeaderMSB == 0 {
		return packetFrame{}, frameUnreadable
	}
	if first&packetNewFormatBit == 0 {
		return oldFormatFrame(data)
	}

	return newFormatFrame(data)
}

// oldFormatFrame reads an old-format header, whose two lowest bits name how
// many octets spell the body length.
func oldFormatFrame(data []byte) (packetFrame, frameStatus) {
	tag := int((data[0] & packetTagMask) >> oldFormatTagShift)

	switch data[0] & oldFormatLengthTypeMask {
	case oldFormatOneOctet:
		if len(data) < tagOctet+lenFieldOne {
			return packetFrame{}, frameUnreadable
		}

		return packetFrame{tag: tag, headerLen: tagOctet + lenFieldOne, bodyLen: int64(data[tagOctet])}, frameBounded
	case oldFormatTwoOctet:
		if len(data) < tagOctet+lenFieldTwo {
			return packetFrame{}, frameUnreadable
		}
		bodyLen := int64(binary.BigEndian.Uint16(data[tagOctet : tagOctet+lenFieldTwo]))

		return packetFrame{tag: tag, headerLen: tagOctet + lenFieldTwo, bodyLen: bodyLen}, frameBounded
	case oldFormatFourOctet:
		if len(data) < tagOctet+lenFieldFour {
			return packetFrame{}, frameUnreadable
		}
		bodyLen := int64(binary.BigEndian.Uint32(data[tagOctet : tagOctet+lenFieldFour]))

		return packetFrame{tag: tag, headerLen: tagOctet + lenFieldFour, bodyLen: bodyLen}, frameBounded
	default:
		// The indeterminate length type: the body runs to the end of the input,
		// so the header declares no total this walk could compare anything
		// against - and go-crypto hands that same body to the parser anyway.
		return packetFrame{tag: tag, framing: framingIndeterminate}, frameUnbounded
	}
}

// newFormatFrame reads a new-format header, whose length is spelled in one, two
// or five octets - or is partial, and declares no total at all.
func newFormatFrame(data []byte) (packetFrame, frameStatus) {
	if len(data) < tagOctet+lenFieldOne {
		return packetFrame{}, frameUnreadable
	}
	tag := int(data[0] & packetTagMask)
	lead := data[tagOctet]

	switch {
	case lead < newFormatOneOctetMax:
		return packetFrame{tag: tag, headerLen: tagOctet + lenFieldOne, bodyLen: int64(lead)}, frameBounded
	case lead < newFormatPartialMin:
		if len(data) < tagOctet+lenFieldTwo {
			return packetFrame{}, frameUnreadable
		}
		// RFC 9580's two-octet form, which encodes lengths from 192 upwards.
		bodyLen := int64(lead-newFormatOneOctetMax)*octetRange + int64(data[tagOctet+lenFieldOne]) + newFormatOneOctetMax

		return packetFrame{tag: tag, headerLen: tagOctet + lenFieldTwo, bodyLen: bodyLen}, frameBounded
	case lead < newFormatFiveOctetMarker:
		// A partial body length introduces a chunked packet, so this octet
		// sizes one chunk rather than the body, and no octet anywhere in the
		// header names the total - go-crypto reads the chunks as they come and
		// parses the body behind them all the same.
		return packetFrame{tag: tag, framing: framingPartial}, frameUnbounded
	default:
		if len(data) < tagOctet+lenFieldFive {
			return packetFrame{}, frameUnreadable
		}
		bodyLen := int64(binary.BigEndian.Uint32(data[tagOctet+lenFieldOne : tagOctet+lenFieldFive]))

		return packetFrame{tag: tag, headerLen: tagOctet + lenFieldFive, bodyLen: bodyLen}, frameBounded
	}
}

// checkSignatureBodyFraming refuses a signature packet body whose declared
// subpacket lengths name more bytes than the body holds, at any signature
// version and at any nesting depth.
//
// depth is how many embedded signatures were descended through to reach this
// body, and a body past maxEmbeddedSignatureDepth is refused rather than walked.
//
// The width of both length fields is read from the version octet, four for v6
// and two for v4 and v5, which is the width that version's own subpacket areas
// are spelled in. A body too short to carry the prefix returns nil, and so does
// a version octet outside those three: there is nothing at the length field's
// offset to judge, and in the second case the octets there are some other field
// entirely.
//
// Both of those are admissions rather than refusals, so what licenses them is a
// measurement rather than an argument: TestAdmittedShapesAllocateNothing walks
// every one of them and pins what the parser behind this gate then spends, which
// is where a version whose octets DID size an allocation would show up.
//
// Judging an unjudged version anyway refuses a real file, and that too is
// measured: a v3 certification signature - the shape PGP 2.x and GnuPG 1.x
// wrote, which survives on long-lived keys - carries its creation time where a
// v4 body spells its hashed length, so a walk reading those two octets as one
// answers a keyring holding it with "a signature declares 40764 bytes of hashed
// subpackets with 16 present". v5 is judged rather than waved through with the
// versions this parser rejects, because its refusal is a build-tag default
// rather than a property of the format, and judging it costs nothing if that
// default ever moves.
//
// The check is the precondition driveParser depends on. It is what bounds every
// make([]byte, n) Signature.parse performs on this body, so it has to be at
// least as thorough as that parser is recursive - hence walkSignatureSubpackets
// behind it rather than two length comparisons and a return.
func checkSignatureBodyFraming(body []byte, depth int) error {
	if depth > maxEmbeddedSignatureDepth {
		// The numbers are this walk's own, so the message carries no part of
		// the input.
		return fmt.Errorf("%w: a signature nests embedded signatures more than %d deep",
			errMalformedSignaturePacket, maxEmbeddedSignatureDepth)
	}
	if len(body) == 0 {
		return nil
	}
	lenSize, judged := subpacketLenSize(body[0])
	if !judged {
		return nil
	}
	prefixLen := signatureHashedLenOffset + lenSize
	if len(body) < prefixLen {
		return nil
	}

	rest := body[prefixLen:]
	hashed := subpacketAreaLen(body[signatureHashedLenOffset:], lenSize)
	if hashed > int64(len(rest)) {
		return subpacketOverrun("hashed", hashed, len(rest))
	}
	if err := walkSignatureSubpackets(rest[:hashed], depth); err != nil {
		return err
	}

	// The unhashed length sits immediately behind the hashed subpackets. A body
	// too short to carry it is left to go-crypto, and what that costs is one of
	// the shapes TestAdmittedShapesAllocateNothing measures.
	rest = rest[hashed:]
	if len(rest) < lenSize {
		return nil
	}
	unhashed := subpacketAreaLen(rest, lenSize)
	rest = rest[lenSize:]
	if unhashed > int64(len(rest)) {
		return subpacketOverrun("unhashed", unhashed, len(rest))
	}

	return walkSignatureSubpackets(rest[:unhashed], depth)
}

// subpacketLenSize reports how many octets a signature of this version spells
// each of its two subpacket area lengths in, and whether those lengths are this
// walk's business at all.
//
// The false answer is the version gate: the three versions here are the ones
// whose bodies spell a subpacket area length at that offset at all, and for any
// other version the octets sitting there belong to some other field.
// checkSignatureBodyFraming's own doc comment holds what judging them anyway
// cost, why v5 is here rather than among the versions the parser refuses, and
// which measurement stands behind admitting the rest.
func subpacketLenSize(version byte) (int, bool) {
	switch version {
	case signatureVersionV6:
		return subpacketLenSizeV6, true
	case signatureVersionV4, signatureVersionV5:
		return subpacketLenSizeV4, true
	default:
		return 0, false
	}
}

// subpacketAreaLen reads a hashed or unhashed subpacket area length of lenSize
// octets from the front of b, which the caller has already checked holds at
// least that many.
func subpacketAreaLen(b []byte, lenSize int) int64 {
	if lenSize == subpacketLenSizeV6 {
		return int64(binary.BigEndian.Uint32(b[:subpacketLenSizeV6]))
	}

	return int64(binary.BigEndian.Uint16(b[:subpacketLenSizeV4]))
}

// walkSignatureSubpackets walks one subpacket area and descends into every
// embedded signature it carries, so that an inner signature's declared lengths
// are judged exactly as the outer one's were.
//
// The area is walked because an embedded signature subpacket holds a whole
// signature packet body, which go-crypto parses with the same Signature.parse
// that allocates from a declared length - so a signature whose own two lengths
// are honest can still name a four-octet length one level down. Both areas are
// walked, not only the hashed one, because go-crypto parses an embedded
// signature out of either.
//
// A shape this walk cannot parse - a length field the area does not carry
// whole, one naming more than the area holds, or a subpacket of no length at
// all - stops it with a nil error rather than a refusal. That is an admission,
// so what stands behind it is TestAdmittedShapesAllocateNothing rather than an
// argument about where the parser gives up: each of those shapes is walked
// there and what the parser then spends on it is pinned.
//
// The last of the three is also a guard on this walk itself, and it is the
// cheapest one here to lose: contents of no length at all reach contents[0]
// below, so deleting it panics inside this package rather than inside the
// parser it protects - measured on the ten bytes framingCases spells as a
// signature subpacket of no length at all, which is that row's whole reason for
// being there.
//
// One shape is refused rather than left alone: a subpacket whose contents are
// the type octet and nothing else. parseSignatureSubpacket strips that octet and
// then dispatches on the type with the remainder possibly empty, and exactly one
// of its arms - the exportable certification subpacket, type 4 - indexes that
// remainder with no length guard, so twelve bytes of blob panic the parser
// rather than failing it. Seven other arms read an empty body harmlessly (the
// three preference lists, the keyserver URL, the policy URI, the signer user id
// and the cipher suites), so the rule does refuse a shape go-crypto would have
// read for those. What it costs is measured rather than assumed: across the
// committed corpus there are 210 subpackets in 24 signature packets, the
// shortest contents length is 2, and not one of them is empty.
//
// It allocates nothing: every step is a subslice of the area it was handed.
func walkSignatureSubpackets(subs []byte, depth int) error {
	for len(subs) > 0 {
		length, rest, ok := readSubpacketLength(subs)
		if !ok || length > int64(len(rest)) {
			return nil
		}
		contents := rest[:length]
		subs = rest[length:]
		if len(contents) < subpacketTypeOctet {
			return nil
		}
		if len(contents) == subpacketTypeOctet {
			// The numbers are this walk's own, so the message carries no part of
			// the input.
			return fmt.Errorf("%w: a signature subpacket carries a type octet and no body",
				errMalformedSignaturePacket)
		}
		if contents[0]&subpacketTypeMask != embeddedSignatureSubpacketType {
			continue
		}
		if err := checkSignatureBodyFraming(contents[subpacketTypeOctet:], depth+1); err != nil {
			return err
		}
	}

	return nil
}

// readSubpacketLength reads one subpacket's own length field from the front of
// subs, which the caller has already checked is not empty, and returns that
// length together with the bytes behind the field.
//
// ok is false for a field the remaining bytes do not carry whole, which ends the
// caller's walk with a nil answer - one of the admissions
// TestAdmittedShapesAllocateNothing measures.
func readSubpacketLength(subs []byte) (int64, []byte, bool) {
	switch {
	case subs[0] < subpacketOneOctetMax:
		return int64(subs[0]), subs[1:], true
	case subs[0] < subpacketFiveOctetMarker:
		if len(subs) < subpacketLenFieldTwo {
			return 0, nil, false
		}
		length := int64(subs[0]-subpacketOneOctetMax)*octetRange + int64(subs[1]) + subpacketOneOctetMax

		return length, subs[subpacketLenFieldTwo:], true
	default:
		if len(subs) < subpacketLenFieldFive {
			return 0, nil, false
		}

		return int64(binary.BigEndian.Uint32(subs[1:subpacketLenFieldFive])), subs[subpacketLenFieldFive:], true
	}
}

// subpacketOverrun renders the refusal for one over-declared subpacket length,
// naming which of the two it was and both numbers, so an operator reading the
// failure can tell a truncated blob from a fabricated length. The numbers are
// this walk's own, so the message carries no part of the input.
func subpacketOverrun(which string, declared int64, available int) error {
	return fmt.Errorf("%w: a signature declares %d bytes of %s subpackets with %d present",
		errMalformedSignaturePacket, declared, which, available)
}

// decodeArmorBlock decodes the first ASCII-armored block in data and returns
// its bytes together with the block type the armor header declared.
//
// The decode is this package's own rather than the library entry point's
// because checkPacketFraming has to run on the decoded bytes: an armored blob
// is base64 text, so its framing is not there to walk, and the library's
// armored entry points hand their decoded stream straight to the packet parser
// with nothing in between. Decoding here is what puts the gate in between.
//
// The block type is returned rather than checked, because the two callers
// accept different ones and each names its own in its refusal.
//
// checkArmorHeaderSection runs ahead of the decode because the cost it bounds is
// spent inside the decode, where nothing this package holds can reach it. Both
// callers carry its refusal exactly as they already carry a decode failure.
func decodeArmorBlock(data []byte) ([]byte, string, error) {
	if err := checkArmorHeaderSection(data); err != nil {
		return nil, "", err
	}

	block, err := armor.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", err
	}

	// Armor is base64, so the decoded body is at most three quarters of the
	// armored text; the extra bytes.MinRead is the headroom ReadFrom wants
	// available before it stops, so the buffer is sized once and never grown.
	buf := bytes.NewBuffer(make([]byte, 0, len(data)*3/4+bytes.MinRead))
	if _, err := buf.ReadFrom(block.Body); err != nil {
		return nil, "", err
	}

	return buf.Bytes(), block.Type, nil
}

// checkArmorHeaderSection refuses armored input carrying a block whose header
// section no blank line ends within armorHeaderMaxSize bytes. That constant's
// own doc comment holds why the section is the term worth bounding, why the
// bound costs a block's body nothing, and where the number comes from.
//
// Every opening line in data is judged rather than the first alone, because the
// first one need not be the block armor.Decode finally reads a header from: a
// header line carrying no colon makes it abandon the block and resume its search
// for the next opening line, so a section judged here can be one the decoder
// walked away from. Bounding one section and waving the rest through would leave
// the whole cost reachable by putting a colon-free line under the first opening
// line and the long header under the second.
//
// Judging all of them still costs one linear pass, because an accepted section
// is skipped rather than re-walked, and skipping it loses nothing: an opening
// line inside a section already accepted reaches the same blank line that ended
// it, from no further away, so its own section cannot be over the bound while
// the one containing it was not.
//
// A candidate opening line is isArmorBlockStart's, which recognizes every line
// armor.Decode does and can recognize one more - a line the decoder skips as
// garbage for being too long to read whole. What is measured is the run behind
// such a line rather than the line's own length, and it is measured even where
// the decoder opens no block there at all, which widens the refusal on the blob
// path: leading garbage carrying a "-----BEGIN "-shaped line of at least
// armorBlockStartMinLen bytes, with more than armorHeaderMaxSize bytes and no
// blank line behind it, is refused here ahead of a real block the decoder does
// reach. Measured on such a blob, armor.Decode with nothing in front of it
// returns that block with type "PGP SIGNATURE" and a nil error, while this
// refusal turns the same bytes into ERRSIG. The keyring path refuses the same
// file with no bound in front of it at all, since readArmoredKeyRing cuts a
// block at that same line and readKeyArmorBlock finds no key material in what it
// was handed, so the blob path is where the widening shows.
//
// It is fail-closed and disclosed rather than narrowed. Narrowing it means
// telling a line the decoder reads whole from one it skips, which only the size
// of the decoder's own bufio.Reader answers - an unexported 100 in a vendored
// dependency that a bump can move with nothing here noticing. A refusal pinned
// to that number is the more fragile of the two.
func checkArmorHeaderSection(data []byte) error {
	for off := 0; off < len(data); {
		line, next := nextLine(data, off)
		off = next
		if !isArmorBlockStart(line) {
			continue
		}

		end, ok := armorHeaderSectionEnd(data, off)
		if !ok {
			// The message carries no part of the input: the numbers are this
			// package's own, so the refusal needs no sanitizing on its way out.
			return fmt.Errorf("%w: no blank line ends one within %d bytes",
				errOversizedArmorHeader, armorHeaderMaxSize)
		}
		off = end
	}

	return nil
}

// armorHeaderSectionEnd walks the header lines beginning at off and reports the
// offset just past the blank line ending them, or ok=false when no blank line
// arrives within armorHeaderMaxSize bytes of off.
//
// Input that runs out while still inside the bound is accepted instead, and left
// to armor.Decode, which reports the block it could not read there as io.EOF -
// the answer the decoder gives such a file with no bound in front of it at all,
// and a truncated or corrupt file is the realistic instance of the shape.
// Accepting it gives the decoder nothing the bound was holding back: falling
// out of the loop proves section-start-to-EOF is inside the bound, by the loop's
// own per-line check, so the largest input that can leave it that way is a
// section of armorHeaderMaxSize bytes, measured at 96800 bytes of allocation
// through the decode - the same order as the 102960 spent by the widest section
// the bound accepts, both on the one-character header name armorHeaderMaxSize's
// own doc comment measures. Answering it with the oversized-header refusal
// instead would name a section oversized for having ended inside the bound. So
// ok=false carries one meaning, that the section is over the bound.
//
// Lines are judged whole. Cutting data at the bound and looking for a blank line
// in the prefix instead would read the leading bytes of a line straddling the
// cut as a line of their own, so a header line long enough to matter would pass
// whenever the byte sitting at the bound happened to fall inside a run of
// spaces.
func armorHeaderSectionEnd(data []byte, off int) (int, bool) {
	start := off
	for off < len(data) {
		line, next := nextLine(data, off)
		if next-start > armorHeaderMaxSize {
			return 0, false
		}
		if len(bytes.TrimSpace(line)) == 0 {
			return next, true
		}
		off = next
	}

	return len(data), true
}

// armorDecodeFoundNothing reports whether err is decodeArmorBlock's answer for
// input holding no armor block at all, which armor.Decode spells as io.EOF.
//
// The test is unambiguous even though decodeArmorBlock has two failing steps:
// bytes.Buffer.ReadFrom reports a clean end of input as a nil error, so io.EOF
// can only have come from the decode.
func armorDecodeFoundNothing(err error) bool {
	return errors.Is(err, io.EOF)
}
