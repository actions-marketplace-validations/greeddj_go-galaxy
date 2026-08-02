package s3

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// The path this package exists for - put an artifact and get it back - was
// never walked by a test. Its helpers were covered in isolation
// (verifyArtifactSHA, metaFromHeaders, objectKey) while Commit and Delete sat
// at zero, so nothing proved the digest Commit records is the one Fetch later
// checks against, or that the two ends agree on where an object lives. There
// is no build-tagged integration suite in this repository either, so this was
// not covered somewhere else.
//
// Everything here runs against the fake already in this package. It can
// already do all of it, fault injection included.

// artifactRoundTripKey is the cache key every test in this file commits and
// fetches under.
const artifactRoundTripKey = "abcdef01.acme-widgets-1.0.0.tar.gz"

// artifactRoundTripBody is the payload committed and read back.
const artifactRoundTripBody = "tarball bytes for the round trip"

// openRoundTripBackend returns an opened backend with a temp base of its own,
// so a test can assert on what TempFile leaves behind without seeing files
// from anywhere else.
func openRoundTripBackend(t *testing.T) *Backend {
	t.Helper()

	b := newTestBackend(t)
	if err := b.Open(context.Background()); err != nil {
		t.Fatalf("Open: %v", err)
	}
	b.artifacts.tmpBase = t.TempDir()
	return b
}

// commitRoundTripArtifact stages artifactRoundTripBody through TempFile and
// commits it under artifactRoundTripKey with meta, returning the temp path
// Commit reports.
func commitRoundTripArtifact(t *testing.T, b *Backend, meta map[string]string) string {
	t.Helper()

	ctx := context.Background()
	tmpFile, cleanup, err := b.artifacts.TempFile(ctx, ".download-")
	if err != nil {
		t.Fatalf("TempFile: %v", err)
	}
	defer cleanupIfNeeded(cleanup)
	if _, err := tmpFile.WriteString(artifactRoundTripBody); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	if err := tmpFile.Close(); err != nil {
		t.Fatalf("close temp file: %v", err)
	}

	committed, err := b.artifacts.Commit(ctx, artifactRoundTripKey, tmpFile.Name(), meta)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return committed.Path
}

// TestArtifactRoundTrip walks the whole cycle: stage, commit, read back,
// remove. The read-back is what makes it a round trip rather than four
// unrelated calls - it proves both ends agree on the object key, and that the
// digest Commit recorded is the one Fetch verifies against, since Fetch
// refuses an object whose recorded digest disagrees with its bytes.
func TestArtifactRoundTrip(t *testing.T) {
	t.Parallel()
	b := openRoundTripBackend(t)
	ctx := context.Background()

	commitRoundTripArtifact(t, b, nil)

	found, err := b.artifacts.Has(ctx, artifactRoundTripKey)
	if err != nil || !found {
		t.Fatalf("Has after Commit = (%v, %v), want (true, nil)", found, err)
	}

	fetched, err := b.artifacts.Fetch(ctx, artifactRoundTripKey)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer cleanupIfNeeded(fetched.Cleanup)
	got, err := os.ReadFile(fetched.Path)
	if err != nil {
		t.Fatalf("read fetched artifact: %v", err)
	}
	if string(got) != artifactRoundTripBody {
		t.Errorf("fetched body = %q, want %q", got, artifactRoundTripBody)
	}

	if err := b.artifacts.Delete(ctx, artifactRoundTripKey); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	found, err = b.artifacts.Has(ctx, artifactRoundTripKey)
	if err != nil || found {
		t.Fatalf("Has after Delete = (%v, %v), want (false, nil)", found, err)
	}
}

// TestCommitRecordsTheDigestFetchVerifies pins the agreement the round trip
// above can only imply. Commit is given no digest, so it hashes the payload
// itself; the object is then overwritten with different bytes under that same
// recorded digest, and Fetch must refuse them. If Commit had recorded nothing,
// or recorded it under a name Fetch does not read, the refusal could not
// happen - the corrupted body would come back as if it were fine.
func TestCommitRecordsTheDigestFetchVerifies(t *testing.T) {
	t.Parallel()
	b := openRoundTripBackend(t)
	ctx := context.Background()

	commitRoundTripArtifact(t, b, nil)

	// Positive control first: the object Commit wrote does come back.
	fetched, err := b.artifacts.Fetch(ctx, artifactRoundTripKey)
	if err != nil {
		t.Fatalf("Fetch of the committed object: %v", err)
	}
	cleanupIfNeeded(fetched.Cleanup)

	// Now replace the bytes while keeping the digest Commit derived, which is
	// the shape a rotted or tampered object has.
	sum := sha256.Sum256([]byte(artifactRoundTripBody))
	putArtifactWithRecordedDigest(ctx, t, b, artifactRoundTripKey, []byte("different bytes entirely"), hex.EncodeToString(sum[:]))

	if _, err := b.artifacts.Fetch(ctx, artifactRoundTripKey); !errors.Is(err, helpers.ErrSHA256Mismatch) {
		t.Fatalf("Fetch of drifted bytes = %v, want errors.Is helpers.ErrSHA256Mismatch", err)
	}
}

// TestCommitHonorsASuppliedDigest covers the other half of Commit's digest
// handling: a caller that already hashed the payload - which every real
// download does - has its value recorded rather than replaced by a second
// hash of the same bytes. Fetch reading the object back is what proves the
// supplied value was actually stored and is the one Fetch checks.
func TestCommitHonorsASuppliedDigest(t *testing.T) {
	t.Parallel()
	b := openRoundTripBackend(t)
	ctx := context.Background()

	sum := sha256.Sum256([]byte(artifactRoundTripBody))
	supplied := hex.EncodeToString(sum[:])
	commitRoundTripArtifact(t, b, map[string]string{"sha256": supplied})

	recorded, found, err := b.artifacts.Meta(ctx, artifactRoundTripKey)
	if err != nil || !found {
		t.Fatalf("Meta = (%v, %v, %v), want found", recorded, found, err)
	}
	if recorded["sha256"] != supplied {
		t.Errorf("recorded sha256 = %q, want the supplied %q", recorded["sha256"], supplied)
	}

	fetched, err := b.artifacts.Fetch(ctx, artifactRoundTripKey)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	cleanupIfNeeded(fetched.Cleanup)
}

// TestTempFileLeavesNothingBehind pins the staging half on both outcomes. A
// temp file survives its own creation - a cleanup that ran too eagerly would
// leave Commit nothing to open - and is gone once its cleanup runs, whether
// the commit succeeded or was refused. The refused row uses the fake's own
// fault injection, so the failure is a real PUT rejection rather than a
// simulated one.
func TestTempFileLeavesNothingBehind(t *testing.T) {
	t.Parallel()

	t.Run("after a successful commit", func(t *testing.T) {
		t.Parallel()
		b := openRoundTripBackend(t)
		tmpPath := commitRoundTripArtifact(t, b, nil)
		// Commit reports the temp path and a cleanup of its own; the caller
		// runs it once the bytes are no longer needed.
		if err := os.Remove(tmpPath); err != nil && !os.IsNotExist(err) {
			t.Fatalf("removing the committed temp: %v", err)
		}
		assertNoLeftoverTempFiles(t, b.artifacts.tmpBase)
	})

	t.Run("after a refused commit", func(t *testing.T) {
		t.Parallel()
		b, fake := newTestBackendAndFake(t)
		if err := b.Open(context.Background()); err != nil {
			t.Fatalf("Open: %v", err)
		}
		b.artifacts.tmpBase = t.TempDir()
		ctx := context.Background()

		tmpFile, cleanup, err := b.artifacts.TempFile(ctx, ".download-")
		if err != nil {
			t.Fatalf("TempFile: %v", err)
		}
		if _, err := tmpFile.WriteString(artifactRoundTripBody); err != nil {
			t.Fatalf("write temp file: %v", err)
		}
		if err := tmpFile.Close(); err != nil {
			t.Fatalf("close temp file: %v", err)
		}
		// The file must exist here: it is what Commit is about to open, and a
		// cleanup that already ran would make the refusal below meaningless.
		if _, err := os.Stat(tmpFile.Name()); err != nil {
			t.Fatalf("temp file missing before Commit: %v", err)
		}

		fake.failNext(b.artifacts.objectKey(artifactRoundTripKey), "PUT", 500, -1)
		if _, err := b.artifacts.Commit(ctx, artifactRoundTripKey, tmpFile.Name(), nil); err == nil {
			t.Fatal("expected Commit to fail against a refusing bucket")
		}

		cleanup()
		assertNoLeftoverTempFiles(t, b.artifacts.tmpBase)
	})
}

// TestDeleteOfAnAbsentObjectIsNotAnError pins the shape eviction relies on:
// removing a key the bucket does not hold is the ordinary outcome of a race
// with another runner's own eviction, not a failure to report. The committed
// row is the control, proving Delete against this fake does reach the object
// it names.
func TestDeleteOfAnAbsentObjectIsNotAnError(t *testing.T) {
	t.Parallel()
	b := openRoundTripBackend(t)
	ctx := context.Background()

	if err := b.artifacts.Delete(ctx, "abcdef01.never-committed-1.0.0.tar.gz"); err != nil {
		t.Fatalf("Delete of an absent object: %v", err)
	}

	commitRoundTripArtifact(t, b, nil)
	if err := b.artifacts.Delete(ctx, artifactRoundTripKey); err != nil {
		t.Fatalf("Delete of a committed object: %v", err)
	}
	found, err := b.artifacts.Has(ctx, artifactRoundTripKey)
	if err != nil || found {
		t.Fatalf("Has after Delete = (%v, %v), want (false, nil)", found, err)
	}
}
