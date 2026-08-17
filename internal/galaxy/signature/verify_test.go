package signature

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	pgperrors "github.com/ProtonMail/go-crypto/openpgp/errors"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// The fixtures below are generated once and committed, never built at test
// time, for the reason keyring_test.go's own fixture note gives: generating a
// key costs seconds and needs a working gpg-agent. All of them come from
// gpg 2.5.21 run against one throwaway GNUPGHOME, and no private half of any
// key in it was exported.
//
// The two manifests are hand-written MANIFEST.json-shaped documents differing
// only in the version they declare, so a signature over one is a well-formed
// signature that does not verify against the other.
//
// Every gpg call below, the sixth key's own three included, additionally
// carried the flags that suppress prompting on an empty passphrase and nothing
// else: --batch, --yes, --pinentry-mode loopback, and an empty --passphrase.
//
// Five keys, all ed25519 and signing-only:
//
//	gpg --quick-generate-key 'go-galaxy signature test key <signer@example.invalid>' ed25519 sign never
//	gpg --quick-generate-key 'go-galaxy second signature test key <signer2@example.invalid>' ed25519 sign never
//	gpg --quick-generate-key 'go-galaxy untrusted test key <outsider@example.invalid>' ed25519 sign never
//	gpg --quick-generate-key 'go-galaxy revoked test key <revoked@example.invalid>' ed25519 sign never
//	gpg --quick-generate-key 'go-galaxy expired test key <expired@example.invalid>' ed25519 sign seconds=10
//
// The second key exists because "two valid signatures" needs two keys to mean
// anything: Result.Verified counts distinct signers, so the same key's armored
// and binary signatures over one manifest are two blobs and one signer.
//
// The signatures, each made with the key -u names, over the manifest given
// last:
//
//	gpg -u signer@example.invalid --detach-sign --armor -o sig-a-valid.asc manifest-a.json
//	gpg -u signer@example.invalid --detach-sign -o sig-a-valid.sig manifest-a.json
//	gpg -u signer@example.invalid --detach-sign --armor -o sig-b-valid.asc manifest-b.json
//	gpg -u signer2@example.invalid --detach-sign --armor -o sig-a-signer2.asc manifest-a.json
//	gpg -u outsider@example.invalid --detach-sign --armor -o sig-a-outsider.asc manifest-a.json
//	gpg -u revoked@example.invalid --detach-sign --armor -o sig-a-revoked.asc manifest-a.json
//	gpg -u expired@example.invalid --detach-sign --armor -o sig-a-expired.asc manifest-a.json
//	gpg --default-sig-expire seconds=5 --ask-sig-expire -u signer@example.invalid \
//	    --detach-sign --armor -o sig-a-expsig.asc manifest-a.json
//
// The expired key carries a ten-second lifetime and was signed with, and
// exported, inside that window; sig-a-expsig.asc carries a five-second
// signature lifetime made by the key that never expires. Both have been expired
// ever since, which is what makes EXPKEYSIG and EXPSIG properties of the
// committed bytes rather than of the clock on the machine running the tests.
//
// The revocation is the certificate gpg writes at key creation, imported back
// over the key it revokes before the export:
//
//	sed 's/^://' "$GNUPGHOME/openpgp-revocs.d/<fpr>.rev" > revoke.asc
//	gpg --import revoke.asc
//
// The exports, and the two keyrings built from them:
//
//	gpg --export --armor signer@example.invalid > signer.asc
//	gpg --export --armor expired@example.invalid > expired.asc   # inside the window
//	gpg --export --armor revoked@example.invalid > revoked.asc   # after the revocation
//	gpg --export --armor signer2@example.invalid > signer2.asc
//	gpg --export --armor outsider@example.invalid > keyring-outsider.asc
//	cat signer.asc expired.asc revoked.asc signer2.asc > keyring.asc
//
// keyring.asc is what every case here verifies against, so the outsider key is
// the one key it does not hold - which is the whole of NO_PUBKEY.
//
// The two damaged blobs are built from sig-a-valid.asc rather than signed, so
// each differs from a blob that verifies in exactly the one way it is named
// for:
//
//	# sig-a-badarmor.asc: the body's line breaks removed, which is what a copy
//	# through anything that folds long lines produces.
//	{ head -2 sig-a-valid.asc; sed -n '3,6p' sig-a-valid.asc | tr -d '\n'; echo; \
//	  sed -n '7,8p' sig-a-valid.asc; } > sig-a-badarmor.asc
//	# sig-a-badbase64.asc: one body character replaced by one that is not base64.
//	sed '3s/iKkE/iK%E/' sig-a-valid.asc > sig-a-badbase64.asc
//
// A sixth key, in a GNUPGHOME of its own, exists for one property none of the
// five above has: a SIGNING SUBKEY, whose binding signature carries the
// primary-key-binding signature that cross-certifies it. That cross-signature
// is an embedded signature subpacket - a whole signature packet body nested
// inside another one - which is the only shape in this repository that reaches
// checkSignatureBodyFraming's recursion on real material rather than on bytes a
// test spelled. Without it, "the recursion refused a hand-built carrier" would
// be indistinguishable from "the recursion refuses everything it descends into".
//
//	gpg --quick-generate-key 'go-galaxy signing subkey test key <subkey@example.invalid>' ed25519 sign never
//	gpg --quick-add-key <fpr> ed25519 sign never
//	gpg --export --armor 'subkey@example.invalid' > signing-subkey.asc
//
// It is public material like every other export here, it signs nothing, and no
// verification case uses it: the framing tests are its only consumers.
//
// Four blobs are spelled out in this file rather than committed, because no gpg
// command writes any of them and an empty file in testdata says less than the
// bytes do: a blob carrying nothing at all, the two empty armor envelopes, and
// the packet-framing amplification blob. See synthesizedBlobs.
const (
	manifestAFixture = "manifest-a.json"
	manifestBFixture = "manifest-b.json"

	keyringFixture         = "keyring.asc"
	outsiderKeyringFixture = "keyring-outsider.asc"
	// subkeyFixture is the export carrying a signing subkey, and so the only
	// committed fixture holding an embedded signature subpacket.
	subkeyFixture = "signing-subkey.asc"

	sigValidArmored  = "sig-a-valid.asc"
	sigValidBinary   = "sig-a-valid.sig"
	sigSecondSigner  = "sig-a-signer2.asc"
	sigOverManifestB = "sig-b-valid.asc"
	sigOutsiderKey   = "sig-a-outsider.asc"
	sigExpiredKey    = "sig-a-expired.asc"
	sigRevokedKey    = "sig-a-revoked.asc"
	sigExpiredSig    = "sig-a-expsig.asc"
	sigFoldedArmor   = "sig-a-badarmor.asc"
	sigBadBase64     = "sig-a-badbase64.asc"

	// The four synthesized blobs, named unlike file names because
	// signatureBlobs resolves them out of synthesizedBlobs rather than reading
	// anything.
	noDataBlob        = "<no data>"
	emptyArmorBlob    = "<empty armor envelope>"
	emptyArmorCRCBlob = "<empty armor envelope with a checksum line>"
	amplifyingBlob    = "<v6 signature declaring 4 GiB of subpackets>"

	// deterministicRuns is how many times TestVerifyIsDeterministicAcrossRuns
	// repeats one call. Any number above one exercises the property; the
	// repetition is what makes an agreement a result rather than a
	// coincidence, since an implementation that iterated a map could still
	// agree with itself twice.
	deterministicRuns = 100
)

// synthesizedBlobs holds the blob shapes gpg does not write, spelled out here
// so each is exactly the bytes it is named for.
//
// The two envelopes are what an attacker sends instead of an empty blob: both
// are well-formed armor around no data at all, so the cheap "is this blank"
// test cannot see them and only a decode can. The checksum line is present in
// one and absent in the other because armor accepts either, and a guard that
// keyed on the layout rather than on the decoded length would pass one and fail
// the other.
//
// amplifyingBlob is a whole new-format packet: 0xc2 is the signature tag, 0x08
// declares an eight-octet body, and that body is a v6 signature header (version
// 6, signature type 0x13, RSA, SHA-256) whose four-octet hashed subpacket
// length is 0xffffffff. Handed to go-crypto ungated it allocates 4.00 GiB - one
// header shape reaching that allocation, and framing_test.go measures the rest.
//
//nolint:gochecknoglobals // a fixed table consumed by the tests, not mutable shared state
var synthesizedBlobs = map[string][]byte{
	noDataBlob:        nil,
	emptyArmorBlob:    []byte("-----BEGIN PGP SIGNATURE-----\n\n-----END PGP SIGNATURE-----\n"),
	emptyArmorCRCBlob: []byte("-----BEGIN PGP SIGNATURE-----\n\n=twTO\n-----END PGP SIGNATURE-----\n"),
	amplifyingBlob:    {0xc2, 0x08, 0x06, 0x13, 0x01, 0x08, 0xff, 0xff, 0xff, 0xff},
}

// errUnanticipated stands in for an error shape no release of go-crypto has
// returned, which is the only input whose status classify chooses rather than
// maps.
var errUnanticipated = errors.New("an error no release of go-crypto has ever returned")

// errGatherFailed stands in for whatever a caller's own pull reports when a
// signature source cannot be fetched. Verify carries it back untouched, so its
// identity is all a test needs.
var errGatherFailed = errors.New("a signature source could not be fetched")

// readFixture reads one testdata file whole.
func readFixture(tb testing.TB, name string) []byte {
	tb.Helper()

	data, err := os.ReadFile(fixturePath(name))
	if err != nil {
		tb.Fatalf("read %s: %v", name, err)
	}

	return data
}

// loadTestKeyring loads a keyring fixture, failing the test if it does not
// load: every case below is about a verdict, so a keyring that could not be
// read has to stop the case rather than quietly shape its outcome.
func loadTestKeyring(t *testing.T, name string) *Keyring {
	t.Helper()

	kr, err := LoadKeyring(fixturePath(name))
	if err != nil {
		t.Fatalf("LoadKeyring(%s) = %v, want nil", name, err)
	}

	return kr
}

// signatureBlobs turns fixture and synthesized-blob names into blobs, naming
// each blob's origin after the name it came from so a Failure can be attributed
// to a row.
func signatureBlobs(t *testing.T, names ...string) []Blob {
	t.Helper()

	blobs := make([]Blob, 0, len(names))
	for _, name := range names {
		blobs = append(blobs, Blob{Origin: name, Data: blobBytes(t, name)})
	}

	return blobs
}

// blobBytes resolves one blob name: a synthesized shape if the table holds it,
// otherwise a committed fixture read whole.
func blobBytes(t *testing.T, name string) []byte {
	t.Helper()

	if data, ok := synthesizedBlobs[name]; ok {
		return data
	}

	return readFixture(t, name)
}

// blobSource turns a slice into the pull Verify takes, so a table can still
// spell its input as a list. It reports how many blobs were actually pulled,
// which is the only way a caller can observe the loop's early stop from
// outside.
func blobSource(blobs []Blob) (NextBlob, *int) {
	pulled := 0
	next := func() (Blob, bool, error) {
		if pulled >= len(blobs) {
			return Blob{}, false, nil
		}
		blob := blobs[pulled]
		pulled++

		return blob, true, nil
	}

	return next, &pulled
}

// verifyBlobs runs Verify over a slice, for the cases that care about the
// verdict rather than about how many blobs were pulled to reach it.
func verifyBlobs(manifest []byte, blobs []Blob, kr *Keyring, p Policy) (Result, error) {
	next, _ := blobSource(blobs)

	return Verify(manifest, next, kr, p)
}

// testPolicy builds a Policy from the spellings an operator would configure,
// through the same parser production uses.
func testPolicy(t *testing.T, required string, ignore []string) Policy {
	t.Helper()

	policy, err := NewPolicy(fixturePath(keyringFixture), required, ignore, false)
	if err != nil {
		t.Fatalf("NewPolicy(%q, %q) = %v, want nil", required, ignore, err)
	}

	return policy
}

// classifyCase is one row of TestClassifyMapsEachFailureToItsStatus: the
// fixture to check against manifest-a, and the status its failure must carry.
type classifyCase struct {
	name    string
	fixture string
	want    Status
}

// classifyCases covers every status classify can produce, one row per arm of
// that function, each driven by an input rather than a hand-built error.
//
// The two BADARMOR rows are the two shapes an armor envelope fails to decode
// in, and they arrive at that status by different routes inside the decoder,
// so a single row would leave one of the arms unexercised.
//
//nolint:gochecknoglobals // a fixed table consumed by one test, not mutable shared state
var classifyCases = []classifyCase{
	{name: "a signature over a different manifest", fixture: sigOverManifestB, want: StatusBadSig},
	{name: "a signature by a key the keyring does not hold", fixture: sigOutsiderKey, want: StatusNoPubKey},
	{name: "a signature by an expired key", fixture: sigExpiredKey, want: StatusExpKeySig},
	{name: "a signature by a revoked key", fixture: sigRevokedKey, want: StatusRevKeySig},
	{name: "a signature that has itself expired", fixture: sigExpiredSig, want: StatusExpSig},
	{name: "armor whose line breaks are gone", fixture: sigFoldedArmor, want: StatusBadArmor},
	{name: "armor whose body is not base64", fixture: sigBadBase64, want: StatusBadArmor},
	{name: "a blob carrying nothing", fixture: noDataBlob, want: StatusNoData},
	// A well-formed envelope around nothing reaches the same status as a blob
	// that is nothing, which is the point: the shape an attacker can choose must
	// not be worth choosing. Both envelope layouts are rows, since one carries
	// the optional checksum line and the other does not.
	{name: "an empty armor envelope", fixture: emptyArmorBlob, want: StatusNoData},
	{name: "an empty armor envelope with a checksum line", fixture: emptyArmorCRCBlob, want: StatusNoData},
	// A key block handed over as a signature is a well-formed OpenPGP object of
	// the wrong kind: nothing about it is a signature that failed, which is
	// exactly the shape the vocabulary's ERRSIG covers.
	{name: "a blob that is not a signature at all", fixture: keyringFixture, want: StatusErrSig},
	// The framing gate's refusal lands on the same status the ungated library
	// call already produced for these bytes - unexpected EOF, after 4.00 GiB of
	// allocation - which is what makes the gate a change of cost rather than of
	// verdict. An operator's ignore list keys on the status, so moving it would
	// silently take an existing configuration into or out of scope.
	{name: "a v6 signature declaring more subpackets than it holds", fixture: amplifyingBlob, want: StatusErrSig},
}

// TestClassifyMapsEachFailureToItsStatus pins the status vocabulary against
// real gpg output rather than against hand-built errors, so a go-crypto release
// that renames or re-shapes one of these failures fails here instead of
// silently collapsing a status into ERRSIG - which an operator's ignore list
// would then stop covering.
func TestClassifyMapsEachFailureToItsStatus(t *testing.T) {
	t.Parallel()

	kr := loadTestKeyring(t, keyringFixture)
	manifest := readFixture(t, manifestAFixture)

	// The positive control for every row below: this keyring and this manifest
	// do verify a signature, so a row that reports a failure is reporting its
	// own fixture rather than a setup that could never have succeeded.
	if _, err := checkOne(manifest, readFixture(t, sigValidArmored), kr); err != nil {
		t.Fatalf("positive control: checkOne(%s) = %v, want nil", sigValidArmored, err)
	}

	for _, tc := range classifyCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := checkOne(manifest, blobBytes(t, tc.fixture), kr)
			if err == nil {
				t.Fatalf("checkOne(%s) = nil, want a failure to classify", tc.name)
			}
			// Killing mutation, actually run against this file: change
			// classify's `default` arm to return StatusMissingPassphrase, a
			// code in the vocabulary that no verdict can carry. Every row
			// reaching that arm fails, this one with
			//
			//	verify_test.go:354: classify(a blob that is not a signature at all) = MISSING_PASSPHRASE, want ERRSIG
			//
			// which is the arm's whole job: an error nothing here anticipated
			// has to land on a status a verdict can carry, or the ignore set
			// an operator configured against the vocabulary silently stops
			// describing what the run can produce. The vocabulary test fails too.
			if got := classify(err); got != tc.want {
				t.Fatalf("classify(%s) = %s, want %s", tc.name, got, tc.want)
			}
		})
	}
}

// TestEmptyPacketStreamWouldReportNoPubKey pins the premise errNoSignatureData
// exists for, against the library rather than in prose.
//
// checkOne raises its own verdict for a blob that carries no OpenPGP data
// because the library answers such a stream with the same error a signature from
// an unknown key produces - so passing it through would report an empty source
// as NO_PUBKEY, a status an operator may have configured as ignorable. That is a
// claim about go-crypto, and a release that answered differently would make the
// early verdict unnecessary without anything saying so.
//
// The control is the same call over a signature that verifies, which returns an
// entity and a nil error, so the row above it is the empty stream rather than
// this keyring or this manifest.
func TestEmptyPacketStreamWouldReportNoPubKey(t *testing.T) {
	t.Parallel()

	kr := loadTestKeyring(t, keyringFixture)
	manifest := readFixture(t, manifestAFixture)

	// The binary encoding, since this call takes a packet stream rather than an
	// armor envelope.
	signer, err := openpgp.CheckDetachedSignature(kr.entities,
		bytes.NewReader(manifest), bytes.NewReader(readFixture(t, sigValidBinary)), nil)
	if err != nil || signer == nil {
		t.Fatalf("positive control: CheckDetachedSignature(%s) = %v, %v, want an entity and nil", sigValidBinary, signer, err)
	}

	_, err = openpgp.CheckDetachedSignature(kr.entities, bytes.NewReader(manifest), bytes.NewReader(nil), nil)
	if got := classify(err); got != StatusNoPubKey {
		t.Fatalf("CheckDetachedSignature(an empty packet stream) = %v, classified %s, want %s", err, got, StatusNoPubKey)
	}
}

// TestCheckOneRefusesAnArmorBlockOfTheWrongKind covers the envelope check
// between the decode and the packet gate.
//
// It takes a blob whose FIRST armor block is not a signature while the bytes
// behind it hold the signature header, since the header test above the decode is
// a substring search over the whole blob and the decode reads the first block
// only - a public key export with a real signature appended is exactly that
// shape, and it is one a mistaken `cat` produces.
//
// The positive control is the same signature on its own, through the same call:
// it verifies, so the refusal is the block in front of it rather than these
// bytes.
func TestCheckOneRefusesAnArmorBlockOfTheWrongKind(t *testing.T) {
	t.Parallel()

	// The whole message hand-spelled, and apart from the production string it
	// checks, so that a mutation to that string cannot reshape the expectation
	// into agreeing with it.
	const want = "openpgp: invalid argument: expected 'PGP SIGNATURE', got: PGP PUBLIC KEY BLOCK"

	kr := loadTestKeyring(t, keyringFixture)
	manifest := readFixture(t, manifestAFixture)
	signature := readFixture(t, sigValidArmored)
	if _, err := checkOne(manifest, signature, kr); err != nil {
		t.Fatalf("positive control: checkOne(%s) = %v, want nil", sigValidArmored, err)
	}

	blob := make([]byte, 0, len(readFixture(t, armoredFixture))+len(signature))
	blob = append(blob, readFixture(t, armoredFixture)...)
	blob = append(blob, signature...)

	_, err := checkOne(manifest, blob, kr)
	// Killing mutation, actually run against this file: delete checkOne's block
	// type check, so a decoded block of any kind is handed to the packet gate.
	// This assertion then fails with
	//
	//	verify_test.go:436: checkOne(a key block with a signature behind it) = malformed OpenPGP packet framing: a tag 6
	//	packet has no place in this stream, want "openpgp: invalid argument: expected 'PGP SIGNATURE', got: PGP PUBLIC KEY
	//	BLOCK"
	//
	// which is the gate behind it catching the same file one step later, under a
	// message that names a packet rather than the block an operator can see.
	if err == nil || err.Error() != want {
		t.Fatalf("checkOne(a key block with a signature behind it) = %v, want %q", err, want)
	}
	// The status is asked separately, because what an operator's ignore list
	// covers is decided by it rather than by the message: a well-formed OpenPGP
	// object of the wrong kind is not a signature that failed.
	if got := classify(err); got != StatusErrSig {
		t.Fatalf("classify(a key block with a signature behind it) = %s, want %s", got, StatusErrSig)
	}
}

// verifyCase is one row of the semantic matrix: the configured policy, the
// blobs gathered, and everything the Result must say about them.
type verifyCase struct {
	name         string
	required     string
	ignore       []string
	blobs        []string
	wantVerified int
	wantFailures int
	wantPass     bool
	wantVacuous  bool
}

// verifyCases is ansible's decision function, spelled out one cell at a time.
//
// The zero-blob rows at the top are the frozen parity this package exists to
// reproduce rather than to improve, and the first of them is the signature
// stripping hole itself: a run with verification switched on, a keyring
// configured, and the default count of one passes when a server or a poisoned
// snapshot serves no signatures at all. That is ansible-galaxy's own answer, an
// operator closes it by writing "+1" rather than by this package deciding
// differently, and what does NOT carry over is the silence - every one of these
// passes is reported through Result.VacuousPass for the caller to warn about.
//
//nolint:gochecknoglobals // a fixed table consumed by one test, not mutable shared state
var verifyCases = []verifyCase{
	{name: "count of one with nothing gathered", required: "1", wantPass: true, wantVacuous: true},
	{name: "strict count of one with nothing gathered", required: "+1"},
	{name: "all with nothing gathered", required: "all", wantPass: true, wantVacuous: true},
	{name: "strict all with nothing gathered", required: "+all"},
	{
		name: "count of one with one valid signature", required: "1",
		blobs: []string{sigValidArmored}, wantVerified: 1, wantPass: true,
	},
	{
		name: "count of one with one invalid signature", required: "1",
		blobs: []string{sigOverManifestB}, wantFailures: 1,
	},
	// The tolerance rule: a failure below the required count is recorded and
	// reported, and the run still passes because the count was reached anyway.
	{
		name: "count of two tolerates a failure below the count", required: "2",
		blobs:        []string{sigValidArmored, sigOverManifestB, sigSecondSigner},
		wantVerified: 2, wantFailures: 1, wantPass: true,
	},
	// The same three blobs reordered: the count is reached before the invalid
	// one is looked at, so it is never checked and never recorded. This is the
	// row that makes the loop's early stop observable at all - the verdict is a
	// pass either way, only the reported failure set differs.
	{
		name: "count of two stops at the second signer", required: "2",
		blobs:        []string{sigValidArmored, sigSecondSigner, sigOverManifestB},
		wantVerified: 2, wantPass: true,
	},
	// The three rows below are one signature standing in for a second signer,
	// which is what a count above one exists to prevent: the blob list arrives
	// across a trust boundary, so offering the same signature under two origins
	// costs nothing. A count of blobs would pass every one of them.
	{
		name: "count of two refuses one signature supplied twice", required: "2",
		blobs:        []string{sigValidArmored, sigValidArmored},
		wantVerified: 1,
	},
	{
		name: "count of three refuses one signature supplied three times", required: "3",
		blobs:        []string{sigValidArmored, sigValidArmored, sigValidArmored},
		wantVerified: 1,
	},
	// The same key in its two encodings: not byte-identical, so a replay check
	// that hashed the blob would pass this while one key still signed both.
	{
		name: "count of two refuses one key under both encodings", required: "2",
		blobs:        []string{sigValidArmored, sigValidBinary},
		wantVerified: 1,
	},
	// The positive control for the three above, on the same policy and the same
	// keyring: two genuinely distinct signers do satisfy a count of two, so
	// those refusals are the replay being caught rather than a count of two
	// being unsatisfiable here.
	{
		name: "count of two accepts two distinct signers", required: "2",
		blobs:        []string{sigValidArmored, sigSecondSigner},
		wantVerified: 2, wantPass: true,
	},
	{
		name: "all refuses one failure beside a valid signature", required: "all",
		blobs: []string{sigValidArmored, sigOverManifestB}, wantVerified: 1, wantFailures: 1,
	},
	{
		name: "all tolerates a failure whose status is ignored", required: "all",
		ignore: []string{string(StatusNoPubKey)},
		blobs:  []string{sigValidArmored, sigOutsiderKey}, wantVerified: 1, wantPass: true,
	},
	// An ignored failure is invisible to both counters, which under "all" - a
	// policy that asks only that nothing be recorded against the collection -
	// is a pass with nothing verified. It is the one vacuous pass that arrives
	// with blobs in hand rather than with none, and so the one a caller could
	// not have derived from an empty blob list.
	{
		name: "all with only an ignored failure", required: "all",
		ignore: []string{string(StatusNoPubKey)},
		blobs:  []string{sigOutsiderKey}, wantPass: true, wantVacuous: true,
	},
	// The same blobs under a counted policy refuse, and the difference is the
	// whole shape of the vacuous clause: it asks whether a signature was
	// GATHERED, which an ignored failure does not change, so this is not the
	// "nothing was gathered" case and the count of one is genuinely unmet.
	// ansible answers this cell the same way, on the same term of the same
	// expression - see this file's own note above TestVerifySemanticMatrix.
	{
		name: "count of one with a single ignored failure", required: "1",
		ignore: []string{string(StatusNoPubKey)}, blobs: []string{sigOutsiderKey},
	},
	{
		name: "strict count of one with a single ignored failure", required: "+1",
		ignore: []string{string(StatusNoPubKey)}, blobs: []string{sigOutsiderKey},
	},
	// A floor of zero is the one spelling under which a signature that verified
	// leaves the counts unequal, since the loop's early stop can never fire on
	// it. ansible refuses this cell for that reason and so does this, which is
	// what makes the last clause an equality rather than an "at least".
	{name: "count of zero with nothing gathered", required: "0", wantPass: true, wantVacuous: true},
	{
		name: "count of zero refuses a signature that verified", required: "0",
		blobs: []string{sigValidArmored}, wantVerified: 1,
	},
	// Every damaged fixture in one row, each tolerated by its own status, which
	// is what keeps a refusal row's fixture from being one this package never
	// managed to look at: reaching the ignore set at all means the blob was
	// checked and classified.
	{
		name: "all tolerates every status it was told to", required: "all",
		ignore: []string{
			string(StatusExpKeySig), string(StatusRevKeySig), string(StatusExpSig),
			string(StatusBadArmor), string(StatusNoData),
		},
		blobs: []string{
			sigExpiredKey, sigRevokedKey, sigExpiredSig, sigFoldedArmor, sigBadBase64, noDataBlob,
		},
		wantPass: true, wantVacuous: true,
	},
}

// TestVerifySemanticMatrix walks the decision function cell by cell.
//
// The verdict is ansible-core 2.21.2's verify_file_signatures, and the third
// clause is `(not detached_signatures) or (require_count == successful)`: the
// first term reads the list as it was handed over, so a blob whose failure was
// ignored still counts as a signature having been gathered even though neither
// counter moved. That is what separates the "all with only an ignored failure"
// row from the "count of one with a single ignored failure" row below it, and
// it is the one place where "an ignored failure is invisible" stops short of
// "the run behaves as though nothing was gathered".
func TestVerifySemanticMatrix(t *testing.T) {
	t.Parallel()

	kr := loadTestKeyring(t, keyringFixture)
	manifest := readFixture(t, manifestAFixture)

	for _, tc := range verifyCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			policy := testPolicy(t, tc.required, tc.ignore)
			blobs := signatureBlobs(t, tc.blobs...)

			result, err := verifyBlobs(manifest, blobs, kr, policy)
			passed := err == nil
			// Killing mutation, actually run against this file: drop the
			// `gathered == 0 ||` term from Policy.verdict's last clause. Only
			// the count-of-one-with-nothing-gathered row then fails, with
			//
			//	verify_test.go:647: Verify(count of one with nothing gathered) passed = false, want true (err: collection
			//	    signature verification failed: fewer valid signatures than required: got 0, need 1)
			//
			// which is the frozen parity this reproduces deliberately. The
			// "all with nothing gathered" row survives it, because an "all"
			// policy is answered by the clause above and never reaches this
			// one. The message is split over two lines to fit the line limit.
			//
			// Killing mutation, actually run against this file: delete
			// Policy.verdict's `if p.Required.Strict && verified == 0` clause.
			// Both strict zero-blob rows then fail, one of them with
			//
			//	verify_test.go:647: Verify(strict count of one with nothing gathered) passed = true, want false (err: <nil>)
			//
			// while the third strict row is untouched: a blob was gathered
			// there, so the count clause refuses it whatever strict says.
			//
			// Killing mutation, actually run against this file: drop
			// recordSigner's `slices.Contains` guard, so every blob that
			// verifies appends and the count is a count of blobs again. All
			// three replay rows then pass their policy, one of them failing
			// here with
			//
			//	verify_test.go:647: Verify(count of two refuses one signature supplied twice) passed = true, want false (err: <nil>)
			//
			// and the two-distinct-signers row beside them is untouched, which
			// is what separates "replays no longer count" from "a count of two
			// stopped being satisfiable".
			if passed != tc.wantPass {
				t.Fatalf("Verify(%s) passed = %t, want %t (err: %v)", tc.name, passed, tc.wantPass, err)
			}
			if !passed && !errors.Is(err, helpers.ErrSignatureVerificationFailed) {
				t.Fatalf("Verify(%s) error = %v, want the verification-failed sentinel", tc.name, err)
			}
			if result.Verified != tc.wantVerified {
				t.Fatalf("Verify(%s) Verified = %d, want %d", tc.name, result.Verified, tc.wantVerified)
			}
			// Killing mutation, actually run against this file: replace
			// walkBlobs' `return walk, nil` in the count-reached arm with
			// `continue`. Of this test's rows only the second-signer one fails,
			//
			//	verify_test.go:666: Verify(count of two stops at the second signer) recorded 1 failures, want 0
			//
			// while its verdict and its Verified count are both unchanged:
			// removing the early stop costs a public-key operation and a
			// reported failure for a signature the policy had already stopped
			// caring about. TestVerifyStopsPullingOnceTheCountIsMet fails too.
			if len(result.Failures) != tc.wantFailures {
				t.Fatalf("Verify(%s) recorded %d failures, want %d", tc.name, len(result.Failures), tc.wantFailures)
			}
			// Killing mutation, actually run against this file: compute
			// VacuousPass as `passed && walk.gathered == 0`, the derivation the
			// field exists to keep a caller from making. Both rows that pass
			// with a blob in hand and nothing verified fail, one of them with
			//
			//	verify_test.go:678: Verify(all with only an ignored failure) VacuousPass = false, want true
			//
			// which is exactly the case an empty blob list cannot reach, and
			// the whole reason this is a field rather than a caller's guess.
			if result.VacuousPass != tc.wantVacuous {
				t.Fatalf("Verify(%s) VacuousPass = %t, want %t", tc.name, result.VacuousPass, tc.wantVacuous)
			}
		})
	}
}

// TestVerifyReportsTheFixtureItRefused is the positive control for the two
// refusals that are about context rather than about the blob: a signature the
// keyring cannot name, and a signature over another document. Both fixtures
// verify cleanly once the missing half is supplied, so their statuses in the
// matrix above are verdicts about the keyring and the manifest rather than
// about a blob that was never a signature.
//
// The expired-key, revoked-key and expired-signature fixtures need no such
// control: go-crypto reaches those three only after the signature itself has
// verified, so carrying one of those statuses is itself the proof that the
// blob is a real signature over this manifest.
func TestVerifyReportsTheFixtureItRefused(t *testing.T) {
	t.Parallel()

	manifestA := readFixture(t, manifestAFixture)
	manifestB := readFixture(t, manifestBFixture)

	outsiderKeyring := loadTestKeyring(t, outsiderKeyringFixture)
	if _, err := checkOne(manifestA, readFixture(t, sigOutsiderKey), outsiderKeyring); err != nil {
		t.Fatalf("checkOne(%s) against the keyring holding its key = %v, want nil", sigOutsiderKey, err)
	}

	kr := loadTestKeyring(t, keyringFixture)
	if _, err := checkOne(manifestB, readFixture(t, sigOverManifestB), kr); err != nil {
		t.Fatalf("checkOne(%s) against the manifest it signs = %v, want nil", sigOverManifestB, err)
	}
}

// TestVerifyAcceptsBothSignatureEncodings covers the branch the armored cases
// do not reach, and shows why the branch exists at all: the same signature in
// its binary encoding verifies through Verify, and fails outright when handed
// to the armored entry point that a sniff-free implementation would use for
// every blob.
func TestVerifyAcceptsBothSignatureEncodings(t *testing.T) {
	t.Parallel()

	kr := loadTestKeyring(t, keyringFixture)
	manifest := readFixture(t, manifestAFixture)
	policy := testPolicy(t, "1", nil)

	for _, name := range []string{sigValidArmored, sigValidBinary} {
		result, err := verifyBlobs(manifest, signatureBlobs(t, name), kr, policy)
		if err != nil {
			t.Fatalf("Verify(%s) = %v, want nil", name, err)
		}
		if result.Verified != 1 {
			t.Fatalf("Verify(%s) Verified = %d, want 1", name, result.Verified)
		}
	}

	binary := readFixture(t, sigValidBinary)
	_, err := openpgp.CheckArmoredDetachedSignature(kr.entities, bytes.NewReader(manifest), bytes.NewReader(binary), nil)
	if err == nil {
		t.Fatalf("openpgp.CheckArmoredDetachedSignature(%s) = nil, want a failure: the sniff would then be pointless", sigValidBinary)
	}
}

// TestVerifyRefusesWithoutKeyMaterial pins the fail-closed guard: a call with
// no keyring reports a configuration failure and never reaches the library,
// where a nil keyring would be a panic rather than a verdict.
//
// The refusal is deliberately not a verification verdict. Nothing was checked,
// so the run learned nothing about the collection, and the remedy is the
// operator's configuration rather than the artifact.
func TestVerifyRefusesWithoutKeyMaterial(t *testing.T) {
	t.Parallel()

	manifest := readFixture(t, manifestAFixture)
	blobs := signatureBlobs(t, sigValidArmored)
	policy := testPolicy(t, "1", nil)

	// The same call with key material passes, so the refusal below is the
	// missing keyring and not this manifest or this blob.
	if _, err := verifyBlobs(manifest, blobs, loadTestKeyring(t, keyringFixture), policy); err != nil {
		t.Fatalf("positive control: Verify with a keyring = %v, want nil", err)
	}

	_, err := verifyBlobs(manifest, blobs, nil, policy)
	if !errors.Is(err, helpers.ErrKeyringRequired) {
		t.Fatalf("Verify with no keyring = %v, want the keyring-required sentinel", err)
	}
	if errors.Is(err, helpers.ErrSignatureVerificationFailed) {
		t.Fatalf("Verify with no keyring reports a verification verdict: %v", err)
	}
}

// clauseCase is one row of TestVerifyErrorNamesTheClauseThatFired.
type clauseCase struct {
	name     string
	required string
	want     string
	blobs    []string
}

// clauseCases covers all three refusal clauses. The phrases are written out
// rather than built from the production strings, so a reworded message fails
// here instead of agreeing with whatever it was changed to.
//
//nolint:gochecknoglobals // a fixed table consumed by one test, not mutable shared state
var clauseCases = []clauseCase{
	{name: "strict with nothing verified", required: "+1", blobs: []string{sigOverManifestB}, want: "no valid signature"},
	{
		name: "all with a failure", required: "all",
		blobs: []string{sigValidArmored, sigOverManifestB}, want: "some signatures failed",
	},
	{
		name: "count not reached", required: "2",
		blobs: []string{sigValidArmored, sigOverManifestB}, want: "fewer valid signatures than required: got 1, need 2",
	},
}

// TestVerifyErrorNamesTheClauseThatFired pins what a failing run tells the
// operator: which of the three clauses refused it, and every failure behind
// that headline.
//
// The clauses share one exit code, so the message is the only place the
// difference between "nothing verified", "something failed" and "not enough
// verified" survives - and each has a different remedy.
func TestVerifyErrorNamesTheClauseThatFired(t *testing.T) {
	t.Parallel()

	kr := loadTestKeyring(t, keyringFixture)
	manifest := readFixture(t, manifestAFixture)

	for _, tc := range clauseCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			policy := testPolicy(t, tc.required, nil)
			_, err := verifyBlobs(manifest, signatureBlobs(t, tc.blobs...), kr, policy)
			if err == nil {
				t.Fatalf("Verify(%s) = nil, want a refusal", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Verify(%s) error does not name the clause %q:\n%v", tc.name, tc.want, err)
			}
			// Each cause is joined behind the headline rather than rendered
			// into it, so a caller can still reach the underlying failure. The
			// blob every row carries is a signature over another manifest, so
			// go-crypto's own signature error is what has to stay reachable.
			// The test asks errors.As directly rather than through classify's
			// own helper, which would agree with a broken join as readily as
			// with a working one.
			var sigErr pgperrors.SignatureError
			if !errors.As(err, &sigErr) {
				t.Fatalf("Verify(%s) error does not carry the joined cause:\n%v", tc.name, err)
			}
			if !strings.Contains(err.Error(), sigOverManifestB) {
				t.Fatalf("Verify(%s) error does not name the failing origin:\n%v", tc.name, err)
			}
		})
	}
}

// TestVerifyIsDeterministicAcrossRuns pins the first of this package's two
// deliberate deviations from ansible, and the one that changes no verdict:
// ansible iterates a set, so which signatures it checks - and therefore which
// failures it reports - is undefined; this walks the order the caller's pull
// yields, so both are the same on every run. The second deviation, counting
// distinct signers rather than blobs, is pinned by the replay rows of the
// semantic matrix instead.
//
// The blobs are three distinct failures around one success, which is what makes
// an order observable at all: a single failure would report identically under
// any iteration order. Each run builds its own source over the same slice, so
// what repeats is the walk and not a cursor left where the previous run
// stopped.
func TestVerifyIsDeterministicAcrossRuns(t *testing.T) {
	t.Parallel()

	kr := loadTestKeyring(t, keyringFixture)
	manifest := readFixture(t, manifestAFixture)
	policy := testPolicy(t, "all", nil)
	blobs := signatureBlobs(t, sigOutsiderKey, sigOverManifestB, sigValidArmored, sigExpiredKey)

	want := []string{
		sigOutsiderKey + " " + string(StatusNoPubKey),
		sigOverManifestB + " " + string(StatusBadSig),
		sigExpiredKey + " " + string(StatusExpKeySig),
	}

	for run := range deterministicRuns {
		result, err := verifyBlobs(manifest, blobs, kr, policy)
		if err == nil {
			t.Fatalf("run %d: Verify = nil, want a refusal", run)
		}
		got := make([]string, 0, len(result.Failures))
		for _, failure := range result.Failures {
			got = append(got, failure.Origin+" "+string(failure.Status))
		}
		if len(got) != len(want) {
			t.Fatalf("run %d: Verify reported %d failures %v, want %d", run, len(got), got, len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("run %d: failure %d is %q, want %q", run, i, got[i], want[i])
			}
		}
		if result.Verified != 1 {
			t.Fatalf("run %d: Verified = %d, want 1", run, result.Verified)
		}
	}
}

// TestClassifyNeverAnswersOutsideTheVocabulary pins the property status.go
// depends on: the statuses classify produces are drawn from the eight that file
// declares a verdict can carry, so an operator's ignore list - which is
// validated against that vocabulary - can name every status a run can report.
//
// The unrecognized-error row is what the guarantee rests on, since it is the
// only input whose status is chosen rather than mapped.
//
// The count assertion ahead of the loop keeps maxRenderedFailures pinned
// beside the vocabulary it is derived from, in the one place that vocabulary
// is already enumerated, so a ninth producible status added here without
// moving that constant fails on the spot instead of only silently widening
// what a render-capped message can no longer promise to exhibit every shape
// of.
func TestClassifyNeverAnswersOutsideTheVocabulary(t *testing.T) {
	t.Parallel()

	producible := map[Status]struct{}{
		StatusBadSig: {}, StatusErrSig: {}, StatusNoPubKey: {}, StatusExpKeySig: {},
		StatusRevKeySig: {}, StatusExpSig: {}, StatusNoData: {}, StatusBadArmor: {},
	}
	// KILLING MUTATION, run and reverted: maxRenderedFailures changed from 8
	// to 7 in verify.go. This assertion then fails with
	//
	//	verify_test.go:914: len(producible) = 8, want maxRenderedFailures (7): the render cap is derived from this vocabulary's size
	if len(producible) != maxRenderedFailures {
		t.Fatalf("len(producible) = %d, want maxRenderedFailures (%d): the render cap is derived from this vocabulary's size",
			len(producible), maxRenderedFailures)
	}

	errs := []error{
		errNoSignatureData,
		errMalformedSignaturePacket,
		pgperrors.ErrUnknownIssuer,
		pgperrors.ErrKeyRevoked,
		pgperrors.ErrKeyExpired,
		pgperrors.ErrSignatureExpired,
		pgperrors.SignatureError("EdDSA verification failure"),
		errUnanticipated,
	}
	for _, err := range errs {
		if _, ok := producible[classify(err)]; !ok {
			t.Fatalf("classify(%v) = %s, which is outside the eight statuses a verdict may carry", err, classify(err))
		}
	}
}

// TestVerifyStrictDoesNotMakeAFailureFatal pins what the strict marker is, and
// what it deliberately is not.
//
// It contributes exactly one clause - at least one signature verified - so a
// "+1" run with two failures beside one success passes. The row below it is the
// contrast on the identical blobs: "all" is the spelling that makes a checked
// failure fatal, and it refuses them.
//
// The pair exists because the two are easy to conflate in prose and the
// difference is a verdict: an operator who wants "nothing may fail" and writes
// "+N" gets a policy that tolerates every failure below its count.
func TestVerifyStrictDoesNotMakeAFailureFatal(t *testing.T) {
	t.Parallel()

	kr := loadTestKeyring(t, keyringFixture)
	manifest := readFixture(t, manifestAFixture)
	// The success is last, so both failures are checked and recorded before the
	// count is reached; with it first the loop would stop and never see them.
	blobs := signatureBlobs(t, sigOverManifestB, sigOutsiderKey, sigValidArmored)

	result, err := verifyBlobs(manifest, blobs, kr, testPolicy(t, "+1", nil))
	if err != nil {
		t.Fatalf("Verify(+1 with two failures beside a success) = %v, want nil", err)
	}
	if len(result.Failures) != 2 {
		t.Fatalf("Verify(+1) recorded %d failures, want 2 checked and tolerated", len(result.Failures))
	}
	if result.Verified != 1 {
		t.Fatalf("Verify(+1) Verified = %d, want 1", result.Verified)
	}

	// The same blobs under the spelling that does make a failure fatal.
	if _, err = verifyBlobs(manifest, blobs, kr, testPolicy(t, "all", nil)); err == nil {
		t.Fatalf("Verify(all with two failures beside a success) = nil, want a refusal")
	}
}

// TestVerifyNamesAFloorOfZeroInItsOwnWords pins the one refusal whose default
// wording says nothing an operator can act on.
//
// A count of zero is refused by the same equality every other count is - see
// Policy.verdict for why that clause is an equality and stays one - but "fewer
// valid signatures than required: got 1, need 0" describes the opposite of what
// happened. The verdict is unchanged; only the sentence is.
func TestVerifyNamesAFloorOfZeroInItsOwnWords(t *testing.T) {
	t.Parallel()

	kr := loadTestKeyring(t, keyringFixture)
	manifest := readFixture(t, manifestAFixture)

	// The control: a floor of zero with nothing gathered passes, so the refusal
	// below is the signature that verified and not the spelling itself.
	if _, err := verifyBlobs(manifest, nil, kr, testPolicy(t, "0", nil)); err != nil {
		t.Fatalf("positive control: Verify(0 with nothing gathered) = %v, want nil", err)
	}

	_, err := verifyBlobs(manifest, signatureBlobs(t, sigValidArmored), kr, testPolicy(t, "0", nil))
	if err == nil {
		t.Fatalf("Verify(0 with a signature that verified) = nil, want a refusal")
	}
	const want = "a required count of 0 is satisfied only while nothing verifies; 1 did"
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("Verify(0) error does not carry %q:\n%v", want, err)
	}
	if strings.Contains(err.Error(), "fewer valid signatures") {
		t.Fatalf("Verify(0) error still reads as a shortfall:\n%v", err)
	}
}

// TestVerifyStopsPullingOnceTheCountIsMet pins the half of the pull contract a
// verdict cannot show: the loop stops asking for blobs the moment the count is
// reached, so a caller that fetches inside its own pull never pays for a source
// the policy had already stopped caring about.
//
// The third blob would fail if it were checked, which is what makes the count
// the reason the pull stopped rather than the list running out.
func TestVerifyStopsPullingOnceTheCountIsMet(t *testing.T) {
	t.Parallel()

	kr := loadTestKeyring(t, keyringFixture)
	manifest := readFixture(t, manifestAFixture)
	blobs := signatureBlobs(t, sigValidArmored, sigSecondSigner, sigOverManifestB)

	next, pulled := blobSource(blobs)
	result, err := Verify(manifest, next, kr, testPolicy(t, "2", nil))
	if err != nil {
		t.Fatalf("Verify(count of two) = %v, want nil", err)
	}
	if result.Verified != 2 {
		t.Fatalf("Verify(count of two) Verified = %d, want 2", result.Verified)
	}
	if *pulled != 2 {
		t.Fatalf("Verify(count of two) pulled %d blobs, want 2 of the 3 supplied", *pulled)
	}
}

// TestVerifySurfacesAGatherErrorWhereItSits pins the other half of that
// contract: a source that cannot be fetched fails at its own position in the
// list, and everything checked before it is still reported.
//
// A slice parameter could not express this. A caller holding one has already
// fetched every source, so a single unreachable one aborts the gather before
// any signature has been looked at, and the run loses what the reachable
// sources would have said.
func TestVerifySurfacesAGatherErrorWhereItSits(t *testing.T) {
	t.Parallel()

	kr := loadTestKeyring(t, keyringFixture)
	manifest := readFixture(t, manifestAFixture)
	policy := testPolicy(t, "2", nil)
	good := signatureBlobs(t, sigValidArmored)[0]

	// One blob checked, then a source that could not be fetched.
	pulls := 0
	next := func() (Blob, bool, error) {
		pulls++
		if pulls == 1 {
			return good, true, nil
		}

		return Blob{}, false, errGatherFailed
	}

	result, err := Verify(manifest, next, kr, policy)
	if !errors.Is(err, errGatherFailed) {
		t.Fatalf("Verify with a failing source = %v, want the gather error", err)
	}
	if errors.Is(err, helpers.ErrSignatureVerificationFailed) {
		t.Fatalf("Verify reports a gather failure as a verification verdict: %v", err)
	}
	if result.Verified != 1 {
		t.Fatalf("Verify with a failing source Verified = %d, want the 1 checked before it", result.Verified)
	}

	// The same failure on the first pull: nothing was checked, and the count
	// clause never gets to answer for a list that was never gathered.
	first := func() (Blob, bool, error) { return Blob{}, false, errGatherFailed }
	result, err = Verify(manifest, first, kr, policy)
	if !errors.Is(err, errGatherFailed) {
		t.Fatalf("Verify with a source failing at once = %v, want the gather error", err)
	}
	if result.Verified != 0 {
		t.Fatalf("Verify with a source failing at once Verified = %d, want 0", result.Verified)
	}
}

// errRenderCapSentinel is the failure buildRenderCapFailures always gives to
// the LAST failure it builds, so a positive errors.Is result over a produced
// error can prove that cause stayed reachable even when the message that
// carries it left it unrendered.
var errRenderCapSentinel = errors.New("verify_test: a cause past the render cap")

// errGenericRenderCapCause is Err for every failure buildRenderCapFailures
// builds except the last one: a single fixed value is enough, since only the
// last failure's own Err is ever asked about by errors.Is.
var errGenericRenderCapCause = errors.New("verify_test: a signature that failed to verify")

// buildRenderCapFailures builds n Failure values, each with its own origin so
// a render cap can be observed by which origins made it into a message.
//
// n is a literal at every call site below rather than maxRenderedFailures
// itself, so a future change to that constant cannot silently move the
// fixture in lockstep with the cap it exists to probe - see the mutation
// fixture note on TestVerificationErrorBoundsWhatItRendersAndNotWhatItMatches.
func buildRenderCapFailures(n int) []Failure {
	failures := make([]Failure, n)
	for i := range failures {
		failures[i] = Failure{Origin: fmt.Sprintf("origin-%d", i), Status: StatusBadSig, Err: errGenericRenderCapCause}
	}
	failures[n-1].Err = errRenderCapSentinel

	return failures
}

// TestVerificationErrorBoundsWhatItRendersAndNotWhatItMatches pins
// verdictError's whole point: maxRenderedFailures bounds the negative
// verdict's own MESSAGE, and nothing else - every cause, rendered or not,
// stays reachable through errors.Is.
//
// The fixture counts below (9 and 8) are hand-spelled rather than built from
// maxRenderedFailures, on purpose: a fixture derived from the same constant
// the cap uses would move with it under a mutation and could never observe
// the cap changing at all.
func TestVerificationErrorBoundsWhatItRendersAndNotWhatItMatches(t *testing.T) {
	t.Parallel()

	policy := testPolicy(t, "1", nil)

	t.Run("past the cap", func(t *testing.T) {
		t.Parallel()

		failures := buildRenderCapFailures(9)
		err := verificationError(policy, 0, failures)
		lastOrigin := failures[len(failures)-1].Origin

		// KILLING MUTATION, run and reverted: verdictError.Error's `shown :=
		// min(len(e.causes), maxRenderedFailures+1)` replaced with `shown :=
		// len(e.causes)`, so the message renders every cause and the cap does
		// nothing. This assertion then fails with
		//
		//	verify_test.go:1141: verificationError() rendered the origin past the cap (origin-8):
		//	collection signature verification failed: fewer valid signatures than required: got 0, need 1
		//	BADSIG from "origin-0": verify_test: a signature that failed to verify
		//	...
		//	BADSIG from "origin-8": verify_test: a cause past the render cap
		if strings.Contains(err.Error(), lastOrigin) {
			t.Fatalf("verificationError() rendered the origin past the cap (%s):\n%v", lastOrigin, err)
		}

		const wantFooter = "showing the first 8 of 9 signature failures"
		if !strings.Contains(err.Error(), wantFooter) {
			t.Fatalf("verificationError() does not carry %q:\n%v", wantFooter, err)
		}

		// KILLING MUTATION, run and reverted: verdictError.Unwrap changed to
		// `return e.causes[:min(len(e.causes), maxRenderedFailures+1)]` - the
		// rejected design where the render cap also bounds what errors.Is can
		// reach. This assertion then fails with
		//
		//	verify_test.go:1160: verificationError() does not reach the cause past the render cap through errors.Is:
		//	collection signature verification failed: fewer valid signatures than required: got 0, need 1
		//	BADSIG from "origin-0": verify_test: a signature that failed to verify
		//	...
		//	showing the first 8 of 9 signature failures; 1 not shown, carrying: BADSIG
		if !errors.Is(err, errRenderCapSentinel) {
			t.Fatalf("verificationError() does not reach the cause past the render cap through errors.Is:\n%v", err)
		}
		if !errors.Is(err, helpers.ErrSignatureVerificationFailed) {
			t.Fatalf("verificationError() does not reach helpers.ErrSignatureVerificationFailed:\n%v", err)
		}
	})

	// The positive control on the same fixture builder: exactly at the cap,
	// nothing is hidden and no footer is rendered, which is what proves the
	// row above withheld its ninth origin because of the cap rather than
	// because this builder cannot produce a message rendering everything it
	// is handed.
	t.Run("at the cap", func(t *testing.T) {
		t.Parallel()

		failures := buildRenderCapFailures(8)
		err := verificationError(policy, 0, failures)

		// KILLING MUTATION, run and reverted: maxRenderedFailures changed
		// from 8 to 1 in verify.go. This row's own fixture still builds 8
		// failures - the count is hand-spelled, not derived from the mutated
		// constant - so the message now hides 7 of them, and this assertion
		// fails on the first origin it does not find, with
		//
		//	verify_test.go:1190: verificationError() at the cap does not render origin origin-1:
		//	collection signature verification failed: fewer valid signatures than required: got 0, need 1
		//	BADSIG from "origin-0": verify_test: a signature that failed to verify
		//	showing the first 1 of 8 signature failures; 7 not shown, carrying: BADSIG
		for _, failure := range failures {
			if !strings.Contains(err.Error(), failure.Origin) {
				t.Fatalf("verificationError() at the cap does not render origin %s:\n%v", failure.Origin, err)
			}
		}
		if strings.Contains(err.Error(), "showing the first") {
			t.Fatalf("verificationError() at the cap rendered a footer, though nothing was hidden:\n%v", err)
		}
	})

	// The eight decoys ahead of a BADSIG is the shape a security audit
	// reproduced: eight file sources signed by a key outside the keyring,
	// classifying NO_PUBKEY, ahead of a signature classifying BADSIG - the
	// strongest evidence available that an artifact's bytes are not what was
	// signed. The render cap fills on the eight decoys and never reaches the
	// ninth cause, so the footer is the one place that fact can still reach
	// an operator who only has the message - Result is dropped on this error
	// path, see verifyCollectionSignatures' own doc comment. The body is a
	// standalone function rather than an inline closure, which is what keeps
	// this test's own cyclomatic complexity under the shared limit.
	t.Run("footer names a status the cap hid, not one it rendered", func(t *testing.T) {
		t.Parallel()
		checkFooterNamesAHiddenStatus(t, policy)
	})
}

// checkFooterNamesAHiddenStatus is
// TestVerificationErrorBoundsWhatItRendersAndNotWhatItMatches' third
// subtest, pulled out into its own function rather than an inline closure so
// that function's own cyclomatic complexity stays under the shared limit; it
// deliberately calls no t.Helper(), so a failing assertion below is reported
// at its own line rather than at the t.Run call site above, which is what
// every quoted mutation output below pins.
//
//nolint:thelper // deliberately no t.Helper(): see the paragraph above.
func checkFooterNamesAHiddenStatus(t *testing.T, policy Policy) {
	statuses := append(repeatedStatuses(StatusNoPubKey, 8), StatusBadSig)
	failures := buildStatusFailures(statuses)
	hiddenOrigin := failures[len(failures)-1].Origin

	err := verificationError(policy, 0, failures)
	lines := strings.Split(err.Error(), "\n")
	footer := lines[len(lines)-1]

	// KILLING MUTATION, actually run against this file: hiddenStatuses'
	// body replaced with `return nil`, so a negative verdict never learns
	// which statuses its own render cap withheld. This assertion then
	// fails with
	//
	//	verify_test.go:1239: footer does not name the hidden status BADSIG: "showing the first 8 of 9 signature failures"
	if !strings.Contains(footer, string(StatusBadSig)) {
		t.Fatalf("footer does not name the hidden status BADSIG: %q", footer)
	}
	// KILLING MUTATION, actually run against this file: hiddenStatuses'
	// `failures[maxRenderedFailures:]` replaced with `failures`, so it
	// walks every failure rather than only the ones the cap withheld.
	// This assertion then fails with
	//
	//	verify_test.go:1249: footer names a status that was actually rendered: "showing the first 8 of 9 signature
	//	    failures; 1 not shown, carrying: NO_PUBKEY, BADSIG"
	if strings.Contains(footer, string(StatusNoPubKey)) {
		t.Fatalf("footer names a status that was actually rendered: %q", footer)
	}

	const wantFooterPrefix = "showing the first 8 of 9 signature failures"
	if !strings.Contains(footer, wantFooterPrefix) {
		t.Fatalf("footer does not carry %q: %q", wantFooterPrefix, footer)
	}
	if strings.Contains(err.Error(), hiddenOrigin) {
		t.Fatalf("verificationError() rendered the hidden failure's own origin (%s):\n%v", hiddenOrigin, err)
	}

	// The positive control on the same builder: a fixture carrying no
	// BADSIG at all still names its own hidden status, which is what
	// shows the refusal above is BADSIG being kept out specifically,
	// rather than the footer never naming a status at all.
	controlErr := verificationError(policy, 0, buildStatusFailures(repeatedStatuses(StatusNoPubKey, 9)))
	controlLines := strings.Split(controlErr.Error(), "\n")
	controlFooter := controlLines[len(controlLines)-1]
	if !strings.Contains(controlFooter, string(StatusNoPubKey)) {
		t.Fatalf("control footer does not name NO_PUBKEY: %q", controlFooter)
	}
	if strings.Contains(controlFooter, string(StatusBadSig)) {
		t.Fatalf("control footer names a status this fixture never carried: %q", controlFooter)
	}

	// KILLING MUTATION, actually run against this file: hiddenStatuses'
	// `slices.Contains` dedupe guard removed, so a status shared by more
	// than one hidden failure is named once per failure instead of once
	// for the status. This assertion then fails with
	//
	//	verify_test.go:1286: footer names BADSIG 2 times, want exactly 1 (the dedupe): "showing the first 8 of 10
	//	    signature failures; 2 not shown, carrying: BADSIG, BADSIG"
	dupStatuses := append(repeatedStatuses(StatusNoPubKey, 8), StatusBadSig, StatusBadSig)
	dupErr := verificationError(policy, 0, buildStatusFailures(dupStatuses))
	dupLines := strings.Split(dupErr.Error(), "\n")
	dupFooter := dupLines[len(dupLines)-1]
	if got := strings.Count(dupFooter, string(StatusBadSig)); got != 1 {
		t.Fatalf("footer names BADSIG %d times, want exactly 1 (the dedupe): %q", got, dupFooter)
	}
}

// repeatedStatuses returns n copies of status, for composing a
// buildStatusFailures fixture out of one run of a status followed by another.
func repeatedStatuses(status Status, n int) []Status {
	statuses := make([]Status, n)
	for i := range statuses {
		statuses[i] = status
	}

	return statuses
}

// buildStatusFailures builds one Failure per element of statuses, each with
// its own origin, so a render cap's footer can be observed by which STATUSES
// - not only which origins - made it past the cap.
func buildStatusFailures(statuses []Status) []Failure {
	failures := make([]Failure, len(statuses))
	for i, status := range statuses {
		failures[i] = Failure{Origin: fmt.Sprintf("origin-%d", i), Status: status, Err: errGenericRenderCapCause}
	}

	return failures
}
