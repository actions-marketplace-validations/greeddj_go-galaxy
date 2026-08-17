package collections

// This file exercises the verification step itself - verifyCollectionSignatures
// and the context it runs against - over artifacts and signatures this test
// binary generates for itself.
//
// The key material is ephemeral rather than the committed fixture set in
// internal/galaxy/signature/testdata, and that is a requirement rather than a
// preference: every test here signs the MANIFEST.json of an artifact it just
// built, so the signature, the manifest and the archive's own chain all have to
// agree. A fixed signature over a fixed document could not, since the archive
// would have to be generated to match it.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
	"github.com/greeddj/go-galaxy/internal/galaxy/signature"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/psvmcc/hub/pkg/types"
)

// testSignedCollection is the identity every fixture in this file signs and
// installs. It is a plain acme.app so the sources map key ("acme.app") and the
// collection key ("acme.app@1.0.0") stay legible in a failure message.
//
//nolint:gochecknoglobals // a fixed fixture identity, not mutable shared state.
var testSignedCollection = collection{Namespace: "acme", Name: "app", Version: "1.0.0"}

// testOtherDigest is a well-formed sha256 that is not the digest of anything a
// fixture here carries, so a listing naming it is wrong about content rather
// than malformed.
const testOtherDigest = "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9"

// testSigningEntity generates one ephemeral OpenPGP entity per test binary and
// hands the same one to every fixture.
//
// Ed25519 rather than RSA, and once rather than per test: key generation is the
// only expensive thing in this file. Measured on this machine over
// openpgp.NewEntity, 87.8us an EdDSA key against 103.3ms a 2048-bit RSA one -
// a cost that would otherwise be paid by every row.
//
// sync.OnceValues carries the error rather than panicking inside the
// initializer, so a generation failure is reported by the test that needed the
// key rather than by an unattributed panic during package initialization.
//
//nolint:gochecknoglobals // a lazily built, immutable fixture, not mutable shared state.
var testSigningEntity = sync.OnceValues(func() (*openpgp.Entity, error) {
	return openpgp.NewEntity("go-galaxy fixture", "signature fixture", "fixture@example.invalid",
		&packet.Config{Algorithm: packet.PubKeyAlgoEdDSA})
})

// mustTestSigningEntity returns the shared fixture entity, failing the test if
// it could not be generated.
func mustTestSigningEntity(t *testing.T) *openpgp.Entity {
	t.Helper()
	entity, err := testSigningEntity()
	if err != nil {
		t.Fatalf("generate the fixture signing key: %v", err)
	}
	return entity
}

// writeTestKeyring writes the fixture entity's PUBLIC half as an armored
// keyring and returns its path. Only the public half is exported, which is what
// LoadKeyring accepts: a file carrying secret material is refused by design.
func writeTestKeyring(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	block, err := armor.Encode(&buf, openpgp.PublicKeyType, nil)
	if err != nil {
		t.Fatalf("open the keyring armor block: %v", err)
	}
	if err := mustTestSigningEntity(t).Serialize(block); err != nil {
		t.Fatalf("serialize the fixture public key: %v", err)
	}
	if err := block.Close(); err != nil {
		t.Fatalf("close the keyring armor block: %v", err)
	}

	path := filepath.Join(t.TempDir(), "keyring.asc")
	mustWriteFile(t, path, buf.Bytes())
	return path
}

// signTestBytes returns an armored detached signature over message, made by the
// fixture entity.
func signTestBytes(t *testing.T, message []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := openpgp.ArmoredDetachSign(&buf, mustTestSigningEntity(t), bytes.NewReader(message), nil); err != nil {
		t.Fatalf("sign the fixture manifest: %v", err)
	}
	return buf.Bytes()
}

// writeTestSignature writes a detached signature over message into a fresh
// temp file and returns a file:// URL naming it - the shape a requirements
// file's signatures: entry takes for a local source.
func writeTestSignature(t *testing.T, message []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "collection.asc")
	mustWriteFile(t, path, signTestBytes(t, message))
	return "file://" + path
}

// buildSignedArtifact builds a collection artifact carrying MANIFEST.json,
// FILES.json and one README.md, and returns the path it was written to
// alongside the manifest bytes a signature is made over.
//
// breakChain makes FILES.json name a digest for README.md that the archive's
// own README.md does not have, which is what a manifest chain that does not
// match looks like from the outside: the signed document is intact, the listing
// it vouches for is not.
func buildSignedArtifact(t *testing.T, breakChain bool) (string, []byte) {
	t.Helper()

	return buildSignedArtifactAs(t,
		testSignedCollection.Namespace, testSignedCollection.Name, testSignedCollection.Version, breakChain)
}

// buildSignedArtifactAs is buildSignedArtifact with the identity the manifest
// declares as parameters, so a fixture can build an artifact that is internally
// perfect - signature, chain and all - and about a different collection than
// the one being installed.
func buildSignedArtifactAs(t *testing.T, namespace, name, version string, breakChain bool) (string, []byte) {
	t.Helper()
	readme := []byte("# " + namespace + "." + name + "\n")
	listed := sha256Hex(readme)
	if breakChain {
		listed = testOtherDigest
	}

	filesJSON := []byte(`{"files":[` +
		`{"name":".","ftype":"dir","chksum_type":null,"chksum_sha256":null},` +
		fmt.Sprintf(`{"name":"README.md","ftype":"file","chksum_type":"sha256","chksum_sha256":%q}`, listed) +
		`],"format":1}`)
	manifestJSON := fmt.Appendf(nil,
		`{"format":1,"collection_info":{"namespace":%q,"name":%q,"version":%q},`+
			`"file_manifest_file":{"name":"FILES.json","ftype":"file","chksum_type":"sha256","chksum_sha256":%q}}`,
		namespace, name, version, sha256Hex(filesJSON))

	// A fixed filename rather than one built from the identity: a fixture may
	// declare an identity no filesystem would accept as a name, which is the
	// whole point of the parameters, and nothing here reads the artifact's own
	// file name.
	path := filepath.Join(t.TempDir(), "collection.tar.gz")
	mustWriteFile(t, path, buildTarGz(t, []tarEntry{
		{name: helpers.ManifestFileName, body: manifestJSON},
		{name: helpers.FilesManifestFileName, body: filesJSON},
		{name: "README.md", body: readme},
	}))
	return path, manifestJSON
}

// buildArtifactWithManifest builds a chain-correct artifact whose MANIFEST.json
// is the caller's own collection_info text, closed with the file_manifest_file
// pointer this builder computes. It exists for the fixtures whose defect is the
// SHAPE of that document - a key declared twice, in one spelling or two - which
// no set of identity parameters can express.
//
// collectionInfo is written verbatim and must therefore be an unterminated
// object: everything from the opening brace up to, but not including, the comma
// that precedes the pointer.
func buildArtifactWithManifest(t *testing.T, collectionInfo string) (string, []byte) {
	t.Helper()
	readme := []byte("# fixture\n")
	filesJSON := []byte(`{"files":[` +
		`{"name":".","ftype":"dir","chksum_type":null,"chksum_sha256":null},` +
		fmt.Sprintf(`{"name":"README.md","ftype":"file","chksum_type":"sha256","chksum_sha256":%q}`, sha256Hex(readme)) +
		`],"format":1}`)
	manifestJSON := fmt.Appendf(nil,
		`%s,"file_manifest_file":{"name":"FILES.json","ftype":"file","chksum_type":"sha256","chksum_sha256":%q}}`,
		collectionInfo, sha256Hex(filesJSON))

	path := filepath.Join(t.TempDir(), "collection.tar.gz")
	mustWriteFile(t, path, buildTarGz(t, []tarEntry{
		{name: helpers.ManifestFileName, body: manifestJSON},
		{name: helpers.FilesManifestFileName, body: filesJSON},
		{name: "README.md", body: readme},
	}))

	return path, manifestJSON
}

// tarEntry is one regular file of a fixture archive.
type tarEntry struct {
	name string
	body []byte
}

// buildTarGz writes entries as a gzipped tar, in order. It is
// buildTarGzWithEntry (lock_pin_test.go) widened to several entries, which a
// chain-carrying artifact needs and that one cannot express.
func buildTarGz(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, entry := range entries {
		header := &tar.Header{Typeflag: tar.TypeReg, Name: entry.name, Size: int64(len(entry.body)), Mode: 0o644}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatalf("write tar header for %s: %v", entry.name, err)
		}
		if _, err := tw.Write(entry.body); err != nil {
			t.Fatalf("write tar body for %s: %v", entry.name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip writer: %v", err)
	}
	return buf.Bytes()
}

// verifyFixture is one verification scenario: the deps verifyCollectionSignatures
// is called with, and the printer whose lines it wrote.
type verifyFixture struct {
	deps    installDeps
	printer *capturingPrinter
}

// newVerifyFixture builds installDeps whose verify context is resolved from a
// real config, through the real newVerifyContext, so every test here exercises
// the same construction an install does. keyring may be empty, which is the
// run that verifies nothing.
func newVerifyFixture(t *testing.T, keyring, count string, sources []string) *verifyFixture {
	t.Helper()
	printer := &capturingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)
	cfg := &config.Config{
		Workers: 1,
		Signature: config.SignatureConfig{
			KeyringPath:   keyring,
			RequiredCount: count,
		},
	}
	root := testSignedCollection
	root.Signatures = sources

	verify, err := newVerifyContext(cfg, runtime, []collection{root})
	if err != nil {
		t.Fatalf("newVerifyContext() error = %v, want nil", err)
	}

	return &verifyFixture{
		deps:    newInstallDeps(cfg, runtime, store.New(), nil, nil, nil, nil, verify),
		printer: printer,
	}
}

// serverSignatureMeta builds the version metadata a Galaxy server serves for a
// signed collection: the wire shape is a list of objects carrying the armored
// signature under "signature", which is what serverSignatureBlobs reads.
func serverSignatureMeta(blobs ...[]byte) *types.GalaxyCollectionVersionInfo {
	meta := &types.GalaxyCollectionVersionInfo{Href: "https://galaxy.example/api/v3/collections/acme/app/versions/1.0.0/"}
	list := make([]any, 0, len(blobs))
	for _, blob := range blobs {
		list = append(list, map[string]any{"signature": string(blob)})
	}
	meta.Signatures = list
	return meta
}

// verifyPayload builds the installPayload verifyCollectionSignatures reads: the
// metadata a server's own signatures ride on, and the artifact those signatures
// are checked against. metaUnavailable defaults false here, which is what every
// row but the metadata-unavailable one means.
func verifyPayload(meta *types.GalaxyCollectionVersionInfo, tarPath string) installPayload {
	return installPayload{meta: meta, artifact: artifactData{Path: tarPath}}
}

// TestVerifyDisabledDoesNoWork pins the first statement of
// verifyCollectionSignatures: a run that verifies nothing does not read the
// artifact at all. The fixture is a file that is not an archive, so anything
// that opened it would fail.
//
// The second row is the positive control on the identical fixture: with
// verification on, the same bytes fail, which is what makes the first row's nil
// a decision rather than a fixture nothing could refuse.
//
// KILLING MUTATION, run and reverted: the `if !vc.enabled() { return nil }`
// guard deleted from verifyCollectionSignatures. The first row fails:
//
//	verify_test.go:317: verifyCollectionSignatures() = /var/folders/09/
//	mv8r2msx43l5t38mwljc2xfm0000gn/T/TestVerifyDisabledDoesNoWork4126773194/
//	001/not-an-archive.tar.gz: downloaded artifact is not a gzip-compressed
//	tar archive: gzip: invalid header, want nil
func TestVerifyDisabledDoesNoWork(t *testing.T) {
	t.Parallel()
	tarPath := filepath.Join(t.TempDir(), "not-an-archive.tar.gz")
	mustWriteFile(t, tarPath, []byte("this is not a gzip stream at all"))

	t.Run("verification off", func(t *testing.T) {
		t.Parallel()
		fx := newVerifyFixture(t, "", "1", nil)
		err := verifyCollectionSignatures(context.Background(), fx.deps, testSignedCollection, verifyPayload(nil, tarPath))
		if err != nil {
			t.Fatalf("verifyCollectionSignatures() = %v, want nil", err)
		}
	})

	t.Run("verification on refuses the same bytes", func(t *testing.T) {
		t.Parallel()
		fx := newVerifyFixture(t, writeTestKeyring(t), "1", nil)
		err := verifyCollectionSignatures(context.Background(), fx.deps, testSignedCollection, verifyPayload(nil, tarPath))
		if !errors.Is(err, helpers.ErrArtifactNotTarGz) {
			t.Fatalf("verifyCollectionSignatures() = %v, want errors.Is helpers.ErrArtifactNotTarGz", err)
		}
	})
}

// TestVerifySucceedsWithServerSignature covers the source an ordinary Galaxy
// install has: the server carries the signature in its own version metadata,
// the requirements file names none, and the artifact's chain is intact.
func TestVerifySucceedsWithServerSignature(t *testing.T) {
	t.Parallel()
	tarPath, manifestJSON := buildSignedArtifact(t, false)
	fx := newVerifyFixture(t, writeTestKeyring(t), "1", nil)

	meta := serverSignatureMeta(signTestBytes(t, manifestJSON))
	if err := verifyCollectionSignatures(context.Background(), fx.deps, testSignedCollection, verifyPayload(meta, tarPath)); err != nil {
		t.Fatalf("verifyCollectionSignatures() = %v, want nil", err)
	}
	// The needle is warnVacuousPass' own leading words: a collection something
	// verified must not carry that line at all, and asserting on text no
	// production line contains would pass whatever the code did.
	if fx.printer.hasWarnContaining("Nothing verified") {
		t.Fatalf("a verified collection must not warn about a vacuous pass: %v", fx.printer.warns)
	}
}

// TestVerifySucceedsWithRequirementSignature covers the other source: the
// requirements file names a signature of its own and the server carries none.
// The metadata is nil here, which is also what an offline cache hit hands the
// verifier.
func TestVerifySucceedsWithRequirementSignature(t *testing.T) {
	t.Parallel()
	tarPath, manifestJSON := buildSignedArtifact(t, false)
	fx := newVerifyFixture(t, writeTestKeyring(t), "1", []string{writeTestSignature(t, manifestJSON)})

	if err := verifyCollectionSignatures(context.Background(), fx.deps, testSignedCollection, verifyPayload(nil, tarPath)); err != nil {
		t.Fatalf("verifyCollectionSignatures() = %v, want nil", err)
	}
}

// TestVerifyFailsOnBadSignature pins the verdict: a signature made over other
// bytes does not verify this manifest, so the required count of one is not
// reached and the collection is refused.
//
// The signature is well-formed and made by a key the keyring holds - only the
// document differs - so the refusal is the verification verdict rather than a
// keyring or an armor failure.
func TestVerifyFailsOnBadSignature(t *testing.T) {
	t.Parallel()
	tarPath, _ := buildSignedArtifact(t, false)
	fx := newVerifyFixture(t, writeTestKeyring(t), "1", nil)

	meta := serverSignatureMeta(signTestBytes(t, []byte("a document this artifact does not carry")))
	err := verifyCollectionSignatures(context.Background(), fx.deps, testSignedCollection, verifyPayload(meta, tarPath))
	if !errors.Is(err, helpers.ErrSignatureVerificationFailed) {
		t.Fatalf("verifyCollectionSignatures() = %v, want errors.Is helpers.ErrSignatureVerificationFailed", err)
	}
	if !strings.Contains(err.Error(), testSignedCollection.key()) {
		t.Fatalf("verifyCollectionSignatures() = %v, want it to name %s", err, testSignedCollection.key())
	}
}

// TestVerifyFailsOnChainMismatch pins the second half of the check: the
// signature verifies, so the chain is walked, and the archive does not agree
// with the listing its own signed manifest vouches for.
func TestVerifyFailsOnChainMismatch(t *testing.T) {
	t.Parallel()
	tarPath, manifestJSON := buildSignedArtifact(t, true)
	fx := newVerifyFixture(t, writeTestKeyring(t), "1", nil)

	meta := serverSignatureMeta(signTestBytes(t, manifestJSON))
	err := verifyCollectionSignatures(context.Background(), fx.deps, testSignedCollection, verifyPayload(meta, tarPath))
	if !errors.Is(err, helpers.ErrManifestChainMismatch) {
		t.Fatalf("verifyCollectionSignatures() = %v, want errors.Is helpers.ErrManifestChainMismatch", err)
	}
}

// TestVerifySkipsChainWhenNothingVerified pins an intentional efficiency rule,
// not an oversight: a chain check over a pass that no signature backs is backed
// by nothing, so it is not run at all. The fixture's chain is broken and its
// signature list is empty, and the default count of 1 passes vacuously.
//
// The second row is the positive control on the identical artifact: one good
// signature makes the chain reachable, and that same broken chain then fails.
// Without it, the first row would be indistinguishable from a fixture whose
// chain was fine all along.
func TestVerifySkipsChainWhenNothingVerified(t *testing.T) {
	t.Parallel()
	tarPath, manifestJSON := buildSignedArtifact(t, true)

	t.Run("nothing verified, chain not walked", func(t *testing.T) {
		t.Parallel()
		fx := newVerifyFixture(t, writeTestKeyring(t), "1", nil)
		if err := verifyCollectionSignatures(context.Background(), fx.deps, testSignedCollection, verifyPayload(nil, tarPath)); err != nil {
			t.Fatalf("verifyCollectionSignatures() = %v, want nil", err)
		}
	})

	t.Run("one signature verified, same broken chain refused", func(t *testing.T) {
		t.Parallel()
		fx := newVerifyFixture(t, writeTestKeyring(t), "1", nil)
		meta := serverSignatureMeta(signTestBytes(t, manifestJSON))
		err := verifyCollectionSignatures(context.Background(), fx.deps, testSignedCollection, verifyPayload(meta, tarPath))
		if !errors.Is(err, helpers.ErrManifestChainMismatch) {
			t.Fatalf("verifyCollectionSignatures() = %v, want errors.Is helpers.ErrManifestChainMismatch", err)
		}
	})
}

// TestVacuousPassIsWarned pins the one thing this tool does not inherit from
// ansible-galaxy: the verdict on an empty signature set is a pass, and the
// silence around it is not. The line has to name the collection and the
// spelling that would have required a signature, since those are the two things
// an operator acts on.
//
// KILLING MUTATION, run and reverted: the `if result.VacuousPass` arm deleted
// from verifyCollectionSignatures:
//
//	verify_test.go:453: no vacuous-pass warning naming acme.app@1.0.0; warns=[]
func TestVacuousPassIsWarned(t *testing.T) {
	t.Parallel()
	tarPath, _ := buildSignedArtifact(t, false)
	fx := newVerifyFixture(t, writeTestKeyring(t), "1", nil)

	if err := verifyCollectionSignatures(context.Background(), fx.deps, testSignedCollection, verifyPayload(nil, tarPath)); err != nil {
		t.Fatalf("verifyCollectionSignatures() = %v, want nil", err)
	}
	if !fx.printer.hasWarnContaining(testSignedCollection.key()) {
		t.Fatalf("no vacuous-pass warning naming %s; warns=%v", testSignedCollection.key(), fx.printer.warns)
	}
	if !fx.printer.hasWarnContaining(`"+1"`) {
		t.Fatalf("the vacuous-pass warning does not name the strict spelling; warns=%v", fx.printer.warns)
	}
}

// TestMetadataFailureUnderVerificationCountPolicyDecides states what happens
// when the metadata that would have carried a server's signatures never
// arrived: nothing is gathered, and which verdict that produces is the
// policy's, not this code's.
//
// Both rows run against one artifact and one keyring, differing only in the
// count spec, which is what makes the pair a statement about the policy: the
// default of 1 is satisfied by an empty set and warns, while its strict
// spelling refuses the collection.
func TestMetadataFailureUnderVerificationCountPolicyDecides(t *testing.T) {
	t.Parallel()
	tarPath, _ := buildSignedArtifact(t, false)
	keyring := writeTestKeyring(t)

	t.Run("the default count passes vacuously", func(t *testing.T) {
		t.Parallel()
		fx := newVerifyFixture(t, keyring, "1", nil)
		if err := verifyCollectionSignatures(context.Background(), fx.deps, testSignedCollection, verifyPayload(nil, tarPath)); err != nil {
			t.Fatalf("verifyCollectionSignatures() = %v, want nil", err)
		}
		if !fx.printer.hasWarnContaining(testSignedCollection.key()) {
			t.Fatalf("no vacuous-pass warning naming %s; warns=%v", testSignedCollection.key(), fx.printer.warns)
		}
	})

	t.Run("the strict spelling refuses it", func(t *testing.T) {
		t.Parallel()
		fx := newVerifyFixture(t, keyring, "+1", nil)
		err := verifyCollectionSignatures(context.Background(), fx.deps, testSignedCollection, verifyPayload(nil, tarPath))
		if !errors.Is(err, helpers.ErrSignatureVerificationFailed) {
			t.Fatalf("verifyCollectionSignatures() = %v, want errors.Is helpers.ErrSignatureVerificationFailed", err)
		}
	})
}

// TestServerSignatureListIsCapped pins helpers.MaxSignaturesPerCollection over
// a server-supplied list, the trust boundary that constant exists for: a
// metadata document naming ten thousand signatures must not cost ten thousand
// public-key operations.
//
// The cap is observed through the verdict rather than through a counter: the
// one good signature sits past the cap, so a capped gather never reaches it and
// the count of one goes unsatisfied. The second row is the positive control on
// the same harness - the same good signature, alone - which must verify, so the
// first row's refusal is the cap rather than a blob this fixture cannot check.
func TestServerSignatureListIsCapped(t *testing.T) {
	t.Parallel()
	tarPath, manifestJSON := buildSignedArtifact(t, false)
	good := signTestBytes(t, manifestJSON)
	junk := signTestBytes(t, []byte("a document this artifact does not carry"))

	t.Run("a good signature past the cap is never reached", func(t *testing.T) {
		t.Parallel()
		blobs := make([][]byte, 0, 10_000)
		for range 10_000 {
			blobs = append(blobs, junk)
		}
		blobs = append(blobs, good)

		fx := newVerifyFixture(t, writeTestKeyring(t), "1", nil)
		err := verifyCollectionSignatures(
			context.Background(), fx.deps, testSignedCollection, verifyPayload(serverSignatureMeta(blobs...), tarPath))
		if !errors.Is(err, helpers.ErrSignatureVerificationFailed) {
			t.Fatalf("verifyCollectionSignatures() = %v, want errors.Is helpers.ErrSignatureVerificationFailed", err)
		}
	})

	t.Run("the same signature within the cap verifies", func(t *testing.T) {
		t.Parallel()
		fx := newVerifyFixture(t, writeTestKeyring(t), "1", nil)
		err := verifyCollectionSignatures(
			context.Background(), fx.deps, testSignedCollection, verifyPayload(serverSignatureMeta(good), tarPath))
		if err != nil {
			t.Fatalf("verifyCollectionSignatures() = %v, want nil", err)
		}
	})
}

// TestSignaturesWithoutKeyringIsAHardError pins the refusal a requirements file
// declaring signatures earns when no keyring is configured, and pins it by exit
// class as well as by sentinel: it is a configuration error the operator fixes,
// which is exit 2, never a verification verdict.
//
// The second row is the positive control on the same requirements: adding a
// keyring makes the identical roots resolve, so the refusal is the missing
// keyring rather than the declaration itself.
func TestSignaturesWithoutKeyringIsAHardError(t *testing.T) {
	t.Parallel()
	root := testSignedCollection
	root.Signatures = []string{"https://example.invalid/acme-app.asc"}
	runtime := infra.New(&capturingPrinter{}, http.DefaultClient)

	t.Run("no keyring refuses the run", func(t *testing.T) {
		t.Parallel()
		cfg := &config.Config{Signature: config.SignatureConfig{RequiredCount: "1"}}
		_, err := newVerifyContext(cfg, runtime, []collection{root})
		if !errors.Is(err, helpers.ErrKeyringRequired) {
			t.Fatalf("newVerifyContext() = %v, want errors.Is helpers.ErrKeyringRequired", err)
		}
		if got := exitcode.FromError(err); got != exitcode.ExitUsage {
			t.Fatalf("exit code = %d, want %d (a configuration error, never a verification verdict)", got, exitcode.ExitUsage)
		}
	})

	t.Run("a keyring accepts the same requirements", func(t *testing.T) {
		t.Parallel()
		cfg := &config.Config{Signature: config.SignatureConfig{KeyringPath: writeTestKeyring(t), RequiredCount: "1"}}
		verify, err := newVerifyContext(cfg, runtime, []collection{root})
		if err != nil {
			t.Fatalf("newVerifyContext() error = %v, want nil", err)
		}
		if !verify.enabled() {
			t.Fatal("newVerifyContext() returned a disabled context for a configured keyring")
		}
	})
}

// TestAnsibleSignatureKeysAreWarnedOnce pins requirement (A) at the point it
// fires: one line per run, naming the discovered ansible.cfg and the key names
// it carried, and never a value any of them was set to.
//
// It runs newVerifyContext twice over the same config to state what "once per
// run" rests on: this function is the single emission point, so the count an
// operator sees is the number of times a command builds a verification context,
// which every verifying command does exactly once.
//
// KILLING MUTATION, run and reverted: the AnsibleSignatureKeysWarning arm
// deleted from newVerifyContext:
//
//	verify_test.go:605: warns = [], want exactly one line naming the
//	ansible.cfg signature keys
func TestAnsibleSignatureKeysAreWarnedOnce(t *testing.T) {
	t.Parallel()
	printer := &capturingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)
	cfg := &config.Config{
		AnsibleConfigPath:    "/etc/ansible/ansible.cfg",
		AnsibleSignatureKeys: []string{"gpg_keyring", "disable_gpg_verify"},
		Signature:            config.SignatureConfig{RequiredCount: "1"},
	}

	if _, err := newVerifyContext(cfg, runtime, nil); err != nil {
		t.Fatalf("newVerifyContext() error = %v, want nil", err)
	}
	if len(printer.warns) != 1 {
		t.Fatalf("warns = %v, want exactly one line naming the ansible.cfg signature keys", printer.warns)
	}
	line := printer.warns[0]
	for _, want := range []string{"/etc/ansible/ansible.cfg", "gpg_keyring", "disable_gpg_verify"} {
		if !strings.Contains(line, want) {
			t.Fatalf("warning %q does not name %q", line, want)
		}
	}

	if _, err := newVerifyContext(cfg, runtime, nil); err != nil {
		t.Fatalf("newVerifyContext() error = %v, want nil", err)
	}
	if len(printer.warns) != 2 {
		t.Fatalf("warns = %v, want one line per newVerifyContext call", printer.warns)
	}
}

// TestVerificationDisabledWarnsAboutDeclaredSources pins the other way
// verification can be off while a requirements file asks for it: the operator
// switched it off explicitly. That is not helpers.ErrKeyringRequired - a
// keyring is configured, and the switch is the operator's own - so the run
// proceeds and says so instead.
func TestVerificationDisabledWarnsAboutDeclaredSources(t *testing.T) {
	t.Parallel()
	root := testSignedCollection
	root.Signatures = []string{"https://example.invalid/acme-app.asc"}
	printer := &capturingPrinter{}
	runtime := infra.New(printer, http.DefaultClient)
	cfg := &config.Config{Signature: config.SignatureConfig{
		KeyringPath:      writeTestKeyring(t),
		RequiredCount:    "1",
		DisableGPGVerify: true,
	}}

	verify, err := newVerifyContext(cfg, runtime, []collection{root})
	if err != nil {
		t.Fatalf("newVerifyContext() error = %v, want nil", err)
	}
	if verify.enabled() {
		t.Fatal("newVerifyContext() returned an enabled context while verification is disabled")
	}
	if !printer.hasWarnContaining(requirementKey(root)) {
		t.Fatalf("no warning naming %s; warns=%v", requirementKey(root), printer.warns)
	}
}

// TestServerSignatureBlobsIgnoresForeignShapes pins the shape filter: a server
// entry this tool does not recognize is skipped rather than failing the
// install, since a third-party server may send something else alongside the
// signatures it does carry.
//
// The last row is the positive control - the recognized shape in the same list
// still produces a blob - so "nothing was gathered" is the filter's doing
// rather than the whole list being dropped.
func TestServerSignatureBlobsIgnoresForeignShapes(t *testing.T) {
	t.Parallel()

	oversized := strings.Repeat("x", int(helpers.SignatureMaxSize)+1)
	meta := &types.GalaxyCollectionVersionInfo{Signatures: []any{
		"a bare string, not an object",
		map[string]any{"pubkey_fingerprint": "abc"},
		map[string]any{"signature": 7},
		map[string]any{"signature": ""},
		map[string]any{"signature": oversized},
		map[string]any{"signature": "-----BEGIN PGP SIGNATURE-----\nreal enough\n-----END PGP SIGNATURE-----"},
	}}

	blobs, offered := serverSignatureBlobs(meta)
	if len(blobs) != 1 {
		t.Fatalf("serverSignatureBlobs() returned %d blobs, want 1 (only the recognized shape)", len(blobs))
	}
	// offered counts entries that pass the shape filter, not the raw list
	// length: six entries were offered and only one is shape-valid, so this
	// must read 1, never 6.
	if offered != 1 {
		t.Fatalf("serverSignatureBlobs() offered = %d, want 1 (shape-valid entries, not the raw list length of 6)", offered)
	}
	if !strings.Contains(string(blobs[0].Data), "real enough") {
		t.Fatalf("serverSignatureBlobs() returned %q, want the recognized entry's own bytes", blobs[0].Data)
	}
	if blobs[0].Origin == "" {
		t.Fatal("serverSignatureBlobs() returned a blob with no origin")
	}
	if got, offered := serverSignatureBlobs(nil); got != nil || offered != 0 {
		t.Fatalf("serverSignatureBlobs(nil) = %v, offered %d, want nil, 0", got, offered)
	}
	if got, offered := serverSignatureBlobs(&types.GalaxyCollectionVersionInfo{}); got != nil || offered != 0 {
		t.Fatalf("serverSignatureBlobs(no signatures) = %v, offered %d, want nil, 0", got, offered)
	}
}

// TestVerifyContextSourcesAreDedupedInFileOrder pins what a requirements entry
// contributes: its own sources, in the order the file wrote them, with a
// repeat collapsed - and nothing at all for a collection that declared none.
func TestVerifyContextSourcesAreDedupedInFileOrder(t *testing.T) {
	t.Parallel()
	signed := testSignedCollection
	signed.Signatures = []string{"file:///b.asc", "file:///a.asc", "file:///b.asc"}
	unsigned := collection{Namespace: "acme", Name: "lib", Version: "1.0.0"}

	got := requirementSources([]collection{signed, unsigned})
	want := []string{"file:///b.asc", "file:///a.asc"}
	if fmt.Sprint(got[requirementKey(signed)]) != fmt.Sprint(want) {
		t.Fatalf("sources = %v, want %v", got[requirementKey(signed)], want)
	}
	if _, ok := got[requirementKey(unsigned)]; ok {
		t.Fatalf("sources carries a key for a collection declaring none: %v", got)
	}
}

// TestVacuousPassAdviceCanActuallyPass pins that the spelling the vacuous-pass
// warning advises is one that could satisfy the policy it replaces.
//
// The zero row is the reason this test exists. A count of zero has no strict
// form: measured against signature.Policy.verdict, "+0" is refused by the
// strict clause when nothing verifies and by the equality clause when something
// does, so a run advised to write it would fail under every outcome. "+1" is
// the smallest spelling that requires a signature, and it is what the operator
// who wants the pass closed has to write.
//
// The other two rows are the control that keeps the zero row a statement about
// zero rather than about the advice in general: a real count and "all" are both
// strict-ified by prefixing, and must stay that way.
//
// KILLING MUTATION, run and reverted: the `spec.Count < 1` arm deleted from
// strictSpelling, which is what the function looked like before the zero case
// was noticed:
//
//	verify_test.go:751: strictSpelling(count 0) = "+0", want "+1"
func TestVacuousPassAdviceCanActuallyPass(t *testing.T) {
	t.Parallel()

	rows := []struct {
		name string
		want string
		spec signature.CountSpec
	}{
		{name: "count 0", spec: signature.CountSpec{}, want: "+1"},
		{name: "count 2", spec: signature.CountSpec{Count: 2}, want: "+2"},
		{name: "all", spec: signature.CountSpec{All: true}, want: "+all"},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			if got := strictSpelling(row.spec); got != row.want {
				t.Fatalf("strictSpelling(%s) = %q, want %q", row.name, got, row.want)
			}
		})
	}
}

// TestVacuousPassUnderCountZeroAdvisesAWritableSpelling is the end-to-end half
// of the row above: a run configured with a count of 0 and nothing to gather
// passes vacuously - the shape that reaches the warning - and the line it emits
// names "+1" rather than the "+0" that could never have passed.
func TestVacuousPassUnderCountZeroAdvisesAWritableSpelling(t *testing.T) {
	t.Parallel()
	tarPath, _ := buildSignedArtifact(t, false)
	fx := newVerifyFixture(t, writeTestKeyring(t), "0", nil)

	if err := verifyCollectionSignatures(context.Background(), fx.deps, testSignedCollection, verifyPayload(nil, tarPath)); err != nil {
		t.Fatalf("verifyCollectionSignatures() = %v, want nil", err)
	}
	if !fx.printer.hasWarnContaining(`"+1"`) {
		t.Fatalf("the vacuous-pass warning does not advise a spelling that can pass; warns=%v", fx.printer.warns)
	}
	if fx.printer.hasWarnContaining(`"+0"`) {
		t.Fatalf("the vacuous-pass warning advises %q, which fails under every outcome; warns=%v", "+0", fx.printer.warns)
	}
}

// buildDistinctFileSources writes n temp files with distinct bytes -
// "candidate-<i>" each - and returns their file:// sources, so the sha dedupe
// nextBlob applies never collapses two of them into one gathered candidate.
func buildDistinctFileSources(t *testing.T, n int) []string {
	t.Helper()
	dir := t.TempDir()
	sources := make([]string, n)
	for i := range sources {
		path := filepath.Join(dir, fmt.Sprintf("sig-%d.asc", i))
		mustWriteFile(t, path, fmt.Appendf(nil, "candidate-%d", i))
		sources[i] = "file://" + path
	}

	return sources
}

// buildDistinctServerBlobs returns n server-carried signature blobs, each with
// distinct bytes - "server-blob-<i>" - so a row naming a large offered count
// does not collapse under nextBlob's own sha dedupe: two identical blobs would
// count as one gathered candidate, understating how many the server side
// alone contributed.
func buildDistinctServerBlobs(n int) [][]byte {
	blobs := make([][]byte, n)
	for i := range blobs {
		blobs[i] = fmt.Appendf(nil, "server-blob-%d", i)
	}

	return blobs
}

// drainGather pulls every blob next offers to exhaustion, sorting each one
// into a file-sourced or a server-sourced count by comparing its Origin
// against the known set of file:// sources this test built. That split is
// exactly the two sides of the gather, since gatherOne draws a requirement's
// own sources before a server's.
func drainGather(t *testing.T, next signature.NextBlob, fileSources []string) (int, int) {
	t.Helper()
	known := make(map[string]struct{}, len(fileSources))
	for _, source := range fileSources {
		known[source] = struct{}{}
	}
	fileCount, serverCount := 0, 0
	for {
		blob, ok, err := next()
		if err != nil {
			t.Fatalf("nextBlob() pull error = %v, want nil", err)
		}
		if !ok {
			return fileCount, serverCount
		}
		if _, isFile := known[blob.Origin]; isFile {
			fileCount++
		} else {
			serverCount++
		}
	}
}

// gatherLimitRow is one row of gatherLimitRows: a declared-and-offered pair
// gatherLimit sees, and what its verdict must be.
type gatherLimitRow struct {
	name            string
	wantWarnSubstr  string
	declared        int
	offered         int
	wantWarns       int
	wantFileCount   int
	wantServerCount int
}

// gatherLimitRows is TestGatherWarnsWhenTheCapTruncatesTheCandidateSet's own
// table, split out so the test function itself stays a fixture plus an
// assertion rather than growing with every row.
//
// The first three rows hold the server side at 2 throughout, which is well
// under the cap on its own, so every one of them keeps offered <= cap and the
// pre-truncation count serverSignatureBlobs returns agrees with the
// post-truncation length of the blob slice it also returns. That is exactly
// why a table shaped like those three rows alone could never have caught
// gatherLimit being handed the post-truncation length instead of the true
// offered count: with the server side under the cap the two numbers are
// identical, so a defect that only shows once they diverge is structurally
// unreachable from a fixture that never lets the server side alone cross the
// cap. The four rows after them do exactly that: each pushes offered past 64
// on its own, with declared sources at 0 or 10, so a caller reading the
// post-truncation slice length instead of offered would report 64 candidates
// where the true count is 65 to 210 - which is why every warning's own
// arithmetic, not merely whether one fired, is asserted on every row.
//
//nolint:gochecknoglobals // a fixed table consumed by one test, not mutable shared state.
var gatherLimitRows = []gatherLimitRow{
	{
		name: "64 declared, 2 offered", declared: 64, offered: 2, wantWarns: 1,
		wantWarnSubstr: "66 signature candidates exceed the limit of 64 (64 declared, 2 offered by the server); " +
			"the last 2 were not gathered",
		wantFileCount: 64, wantServerCount: 0,
	},
	{
		name: "63 declared, 2 offered", declared: 63, offered: 2, wantWarns: 1,
		wantWarnSubstr: "65 signature candidates exceed the limit of 64 (63 declared, 2 offered by the server); " +
			"the last 1 were not gathered",
		wantFileCount: 63, wantServerCount: 1,
	},
	// The positive control on the same fixture builder: one candidate
	// under the point where the cap starts truncating, both server blobs
	// are gathered and no warning fires at all - which is what shows the
	// harness above is capable of a silent, complete gather rather than
	// one that just happens never to reach the check.
	{name: "62 declared, 2 offered", declared: 62, offered: 2, wantWarns: 0, wantFileCount: 62, wantServerCount: 2},
	// The four rows below push the SERVER side itself past the cap,
	// which none of the three rows above can: with the server side fixed
	// at 2, offered never crosses the cap on its own.
	{
		name: "0 declared, 200 offered", declared: 0, offered: 200, wantWarns: 1,
		wantWarnSubstr: "200 signature candidates exceed the limit of 64 (0 declared, 200 offered by the server); " +
			"the last 136 were not gathered",
		wantFileCount: 0, wantServerCount: 64,
	},
	{
		name: "0 declared, 65 offered", declared: 0, offered: 65, wantWarns: 1,
		wantWarnSubstr: "the last 1 were not gathered",
		wantFileCount:  0, wantServerCount: 64,
	},
	{
		name: "10 declared, 200 offered", declared: 10, offered: 200, wantWarns: 1,
		wantWarnSubstr: "210 signature candidates exceed the limit of 64 (10 declared, 200 offered by the server); " +
			"the last 146 were not gathered",
		wantFileCount: 10, wantServerCount: 54,
	},
	// The boundary control on the divergent side, the counterpart of the
	// "62 declared" control above: the server alone fills the cap
	// exactly and nothing is dropped, which is what shows the three rows
	// above it are the cap actually truncating rather than a large
	// server side warning unconditionally.
	{name: "0 declared, 64 offered", declared: 0, offered: 64, wantWarns: 0, wantFileCount: 0, wantServerCount: 64},
}

// TestGatherWarnsWhenTheCapTruncatesTheCandidateSet pins gatherLimit's own
// decision: a combined candidate set - a requirements entry's own declared
// sources plus whatever the server offers alongside the artifact - past
// helpers.MaxSignaturesPerCollection is warned about, once, naming what was
// withheld, rather than silently dropped or refused outright. gatherLimitRows'
// own doc comment holds why the table is shaped the way it is.
func TestGatherWarnsWhenTheCapTruncatesTheCandidateSet(t *testing.T) {
	t.Parallel()

	for _, row := range gatherLimitRows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()

			sources := buildDistinctFileSources(t, row.declared)
			fx := newVerifyFixture(t, writeTestKeyring(t), "1", sources)
			meta := serverSignatureMeta(buildDistinctServerBlobs(row.offered)...)

			next := fx.deps.verify.nextBlob(context.Background(), fx.deps.runtime, testSignedCollection, meta)
			fileCount, serverCount := drainGather(t, next, sources)

			if fileCount != row.wantFileCount {
				t.Fatalf("gathered %d file candidates, want %d", fileCount, row.wantFileCount)
			}
			if serverCount != row.wantServerCount {
				t.Fatalf("gathered %d server candidates, want %d", serverCount, row.wantServerCount)
			}
			// KILLING MUTATION, run and reverted: gatherLimit's whole
			// `if dropped := ...; dropped > 0 { ... }` block deleted, so
			// nothing is ever warned about. Every row wanting a warning then
			// fails, one of them with
			//
			//	verify_test.go:1005: warns = [], want exactly 1
			//
			// while the two boundary controls survive untouched, since
			// neither ever expected a warning either.
			//
			// KILLING MUTATION, run and reverted: gatherLimit's predicate
			// narrowed to `sources >= helpers.MaxSignaturesPerCollection`,
			// the premise checkSignatureSources' own doc comment used to
			// state before this function existed. Only the rows whose
			// declared count alone stays under the cap - "63 declared, 2
			// offered", "0 declared, 200 offered", "0 declared, 65 offered"
			// and "10 declared, 200 offered" - then fail, one of them with
			//
			//	verify_test.go:1005: warns = [], want exactly 1
			//
			// while every row already past the threshold on declared sources
			// alone, and both boundary controls, are untouched: it is the
			// rows whose truncation comes from the server side, not the
			// declared one, that tell the correct predicate apart from the
			// one stated above.
			//
			// KILLING MUTATION, run and reverted: gatherLimit's predicate
			// widened to `dropped >= 0`, so it fires even when nothing was
			// actually dropped. Only the two boundary controls then fail,
			// one of them with
			//
			//	verify_test.go:1005: warns = [acme.app@1.0.0: 64 signature candidates exceed the limit of 64 ...], want exactly 0
			//
			// which is what proves them controls: the identical fixture
			// shape the rows around them pass now catches a warning that
			// should never have fired.
			//
			// KILLING MUTATION, run and reverted: serverSignatureBlobs' own
			// second return changed from `offered` to `len(blobs)` - the
			// live defect this table exists to catch, handing gatherLimit a
			// post-truncation count again. The two rows whose server side
			// alone crosses the cap with no declared sources then fail with
			// no warning at all, one of them with
			//
			//	verify_test.go:1005: warns = [], want exactly 1
			//
			// and the "10 declared, 200 offered" row still warns once -
			// dropped is still positive there - but on the wrong arithmetic,
			// failing its own substring check instead:
			//
			//	verify_test.go:1008: warns do not name "210 signature candidates exceed the limit of 64 (10
			//	    declared, 200 offered by the server); the last 146 were not gathered": [acme.app@1.0.0: 74
			//	    signature candidates exceed the limit of 64 (10 declared, 64 offered by the server); the last
			//	    10 were not gathered, and declared sources always come first]
			//
			// while the three rows whose server side never approaches the
			// cap, and the boundary control at exactly 64 offered, are all
			// unaffected: none of them is where offered and len(blobs) ever
			// disagreed. A sibling mutation - offered++ kept but the early
			// break at the cap restored instead of the len(blobs) < cap
			// guard - was checked separately and produces byte-identical
			// failures to this one on every row, since the break stops
			// counting at exactly the same point the guard stops appending;
			// it earns no separate entry here for that reason.
			if len(fx.printer.warns) != row.wantWarns {
				t.Fatalf("warns = %v, want exactly %d", fx.printer.warns, row.wantWarns)
			}
			if row.wantWarnSubstr != "" && !fx.printer.hasWarnContaining(row.wantWarnSubstr) {
				t.Fatalf("warns do not name %q: %v", row.wantWarnSubstr, fx.printer.warns)
			}
		})
	}
}
