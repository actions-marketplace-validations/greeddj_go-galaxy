package signature

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/ProtonMail/go-crypto/openpgp/armor"
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

	// signatureVersionV6 is the one signature version whose subpacket lengths
	// are four octets wide.
	signatureVersionV6 = 6
	// v6HashedLenOffset is where a v6 signature packet body carries the
	// four-octet length of its hashed subpackets: after the version, the
	// signature type, the public key algorithm and the hash algorithm.
	v6HashedLenOffset = 4
	// v6SignaturePrefixLen is the whole fixed prefix ahead of those hashed
	// subpackets, which is v6HashedLenOffset plus the four length octets.
	v6SignaturePrefixLen = 8
	// subpacketLenSize is the width of a v6 signature's subpacket length
	// fields, hashed and unhashed alike.
	subpacketLenSize = 4

	// framingIndeterminate and framingPartial name the two header shapes that
	// introduce a body this walk cannot bound, spelled as the refusal prints
	// them.
	framingIndeterminate = "an old-format indeterminate length"
	framingPartial       = "a new-format partial length"
)

// errMalformedSignaturePacket is the verdict for a packet stream this walk
// refuses: one whose framing declares more bytes than the stream holds, or a
// packet whose framing declares no body length at all.
//
// It is deliberately absent from classify's arms, so it lands on the default
// one and becomes ERRSIG - the status go-crypto's own unexpected EOF already
// produces for every input this gate refuses. That keeps the gate a change of
// cost rather than a change of verdict, which matters because the ignore set an
// operator configures is keyed on the status: a gate that moved these inputs to
// another status would silently take an existing configuration out of, or into,
// scope.
var errMalformedSignaturePacket = errors.New("malformed OpenPGP packet framing")

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

// checkPacketFraming walks data's OpenPGP packet framing once and refuses two
// things: a stream that declares more bytes than it holds, and a packet whose
// framing declares no body length at all.
//
// It exists because go-crypto sizes two buffers from octets the stream itself
// supplies and only afterwards discovers the stream is short. A v6 signature
// packet spells its hashed and unhashed subpacket lengths in four octets each,
// and the signature packet parser allocates each with make([]byte, n) before
// the read that fails: ten bytes of blob - a new-format signature header
// declaring an eight-octet body, a v6 version octet, and a hashed subpacket
// length of 0xffffffff - allocate 4.00 GiB, an amplification of 4.3e8.
// MaxSignaturesPerCollection of them is 640 bytes of input.
//
// SignatureMaxSize bounds none of that, because the worst case is a tiny blob,
// and the blobs are not this program's own: they arrive from a Galaxy server
// or from a persisted snapshot, both documented trust boundaries of this
// project. A keyring is the operator's own file and so a weaker case, but it
// reaches the identical parser through the identical packets, so both callers
// pass through here rather than one.
//
// The gate is narrow in two directions, and both are deliberate.
//
// It judges declared lengths and nothing else, so a nil return means "nothing
// here declares an allocation the bytes cannot back", never "this is a
// signature". Two header shapes end the walk with a nil error rather than a
// refusal, and they are two separate arguments rather than one: an octet with
// no MSB is refused by go-crypto's own readHeader before it reads a length at
// all, and a length field running off the end of the input fails readLength or
// the readFull behind it before any body is parsed. Both are cheap refusals
// somebody else already makes, and a second opinion here could only disagree
// with the one that decides the verdict.
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
// serializeType spells every header this library emits in the new format. So
// the only input this refuses that go-crypto would have accepted is valid
// material with an unbounded-framed packet appended behind it.
//
// The version test is v6 alone because the other two versions are bounded by
// their own encoding: v4 and v5 spell both subpacket lengths in two octets, so
// the largest buffer either can name is 65535 bytes, and v5 is refused outright
// under go-crypto's V5Disabled default. Only v6 widened those fields to four
// octets, and only v6 is ungated behind them.
func checkPacketFraming(data []byte) error {
	for len(data) > 0 {
		frame, status := readPacketFrame(data)
		if status == frameUnbounded {
			// Both readers mask the tag to at most six bits, so it renders as a
			// number below 64 and can carry no character able to forge a line or
			// drive a terminal: it needs no sanitizing on its way in here.
			return fmt.Errorf("%w: a tag %d packet carries %s, which declares no body length at all",
				errMalformedSignaturePacket, frame.tag, frame.framing)
		}
		if status != frameBounded {
			return nil
		}

		body := data[frame.headerLen:]
		if frame.bodyLen > int64(len(body)) {
			return fmt.Errorf("%w: a packet declares a %d byte body with %d present",
				errMalformedSignaturePacket, frame.bodyLen, len(body))
		}
		if frame.tag == packetTagSignature {
			if err := checkSignatureSubpacketFraming(body[:frame.bodyLen]); err != nil {
				return err
			}
		}

		data = body[frame.bodyLen:]
	}

	return nil
}

// frameStatus is what readPacketFrame made of the octets at the front of the
// walk's remaining bytes.
type frameStatus int

const (
	// frameUnreadable means the octets are not a header this walk can read, so
	// there is nothing here to judge and nothing behind it to reach.
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
// which header shapes reach frameUnreadable and frameUnbounded, and why only
// one of the two is safe to end the walk on.
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

// checkSignatureSubpacketFraming refuses a v6 signature packet whose four-octet
// subpacket lengths name more bytes than the packet body holds. Every other
// signature version returns nil, for the encoding reason checkPacketFraming's
// own doc comment gives.
func checkSignatureSubpacketFraming(body []byte) error {
	if len(body) < v6SignaturePrefixLen || body[0] != signatureVersionV6 {
		return nil
	}

	rest := body[v6SignaturePrefixLen:]
	hashed := int64(binary.BigEndian.Uint32(body[v6HashedLenOffset:v6SignaturePrefixLen]))
	if hashed > int64(len(rest)) {
		return subpacketOverrun("hashed", hashed, len(rest))
	}

	// The unhashed length sits immediately behind the hashed subpackets. A body
	// too short to carry it is left to go-crypto, which fails the read into a
	// four-byte stack buffer rather than into an allocation.
	rest = rest[hashed:]
	if len(rest) < subpacketLenSize {
		return nil
	}
	unhashed := int64(binary.BigEndian.Uint32(rest[:subpacketLenSize]))
	rest = rest[subpacketLenSize:]
	if unhashed > int64(len(rest)) {
		return subpacketOverrun("unhashed", unhashed, len(rest))
	}

	return nil
}

// subpacketOverrun renders the refusal for one over-declared subpacket length,
// naming which of the two it was and both numbers, so an operator reading the
// failure can tell a truncated blob from a fabricated length.
func subpacketOverrun(which string, declared int64, available int) error {
	return fmt.Errorf("%w: a v6 signature declares %d bytes of %s subpackets with %d present",
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
