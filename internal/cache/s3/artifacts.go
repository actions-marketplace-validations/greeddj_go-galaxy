package s3

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

// Artifacts implements ArtifactStore backed by S3 objects.
type Artifacts struct {
	client  *Client
	prefix  string
	tmpBase string
}

// Meta returns key's cached metadata via a single HEAD request, without ever
// downloading the artifact body - see cacheManager.ArtifactStore's own doc
// comment for the tri-state contract this implements. It shares its HEAD
// with Has via headArtifact, so presence has exactly one implementation on
// this backend, and only Meta itself pays for parsing the response headers.
func (s *Artifacts) Meta(ctx context.Context, key string) (map[string]string, bool, error) {
	headers, found, err := s.headArtifact(ctx, key)
	if err != nil || !found {
		return nil, found, err
	}
	return metaFromHeaders(headers), true, nil
}

// Has reports whether the artifact exists in S3, sharing headArtifact with
// Meta so presence has exactly one implementation on this backend: a caller
// that only needs presence (isCacheHit, the prefetch scan) pays for the same
// one HEAD Meta issues, without also paying for the metadata-header parse
// only Meta's own caller needs - an unconditional map allocation plus a
// strings.ToLower per response header, on a hot path both isCacheHit and the
// prefetch scan run for every collection.
func (s *Artifacts) Has(ctx context.Context, key string) (bool, error) {
	_, found, err := s.headArtifact(ctx, key)
	return found, err
}

// Fetch downloads an artifact from S3 into a temporary file.
func (s *Artifacts) Fetch(ctx context.Context, key string) (cacheManager.ArtifactFile, error) {
	if s.client == nil {
		return cacheManager.ArtifactFile{}, errS3ClientNil
	}
	tmpFile, cleanup, err := s.TempFile(ctx, ".artifact-")
	if err != nil {
		return cacheManager.ArtifactFile{}, err
	}
	meta, sum, err := s.downloadToFile(ctx, key, tmpFile)
	if err != nil {
		_ = tmpFile.Close()
		cleanupIfNeeded(cleanup)
		return cacheManager.ArtifactFile{}, err
	}
	if err := tmpFile.Close(); err != nil {
		cleanupIfNeeded(cleanup)
		return cacheManager.ArtifactFile{}, err
	}
	if err := verifyArtifactSHA(meta, sum); err != nil {
		cleanupIfNeeded(cleanup)
		return cacheManager.ArtifactFile{}, err
	}
	return cacheManager.ArtifactFile{Path: tmpFile.Name(), Cleanup: cleanup, Meta: meta}, nil
}

// verifyArtifactSHA compares a fetched object's real sha256 (computed by this
// process, hence hex.EncodeToString - always lowercase) against the object's
// metadata sidecar. The comparison is exact (==), not strings.EqualFold: a
// case-only difference is a rejection, not a match. Lowercase hex is the
// only shape anything in this program ever writes - hex.EncodeToString at
// every producer, including this package's own Commit and hashReader - and
// the local backend (local.Artifacts.Fetch) requires exactly that shape on
// its own sidecar too, so this exact comparison is what keeps this backend
// from being the only place in the program still willing to accept a second,
// uppercase spelling. That spelling has nowhere to go: helpers.IsSHA256Hex
// gates resolveArtifactSHA, so a digest accepted here but rejected there
// fails the install with helpers.ErrMalformedArtifactSHA256, which is
// deliberately outside the evict-and-refetch class - every run against such
// an object would fail identically, with no recovery. Rejecting it here
// instead is what turns that dead end into the recoverable path described
// below. The exact comparison is also the only place in this file where the
// case distinction matters: an uppercase actual can never happen (this
// process's own hex.EncodeToString), so the entire discriminating power is on
// expected, the metadata read back from the object.
//
// The rejection is deliberately still classified as helpers.ErrSHA256Mismatch,
// not helpers.ErrMalformedArtifactSHA256: prepareWithRecovery's
// prepareInstall-error arm retries exactly on ErrSHA256Mismatch, so a
// case-only mismatch evicts the object and refetches it, and the refetch
// re-commits canonical lowercase metadata via this package's own Commit -
// clearing the condition in one online run instead of failing forever.
func verifyArtifactSHA(meta map[string]string, sum []byte) error {
	if meta == nil {
		return nil
	}
	expected := strings.TrimSpace(meta["sha256"])
	if expected == "" {
		return nil
	}
	actual := hex.EncodeToString(sum)
	if actual == expected {
		return nil
	}
	// Wrap both the package-local sentinel (kept for any existing callers
	// that already match on it) and helpers.ErrSHA256Mismatch, so the
	// collections layer can classify this as a recoverable cache-integrity
	// failure without importing s3-specific error types.
	return fmt.Errorf("%w: %w: %s != %s", errArtifactSHA256Mismatch, helpers.ErrSHA256Mismatch, actual, expected)
}

func cleanupIfNeeded(cleanup func()) {
	if cleanup != nil {
		cleanup()
	}
}

// TempFile creates a temporary file for staging an artifact.
func (s *Artifacts) TempFile(_ context.Context, prefix string) (*os.File, func(), error) {
	base := strings.TrimSpace(s.tmpBase)
	if base == "" {
		base = os.TempDir()
	}
	tmpFile, err := os.CreateTemp(base, prefix)
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() {
		_ = os.Remove(tmpFile.Name())
	}
	return tmpFile, cleanup, nil
}

// Commit uploads a temporary artifact to S3 and returns its file reference.
func (s *Artifacts) Commit(ctx context.Context, key, tmpPath string, meta map[string]string) (cacheManager.ArtifactFile, error) {
	if s.client == nil {
		return cacheManager.ArtifactFile{}, errS3ClientNil
	}
	//nolint:gosec // tmpPath is created by this process and is trusted.
	file, err := os.Open(tmpPath)
	if err != nil {
		return cacheManager.ArtifactFile{}, err
	}
	defer func() {
		_ = file.Close()
	}()
	info, err := file.Stat()
	if err != nil {
		return cacheManager.ArtifactFile{}, err
	}
	if meta == nil {
		meta = make(map[string]string)
	}
	payloadHash := strings.TrimSpace(meta["sha256"])
	if payloadHash == "" {
		hash, err := hashReader(file)
		if err != nil {
			return cacheManager.ArtifactFile{}, err
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return cacheManager.ArtifactFile{}, err
		}
		payloadHash = hash
		meta["sha256"] = hash
	}
	if err := s.client.putObject(ctx, s.objectKey(key), file, info.Size(), "application/gzip", "", meta, false, payloadHash); err != nil {
		return cacheManager.ArtifactFile{}, err
	}
	cleanup := func() {
		_ = os.Remove(tmpPath)
	}
	return cacheManager.ArtifactFile{Path: tmpPath, Cleanup: cleanup, Meta: meta}, nil
}

// Delete removes an artifact from S3.
func (s *Artifacts) Delete(ctx context.Context, key string) error {
	if s.client == nil {
		return errS3ClientNil
	}
	return s.client.deleteObject(ctx, s.objectKey(key))
}

// headArtifact issues the single HEAD both Has and Meta are built on, and is
// the one place this backend decides what "present" means.
func (s *Artifacts) headArtifact(ctx context.Context, key string) (map[string][]string, bool, error) {
	if s.client == nil {
		return nil, false, errS3ClientNil
	}
	headers, err := s.client.headObject(ctx, s.objectKey(key))
	if err == nil {
		return headers, true, nil
	}
	if errors.Is(err, errS3NotFound) {
		return nil, false, nil
	}
	return nil, false, err
}

// objectKey builds a full S3 object key for an artifact key.
func (s *Artifacts) objectKey(key string) string {
	trimmed := strings.TrimLeft(key, "/")
	return path.Join(s.prefix, trimmed)
}

// metaFromHeaders extracts user metadata from S3 response headers.
func metaFromHeaders(headers map[string][]string) map[string]string {
	meta := make(map[string]string)
	for name, values := range headers {
		lower := strings.ToLower(name)
		if !strings.HasPrefix(lower, "x-amz-meta-") {
			continue
		}
		if len(values) == 0 {
			continue
		}
		key := strings.TrimPrefix(lower, "x-amz-meta-")
		meta[key] = strings.TrimSpace(values[0])
	}
	if len(meta) == 0 {
		return nil
	}
	return meta
}

func (s *Artifacts) downloadToFile(ctx context.Context, key string, file *os.File) (map[string]string, []byte, error) {
	resp, err := s.client.getObject(ctx, s.objectKey(key))
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	hasher := sha256.New()
	writer := io.MultiWriter(file, hasher)
	limited := helpers.NewSizeLimitedReader(resp.Body, helpers.ArtifactMaxDownloadSize)
	if _, err := io.Copy(writer, limited); err != nil {
		// helpers.ErrResponseTooLarge is a bare size ceiling shared with three
		// other capped surfaces, so it is wrapped here naming this one. Without
		// the wrap this is the only capped body that does not identify itself,
		// leaving an operator to infer it from the absence of the other labels -
		// which fails exactly where it matters, since a metadata re-resolution
		// inside an install worker prints under the same per-collection line.
		// Deliberately uncovered: ArtifactMaxDownloadSize is 4 GiB, so tripping
		// the ceiling end to end is impractical rather than merely inconvenient.
		return nil, nil, fmt.Errorf("cached artifact object: %w", err)
	}
	return metaFromHeaders(resp.Header), hasher.Sum(nil), nil
}
