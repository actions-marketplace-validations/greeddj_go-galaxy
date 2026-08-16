package signature

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// Fixtures under testdata are generated once and committed, never built at
// test time: generating a key costs seconds and needs a working gpg-agent,
// neither of which belongs in a unit test. All of them come from gpg 2.5.21,
// run against throwaway GNUPGHOMEs holding one key each.
//
// The first key backs every fixture not named further down. It was created
// with
//
//	gpg --batch --passphrase '' --quick-generate-key \
//	    'go-galaxy test key <test@example.invalid>' ed25519 sign never
//
// and then exported with
//
//	gpg --batch --export --armor 'test@example.invalid' > public.asc
//	gpg --batch --export 'test@example.invalid' > public.gpg
//	cp "$GNUPGHOME/pubring.kbx" pubring.kbx
//	gpg --batch --export 'absent@example.invalid' > empty.gpg
//	printf 'this file is not OpenPGP key material at all\n' > garbage.bin
//
// empty.gpg is zero bytes because that export names a key the keyring does not
// hold, which is the one shape openpgp.ReadKeyRing answers with an empty list
// and a nil error.
//
// A second key, in a GNUPGHOME of its own, backs second.asc; two-keys.asc is
// the two armored exports concatenated, which is how an operator builds a
// multi-key keyring:
//
//	gpg --batch --passphrase '' --quick-generate-key \
//	    'go-galaxy second test key <second@example.invalid>' ed25519 sign never
//	gpg --batch --export --armor 'second@example.invalid' > second.asc
//	cat public.asc second.asc > two-keys.asc
//
// A third key, again in its own GNUPGHOME, is the single exception to the rule
// that only public material is committed here: secret.gpg and secret.asc carry
// its private half. It is a discardable key generated for exactly this
// purpose - proving LoadKeyring refuses a file holding secret key material -
// and nothing else in this repository refers to it: it certifies nothing, no
// fixture was ever signed with it, and it is trusted by no test. Its public
// half is committed as secret-public.asc so the refusals have a same-key
// control.
//
//	gpg --batch --passphrase '' --quick-generate-key \
//	    'go-galaxy discardable secret test key <secret@example.invalid>' \
//	    ed25519 sign never
//	gpg --batch --pinentry-mode loopback --passphrase '' \
//	    --export-secret-keys 'secret@example.invalid' > secret.gpg
//	gpg --batch --pinentry-mode loopback --passphrase '' \
//	    --export-secret-keys --armor 'secret@example.invalid' > secret.asc
//	gpg --batch --export --armor 'secret@example.invalid' > secret-public.asc
//
// The private halves of the first two keys never left their GNUPGHOMEs.
const (
	testdataDir = "testdata"

	armoredFixture = "public.asc"
	binaryFixture  = "public.gpg"
	keyboxFixture  = "pubring.kbx"
	garbageFixture = "garbage.bin"
	emptyFixture   = "empty.gpg"
	secondFixture  = "second.asc"
	twoKeyFixture  = "two-keys.asc"
	// secretBinaryFixture and secretArmoredFixture are the two encodings of
	// one secret key export, so the refusal is shown to be this tool's policy
	// rather than a property of whichever reader the bytes reach.
	secretBinaryFixture  = "secret.gpg"
	secretArmoredFixture = "secret.asc"
	secretPublicFixture  = "secret-public.asc"
	// absentFixture names a file testdata deliberately does not hold.
	absentFixture = "no-such-keyring.asc"

	// signatureArmorBlock is a well-formed armor block that is not key
	// material. Its body is never decoded: openpgp.ReadArmoredKeyRing refuses
	// the block on its type before reading any of it.
	signatureArmorBlock = "-----BEGIN PGP SIGNATURE-----\n\naGVsbG8=\n-----END PGP SIGNATURE-----\n"

	// ceilingHeadroom is how far above the armored fixture's own length the
	// ceiling test sets its limit, so the control file fits and the padded one
	// does not.
	ceilingHeadroom = 16
	// ceilingFiller is how many bytes the ceiling test appends after the
	// armored fixture's end line. It only has to exceed ceilingHeadroom; the
	// margin is what keeps the truncated read landing inside the filler rather
	// than inside the armor block, which is the whole point of the fixture.
	ceilingFiller = 1024
)

func fixturePath(name string) string {
	return filepath.Join(testdataDir, name)
}

// requirePositiveControl loads the armored fixture out of the same testdata
// directory every refusal case below draws its input from, and fails the test
// if that load does not succeed.
//
// It is what separates "LoadKeyring refused this file" from "LoadKeyring
// refuses everything here": without it, a refusal assertion passes just as
// well against a loader that never reaches its check, or against a testdata
// directory the test binary cannot read at all.
func requirePositiveControl(t *testing.T) {
	t.Helper()

	kr, err := LoadKeyring(fixturePath(armoredFixture))
	if err != nil {
		t.Fatalf("positive control: LoadKeyring(%s) = %v, want nil", armoredFixture, err)
	}
	if kr.Len() == 0 {
		t.Fatalf("positive control: LoadKeyring(%s) Len() = 0, want > 0", armoredFixture)
	}
}

// keyFingerprints returns the hex primary-key fingerprints a keyring holds, so
// a case can compare which keys were read rather than only how many.
func keyFingerprints(kr *Keyring) []string {
	out := make([]string, 0, kr.Len())
	for _, entity := range kr.entities {
		out = append(out, hex.EncodeToString(entity.PrimaryKey.Fingerprint))
	}

	return out
}

// loadFingerprint loads a fixture that must hold exactly one key and returns
// that key's fingerprint.
func loadFingerprint(t *testing.T, name string) string {
	t.Helper()

	kr, err := LoadKeyring(fixturePath(name))
	if err != nil {
		t.Fatalf("LoadKeyring(%s) = %v, want nil", name, err)
	}
	fprs := keyFingerprints(kr)
	if len(fprs) != 1 {
		t.Fatalf("LoadKeyring(%s) holds %d keys, want exactly 1", name, len(fprs))
	}

	return fprs[0]
}

func TestLoadKeyringAcceptsArmoredExport(t *testing.T) {
	t.Parallel()

	path := fixturePath(armoredFixture)

	kr, err := LoadKeyring(path)
	if err != nil {
		t.Fatalf("LoadKeyring(%s) = %v, want nil", armoredFixture, err)
	}
	if kr.Len() == 0 {
		t.Fatalf("LoadKeyring(%s) Len() = 0, want > 0", armoredFixture)
	}
	if kr.Path() != path {
		t.Fatalf("Path() = %q, want %q", kr.Path(), path)
	}
}

// TestLoadKeyringAcceptsBinaryKeyring covers the branch the armored test does
// not reach at all. Both fixtures are exports of the same single key, so the
// entity counts have to agree: an equal, non-zero count is what shows the
// binary reader read the same key material rather than merely returning
// without an error.
func TestLoadKeyringAcceptsBinaryKeyring(t *testing.T) {
	t.Parallel()

	binary, err := LoadKeyring(fixturePath(binaryFixture))
	if err != nil {
		t.Fatalf("LoadKeyring(%s) = %v, want nil", binaryFixture, err)
	}
	if binary.Len() == 0 {
		t.Fatalf("LoadKeyring(%s) Len() = 0, want > 0", binaryFixture)
	}

	armored, err := LoadKeyring(fixturePath(armoredFixture))
	if err != nil {
		t.Fatalf("LoadKeyring(%s) = %v, want nil", armoredFixture, err)
	}
	if binary.Len() != armored.Len() {
		t.Fatalf("Len() = %d for %s, %d for %s, want equal", binary.Len(), binaryFixture, armored.Len(), armoredFixture)
	}
}

// TestLoadKeyringReadsEveryArmorBlock pins the accumulation across armor
// blocks.
//
// openpgp.ReadArmoredKeyRing decodes one block and stops, so a loader handing
// it the whole file answers `cat teamA.asc teamB.asc` with team A's keys and a
// nil error - a trust set silently smaller than the configured one, which then
// reports an artifact signed by a team B key as signed by nobody trusted. That
// is the failure the size ceiling's own refusal exists to avoid, arriving by a
// route the ceiling never sees.
func TestLoadKeyringReadsEveryArmorBlock(t *testing.T) {
	t.Parallel()

	first := loadFingerprint(t, armoredFixture)
	second := loadFingerprint(t, secondFixture)
	// The two blocks have to be different keys, or a count cannot tell "read
	// both blocks" from "read the first block twice" - and would stop telling
	// them apart entirely if the library ever deduplicated by fingerprint.
	if first == second {
		t.Fatalf("%s and %s carry the same key %s, so this case proves nothing", armoredFixture, secondFixture, first)
	}

	kr, err := LoadKeyring(fixturePath(twoKeyFixture))
	if err != nil {
		t.Fatalf("LoadKeyring(%s) = %v, want nil", twoKeyFixture, err)
	}
	// Killing mutation, actually run against this file: replace readEntities'
	// `readArmoredKeyRing(data)` call with the single-block
	// `openpgp.ReadArmoredKeyRing(bytes.NewReader(data))` it replaced. This
	// assertion then fails with
	//
	//	keyring_test.go:234: LoadKeyring(two-keys.asc) Len() = 1, want 2
	//
	// and no error is reported alongside it, which is the whole defect: the
	// second key is gone and nothing says so.
	if kr.Len() != 2 {
		t.Fatalf("LoadKeyring(%s) Len() = %d, want 2", twoKeyFixture, kr.Len())
	}

	got := keyFingerprints(kr)
	// The count above cannot be the whole assertion, and this line's first
	// failing state is a two-keys.asc rebuilt from two copies of one export:
	// every assertion above it still passes there, and only this one fails.
	// That is a fixture defect rather than a code mutation, which is exactly
	// the state it is here for.
	if !slices.Contains(got, first) || !slices.Contains(got, second) {
		t.Fatalf("LoadKeyring(%s) holds %v, want both %s and %s", twoKeyFixture, got, first, second)
	}
}

// TestLoadKeyringRefusesSecretKeyMaterial pins that a keyring holding a private
// key is refused, in both encodings, by policy rather than by accident.
//
// Verification needs public keys alone, so secret material in the configured
// keyring is a file an operator did not mean to point a build at. The binary
// encoding used to load clean; the armored one used to be refused only because
// the routing needle named the public armor header, so it reached the binary
// reader and died on a malformed packet - a message naming nothing an operator
// could act on.
func TestLoadKeyringRefusesSecretKeyMaterial(t *testing.T) {
	t.Parallel()
	requirePositiveControl(t)

	// A second control, on the very key the fixtures below carry: its public
	// half loads. Without it, "the loader refused secret.gpg" would be just as
	// consistent with this throwaway key being unreadable for a reason of its
	// own.
	if _, err := LoadKeyring(fixturePath(secretPublicFixture)); err != nil {
		t.Fatalf("positive control: LoadKeyring(%s) = %v, want nil", secretPublicFixture, err)
	}

	for _, name := range []string{secretBinaryFixture, secretArmoredFixture} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := LoadKeyring(fixturePath(name))
			// Killing mutation, actually run against this file: delete the
			// secretKeyPacketTags arm from judgeHeader, so the two secret-key
			// tags fall through to the allow-list that no longer admits them.
			// Both rows then keep failing the sentinel below and fail the
			// message assertion under it instead, one of them with
			//
			//	keyring_test.go:306: LoadKeyring(secret.gpg) error does not name the problem:
			//	keyring could not be read: "testdata/secret.gpg": malformed OpenPGP packet framing: a tag 5 packet has
			//	no place in this stream
			//
			// The branch this paragraph used to name - loadKeyring's own
			// holdsSecretKey call - no longer kills anything on its own: the walk
			// refuses both encodings before a packet is parsed, so deleting that
			// call leaves every row here passing. It stays as a backstop, and
			// TestHoldsSecretKeyReportsMaterialInEitherPlace is what pins it.
			if !errors.Is(err, helpers.ErrKeyringUnreadable) {
				t.Fatalf("LoadKeyring(%s) error = %v, want the unreadable sentinel", name, err)
			}
			// Killing mutation, actually run against this file: drop the
			// `|| bytes.Contains(data, []byte(privateKeyArmorHeader))` arm of
			// readEntities' routing test. Only the secret.asc row then fails,
			// with
			//
			//	keyring_test.go:306: LoadKeyring(secret.asc) error does not name the problem:
			//	    keyring could not be read: "testdata/secret.asc": malformed OpenPGP packet framing: 532 bytes are
			//	    not a packet header this walk can read
			//
			// while the assertion above it still passes: the armored secret key
			// is refused either way, but only the routed one is refused for the
			// reason that is true. The message is split over two lines so this
			// quote of it fits the line limit the repository lints for.
			if !strings.Contains(err.Error(), "secret key material") {
				t.Fatalf("LoadKeyring(%s) error does not name the problem:\n%v", name, err)
			}
			if !strings.Contains(err.Error(), "--export --armor") {
				t.Fatalf("LoadKeyring(%s) error does not name the remedy:\n%v", name, err)
			}
		})
	}
}

// TestHoldsSecretKeyReportsMaterialInEitherPlace pins the backstop no file can
// reach any more.
//
// The packet walk refuses the two secret-key tags before openpgp.ReadKeyRing
// sees them, so no entity this package builds from a file can carry private
// material and holdsSecretKey answers false for every one of them. That is what
// makes a file-driven test impossible and a direct one necessary: it takes
// hand-built entities to ask the question the function exists to answer, and
// without them the function could be inverted or emptied with the whole suite
// still green.
//
// The false rows are the positive control, on entities of the same shape: a
// primary key with no private half and a subkey with none answer false, so a true
// row is the private material rather than the presence of an entity at all.
func TestHoldsSecretKeyReportsMaterialInEitherPlace(t *testing.T) {
	t.Parallel()

	public := &packet.PublicKey{}
	private := &packet.PrivateKey{}

	for _, tc := range []struct {
		name     string
		entities openpgp.EntityList
		want     bool
	}{
		{name: "no entities at all", entities: nil},
		{name: "a public primary with a public subkey", entities: openpgp.EntityList{{
			PrimaryKey: public,
			Subkeys:    []openpgp.Subkey{{PublicKey: public}},
		}}},
		{name: "a private primary", entities: openpgp.EntityList{{
			PrimaryKey: public,
			PrivateKey: private,
		}}, want: true},
		{name: "a public primary with a private subkey", entities: openpgp.EntityList{{
			PrimaryKey: public,
			Subkeys:    []openpgp.Subkey{{PublicKey: public}, {PublicKey: public, PrivateKey: private}},
		}}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Killing mutation, actually run against this file: delete the subkey
			// loop from holdsSecretKey, which is the arm gpg's own exports never
			// reach. Exactly its row fails, with
			//
			//	keyring_test.go:366: holdsSecretKey(a public primary with a private subkey) = false, want true
			//
			// and nothing else in the package notices, this being the only place
			// that arm is reached at all.
			if got := holdsSecretKey(tc.entities); got != tc.want {
				t.Fatalf("holdsSecretKey(%s) = %t, want %t", tc.name, got, tc.want)
			}
		})
	}
}

// TestLoadKeyringRefusesADirectory covers the read failure between opening the
// keyring and looking at its bytes.
//
// A directory opens like a file and fails on the read, on darwin and on Linux
// alike, which is the one shape of that failure a test can stage portably - and
// it is not a contrived one: an operator pointing --keyring at a directory of
// exported keys is an ordinary mistake. What it pins is that such a failure is
// reported as a keyring this tool could not read, wrapped rather than rendered,
// so the cause stays reachable.
func TestLoadKeyringRefusesADirectory(t *testing.T) {
	t.Parallel()
	requirePositiveControl(t)

	dir := filepath.Join(t.TempDir(), "keyring.d")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("make the directory: %v", err)
	}

	_, err := LoadKeyring(dir)
	// Killing mutation, actually run against this file: have loadKeyring ignore
	// io.ReadAll's error and carry on with whatever it read. This assertion then
	// fails with
	//
	//	keyring_test.go:401: LoadKeyring(a directory) = keyring could not be read: "...keyring.d" holds no OpenPGP keys,
	//	want the read failure to be named
	//
	// which is the wrong diagnosis: an unreadable path reported as a readable
	// file holding nothing.
	if err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("LoadKeyring(a directory) = %v, want the read failure to be named", err)
	}
	if !errors.Is(err, helpers.ErrKeyringUnreadable) {
		t.Fatalf("LoadKeyring(a directory) error = %v, want the unreadable sentinel", err)
	}
}

// TestArmoredKeyRingRefusesANonFinalBlock pins that a block failing anywhere but
// last stops the file.
//
// readArmoredKeyRing reads every block, and the reason it refuses rather than
// skips is that using part of a keyring quietly is the failure the whole loader
// exists to avoid. Nothing else reaches that in-loop refusal: every other case
// here puts its bad block last, where the error comes from the call after the
// loop instead.
//
// Both rows are the same key export with something in front of it, and the
// control is that export alone, so a refusal is the leading block rather than
// the file having two of them.
func TestArmoredKeyRingRefusesANonFinalBlock(t *testing.T) {
	t.Parallel()

	armored := readFixture(t, armoredFixture)
	if _, err := readEntities(armored); err != nil {
		t.Fatalf("positive control: readEntities(%s) = %v, want nil", armoredFixture, err)
	}

	// Each leader ends with its own newline, and the block count below is what
	// says so: a leader glued to the key block's opening line would present one
	// block rather than two, and the file would then be measuring nothing.
	for _, tc := range []struct {
		name   string
		want   string
		leader []byte
	}{
		{
			name:   "a signature block ahead of the key block",
			leader: readFixture(t, sigValidArmored),
			want:   "openpgp: invalid argument: expected public or private key block, got: PGP SIGNATURE",
		},
		{
			// An opening line with nothing behind it inside its own cut: the
			// decoder takes the line as a block start, runs out of input looking
			// for the blank line that ends its header section, and reports the
			// block it could not read as io.EOF.
			name:   "an opening line with no block behind it",
			leader: []byte("-----BEGIN NOT REALLY AN ARMOR BLOCK-----\n"),
			want:   "openpgp: invalid argument: no armored data found",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			file := make([]byte, 0, len(tc.leader)+len(armored))
			file = append(file, tc.leader...)
			file = append(file, armored...)
			if got := countArmorBlocks(file); got != 2 {
				t.Fatalf("the fixture presents %d armor blocks, want 2", got)
			}

			entities, err := readEntities(file)
			// Killing mutation, actually run against this file: drop the error
			// arm from readArmoredKeyRing's in-loop readBlock call, so only the
			// block after the loop can fail the read. Both rows then fail, one of
			// them with
			//
			//	keyring_test.go:472: readEntities(a signature block ahead of the key block) = 1 entities, <nil>, want
			//	"openpgp: invalid argument: expected public or private key block, got: PGP SIGNATURE"
			//
			// which is the loader using the half of the file it liked.
			if err == nil || err.Error() != tc.want {
				t.Fatalf("readEntities(%s) = %d entities, %v, want %q", tc.name, len(entities), err, tc.want)
			}
		})
	}
}

// TestLoadKeyringRefusesNonKeyArmorBlock pins the direction reading every block
// errs in: a block that is not key material stops the load and names its type,
// rather than being passed over.
//
// Skipping it would put the loader back in the business of quietly using part
// of a file, which is the defect the multi-block read exists to remove; and
// deciding which non-key types are safe to ignore is a judgement the reader
// behind this one already makes for itself.
func TestLoadKeyringRefusesNonKeyArmorBlock(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	armored, err := os.ReadFile(fixturePath(armoredFixture))
	if err != nil {
		t.Fatalf("read %s: %v", armoredFixture, err)
	}

	controlPath := filepath.Join(dir, "control.asc")
	// #nosec G703 -- dir is this test's own t.TempDir and the leaf is a
	// constant; no part of either path comes from outside this function.
	if writeErr := os.WriteFile(controlPath, armored, 0o600); writeErr != nil {
		t.Fatalf("write control fixture: %v", writeErr)
	}

	mixed := make([]byte, 0, len(armored)+len(signatureArmorBlock))
	mixed = append(mixed, armored...)
	mixed = append(mixed, signatureArmorBlock...)
	mixedPath := filepath.Join(dir, "mixed.asc")
	// #nosec G703 -- same t.TempDir and a constant leaf, as just above.
	if writeErr := os.WriteFile(mixedPath, mixed, 0o600); writeErr != nil {
		t.Fatalf("write mixed fixture: %v", writeErr)
	}

	// The control is the same key block from the same directory, so the
	// refusal below is the appended block being refused, not this directory or
	// this copy of the export.
	if _, ctlErr := LoadKeyring(controlPath); ctlErr != nil {
		t.Fatalf("positive control: LoadKeyring(control.asc) = %v, want nil", ctlErr)
	}

	_, err = LoadKeyring(mixedPath)
	if !errors.Is(err, helpers.ErrKeyringUnreadable) {
		t.Fatalf("LoadKeyring(mixed.asc) error = %v, want the unreadable sentinel", err)
	}
	// Naming the block type is what makes the refusal actionable: the operator
	// learns which appended object the file has to lose.
	if !strings.Contains(err.Error(), "PGP SIGNATURE") {
		t.Fatalf("LoadKeyring(mixed.asc) error does not name the offending block type: %v", err)
	}
}

func TestLoadKeyringRejectsKeybox(t *testing.T) {
	t.Parallel()
	requirePositiveControl(t)

	_, err := LoadKeyring(fixturePath(keyboxFixture))
	if !errors.Is(err, helpers.ErrKeyringIsKeybox) {
		t.Fatalf("LoadKeyring(%s) error = %v, want the keybox sentinel", keyboxFixture, err)
	}
	// Naming the remedy is the entire reason this sentinel is separate from
	// helpers.ErrKeyringUnreadable, so the export command is part of the
	// contract rather than decoration. The assertion guards the sentinel's own
	// wording in internal/galaxy/helpers, which is where an edit could drop it.
	if !strings.Contains(err.Error(), "--export --armor") {
		t.Fatalf("keybox error does not name the export command: %v", err)
	}
}

func TestLoadKeyringRejectsGarbage(t *testing.T) {
	t.Parallel()
	requirePositiveControl(t)

	// One assertion, and the keybox case is covered by it rather than by a
	// second: the keybox branch returns helpers.ErrKeyringIsKeybox alone, so
	// garbage misrouted there fails this very check. A separate "and not a
	// keybox" assertion below it could never be the first line to fail, which
	// would make it documentary rather than pinned.
	_, err := LoadKeyring(fixturePath(garbageFixture))
	if !errors.Is(err, helpers.ErrKeyringUnreadable) {
		t.Fatalf("LoadKeyring(%s) error = %v, want the unreadable sentinel", garbageFixture, err)
	}
}

// TestLoadKeyringRejectsEmptyKeyring pins the one refusal the OpenPGP library
// does not make for itself: openpgp.ReadKeyRing answers an empty packet stream
// with an empty list and a nil error, so a loader that only checks the error
// hands back a keyring that verifies nothing while reporting nothing wrong.
func TestLoadKeyringRejectsEmptyKeyring(t *testing.T) {
	t.Parallel()
	requirePositiveControl(t)

	_, err := LoadKeyring(fixturePath(emptyFixture))
	// Killing mutation, actually run against this file: delete loadKeyring's
	// `if len(entities) == 0` branch. This assertion then fails with
	//
	//	keyring_test.go:579: LoadKeyring(empty.gpg) = nil error, want the unreadable sentinel
	//
	// which is the shape the check exists to prevent - a nil error alongside a
	// keyring holding no keys at all.
	if err == nil {
		t.Fatalf("LoadKeyring(%s) = nil error, want the unreadable sentinel", emptyFixture)
	}
	if !errors.Is(err, helpers.ErrKeyringUnreadable) {
		t.Fatalf("LoadKeyring(%s) error = %v, want the unreadable sentinel", emptyFixture, err)
	}
}

func TestLoadKeyringRejectsMissingFile(t *testing.T) {
	t.Parallel()
	requirePositiveControl(t)

	_, err := LoadKeyring(fixturePath(absentFixture))
	if !errors.Is(err, helpers.ErrKeyringUnreadable) {
		t.Fatalf("LoadKeyring(%s) error = %v, want the unreadable sentinel", absentFixture, err)
	}
	// The open failure is wrapped with %w rather than rendered, so a caller can
	// still tell an absent keyring from an unparseable one without matching on
	// message text.
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("LoadKeyring(%s) error = %v, want fs.ErrNotExist to stay reachable", absentFixture, err)
	}
}

// TestLoadKeyringRefusesFileOverCeiling exercises the size ceiling through
// loadKeyring's limit parameter rather than a 64 MiB fixture.
//
// The padded file is built so that a truncating read would still SUCCEED: the
// filler sits after the armor block's end line, so bytes cut from it leave a
// complete, parseable block behind. That is what makes the refusal load-bearing
// rather than incidental - drop the check and this file is accepted, not
// reported as corrupt.
func TestLoadKeyringRefusesFileOverCeiling(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	armored, err := os.ReadFile(fixturePath(armoredFixture))
	if err != nil {
		t.Fatalf("read %s: %v", armoredFixture, err)
	}
	limit := int64(len(armored)) + ceilingHeadroom

	controlPath := filepath.Join(dir, "control.asc")
	// #nosec G703 -- dir is this test's own t.TempDir and the leaf is a
	// constant; no part of either path comes from outside this function.
	if writeErr := os.WriteFile(controlPath, armored, 0o600); writeErr != nil {
		t.Fatalf("write control fixture: %v", writeErr)
	}

	padded := make([]byte, 0, len(armored)+ceilingFiller)
	padded = append(padded, armored...)
	padded = append(padded, bytes.Repeat([]byte("x"), ceilingFiller)...)
	paddedPath := filepath.Join(dir, "padded.asc")
	// #nosec G703 -- same t.TempDir and a constant leaf, as just above.
	if writeErr := os.WriteFile(paddedPath, padded, 0o600); writeErr != nil {
		t.Fatalf("write padded fixture: %v", writeErr)
	}

	// The control shares the directory and the limit with the refusal below, so
	// a refusal cannot be the limit refusing everything.
	if _, ctlErr := loadKeyring(controlPath, limit); ctlErr != nil {
		t.Fatalf("positive control: loadKeyring at limit %d = %v, want nil", limit, ctlErr)
	}

	_, err = loadKeyring(paddedPath, limit)
	// Killing mutation, actually run against this file: delete loadKeyring's
	// `if int64(len(data)) > limit` branch. This assertion then fails with
	//
	//	keyring_test.go:651: loadKeyring(padded) error = <nil>, want the unreadable sentinel
	//
	// because the truncated read still carries a whole armor block.
	if !errors.Is(err, helpers.ErrKeyringUnreadable) {
		t.Fatalf("loadKeyring(padded) error = %v, want the unreadable sentinel", err)
	}
}

// TestLooksLikeKeybox pins that the magic is identified at its offset rather
// than searched for. Its offset-8 and offset-zero rows are the discriminating
// pair: the same four bytes are a keybox at one offset and not at the other.
func TestLooksLikeKeybox(t *testing.T) {
	t.Parallel()

	keybox, err := os.ReadFile(fixturePath(keyboxFixture))
	if err != nil {
		t.Fatalf("read %s: %v", keyboxFixture, err)
	}
	armored, err := os.ReadFile(fixturePath(armoredFixture))
	if err != nil {
		t.Fatalf("read %s: %v", armoredFixture, err)
	}
	binary, err := os.ReadFile(fixturePath(binaryFixture))
	if err != nil {
		t.Fatalf("read %s: %v", binaryFixture, err)
	}

	tests := []struct {
		name  string
		input []byte
		want  bool
	}{
		{name: "real keybox fixture", input: keybox, want: true},
		{name: "magic at its offset behind arbitrary bytes", input: []byte("01234567KBXf"), want: true},
		{name: "magic at offset zero", input: []byte("KBXf01234567"), want: false},
		{name: "armored export", input: armored, want: false},
		{name: "binary export", input: binary, want: false},
		{name: "shorter than the header prefix", input: []byte("0123456KBX"), want: false},
		{name: "empty", input: nil, want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Killing mutation, actually run against this file: set
			// keyboxMagicOffset to 0. Three rows fail, one of them with
			//
			//	keyring_test.go:700: looksLikeKeybox(real keybox fixture) = false, want true
			//
			// and among them "magic at offset zero" flips to true, which is the
			// pair that shows the offset is what does the identifying.
			if got := looksLikeKeybox(tc.input); got != tc.want {
				t.Fatalf("looksLikeKeybox(%s) = %t, want %t", tc.name, got, tc.want)
			}
		})
	}
}

// gluedBlockCount is how many armor blocks the concatenation case below builds.
// Two would carry the defect, and the third is what separates "the loader read
// the first block" from "the loader read one block per opening line it found":
// with three, a cut fooled once still has to be fooled twice.
const gluedBlockCount = 3

// TestGluedArmorBlocksAreRefused pins the refusal that keeps reading every
// block from being a silent read of the first one.
//
// Measured on blocks armorEncode writes through armor.Encode: the last byte of
// a block is the final "-" of its end line, so concatenating two of them puts
// the second's opening line on the end of the first's last line and the cut
// sees ONE block start in a file carrying three. Measured before this refusal
// existed, such a file loaded as 1 entity with a nil error - a trust set
// silently smaller than the configured one, which is the failure keyringMaxSize's
// own refusal exists to avoid, arriving by a route neither that ceiling nor the
// block loop can see.
//
// Two positive controls, answering two different objections. two-keys.asc is
// the shape an operator really builds - two gpg exports catted, each ending
// with the newline gpg writes - and it still loads as two keys, so the refusal
// is not concatenation. The same three blocks with a newline between them load
// as three, so it is not these blocks or the key they carry either.
func TestGluedArmorBlocksAreRefused(t *testing.T) {
	t.Parallel()

	requireCattedKeyringStillLoads(t)

	glued, separated := gluedArmorBlocks(t)
	// The fixture ahead of its verdict, and the defect itself in one number: the
	// cut finds one opening line in a file that carries three blocks.
	if got := countArmorBlocks(glued); got != 1 {
		t.Fatalf("the glued fixture presents %d armor blocks, want 1: it is not the shape this refusal is about", got)
	}

	dir := t.TempDir()
	kr, err := LoadKeyring(writeKeyringFile(t, dir, "separated.asc", separated))
	if err != nil || kr.Len() != gluedBlockCount {
		t.Fatalf("positive control: LoadKeyring(%d separated blocks) = %v holding %d keys, want nil and %d",
			gluedBlockCount, err, keyringLen(kr), gluedBlockCount)
	}

	kr, err = LoadKeyring(writeKeyringFile(t, dir, "glued.asc", glued))
	// Killing mutation, actually run against this file: delete the
	// checkArmorBlockStartsAtLineStart call from readArmoredKeyRing. This
	// assertion then fails with
	//
	//	keyring_test.go:758: LoadKeyring(3 glued blocks) = 1 keys, <nil>, want the mid-line refusal
	//
	// which is the defect in full: two of the three keys are gone and the load
	// reports nothing wrong.
	if !errors.Is(err, errArmorBlockStartMidLine) {
		t.Fatalf("LoadKeyring(%d glued blocks) = %d keys, %v, want the mid-line refusal",
			gluedBlockCount, keyringLen(kr), err)
	}
	if !errors.Is(err, helpers.ErrKeyringUnreadable) {
		t.Fatalf("LoadKeyring(%d glued blocks) error = %v, want the unreadable sentinel", gluedBlockCount, err)
	}
	// The remedy, which is the whole reason a refusal is worth more here than a
	// smarter cut: the operator has to separate the exports they concatenated.
	if !strings.Contains(err.Error(), "separate concatenated key exports with a newline") {
		t.Fatalf("LoadKeyring(%d glued blocks) error does not name the remedy:\n%v", gluedBlockCount, err)
	}
}

// requireCattedKeyringStillLoads is the first of that test's two controls, and
// the one about the shape rather than about the blocks: two gpg exports catted,
// each ending with the newline gpg writes, still load as two keys.
func requireCattedKeyringStillLoads(t *testing.T) {
	t.Helper()

	kr, err := LoadKeyring(fixturePath(twoKeyFixture))
	if err != nil || kr.Len() != 2 {
		t.Fatalf("positive control: LoadKeyring(%s) = %v holding %d keys, want nil and 2",
			twoKeyFixture, err, keyringLen(kr))
	}
}

// gluedArmorBlocks builds gluedBlockCount copies of one armored key export
// concatenated directly, and the same copies with a newline between them, in
// that order. The two differ by those newlines and by nothing else, which is
// what makes the second the control for the first.
func gluedArmorBlocks(t *testing.T) ([]byte, []byte) {
	t.Helper()

	block := armorEncode(t, openpgp.PublicKeyType, readFixture(t, binaryFixture))
	glued := make([]byte, 0, gluedBlockCount*len(block))
	separated := make([]byte, 0, gluedBlockCount*(len(block)+1))
	for range gluedBlockCount {
		glued = append(glued, block...)
		separated = append(separated, block...)
		separated = append(separated, '\n')
	}

	return glued, separated
}

// writeKeyringFile writes one keyring into dir under leaf and returns its path.
func writeKeyringFile(t *testing.T, dir, leaf string, data []byte) string {
	t.Helper()

	path := filepath.Join(dir, leaf)
	// #nosec G703 -- dir is the caller's own t.TempDir and leaf is a constant at
	// every call site; no part of the path comes from outside this package.
	if err := os.WriteFile(path, data, keyringFileMode); err != nil {
		t.Fatalf("write %s: %v", leaf, err)
	}

	return path
}

// keyringLen reports how many keys a keyring holds, and zero for the nil one a
// refusal returns, so a failure message can name both outcomes in one line.
func keyringLen(kr *Keyring) int {
	if kr == nil {
		return 0
	}

	return kr.Len()
}

// TestEveryCommittedFixtureOpensItsArmorAtALineStart is the acceptance half of
// that refusal, stated over every file testdata holds rather than over the
// armored ones a hand-written list would have named.
//
// The refusal above carries its own controls, so a rule tight enough to turn
// away ordinary armor does not go unnoticed. What those controls cannot state
// is the property over the directory: a fixture committed later is inside it
// with nobody adding it to a list.
//
// Two fixtures are named as well as swept, and
// requireDamagedArmorFixturesOpenAtALineStart holds which two and why.
func TestEveryCommittedFixtureOpensItsArmorAtALineStart(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir(testdataDir)
	if err != nil {
		t.Fatalf("read %s: %v", testdataDir, err)
	}

	carrying := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		data := readFixture(t, name)
		if checkErr := checkArmorBlockStartsAtLineStart(data); checkErr != nil {
			t.Errorf("checkArmorBlockStartsAtLineStart(%s) = %v, want nil", name, checkErr)

			continue
		}
		if bytes.Contains(data, []byte(armorBlockStart)) {
			carrying++
		}
	}

	// Without this the sweep passes just as well against a testdata directory
	// holding no armor at all.
	if carrying == 0 {
		t.Fatalf("no committed fixture carries an armor block start, so the sweep measured nothing")
	}
	requireDamagedArmorFixturesOpenAtALineStart(t)

	// The positive control for the sweep itself: the check does refuse
	// something, so "every fixture passed" is not "the check never fires".
	glued, _ := gluedArmorBlocks(t)
	if checkErr := checkArmorBlockStartsAtLineStart(glued); !errors.Is(checkErr, errArmorBlockStartMidLine) {
		t.Fatalf("positive control: checkArmorBlockStartsAtLineStart(glued blocks) = %v, want the mid-line refusal", checkErr)
	}
	t.Logf("%d of the %d committed fixtures carry an armor block start, every one of them at a line start", carrying, len(entries))
}

// requireDamagedArmorFixturesOpenAtALineStart names the two fixtures the sweep
// above would otherwise cover anonymously.
//
// They are the ones whose armor was deliberately damaged, and so the ones a
// reader would expect this rule to catch. Neither is its shape: sig-a-badarmor.asc
// has its BODY joined into one line with the opening line untouched, and
// sig-a-badbase64.asc has one body character replaced. Both still open their
// armor at the start of a line, and this rule is about nothing else.
func requireDamagedArmorFixturesOpenAtALineStart(t *testing.T) {
	t.Helper()

	for _, name := range []string{sigFoldedArmor, sigBadBase64} {
		data := readFixture(t, name)
		if !bytes.Contains(data, []byte(armorBlockStart)) {
			t.Fatalf("%s carries no armor block start, so it is not the fixture this row is about", name)
		}
		if checkErr := checkArmorBlockStartsAtLineStart(data); checkErr != nil {
			t.Errorf("checkArmorBlockStartsAtLineStart(%s) = %v, want nil: its damage is inside the body", name, checkErr)
		}
	}
}
