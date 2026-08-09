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
	// framingAllocCeiling is what a refusal of the amplifying blob may cost.
	// The ungated library call allocates 4294969120 bytes (4.00 GiB) for the
	// same ten bytes, measured through openpgp.CheckDetachedSignature; the
	// gated path allocates the error value and the buffers around the call and
	// nothing else. A ceiling four thousand times below the ungated figure is
	// far enough above real noise to survive the race detector and far enough
	// below the defect to be nowhere near it.
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
// The refusing rows are the two over-declared shapes. The accepting rows are
// two different things and both are load-bearing: a real signature must pass,
// or the gate would refuse every collection; and every header shape the walk
// declines to follow must pass too, since go-crypto refuses each of those
// itself and a second opinion here could only disagree with the one that
// decides the verdict.
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
		// of the input, so nothing is declared to compare.
		name: "an old-format packet with an indeterminate length",
		data: []byte{0x8b, 0x06, 0x13, 0x01, 0x08, 0xff, 0xff, 0xff, 0xff},
	},
	{
		// A new-format partial length introduces a chunked body, so its octet
		// sizes one chunk rather than the packet.
		name: "a new-format packet with a partial length",
		data: []byte{0xc2, 0xe1, 0x06, 0x13, 0x01, 0x08, 0xff, 0xff, 0xff, 0xff},
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
	//	framing_test.go:201: checkOne(v6 amplification blob) allocated 4294974552 bytes, want at most 1048576
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

	var armored bytes.Buffer
	writer, err := armor.Encode(&armored, openpgp.PublicKeyType, nil)
	if err != nil {
		t.Fatalf("armor.Encode = %v, want nil", err)
	}
	if _, err = writer.Write(blob); err != nil {
		t.Fatalf("write armored body: %v", err)
	}
	if err = writer.Close(); err != nil {
		t.Fatalf("close armored body: %v", err)
	}

	// The control is a real keyring written into the same directory, so a
	// refusal below is the blob and not this directory or this test's writes.
	controlPath := filepath.Join(dir, "control.asc")
	// #nosec G703 -- dir is this test's own t.TempDir and the leaf is a
	// constant; no part of the path comes from outside this function.
	if err = os.WriteFile(controlPath, readFixture(t, armoredFixture), keyringFileMode); err != nil {
		t.Fatalf("write control keyring: %v", err)
	}
	if _, err = LoadKeyring(controlPath); err != nil {
		t.Fatalf("positive control: LoadKeyring(control.asc) = %v, want nil", err)
	}

	for _, tc := range []struct {
		name string
		data []byte
	}{
		{name: "binary.gpg", data: blob},
		{name: "armored.asc", data: armored.Bytes()},
	} {
		path := filepath.Join(dir, tc.name)
		// #nosec G703 -- same t.TempDir and a leaf from this function's own
		// fixed table, as just above.
		if err = os.WriteFile(path, tc.data, keyringFileMode); err != nil {
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
		// decode and the read with no gate between them. Only the armored row
		// then fails, with
		//
		//	framing_test.go:287: LoadKeyring(armored.asc) allocated 4294980864 bytes, want at most 1048576
		//
		// while its sentinel assertion above still passes: the file is refused
		// either way, and what the gate removes is the 4 GiB spent reaching
		// that refusal.
		if allocated > framingAllocCeiling {
			t.Fatalf("LoadKeyring(%s) allocated %d bytes, want at most %d", tc.name, allocated, framingAllocCeiling)
		}
	}
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
