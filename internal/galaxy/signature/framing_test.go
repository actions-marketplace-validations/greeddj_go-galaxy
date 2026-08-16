package signature

import (
	"bytes"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

const (
	// framingAllocCeiling is what a refusal of an amplifying blob may cost,
	// whichever header shape carried it. The ungated library call allocates
	// 4294969120 bytes (4.00 GiB) for ten such bytes, measured through
	// openpgp.CheckDetachedSignature; the gated path allocates the error value
	// and the buffers around the call and nothing else. A ceiling four thousand
	// times below the ungated figure is far enough above real noise to survive
	// the race detector and far enough below the defect to be nowhere near it.
	framingAllocCeiling = 1 << 20 // 1 MiB

	// keyringFileMode is the mode the temporary keyrings below are written
	// with.
	keyringFileMode = 0o600
)

// framingCase is one row of TestCheckPacketFraming: the bytes to walk, the tag
// set to walk them under, and whether the walk must refuse them.
type framingCase struct {
	name       string
	data       []byte
	profile    packetProfile
	wantRefuse bool
}

// framingCases is the gate's whole predicate, one row per answer it can give.
//
// The predicate has seven refusing arms and the rows cover six: a length
// declared over the bytes actually present, a packet framed so that this walk
// can find no end to its body at all, a packet whose tag has no place in the
// stream the row is walked under, a packet carrying secret key material, bytes
// that are not a packet header this walk can read, and a packet the parser reads
// only part of. The seventh is the packet ceiling, which has a test of its own
// because its positive control is a stream sitting exactly at the ceiling rather
// than any of these shapes.
//
// Both unbounded spellings appear twice, once on the signature tag and once off
// it, and they still say "on any tag" rather than "on a tag this profile happens
// to exclude", because the unbounded arm is checked ahead of the tag arm - an
// order TestUnboundedFramingIsJudgedBeforeTheTag pins on the bytes of one of
// those very rows. Two rows carry identical bytes under the two different
// profiles and get opposite answers, which is what says the profile is a
// parameter of the verdict rather than decoration.
//
// The accepting rows are load-bearing in three ways: real key material must
// pass, or the gate would refuse every collection; an empty stream must pass,
// since a walk that never begins is not a stream ending off a packet boundary;
// and a packet the parser consumes to its end must pass whether or not the
// parser could make sense of it, or the gate would start refusing material
// go-crypto skips and reads on from.
//
//nolint:gochecknoglobals // a fixed table consumed by one test, not mutable shared state
var framingCases = []framingCase{
	{
		name:       "a v6 signature declaring 4 GiB of hashed subpackets",
		data:       synthesizedBlobs[amplifyingBlob],
		profile:    keyringProfile(),
		wantRefuse: true,
	},
	{
		// The hashed length is honest and the unhashed one is not, so the walk
		// has to reach past the hashed subpackets to see it.
		name:       "a v6 signature declaring 4 GiB of unhashed subpackets",
		data:       []byte{0xc2, 0x0d, 0x06, 0x13, 0x01, 0x08, 0x00, 0x00, 0x00, 0x01, 0x2a, 0xff, 0xff, 0xff, 0xff},
		profile:    keyringProfile(),
		wantRefuse: true,
	},
	{
		// A packet body longer than the bytes behind it, whatever the packet
		// turns out to be.
		name:       "a packet declaring a body longer than the input",
		data:       []byte{0xc2, 0xff, 0xff, 0xff, 0xff, 0xff, 0x06},
		profile:    keyringProfile(),
		wantRefuse: true,
	},
	{
		// The same four octets under a v4 version octet, where they are not one
		// length but two fields: v4 spells the hashed length in the first two,
		// so this declares 65535 bytes of hashed subpackets with two present.
		// The bytes present are what bounds it, at this version as at any other,
		// which is why the row refuses rather than resting on 65535 being a
		// small number - measured, go-crypto allocates 67360 for these ten bytes.
		name:       "a v4 signature carrying the same octets",
		data:       []byte{0xc2, 0x08, 0x04, 0x13, 0x01, 0x08, 0xff, 0xff, 0xff, 0xff},
		profile:    keyringProfile(),
		wantRefuse: true,
	},
	{
		// Nothing about this is a packet header, which go-crypto reports as a
		// tag byte without its MSB. The walk refuses the stream rather than
		// ending on it: go-crypto recovers past bytes it cannot read and this
		// walk cannot, so ending here with a nil error hands everything behind
		// these bytes to a reader nothing has looked at them for.
		name:       "bytes that are not a packet header",
		data:       []byte("-----BEGIN PGP PUBLIC KEY BLOCK-----\n"),
		profile:    keyringProfile(),
		wantRefuse: true,
	},
	{
		// An old-format header whose length type is 3: the body runs to the end
		// of the input and no octet names its length, so this walk can bound
		// neither that body nor anything behind it. A body it cannot bound is
		// refused rather than followed, because go-crypto accepts the framing
		// and keeps parsing where this walk had to stop.
		name:       "an old-format packet with an indeterminate length",
		data:       []byte{0x8b, 0x06, 0x13, 0x01, 0x08, 0xff, 0xff, 0xff, 0xff},
		profile:    keyringProfile(),
		wantRefuse: true,
	},
	{
		// A new-format partial length introduces a chunked body, so its octet
		// sizes one chunk rather than the packet and nothing declares a total.
		// Refused for the reason the row above is, and the tag it sits on is no
		// part of that reason: what the walk cannot do is find the end of this
		// packet.
		name:       "a new-format packet with a partial length",
		data:       []byte{0xc2, 0xe1, 0x06, 0x13, 0x01, 0x08, 0xff, 0xff, 0xff, 0xff},
		profile:    keyringProfile(),
		wantRefuse: true,
	},
	{
		// The same partial length on tag 1, carrying no signature body at all.
		// The walk still cannot find this packet's end, so the refusal is the
		// framing's and the two rows above are two points of one predicate
		// rather than a rule about signatures.
		name:       "a new-format partial length off the signature tag",
		data:       []byte{0xc1, 0xe0, 0x00, 0x00},
		profile:    keyringProfile(),
		wantRefuse: true,
	},
	{
		// And the other unbounded spelling off the signature tag: one octet
		// naming tag 1 and length type 3, with a zero body behind it. Refused
		// for the same reason, from a header a byte long.
		name:       "an old-format indeterminate length off the signature tag",
		data:       []byte{0x87, 0x00},
		profile:    keyringProfile(),
		wantRefuse: true,
	},
	{
		// A marker packet, which is tag 10 in no set: it belongs to neither a
		// keyring nor a detached signature, and it is what a carrier packet
		// ahead of real material is written as.
		name:       "a marker packet in a keyring",
		data:       []byte{0xca, 0x03, 0x50, 0x47, 0x50},
		profile:    keyringProfile(),
		wantRefuse: true,
	},
	{
		// An empty public key packet: the parser fails it and consumes it, so
		// the walk has nothing against it in a file made of key packets.
		name:    "an empty public key packet in a keyring",
		data:    []byte{0xc6, 0x00},
		profile: keyringProfile(),
	},
	{
		// The same packet on the tag one bit below it, which is the secret key
		// packet. The pair is the secret-key arm's own control: identical bytes
		// but for the tag, one accepted and one refused, so the refusal is that
		// tag rather than anything about an empty body.
		name:       "an empty secret key packet in a keyring",
		data:       []byte{0xc5, 0x00},
		profile:    keyringProfile(),
		wantRefuse: true,
	},
	{
		// And its subkey spelling, which no exporter writes without the packet
		// above it - refused on its own terms all the same, since a rule that
		// covered only the primary is one a file steps out of by dropping it.
		name:       "an empty secret subkey packet in a keyring",
		data:       []byte{0xc7, 0x00},
		profile:    keyringProfile(),
		wantRefuse: true,
	},
	{
		// A v6 signature whose four-octet hashed length is spelled in its low
		// two octets alone: 256 bytes declared over the none behind it. The high
		// octets are zero deliberately - every other v6 row here declares
		// 0xffffffff, whose low half over-declares too, so this is the row that
		// says the width read is four octets rather than two.
		name:       "a v6 signature declaring 256 bytes of hashed subpackets",
		data:       []byte{0xc2, 0x08, 0x06, 0x13, 0x01, 0x08, 0x00, 0x00, 0x01, 0x00},
		profile:    signatureBlobProfile(),
		wantRefuse: true,
	},
	{
		// A hashed area holding one subpacket whose own length is zero, so its
		// contents are empty - not the type-octet-and-no-body shape the walk
		// refuses, but the one an octet shorter. The walk stops there with a nil
		// answer, and go-crypto refuses the identical subpacket as a zero length
		// signature subpacket before it reads the unhashed length, so nothing
		// behind it is reached either.
		name:    "a signature subpacket of no length at all",
		data:    []byte{0xc2, 0x08, 0x04, 0x13, 0x01, 0x08, 0x00, 0x02, 0x00, 0x00},
		profile: signatureBlobProfile(),
	},
	{
		// The identical bytes in a detached signature, where a key packet has
		// no place. This row and the one above it differ in the set alone.
		name:       "the same packet in a signature blob",
		data:       []byte{0xc6, 0x00},
		profile:    signatureBlobProfile(),
		wantRefuse: true,
	},
	{
		// GnuPG's ring-trust packet, which a raw pubring.gpg carries and which
		// go-crypto never parses at all - it errors on the tag and consumes the
		// body, so the walk finds nothing unread.
		name:    "a ring-trust packet in a keyring",
		data:    []byte{0xcc, 0x02, 0x00, 0x00},
		profile: keyringProfile(),
	},
	{
		// A public key packet whose parse SUCCEEDS while reading less than its
		// header declared: version 4, a creation time, RSA, and two 8-bit MPIs,
		// framed as fifteen body bytes so three go unread. Nothing here declares
		// an allocation it cannot back, so a walk of declared lengths alone
		// accepts it - and leaves go-crypto's own reader three bytes into a body
		// it thinks is the next packet's header.
		name: "a public key packet the parser under-reads",
		data: []byte{
			0xc6, 0x0f, 0x04, 0x00, 0x00, 0x00, 0x00, 0x01,
			0x00, 0x08, 0xff, 0x00, 0x08, 0x03, 0xaa, 0xbb, 0xcc,
		},
		profile:    keyringProfile(),
		wantRefuse: true,
	},
	{
		// A header cut off before its own length field: there is no declared
		// length to read, let alone to compare, so the walk can locate neither
		// this packet's end nor anything behind it. Refused for the reason the
		// row above is - it is the same arm, reached from the other of its two
		// header shapes.
		name:       "a header truncated inside its length field",
		data:       []byte{0xc2, 0xff, 0x00},
		profile:    keyringProfile(),
		wantRefuse: true,
	},
	// The boundary those two rows sit against, and the reason this row stays:
	// an empty input never enters the walk at all, so a stream of no bytes is
	// not a stream ending off a packet boundary. It is the positive control for
	// that arm rather than a row about a tag or a length.
	{name: "no bytes at all", data: nil, profile: keyringProfile()},
}

// TestCheckPacketFraming walks the gate's predicate row by row.
//
// The refusals are what the gate exists for; the acceptances are what keeps it
// from being a second parser with an opinion of its own. A nil answer means
// only that every packet is one this caller may hold, backs what it declares,
// and is read to its end - never that any of it is a signature, which is why
// rows that are plainly not one still sit under it.
func TestCheckPacketFraming(t *testing.T) {
	t.Parallel()

	// The positive control for the whole table, on real key material rather
	// than on a hand-built shape: a committed keyring walks clean, so a
	// refusing row is its own bytes and not a gate that refuses everything.
	if err := checkPacketFraming(readFixture(t, binaryFixture), keyringProfile()); err != nil {
		t.Fatalf("positive control: checkPacketFraming(%s) = %v, want nil", binaryFixture, err)
	}

	for _, tc := range framingCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := checkPacketFraming(tc.data, tc.profile)
			// Killing mutation, actually run against this file: delete the
			// allow-list arm from judgeHeader, so a tag decides nothing. Exactly
			// the two rows built for it fail:
			//
			//	framing_test.go:301: checkPacketFraming(a marker packet in a keyring) refused = false (err <nil>), want refused = true
			//	framing_test.go:301: checkPacketFraming(the same packet in a signature blob) refused = false (err <nil>), want refused = true
			//
			// One measurement further down fails alongside them, and it did not
			// before this walk stopped driving tag 10:
			// TestEveryCarrierIsRefusedCheaplyAtBothEntryPoints names the marker
			// carrier through all four of its entry points, at about 4 GiB
			// apiece. So these rows are the allow-list's verdict and that one is
			// its cost, rather than either being a second spelling of the other.
			if got := errors.Is(err, errMalformedSignaturePacket); got != tc.wantRefuse {
				t.Fatalf("checkPacketFraming(%s) refused = %t (err %v), want refused = %t", tc.name, got, err, tc.wantRefuse)
			}
		})
	}
}

// packetCeilingCase is one row of TestPacketCeilingIsPerProfile: how many
// packets to build, the profile to walk them under, and whether that profile
// must refuse them.
type packetCeilingCase struct {
	name       string
	profile    packetProfile
	packets    int
	wantRefuse bool
}

// packetCeilingCases states each profile's ceiling as a boundary rather than as
// a direction: a stream of exactly maxPackets is walked and one packet more is
// refused, at both profiles.
//
// The two accepting rows are the positive controls, and they are one packet away
// from the refusing row beside them, so a gate that refused every long stream
// would fail them. The middle row is the second control and the one that says
// the ceiling is the profile's rather than the walk's: the identical 65 packets
// refused as a signature blob are accepted as a keyring.
func packetCeilingCases() []packetCeilingCase {
	return []packetCeilingCase{
		{name: "a blob at its ceiling", profile: signatureBlobProfile(), packets: signatureBlobMaxPackets},
		{name: "a blob one packet past it", profile: signatureBlobProfile(), packets: signatureBlobMaxPackets + 1, wantRefuse: true},
		{name: "the same packets in a keyring", profile: keyringProfile(), packets: signatureBlobMaxPackets + 1},
		{name: "a keyring at its ceiling", profile: keyringProfile(), packets: keyringMaxPackets},
		{name: "a keyring one packet past it", profile: keyringProfile(), packets: keyringMaxPackets + 1, wantRefuse: true},
	}
}

// TestPacketCeilingIsPerProfile pins the second term of the drive's bound.
//
// The tag set closes which of go-crypto's parsers a stream can reach; this
// closes how many times it can reach one, which is the term the input chooses
// and the one no measurement of a fixture can bound. Every row is built from the
// packet a stream buys most cheaply - a new-format signature header naming an
// empty body, two bytes - so the rows differ in their packet count and in
// nothing else.
//
// Parallel: it counts verdicts rather than bytes allocated, so nothing running
// alongside it is counted in its answer.
func TestPacketCeilingIsPerProfile(t *testing.T) {
	t.Parallel()

	// The whole message hand-spelled, and apart from the production format
	// string and the constant it names, so that a mutation to either cannot
	// reshape the expectation into agreeing with it.
	const wantBlob = "malformed OpenPGP packet framing: more than 64 packets across the whole input"

	for _, tc := range packetCeilingCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			stream := repeat([]byte{0xc2, 0x00}, tc.packets*2)
			err := checkPacketFraming(stream, tc.profile)
			// Killing mutation, actually run against this file: change the
			// ceiling test from packets > profile.maxPackets to packets >
			// profile.maxPackets+1, which is the off-by-one a reader cannot see
			// by eye. The two refusing rows then fail, one of them with
			//
			//	framing_test.go:371: checkPacketFraming(a blob one packet past it, 65 packets) refused = false (err <nil>), want refused = true
			//
			// while the accepting rows pass throughout, which is what makes the
			// pair a boundary rather than a direction.
			if got := errors.Is(err, errMalformedSignaturePacket); got != tc.wantRefuse {
				t.Fatalf("checkPacketFraming(%s, %d packets) refused = %t (err %v), want refused = %t",
					tc.name, tc.packets, got, err, tc.wantRefuse)
			}
			// Which arm refused it, asked on the one row whose ceiling is small
			// enough to spell: a stream refused as malformed by some other arm
			// would pass the assertion above and fail here.
			if tc.wantRefuse && tc.profile.maxPackets == signatureBlobMaxPackets && err.Error() != wantBlob {
				t.Fatalf("checkPacketFraming(%s) = %v, want %q", tc.name, err, wantBlob)
			}
		})
	}
}

const (
	// budgetBlockCopies is how many copies of the binary key export each block of
	// the two-block keyring below carries. The export is three packets, so a
	// block is 2049 of them - inside keyringMaxPackets on its own, and over it
	// once the second block is counted with it.
	budgetBlockCopies = 683
	// budgetBlockPackets is what that comes to, hand-multiplied rather than
	// derived from the fixture, so a fixture that stopped being three packets
	// fails the count below instead of quietly restating itself.
	budgetBlockPackets = 2049
	// binaryFixturePackets is that three, named so the arithmetic above has
	// something to be checked against.
	binaryFixturePackets = 3
)

// TestArmorBlocksShareOnePacketBudget pins the unit the packet ceiling is stated
// over: the keyring file, not the armor block.
//
// readArmoredKeyRing reads every block in a file, so a ceiling counted afresh
// per block bounds a block and nothing else - and the number of blocks a file
// may carry is bounded only by its size. Measured before the budget was carried
// across them, a file of two blocks of 2049 packets each loaded with a nil error
// and 1366 entities, 4098 packets against a ceiling of 4096.
//
// The block count is asserted before anything is loaded, and that is not
// ceremony: two armored blocks concatenated without a newline between them read
// as ONE block, since the first block's end line and the second's opening line
// land on one line. A measurement over two blocks that silently ran over one
// would report the budget working while proving nothing.
//
// The single-block load is the positive control, on the same bytes: one block of
// this file loads, so the refusal is the pair being counted together rather than
// a block that was over the ceiling by itself.
//
// Parallel: it counts verdicts and entities rather than bytes allocated, so
// nothing running alongside it is counted in its answer.
func TestArmorBlocksShareOnePacketBudget(t *testing.T) {
	t.Parallel()

	unit := readFixture(t, binaryFixture)
	if got := countBoundedPackets(unit); got != binaryFixturePackets {
		t.Fatalf("%s holds %d packets, want %d: the arithmetic below is stated over that number",
			binaryFixture, got, binaryFixturePackets)
	}
	if budgetBlockCopies*binaryFixturePackets != budgetBlockPackets {
		t.Fatalf("%d copies of %d packets is not %d", budgetBlockCopies, binaryFixturePackets, budgetBlockPackets)
	}
	if budgetBlockPackets > keyringMaxPackets || 2*budgetBlockPackets <= keyringMaxPackets {
		t.Fatalf("a block of %d packets is not one that fits alone and overruns in a pair against a ceiling of %d",
			budgetBlockPackets, keyringMaxPackets)
	}

	block := armorEncode(t, openpgp.PublicKeyType, bytes.Repeat(unit, budgetBlockCopies))
	// The newline is what keeps the two blocks two: armor.Encode ends a block
	// without one, so concatenating them directly glues the end line of the first
	// to the opening line of the second.
	file := make([]byte, 0, 2*len(block)+2)
	file = append(file, block...)
	file = append(file, '\n')
	file = append(file, block...)
	file = append(file, '\n')

	if got := countArmorBlocks(file); got != 2 {
		t.Fatalf("the two-block fixture presents %d armor blocks, want 2", got)
	}

	entities, err := readEntities(block)
	if err != nil || len(entities) != budgetBlockCopies {
		t.Fatalf("positive control: readEntities(one block) = %d entities, %v, want %d and nil",
			len(entities), err, budgetBlockCopies)
	}

	_, err = readEntities(file)
	// Killing mutation, actually run against this file: have readArmoredKeyRing
	// pass 0 rather than spent to readKeyArmorBlock, which is the per-block count
	// this replaced. This assertion then fails with
	//
	//	framing_test.go:466: readEntities(two blocks of 2049 packets) = <nil>, want the malformed-packet refusal
	//
	// and nothing else in the package notices, since every committed fixture is
	// three orders of magnitude below the ceiling.
	if !errors.Is(err, errMalformedSignaturePacket) {
		t.Fatalf("readEntities(two blocks of %d packets) = %v, want the malformed-packet refusal", budgetBlockPackets, err)
	}
}

// countBoundedPackets reports how many bounded packet frames data holds, stopping
// where the walk can no longer read one.
func countBoundedPackets(data []byte) int {
	packets := 0
	for len(data) > 0 {
		frame, status := readPacketFrame(data)
		if status != frameBounded || frame.bodyLen > int64(len(data)-frame.headerLen) {
			return packets
		}
		packets++
		data = data[int64(frame.headerLen)+frame.bodyLen:]
	}

	return packets
}

// countArmorBlocks reports how many opening lines data carries, cut the way
// readArmoredKeyRing cuts them.
func countArmorBlocks(data []byte) int {
	blocks := 0
	for off := 0; off < len(data); {
		line, next := nextLine(data, off)
		if isArmorBlockStart(line) {
			blocks++
		}
		off = next
	}

	return blocks
}

// blobProfileCase is one input reaching checkOne, and the whole refusal the blob
// profile owes it.
type blobProfileCase struct {
	name string
	want string
	data []byte
}

// blobProfileCases are the three refusals that separate the blob profile from
// the keyring one, each spelled whole and apart from the production format
// strings, so that a mutation to one of those cannot reshape the expectation
// into agreeing with it.
//
// Two of the three are about the tag set and one about the ceiling, which is
// what a profile is: a keyring's vocabulary admits key packets and 4096 of them,
// a blob's admits signature packets and 64, and checkOne passing the wrong one
// would take all three refusals away at once.
func blobProfileCases(t *testing.T) []blobProfileCase {
	t.Helper()

	// A real public key export offered as a detached signature, which is the
	// shape an operator produces by naming the wrong file.
	keyExport := readFixture(t, binaryFixture)
	// A key packet ahead of a signature that verifies, which is the shape an
	// attacker produces: under the keyring vocabulary the leading packet is
	// admitted and the signature behind it is still checked.
	prefixed := make([]byte, 0, len(keyExport)+len(readFixture(t, sigValidBinary)))
	prefixed = append(prefixed, keyExport...)
	prefixed = append(prefixed, readFixture(t, sigValidBinary)...)

	return []blobProfileCase{
		{
			name: "a public key export offered as a signature",
			data: keyExport,
			want: "malformed OpenPGP packet framing: a tag 6 packet has no place in this stream",
		},
		{
			name: "a key packet ahead of a signature that verifies",
			data: prefixed,
			want: "malformed OpenPGP packet framing: a tag 6 packet has no place in this stream",
		},
		{
			name: "65 empty signature packets",
			data: repeat([]byte{0xc2, 0x00}, 2*(signatureBlobMaxPackets+1)),
			want: "malformed OpenPGP packet framing: more than 64 packets across the whole input",
		},
	}
}

// TestCheckOnePassesTheBlobProfile pins which profile the untrusted entry point
// hands the gate.
//
// Everything else here calls checkPacketFraming with a profile of the test's own
// choosing, so the one thing none of it can see is checkOne passing the wrong
// one - a keyring profile there admits key packets into a blob and raises the
// packet ceiling by two orders of magnitude, and every existing measurement
// still passes, since the carriers are all signature packets a keyring may hold
// too.
//
// Parallel: it compares messages rather than bytes allocated, so nothing running
// alongside it is counted in its answer.
func TestCheckOnePassesTheBlobProfile(t *testing.T) {
	t.Parallel()

	kr := loadTestKeyring(t, keyringFixture)
	manifest := readFixture(t, manifestAFixture)

	// The positive control, through the identical call: a real signature over
	// this manifest still verifies, so a refusal below is the profile rather than
	// a path that refuses everything.
	if _, err := checkOne(manifest, readFixture(t, sigValidBinary), kr); err != nil {
		t.Fatalf("positive control: checkOne(%s) = %v, want nil", sigValidBinary, err)
	}

	for _, tc := range blobProfileCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := checkOne(manifest, tc.data, kr)
			// Killing mutation, actually run against this file: have checkOne
			// pass keyringProfile() instead of signatureBlobProfile(). All three
			// rows then fail, one of them with
			//
			//	framing_test.go:590: checkOne(a key packet ahead of a signature that verifies) = <nil>, want "malformed
			//	OpenPGP packet framing: a tag 6 packet has no place in this stream"
			//
			// which is a blob carrying a key packet ahead of real material being
			// verified rather than refused.
			if err == nil || err.Error() != tc.want {
				t.Fatalf("checkOne(%s) = %v, want %q", tc.name, err, tc.want)
			}
		})
	}
}

// TestUnreadableHeaderNamesTheRemainingBytes pins the arm that closed this
// gate's own bypass, by its message rather than by its sentinel.
//
// Six refusals share errMalformedSignaturePacket, so a row asserting only the
// sentinel says nothing about which one answered. This is the arm where that
// matters most: a walk that ended on unreadable bytes with a nil error handed
// everything behind them to a reader that recovers past them.
//
// The control is the same two bytes with the header MSB set, which is a user id
// packet declaring an empty body and is accepted - so the refusal is that one
// bit rather than anything else about these octets.
//
// Parallel: it allocates one error value and measures nothing.
func TestUnreadableHeaderNamesTheRemainingBytes(t *testing.T) {
	t.Parallel()

	// The whole message hand-spelled, and apart from the production format
	// string it checks, so that a mutation to that string cannot reshape the
	// expectation into agreeing with it.
	const want = "malformed OpenPGP packet framing: 2 bytes are not a packet header this walk can read"

	if err := checkPacketFraming([]byte{0xcd, 0x00}, keyringProfile()); err != nil {
		t.Fatalf("positive control: checkPacketFraming(the same octets with the header MSB) = %v, want nil", err)
	}

	err := checkPacketFraming([]byte{0x4d, 0x00}, keyringProfile())
	// Killing mutation, actually run against this file: have readPacketFrame's own
	// MSB test return frameBounded with a zero body and the tag its octet spells.
	// This assertion then fails with
	//
	//	framing_test.go:634: checkPacketFraming(two bytes without the header MSB) = malformed OpenPGP packet framing: more
	//	than 4096 packets across the whole input, want "malformed OpenPGP packet framing: 2 bytes are not a packet header
	//	this walk can read"
	//
	// which is what an unreadable header costs once nothing names it: the frame
	// carries no header length, so the walk re-reads the same two bytes until the
	// packet ceiling stops it, and the refusal an operator sees is that.
	if err == nil || err.Error() != want {
		t.Fatalf("checkPacketFraming(two bytes without the header MSB) = %v, want %q", err, want)
	}
}

// truncationCase is one input cut off inside a length field or a body, and the
// verdict the walk owes it.
type truncationCase struct {
	name       string
	data       []byte
	wantRefuse bool
}

// truncationCases is one row per guard in this file that tests whether the bytes
// it needs are actually there - seven in the header readers, and six in the
// signature body walk below them.
//
// The two answers are not one rule. A header this walk cannot read whole ends
// the stream off a packet boundary, which is a refusal: the reader behind the
// gate recovers past such bytes and this walk cannot, so an acceptance would
// hand it everything behind them. A signature body cut short inside its own
// length fields is accepted instead, because go-crypto fails those reads into a
// stack buffer rather than into an allocation and then consumes the frame, so
// there is nothing here left to bound - which the drive proves per row rather
// than being taken on trust, since every accepting row below is a tag 2 packet
// and is therefore driven.
func truncationCases() []truncationCase {
	return []truncationCase{
		{name: "a tag octet without the header MSB", data: []byte{0x00}, wantRefuse: true},
		{name: "an old-format one-octet length field absent", data: []byte{0x8c}, wantRefuse: true},
		{name: "an old-format two-octet length field truncated", data: []byte{0x8d, 0x00}, wantRefuse: true},
		{name: "an old-format four-octet length field truncated", data: []byte{0x8e, 0x00, 0x00, 0x00}, wantRefuse: true},
		{name: "a new-format header with no length octet", data: []byte{0xc2}, wantRefuse: true},
		{name: "a new-format two-octet length field truncated", data: []byte{0xc2, 0xc0}, wantRefuse: true},
		{name: "a new-format five-octet length field truncated", data: []byte{0xc2, 0xff, 0x00}, wantRefuse: true},
		// Below here the header is whole and the body is what runs out, so each
		// row is a signature packet framed exactly around the bytes it holds.
		{name: "a signature packet with an empty body", data: []byte{0xc2, 0x00}},
		{name: "a signature body too short for its length prefix", data: []byte{0xc2, 0x03, 0x04, 0x13, 0x01}},
		{
			name: "a signature body with no unhashed length behind its hashed area",
			data: []byte{0xc2, 0x06, 0x04, 0x13, 0x01, 0x08, 0x00, 0x00},
		},
		{
			name: "a subpacket two-octet length field truncated",
			data: []byte{0xc2, 0x07, 0x04, 0x13, 0x01, 0x08, 0x00, 0x01, 0xc0},
		},
		{
			name: "a subpacket five-octet length field truncated",
			data: []byte{0xc2, 0x09, 0x04, 0x13, 0x01, 0x08, 0x00, 0x03, 0xff, 0x00, 0x00},
		},
		{
			name: "a subpacket declaring more than its area holds",
			data: []byte{0xc2, 0x08, 0x04, 0x13, 0x01, 0x08, 0x00, 0x02, 0x05, 0x02},
		},
	}
}

// TestTruncatedFramingVerdicts asserts what each of those guards decides rather
// than only that none of them panics.
//
// Nothing else in this package reaches most of them: the committed corpus is
// whole files, and every hand-built case elsewhere carries a complete header, so
// a guard could be inverted or dropped with the rest of the suite still green.
//
// Parallel: it allocates a handful of small errors and measures nothing.
func TestTruncatedFramingVerdicts(t *testing.T) {
	t.Parallel()

	// The positive control on the same shape as the rows: a signature packet
	// framed around a body that is not truncated at all walks clean, so a
	// refusing row is its own truncation rather than a walk that refuses every
	// hand-built packet.
	whole := []byte{0xc2, 0x0b, 0x04, 0x13, 0x01, 0x08, 0x00, 0x03, 0x02, 0x04, 0x01, 0x00, 0x00}
	if err := checkPacketFraming(whole, signatureBlobProfile()); err != nil {
		t.Fatalf("positive control: checkPacketFraming(a whole signature packet) = %v, want nil", err)
	}

	for _, tc := range truncationCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := checkPacketFraming(tc.data, signatureBlobProfile())
			// Killing mutation, actually run against this file: restore the
			// `if status != frameBounded { return nil }` this walk's loop used to
			// open with, so a header it cannot read ends the walk with a nil
			// error again. All seven refusing rows then fail, one of them with
			//
			//	framing_test.go:726: checkPacketFraming(a tag octet without the header MSB) refused = false (err <nil>), want refused = true
			//
			// while every accepting row passes throughout, which is what says the
			// two answers are two rules rather than one.
			if got := errors.Is(err, errMalformedSignaturePacket); got != tc.wantRefuse {
				t.Fatalf("checkPacketFraming(%s) refused = %t (err %v), want refused = %t", tc.name, got, err, tc.wantRefuse)
			}
		})
	}
}

// fixtureGateCase is one committed fixture, the tag set the entry point that
// really reads it passes to the gate, and whether that gate refuses it as secret
// key material.
type fixtureGateCase struct {
	name       string
	profile    packetProfile
	wantSecret bool
}

// gatedFixtures is every committed fixture that holds OpenPGP packets, under
// the set its own entry point uses.
//
// Running a keyring under the blob set, or a signature under the keyring set,
// would assert something false in the other direction: the sets differ, so a
// fixture accepted under the wrong one says nothing about whether this program
// accepts it.
func gatedFixtures() []fixtureGateCase {
	return []fixtureGateCase{
		{name: sigValidArmored, profile: signatureBlobProfile()},
		{name: sigValidBinary, profile: signatureBlobProfile()},
		{name: sigSecondSigner, profile: signatureBlobProfile()},
		{name: sigOverManifestB, profile: signatureBlobProfile()},
		{name: sigOutsiderKey, profile: signatureBlobProfile()},
		{name: sigExpiredKey, profile: signatureBlobProfile()},
		{name: sigRevokedKey, profile: signatureBlobProfile()},
		{name: sigExpiredSig, profile: signatureBlobProfile()},
		{name: keyringFixture, profile: keyringProfile()},
		{name: outsiderKeyringFixture, profile: keyringProfile()},
		{name: armoredFixture, profile: keyringProfile()},
		{name: binaryFixture, profile: keyringProfile()},
		{name: twoKeyFixture, profile: keyringProfile()},
		{name: secondFixture, profile: keyringProfile()},
		{name: secretPublicFixture, profile: keyringProfile()},
		{name: secretArmoredFixture, profile: keyringProfile(), wantSecret: true},
		{name: secretBinaryFixture, profile: keyringProfile(), wantSecret: true},
		{name: subkeyFixture, profile: keyringProfile()},
	}
}

// TestCheckPacketFramingAcceptsEverySignatureFixture is the acceptance half of
// the gate stated as a property rather than as a row: every signature this
// repository committed, and every keyring holding public material, walks clean
// under the set its own entry point passes.
//
// A gate that refused any of them would refuse ordinary collections, and it
// would do so on the one input class nobody hand-builds a case for. The two
// secret exports are in the sweep for the opposite reason, and they are the only
// rows carrying a wanted refusal: they are real gpg output for the one shape the
// walk refuses on sight, so they say the secret-key arm fires on a file a person
// can actually produce rather than only on hand-spelled octets. Their own
// same-key control is secret-public.asc, the public half of that very key, three
// rows above them.
func TestCheckPacketFramingAcceptsEverySignatureFixture(t *testing.T) {
	t.Parallel()

	// The armored fixtures are decoded first, since the gate runs on decoded
	// bytes and armor text carries no framing to walk.
	for _, tc := range gatedFixtures() {
		data := readFixture(t, tc.name)
		if bytes.Contains(data, []byte(armorBlockStart)) {
			decoded, _, err := decodeArmorBlock(data)
			if err != nil {
				t.Fatalf("decodeArmorBlock(%s) = %v, want nil", tc.name, err)
			}
			data = decoded
		}

		err := checkPacketFraming(data, tc.profile)
		if got := errors.Is(err, errSecretKeyPacket); got != tc.wantSecret {
			t.Fatalf("checkPacketFraming(%s) refused as secret material = %t (err %v), want %t",
				tc.name, got, err, tc.wantSecret)
		}
		if !tc.wantSecret && err != nil {
			t.Fatalf("checkPacketFraming(%s) = %v, want nil", tc.name, err)
		}
	}
}

// TestCheckOneRefusesAnAmplifyingPacketCheaply is the gate measured rather than
// asserted: the refusal has to be cheap, because a refusal that still allocated
// would leave the defect exactly where it was.
//
// The blob is ten bytes and the ungated call allocates 4.00 GiB for it, so
// helpers.SignatureMaxSize bounds nothing here and
// helpers.MaxSignaturesPerCollection of them is 640 bytes of input. The
// assertion is a byte ceiling rather than an allocation count, since the defect
// is a single allocation of a size the input chose.
//
// The test is deliberately not parallel: runtime.MemStats is process-wide, so a
// parallel test allocating alongside it would be counted here.
func TestCheckOneRefusesAnAmplifyingPacketCheaply(t *testing.T) {
	kr := loadTestKeyring(t, keyringFixture)
	manifest := readFixture(t, manifestAFixture)

	// The positive control, through the identical call: this keyring and this
	// manifest do verify a signature, so the refusal below is the blob's own
	// framing and not a path that refuses everything.
	if _, err := checkOne(manifest, readFixture(t, sigValidArmored), kr); err != nil {
		t.Fatalf("positive control: checkOne(%s) = %v, want nil", sigValidArmored, err)
	}

	blob := synthesizedBlobs[amplifyingBlob]

	var err error
	allocated := measureAlloc(func() {
		_, err = checkOne(manifest, blob, kr)
	})
	// Killing mutation, actually run against this file: make
	// checkSignatureBodyFraming return nil unconditionally, which removes the
	// precondition and nothing else - the packet body length check above it is
	// untouched, and this blob's body length is honest, so nothing else in the
	// walk has anything to say about it. The drive behind it then hands the same
	// blob to the parser and spends the allocation inside the gate, which is
	// what makes the ordering of those two steps the whole security property.
	// This assertion then fails with
	//
	//	framing_test.go:852: checkOne(v6 amplification blob) allocated 8589943504 bytes, want at most 1048576
	//
	// which is the defect itself, in bytes.
	if allocated > framingAllocCeiling {
		t.Fatalf("checkOne(v6 amplification blob) allocated %d bytes, want at most %d", allocated, framingAllocCeiling)
	}
	// Which refusal produced that cheap outcome. It sits below the measurement
	// because the measurement is what the gate is for; a mutation that refused
	// this blob cheaply under some other error would fail here and pass above.
	if !errors.Is(err, errMalformedSignaturePacket) {
		t.Fatalf("checkOne(v6 amplification blob) = %v, want the malformed-packet refusal", err)
	}
}

// TestLoadKeyringRefusesAnAmplifyingPacketCheaply is the same measurement on
// the keyring path, in both encodings.
//
// The keyring is the operator's own file rather than a value from a trust
// boundary, so this is the weaker of the two cases - but it reaches the
// identical parser through the identical packets, and closing one and not the
// other would leave a reader guessing which. The armored row is the one that
// matters structurally: it is the row a gate applied to raw file bytes rather
// than to decoded ones would let through.
//
// A sentinel assertion alone would prove nothing here. Both files are refused
// with or without the gate - ungated, go-crypto answers unexpected EOF after
// the allocation - so the allocation ceiling is the whole of what this pins.
//
// Not parallel, for the reason the test above gives.
func TestLoadKeyringRefusesAnAmplifyingPacketCheaply(t *testing.T) {
	dir := t.TempDir()
	blob := synthesizedBlobs[amplifyingBlob]
	armored := armorEncode(t, openpgp.PublicKeyType, blob)

	// The control is a real keyring written into the same directory, so a
	// refusal below is the blob and not this directory or this test's writes.
	controlPath := filepath.Join(dir, "control.asc")
	// #nosec G703 -- dir is this test's own t.TempDir and the leaf is a
	// constant; no part of the path comes from outside this function.
	if err := os.WriteFile(controlPath, readFixture(t, armoredFixture), keyringFileMode); err != nil {
		t.Fatalf("write control keyring: %v", err)
	}
	if _, err := LoadKeyring(controlPath); err != nil {
		t.Fatalf("positive control: LoadKeyring(control.asc) = %v, want nil", err)
	}

	for _, tc := range []struct {
		name string
		data []byte
	}{
		{name: "binary.gpg", data: blob},
		{name: "armored.asc", data: armored},
	} {
		path := filepath.Join(dir, tc.name)
		// #nosec G703 -- same t.TempDir and a leaf from this function's own
		// fixed table, as just above.
		if err := os.WriteFile(path, tc.data, keyringFileMode); err != nil {
			t.Fatalf("write %s: %v", tc.name, err)
		}

		var loadErr error
		allocated := measureAlloc(func() {
			_, loadErr = LoadKeyring(path)
		})
		if !errors.Is(loadErr, helpers.ErrKeyringUnreadable) {
			t.Fatalf("LoadKeyring(%s) error = %v, want the unreadable sentinel", tc.name, loadErr)
		}
		// Killing mutation, actually run against this file: replace
		// readKeyArmorBlock's body with a call to
		// openpgp.ReadArmoredKeyRing(bytes.NewReader(data)), which is the
		// decode and the read with no gate between them. Only its armored row
		// then fails, with
		//
		//	framing_test.go:927: LoadKeyring(armored.asc) allocated 4294978576 bytes, want at most 1048576
		//
		// while its sentinel assertion above still passes: the file is refused
		// either way, and what the gate removes is the 4 GiB spent reaching
		// that refusal.
		if allocated > framingAllocCeiling {
			t.Fatalf("LoadKeyring(%s) allocated %d bytes, want at most %d", tc.name, allocated, framingAllocCeiling)
		}
	}
}

// TestAmplifyingFramingsAreRefusedCheaply measures the gate over five ways one
// v6 signature body reaches go-crypto's four-octet subpacket allocation, each
// through the entry point it actually arrives by.
//
// Every row carries the identical eight bytes of body and differs only in the
// header in front of it, the encoding around both, and the entry point it
// arrives by, which is the whole reason this is a table: the body is what
// allocates, and a gate that judged one header shape while waving the rest
// through leaves the defect reachable by rewriting two octets. Both entry points
// are covered because a keyring reaches the parser through one and a detached
// signature through the other, and neither routes through the other. The two
// armored rows are the structural ones: they are what a gate applied to raw file
// bytes rather than to decoded ones would let through.
//
// The ceiling is the assertion that carries the test, since the defect is a
// single allocation of a size the input chose. The refusal's identity is
// asserted below it and is the cheaper question: ungated, go-crypto refuses
// every row here too, answering unexpected EOF after the allocation, so what
// the gate removes is the 4 GiB spent reaching the same verdict.
//
// Not parallel: runtime.MemStats is process-wide, so a parallel test allocating
// alongside it would be counted here.
func TestAmplifyingFramingsAreRefusedCheaply(t *testing.T) {
	kr := loadTestKeyring(t, keyringFixture)
	manifest := readFixture(t, manifestAFixture)
	requireFramingFixturesStillWork(t, kr, manifest)

	// The v6 signature body every row below carries: version 6, signature type
	// 0x13, RSA, SHA-256, and a four-octet hashed subpacket length of
	// 0xffffffff. Only the header in front of it, and the encoding around both,
	// change.
	body := []byte{0x06, 0x13, 0x01, 0x08, 0xff, 0xff, 0xff, 0xff}
	// 0xc2 is the new format on tag 2. 0x08 behind it is an ordinary one-octet
	// length naming the body, the shape the length comparison judges; 0xe6 is a
	// partial length, sizing one 64-byte chunk rather than the packet and naming
	// no total at all. 0x8b is the old format on tag 2 with length type 3, so
	// the body instead runs to the end of the input and again no octet names its
	// length.
	declared := append([]byte{0xc2, 0x08}, body...)
	partial := append([]byte{0xc2, 0xe6}, body...)
	indeterminate := append([]byte{0x8b}, body...)

	dir := t.TempDir()
	for _, tc := range []struct {
		name string
		leaf string
		data []byte
	}{
		{name: "a new-format declared length", leaf: "declared.gpg", data: declared},
		{name: "an old-format indeterminate length", leaf: "indeterminate.gpg", data: indeterminate},
		{name: "an armored public key block", leaf: "indeterminate.asc", data: armorEncode(t, openpgp.PublicKeyType, indeterminate)},
	} {
		path := filepath.Join(dir, tc.leaf)
		// #nosec G703 -- dir is this test's own t.TempDir and the leaf comes
		// from this function's own fixed table; no part of the path comes from
		// outside this function.
		if err := os.WriteFile(path, tc.data, keyringFileMode); err != nil {
			t.Fatalf("write %s: %v", tc.leaf, err)
		}

		var loadErr error
		allocated := measureAlloc(func() {
			_, loadErr = LoadKeyring(path)
		})
		// Killing mutation, actually run against this file: restore the
		// `if status != frameBounded { return nil }` this walk's loop used to
		// open with, so a framing it cannot bound ends the walk with a nil error
		// whatever tag it sits on. The declared-length row still passes, since
		// its header names a body length; the run then stops here on the first
		// row the mutation unguards, with
		//
		//	framing_test.go:1007: LoadKeyring(an old-format indeterminate length) allocated 4294978416 bytes, want at most 1048576
		//
		// which is 4.00 GiB of allocation from nine bytes of keyring file.
		if allocated > framingAllocCeiling {
			t.Fatalf("LoadKeyring(%s) allocated %d bytes, want at most %d", tc.name, allocated, framingAllocCeiling)
		}
		// Which refusal produced that cheap outcome. It sits below the
		// measurement because the measurement is what the gate is for, and the
		// two are separate questions: a cheap refusal under some other error
		// would still leave this package's own verdict unaccounted for.
		if !errors.Is(loadErr, errMalformedSignaturePacket) {
			t.Fatalf("LoadKeyring(%s) error = %v, want the malformed-packet refusal", tc.name, loadErr)
		}
	}

	for _, tc := range []struct {
		name string
		data []byte
	}{
		{name: "an armored signature block", data: armorEncode(t, openpgp.SignatureType, indeterminate)},
		{name: "a new-format partial length", data: partial},
	} {
		var checkErr error
		allocated := measureAlloc(func() {
			_, checkErr = checkOne(manifest, tc.data, kr)
		})
		if allocated > framingAllocCeiling {
			t.Fatalf("checkOne(%s) allocated %d bytes, want at most %d", tc.name, allocated, framingAllocCeiling)
		}
		if !errors.Is(checkErr, errMalformedSignaturePacket) {
			t.Fatalf("checkOne(%s) error = %v, want the malformed-packet refusal", tc.name, checkErr)
		}
	}
}

// TestNoPacketTagCarriesAnUnboundedFramingPastTheGate sweeps the gate over
// every tag a packet header can spell, rather than over the one tag the
// amplifying rows above happen to sit on.
//
// The tag is at most six bits of an octet the input itself writes, so a gate
// that refused an unbounded framing on some tags and waved it through on
// others is one an attacker leaves by rewriting them. Every row here is that
// move: an unbounded-framed carrier packet with the amplifying signature packet
// behind it, so a carrier this walk waves through hands that signature to
// go-crypto, whose four-octet subpacket length is what allocates. Both entry
// points are driven for every carrier, since a keyring reaches the parser
// through one and a detached signature through the other, and neither routes
// through the other.
//
// The two questions are accumulated across the sweep and reported once each
// rather than stopping it. A t.Fatalf inside the loop would let whichever tag
// failed first decide which of the two ever got asked, and it would name one
// tag where the answer this test exists to give is which tags.
//
// Not parallel: runtime.MemStats is process-wide, so a parallel test allocating
// alongside it would be counted here.
func TestNoPacketTagCarriesAnUnboundedFramingPastTheGate(t *testing.T) {
	kr := loadTestKeyring(t, keyringFixture)
	manifest := readFixture(t, manifestAFixture)
	// The positive control, ahead of the sweep and on committed fixtures rather
	// than hand-built shapes: real keyrings still load and real signatures still
	// verify, so a finding below is a carrier's own framing and not a gate that
	// refuses everything.
	requireFramingFixturesStillWork(t, kr, manifest)

	var overCeiling, wrongVerdict []unboundedFinding
	// Declared once rather than per iteration: it closes over the two
	// accumulators, and a closure rebuilt inside the loop would allocate one per
	// carrier for no gain.
	record := func(carrier unboundedCarrier, entry string, allocated uint64, err error) {
		finding := unboundedFinding{framing: carrier.framing, entry: entry, allocated: allocated, tag: carrier.tag}
		if allocated > framingAllocCeiling {
			overCeiling = append(overCeiling, finding)
		}
		if !errors.Is(err, errMalformedSignaturePacket) {
			wrongVerdict = append(wrongVerdict, finding)
		}
	}

	// One leaf, rewritten per carrier: LoadKeyring takes a path and every write
	// truncates, so the sweep costs one file rather than one per tag.
	path := filepath.Join(t.TempDir(), "carrier.gpg")
	for _, carrier := range unboundedCarriers() {
		// #nosec G703 -- the directory is this test's own t.TempDir and the leaf
		// is a constant; no part of the path comes from outside this function.
		if err := os.WriteFile(path, carrier.data, keyringFileMode); err != nil {
			t.Fatalf("write %s tag %d: %v", carrier.framing, carrier.tag, err)
		}

		var loadErr error
		allocated := measureAlloc(func() {
			_, loadErr = LoadKeyring(path)
		})
		record(carrier, "LoadKeyring", allocated, loadErr)

		var checkErr error
		allocated = measureAlloc(func() {
			_, checkErr = checkOne(manifest, carrier.data, kr)
		})
		record(carrier, "checkOne", allocated, checkErr)
	}

	// Killing mutation, actually run against this file: restore the
	// `if status != frameBounded && frame.tag != packetTagSignature` this walk's
	// loop used to open with, so a framing it cannot bound is refused on the
	// signature tag alone. This assertion then fails with the list of every
	// carrier that got through, wrapped here and cut after its first element:
	//
	//	framing_test.go:1126: 58 carriers allocated more than 1048576 bytes:
	//	[{framing:new-format partial entry:LoadKeyring allocated:4294979624 tag:1} ...]
	//
	// It names 30 of the 64 new-format partial carriers through LoadKeyring and
	// 27 of them through checkOne, about 4.00 GiB apiece out of 14 bytes of
	// input, plus the old-format tag 9 carrier through LoadKeyring, whose 11
	// bytes reach the same allocation because go-crypto consumes none of that
	// packet's body and reads the amplifier behind it as the next packet. The
	// other 15 old-format rows are documentary for this assertion and pinned
	// only for the sentinel below: the mutation takes their refusal away while
	// leaving them cheap, since that same parse swallows the rest of the input.
	// TestAmplifyingFramingsAreRefusedCheaply passes in full under this
	// mutation - all five of its rows sit on tag 2 - which is what makes this
	// sweep a test of its own rather than a second spelling of that one.
	if len(overCeiling) > 0 {
		t.Errorf("%d carriers allocated more than %d bytes: %+v", len(overCeiling), framingAllocCeiling, overCeiling)
	}
	// Which verdict came with that cheap outcome, asked separately and below the
	// measurement for the reason the tests above give: a carrier refused cheaply
	// under some other error would leave this package's own verdict
	// unaccounted for.
	if len(wrongVerdict) > 0 {
		t.Errorf("%d carriers were not refused as malformed framing: %+v", len(wrongVerdict), wrongVerdict)
	}
}

// unboundedCarrier is one packet header declaring no total for its body, with
// the amplifying signature packet behind it - the whole point being that this
// walk cannot find where the carrier ends while go-crypto reads on regardless.
type unboundedCarrier struct {
	framing string
	data    []byte
	tag     int
}

// unboundedFinding is one carrier that got past the gate through one entry
// point, kept rather than fataled on so the failure can name every tag that did
// and what each of them cost.
type unboundedFinding struct {
	framing   string
	entry     string
	allocated uint64
	tag       int
}

// unboundedCarriers builds every tag's spelling of the two unbounded framings:
// the new format's partial length on all 64 tags it can name, and the old
// format's indeterminate length on the 16 its narrower tag field reaches.
//
// The octets are hand-spelled here, and named apart from the production
// constants they exercise, so that mutating one of those constants cannot
// reshape this fixture into agreeing with it.
func unboundedCarriers() []unboundedCarrier {
	const (
		// The header MSB plus the new-format bit, leaving the six bits below it
		// for the tag; and the header MSB alone, where the tag sits two bits up
		// and length type 3 is the indeterminate one.
		newFormatLead      = 0xc0
		newFormatTagMax    = 63
		oldFormatLead      = 0x80
		oldFormatTagMax    = 15
		oldFormatTagShiftT = 2
		indeterminateType  = 0x03
		// A partial length sizing one one-byte chunk, that chunk, and the final
		// zero-length octet ending the chunk sequence.
		partialLead      = 0xe0
		newFormatCarrier = 4
		oldFormatCarrier = 1
	)

	// The ten bytes that allocate: 0xc2 is a new-format signature header, 0x08
	// declares its eight-octet body, and that body is a v6 signature header
	// (version 6, signature type 0x13, RSA, SHA-256) whose four-octet hashed
	// subpacket length is 0xffffffff.
	amplifier := []byte{0xc2, 0x08, 0x06, 0x13, 0x01, 0x08, 0xff, 0xff, 0xff, 0xff}

	// The tags are counted in bytes rather than ints, since a tag is six bits of
	// one octet and the loop bound says so: the header octets below are then
	// assembled without a narrowing conversion for a linter to doubt.
	carriers := make([]unboundedCarrier, 0, newFormatTagMax+oldFormatTagMax+2)
	for tag := byte(0); tag <= newFormatTagMax; tag++ {
		// One allocation per carrier, sized once: each carrier owns its own
		// bytes, since they are written to a file and passed to checkOne
		// unchanged rather than consumed inside this loop.
		data := make([]byte, 0, newFormatCarrier+len(amplifier))
		data = append(data, newFormatLead|tag, partialLead, 0x00, 0x00)
		data = append(data, amplifier...)
		carriers = append(carriers, unboundedCarrier{framing: "new-format partial", data: data, tag: int(tag)})
	}
	for tag := byte(0); tag <= oldFormatTagMax; tag++ {
		// The indeterminate body runs to the end of the input, so here the
		// amplifier sits inside the carrier packet rather than behind it.
		data := make([]byte, 0, oldFormatCarrier+len(amplifier))
		data = append(data, oldFormatLead|tag<<oldFormatTagShiftT|indeterminateType)
		data = append(data, amplifier...)
		carriers = append(carriers, unboundedCarrier{framing: "old-format indeterminate", data: data, tag: int(tag)})
	}

	return carriers
}

// requireFramingFixturesStillWork is the positive control for every measurement
// here, run before each of them and on committed fixtures rather than on
// hand-built shapes: the binary key export, the multi-key armored keyring and
// the signing-subkey export all load - a fourth, keyring.asc, is what the caller
// loaded to get kr - and both encodings of a real signature still verify.
//
// The subkey export is in the list for what it carries rather than for being a
// third keyring: its binding signature holds an embedded signature subpacket, so
// it is the one committed file whose acceptance says the recursion descends into
// real material and comes back with a nil answer.
//
// Everything this gate refuses is aimed at shapes gpg writes into neither a
// keyring nor a detached signature. A fixture failing here would be that claim
// coming back wrong, so it stops the test rather than being worked around.
func requireFramingFixturesStillWork(t *testing.T, kr *Keyring, manifest []byte) {
	t.Helper()

	for _, name := range []string{binaryFixture, twoKeyFixture, subkeyFixture} {
		if _, err := LoadKeyring(fixturePath(name)); err != nil {
			t.Fatalf("positive control: LoadKeyring(%s) = %v, want nil", name, err)
		}
	}
	for _, name := range []string{sigValidArmored, sigValidBinary} {
		if _, err := checkOne(manifest, readFixture(t, name), kr); err != nil {
			t.Fatalf("positive control: checkOne(%s) = %v, want nil", name, err)
		}
	}
}

// armorEncode wraps body in an ASCII-armored block of blockType, which is how
// the armored rows above reach their entry point: armor is base64 text carrying
// no packet framing to walk, so those rows exercise the gate only once a decode
// has happened.
func armorEncode(t *testing.T, blockType string, body []byte) []byte {
	t.Helper()

	var buf bytes.Buffer
	writer, err := armor.Encode(&buf, blockType, nil)
	if err != nil {
		t.Fatalf("armor.Encode(%s) = %v, want nil", blockType, err)
	}
	if _, err = writer.Write(body); err != nil {
		t.Fatalf("write armored body: %v", err)
	}
	if err = writer.Close(); err != nil {
		t.Fatalf("close armored body: %v", err)
	}

	return buf.Bytes()
}

// measureAlloc reports how many bytes the Go heap handed out while f ran.
//
// TotalAlloc is cumulative and never decreases, so the difference across a call
// counts everything allocated during it whether or not it survived - which is
// the right measure for a defect whose allocation is freed a moment later. It
// is process-wide, which is why its callers do not run in parallel.
func measureAlloc(f func()) uint64 {
	var before, after runtime.MemStats

	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)

	return after.TotalAlloc - before.TotalAlloc
}

const (
	// armorHeaderLineSize is how long the single header line the two
	// measurements below carry: a megabyte less the room the delimiter lines
	// need, so the signature fixture built from it stays inside
	// helpers.SignatureMaxSize and is a blob this program will genuinely accept
	// from a source and hand on. Larger would only make the mutated run slower to
	// arrive at the same answer, since the cost is quadratic in this length.
	armorHeaderLineSize = 1<<20 - 128 // just under 1 MiB

	// armorHeaderAllocCeiling is what refusing such an input may cost. It is
	// stated against the fixture's own size rather than as the flat
	// framingAllocCeiling above, because LoadKeyring legitimately reads the whole
	// file into memory before it looks at anything and io.ReadAll grows its
	// buffer geometrically, so a small multiple of the file's length is honest
	// work here - measured at 2230664 bytes plainly and 4457576 under the race
	// detector, which is the figure the multiple has to clear. The defect spends
	// hundreds of times it either way.
	armorHeaderAllocCeiling = 16 * armorHeaderLineSize // 16 MiB

	// largestFixtureHeaderSection is the longest header section any committed
	// fixture carries, as measured by the sweep below: gpg writes an opening line
	// and then the blank line that ends the section, so a section is the single
	// byte that blank line occupies. The distance from here to armorHeaderMaxSize
	// is the headroom the bound holds over real armor, and it is written down so
	// that a fixture arriving with real header lines has to restate it rather
	// than consume it unremarked.
	largestFixtureHeaderSection = 1
)

// TestCheckOneRefusesAnOversizedArmorHeaderCheaply measures the header bound on
// the blob path, which is the path whose input is not the operator's own.
//
// armor.Decode reads through a bufio.Reader of 100 bytes and appends every
// continuation chunk of an over-long header line to the value it has built so
// far, so its header loop costs about N*N/200 bytes for an N-byte section. This
// blob carries just under a megabyte of that, inside helpers.SignatureMaxSize
// and therefore a blob this program accepts from a source and hands on.
//
// The ceiling is the assertion that carries the test. Ungated this blob is
// refused too, for the incidental reason that its body is empty, so what the
// bound removes is the gigabytes spent reaching a refusal rather than the
// refusal itself; the sentinel below is what says the refusal was this one's.
//
// Not parallel: runtime.MemStats is process-wide, so a parallel test allocating
// alongside it would be counted here.
func TestCheckOneRefusesAnOversizedArmorHeaderCheaply(t *testing.T) {
	kr := loadTestKeyring(t, keyringFixture)
	manifest := readFixture(t, manifestAFixture)

	// The positive control, through the identical call: a real armored signature
	// still decodes and still verifies, so the refusal below is this blob's own
	// header section and not a bound that refuses every armored blob.
	if _, err := checkOne(manifest, readFixture(t, sigValidArmored), kr); err != nil {
		t.Fatalf("positive control: checkOne(%s) = %v, want nil", sigValidArmored, err)
	}

	blob := oversizedArmorHeader(openpgp.SignatureType)

	var err error
	allocated := measureAlloc(func() {
		_, err = checkOne(manifest, blob, kr)
	})
	// Killing mutation, actually run against this file: delete the
	// checkArmorHeaderSection call from decodeArmorBlock, which hands the blob to
	// armor.Decode with nothing in front of it and removes nothing else - the
	// packet framing gate below the decode is untouched, and this blob's decoded
	// body is empty rather than malformed, so that gate has nothing to say about
	// it. This assertion then fails with
	//
	//	framing_test.go:1353: checkOne(oversized armor header) allocated 5541095464 bytes, want at most 16775168
	//
	// which is the defect itself, in bytes, out of under a megabyte of input.
	if allocated > armorHeaderAllocCeiling {
		t.Fatalf("checkOne(oversized armor header) allocated %d bytes, want at most %d", allocated, armorHeaderAllocCeiling)
	}
	// Which refusal produced that cheap outcome. It sits below the measurement
	// because the measurement is what the bound is for; a mutation that refused
	// this blob cheaply under some other error would fail here and pass above.
	if !errors.Is(err, errOversizedArmorHeader) {
		t.Fatalf("checkOne(oversized armor header) = %v, want the oversized-header refusal", err)
	}
}

// TestLoadKeyringRefusesAnOversizedArmorHeaderCheaply is the same measurement on
// the keyring path.
//
// The keyring is the operator's own file rather than a value from a trust
// boundary, so this is the weaker of the two cases - but it reaches the
// identical decoder through the identical armor, and closing one and not the
// other would leave a reader guessing which. It is also the path where the
// defect is worst: keyringMaxSize is sixty-four times helpers.SignatureMaxSize
// and the cost is quadratic, so the ceiling a keyring file may reach buys about
// four thousand times the copying one blob can.
//
// The ceiling carries this test too. Ungated the file is refused as well, its
// empty packet stream leaving loadKeyring with no keys to report, so what the
// bound removes is again the gigabytes spent reaching a refusal rather than the
// refusal itself.
//
// Not parallel, for the reason the test above gives.
func TestLoadKeyringRefusesAnOversizedArmorHeaderCheaply(t *testing.T) {
	dir := t.TempDir()

	// The control is a real keyring written into the same directory, so a refusal
	// below is the file's own header section and not this directory or this
	// test's writes.
	controlPath := filepath.Join(dir, "control.asc")
	// #nosec G703 -- dir is this test's own t.TempDir and the leaf is a
	// constant; no part of the path comes from outside this function.
	if err := os.WriteFile(controlPath, readFixture(t, armoredFixture), keyringFileMode); err != nil {
		t.Fatalf("write control keyring: %v", err)
	}
	if _, err := LoadKeyring(controlPath); err != nil {
		t.Fatalf("positive control: LoadKeyring(control.asc) = %v, want nil", err)
	}

	path := filepath.Join(dir, "oversized.asc")
	// #nosec G703 -- the same t.TempDir and another constant leaf, as just above.
	if err := os.WriteFile(path, oversizedArmorHeader(openpgp.PublicKeyType), keyringFileMode); err != nil {
		t.Fatalf("write oversized keyring: %v", err)
	}

	var err error
	allocated := measureAlloc(func() {
		_, err = LoadKeyring(path)
	})
	// Killing mutation, actually run against this file: the same deletion of the
	// checkArmorHeaderSection call from decodeArmorBlock. This assertion then
	// fails with
	//
	//	framing_test.go:1414: LoadKeyring(oversized armor header) allocated 5543309360 bytes, want at most 16775168
	//
	// out of a keyring file just under a megabyte, which a real one legitimately is.
	if allocated > armorHeaderAllocCeiling {
		t.Fatalf("LoadKeyring(oversized armor header) allocated %d bytes, want at most %d", allocated, armorHeaderAllocCeiling)
	}
	// Which refusal produced that cheap outcome, asked below the measurement for
	// the reason the test above gives.
	if !errors.Is(err, errOversizedArmorHeader) {
		t.Fatalf("LoadKeyring(oversized armor header) = %v, want the oversized-header refusal", err)
	}
}

// TestArmorHeaderBoundAcceptsEveryCommittedFixture is the acceptance half of the
// header bound, stated over every file testdata holds rather than over the
// armored ones a hand-written list would have named.
//
// Each measurement above carries a positive control, so a bound tight enough to
// refuse all ordinary armor does not go unnoticed. What those controls cannot
// state is the property over the directory: a bound refusing some armored shape
// neither of them happens to load would pass both and fail here, and a fixture
// committed later is inside the property with nobody adding it to a list.
//
// The sweep also reports the longest section it found, which is what says
// whether armorHeaderMaxSize holds headroom over real armor or was picked out of
// the air.
func TestArmorHeaderBoundAcceptsEveryCommittedFixture(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir(testdataDir)
	if err != nil {
		t.Fatalf("read %s: %v", testdataDir, err)
	}

	longest, longestIn, sections := 0, "", 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		data := readFixture(t, name)
		// Killing mutation, actually run against this file: set armorHeaderMaxSize
		// to 0, a bound no real section can sit inside. Every armored fixture is
		// then refused and this assertion names each of them, the first being
		//
		//	framing_test.go:1463: checkArmorHeaderSection(keyring-outsider.asc) =
		//	oversized OpenPGP armor header section: no blank line ends one within 0 bytes, want nil
		//
		// with 16 more behind it, and the sweep ending on the fatal below for having
		// measured no section at all. The mutation is not this test's alone - the two
		// measurements above fail it on their positive controls - so what this one
		// adds is every fixture nothing else loads, and which of them failed.
		if err := checkArmorHeaderSection(data); err != nil {
			t.Errorf("checkArmorHeaderSection(%s) = %v, want nil", name, err)

			continue
		}

		fileLongest, fileSections := armorHeaderSections(t, name, data)
		sections += fileSections
		if fileLongest > longest {
			longest, longestIn = fileLongest, name
		}
	}

	// Without this the sweep passes just as well against a testdata directory
	// holding no armor at all, or against a walk that never recognized an opening
	// line: every file would be accepted for having no section to judge.
	if sections == 0 {
		t.Fatalf("the sweep measured no armor header section at all in %s", testdataDir)
	}
	if longest > largestFixtureHeaderSection {
		t.Errorf("the longest committed header section is %d bytes, in %s, want at most %d",
			longest, longestIn, largestFixtureHeaderSection)
	}
	t.Logf("%d committed header sections, longest %d bytes (in %s), against a bound of %d",
		sections, longest, longestIn, armorHeaderMaxSize)
}

// armorHeaderSections walks data exactly as checkArmorHeaderSection walks it and
// reports the one thing that check does not: how long the longest header section
// in data is, and how many sections it found, in that order.
//
// A false from armorHeaderSectionEnd here is unreachable and so documentary
// rather than pinned - the caller has already had checkArmorHeaderSection return
// nil for this data, over these same offsets by this same rule. It is a t.Fatalf
// rather than a skip so that a divergence between the two walks stops the sweep
// instead of quietly dropping a section from the measurement.
func armorHeaderSections(t *testing.T, name string, data []byte) (int, int) {
	t.Helper()

	longest, sections := 0, 0
	for off := 0; off < len(data); {
		line, next := nextLine(data, off)
		off = next
		if !isArmorBlockStart(line) {
			continue
		}
		end, ok := armorHeaderSectionEnd(data, off)
		if !ok {
			t.Fatalf("armorHeaderSectionEnd(%s, %d) = _, false, want true", name, off)
		}
		sections++
		if end-off > longest {
			longest = end - off
		}
		off = end
	}

	return longest, sections
}

// oversizedArmorHeader builds an armor block of blockType whose header section
// is a single line of armorHeaderLineSize bytes, which is the shape both
// measurements above are made on.
//
// The delimiter lines are hand-spelled rather than taken from the package
// constants they have to agree with, so that mutating one of those constants
// cannot reshape this fixture into agreeing with it. The header line carries a
// colon because armor.Decode's header loop abandons the block without one, and
// it is the colon that makes every chunk behind the first a continuation
// appended to the value already accumulated - which is the copying being
// measured.
func oversizedArmorHeader(blockType string) []byte {
	const header = "Comment: "

	opening := "-----BEGIN " + blockType + "-----\n"
	closing := "\n-----END " + blockType + "-----\n"

	// Sized once, since the whole point of the fixture is its length: the header
	// line, the blank line that ends the section, and the two delimiters.
	buf := bytes.NewBuffer(make([]byte, 0, len(opening)+len(header)+armorHeaderLineSize+len(closing)+1))
	buf.WriteString(opening)
	buf.WriteString(header)
	for range armorHeaderLineSize {
		buf.WriteByte('A')
	}
	buf.WriteString("\n")
	buf.WriteString(closing)

	return buf.Bytes()
}

const (
	// acceptedSectionFill is how many bytes of header line each section of the
	// blob below carries. It is hand-spelled rather than derived from
	// armorHeaderMaxSize, for the reason oversizedArmorHeader gives about its own
	// delimiters, and 4000 leaves room under that bound's 4096 for the colon
	// prefix, the colon-free line and the blank line that ends the section. It is
	// also close to the worst packing an attacker can choose: measured over fills
	// from 256 to 4089, the cost per byte of input peaks around here, at 23.9
	// times the input against 18.6 at 3000 and 23.4 at 4089.
	acceptedSectionFill = 4000

	// acceptedSectionAllocRatio is what a blob of accepted sections may cost,
	// stated as a multiple of the blob's own length. The measured ratio is 23.9,
	// unmoved by the race detector and varying by a few thousand bytes between
	// runs, so this holds most of a factor of three in reserve while staying far
	// below what a return to a cost quadratic in the whole input would reach.
	acceptedSectionAllocRatio = 64
)

// TestArmorHeaderBoundLeavesALinearResidual measures what the bound does not
// remove.
//
// An accepted section may cost everything armorHeaderMaxSize allows, and one
// input may carry as many accepted sections as it has room for: a header line
// with no colon makes armor.Decode abandon the block and resume its search for
// an opening line, so the header loop starts again at the next one. The blob
// here is that shape at the size a source may hand over, so what it measures is
// the residual one blob can reach on the path whose input is not the operator's
// own. armorHeaderMaxSize's own doc comment holds what that residual is worth
// per collection; this is what makes the figure a checked number rather than an
// asserted one, to its order rather than to its digits.
//
// The ceiling is a multiple of the blob's own length rather than a byte count,
// because the property is the linearity: a cost quadratic in the whole input
// again would pass no multiple, while the run-to-run variance a process-wide
// TotalAlloc difference carries - thousands of bytes against a ratio counted in
// the tens - moves nothing.
//
// Not parallel: runtime.MemStats is process-wide, for the reason the
// measurements above give.
func TestArmorHeaderBoundLeavesALinearResidual(t *testing.T) {
	kr := loadTestKeyring(t, keyringFixture)
	manifest := readFixture(t, manifestAFixture)

	blob, sections := acceptedSectionArmor(openpgp.SignatureType)
	if int64(len(blob)) > helpers.SignatureMaxSize {
		t.Fatalf("the blob is %d bytes, past the %d a source may hand over", len(blob), helpers.SignatureMaxSize)
	}
	// The acceptance half, and the anti-vacuity one: every section is inside the
	// bound, so the measurement below is the cost of walking them rather than the
	// cost of the gate refusing the first.
	if err := checkArmorHeaderSection(blob); err != nil {
		t.Fatalf("checkArmorHeaderSection(%d accepted sections) = %v, want nil", sections, err)
	}

	var err error
	allocated := measureAlloc(func() {
		_, err = checkOne(manifest, blob, kr)
	})
	ceiling := uint64(len(blob)) * acceptedSectionAllocRatio
	if allocated > ceiling {
		t.Fatalf("checkOne(%d accepted armor header sections) allocated %d bytes for %d bytes of input, want at most %d",
			sections, allocated, len(blob), ceiling)
	}
	// What the blob decoded to, asked below the measurement because the
	// measurement is the point: every section is abandoned at its colon-free line
	// and no block is ever read, so the bytes counted above are the walk itself.
	if !armorDecodeFoundNothing(err) {
		t.Fatalf("checkOne(%d accepted armor header sections) = %v, want the decoder to find no block", sections, err)
	}
	t.Logf("%d accepted sections in %d bytes allocated %d, %.1f times the input",
		sections, len(blob), allocated, float64(allocated)/float64(len(blob)))
}

// acceptedSectionArmor builds a blob of as many maximal-but-accepted armor
// header sections as fit inside helpers.SignatureMaxSize, and reports how many
// that was. Each section is an opening line, one header line of
// acceptedSectionFill bytes carrying a colon, a line carrying none, and the
// blank line that ends the section.
//
// The colon-free line is what makes the blob a walk rather than one section, for
// the reason the test above gives. The delimiters are hand-spelled, as
// oversizedArmorHeader's are and for the same reason.
func acceptedSectionArmor(blockType string) ([]byte, int) {
	var section bytes.Buffer

	section.WriteString("-----BEGIN " + blockType + "-----\n")
	section.WriteString("C: ")
	for range acceptedSectionFill {
		section.WriteByte('A')
	}
	// The header line's terminator, then a header line with no colon in it, then
	// the blank line ending the section.
	section.WriteString("\nx\n\n")

	sections := int(helpers.SignatureMaxSize) / section.Len()
	buf := bytes.NewBuffer(make([]byte, 0, sections*section.Len()))
	for range sections {
		buf.Write(section.Bytes())
	}

	return buf.Bytes(), sections
}

// armorSectionTermination is how armorSectionOfSpan ends the header section it
// builds: the way real armor ends one, or by running the input out inside it.
type armorSectionTermination int

const (
	// sectionEndsBlank ends the section with the blank line armorHeaderSectionEnd
	// is looking for, which is what gives the section a span of its own choosing.
	sectionEndsBlank armorSectionTermination = iota
	// sectionRunsOut ends the input with no blank line behind the opening line at
	// all, which is the shape a truncated or corrupt file hands the walk.
	sectionRunsOut
)

// armorSectionRunOutSpan is the run-out row's span: a header line short enough
// that no arithmetic against armorHeaderMaxSize can decide it. The two rows
// above it state their spans against that bound because there the bound is the
// answer; here it is not, and a span anywhere near it would let the comparison
// mutations those rows name fail this one too, which is the single thing it must
// not do - what it pins is the loop falling out of input instead.
const armorSectionRunOutSpan = 16

// armorSectionBoundaryCase is one row of TestArmorHeaderSectionEndBoundary: the
// section to build, whether checkArmorHeaderSection has to refuse it, and
// whether armor.Decode has to find no block at all in the same bytes.
type armorSectionBoundaryCase struct {
	name                   string
	span                   int
	term                   armorSectionTermination
	wantRefuse             bool
	wantDecodeFoundNothing bool
}

// armorSectionBoundaryCases is armorHeaderSectionEnd's predicate at its edges,
// one row per answer it gives there: the widest section it accepts, the
// narrowest it refuses, and a section whose input runs out inside the bound.
func armorSectionBoundaryCases() []armorSectionBoundaryCase {
	return []armorSectionBoundaryCase{
		{
			// Killing mutation, actually run against this file: narrow the loop's own
			// per-line check by one byte, from next-start > armorHeaderMaxSize to
			// next-start >= armorHeaderMaxSize. This row then fails with
			//
			//	framing_test.go:1779: checkArmorHeaderSection(a 4096-byte section) refused = true
			//	(err oversized OpenPGP armor header section: no blank line ends one within 4096 bytes), want refused = false
			//
			// while the row below it, one byte wider, passes throughout - which is what
			// makes the pair a boundary rather than a direction.
			name:       "a section ending exactly at the bound",
			span:       armorHeaderMaxSize,
			term:       sectionEndsBlank,
			wantRefuse: false,
		},
		{
			// Killing mutation, actually run against this file: widen that same check
			// by one byte instead, to next-start > armorHeaderMaxSize+1. This row then
			// fails with
			//
			//	framing_test.go:1779: checkArmorHeaderSection(a 4097-byte section) refused = false (err <nil>), want refused = true
			//
			// while the row above it still passes, so neither row alone says where the
			// bound sits and the two of them say it exactly.
			name:       "a section one byte past the bound",
			span:       armorHeaderMaxSize + 1,
			term:       sectionEndsBlank,
			wantRefuse: true,
		},
		{
			// Killing mutation, actually run against this file: turn the loop's
			// fall-through into the refusal, from return len(data), true to return 0,
			// false, which is the arm accepting input that runs out inside the bound.
			// This row then fails with
			//
			//	framing_test.go:1779: checkArmorHeaderSection(a 16-byte section) refused = true
			//	(err oversized OpenPGP armor header section: no blank line ends one within 4096 bytes), want refused = false
			//
			// calling a section of sixteen bytes oversized for a bound of 4096, which
			// is the answer armorHeaderSectionEnd's own doc comment refuses to hand an
			// operator.
			name:                   "a section the input runs out inside",
			span:                   armorSectionRunOutSpan,
			term:                   sectionRunsOut,
			wantDecodeFoundNothing: true,
		},
	}
}

// TestArmorHeaderSectionEndBoundary pins the two answers armorHeaderSectionEnd
// gives that nothing else in this package reaches: a section ending exactly at
// armorHeaderMaxSize, and a section the input runs out inside.
//
// Every other fixture here sits nowhere near either - the committed ones carry a
// single-byte section, the linear-residual blob 4007-byte ones, and the two
// measurements about a megabyte - so the comparison could be narrowed by a byte,
// or the fall-through turned into a refusal, with the whole suite still passing.
//
// The first two rows are each other's positive control, one byte apart out of
// the same generator: the bound is the only thing separating them, so a refusal
// that never reached the check would fail the accepting row beside it. Their
// spans are stated against armorHeaderMaxSize rather than spelled, since here
// the constant is the boundary being asserted and the mutations the rows name
// are the comparison and the fall-through, neither of which its value can mask.
//
// Parallel, unlike the measurements above: it allocates a few kilobytes and
// measures nothing, so nothing running alongside it is counted here.
func TestArmorHeaderSectionEndBoundary(t *testing.T) {
	t.Parallel()

	for _, tc := range armorSectionBoundaryCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			blob := armorSectionOfSpan(openpgp.SignatureType, tc.span, tc.term)
			// The fixture ahead of its verdict: nothing sits behind the section, so
			// everything past the opening line is it. Only the generator above can
			// answer this one - it says the row got the span it asked for, not what
			// the walk makes of that span.
			if _, start := nextLine(blob, 0); len(blob)-start != tc.span {
				t.Fatalf("the generated section spans %d bytes, want %d", len(blob)-start, tc.span)
			}

			err := checkArmorHeaderSection(blob)
			if got := errors.Is(err, errOversizedArmorHeader); got != tc.wantRefuse {
				t.Fatalf("checkArmorHeaderSection(a %d-byte section) refused = %t (err %v), want refused = %t", tc.span, got, err, tc.wantRefuse)
			}
			if !tc.wantDecodeFoundNothing {
				return
			}
			// What the decoder makes of the bytes the walk just accepted, which is the
			// io.EOF armorHeaderSectionEnd's doc comment leaves this shape to. It sits
			// below the verdict because the verdict is what the row exists for.
			if _, _, err := decodeArmorBlock(blob); !armorDecodeFoundNothing(err) {
				t.Fatalf("decodeArmorBlock(a %d-byte section running out of input) = %v, want no block found", tc.span, err)
			}
		})
	}
}

// armorSectionOfSpan builds an armor block whose header section spans exactly
// span bytes - the distance armorHeaderSectionEnd measures, from the byte after
// the opening line through the last byte of whatever ends the section - and puts
// nothing behind it, so the section is the whole blob below its opening line.
//
// term picks that ending: the blank line real armor writes, or the end of the
// input with no blank line behind the opening one anywhere.
//
// The header name is the one-character one acceptedSectionArmor writes, which is
// what the allocation figures in armorHeaderMaxSize's and armorHeaderSectionEnd's
// own doc comments are measured on: armor.Decode accumulates the header's value
// rather than its whole line, so a longer name lands the final append in a
// smaller size class and moves those figures with it. The opening delimiter is
// hand-spelled for the reason oversizedArmorHeader's are.
func armorSectionOfSpan(blockType string, span int, term armorSectionTermination) []byte {
	const name = "C: "

	// What the section spends on its own shape rather than on fill: the header
	// name, the header line's terminator, and, where the section ends the way
	// armor does, the blank line ending it.
	overhead := len(name) + len("\n")
	if term == sectionEndsBlank {
		overhead += len("\n")
	}

	opening := "-----BEGIN " + blockType + "-----\n"

	// Sized once, since the fixture is its length: the opening line, the section
	// behind it, and nothing else.
	buf := bytes.NewBuffer(make([]byte, 0, len(opening)+span))
	buf.WriteString(opening)
	buf.WriteString(name)
	for range span - overhead {
		buf.WriteByte('A')
	}
	buf.WriteString("\n")
	if term == sectionEndsBlank {
		buf.WriteString("\n")
	}

	return buf.Bytes()
}

const (
	// committedPacketMeasurements is how many bounded packet frames the sweep
	// below finds across the whole testdata directory, counting every armor
	// block's decoded body rather than only the first one in a file.
	//
	// It is asserted rather than merely reported so that a fixture dropping out
	// of the sweep is red: a file that stopped decoding, or one whose framing
	// stopped being walkable, would otherwise quietly shrink the property to
	// whatever still worked.
	committedPacketMeasurements = 53

	// subkeyFixtureSignatures is how many signature packets the signing-subkey
	// export carries: the user id self-certification and the subkey binding
	// signature. Exactly one of them holds an embedded signature, which is what
	// makes the pair a control for the recursion in both directions.
	subkeyFixtureSignatures = 2

	// driveAllocPerByte is what one byte of an adversarial segment may cost the
	// drive. It is twice the worst per-byte ratio any driven tag reached across
	// the four sizes below - 59.0, the packed-subpacket signature at 256 KiB -
	// rounded up to a power of two.
	driveAllocPerByte = 128
	// driveAllocFloor is what a segment of no length at all may cost. It sits
	// above the largest constant-size allocation any driven tag makes, which is
	// a public key's fixed MPI budget at just under 30 KiB, so the floor is the
	// segment-independent term rather than a second ratio.
	driveAllocFloor = 64 << 10

	// mpiDeclaredBits is the two-octet bit length the MPI probe declares, which
	// is the largest one that field can name.
	mpiDeclaredBits = 0xffff
	// mpiAllocFloor is the allocation that bit length names: one byte per eight
	// bits. Asserting it from below is what says the probe reached the MPI read
	// at all, rather than measuring a parse that failed before it.
	mpiAllocFloor = (mpiDeclaredBits + 7) / 8
	// mpiAllocCeiling is what the whole packet may then cost. Measured at 9560
	// bytes, so the ceiling holds most of a factor of seven while staying far
	// below what a four-octet bit length would reach.
	mpiAllocCeiling = 64 << 10
)

// TestDriveConsumesEveryCommittedPacketExactly is the primary pin on the
// assumption the drive rests on: that go-crypto reads a packet to exactly the
// end this walk computed for it.
//
// The drive refuses a packet the parser leaves bytes inside, which is only a
// usable rule while real material leaves none. Every bounded frame of every
// committed fixture is measured, including every armor block's decoded body
// rather than only the first in a file, and every one of them must come back
// with nothing unread. A dependency bump that changed where the parser stops -
// a packet type that starts skipping a trailing field, a new length shape read
// differently - turns this red on real gpg output rather than on a hand-built
// case nobody would have written for it.
//
// Parallel, unlike the measurements here: it counts packets rather than bytes
// allocated, so nothing running alongside it is counted in its answer.
func TestDriveConsumesEveryCommittedPacketExactly(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir(testdataDir)
	if err != nil {
		t.Fatalf("read %s: %v", testdataDir, err)
	}

	measured := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		data := readFixture(t, name)
		if !bytes.Contains(data, []byte(armorBlockStart)) {
			measured += driveEveryPacket(t, name, data)

			continue
		}
		// Every block, cut the way readArmoredKeyRing cuts them. A block that
		// does not decode is skipped rather than fataled on, since two committed
		// fixtures are damaged armor on purpose; what keeps that from hiding a
		// regression is the total below.
		for off := 0; off < len(data); {
			line, next := nextLine(data, off)
			start := off
			off = next
			if !isArmorBlockStart(line) {
				continue
			}
			decoded, _, decodeErr := decodeArmorBlock(data[start:])
			if decodeErr != nil {
				t.Logf("%s: a block did not decode (%v), so it carries no packets to measure", name, decodeErr)

				continue
			}
			measured += driveEveryPacket(t, name, decoded)
		}
	}

	// Killing mutation, actually run against this file: change committedPacket-
	// Measurements to 52, standing in for a fixture that silently stopped being
	// walked. This assertion then fails with
	//
	//	framing_test.go:1943: the sweep measured 53 packets, want 52
	//
	// which is the count doing its job; the per-packet assertion above cannot
	// see a packet that was never reached.
	if measured != committedPacketMeasurements {
		t.Fatalf("the sweep measured %d packets, want %d", measured, committedPacketMeasurements)
	}
	t.Logf("%d committed packets, every one read to its declared end", measured)
}

// driveEveryPacket walks data's bounded frames, drives the parser over each and
// reports how many it measured, failing the test for any packet the parser does
// not read to the end of.
//
// It stops at the first frame that is not bounded rather than reporting it: an
// unreadable header is the ordinary way a file of something other than packets
// ends this walk, and the fixtures include several files that are not packets at
// all.
func driveEveryPacket(t *testing.T, name string, data []byte) int {
	t.Helper()

	measured := 0
	for len(data) > 0 {
		frame, status := readPacketFrame(data)
		if status != frameBounded {
			return measured
		}
		body := data[frame.headerLen:]
		if frame.bodyLen > int64(len(body)) {
			return measured
		}

		segment := data[:int64(frame.headerLen)+frame.bodyLen]
		reader := bytes.NewReader(segment)
		_, parseErr := packet.Read(reader)
		// Killing mutation, actually run against this file: give Signature.parse
		// a `return nil` immediately after it reads the version octet, standing
		// in for a parser that stops short of a packet's end. This assertion then
		// fails with
		//
		//	framing_test.go:1930: keyring-outsider.asc: a tag 2 packet left 174 of 177 bytes unread (parse error <nil>)
		//
		// on the first signature packet in the directory, reported against the
		// caller because this is a helper, with every other one behind it.
		if unread := reader.Len(); unread != 0 {
			t.Errorf("%s: a tag %d packet left %d of %d bytes unread (parse error %v)",
				name, frame.tag, unread, len(segment), parseErr)
		}
		measured++
		data = body[frame.bodyLen:]
	}

	return measured
}

// TestV5ParsingStaysDisabled pins a build-tag default two entries of the
// enumeration on driveParser rest on.
//
// A v5 signature allocates nothing because Signature.parse refuses the version
// before reading a length, and a v5 private key's four-octet secret-material
// counter is unreachable because PublicKey.parse refuses the version first.
// Both refusals are conditional on packet.V5Disabled, which go-crypto sets under
// a `!v5` build constraint - so a build with -tags v5, or a release that flipped
// the default, reopens a length field this gate does not judge. Nothing in this
// repository passes that tag, and this is what says so at test time rather than
// in prose.
func TestV5ParsingStaysDisabled(t *testing.T) {
	t.Parallel()

	if !packet.V5Disabled {
		t.Fatalf("packet.V5Disabled = false, want true: two entries of driveParser's own enumeration assume it")
	}
}

// TestMPIStaysBoundedByItsTwoOctetLength pins the one number that makes a public
// key packet's cost a constant rather than a function of its input.
//
// driveParser's enumeration says tags 6 and 14 cost a constant because each
// algorithm reads a FIXED number of MPIs and one MPI is sized from a two-octet
// bit length. The fixed count is a property of go-crypto's own switch, which a
// bump can extend; the two octets are what bounds each read, and this asserts
// them through observable behavior rather than by reading the vendored source.
//
// The floor is asserted as well as the ceiling. Without it the test passes just
// as well against a parse that refused the bit length before allocating
// anything, which would leave the ceiling measuring a path the claim is not
// about.
//
// Not parallel: runtime.MemStats is process-wide, so a parallel test allocating
// alongside it would be counted here.
func TestMPIStaysBoundedByItsTwoOctetLength(t *testing.T) {
	// A public key packet body: version 4, a creation time, RSA, and then a
	// modulus MPI declaring the largest bit length its two octets can name. The
	// bytes it names are not present, so the read fails after the allocation.
	segment := newFormatPacket(packetTagPublicKey, []byte{0x04, 0x00, 0x00, 0x00, 0x00, 0x01, 0xff, 0xff})

	var err error
	allocated := measureAlloc(func() {
		err = checkPacketFraming(segment, keyringProfile())
	})
	// Killing mutation, actually run against this file: change encoding.MPI's
	// ReadFrom to size its buffer from a four-octet bit length instead of the
	// two-octet one, reading the two octets it has and shifting them 16 bits up.
	// This assertion then fails with
	//
	//	framing_test.go:2047: a 65535-bit MPI allocated 536864200 bytes, want at most 65536
	//
	// out of ten bytes of input.
	if allocated > mpiAllocCeiling {
		t.Fatalf("a %d-bit MPI allocated %d bytes, want at most %d", mpiDeclaredBits, allocated, mpiAllocCeiling)
	}
	if allocated < mpiAllocFloor {
		t.Fatalf("a %d-bit MPI allocated %d bytes, want at least the %d it names: the probe never reached the read",
			mpiDeclaredBits, allocated, mpiAllocFloor)
	}
	// What the gate made of it, below the measurement because the measurement is
	// the point: the parse fails and go-crypto consumes the rest, so nothing is
	// left unread and the walk has nothing to refuse.
	if err != nil {
		t.Fatalf("checkPacketFraming(a %d-bit MPI) = %v, want nil", mpiDeclaredBits, err)
	}
	t.Logf("a %d-bit MPI cost %d bytes over a %d-byte segment", mpiDeclaredBits, allocated, len(segment))
}

// admittedTagBodies are the bodies every admitted tag is measured over: nothing
// at all, one octet, 64 KiB of zeros, and a version 4 key prefix naming an
// algorithm no build knows.
//
// None of the four parses. That is what they are for: the drive judges where the
// reader stopped rather than what it made of the packet, so a body built to fail
// is the case that says a failed parse still consumes its frame.
//
// The fourth one's octets past its version are zero rather than arbitrary
// because it is walked as a signature body too, where the same bytes are two
// declared subpacket lengths: an over-declared one would be refused by the
// length arm, which is a verdict this sweep is not about.
func admittedTagBodies() [][]byte {
	return [][]byte{
		nil,
		{0x00},
		make([]byte, 64<<10),
		{0x04, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xff},
	}
}

// admittedKeyringTags is every tag keyringPacketTags admits, hand-spelled and
// named apart from the production constant so that a tag leaving that set cannot
// quietly leave this sweep with it.
func admittedKeyringTags() []int {
	return []int{2, 6, 12, 13, 14, 17, 21}
}

// TestEveryAdmittedTagIsReadToItsEnd is the cost half of driving every packet a
// profile admits.
//
// The gate refuses a packet whose parse leaves bytes unread, so a tag whose
// parser legitimately stopped short would turn ordinary key material into a
// refusal. This is that question asked of every admitted tag over four bodies
// built to fail the parse, through the production gate rather than through a
// harness of its own - which is what makes it the same verdict an operator's
// file would get.
//
// It replaces the sweep that measured the tags the drive used to skip, and
// inverts its purpose: that one licensed leaving six tags undriven, and the
// license was wrong. What survives is the measurement, now stating what driving
// them costs rather than what skipping them saved.
//
// The positive control is the under-read carrier through the identical call: it
// is refused, so "every row came back nil" is distinguishable from a gate whose
// residual check does nothing at all.
//
// Parallel: it counts verdicts rather than bytes allocated, so nothing running
// alongside it is counted in its answer.
func TestEveryAdmittedTagIsReadToItsEnd(t *testing.T) {
	t.Parallel()

	if err := checkPacketFraming(underReadCarrier(t), keyringProfile()); !errors.Is(err, errMalformedSignaturePacket) {
		t.Fatalf("positive control: checkPacketFraming(the under-read carrier) = %v, want the malformed-packet refusal", err)
	}

	for _, tag := range admittedKeyringTags() {
		// The hand-spelled list and the production set are each other's check: a
		// tag listed here that the profile no longer admits would be refused by
		// the allow-list, and the row would measure that instead.
		if !keyringPacketTags.has(tag) {
			t.Fatalf("tag %d is not in keyringPacketTags, so it is not one of the tags this test is about", tag)
		}
		for i, body := range admittedTagBodies() {
			segment := newFormatPacket(tag, body)
			// Killing mutation, actually run against this file: delete the
			// consumeAll call from packet.Read's error path, so a parse that
			// fails stops consuming the frame. 9 of the 28 rows then fail, one
			// of them with
			//
			//	framing_test.go:2139: tag 12 body 2 (65536 bytes): checkPacketFraming = malformed OpenPGP packet framing: the
			//	parser left 65536 of a tag 12 packet's 65542 bytes unread, want nil
			//
			// which is a real keyring's ring-trust packet becoming a refusal.
			// Every row of tags 13, 17 and 21 survives, consuming through an
			// io.ReadAll or an io.CopyN that runs whether the parse fails or not.
			if err := checkPacketFraming(segment, keyringProfile()); err != nil {
				t.Errorf("tag %d body %d (%d bytes): checkPacketFraming = %v, want nil", tag, i, len(body), err)
			}
		}
	}
}

// driveTagCase is one driven tag and a body built to cost the drive as much as
// that tag's parser can be made to spend at the given length.
type driveTagCase struct {
	name string
	body []byte
	tag  int
}

// driveTagCases builds the adversarial body for each admitted tag at n bytes,
// aimed at that tag's own costliest reachable path rather than at bytes the
// parser refuses on sight.
//
// Every tag a keyring profile admits is here, because every one of them is now
// driven: a row missing from this table is a parser whose cost nobody measured.
// The four that carry no structure of their own - trust, user id, user attribute
// and padding - are measured over a zero-filled body of the same length, which
// is what their parsers actually spend their time on, since none of them reads a
// declared length the way a key or a signature does.
//
// A body that fails at its version octet would measure nothing, so the key rows
// carry a real version, algorithm and MPI prefix.
func driveTagCases(n int) []driveTagCase {
	// A public key prefix go-crypto parses: version 4, a creation time, RSA, and
	// a modulus declaring the largest bit length two octets can name, which is
	// the costliest a key packet gets.
	wideMPI := []byte{0x04, 0x00, 0x00, 0x00, 0x00, 0x01, 0xff, 0xff}
	// A v4 signature whose hashed area is packed with the smallest subpackets
	// this walk accepts, so the parser builds one record per three input bytes.
	signature := packedSignatureBody(n)
	// A user attribute area declaring one image subpacket that fills the body,
	// which is that parser's own costliest shape at this length.
	attribute := padTo([]byte{0xff, lowOctet(n >> 24), lowOctet(n >> 16), lowOctet(n >> 8), lowOctet(n), 0x01}, n)

	return []driveTagCase{
		{name: "a signature packed with subpackets", tag: packetTagSignature, body: signature},
		{name: "a public key naming a 65535-bit MPI", tag: packetTagPublicKey, body: padTo(wideMPI, n)},
		{name: "a public subkey naming a 65535-bit MPI", tag: packetTagPublicSubkey, body: padTo(wideMPI, n)},
		{name: "a ring-trust packet", tag: packetTagTrust, body: padTo(nil, n)},
		{name: "a user id", tag: packetTagUserID, body: padTo(nil, n)},
		{name: "a user attribute declaring one image subpacket", tag: packetTagUserAttribute, body: attribute},
		{name: "a padding packet", tag: packetTagPadding, body: padTo(nil, n)},
	}
}

// driveTagSizes are the segment lengths every driven tag is measured at. Four
// sizes across two orders of magnitude are what make the claim about how the cost
// moves with the segment rather than about one length of it: a per-packet term
// that is really quadratic, or one keyed on a size class, shows up as a ratio
// that climbs across the rows instead of holding.
func driveTagSizes() []int {
	return []int{1 << 10, 8 << 10, 64 << 10, 256 << 10}
}

// TestDriveAllocationStaysLinearInTheSegment measures what driveParser's own
// enumeration claims: that no driven tag turns a segment into an allocation out
// of proportion to it.
//
// The enumeration is an argument about go-crypto's source, and a dependency bump
// can falsify it without touching a line of this repository. This is that
// argument as a measurement, one row per driven tag at each of four sizes,
// against a ceiling derived from the worst ratio any of them reached rather than
// picked.
//
// What it pins is the cost, not the drive's presence: a build with the drive
// deleted passes every row, since a row's segment then costs nothing at all. The
// carrier measurements are what pin the drive itself.
//
// Not parallel: runtime.MemStats is process-wide, so a parallel test allocating
// alongside it would be counted here.
func TestDriveAllocationStaysLinearInTheSegment(t *testing.T) {
	for _, size := range driveTagSizes() {
		for _, tc := range driveTagCases(size) {
			segment := newFormatPacket(tc.tag, tc.body)
			// The fixture ahead of the measurement: a row whose frame did not
			// read as a bounded packet of its own tag would never reach the
			// drive, and would measure the walk refusing it instead.
			frame, status := readPacketFrame(segment)
			if status != frameBounded || frame.tag != tc.tag || frame.bodyLen != int64(len(tc.body)) {
				t.Fatalf("%s at %d: readPacketFrame = tag %d, body %d, status %d, want tag %d, body %d, bounded",
					tc.name, size, frame.tag, frame.bodyLen, status, tc.tag, len(tc.body))
			}

			allocated := measureAlloc(func() {
				_ = checkPacketFraming(segment, keyringProfile())
			})
			ceiling := uint64(driveAllocFloor) + uint64(len(segment))*driveAllocPerByte
			// Killing mutation, actually run against this file: set
			// driveAllocPerByte to 1, standing in for a ratio nobody measured.
			// 7 of the 28 rows then fail, the costliest of them with
			//
			//	framing_test.go:2240: a signature packed with subpackets at 262144: tag 2 allocated 15478456 bytes over 262152, want at most 327688
			//
			// which is the packed-subpacket path's real 59 times the input,
			// against a ceiling that no longer holds it.
			if allocated > ceiling {
				t.Errorf("%s at %d: tag %d allocated %d bytes over %d, want at most %d",
					tc.name, size, tc.tag, allocated, len(segment), ceiling)
			}
			t.Logf("%-40s tag %2d: %9d bytes over %7d, %.1f times the segment",
				tc.name, tc.tag, allocated, len(segment), float64(allocated)/float64(len(segment)))
		}
	}
}

// TestAllowedTagsAreTheHandSpelledSet states all three sets as literals, so a
// tag joining or leaving one is a deliberate edit rather than a side effect.
//
// The literals are hand-spelled and named apart from the production constants
// they check, for the reason the octet fixtures here already give: an
// expectation built from the constant it checks cannot be killed by mutating
// that constant.
//
// The secret set is here for a reason the other two do not carry. Its two tags
// are in no allow-list, so nothing else in this package would notice one of them
// leaving it: the tag would simply be refused a sentence later by the
// allow-list, with the message that names the export an operator should have
// made instead silently gone.
//
// There is no set of driven tags to state. Every packet an allow-list admits is
// driven, which is what TestEveryAdmittedTagIsReadToItsEnd measures over the
// keyring set and what TestDriveAllocationStaysLinearInTheSegment prices.
//
// The out-of-range rows are the other half. packetTagSet.has takes an int and
// bound-checks nothing, resting on Go defining an over-wide shift as zero, so a
// tag no reader can produce has to read as absent rather than wrap onto a
// neighbour's bit.
func TestAllowedTagsAreTheHandSpelledSet(t *testing.T) {
	t.Parallel()

	const tagSpace = 64

	blobTags := []int{2}
	keyTags := []int{2, 6, 12, 13, 14, 17, 21}
	secretTags := []int{5, 7}

	for tag := range tagSpace {
		assertTagMembership(t, tag, blobTags, keyTags, secretTags)
	}

	for _, tag := range []int{tagSpace, tagSpace + packetTagSignature, 255} {
		if keyringPacketTags.has(tag) {
			t.Errorf("keyringPacketTags.has(%d) = true, want false: a tag outside the space read as a member", tag)
		}
	}

	// The ceilings are the profiles' other term, and they are hand-spelled here
	// for the same reason the sets are. A profile is not a set: two callers with
	// the same vocabulary and different ceilings are two profiles.
	for _, tc := range []struct {
		name    string
		profile packetProfile
		want    int
	}{
		{name: "signatureBlobProfile", profile: signatureBlobProfile(), want: 64},
		{name: "keyringProfile", profile: keyringProfile(), want: 4096},
	} {
		if tc.profile.maxPackets != tc.want {
			t.Errorf("%s().maxPackets = %d, want %d", tc.name, tc.profile.maxPackets, tc.want)
		}
	}
}

// assertTagMembership checks one tag against all three sets, and against the
// property that no tag is in both an allow-list and the secret set.
//
// It is a helper rather than four inline conditions so that the caller stays
// one loop over the tag space; the failures it reports are the caller's own
// line, which is what t.Helper buys.
func assertTagMembership(t *testing.T, tag int, blobTags, keyTags, secretTags []int) {
	t.Helper()

	// Killing mutation, actually run against this file: add tag 10 to
	// keyringPacketTags, which is the marker packet a carrier below rides in on.
	// This assertion then fails with
	//
	//	framing_test.go:2281: keyringPacketTags.has(10) = true, want false
	//
	// and one other place notices, the framingCases row named after that packet.
	// The carrier measurement does not: every admitted tag is driven, so the
	// drive refuses those same bytes for their unread tail.
	if got := keyringPacketTags.has(tag); got != slices.Contains(keyTags, tag) {
		t.Errorf("keyringPacketTags.has(%d) = %t, want %t", tag, got, !got)
	}
	if got := signatureBlobPacketTags.has(tag); got != slices.Contains(blobTags, tag) {
		t.Errorf("signatureBlobPacketTags.has(%d) = %t, want %t", tag, got, !got)
	}
	if got := secretKeyPacketTags.has(tag); got != slices.Contains(secretTags, tag) {
		t.Errorf("secretKeyPacketTags.has(%d) = %t, want %t", tag, got, !got)
	}
	// The two allow-lists and the secret set are disjoint by construction: an
	// admitted secret tag would be refused by an arm nobody reordered, which is a
	// state no message would explain.
	if keyringPacketTags.has(tag) && secretKeyPacketTags.has(tag) {
		t.Errorf("tag %d is both admitted into a keyring and refused as secret material", tag)
	}
}

// framedShape is one of the bounded header shapes a packet length can be spelled
// in, together with the body length that shape carries here.
type framedShape struct {
	name   string
	header []byte
	body   int
}

// framedShapes spells all six bounded length encodings by hand: the old format's
// one-, two- and four-octet forms and the new format's one-, two- and five-octet
// ones, each on the ring-trust tag.
//
// The tag is trust for a reason: it is the one admitted tag whose packet the
// parser consumes to its declared end whatever the body holds, which is what
// TestEveryAdmittedTagIsReadToItsEnd measures over four adversarial bodies. That
// leaves the header shape as the only variable, which is what the test is about.
//
// The lengths are hand-computed rather than taken from the production reader, so
// a mutation to that reader's arithmetic cannot reshape the fixture into
// agreeing with it. The two-octet forms carry a body no other form's arithmetic
// would produce for the same octets.
func framedShapes() []framedShape {
	return []framedShape{
		{name: "an old-format one-octet length", header: []byte{0xb0, 0x0a}, body: 10},
		{name: "an old-format two-octet length", header: []byte{0xb1, 0x01, 0x2c}, body: 300},
		{name: "an old-format four-octet length", header: []byte{0xb2, 0x00, 0x00, 0x01, 0x90}, body: 400},
		{name: "a new-format one-octet length", header: []byte{0xcc, 0x0a}, body: 10},
		{name: "a new-format two-octet length", header: []byte{0xcc, 0xc0, 0x08}, body: 200},
		{name: "a new-format five-octet length", header: []byte{0xcc, 0xff, 0x00, 0x00, 0x01, 0xf4}, body: 500},
	}
}

// TestFrameBoundsAgreeWithTheParser closes the gap the committed corpus leaves.
//
// Every packet in testdata carries a one-octet new-format length or an
// old-format two-octet one, so nothing there exercises whether this walk and
// go-crypto's own readHeader agree on where a packet ends. They have to: the
// drive cuts a segment at this walk's boundary and judges what the parser left
// inside it, so a walk that computed a longer packet than the parser reads would
// report every packet as under-read, and one that computed a shorter packet
// would hand the parser a segment cut inside a body and call whatever it managed
// to read exact.
//
// Each shape is measured by handing the parser the whole buffer, sentinel bytes
// behind the packet included, and asking how many bytes it took.
func TestFrameBoundsAgreeWithTheParser(t *testing.T) {
	t.Parallel()

	for _, shape := range framedShapes() {
		// Four bytes nothing should touch, so a parser reading past the declared
		// body has somewhere to read into.
		data := append(append([]byte{}, shape.header...), padTo(nil, shape.body+len(shape.header))[len(shape.header):]...)
		data = append(data, 0xde, 0xad, 0xbe, 0xef)

		frame, status := readPacketFrame(data)
		if status != frameBounded || frame.bodyLen != int64(shape.body) {
			t.Errorf("%s: readPacketFrame = body %d, status %d, want body %d, bounded", shape.name, frame.bodyLen, status, shape.body)

			continue
		}

		reader := bytes.NewReader(data)
		if _, err := packet.Read(reader); err == nil {
			t.Errorf("%s: packet.Read = nil error, want the unknown-tag error a trust packet produces", shape.name)

			continue
		}
		consumed := len(data) - reader.Len()
		// Killing mutation, actually run against this file: drop the tag octet
		// from newFormatFrame's five-octet header length, returning lenFieldFive
		// where it returns tagOctet+lenFieldFive. This assertion then fails with
		//
		//	framing_test.go:2419: a new-format five-octet length: the parser took 506 bytes, the walk computed 505
		//
		// and nothing else in the suite fails, since no committed fixture and no
		// other case here carries a five-octet length.
		if want := int64(frame.headerLen) + frame.bodyLen; int64(consumed) != want {
			t.Errorf("%s: the parser took %d bytes, the walk computed %d", shape.name, consumed, want)
		}
	}
}

// TestEmbeddedSignatureRecursionReachesCommittedMaterial is the positive control
// for the recursion, and it is the reason the signing-subkey fixture exists.
//
// Every other case that reaches checkSignatureBodyFraming's embedded arm is
// bytes a test spelled, so "the recursion refused that carrier" is
// indistinguishable from "the recursion refuses everything it descends into"
// without a real embedded signature to descend into and come back from.
//
// The probe is the depth cap used as an oracle. Walking a body from depth zero
// says whether it is well framed; walking the identical body from the cap turns
// the first descent into a refusal, so a body that refuses on the second walk and
// accepts on the first is one the walk really descended into. The fixture holds
// one signature of each kind - the subkey binding signature carries the
// cross-certification, the user id self-certification does not - so the two
// directions are controls for each other on the same file.
func TestEmbeddedSignatureRecursionReachesCommittedMaterial(t *testing.T) {
	t.Parallel()

	decoded, _, err := decodeArmorBlock(readFixture(t, subkeyFixture))
	if err != nil {
		t.Fatalf("decodeArmorBlock(%s) = %v, want nil", subkeyFixture, err)
	}

	bodies := signaturePacketBodies(decoded)
	if len(bodies) != subkeyFixtureSignatures {
		t.Fatalf("%s holds %d signature packets, want %d", subkeyFixture, len(bodies), subkeyFixtureSignatures)
	}

	descended := 0
	for i, body := range bodies {
		// The acceptance half: real material walks clean, embedded signature and
		// all, or the refusal below would say nothing.
		if walkErr := checkSignatureBodyFraming(body, 0); walkErr != nil {
			t.Fatalf("checkSignatureBodyFraming(%s signature %d) = %v, want nil", subkeyFixture, i, walkErr)
		}
		if errors.Is(checkSignatureBodyFraming(body, maxEmbeddedSignatureDepth), errMalformedSignaturePacket) {
			descended++
		}
	}

	// Killing mutation, actually run against this file: make
	// walkSignatureSubpackets return nil at its first line, so no subpacket area
	// is ever walked and no embedded signature is ever descended into. This
	// assertion then fails with
	//
	//	framing_test.go:2474: the walk descended into 0 of signing-subkey.asc's 2 signatures, want exactly 1
	//
	// while every acceptance above still passes, which is the difference between
	// a recursion that works and one that is never entered.
	if descended != 1 {
		t.Fatalf("the walk descended into %d of %s's %d signatures, want exactly 1", descended, subkeyFixture, len(bodies))
	}
}

// TestEmbeddedSignatureDepthIsCapped states the cap as a boundary rather than as
// a direction: a chain exactly as deep as the cap allows is walked, and one
// level more is refused.
//
// Neither row alone says where the cap sits. The accepting one is also what says
// the refusal is the depth and not the shape, since the two differ by one
// wrapping and nothing else.
func TestEmbeddedSignatureDepthIsCapped(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		levels     int
		wantRefuse bool
	}{
		{name: "a chain as deep as the cap allows", levels: maxEmbeddedSignatureDepth},
		{name: "a chain one level deeper", levels: maxEmbeddedSignatureDepth + 1, wantRefuse: true},
	} {
		data := nestedEmbeddedCarrier(tc.levels)
		err := checkPacketFraming(data, signatureBlobProfile())
		// Killing mutation, actually run against this file: change the depth test
		// from depth > maxEmbeddedSignatureDepth to depth >= it. The accepting
		// row then fails with
		//
		//	framing_test.go:2508: a chain as deep as the cap allows (54 bytes) refused = true
		//	(err malformed OpenPGP packet framing: a signature nests embedded signatures more than 4 deep), want refused = false
		//
		// while the refusing row passes throughout, which is what makes the pair a
		// boundary.
		if got := errors.Is(err, errMalformedSignaturePacket); got != tc.wantRefuse {
			t.Errorf("%s (%d bytes) refused = %t (err %v), want refused = %t", tc.name, len(data), got, err, tc.wantRefuse)
		}
	}
}

// signatureVersionCase is one signature packet differing from its neighbors in
// its version octet alone, and the verdict that octet earns it.
type signatureVersionCase struct {
	name       string
	body       []byte
	wantRefuse bool
}

// signatureVersionCases states what the version octet decides: whether the
// octets sitting at the length field's offset are a length at all.
//
// The three refusing rows and the three accepting ones are each other's positive
// control, since every row over-declares its hashed area by the same 0xffff or
// 0xffffffff and they differ in that one octet. Without the accepting rows the
// suite would pass just as well against a walk that judged every version; without
// the refusing ones, against a walk that judged none.
//
// The v3 row is the disclosure rather than a hand-built shape: its octets are a
// real v3 certification signature's - a version, the length of its hashed
// material, a signature type, a four-octet creation time, an eight-octet key id,
// the algorithm pair, two hash tag octets and one MPI - and the creation time is
// what sits where a v4 body spells its hashed length, which is why judging it as
// one refused the file.
func signatureVersionCases() []signatureVersionCase {
	return []signatureVersionCase{
		{
			name: "a v3 certification signature",
			body: []byte{
				0x03, 0x05, 0x10, 0x68, 0x9f, 0x3c, 0x21,
				0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
				0x01, 0x08, 0xaa, 0xbb, 0x00, 0x08, 0xff,
			},
		},
		{name: "a v4 signature over-declaring its hashed area", body: []byte{0x04, 0x13, 0x01, 0x08, 0xff, 0xff}, wantRefuse: true},
		{name: "a v5 signature over-declaring its hashed area", body: []byte{0x05, 0x13, 0x01, 0x08, 0xff, 0xff}, wantRefuse: true},
		{
			name:       "a v6 signature over-declaring its hashed area",
			body:       []byte{0x06, 0x13, 0x01, 0x08, 0xff, 0xff, 0xff, 0xff},
			wantRefuse: true,
		},
		{name: "a version 0 signature", body: []byte{0x00, 0x13, 0x01, 0x08, 0xff, 0xff}},
		{name: "a version 7 signature", body: []byte{0x07, 0x13, 0x01, 0x08, 0xff, 0xff}},
	}
}

// TestSignatureVersionDecidesWhetherLengthsAreJudged pins the gate that keeps
// this walk from reading a field that is not there.
//
// A version go-crypto refuses before it reads a length leaves no allocation for
// this walk to bound, and the octets at that offset belong to some other field
// entirely - so judging them is not conservative, it is wrong, and it refused a
// keyring carrying a v3 certification signature until this gate existed.
//
// v5 is judged with the versions the parser reads rather than waved through with
// the ones it refuses, because its refusal is a build-tag default rather than a
// property of the format: TestV5ParsingStaysDisabled pins that default, and this
// row is what makes the gate correct if it ever moves.
//
// Parallel: it allocates a handful of small errors and measures nothing.
func TestSignatureVersionDecidesWhetherLengthsAreJudged(t *testing.T) {
	t.Parallel()

	for _, tc := range signatureVersionCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			data := newFormatPacket(packetTagSignature, tc.body)
			err := checkPacketFraming(data, signatureBlobProfile())
			// Killing mutation, actually run against this file: give
			// subpacketLenSize's default arm the v4 width and a true, which is
			// the walk as it was before this gate existed. All three accepting
			// rows then fail, the v3 one with
			//
			//	framing_test.go:2594: checkPacketFraming(a v3 certification signature) refused = true (err malformed OpenPGP
			//	packet framing: a signature declares 40764 bytes of hashed subpackets with 16 present), want refused = false
			//
			// while every refusing row passes throughout - the mutation widens
			// what is judged, it does not stop the judging. The message is split
			// over two lines so this quote of it fits the line limit the
			// repository lints for.
			if got := errors.Is(err, errMalformedSignaturePacket); got != tc.wantRefuse {
				t.Fatalf("checkPacketFraming(%s) refused = %t (err %v), want refused = %t", tc.name, got, err, tc.wantRefuse)
			}
		})
	}
}

// admittedShapeAllocCeiling is what one admitted shape's whole walk, drive
// included, may allocate. Measured at 1800 bytes for the costliest of the ten,
// and at 10112 for the control below, so the ceiling sits between the two with
// room on either side.
const admittedShapeAllocCeiling = 4 << 10

// admittedShapes are the inputs this walk stops on with a nil answer because the
// parser behind it has no data-derived allocation left to make from them.
//
// They are collected from the two tables that already state those verdicts, so
// this list cannot drift from them: every accepting row of the truncation table
// and of the version table, plus the zero-length subpacket. What is added here is
// the other half of each verdict - that admitting the shape costs nothing - which
// no verdict assertion can see.
func admittedShapes() []framingCase {
	shapes := make([]framingCase, 0, len(truncationCases())+len(signatureVersionCases())+1)
	for _, tc := range truncationCases() {
		if !tc.wantRefuse {
			shapes = append(shapes, framingCase{name: tc.name, data: tc.data, profile: signatureBlobProfile()})
		}
	}
	for _, tc := range signatureVersionCases() {
		if !tc.wantRefuse {
			shapes = append(shapes, framingCase{
				name:    tc.name,
				data:    newFormatPacket(packetTagSignature, tc.body),
				profile: signatureBlobProfile(),
			})
		}
	}

	return append(shapes, framingCase{
		name:    "a signature subpacket of no length at all",
		data:    []byte{0xc2, 0x08, 0x04, 0x13, 0x01, 0x08, 0x00, 0x02, 0x00, 0x00},
		profile: signatureBlobProfile(),
	})
}

// TestAdmittedShapesAllocateNothing measures the half of this walk's contract
// that a verdict cannot show.
//
// Several arms stop with a nil answer rather than a refusal - a body too short
// for its own length prefix, a subpacket length field cut off, a signature
// version this parser refuses - and each of them rests on the same thing: that
// the parser behind the gate has nothing left to allocate from those bytes. That
// is a claim about go-crypto, so it is measured here instead of asserted in
// prose, and it is the claim that matters, since every one of these shapes is
// admitted rather than refused.
//
// The v3 row is the one with real history: judged as a v4 body, its creation
// time reads as 40764 bytes of hashed subpackets, and a parser that allocated
// from those octets would be doing so here with the gate's permission.
//
// The positive control is another admitted shape - a v4 signature declaring a
// 65535-bit MPI - measured through the identical call at 10064 bytes. It is what
// says this measurement can see an allocation at all, and it says so on an input
// the gate admits rather than one it refuses.
//
// Not parallel: runtime.MemStats is process-wide, so a parallel test allocating
// alongside it would be counted here.
func TestAdmittedShapesAllocateNothing(t *testing.T) {
	// A v4 signature carrying the one subpacket its parser insists on - a
	// creation time - an empty unhashed area, the two hash tag octets, and then
	// an RSA signature MPI declaring the largest bit length its own two octets
	// can name, which is an allocation the parser makes from bytes this walk
	// admits.
	control := newFormatPacket(packetTagSignature, []byte{
		0x04, 0x13, 0x01, 0x08, 0x00, 0x06,
		0x05, 0x02, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0xaa, 0xbb, 0xff, 0xff,
	})
	var controlErr error
	controlAlloc := measureAlloc(func() {
		controlErr = checkPacketFraming(control, signatureBlobProfile())
	})
	if controlErr != nil {
		t.Fatalf("positive control: checkPacketFraming(a v4 signature declaring a 65535-bit MPI) = %v, want nil", controlErr)
	}
	if controlAlloc <= admittedShapeAllocCeiling {
		t.Fatalf("positive control: an admitted 65535-bit MPI allocated %d bytes, want more than the %d ceiling: "+
			"this measurement cannot see an allocation", controlAlloc, admittedShapeAllocCeiling)
	}

	var worst uint64
	worstName := "none"
	for _, shape := range admittedShapes() {
		var err error
		allocated := measureAlloc(func() {
			err = checkPacketFraming(shape.data, shape.profile)
		})
		if err != nil {
			t.Errorf("checkPacketFraming(%s) = %v, want nil: this row is not one of the admitted shapes", shape.name, err)

			continue
		}
		if allocated > worst {
			worst, worstName = allocated, shape.name
		}
		// Killing mutation, actually run against this file: give
		// subpacketLenSize's default arm the v4 width and a true, so a version
		// this parser refuses has its octets judged as a length. That does not
		// fail this test - it fails the version table's own verdict rows - but
		// the inverse mutation does: give Signature.parse a v3 arm that reads the
		// two octets at the v4 offset as a hashed length and allocates from them,
		// and this row fails with
		//
		//	framing_test.go:2711: checkPacketFraming(a v3 certification signature) allocated 42712 bytes over 28, want at
		//	most 4096
		//
		// which is the 40764 those octets name, arriving inside the gate.
		if allocated > admittedShapeAllocCeiling {
			t.Errorf("checkPacketFraming(%s) allocated %d bytes over %d, want at most %d",
				shape.name, allocated, len(shape.data), admittedShapeAllocCeiling)
		}
	}
	t.Logf("the costliest of %d admitted shapes (%s) allocated %d bytes, against the control's %d",
		len(admittedShapes()), worstName, worst, controlAlloc)
}

// emptySubpacketPacket is the twelve-byte blob that panics go-crypto's own
// subpacket parser: a new-format signature header naming a ten-octet body, a v4
// signature prefix, a two-octet hashed area holding one subpacket whose declared
// length is 1, and that one octet being the exportable-certification type.
//
// parseSignatureSubpacket strips the type octet and then dispatches on it with
// the remainder empty, and that arm indexes the remainder with no length guard.
// It is spelled here rather than built, because what makes it a reproducer is
// exactly these octets.
//
//nolint:gochecknoglobals // a fixed fixture consumed by the tests, not mutable shared state
var emptySubpacketPacket = []byte{0xc2, 0x0a, 0x04, 0x13, 0x01, 0x08, 0x00, 0x02, 0x01, 0x04, 0x00, 0x00}

// emptySubpacketControl is the same packet with one octet of subpacket body
// added, which is the shortest subpacket the walk accepts and the shape every
// one of the 210 subpackets in the committed corpus has or exceeds.
//
//nolint:gochecknoglobals // a fixed fixture consumed by the tests, not mutable shared state
var emptySubpacketControl = []byte{0xc2, 0x0b, 0x04, 0x13, 0x01, 0x08, 0x00, 0x03, 0x02, 0x04, 0x01, 0x00, 0x00}

// TestEmptySubpacketBodyIsRefused pins the one refusal here that is about a
// crash rather than about an allocation.
//
// The two fixtures differ by one octet of subpacket body and by the two lengths
// that count it, so the control is not merely another packet that passes: it is
// this packet with the defect removed, which is what says the refusal is the
// empty body rather than anything else about these bytes. The control also has
// to reach the parser and come back, since the walk accepting it is worth
// nothing if the drive behind it then refuses the packet for some other reason.
//
// Parallel: it allocates two small errors and measures nothing.
func TestEmptySubpacketBodyIsRefused(t *testing.T) {
	t.Parallel()

	// The whole message hand-spelled, and apart from the production format
	// string it checks, so that a mutation to that string cannot reshape the
	// expectation into agreeing with it.
	const want = "malformed OpenPGP packet framing: a signature subpacket carries a type octet and no body"

	if err := checkPacketFraming(emptySubpacketControl, signatureBlobProfile()); err != nil {
		t.Fatalf("positive control: checkPacketFraming(a one-octet subpacket body) = %v, want nil", err)
	}

	err := checkPacketFraming(emptySubpacketPacket, signatureBlobProfile())
	// Killing mutation, actually run against this file: delete the
	// len(contents) == subpacketTypeOctet arm from walkSignatureSubpackets, so
	// the packet reaches the drive. The run does not fail an assertion at all -
	// it dies, which is the defect:
	//
	//	--- FAIL: TestEmptySubpacketBodyIsRefused (0.00s)
	//	panic: runtime error: index out of range [0] with length 0 [recovered, repanicked]
	//
	// with parseSignatureSubpacket at the top of the stack below driveParser,
	// while the control above still passes: one octet of body is all that arm
	// ever needed. The stack frames are described rather than quoted, since a
	// frame naming a vendored file and a line in it is a citation into a
	// production file, which this repository bans.
	if err == nil || err.Error() != want {
		t.Fatalf("checkPacketFraming(a subpacket with no body) = %v, want %q", err, want)
	}
}

// subpacketFormCase is one subpacket area whose first subpacket spells its
// length in one of the three forms, and the verdict the area earns.
type subpacketFormCase struct {
	name       string
	area       []byte
	wantRefuse bool
}

// subpacketFormCases exercises all three of readSubpacketLength's length forms
// inside an area whose verdict depends on that length being read exactly right.
//
// Each area is one subpacket of the form under test, filled with 0xff and
// carrying a type that is not the embedded signature, followed by a subpacket
// the walk refuses: an embedded signature whose own body declares 65535 bytes of
// hashed subpackets over the three it holds. So the refusal is reached only by a
// walk that landed exactly on that second subpacket, and any arithmetic that
// lands short of it reads a 0xff fill octet as a five-octet length field naming
// 0xffffffff bytes, which overruns the area and ends the walk with a nil error.
// Landing long overruns it directly. Either way the verdict flips, which is what
// makes these rows a pin on the arithmetic rather than on the walk merely
// reaching the end.
//
// The positive control is the same one-octet area with a well-formed subpacket
// in place of the refused one, so the fixture is shown capable of acceptance
// with the length arithmetic unchanged. It covers the one-octet form alone, and
// the two wide rows have no accepting counterpart of their own; what shows those
// two fixtures can be accepted is their own mutations, each of which flips its
// row from refused to accepted rather than to some other refusal.
func subpacketFormCases() []subpacketFormCase {
	// An embedded signature subpacket whose inner body over-declares its hashed
	// area, and a well-formed subpacket of the shortest length the walk accepts.
	refusing := []byte{0x0a, 0x20, 0x04, 0x13, 0x01, 0x08, 0xff, 0xff, 0x00, 0x00, 0x00}
	accepting := []byte{0x02, 0x0b, 0x09}

	return []subpacketFormCase{
		{name: "a one-octet subpacket length", area: subpacketFormArea([]byte{0x0a}, 10, refusing), wantRefuse: true},
		{name: "a two-octet subpacket length", area: subpacketFormArea([]byte{0xc0, 0x22}, 226, refusing), wantRefuse: true},
		{
			name:       "a five-octet subpacket length",
			area:       subpacketFormArea([]byte{0xff, 0x00, 0x00, 0x01, 0x2c}, 300, refusing),
			wantRefuse: true,
		},
		{name: "the same area ending on a subpacket with a body", area: subpacketFormArea([]byte{0x0a}, 10, accepting)},
	}
}

// subpacketFormArea builds one subpacket area: a length field spelled by hand,
// contents of that many octets whose first names a type this walk does not
// descend into and whose rest is 0xff fill, and then tail.
//
// The length fields are hand-spelled and named apart from the production
// arithmetic they exercise, for the reason every octet fixture here is: an
// expectation built from the code it checks cannot be killed by mutating that
// code. 226 and 300 are the shortest lengths their forms can spell that no other
// form's arithmetic produces from the same octets.
func subpacketFormArea(lengthField []byte, contents int, tail []byte) []byte {
	area := make([]byte, 0, len(lengthField)+contents+len(tail))
	area = append(area, lengthField...)
	area = append(area, 0x0b)
	for range contents - 1 {
		area = append(area, 0xff)
	}

	return append(area, tail...)
}

// TestSubpacketLengthFormsDecideTheVerdict covers the two wide forms of
// readSubpacketLength, which nothing else in this package reaches.
//
// Every committed subpacket and every hand-built area elsewhere spells its
// length in one octet, so both wide arms could have their arithmetic changed -
// the two-octet form's +192 bias dropped, the five-octet form's offset moved by
// one - with the rest of the suite still green.
//
// Parallel: it allocates a few kilobytes of fixture and measures nothing.
func TestSubpacketLengthFormsDecideTheVerdict(t *testing.T) {
	t.Parallel()

	for _, tc := range subpacketFormCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			body := make([]byte, 0, 6+len(tc.area))
			body = append(body, 0x04, 0x13, 0x01, 0x08, lowOctet(len(tc.area)>>8), lowOctet(len(tc.area)))
			body = append(body, tc.area...)

			err := checkPacketFraming(newFormatPacket(packetTagSignature, body), signatureBlobProfile())
			// Killing mutations, each actually run against this file, one per
			// wide form. Drop the subpacketOneOctetMax bias from
			// readSubpacketLength's two-octet form, so it computes 34 where it
			// computed 226; exactly its own row then fails with
			//
			//	framing_test.go:2881: checkPacketFraming(a two-octet subpacket length) refused = false (err <nil>), want refused = true
			//
			// and move the five-octet form's slice one octet earlier, so it reads
			// the marker as the high octet of its length; exactly that row fails
			// instead, with the same wording. Nothing else in the package notices
			// either, since no committed subpacket spells its length in more than
			// one octet.
			if got := errors.Is(err, errMalformedSignaturePacket); got != tc.wantRefuse {
				t.Fatalf("checkPacketFraming(%s) refused = %t (err %v), want refused = %t", tc.name, got, err, tc.wantRefuse)
			}
		})
	}
}

// refusedByTheGate reports whether err is one of the two verdicts this gate
// produces for a carrier.
//
// The second exists because one carrier's refusal is worded differently at the
// two entry points, deliberately: a secret key packet reaching LoadKeyring earns
// the message naming the export an operator should have made instead, which
// carries errSecretKeyPacket and not the framing sentinel, while the same bytes
// reaching checkOne carry both. Asserting one sentinel alone would either fail
// those rows or take the wording away.
func refusedByTheGate(err error) bool {
	return errors.Is(err, errMalformedSignaturePacket) || errors.Is(err, errSecretKeyPacket)
}

// carrierFinding is one carrier that got past the gate through one entry point,
// kept rather than fataled on so the failure can name every one that did and
// what each of them cost.
type carrierFinding struct {
	carrier   string
	entry     string
	allocated uint64
}

// framingCarrier is one input built to reach go-crypto's data-derived allocation
// through a gate that judges declared lengths alone.
type framingCarrier struct {
	name string
	data []byte
}

// framingCarriers builds every input measured to reach go-crypto's data-derived
// allocation past a gate that judges declared lengths alone. Each was verified
// to do so against that gate before this one replaced it.
//
// The marker carrier hides the amplifying signature packet inside the body of a
// tag 10 packet, which go-crypto's own reader skips, so a walk of declared
// lengths steps over the marker and hands that signature to the parser: measured
// at 4294974592 bytes from fifteen. The under-read carrier is a real public key
// body re-framed to declare ten bytes more than it holds, with the amplifier in
// the bytes the parser never reads, so a gate that trusts its own boundaries
// leaves go-crypto's reader inside the body and lets it read the amplifier as
// the next packet: 4294975376 bytes from sixty-seven. The embedded carrier is a
// v4 signature whose own two lengths are honest and whose hashed area carries an
// embedded v6 signature naming four octets of subpacket length: 4294969944 bytes
// from eighteen. The nested one is a chain past the depth cap, and the v4 one
// declares 65535 bytes of hashed subpackets over two: 67360 bytes from ten.
//
// The amplifying blob is the case the gate was first built for, kept in the
// table so a change that closes the newer holes and reopens the oldest one is
// caught by the same measurement.
//
// The dummy-S2K carrier is the newest and the one that falsified an argument
// rather than an implementation: a tag 5 packet whose parse SUCCEEDS at a
// GNU-dummy S2K, ten bytes before the end of its declared body, with the
// amplifier in those ten. Measured against the walk that left tags 5 and 7
// undriven on the argument that their parse always consumes the frame, those 36
// bytes walked clean and left readEntities allocating 4294980800.
//
// No row here is aimed at one arm alone, and the mutations below say which arm
// each of them is really pinned by. The marker carrier now has two: the
// allow-list refuses tag 10, and since every packet a profile admits is driven,
// the drive would refuse the same bytes anyway - measured, judgePacket answers
// them with "the parser left 10 of a tag 10 packet's 15 bytes unread", because
// go-crypto's marker parse reads the three octets it expects and stops. So
// deleting either arm alone leaves this row refused and cheap, and what pins the
// allow-list is the pair of framingCases rows at the top of this file rather
// than a measurement here.
func framingCarriers(tb testing.TB) []framingCarrier {
	tb.Helper()

	return []framingCarrier{
		{
			name: "a marker packet carrying the amplifier",
			data: []byte{0xca, 0x0d, 0x50, 0x47, 0x50, 0xc2, 0x08, 0x06, 0x13, 0x01, 0x08, 0xff, 0xff, 0xff, 0xff},
		},
		{name: "a public key packet under-reading the amplifier", data: underReadCarrier(tb)},
		{name: "a secret key packet under-reading the amplifier", data: dummyS2KCarrier()},
		{
			name: "a v4 signature embedding a v6 one",
			data: []byte{0xc2, 0x10, 0x04, 0x00, 0x01, 0x08, 0x00, 0x0a, 0x09, 0x20, 0x06, 0x19, 0x01, 0x08, 0xff, 0xff, 0xff, 0xff},
		},
		{name: "a v6 signature declaring 4 GiB", data: synthesizedBlobs[amplifyingBlob]},
		{name: "a chain of embedded signatures", data: nestedEmbeddedCarrier(maxEmbeddedSignatureDepth + 1)},
		{name: "a v4 signature declaring 65535 bytes", data: []byte{0xc2, 0x08, 0x04, 0x13, 0x01, 0x08, 0xff, 0xff, 0xff, 0xff}},
	}
}

// TestEveryCarrierIsRefusedCheaplyAtBothEntryPoints measures every carrier
// through every way one can arrive: as a keyring file and as a detached
// signature, binary and armored.
//
// The two entry points are covered because a keyring reaches the parser through
// one and a signature through the other, and neither routes through the other.
// The two encodings are covered because armor is base64 text carrying no framing
// to walk, so an armored row exercises the gate only once a decode has happened -
// which is what a gate applied to raw file bytes would miss.
//
// Both questions are accumulated across the table and reported once each rather
// than stopping it, so the failure names which carriers got through rather than
// whichever one happened to be first.
//
// Not parallel: runtime.MemStats is process-wide, so a parallel test allocating
// alongside it would be counted here.
func TestEveryCarrierIsRefusedCheaplyAtBothEntryPoints(t *testing.T) {
	kr := loadTestKeyring(t, keyringFixture)
	manifest := readFixture(t, manifestAFixture)
	// The positive control, ahead of the table and on committed fixtures rather
	// than hand-built shapes: real keyrings still load and real signatures still
	// verify, so a finding below is a carrier's own bytes and not a gate that
	// refuses everything.
	requireFramingFixturesStillWork(t, kr, manifest)

	var overCeiling, wrongVerdict []carrierFinding
	// Declared once rather than per iteration: it closes over the two
	// accumulators, and a closure rebuilt inside the loop would allocate one per
	// carrier for no gain.
	record := func(carrier, entry string, allocated uint64, err error) {
		finding := carrierFinding{carrier: carrier, entry: entry, allocated: allocated}
		if allocated > framingAllocCeiling {
			overCeiling = append(overCeiling, finding)
		}
		if !refusedByTheGate(err) {
			wrongVerdict = append(wrongVerdict, finding)
		}
	}

	// One leaf, rewritten per row: LoadKeyring takes a path and every write
	// truncates, so the table costs one file rather than one per carrier.
	path := filepath.Join(t.TempDir(), "carrier.gpg")
	for _, carrier := range framingCarriers(t) {
		for _, enc := range []struct {
			entry string
			data  []byte
		}{
			{entry: "LoadKeyring binary", data: carrier.data},
			{entry: "LoadKeyring armored", data: armorEncode(t, openpgp.PublicKeyType, carrier.data)},
		} {
			// #nosec G703 -- the directory is this test's own t.TempDir and the
			// leaf is a constant; no part of the path comes from outside this
			// function.
			if err := os.WriteFile(path, enc.data, keyringFileMode); err != nil {
				t.Fatalf("write %s as %s: %v", carrier.name, enc.entry, err)
			}

			var loadErr error
			allocated := measureAlloc(func() {
				_, loadErr = LoadKeyring(path)
			})
			record(carrier.name, enc.entry, allocated, loadErr)
		}

		for _, enc := range []struct {
			entry string
			data  []byte
		}{
			{entry: "checkOne binary", data: carrier.data},
			{entry: "checkOne armored", data: armorEncode(t, openpgp.SignatureType, carrier.data)},
		} {
			var checkErr error
			allocated := measureAlloc(func() {
				_, checkErr = checkOne(manifest, enc.data, kr)
			})
			record(carrier.name, enc.entry, allocated, checkErr)
		}
	}

	// Killing mutations, each actually run against this file, and each unguarding
	// a different carrier - which is the whole reason the table has six rows
	// rather than one. All three are quoted cut after their first element.
	//
	// Give driveParser a `return nil` at its first line, so this walk's
	// boundaries are never checked against the parser's:
	//
	//	framing_test.go:3083: 2 carriers allocated more than 1048576 bytes:
	//	[{carrier:a public key packet under-reading the amplifier entry:LoadKeyring binary allocated:4294984432} ...]
	//
	// Restore the v6-only reading of the version octet in subpacketLenSize, so
	// every version but v6 has its declared lengths unjudged again:
	//
	//	framing_test.go:3083: 4 carriers allocated more than 1048576 bytes:
	//	[{carrier:a v4 signature embedding a v6 one entry:LoadKeyring binary allocated:8589949088} ...]
	//
	// Restore the whole walk the secret carrier was built against - the arm gone
	// from judgeHeader, tags 5 and 7 back in keyringPacketTags, and judgePacket
	// leaving those two undriven - which takes all three, since either of the
	// first two alone leaves the drive refusing the same bytes:
	//
	//	framing_test.go:3083: 2 carriers allocated more than 1048576 bytes:
	//	[{carrier:a secret key packet under-reading the amplifier entry:LoadKeyring binary allocated:4294979104} ...]
	//
	// The second names the embedded carrier through all four entry points, at
	// about 8 GiB apiece: the drive spends the first 4 on a carrier the walk no
	// longer refuses, and the reader behind the gate spends the second 4 on the
	// same bytes. Deleting the allow-list arm is NOT among these, and that is the
	// widened drive's doing: the marker carrier it used to unguard is refused by
	// the drive for its unread tail either way.
	if len(overCeiling) > 0 {
		t.Errorf("%d carriers allocated more than %d bytes: %+v", len(overCeiling), framingAllocCeiling, overCeiling)
	}
	// Which verdict came with that cheap outcome, asked separately and below the
	// measurement for the reason the measurements above give: a carrier refused
	// cheaply under some other error would leave this package's own verdict
	// unaccounted for. It is also where the two carriers that cost nothing to
	// leave unguarded are caught: under the second mutation above it names the
	// chain of embedded signatures and the v4 signature as well, which are
	// refusals rather than allocations.
	if len(wrongVerdict) > 0 {
		t.Errorf("%d carriers were not refused as malformed framing: %+v", len(wrongVerdict), wrongVerdict)
	}
}

// TestUnboundedFramingIsJudgedBeforeTheTag pins the order of the two arms that
// can both refuse one packet.
//
// A partial length on tag 1 is a framing this walk cannot bound, on a tag
// keyringPacketTags does not admit, so both arms have something to say about it
// and only their order decides which one answers. The table at the top of this
// file already asserts that these four bytes are refused, and says in prose that
// the framing is judged first; this is that order as a verdict, which is the
// claim the frameUnbounded arm makes about itself - the tag of a packet whose
// end cannot be found decides nothing.
//
// Parallel, unlike the measurements here: it allocates one error value and
// measures nothing, so nothing running alongside it is counted in its answer.
func TestUnboundedFramingIsJudgedBeforeTheTag(t *testing.T) {
	t.Parallel()

	// The whole message hand-spelled, and apart from the production format string
	// it checks, so that a mutation to that string cannot reshape the expectation
	// into agreeing with it.
	const want = "malformed OpenPGP packet framing: a tag 1 packet carries " +
		"a new-format partial length, which declares no body length at all"

	err := checkPacketFraming([]byte{0xc1, 0xe0, 0x00, 0x00}, keyringProfile())
	// Killing mutation, actually run against this file: move the allow-list arm
	// above the frameUnbounded one in checkPacketFraming, leaving the
	// frameUnreadable early return ahead of both. This assertion then fails with
	//
	//	framing_test.go:3131: checkPacketFraming(a partial length on tag 1) = malformed OpenPGP packet framing: a tag 1 packet has
	//	no place in this stream, want "malformed OpenPGP packet framing: a tag 1 packet carries a new-format partial length, which
	//	declares no body length at all"
	//
	// and nothing else in the package notices the reorder, which is what makes
	// this the only place the order is pinned rather than described.
	if err == nil || err.Error() != want {
		t.Fatalf("checkPacketFraming(a partial length on tag 1) = %v, want %q", err, want)
	}
}

const (
	// secretKeyHalfBits is how many bits each of the two factors of the CPU
	// carrier's modulus carries. Two of them make a modulus of 65520 bits, just
	// under the 65535 an MPI's two-octet bit length can name, so the packet built
	// from them is the costliest an RSA secret key packet can be.
	secretKeyHalfBits = 32760
	// secretKeyProofHalfBits is the size the parse cost is actually measured at,
	// where it is a fifth of a second rather than twelve of them.
	secretKeyProofHalfBits = 8192

	// secretKeyRefusalCeiling is what refusing the CPU carrier may take. Measured
	// across runs between 104.125µs and 272.125µs, under the race detector and
	// without it, against the 11.936526375s the same bytes cost when the parser
	// saw them - so the ceiling sits four orders of magnitude above what the
	// refusal spends and one below what the defect did, which is the margin a
	// wall-clock assertion needs to be about the defect rather than about the
	// machine.
	secretKeyRefusalCeiling = 2 * time.Second
	// secretKeyParseFloor is what the parse of the smaller key must exceed for
	// the proof below it to have measured the path it names. Measured at
	// 199.014584ms, so the floor holds an order of magnitude of headroom for a
	// slower machine while staying far above the microseconds a refusal costs.
	secretKeyParseFloor = 10 * time.Millisecond

	// dummyS2KBodyLen is how many octets of that carrier's body the parse reads
	// before it returns at the GNU-dummy S2K: a v4 key prefix, two 8-bit MPIs,
	// the s2k usage octet, a cipher, and the six-octet dummy s2k.
	dummyS2KBodyLen = 20
)

// TestSecretKeyPacketsAreRefusedAtTheGate is the whole case for refusing tags 5
// and 7 at the header, in the two terms that made it worth doing: a desync the
// drive cannot catch, and a per-packet cost no ceiling here bounds.
//
// The desync half is the one that moves a verdict. PrivateKey.parse returns at a
// GNU-dummy S2K, ahead of the read that would have consumed the body, so this
// packet's parse SUCCEEDS while leaving its declared body unread - which is
// exactly what driveParser exists to refuse, and which the walk that left these
// two tags undriven let through. The parse is measured here rather than
// described, because "the parser stops early" is the claim the whole finding
// turned on.
//
// The cost half is the one that moves nothing but time. parsePrivateKey is
// reachable only through these two tags, and it ends in RSA key validation over
// big.Ints the packet's own MPI lengths size, which no size or packet ceiling in
// this package bounds: measured with this builder, one packet.Read took
// 1.235792ms on a 541-byte packet, 35.948ms on 2077, 199.014584ms on 4125,
// 1.514309167s on 8221 and 11.936526375s on 16409, each of them ONE packet. The
// ladder is what says the term is the MPI's rather than the file's - about eight
// times the cost for twice the bits - and the last rung is a packet a keyring
// file may carry 4089 copies of inside keyringMaxSize, which is 13 hours of CPU
// under a packet ceiling that permits 4096 of them.
//
// The proof and the refusal are measured at different sizes on purpose. What has
// to be shown is that the parse spends real time on bytes of this shape, which a
// fifth of a second says as well as twelve seconds do; what has to be measured
// is the refusal of the largest such packet, which is where the saving is.
//
// Not parallel: it measures elapsed time, so a parallel test competing for a
// core would be counted in its answer.
func TestSecretKeyPacketsAreRefusedAtTheGate(t *testing.T) {
	// The positive control, on the same builder and the same path: the public
	// half of the same key material, framed as a public key packet, walks clean.
	// So a refusal below is the secret-key tag rather than anything about a key
	// packet, an MPI, or this builder's octets.
	control := newFormatPacket(packetTagPublicKey, rsaPublicKeyBody(secretKeyProofHalfBits))
	if err := checkPacketFraming(control, keyringProfile()); err != nil {
		t.Fatalf("positive control: checkPacketFraming(a public key packet of the same key) = %v, want nil", err)
	}

	carrier := requireDummyS2KDesync(t)

	// The CPU carrier is first because it is the row that can fail on the time
	// rather than on the message, and only a row reached at all can do that.
	dir := t.TempDir()
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{name: "an RSA secret key of the widest MPIs one can carry", data: rsaSecretKeyPacket(t, secretKeyHalfBits)},
		{name: "a secret key packet parsing to a dummy S2K", data: carrier},
		{name: "the same body on the secret subkey tag", data: newFormatPacket(packetTagPrivateSubkey, carrier[6:])},
	} {
		path := filepath.Join(dir, "secret.gpg")
		// #nosec G703 -- dir is this test's own t.TempDir and the leaf is a
		// constant; no part of the path comes from outside this function.
		if err := os.WriteFile(path, tc.data, keyringFileMode); err != nil {
			t.Fatalf("write %s: %v", tc.name, err)
		}

		start := time.Now()
		_, err := LoadKeyring(path)
		elapsed := time.Since(start)
		// Killing mutation, actually run against this file: delete the
		// secretKeyPacketTags arm from judgeHeader AND put tags 5 and 7 back
		// into keyringPacketTags, which is the walk as it was when this packet
		// reached parsePrivateKey. The first row then fails here with
		//
		//	framing_test.go:3242: LoadKeyring(an RSA secret key of the widest MPIs one can carry) took 24.808035583s,
		//	want at most 2s
		//
		// which is one file of one packet spending that. The measurement sits
		// above the two verdict assertions because it is what refusing at the
		// header is for, and because it is the one this mutation reaches: under
		// it the parse fails as an invalid key rather than yielding an entity, so
		// the message below would answer for the parser instead.
		if elapsed > secretKeyRefusalCeiling {
			t.Fatalf("LoadKeyring(%s) took %v, want at most %v", tc.name, elapsed, secretKeyRefusalCeiling)
		}
		// Killing mutation, actually run against this file, and a smaller one:
		// delete that same arm alone, so the two tags fall through to an
		// allow-list that no longer admits them. Every row then fails here, the
		// first with
		//
		//	framing_test.go:3257: LoadKeyring(an RSA secret key of the widest MPIs one can carry) = keyring could not be
		//	read: "...secret.gpg": malformed OpenPGP packet framing: a tag 5 packet has no place in this stream, want the
		//	secret-material message
		//
		// which is the refusal an operator cannot act on standing where the one
		// naming the export belongs. The quoted path is elided at the temporary
		// directory that run wrote into.
		if err == nil || !strings.Contains(err.Error(), "secret key material") {
			t.Fatalf("LoadKeyring(%s) = %v, want the secret-material message", tc.name, err)
		}
		if !errors.Is(err, helpers.ErrKeyringUnreadable) {
			t.Fatalf("LoadKeyring(%s) error = %v, want the unreadable sentinel", tc.name, err)
		}
		t.Logf("%-48s %6d bytes refused in %v", tc.name, len(tc.data), elapsed)
	}

	// What that refusal saves, measured on the same shape at a size worth
	// waiting for: the parse of a secret key packet this package no longer
	// performs. Without this the ceiling above would pass just as well against a
	// parser that never spent anything on these bytes at all.
	proof := rsaSecretKeyPacket(t, secretKeyProofHalfBits)
	start := time.Now()
	_, _ = packet.Read(bytes.NewReader(proof))
	spent := time.Since(start)
	if spent < secretKeyParseFloor {
		t.Fatalf("parsing a %d-byte secret key packet took %v, want at least %v: the proof never reached key validation",
			len(proof), spent, secretKeyParseFloor)
	}
	t.Logf("the parser spent %v on one %d-byte secret key packet, which is what the refusal above no longer pays", spent, len(proof))
}

// requireDummyS2KDesync returns the dummy-S2K carrier, having measured that it
// still is one: the parse must SUCCEED and must leave bytes behind, or the
// finding the secret-key arm answers is not what these bytes are any more.
//
// It is a helper rather than four lines in its caller because what it asserts is
// a property of the fixture rather than of the gate, and because the caller is
// the one measuring elapsed time.
func requireDummyS2KDesync(t *testing.T) []byte {
	t.Helper()

	carrier := dummyS2KCarrier()
	reader := bytes.NewReader(carrier)
	_, parseErr := packet.Read(reader)
	if parseErr != nil || reader.Len() == 0 {
		t.Fatalf("the dummy-S2K carrier parsed with error %v leaving %d of %d bytes unread, want a nil error and a residual",
			parseErr, reader.Len(), len(carrier))
	}
	t.Logf("the parser read the dummy-S2K carrier with a nil error and left %d of its %d bytes unread",
		reader.Len(), len(carrier))

	return carrier
}

// dummyS2KCarrier builds a tag 5 packet whose parse succeeds while leaving its
// declared body unread, with the amplifying signature packet in the bytes nobody
// reads.
//
// The octets are hand-spelled: a v4 key packet header, an 8-bit modulus and
// exponent, the s2k usage octet naming a checksum, a cipher, and then the GNU
// dummy s2k - mode 101 followed by "GNU" and 1 - which is where PrivateKey.parse
// returns.
func dummyS2KCarrier() []byte {
	amplifier := synthesizedBlobs[amplifyingBlob]
	body := make([]byte, 0, dummyS2KBodyLen+len(amplifier))
	body = append(body,
		0x04, 0x00, 0x00, 0x00, 0x00, 0x01,
		0x00, 0x08, 0xff,
		0x00, 0x08, 0x03,
		0xfe,
		0x09,
		0x65, 0x02, 'G', 'N', 'U', 0x01,
	)

	return newFormatPacket(packetTagPrivateKey, append(body, amplifier...))
}

// rsaPublicKeyBody builds the public half of the key rsaSecretKeyPacket builds:
// a v4 key packet body carrying a modulus of two halfBits-bit factors and an
// exponent.
func rsaPublicKeyBody(halfBits int) []byte {
	p, q := patternFactor(halfBits, 251), patternFactor(halfBits, 241)

	body := []byte{0x04, 0x00, 0x00, 0x00, 0x00, 0x01}
	body = append(body, encodeMPI(new(big.Int).Mul(p, q))...)

	return append(body, encodeMPI(big.NewInt(65537))...)
}

// rsaSecretKeyPacket builds an unencrypted RSA secret key packet whose modulus
// is the product of two halfBits-bit factors, which is what drives go-crypto
// into key validation over numbers of that size.
//
// The factors are a fixed pattern rather than random or prime, because what the
// measurement needs is their SIZE: rsa.Validate precomputes before it decides
// the key is invalid, so the arithmetic is paid whatever the numbers turn out to
// be. Building them deterministically is what keeps the timing reproducible and
// keeps a prime search of this size out of a unit test.
func rsaSecretKeyPacket(t *testing.T, halfBits int) []byte {
	t.Helper()

	p, q := patternFactor(halfBits, 251), patternFactor(halfBits, 241)

	body := rsaPublicKeyBody(halfBits)
	// The s2k usage octet naming no encryption, so the secret MPIs behind it are
	// read in the clear and the 16-bit checksum is the only thing between them
	// and parsePrivateKey.
	body = append(body, 0x00)

	// A one-octet private exponent keeps the packet at the size of its two
	// factors, which is what the ladder in this test's own doc comment measures.
	secret := encodeMPI(big.NewInt(3))
	secret = append(secret, encodeMPI(p)...)
	secret = append(secret, encodeMPI(q)...)
	var sum uint16
	for _, b := range secret {
		sum += uint16(b)
	}
	secret = append(secret, lowOctet(int(sum>>8)), lowOctet(int(sum)))

	return newFormatPacket(packetTagPrivateKey, append(body, secret...))
}

// patternFactor builds one halfBits-bit odd number from a repeating pattern of
// period bytes, with the top bit set so it is exactly that wide.
func patternFactor(halfBits, period int) *big.Int {
	buf := make([]byte, halfBits/8)
	for i := range buf {
		buf[i] = lowOctet(i%period) | 0x01
	}
	buf[0] |= 0x80

	return new(big.Int).SetBytes(buf)
}

// encodeMPI renders n as an OpenPGP multiprecision integer: a two-octet bit
// length and then the bytes.
func encodeMPI(n *big.Int) []byte {
	b := n.Bytes()
	bits := n.BitLen()
	out := make([]byte, 0, 2+len(b))
	out = append(out, lowOctet(bits>>8), lowOctet(bits))

	return append(out, b...)
}

// underReadCarrier builds the carrier the drive exists for: a real public key
// packet body, re-framed to declare ten bytes more than it holds, with the
// amplifying signature packet in the bytes nothing reads.
//
// The key body is taken from a committed fixture rather than hand-spelled,
// because what the carrier needs is a body go-crypto parses SUCCESSFULLY while
// reading less than the header declared - a property of real key material that
// hand-spelled octets would only accidentally have.
func underReadCarrier(tb testing.TB) []byte {
	tb.Helper()

	frame, status := readPacketFrame(readFixture(tb, binaryFixture))
	if status != frameBounded || frame.tag != packetTagPublicKey {
		tb.Fatalf("%s does not open with a bounded public key packet: tag %d, status %d", binaryFixture, frame.tag, status)
	}
	body := readFixture(tb, binaryFixture)[frame.headerLen:][:frame.bodyLen]

	payload := make([]byte, 0, len(body)+len(synthesizedBlobs[amplifyingBlob]))
	payload = append(payload, body...)
	payload = append(payload, synthesizedBlobs[amplifyingBlob]...)

	return newFormatPacket(packetTagPublicKey, payload)
}

// nestedEmbeddedCarrier builds a signature packet whose body carries levels of
// embedded signature, each one the single hashed subpacket of the level above.
//
// The innermost body is a minimal v4 signature: a version, a signature type, an
// algorithm pair and two empty subpacket areas. Every wrapping adds about ten
// bytes, which is what makes an uncapped recursion a function of the input.
func nestedEmbeddedCarrier(levels int) []byte {
	body := []byte{0x04, 0x13, 0x01, 0x08, 0x00, 0x00, 0x00, 0x00}

	for range levels {
		// A subpacket: a one-octet length covering the type octet and the body,
		// then the embedded signature type, then that body. The lengths stay
		// inside one octet for every depth this builds.
		sub := make([]byte, 0, 2+len(body))
		sub = append(sub, lowOctet(1+len(body)), 0x20)
		sub = append(sub, body...)

		// A v4 signature whose hashed area is that subpacket and whose unhashed
		// area is empty.
		wrapped := make([]byte, 0, 6+len(sub)+2)
		wrapped = append(wrapped, 0x04, 0x13, 0x01, 0x08, lowOctet(len(sub)>>8), lowOctet(len(sub)))
		wrapped = append(wrapped, sub...)
		wrapped = append(wrapped, 0x00, 0x00)
		body = wrapped
	}

	return newFormatPacket(packetTagSignature, body)
}

// signaturePacketBodies returns the body of every signature packet in a walked
// packet stream, in stream order, stopping where the walk can no longer read a
// bounded frame.
func signaturePacketBodies(data []byte) [][]byte {
	var bodies [][]byte

	for len(data) > 0 {
		frame, status := readPacketFrame(data)
		if status != frameBounded {
			return bodies
		}
		body := data[frame.headerLen:]
		if frame.bodyLen > int64(len(body)) {
			return bodies
		}
		if frame.tag == packetTagSignature {
			bodies = append(bodies, body[:frame.bodyLen])
		}
		data = body[frame.bodyLen:]
	}

	return bodies
}

// newFormatPacket frames body as a new-format packet of tag, always in the
// five-octet length form so that the header is a fixed size whatever the body's
// length is.
//
// The header octets are assembled here rather than taken from the production
// constants they exercise, so that mutating one of those cannot reshape a
// fixture into agreeing with it.
func newFormatPacket(tag int, body []byte) []byte {
	const (
		newFormatLead      = 0xc0
		fiveOctetMarker    = 0xff
		fiveOctetHeaderLen = 6
	)

	n := len(body)
	out := make([]byte, 0, fiveOctetHeaderLen+n)
	out = append(out, lowOctet(newFormatLead|tag), fiveOctetMarker,
		lowOctet(n>>24), lowOctet(n>>16), lowOctet(n>>8), lowOctet(n))

	return append(out, body...)
}

// lowOctet is the low eight bits of n, which is how the hand-built fixtures here
// spell a length every one of them keeps below 256 by construction. The mask is
// what says so to a reader and to the integer-conversion analyzer alike.
func lowOctet(n int) byte {
	return byte(n & 0xff)
}

// repeat tiles unit until the result is at least n bytes long, which is how the
// adversarial bodies above are packed with the smallest record a parser will
// build one struct for.
func repeat(unit []byte, n int) []byte {
	out := make([]byte, 0, n+len(unit))
	for len(out) < n {
		out = append(out, unit...)
	}

	return out
}

// padTo returns prefix followed by enough zero bytes to reach n, which is how a
// body that has to parse is given a length worth measuring.
func padTo(prefix []byte, n int) []byte {
	out := make([]byte, n)
	copy(out, prefix)

	return out
}

// packedSignatureBody builds a v6 signature body of about n bytes whose hashed
// area is packed with the smallest subpackets this walk accepts: a one-octet
// length naming a type octet and one octet of body.
//
// One octet smaller is the shape walkSignatureSubpackets refuses outright - a
// type and no body - so packing with that would measure the refusal rather than
// the drive.
//
// The version is 6 for the length field rather than for the version: a v4 body
// spells its hashed length in two octets, so every size above 65535 would carry
// a declared length that had wrapped, and the rows past that point would measure
// a body with a short subpacket area and a long trailer instead of the packing
// they name.
func packedSignatureBody(n int) []byte {
	const (
		prefixLen   = 8
		unhashedLen = 4
	)

	hashed := repeat([]byte{0x02, 0x0b, 0x09}, n-prefixLen-unhashedLen)

	body := make([]byte, 0, prefixLen+len(hashed)+unhashedLen)
	body = append(body, 0x06, 0x13, 0x01, 0x08,
		lowOctet(len(hashed)>>24), lowOctet(len(hashed)>>16), lowOctet(len(hashed)>>8), lowOctet(len(hashed)))
	body = append(body, hashed...)
	body = append(body, 0x00, 0x00, 0x00, 0x00)

	return body
}

const (
	// fuzzMaxInput is the largest input the oracle below judges. The ceiling is
	// linear in the input's length, so an unbounded input would make it one too,
	// and nothing about the property needs a large case: every hole this gate
	// closes was reached with fewer than a hundred bytes.
	fuzzMaxInput = 1 << 16

	// fuzzAllocPerByte is what one byte of input may cost either entry point.
	//
	// It is the next power of two above the worst per-byte ratio measured at
	// either profile's ceiling, which is 966.5: a keyring of 64 ten-byte public
	// key packets, each declaring a 0xffff-bit MPI, through readEntities. The
	// ratio is that high because the packet ceiling is what a small input buys
	// most cheaply - the densest packet count an input can spell is a two-byte
	// packet with an empty body, so 4096 of them is 8 KiB of input against
	// roughly 7 MB of drive - and the ten-byte key packet is the same trade with
	// an MPI allocation attached to each.
	//
	// It is deliberately a single constant rather than a constant plus a packet
	// term. Adding one would put this gate's own design into the oracle, and the
	// whole reason this target found what it found is that it names none of it.
	fuzzAllocPerByte = 1024
	// fuzzAllocFloor is what an input of no length may cost. Both entry points
	// load a keyring's worth of structures before they look at anything, and the
	// smallest fixtures measure tens of kilobytes, so the floor is what keeps the
	// ratio from having to absorb a fixed cost.
	fuzzAllocFloor = 1 << 20
)

// fuzzCeiling is what either entry point may allocate for data's own length.
func fuzzCeiling(data []byte) uint64 {
	return fuzzAllocFloor + uint64(len(data))*fuzzAllocPerByte
}

// allocDelta reports how many bytes the Go heap handed out while f ran, without
// the collection measureAlloc performs first.
//
// TotalAlloc is cumulative and exact whether or not a collection has just run,
// so the GC buys the measurement nothing here; what it costs is the fuzzer,
// which executes this millions of times and cannot afford a collection apiece.
func allocDelta(f func()) uint64 {
	var before, after runtime.MemStats

	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)

	return after.TotalAlloc - before.TotalAlloc
}

// FuzzSignatureParsingIsBoundedByItsInput states the property the whole gate
// exists for, over inputs nobody wrote down: what this package allocates for a
// blob or a keyring stays proportional to that blob or keyring.
//
// The oracle deliberately names no internal of the gate - not the walk, not the
// profiles, not a refusal - so it states the invariant rather than today's
// implementation and survives a refactor that moves where the invariant is
// enforced. It also says nothing about the verdict: an input may be refused or
// accepted, and only what it cost is judged.
//
// A panic fails this target by construction, since the fuzzer reports one as a
// failing input rather than swallowing it, and that is not incidental: two of
// the three defects this target has found were a panic and a bypass, both from
// inputs under twenty bytes, and neither is a shape the oracle above mentions.
//
// The seed corpus is every committed fixture, every armor block's decoded body,
// every carrier the table above builds, and three reproducers kept as seeds so a
// regression is caught by `go test` rather than only by a fuzzing run: the
// twelve-byte packet that panicked go-crypto's subpacket parser, the amplifying
// blob behind one octet that is not a packet header, and a short stream of empty
// signature packets, which is the shape whose cost the packet ceiling bounds.
//
// If a legitimate input ever exceeds the ceiling, the constant is what moves and
// the new measurement is what gets recorded - never a special case for the input,
// which would be the property quietly narrowing to whatever still passed.
func FuzzSignatureParsingIsBoundedByItsInput(f *testing.F) {
	kr, err := LoadKeyring(fixturePath(keyringFixture))
	if err != nil {
		f.Fatalf("LoadKeyring(%s) = %v, want nil", keyringFixture, err)
	}
	manifest := readFixture(f, manifestAFixture)

	for _, seed := range fuzzSeeds(f) {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > fuzzMaxInput {
			t.Skip("larger than the input this ceiling is stated for")
		}
		ceiling := fuzzCeiling(data)

		if allocated := allocDelta(func() { _, _ = readEntities(data) }); allocated > ceiling {
			t.Fatalf("readEntities(%d bytes) allocated %d bytes, want at most %d", len(data), allocated, ceiling)
		}
		if allocated := allocDelta(func() { _, _ = checkOne(manifest, data, kr) }); allocated > ceiling {
			t.Fatalf("checkOne(%d bytes) allocated %d bytes, want at most %d", len(data), allocated, ceiling)
		}
	})
}

// fuzzSeeds collects every input this package already knows reaches a parser:
// each committed fixture whole, each armor block's decoded body, each carrier,
// and the three reproducers.
//
// The decoded bodies are seeds of their own because the gate runs on decoded
// bytes, so a seed that stays armored only ever exercises the decode in front of
// it.
//
// The reproducers are seeds rather than tests of their own only here; each also
// has an assertion elsewhere in this file. What being a seed adds is that the
// fuzzer starts from them, so a mutation of one of the three shapes is where its
// exploration begins rather than somewhere it might reach.
func fuzzSeeds(f *testing.F) [][]byte {
	f.Helper()

	entries, err := os.ReadDir(testdataDir)
	if err != nil {
		f.Fatalf("read %s: %v", testdataDir, err)
	}

	seeds := make([][]byte, 0, len(entries)*2)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		data := readFixture(f, entry.Name())
		seeds = append(seeds, data)
		if decoded, _, decodeErr := decodeArmorBlock(data); decodeErr == nil {
			seeds = append(seeds, decoded)
		}
	}

	for _, carrier := range framingCarriers(f) {
		seeds = append(seeds, carrier.data)
	}

	// The subpacket panic; the amplifying blob behind one octet that is not a
	// packet header, which walked clean before the unreadable arm existed; and a
	// stream of empty signature packets, kept short so the corpus stays cheap to
	// replay.
	seeds = append(seeds, emptySubpacketPacket)
	seeds = append(seeds, append([]byte{0x00}, synthesizedBlobs[amplifyingBlob]...))
	seeds = append(seeds, repeat([]byte{0xc2, 0x00}, 2*(signatureBlobMaxPackets+1)))

	return seeds
}

const (
	// emptyBlocksAtCeiling is how many empty armor blocks a file can carry ahead
	// of the committed binary key export and still sit exactly at the packet
	// ceiling: one packet apiece, plus one for the export's own block, plus that
	// export's three packets. The arithmetic is spelled here rather than left to
	// the reader, since it is what the boundary rows below are a measurement of.
	emptyBlocksAtCeiling = keyringMaxPackets - binaryFixturePackets - armorBlockPacketCost

	// emptyBlockAllocCeiling is what refusing a file of empty armor blocks may
	// cost. Measured at 17113584 bytes plainly and 20519824 under the race
	// detector, for 20000 empty blocks and one real one, against the 70500184
	// the identical file spent when a block cost nothing - where it also loaded,
	// with one key and a nil error. So the ceiling sits between the two, with
	// most of a factor of two in hand above the refusal and most of one below
	// the defect.
	emptyBlockAllocCeiling = 32 << 20

	// emptyBlockAbuseCount is how many empty blocks that measurement carries. It
	// is far past the ceiling on purpose: what it measures is that the refusal
	// arrives at the ceiling rather than at the end of the file.
	emptyBlockAbuseCount = 20000
)

// armorBlockBudgetCase is one file of empty armor blocks, optionally ending on
// a real key export, and the verdict the packet budget owes it.
type armorBlockBudgetCase struct {
	name         string
	wantMessage  string
	empties      int
	wantEntities int
	withKey      bool
	wantRefuse   bool
}

// armorBlockBudgetCases state the block charge as a boundary rather than as a
// direction, at both of the arms that can report it.
//
// The first pair is the charge itself: a file whose empty blocks and one real
// export come to exactly the ceiling loads its key, and one empty block more is
// refused. Nothing but the charge separates them - the empty blocks carry no
// packets at all, so without it both rows cost the export's three and both load.
//
// The second pair is the arm walkPacketFraming cannot reach, since a block
// decoding to no packets never enters its loop: a file of nothing but empty
// blocks, at the ceiling and one past it. Its accepting row holds no keys, which
// loadKeyring refuses a step later for a different reason, so both rows are
// stated over readEntities where the budget's own verdict is the whole answer.
func armorBlockBudgetCases() []armorBlockBudgetCase {
	return []armorBlockBudgetCase{
		{name: "a key export at the ceiling", empties: emptyBlocksAtCeiling, withKey: true, wantEntities: 1},
		{name: "one empty block more", empties: emptyBlocksAtCeiling + 1, withKey: true, wantRefuse: true,
			wantMessage: "malformed OpenPGP packet framing: more than 4096 packets across the whole input"},
		{name: "empty blocks alone at the ceiling", empties: keyringMaxPackets},
		{name: "one empty block past it", empties: keyringMaxPackets + 1, wantRefuse: true,
			wantMessage: "malformed OpenPGP packet framing: more than 4096 packets across the whole input, " +
				"counting each armor block as one"},
	}
}

// TestArmorBlocksAreChargedAgainstThePacketBudget pins what an armor block
// costs its file's packet budget.
//
// A block that decodes to no packets used to cost nothing, so a file's block
// count was bounded by its size alone while its packet count was bounded by the
// ceiling - and a block is not free: it is an armor decode, its buffers and an
// openpgp.ReadKeyRing call. Charging it bounds both counts with one number, and
// these rows are where that arithmetic is a measurement rather than a claim.
//
// Each refusing row is one empty block away from an accepting one, so a gate
// that refused every long file would fail the row beside it; and each of them
// names its whole message, hand-spelled and apart from the production format
// strings, so a row refused by some other arm fails here rather than passing.
//
// Parallel: it counts verdicts and entities rather than bytes allocated, so
// nothing running alongside it is counted in its answer.
func TestArmorBlocksAreChargedAgainstThePacketBudget(t *testing.T) {
	t.Parallel()

	empty := armorEncode(t, openpgp.PublicKeyType, nil)
	export := armorEncode(t, openpgp.PublicKeyType, readFixture(t, binaryFixture))

	for _, tc := range armorBlockBudgetCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			blocks := tc.empties
			file := make([]byte, 0, blocks*(len(empty)+1)+len(export)+1)
			for range blocks {
				file = append(file, empty...)
				file = append(file, '\n')
			}
			if tc.withKey {
				file = append(file, export...)
				file = append(file, '\n')
				blocks++
			}
			// The fixture ahead of its verdict: every block has to present its own
			// opening line, or the file is measuring the cut rather than the charge.
			if got := countArmorBlocks(file); got != blocks {
				t.Fatalf("the fixture presents %d armor blocks, want %d", got, blocks)
			}

			entities, err := readEntities(file)
			// Killing mutation, actually run against this file: set
			// armorBlockPacketCost to 0, which is what a block cost before it was
			// charged. Both refusing rows then fail, one of them with
			//
			//	framing_test.go:3812: readEntities(one empty block more) = 1 entities, <nil>, want refused = true
			//
			// while both accepting rows pass throughout, which is what makes each
			// pair a boundary rather than a direction.
			if got := errors.Is(err, errMalformedSignaturePacket); got != tc.wantRefuse {
				t.Fatalf("readEntities(%s) = %d entities, %v, want refused = %t", tc.name, len(entities), err, tc.wantRefuse)
			}
			if tc.wantRefuse {
				if err.Error() != tc.wantMessage {
					t.Fatalf("readEntities(%s) = %v, want %q", tc.name, err, tc.wantMessage)
				}

				return
			}
			if len(entities) != tc.wantEntities {
				t.Fatalf("readEntities(%s) = %d entities, want %d", tc.name, len(entities), tc.wantEntities)
			}
		})
	}
}

// TestEmptyArmorBlocksAreRefusedCheaply is the block charge measured rather
// than asserted: the refusal has to arrive at the ceiling, or a file can still
// spend a block's cost as many times as its size allows.
//
// A block costs about three and a half kilobytes of decode and reader whatever
// it holds, and an empty one is 80 bytes of file, so an uncharged budget let
// keyringMaxSize buy that cost hundreds of thousands of times. The assertion is
// a byte ceiling rather than a verdict, because charging the block AFTER
// decoding it would produce the identical verdict and none of the saving.
//
// The positive control is that same export on its own, written into the same
// directory, so the refusal below is the empty blocks rather than this
// directory, this test's writes, or the export it ends on.
//
// Not parallel: runtime.MemStats is process-wide, so a parallel test allocating
// alongside it would be counted here.
func TestEmptyArmorBlocksAreRefusedCheaply(t *testing.T) {
	empty := armorEncode(t, openpgp.PublicKeyType, nil)
	export := armorEncode(t, openpgp.PublicKeyType, readFixture(t, binaryFixture))

	file := make([]byte, 0, emptyBlockAbuseCount*(len(empty)+1)+len(export)+1)
	for range emptyBlockAbuseCount {
		file = append(file, empty...)
		file = append(file, '\n')
	}
	file = append(file, export...)
	file = append(file, '\n')

	dir := t.TempDir()
	controlPath := filepath.Join(dir, "control.asc")
	// #nosec G703 -- dir is this test's own t.TempDir and the leaf is a
	// constant; no part of either path comes from outside this function.
	if err := os.WriteFile(controlPath, export, keyringFileMode); err != nil {
		t.Fatalf("write control keyring: %v", err)
	}
	if _, err := LoadKeyring(controlPath); err != nil {
		t.Fatalf("positive control: LoadKeyring(the export alone) = %v, want nil", err)
	}

	path := filepath.Join(dir, "empties.asc")
	// #nosec G703 -- same t.TempDir and a constant leaf, as just above.
	if err := os.WriteFile(path, file, keyringFileMode); err != nil {
		t.Fatalf("write the empty-block keyring: %v", err)
	}

	var loadErr error
	allocated := measureAlloc(func() {
		_, loadErr = LoadKeyring(path)
	})
	// Killing mutation, actually run against this file: set armorBlockPacketCost
	// to 0. This assertion then fails with
	//
	//	framing_test.go:3884: LoadKeyring(20000 empty blocks) allocated 70505328 bytes, want at most 33554432
	//
	// and the file loads its key too, out of 1600449 bytes of keyring.
	if allocated > emptyBlockAllocCeiling {
		t.Fatalf("LoadKeyring(%d empty blocks) allocated %d bytes, want at most %d",
			emptyBlockAbuseCount, allocated, emptyBlockAllocCeiling)
	}
	// Which refusal produced that cheap outcome, asked below the measurement for
	// the reason every measurement here gives.
	if !errors.Is(loadErr, errMalformedSignaturePacket) {
		t.Fatalf("LoadKeyring(%d empty blocks) = %v, want the malformed-packet refusal", emptyBlockAbuseCount, loadErr)
	}
	t.Logf("%d empty blocks in %d bytes were refused in %d bytes of allocation", emptyBlockAbuseCount, len(file), allocated)
}
