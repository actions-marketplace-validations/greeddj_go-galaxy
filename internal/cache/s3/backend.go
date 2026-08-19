// Package s3 implements the cache backend backed by an S3-compatible object
// store, together with the minimal SigV4-signing HTTP client it speaks to one
// through - there is no AWS SDK here. Snapshot state and the project registry
// are each a single gzipped-JSON object, artifacts are objects under their own
// key prefix, and exclusive access is a distributed lock built on conditional
// writes plus a heartbeat that renews the holder's TTL and cancels the holder
// context once another acquirer's token appears. Open refuses a store that
// does not actually enforce those conditional writes, since the lock's whole
// mutual-exclusion guarantee rests on them.
//
// Which of this package's errors carries which cache-backend failure class is
// settled by the partition documented in variables.go, not per call site.
package s3

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/store"
	"github.com/greeddj/go-galaxy/internal/gzipstream"
	"github.com/klauspost/pgzip"
)

// Backend provides an S3-backed cache backend.
type Backend struct {
	client     *Client
	httpClient *http.Client
	artifacts  *Artifacts
	prefix     string
	tempDir    string
	cfg        config.S3CacheConfig
	lock       lockTiming
}

// New creates an S3-backed cache backend for the given config.
func New(cfg config.S3CacheConfig, httpClient *http.Client, tempDir string) (*Backend, error) {
	if cfg.Bucket == "" {
		return nil, errS3BucketEmpty
	}
	if httpClient == nil {
		return nil, errS3HTTPClientNil
	}
	if tempDir == "" {
		tempDir = os.TempDir()
	}
	return &Backend{
		cfg:        cfg,
		httpClient: httpClient,
		prefix:     strings.Trim(cfg.Prefix, "/"),
		tempDir:    tempDir,
		lock: lockTiming{
			ttl:                lockTTL,
			heartbeatInterval:  heartbeatInterval,
			heartbeatOpTimeout: heartbeatOpTimeout,
			releaseTimeout:     lockReleaseTimeout,
			waitCeiling:        lockWaitCeiling,
			backoffBase:        lockBackoffBase,
			backoffCap:         lockBackoffCap,
		},
	}, nil
}

// Open initializes the S3 client, ensures the bucket exists, and verifies
// once that the backend actually enforces conditional PUT (If-None-Match) -
// the distributed lock's whole mutual-exclusion guarantee rests on that
// being true, so a backend that silently ignores it and overwrites must
// fail loudly here rather than let two processes both believe they hold
// the lock later.
func (b *Backend) Open(ctx context.Context) error {
	if b.client != nil {
		return nil
	}
	client, err := newClient(b.cfg, b.httpClient)
	if err != nil {
		return err
	}
	b.client = client
	if err := b.client.ensureBucket(ctx); err != nil {
		b.client = nil
		return err
	}
	if err := b.probeConditionalPut(ctx); err != nil {
		b.client = nil
		return err
	}
	b.artifacts = &Artifacts{
		client:  client,
		prefix:  b.key(artifactsPrefix),
		tmpBase: b.tempDir,
	}
	return nil
}

// Close releases backend resources.
func (b *Backend) Close(_ context.Context) error {
	return nil
}

// Lock acquires an S3-based distributed lock. The holder context it returns
// alongside the release closure is canceled, with a cause matching
// helpers.ErrCacheLockLost, as soon as the heartbeat sees another acquirer's
// token on the lock object - see acquireLock and the cacheManager.Backend
// interface's own contract for Lock.
func (b *Backend) Lock(ctx context.Context) (context.Context, func() error, error) {
	if err := b.Open(ctx); err != nil {
		return nil, nil, err
	}
	lockKey := b.key(locksPrefix, lockObject)
	return b.acquireLock(ctx, lockKey)
}

// LoadStore loads the snapshot store from S3. It applies the same schema
// policy as the local backend's Load: a newer-than-current schema version
// is reported as an error since this binary cannot safely interpret it, an
// older-than-current version causes the snapshot to be dropped and rebuilt
// (a fresh empty Store, nil error) rather than partially trusted, and a
// matching version returns the loaded data as-is. The schema version is
// checked via a lightweight probe decode of just the meta object before the
// full payload is unmarshaled into a *store.Store: an outdated schema's data
// buckets can have a shape the current Store type can no longer decode (as
// happened across the v3 -> v4 bump), so validating first means that case is
// dropped and rebuilt like the local backend, instead of failing the full
// unmarshal.
func (b *Backend) LoadStore(ctx context.Context) (*store.Store, error) {
	if err := b.Open(ctx); err != nil {
		return nil, err
	}
	key := b.key(statePrefix, storeObject)
	data, err := b.readObject(ctx, key)
	if err != nil {
		if errors.Is(err, errS3NotFound) {
			return store.New(), nil
		}
		return nil, err
	}

	var probe struct {
		Meta store.SnapshotMeta `json:"meta"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, err
	}
	switch verr := store.ValidateSchema(probe.Meta.SchemaVersion); {
	case errors.Is(verr, helpers.ErrOutdatedSchemaVersion):
		return store.New(), nil
	case verr != nil:
		return nil, verr
	}

	st := store.New()
	if err := json.Unmarshal(data, st); err != nil {
		return nil, err
	}
	return st, nil
}

// SaveStore persists the snapshot store to S3. It marshals via
// Store.MarshalSnapshot rather than json.Marshal directly, since st may
// still be concurrently mutated by other goroutines: MarshalSnapshot takes
// the store's RLock and deep-copies before encoding, so this never races on
// the live maps.
func (b *Backend) SaveStore(ctx context.Context, st *store.Store) error {
	if st == nil {
		return nil
	}
	if err := b.Open(ctx); err != nil {
		return err
	}
	payload, err := st.MarshalSnapshot()
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	zw := pgzip.NewWriter(&buf)
	if _, err := zw.Write(payload); err != nil {
		_ = zw.Close()
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	key := b.key(statePrefix, storeObject)
	reader := bytes.NewReader(buf.Bytes())
	return b.client.putObject(ctx, key, reader, int64(buf.Len()),
		putObjectAttrs{contentType: "application/json", contentEncoding: "gzip"}, putCondition{})
}

// ClearFiles removes cached artifacts from S3, batching the deletes via
// DeleteObjects (one batch per list page) rather than issuing one DELETE per
// object.
func (b *Backend) ClearFiles(ctx context.Context) error {
	if err := b.Open(ctx); err != nil {
		return err
	}
	return b.client.deleteAllUnderPrefix(ctx, b.key(artifactsPrefix))
}

// RecordProject records the project metadata in S3.
func (b *Backend) RecordProject(ctx context.Context, requirementsFile, downloadPath string) error {
	if err := b.Open(ctx); err != nil {
		return err
	}
	registry, err := b.LoadProjectRegistry(ctx)
	if err != nil {
		return err
	}
	if registry.Projects == nil {
		registry.Projects = make(map[string]store.ProjectRecord)
	}
	absReq, err := filepath.Abs(requirementsFile)
	if err != nil {
		absReq = requirementsFile
	}
	projectPath := filepath.Dir(absReq)
	collectionsPath := resolveCollectionsPath(projectPath, downloadPath)
	registry.Projects[projectPath] = store.ProjectRecord{
		RequirementsFile: absReq,
		CollectionsPath:  collectionsPath,
		LastRun:          time.Now().UTC(),
	}
	return b.saveProjectRegistry(ctx, registry)
}

// LoadProjectRegistry loads the project registry from S3. A missing object
// is treated as an empty, freshly-initialized registry, but an object that
// exists and fails to decode is reported as an error rather than silently
// replaced by an empty registry: cleanup relies on the registry to compute
// which installed collections are still reachable, so an empty registry
// would make it believe nothing is reachable and delete everything.
func (b *Backend) LoadProjectRegistry(ctx context.Context) (*store.ProjectRegistry, error) {
	if err := b.Open(ctx); err != nil {
		return nil, err
	}
	key := b.key(statePrefix, projectsObject)
	data, err := b.readObject(ctx, key)
	if err != nil {
		if errors.Is(err, errS3NotFound) {
			return &store.ProjectRegistry{Projects: make(map[string]store.ProjectRecord)}, nil
		}
		return nil, err
	}
	var registry store.ProjectRegistry
	if err := json.Unmarshal(data, &registry); err != nil {
		return nil, fmt.Errorf("%w at %s: %w (remove the object or clear the cache to rebuild the registry)",
			helpers.ErrCorruptProjectRegistry, key, err)
	}
	if registry.Projects == nil {
		registry.Projects = make(map[string]store.ProjectRecord)
	}
	return &registry, nil
}

// Artifacts returns the S3-backed artifact store.
func (b *Backend) Artifacts() cacheManager.ArtifactStore {
	return b.artifacts
}

// SweepTemp is a no-op for the S3 backend: its artifact download temps are
// created under the OS temp directory (os.TempDir() or a configured base),
// which the operating system reclaims, not under the shared cache, so there
// is nothing in the backend's own storage to sweep.
func (b *Backend) SweepTemp(_ context.Context) error {
	return nil
}

// probeConditionalPut verifies that the configured backend actually enforces
// the conditional writes the lock protocol relies on, before it is allowed to
// rely on them. It writes a small, per-process-unique probe object under the
// locks prefix with a create-if-absent PUT (which must succeed since the key is
// new), then repeats the same create-if-absent PUT against the same
// now-existing key: a conforming backend must reject the second write with a
// precondition-failed error. If it instead reports success, the backend
// silently overwrote the object despite If-None-Match, meaning it cannot be
// trusted to enforce the create-if-absent semantics the lock depends on.
//
// It owns the probe object's whole lifetime, including for the compare-and-swap
// half it hands off to probeCompareAndSwap once create-if-absent has been
// proven: one object, one deferred cleanup, and the swap probe inherits an
// object that demonstrably exists.
func (b *Backend) probeConditionalPut(ctx context.Context) error {
	suffix, err := generateLockToken()
	if err != nil {
		return err
	}
	key := b.key(locksPrefix, conditionalProbeObject+"-"+suffix)
	body := []byte("probe")

	if err := b.client.putObject(ctx, key, bytes.NewReader(body), int64(len(body)),
		putObjectAttrs{contentType: "text/plain"}, putCondition{ifNoneMatch: true}); err != nil {
		return err
	}
	//nolint:contextcheck // best-effort cleanup deliberately uses a fresh context, not ctx:
	// ctx may be near its own deadline by the time Open runs the probe, but a leftover probe
	// object is harmless (its random suffix avoids colliding with the next Open) so it is not
	// worth failing Open over.
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), lockReleaseTimeout)
		defer cancel()
		_ = b.client.deleteObject(cleanupCtx, key)
	}()

	switch putErr := b.client.putObject(ctx, key, bytes.NewReader(body), int64(len(body)),
		putObjectAttrs{contentType: "text/plain"}, putCondition{ifNoneMatch: true}); {
	case putErr == nil:
		return errS3ConditionalPutUnsupported
	case errors.Is(putErr, errS3PreconditionFailed):
		return b.probeCompareAndSwap(ctx, key, body)
	default:
		return putErr
	}
}

// probeCompareAndSwap verifies that the backend honors If-Match against an
// ETag, the second conditional write the lock protocol is built on: it is how
// reclaimIfExpired takes over an expired holder's object, and the only thing
// that arbitrates between two acquirers doing so at once. It runs against the
// probe object probeConditionalPut has already created, and that object's own
// deferred cleanup covers it.
//
// Three answers are required, and each rules out a different non-conforming
// backend. The HEAD must name an ETag at all, since a backend that omits it
// leaves nothing to condition a swap on. A swap against a stale ETag must be
// refused, since a backend that accepts it arbitrates nothing and lets two
// reclaimers both believe they won. And a swap against the CURRENT ETag must
// succeed, which is the check with the least obvious failure mode: a backend
// that refuses every If-Match alike would pass the first two and then fail
// every reclaim at runtime, turning one dead holder's lock object into a
// permanent one - the acquirers waiting on it would each read the refusal as
// another acquirer winning the race, which is indistinguishable from ordinary
// contention and would never resolve.
//
// The stale ETag is this package's own literal rather than a real earlier
// version of the object: a value no backend can have minted is exactly what
// must not match, and using one avoids depending on whether the endpoint
// changes an ETag when identical bytes are rewritten.
func (b *Backend) probeCompareAndSwap(ctx context.Context, key string, body []byte) error {
	headers, err := b.client.headObject(ctx, key)
	if err != nil {
		return err
	}
	etag := strings.TrimSpace(headers.Get("ETag"))
	if etag == "" {
		return errS3CompareAndSwapUnsupported
	}

	switch staleErr := b.client.putObject(ctx, key, bytes.NewReader(body), int64(len(body)),
		putObjectAttrs{contentType: "text/plain"}, putCondition{ifMatch: staleProbeETag}); {
	case staleErr == nil:
		return errS3CompareAndSwapUnsupported
	case errors.Is(staleErr, errS3PreconditionFailed):
		// The refusal this probe requires; fall through to the positive half.
	default:
		return staleErr
	}

	switch currentErr := b.client.putObject(ctx, key, bytes.NewReader(body), int64(len(body)),
		putObjectAttrs{contentType: "text/plain"}, putCondition{ifMatch: etag}); {
	case currentErr == nil:
		return nil
	case errors.Is(currentErr, errS3PreconditionFailed), errors.Is(currentErr, errS3NotFound):
		return errS3CompareAndSwapUnsupported
	default:
		return currentErr
	}
}

// readObject downloads a cache-state object (the S3 snapshot or the project
// registry) and transparently inflates gzip data if needed, bounding both the
// raw and inflated size so a planted oversized or high-ratio gzip object
// cannot be buffered whole into memory. A size-ceiling failure is reported to
// the caller as helpers.ErrStateObjectTooLarge, not readAllCapped's own
// helpers.ErrResponseTooLarge - see the reclassification below for why.
func (b *Backend) readObject(ctx context.Context, key string) ([]byte, error) {
	resp, err := b.client.getObject(ctx, key)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	data, err := readAllCapped(ctx, resp.Body, resp.Header, key,
		helpers.StateObjectMaxCompressedSize, helpers.StateObjectMaxDecompressedSize)
	if err != nil {
		// Reclassify the size ceiling into its own state-object sentinel,
		// deliberately breaking the errors.Is chain to
		// helpers.ErrResponseTooLarge: readAllCapped's cap failure carries
		// that sentinel only because it is built on the same sizeLimitedReader
		// every capped response uses, but a state object is not a response
		// body this program streams to a consumer - it is persisted state
		// this program must be able to read back. Leaving both sentinels
		// reachable here would let this error carry two exit classes at once
		// - ExitCacheCorrupt from the state-object sentinel and ExitNetwork
		// from the response one - leaving exitcode.FromError's own check
		// order to decide which wins rather than what the error means.
		// (Neither sentinel belongs to the three cache-backend classes
		// variables.go's partition governs; this is FromError's rule, not
		// that one.) The cause is rendered with %v, not %w, so only
		// errors.Is matching against helpers.ErrResponseTooLarge is dropped;
		// the "read N bytes, limit is M" detail readAllCapped's own error
		// carries still renders into the message.
		if errors.Is(err, helpers.ErrResponseTooLarge) {
			//nolint:errorlint // deliberately %v, not %w: see the comment above.
			return nil, fmt.Errorf("%w: state object %s: %v", helpers.ErrStateObjectTooLarge, key, err)
		}
		// A gzip member producing no bytes says the object is not the gzipped
		// JSON this backend writes, so it is reclassified here into the same
		// unusable-state class the ceiling above lands in: a state object
		// nobody can read back, whose remedy is discarding it. It is done at
		// this producer rather than by adding helpers.ErrEmptyGzipMember to an
		// exitcode predicate, because that sentinel's other readers - the
		// download shape probe and the per-collection install aggregation -
		// already classify correctly without one and would be dragged along
		// (see the sentinel's own doc comment). The cause keeps its %w, unlike
		// the ceiling above: helpers.ErrEmptyGzipMember belongs to no
		// predicate, so leaving it reachable adds no second exit class to the
		// tree and keeps the message's own cause matchable.
		if errors.Is(err, helpers.ErrEmptyGzipMember) {
			return nil, fmt.Errorf("%w: state object %s: %w", helpers.ErrCorruptStateObject, key, err)
		}
		return nil, err
	}
	return data, nil
}

// readAllCapped reads an object body into memory, transparently inflating
// gzip, while bounding both the compressed read (so an oversized object
// cannot be buffered whole) and the decompressed size (so a gzip bomb cannot
// inflate without bound). Either ceiling being crossed surfaces
// helpers.ErrResponseTooLarge. Gzip detection mirrors readObject's own
// pre-cap logic (isGzip/isGzipStream), so the caps are layered on top of the
// existing decision of whether to gunzip rather than changing it.
//
// It inflates through internal/gzipstream, the same seam the collection
// artifact readers use, and this is the reader that made the shared seam worth
// having: the state object is fetched with the distributed lock already held,
// so a member-flood object crashing this read holds up every runner sharing
// the bucket for a full lockTTL rather than failing one install. The
// compressed cap is no defense against that shape, since it sits at 256 MiB
// and the flood measured on go1.26.6, darwin/arm64 (Apple M3 Pro) needed only
// 64 MB - see internal/gzipstream's own package comment.
//
// This package imports pgzip under its own name, and reaches only its writer
// (SaveStore compressing the snapshot it is about to upload). The name is a
// rule rather than a preference: an alias - `gzip "github.com/klauspost/pgzip"`
// being the natural one - makes a pgzip reader on this seam read like the
// standard library's, which is how the reader below stayed unnoticed through a
// review that found every other one. internal/gzipstream's gate resolves the
// import path rather than the identifier so that no alias can hide the next
// one, and the plain name is what a human reading this file needs.
//
// ctx bounds the inflate on the compressed side. It is the caller's own
// context rather than a budget of this function's making, so a state-object
// read stays governed by helpers.StateObjectDeadline exactly as before.
func readAllCapped(
	ctx context.Context,
	body io.Reader,
	header http.Header,
	key string,
	compressedCap, decompressedCap int64,
) ([]byte, error) {
	limited := helpers.NewSizeLimitedReader(body, compressedCap)
	shouldGzip := isGzip(header) || strings.HasSuffix(key, ".gz")
	if !shouldGzip {
		return io.ReadAll(limited)
	}
	buffered := bufio.NewReader(limited)
	if isGzipStream(buffered) {
		gz, err := gzipstream.NewReader(ctx, buffered)
		if err != nil {
			return nil, err
		}
		defer func() {
			_ = gz.Close()
		}()
		return io.ReadAll(helpers.NewSizeLimitedReader(gz, decompressedCap))
	}
	return io.ReadAll(buffered)
}

// saveProjectRegistry writes the project registry to S3.
func (b *Backend) saveProjectRegistry(ctx context.Context, registry *store.ProjectRegistry) error {
	if registry == nil {
		return nil
	}
	payload, err := json.MarshalIndent(registry, "", "  ")
	if err != nil {
		return err
	}
	key := b.key(statePrefix, projectsObject)
	reader := bytes.NewReader(payload)
	return b.client.putObject(ctx, key, reader, int64(len(payload)),
		putObjectAttrs{contentType: "application/json"}, putCondition{})
}

// key builds a key under the configured S3 prefix.
func (b *Backend) key(parts ...string) string {
	if len(parts) == 0 {
		return b.prefix
	}
	if b.prefix == "" {
		return path.Join(parts...)
	}
	all := make([]string, 0, len(parts)+1)
	all = append(all, b.prefix)
	all = append(all, parts...)
	return path.Join(all...)
}

// resolveCollectionsPath returns an absolute collections path for a project.
func resolveCollectionsPath(projectPath, downloadPath string) string {
	if downloadPath == "" {
		return ""
	}
	if filepath.IsAbs(downloadPath) {
		return downloadPath
	}
	return filepath.Join(projectPath, downloadPath)
}

// isGzip reports whether the headers indicate gzip encoding.
func isGzip(headers http.Header) bool {
	enc := strings.ToLower(strings.TrimSpace(headers.Get("Content-Encoding")))
	return strings.Contains(enc, "gzip")
}

// isGzipStream reports whether the stream begins with gzip magic bytes.
func isGzipStream(reader *bufio.Reader) bool {
	header, err := reader.Peek(peekBytes)
	if err != nil || len(header) < headerLength {
		return false
	}
	return header[0] == 0x1f && header[1] == 0x8b
}
