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
)

// errMalformedSignaturePacket is the verdict for a packet stream whose framing
// declares more bytes than the stream holds.
//
// It is deliberately absent from classify's arms, so it lands on the default
// one and becomes ERRSIG - the status go-crypto's own unexpected EOF already
// produces for every input this gate refuses. That keeps the gate a change of
// cost rather than a change of verdict, which matters because the ignore set an
// operator configures is keyed on the status: a gate that moved these inputs to
// another status would silently take an existing configuration out of, or into,
// scope.
var errMalformedSignaturePacket = errors.New("OpenPGP packet declares more bytes than the blob holds")

// checkPacketFraming walks data's OpenPGP packet framing once and refuses a
// stream that declares more bytes than it holds.
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
// It judges declared lengths and nothing else. A header this walk cannot
// follow - an octet with no MSB, an old-format indeterminate length, a
// new-format partial length, or a length field running off the end of the
// input - ends the walk with a nil error rather than a refusal, because
// go-crypto refuses each of those itself and cheaply, and a second opinion
// here could only disagree with the one that decides the verdict. So a nil
// return means "nothing here declares an allocation the bytes cannot back",
// never "this is a signature".
//
// The version test is v6 alone because the other two versions are bounded by
// their own encoding: v4 and v5 spell both subpacket lengths in two octets, so
// the largest buffer either can name is 65535 bytes, and v5 is refused outright
// under go-crypto's V5Disabled default. Only v6 widened those fields to four
// octets, and only v6 is ungated behind them.
func checkPacketFraming(data []byte) error {
	for len(data) > 0 {
		frame, ok := readPacketFrame(data)
		if !ok {
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

// packetFrame is one packet header as the walk read it: where its body starts,
// how long the header says that body is, and which packet type it introduces.
type packetFrame struct {
	bodyLen   int64
	headerLen int
	tag       int
}

// readPacketFrame reads the packet header at the front of data. The second
// result is false for every header shape this walk deliberately declines to
// follow, which checkPacketFraming's own doc comment enumerates.
func readPacketFrame(data []byte) (packetFrame, bool) {
	first := data[0]
	if first&packetHeaderMSB == 0 {
		return packetFrame{}, false
	}
	if first&packetNewFormatBit == 0 {
		return oldFormatFrame(data)
	}

	return newFormatFrame(data)
}

// oldFormatFrame reads an old-format header, whose two lowest bits name how
// many octets spell the body length.
func oldFormatFrame(data []byte) (packetFrame, bool) {
	tag := int((data[0] & packetTagMask) >> oldFormatTagShift)

	switch data[0] & oldFormatLengthTypeMask {
	case oldFormatOneOctet:
		if len(data) < tagOctet+lenFieldOne {
			return packetFrame{}, false
		}

		return packetFrame{tag: tag, headerLen: tagOctet + lenFieldOne, bodyLen: int64(data[tagOctet])}, true
	case oldFormatTwoOctet:
		if len(data) < tagOctet+lenFieldTwo {
			return packetFrame{}, false
		}
		bodyLen := int64(binary.BigEndian.Uint16(data[tagOctet : tagOctet+lenFieldTwo]))

		return packetFrame{tag: tag, headerLen: tagOctet + lenFieldTwo, bodyLen: bodyLen}, true
	case oldFormatFourOctet:
		if len(data) < tagOctet+lenFieldFour {
			return packetFrame{}, false
		}
		bodyLen := int64(binary.BigEndian.Uint32(data[tagOctet : tagOctet+lenFieldFour]))

		return packetFrame{tag: tag, headerLen: tagOctet + lenFieldFour, bodyLen: bodyLen}, true
	default:
		// The indeterminate length type: the body runs to the end of the input,
		// so there is no declared length to compare anything against.
		return packetFrame{}, false
	}
}

// newFormatFrame reads a new-format header, whose length is spelled in one, two
// or five octets - or is partial, and declares no total at all.
func newFormatFrame(data []byte) (packetFrame, bool) {
	if len(data) < tagOctet+lenFieldOne {
		return packetFrame{}, false
	}
	tag := int(data[0] & packetTagMask)
	lead := data[tagOctet]

	switch {
	case lead < newFormatOneOctetMax:
		return packetFrame{tag: tag, headerLen: tagOctet + lenFieldOne, bodyLen: int64(lead)}, true
	case lead < newFormatPartialMin:
		if len(data) < tagOctet+lenFieldTwo {
			return packetFrame{}, false
		}
		// RFC 9580's two-octet form, which encodes lengths from 192 upwards.
		bodyLen := int64(lead-newFormatOneOctetMax)*octetRange + int64(data[tagOctet+lenFieldOne]) + newFormatOneOctetMax

		return packetFrame{tag: tag, headerLen: tagOctet + lenFieldTwo, bodyLen: bodyLen}, true
	case lead < newFormatFiveOctetMarker:
		// A partial body length introduces a chunked packet, so this octet
		// declares one chunk rather than the body.
		return packetFrame{}, false
	default:
		if len(data) < tagOctet+lenFieldFive {
			return packetFrame{}, false
		}
		bodyLen := int64(binary.BigEndian.Uint32(data[tagOctet+lenFieldOne : tagOctet+lenFieldFive]))

		return packetFrame{tag: tag, headerLen: tagOctet + lenFieldFive, bodyLen: bodyLen}, true
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
func decodeArmorBlock(data []byte) ([]byte, string, error) {
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

// armorDecodeFoundNothing reports whether err is decodeArmorBlock's answer for
// input holding no armor block at all, which armor.Decode spells as io.EOF.
//
// The test is unambiguous even though decodeArmorBlock has two failing steps:
// bytes.Buffer.ReadFrom reports a clean end of input as a nil error, so io.EOF
// can only have come from the decode.
func armorDecodeFoundNothing(err error) bool {
	return errors.Is(err, io.EOF)
}
