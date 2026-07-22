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
	gzip "github.com/klauspost/pgzip"
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
		return nil, errS3BucketIsEmpty
	}
	if httpClient == nil {
		return nil, errS3HttpClientIsNil
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

// Open initializes the S3 client and ensures the bucket exists.
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

// Lock acquires an S3-based distributed lock.
func (b *Backend) Lock(ctx context.Context) (func() error, error) {
	if err := b.Open(ctx); err != nil {
		return nil, err
	}
	lockKey := b.key(locksPrefix, lockObject)
	return b.acquireLock(ctx, lockKey)
}

// LoadStore loads the snapshot store from S3. It applies the same schema
// policy as the local backend's Load: a newer-than-current schema version
// is reported as an error since this binary cannot safely interpret it, an
// older-than-current version causes the snapshot to be dropped and rebuilt
// (a fresh empty Store, nil error) rather than partially trusted, and a
// matching version returns the loaded data as-is.
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
	st := store.New()
	if err := json.Unmarshal(data, st); err != nil {
		return nil, err
	}
	switch verr := store.ValidateSchema(st.Meta.SchemaVersion); {
	case errors.Is(verr, helpers.ErrOutdatedSchemaVersion):
		return store.New(), nil
	case verr != nil:
		return nil, verr
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
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(payload); err != nil {
		_ = zw.Close()
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	key := b.key(statePrefix, storeObject)
	reader := bytes.NewReader(buf.Bytes())
	return b.client.putObject(ctx, key, reader, int64(buf.Len()), "application/json", "gzip", nil, false, "")
}

// ClearFiles removes cached artifacts from S3.
func (b *Backend) ClearFiles(ctx context.Context) error {
	if err := b.Open(ctx); err != nil {
		return err
	}
	prefix := b.key(artifactsPrefix)
	keys, err := b.client.listObjects(ctx, prefix)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if err := b.client.deleteObject(ctx, key); err != nil {
			return err
		}
	}
	return nil
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

// readObject downloads an object and transparently inflates gzip data if needed.
func (b *Backend) readObject(ctx context.Context, key string) ([]byte, error) {
	resp, err := b.client.getObject(ctx, key)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	shouldGzip := isGzip(resp.Header) || strings.HasSuffix(key, ".gz")
	if !shouldGzip {
		return io.ReadAll(resp.Body)
	}
	buffered := bufio.NewReader(resp.Body)
	if isGzipStream(buffered) {
		gz, err := gzip.NewReader(buffered)
		if err != nil {
			return nil, err
		}
		defer func() {
			_ = gz.Close()
		}()
		return io.ReadAll(gz)
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
	return b.client.putObject(ctx, key, reader, int64(len(payload)), "application/json", "", nil, false, "")
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
