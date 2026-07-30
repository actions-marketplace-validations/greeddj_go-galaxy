package collections

import (
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
// The reset (RemoveAll then MkdirAll) and the extract-marker read/write are
// the only writes here that go through target.root: unpack itself is handed
// target.path, a plain string, and operates below root's own reach. This is
// deliberate, not an oversight - rooting per-archive-entry writes was
// measured at 1.5x-2.3x the cost of this function for zero marginal
// coverage, because the reset immediately above always wipes and recreates
// target.path through root right before unpack runs, so nothing can be
// pre-planted inside it between the two calls.
func extractCollection(
	col collection,
	tarPath string,
	target installTarget,
	runtime *infra.Infra,
	extractStore *extracted.Store,
	artifactSHA string,
) error {
	if artifactSHA == "" {
		hash, err := archive.FileHashSHA256(tarPath)
		if err != nil {
			return err
		}
		artifactSHA = hash
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
		runtime.Output.Printf("⏭️ Skipping extraction, already done: %s/%s", col.Namespace, col.Name)
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

	if err := unpack(tarPath, target.path, extractStore, artifactSHA); err != nil {
		return err
	}

	return writeExtractMarker(target, artifactSHA)
}

func unpack(tarPath, installPath string, extractStore *extracted.Store, artifactSHA string) error {
	if extractStore == nil {
		return archive.ExtractTarGz(tarPath, installPath)
	}
	src, err := extractStore.Ensure(artifactSHA, tarPath)
	if err != nil {
		return err
	}
	return extracted.Materialize(src, installPath)
}
