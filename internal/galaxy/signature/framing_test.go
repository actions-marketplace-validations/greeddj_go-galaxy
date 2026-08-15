package signature

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
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

// framingCase is one row of TestCheckPacketFraming: the bytes to walk and
// whether the walk must refuse them.
type framingCase struct {
	name       string
	data       []byte
	wantRefuse bool
}

// framingCases is the gate's whole predicate, one row per answer it can give.
//
// The refusing rows are two kinds rather than one: a length declared over the
// bytes actually present, and a packet framed so that this walk can find no end
// to its body at all - which is refused on any tag, so both spellings of it
// appear twice below, once on the signature tag and once off it. The gate reads
// the framing and the tag is not part of its predicate; a table stating that
// only on tag 2 would state one point of the predicate rather than its domain,
// and would keep passing if the tag came back into the rule. The accepting rows
// are two more, both
// load-bearing: a real signature must pass, or the gate would refuse every
// collection; and a header shape this walk cannot read at all must pass too,
// since go-crypto refuses each of those itself and cheaply, and a second
// opinion here could only disagree with the one that decides the verdict.
//
//nolint:gochecknoglobals // a fixed table consumed by one test, not mutable shared state
var framingCases = []framingCase{
	{
		name:       "a v6 signature declaring 4 GiB of hashed subpackets",
		data:       synthesizedBlobs[amplifyingBlob],
		wantRefuse: true,
	},
	{
		// The hashed length is honest and the unhashed one is not, so the walk
		// has to reach past the hashed subpackets to see it.
		name:       "a v6 signature declaring 4 GiB of unhashed subpackets",
		data:       []byte{0xc2, 0x0d, 0x06, 0x13, 0x01, 0x08, 0x00, 0x00, 0x00, 0x01, 0x2a, 0xff, 0xff, 0xff, 0xff},
		wantRefuse: true,
	},
	{
		// A packet body longer than the bytes behind it, whatever the packet
		// turns out to be.
		name:       "a packet declaring a body longer than the input",
		data:       []byte{0xc2, 0xff, 0xff, 0xff, 0xff, 0xff, 0x06},
		wantRefuse: true,
	},
	{
		// The same four-octet subpacket length, held by a v4 signature, where
		// it is not a length at all: v4 spells that field in two octets, so
		// these bytes mean something else entirely and go-crypto can allocate
		// at most 65535 for it.
		name: "a v4 signature carrying the same octets",
		data: []byte{0xc2, 0x08, 0x04, 0x13, 0x01, 0x08, 0xff, 0xff, 0xff, 0xff},
	},
	{
		// Nothing about this is a packet header, which go-crypto reports as a
		// tag byte without its MSB.
		name: "bytes that are not a packet header",
		data: []byte("-----BEGIN PGP PUBLIC KEY BLOCK-----\n"),
	},
	{
		// An old-format header whose length type is 3: the body runs to the end
		// of the input and no octet names its length, so this walk can bound
		// neither that body nor anything behind it. A body it cannot bound is
		// refused rather than followed, because go-crypto accepts the framing
		// and keeps parsing where this walk had to stop.
		name:       "an old-format packet with an indeterminate length",
		data:       []byte{0x8b, 0x06, 0x13, 0x01, 0x08, 0xff, 0xff, 0xff, 0xff},
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
		wantRefuse: true,
	},
	{
		// The same partial length on tag 1, carrying no signature body at all.
		// The walk still cannot find this packet's end, so the refusal is the
		// framing's and the two rows above are two points of one predicate
		// rather than a rule about signatures.
		name:       "a new-format partial length off the signature tag",
		data:       []byte{0xc1, 0xe0, 0x00, 0x00},
		wantRefuse: true,
	},
	{
		// And the other unbounded spelling off the signature tag: one octet
		// naming tag 1 and length type 3, with a zero body behind it. Refused
		// for the same reason, from a header a byte long.
		name:       "an old-format indeterminate length off the signature tag",
		data:       []byte{0x87, 0x00},
		wantRefuse: true,
	},
	{
		// A header cut off before its own length field: there is no declared
		// length to read, let alone to compare.
		name: "a header truncated inside its length field",
		data: []byte{0xc2, 0xff, 0x00},
	},
	{name: "no bytes at all", data: nil},
}

// TestCheckPacketFraming walks the gate's predicate row by row.
//
// The refusals are what the gate exists for; the acceptances are what keeps it
// from being a second parser with an opinion of its own. A nil answer means
// only "nothing here declares an allocation the bytes cannot back", which is
// why rows that are plainly not signatures still sit under it.
func TestCheckPacketFraming(t *testing.T) {
	t.Parallel()

	// The positive control for the whole table, on real key material rather
	// than on a hand-built shape: a committed keyring walks clean, so a
	// refusing row is its own bytes and not a gate that refuses everything.
	if err := checkPacketFraming(readFixture(t, binaryFixture)); err != nil {
		t.Fatalf("positive control: checkPacketFraming(%s) = %v, want nil", binaryFixture, err)
	}

	for _, tc := range framingCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := checkPacketFraming(tc.data)
			if got := errors.Is(err, errMalformedSignaturePacket); got != tc.wantRefuse {
				t.Fatalf("checkPacketFraming(%s) refused = %t (err %v), want refused = %t", tc.name, got, err, tc.wantRefuse)
			}
		})
	}
}

// TestCheckPacketFramingAcceptsEverySignatureFixture is the acceptance half of
// the gate stated as a property rather than as a row: every signature this
// repository committed, and both keyrings, walk clean.
//
// A gate that refused any of them would refuse ordinary collections, and it
// would do so on the one input class nobody hand-builds a case for.
func TestCheckPacketFramingAcceptsEverySignatureFixture(t *testing.T) {
	t.Parallel()

	// The armored fixtures are decoded first, since the gate runs on decoded
	// bytes and armor text carries no framing to walk.
	for _, name := range []string{
		sigValidArmored, sigValidBinary, sigSecondSigner, sigOverManifestB,
		sigOutsiderKey, sigExpiredKey, sigRevokedKey, sigExpiredSig,
		keyringFixture, outsiderKeyringFixture, armoredFixture, binaryFixture,
	} {
		data := readFixture(t, name)
		if bytes.Contains(data, []byte(armorBlockStart)) {
			decoded, _, err := decodeArmorBlock(data)
			if err != nil {
				t.Fatalf("decodeArmorBlock(%s) = %v, want nil", name, err)
			}
			data = decoded
		}
		if err := checkPacketFraming(data); err != nil {
			t.Fatalf("checkPacketFraming(%s) = %v, want nil", name, err)
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
	// checkSignatureSubpacketFraming return nil unconditionally, which removes
	// the v6 gate and nothing else - the packet body length check above it is
	// untouched, and this blob's body length is honest, so nothing else in the
	// walk has anything to say about it. This assertion then fails with
	//
	//	framing_test.go:232: checkOne(v6 amplification blob) allocated 4294969120 bytes, want at most 1048576
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
		//	framing_test.go:307: LoadKeyring(armored.asc) allocated 4294980752 bytes, want at most 1048576
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
		// Killing mutation, actually run against this file: delete the
		// frameUnbounded arm from checkPacketFraming, so an unbounded body ends
		// the walk with a nil error whatever tag it sits on. The declared-length
		// row still passes, since its header names a body length; the run then
		// stops here on the first row the mutation unguards, with
		//
		//	framing_test.go:386: LoadKeyring(an old-format indeterminate length) allocated 4294978256 bytes, want at most 1048576
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
	// "&& frame.tag == packetTagSignature" that checkPacketFraming's
	// frameUnbounded arm used to carry, so an unbounded framing is refused on
	// the signature tag alone. This assertion then fails with the list of every
	// carrier that got through, wrapped here and cut after its first element:
	//
	//	framing_test.go:505: 58 carriers allocated more than 1048576 bytes:
	//	[{framing:new-format partial entry:LoadKeyring allocated:4294979576 tag:1} ...]
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

// requireFramingFixturesStillWork is the positive control for both measurements
// above, run before each of them and on committed fixtures rather than on
// hand-built shapes: the binary key export and the multi-key armored keyring
// both load - the third, keyring.asc, is what the caller loaded to get kr - and
// both encodings of a real signature still verify.
//
// Refusing a body the framing walk cannot bound is aimed at shapes gpg writes
// into neither a keyring nor a detached signature. A fixture failing here would
// be that claim coming back wrong, so it stops the test rather than being
// worked around.
func requireFramingFixturesStillWork(t *testing.T, kr *Keyring, manifest []byte) {
	t.Helper()

	for _, name := range []string{binaryFixture, twoKeyFixture} {
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
	//	framing_test.go:728: checkOne(oversized armor header) allocated 5541088032 bytes, want at most 16775168
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
	//	framing_test.go:789: LoadKeyring(oversized armor header) allocated 5543303304 bytes, want at most 16775168
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
		//	framing_test.go:838: checkArmorHeaderSection(keyring-outsider.asc) =
		//	oversized OpenPGP armor header section: no blank line ends one within 0 bytes, want nil
		//
		// with 15 more behind it, and the sweep ending on the fatal below for having
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
			//	framing_test.go:1154: checkArmorHeaderSection(a 4096-byte section) refused = true
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
			//	framing_test.go:1154: checkArmorHeaderSection(a 4097-byte section) refused = false (err <nil>), want refused = true
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
			//	framing_test.go:1154: checkArmorHeaderSection(a 16-byte section) refused = true
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
