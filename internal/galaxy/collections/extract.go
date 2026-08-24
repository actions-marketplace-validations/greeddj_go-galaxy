package collections

import (
	"context"
	"fmt"

	"github.com/greeddj/go-galaxy/internal/galaxy/archive"
	"github.com/greeddj/go-galaxy/internal/galaxy/extracted"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
)

// extractCollection materializes a collection tarball into target's install
// directory. When extractStore is provided, the tarball is unpacked once into
// the content-addressable store and linked via hard links into target.path.
// Otherwise the tarball is unpacked directly into target.path.
//
// artifactSHAComputed declares whether artifactSHA was hashed by this process
// over tarPath's bytes (see installPayload.artifactSHAComputed); through
// shaProvenance it decides whether the store's ingest must hash the tarball
// once more before extracting under that sha. The empty-artifactSHA fallback
// below hashes the file right here, so it always yields a self-computed sha.
//
// The reset (RemoveAll then MkdirAll) and the extract-marker read/write are
// the only writes here that go through target.root: unpack itself is handed
// target.path, a plain string, and operates below root's own reach. This is
// deliberate, not an oversight - rooting per-archive-entry writes was
// measured at 1.5x-2.3x the cost of this function for zero marginal
// coverage, because the reset immediately above always wipes and recreates
// target.path through root right before unpack runs, so nothing can be
// pre-planted inside it between the two calls.
func extractCollection(
	ctx context.Context,
	col collection,
	tarPath string,
	target installTarget,
	runtime *infra.Infra,
	extractStore *extracted.Store,
	artifactSHA string,
	artifactSHAComputed bool,
) error {
	return extractTree(ctx, col.Namespace+"/"+col.Name, tarPath, target, runtime, extractStore, artifactSHA, artifactSHAComputed, nil)
}

// extractTree is the tree materialization both a collection and a role go
// through: the reset, the unpack (straight or through the extracted store)
// and the extract marker. display names the tree in the skip line.
// postExtract, when set, runs after the unpack and before the marker is
// written, so whatever it adds to the tree - a role's
// meta/.galaxy_install_info - is counted by the marker's tally rather than
// read as drift on the next run.
func extractTree(
	ctx context.Context,
	display string,
	tarPath string,
	target installTarget,
	runtime *infra.Infra,
	extractStore *extracted.Store,
	artifactSHA string,
	artifactSHAComputed bool,
	postExtract func(installTarget) error,
) error {
	if artifactSHA == "" {
		hash, err := archive.FileHashSHA256(tarPath)
		if err != nil {
			return err
		}
		artifactSHA = hash
		artifactSHAComputed = true
	}
	// Refused here, before any of the destructive work below, rather than
	// left to writeExtractMarker's own guard at the end of this function:
	// verifyExtractMarker, RemoveAll, MkdirAll, and a full unpack all sit
	// between this point and that one, so deferring the refusal would
	// guarantee target.path gets wiped and fully re-extracted for a value
	// that was never usable, before the failure is ever reported.
	// writeExtractMarker keeps its own guard regardless - that is its own
	// invariant, independent of any caller, not made redundant by this one.
	if !helpers.IsSHA256Hex(artifactSHA) {
		return fmt.Errorf("%w: %q", helpers.ErrMalformedArtifactSHA256, artifactSHA)
	}
	if verifyExtractMarker(runtime.Output, target, artifactSHA) {
		runtime.Output.Printf("Skipping extraction, already done: %s", display)
		return nil
	}

	// The severest primitive in this whole pipeline: unlike every other
	// rooted call here, a failure is not "nothing was written yet", it is
	// "the previous tree may be gone". Its error is therefore checked and
	// classified, not discarded - a symlinked ansible_collections (or a
	// symlinked namespace/name component) makes this refuse atomically,
	// inside the kernel, before anything is destroyed.
	if err := target.root.RemoveAll(target.rel); err != nil {
		return classifyCollectionsRootError(target.root, target.rel, err)
	}
	if err := target.root.MkdirAll(target.rel, helpers.DirMod); err != nil {
		return classifyCollectionsRootError(target.root, target.rel, err)
	}

	if err := unpack(ctx, tarPath, target.path, extractStore, artifactSHA, artifactSHAComputed); err != nil {
		return err
	}
	if postExtract != nil {
		if err := postExtract(target); err != nil {
			return err
		}
	}

	return writeExtractMarker(target, artifactSHA)
}

func unpack(
	ctx context.Context,
	tarPath, installPath string,
	extractStore *extracted.Store,
	artifactSHA string,
	artifactSHAComputed bool,
) error {
	if extractStore == nil {
		return archive.ExtractTarGz(ctx, tarPath, installPath)
	}
	src, err := extractStore.Ensure(ctx, artifactSHA, tarPath, shaProvenance(artifactSHAComputed))
	if err != nil {
		return err
	}
	return extracted.Materialize(src, installPath)
}

// shaProvenance maps the payload's "this process hashed these bytes" flag
// onto the extracted store's provenance declaration: a self-computed sha lets
// Ensure ingest the tarball without re-reading it, any other sha must still
// be verified against the file's bytes before it keys the shared CAS.
func shaProvenance(selfComputed bool) extracted.SHAProvenance {
	if selfComputed {
		return extracted.SHASelfComputed
	}
	return extracted.SHAFromRecord
}
