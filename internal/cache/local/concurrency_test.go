package local

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// errArtifactNotFoundAfterCommit and errArtifactMetaNotFoundAfterCommit are
// static sentinels for readBackTestArtifact's two presence checks, declared
// at package level per err113 rather than as inline errors.New calls.
var (
	errArtifactNotFoundAfterCommit     = errors.New("Has reported the key absent right after Commit")
	errArtifactMetaNotFoundAfterCommit = errors.New("Meta reported the key absent right after Commit")
)

// TestArtifactsSafeForConcurrentUse pins the goroutine-safety half of
// cacheManager.ArtifactStore's contract for the local backend: eight
// goroutines each run the full TempFile/Commit/Has/Meta/Fetch/Delete cycle
// concurrently, under the race detector, every one against its own,
// distinct key. That is a deliberate choice, not an oversight - the
// contract only promises safety across distinct keys, and a shared key
// would exercise a guarantee the contract does not make.
//
// Killing mutation, actually run against this file: a temporary
// unsynchronized `calls int` field was added to Artifacts and incremented at
// the top of Has, with no other change. `go test ./internal/cache/local/
// -run TestArtifactsSafeForConcurrentUse -race -v -count=1` then failed with
// a real data race, both accesses inside Has (stack frame source locations
// are omitted below: a Go comment in this module may not cite a production
// file's line number, and every frame in a real -race dump carries one):
//
//	WARNING: DATA RACE
//	Read at 0x00c0000101d8 by goroutine 14:
//	  github.com/greeddj/go-galaxy/internal/cache/local.(*Artifacts).Has()
//	Previous write at 0x00c0000101d8 by goroutine 13:
//	  github.com/greeddj/go-galaxy/internal/cache/local.(*Artifacts).Has()
//	==================
//	--- FAIL: TestArtifactsSafeForConcurrentUse (0.00s)
//	FAIL
//
// Removing the field restored a clean, race-free pass.
//
// Not covered here: the S3 backend's own ArtifactStore implementation is not
// exercised by this test, because the fake HTTP server its tests run against
// (fakeS3, defined across internal/cache/s3's own _test.go files) is
// unexported and unreachable from this package. What is already covered
// there instead: TestSaveStoreConcurrentMutationIsRaceFree pins the absence
// of races when the store is mutated concurrently during SaveStore.
func TestArtifactsSafeForConcurrentUse(t *testing.T) {
	dir := t.TempDir()
	artifacts := NewArtifacts(dir)

	const goroutines = 8
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for i := range goroutines {
		wg.Go(func() {
			errs <- exerciseArtifactKey(artifacts, fmt.Sprintf("k%02d", i))
		})
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
}

// exerciseArtifactKey runs the full artifact lifecycle - commit, then
// Has/Meta/Fetch/Delete - against one key, returning the first error
// encountered. Every caller of this helper works on its own key, so
// concurrent calls never touch the same on-disk path. Split into two
// smaller helpers below rather than kept as one function so each stays
// under the project's cyclomatic-complexity ceiling.
func exerciseArtifactKey(artifacts *Artifacts, key string) error {
	if err := commitTestArtifact(artifacts, key); err != nil {
		return err
	}
	return readBackTestArtifact(artifacts, key)
}

// commitTestArtifact creates a temp file, writes a few bytes naming key, and
// commits it under key with a validly-shaped sha256 in its metadata.
func commitTestArtifact(artifacts *Artifacts, key string) error {
	ctx := context.Background()

	file, cleanupTemp, err := artifacts.TempFile(ctx, ".artifact-")
	if err != nil {
		return fmt.Errorf("key %s: TempFile: %w", key, err)
	}
	defer cleanupTemp()

	if _, err := file.WriteString("artifact bytes for " + key); err != nil {
		return fmt.Errorf("key %s: write temp file: %w", key, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("key %s: close temp file: %w", key, err)
	}

	if _, err := artifacts.Commit(ctx, key, file.Name(), map[string]string{"sha256": testSHA}); err != nil {
		return fmt.Errorf("key %s: Commit: %w", key, err)
	}
	return nil
}

// readBackTestArtifact runs Has, Meta, Fetch, and Delete against key, all of
// which a prior commitTestArtifact(artifacts, key) must satisfy.
func readBackTestArtifact(artifacts *Artifacts, key string) error {
	ctx := context.Background()

	found, err := artifacts.Has(ctx, key)
	if err != nil {
		return fmt.Errorf("key %s: Has: %w", key, err)
	}
	if !found {
		return fmt.Errorf("key %s: %w", key, errArtifactNotFoundAfterCommit)
	}

	_, found, err = artifacts.Meta(ctx, key)
	if err != nil {
		return fmt.Errorf("key %s: Meta: %w", key, err)
	}
	if !found {
		return fmt.Errorf("key %s: %w", key, errArtifactMetaNotFoundAfterCommit)
	}

	fetched, err := artifacts.Fetch(ctx, key)
	if err != nil {
		return fmt.Errorf("key %s: Fetch: %w", key, err)
	}
	if fetched.Cleanup != nil {
		fetched.Cleanup()
	}

	if err := artifacts.Delete(ctx, key); err != nil {
		return fmt.Errorf("key %s: Delete: %w", key, err)
	}
	return nil
}
