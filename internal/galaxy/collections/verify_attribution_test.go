package collections

// This file pins the binding between a verified signature and the collection it
// was verified FOR, plus the two message bounds that keep an attacker-chosen
// string out of an operator's terminal at scale.
//
// Every artifact here is internally perfect - the signature verifies, the chain
// walks, the bytes hash - and differs from a legitimate one in exactly one way:
// the identity its signed manifest declares. That is the whole point. A
// verification step that never compares those three fields accepts all of them.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/exitcode"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/psvmcc/hub/pkg/types"
)

// TestSignedManifestMustNameTheCollection is the load-bearing proof that a
// signature is bound to what it vouches for.
//
// The first two rows are the two shapes measured before the check existed, both
// of which installed with a nil error: a signed artifact for an entirely
// different collection, and a signed OLDER version of the right one - a signed
// downgrade, which is the shape with a CVE behind it, recorded as the resolved
// version in the store, the lockfile and GALAXY.yml.
//
// The third row is the positive control on the identical fixture builder: the
// same signing key, the same chain, the same policy, with the manifest naming
// the collection actually being installed, must install. Without it, the two
// refusals would be indistinguishable from a fixture nothing accepts.
//
// KILLING MUTATION, run and reverted: the checkManifestAttribution call deleted
// from verifyCollectionSignatures. Both refusal rows fail; the first reads:
//
//	verify_attribution_test.go:73: verifyCollectionSignatures() = <nil>, want errors.Is helpers.ErrSignatureAttributionMismatch
func TestSignedManifestMustNameTheCollection(t *testing.T) {
	t.Parallel()

	rows := []struct {
		name      string
		namespace string
		collName  string
		version   string
		wantErr   bool
	}{
		{name: "another collection entirely", namespace: "trusted", collName: "lib", version: "2.0.0", wantErr: true},
		{name: "a signed downgrade of the same collection", namespace: "acme", collName: "app", version: "0.0.1", wantErr: true},
		{name: "the collection being installed", namespace: "acme", collName: "app", version: "1.0.0"},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			tarPath, manifestJSON := buildSignedArtifactAs(t, row.namespace, row.collName, row.version, false)
			fx := newVerifyFixture(t, writeTestKeyring(t), "1", nil)
			meta := serverSignatureMeta(signTestBytes(t, manifestJSON))

			err := verifyCollectionSignatures(
				context.Background(), fx.deps, testSignedCollection, verifyPayload(meta, tarPath))
			if !row.wantErr {
				if err != nil {
					t.Fatalf("verifyCollectionSignatures() = %v, want nil", err)
				}

				return
			}
			if !errors.Is(err, helpers.ErrSignatureAttributionMismatch) {
				t.Fatalf("verifyCollectionSignatures() = %v, want errors.Is helpers.ErrSignatureAttributionMismatch", err)
			}
			if got := exitcode.FromError(err); got != exitcode.ExitSignature {
				t.Fatalf("exit code = %d, want %d", got, exitcode.ExitSignature)
			}
			if want := row.namespace + "." + row.collName + "@" + row.version; !strings.Contains(err.Error(), want) {
				t.Fatalf("verifyCollectionSignatures() = %v, want it to name what the manifest vouches for (%s)", err, want)
			}
		})
	}
}

// TestAttributionIsNotCheckedWithoutAVerifiedSignature states the placement of
// the check as a decision rather than an accident: it runs inside the arm where
// at least one signature verified, so a run that verified nothing does not
// consult it.
//
// The reason is that the comparison is worth exactly what the signature is
// worth. On an unsigned manifest an attacker chooses both sides of it - the
// document declaring the identity and the artifact carrying the document - so
// refusing there would manufacture assurance rather than establish any, and
// would additionally fail an ordinary unsigned install for a mismatch nobody
// vouched for either way.
//
// The fixture is the same mis-attributed artifact the rows above refuse, handed
// over with nothing to gather: the default count passes vacuously, and the
// mismatch goes unremarked.
func TestAttributionIsNotCheckedWithoutAVerifiedSignature(t *testing.T) {
	t.Parallel()
	tarPath, _ := buildSignedArtifactAs(t, "trusted", "lib", "2.0.0", false)
	fx := newVerifyFixture(t, writeTestKeyring(t), "1", nil)

	if err := verifyCollectionSignatures(
		context.Background(), fx.deps, testSignedCollection, verifyPayload(nil, tarPath)); err != nil {
		t.Fatalf("verifyCollectionSignatures() = %v, want nil", err)
	}
}

// TestAttributionMessageIsBounded pins the ceiling on the one message that
// renders a manifest-declared value: those three strings come out of an
// archive-chosen document bounded only by helpers.ManifestScanMaxBytes, so
// without a cap a mis-attributed artifact chooses how many bytes reach an
// operator's terminal.
//
// The assertion is on both halves of what helpers.TruncateForMessage promises: the
// message stays small, and it says the value was cut rather than shortening it
// silently.
func TestAttributionMessageIsBounded(t *testing.T) {
	t.Parallel()
	huge := strings.Repeat("n", 64<<10)
	tarPath, manifestJSON := buildSignedArtifactAs(t, huge, "lib", "2.0.0", false)
	fx := newVerifyFixture(t, writeTestKeyring(t), "1", nil)
	meta := serverSignatureMeta(signTestBytes(t, manifestJSON))

	err := verifyCollectionSignatures(context.Background(), fx.deps, testSignedCollection, verifyPayload(meta, tarPath))
	if !errors.Is(err, helpers.ErrSignatureAttributionMismatch) {
		t.Fatalf("verifyCollectionSignatures() = %v, want errors.Is helpers.ErrSignatureAttributionMismatch", err)
	}
	if len(err.Error()) > 2*helpers.MessageValueMaxLen {
		t.Fatalf("the refusal renders %d bytes for a %d-byte declared namespace, want it bounded", len(err.Error()), len(huge))
	}
	if !strings.Contains(err.Error(), "bytes)") {
		t.Fatalf("the refusal = %q, want it to say the value was truncated", err.Error())
	}
}

// TestServerBlobOriginIsBounded pins the same ceiling on the other
// attacker-influenced string this file guards: a version-metadata href, which
// is copied onto every blob gathered for a collection and rendered once per
// non-ignored failure by signature.verificationError.
//
// The href here is small next to the 8 MiB the real amplification was measured
// on - what matters is crossing the ceiling, and a fixture that has to allocate
// megabytes to prove a bound of 512 bytes buys nothing.
//
// The second half is the positive control: an ordinary href passes through
// untouched, so the cap is a cap rather than a mangling of every origin.
func TestServerBlobOriginIsBounded(t *testing.T) {
	t.Parallel()

	long := &types.GalaxyCollectionVersionInfo{Href: "https://sigs.example/" + strings.Repeat("p", 64<<10)}
	long.Signatures = []any{map[string]any{"signature": "-----BEGIN PGP SIGNATURE-----\nx\n-----END PGP SIGNATURE-----"}}
	blobs := serverSignatureBlobs(long)
	if len(blobs) != 1 {
		t.Fatalf("serverSignatureBlobs() returned %d blobs, want 1", len(blobs))
	}
	if got := len(blobs[0].Origin); got > 2*helpers.MessageValueMaxLen {
		t.Fatalf("blob origin is %d bytes for a %d-byte href, want it bounded", got, len(long.Href))
	}
	if !strings.Contains(blobs[0].Origin, "bytes)") {
		t.Fatalf("blob origin = %q, want it to say the value was truncated", blobs[0].Origin)
	}

	href := "https://galaxy.example/api/v3/collections/acme/app/versions/1.0.0/"
	short := &types.GalaxyCollectionVersionInfo{Href: href}
	short.Signatures = long.Signatures
	if got := serverSignatureBlobs(short); len(got) != 1 || got[0].Origin != href {
		t.Fatalf("serverSignatureBlobs() origin = %v, want the href unchanged", got)
	}
}

// TestMetadataUnavailableIsADifferentLine pins the distinction the operator
// acts on: "this collection carries no signatures" and "this run could not
// learn whether it does" are two different facts, and a vacuous pass reports
// which one it was.
//
// Both rows gather nothing and both pass, so the verdict is identical and only
// the line differs - which is exactly why the fact has to be carried on the
// payload rather than inferred from meta being nil.
func TestMetadataUnavailableIsADifferentLine(t *testing.T) {
	t.Parallel()
	tarPath, _ := buildSignedArtifact(t, false)

	t.Run("metadata was in hand and carried no signatures", func(t *testing.T) {
		t.Parallel()
		fx := newVerifyFixture(t, writeTestKeyring(t), "1", nil)
		if err := verifyCollectionSignatures(
			context.Background(), fx.deps, testSignedCollection, verifyPayload(nil, tarPath)); err != nil {
			t.Fatalf("verifyCollectionSignatures() = %v, want nil", err)
		}
		if fx.printer.hasWarnContaining("version metadata was unavailable") {
			t.Fatalf("an ordinary vacuous pass must not blame the metadata; warns=%v", fx.printer.warns)
		}
	})

	t.Run("metadata could not be loaded at all", func(t *testing.T) {
		t.Parallel()
		fx := newVerifyFixture(t, writeTestKeyring(t), "1", nil)
		payload := verifyPayload(nil, tarPath)
		payload.metaUnavailable = true

		if err := verifyCollectionSignatures(context.Background(), fx.deps, testSignedCollection, payload); err != nil {
			t.Fatalf("verifyCollectionSignatures() = %v, want nil", err)
		}
		if !fx.printer.hasWarnContaining("version metadata was unavailable") {
			t.Fatalf("no warning naming the unavailable metadata; warns=%v", fx.printer.warns)
		}
	})
}

// foldingBypass is one manifest whose collection_info this tool must refuse to
// read a single identity out of.
type foldingBypass struct {
	name     string
	manifest string
}

// foldingBypassShapes enumerates the ways a document can declare its identity
// more than once. Each was measured accepted, with a nil error, against a
// struct decode of the same document - which is what readIdentityObject
// replaced.
//
// The last row is the one a top-level-only fix leaves open: encoding/json folds
// an inner field name exactly as readily as an outer one.
//
//nolint:gochecknoglobals // a fixed table consumed by one test, not mutable shared state.
var foldingBypassShapes = []foldingBypass{
	{
		name: "a folded top-level shadow",
		manifest: `{"collection_info":{"namespace":"evil","name":"lib","version":"6.6.6"},` +
			`"COLLECTION_INFO":{"namespace":"acme","name":"app","version":"1.0.0"}`,
	},
	{
		name: "an identity assembled across three spellings",
		manifest: `{"collection_info":{"namespace":"evil","name":"lib","version":"6.6.6"},` +
			`"CoLlEcTiOn_InFo":{"namespace":"acme"},` +
			`"COLLECTION_INFO":{"name":"app","version":"1.0.0"}`,
	},
	{
		name: "two exact duplicate keys",
		manifest: `{"collection_info":{"namespace":"evil","name":"lib","version":"6.6.6"},` +
			`"collection_info":{"namespace":"acme","name":"app","version":"1.0.0"}`,
	},
	{
		name:     "a folded shadow one level down",
		manifest: `{"collection_info":{"namespace":"evil","NAMESPACE":"acme","name":"app","version":"1.0.0"}`,
	},
	{
		// The row that proves the refusal is the DECODE rather than the
		// comparison: every value here matches the collection being installed,
		// so a reader that resolved the duplicate any way at all would accept
		// it. What is wrong with the document is that it names its identity
		// twice, which makes that identity a function of the parser.
		name: "a folded shadow whose values all match",
		manifest: `{"collection_info":{"namespace":"acme","name":"app","version":"1.0.0"},` +
			`"COLLECTION_INFO":{"namespace":"acme","name":"app","version":"1.0.0"}`,
	},
}

// TestFoldedIdentityKeysAreRefused pins the decode half of the attribution
// check: a manifest this tool cannot read ONE identity out of has failed to
// attribute itself, whatever the values involved say.
//
// Every fixture here is otherwise perfect - signed by a key the keyring holds,
// with a chain that walks - so the only thing under test is how the identity is
// read. TestSignedManifestMustNameTheCollection's own accepting row is the
// positive control that the same builder produces documents this check accepts.
//
// KILLING MUTATION, run and reverted: checkManifestAttribution's
// readIdentityObject/readIdentityField calls replaced by the struct decode they
// were written to replace (a json.Unmarshal into a collection_info struct with
// three string fields). Four of the five rows fail; the first reads:
//
//	verify_attribution_test.go:290: verifyCollectionSignatures() = <nil>, want errors.Is helpers.ErrSignatureAttributionMismatch
func TestFoldedIdentityKeysAreRefused(t *testing.T) {
	t.Parallel()

	for _, shape := range foldingBypassShapes {
		t.Run(shape.name, func(t *testing.T) {
			t.Parallel()
			tarPath, manifestJSON := buildArtifactWithManifest(t, shape.manifest)
			fx := newVerifyFixture(t, writeTestKeyring(t), "1", nil)
			meta := serverSignatureMeta(signTestBytes(t, manifestJSON))

			err := verifyCollectionSignatures(
				context.Background(), fx.deps, testSignedCollection, verifyPayload(meta, tarPath))
			if !errors.Is(err, helpers.ErrSignatureAttributionMismatch) {
				t.Fatalf("verifyCollectionSignatures() = %v, want errors.Is helpers.ErrSignatureAttributionMismatch", err)
			}
		})
	}
}

// TestAttributionMessageQuotesTheDeclaredIdentity pins the %q rendering: a
// manifest choosing its own version string chooses bytes that reach an
// operator's terminal, and internal/safeout deliberately passes a newline
// through. Quoted, it renders as an escape and cannot forge a line of its own.
func TestAttributionMessageQuotesTheDeclaredIdentity(t *testing.T) {
	t.Parallel()
	tarPath, manifestJSON := buildSignedArtifactAs(t, "acme", "app", "9.9.9\nSuccessfully installed acme.app@1.0.0", false)
	fx := newVerifyFixture(t, writeTestKeyring(t), "1", nil)
	meta := serverSignatureMeta(signTestBytes(t, manifestJSON))

	err := verifyCollectionSignatures(context.Background(), fx.deps, testSignedCollection, verifyPayload(meta, tarPath))
	if !errors.Is(err, helpers.ErrSignatureAttributionMismatch) {
		t.Fatalf("verifyCollectionSignatures() = %v, want errors.Is helpers.ErrSignatureAttributionMismatch", err)
	}
	if strings.Contains(err.Error(), "\n") {
		t.Fatalf("the refusal carries a real newline from the manifest: %q", err.Error())
	}
	if !strings.Contains(err.Error(), `\n`) {
		t.Fatalf("the refusal = %q, want the declared version quoted", err.Error())
	}
}
