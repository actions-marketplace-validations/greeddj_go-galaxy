package collections

import (
	"fmt"
	"os"

	"github.com/greeddj/go-galaxy/internal/galaxy/archive"
	"github.com/greeddj/go-galaxy/internal/galaxy/extracted"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
)

// extractCollection materializes a collection tarball into the install path.
// When extractStore is provided, the tarball is unpacked once into the
// content-addressable store and linked via hard links into installPath.
// Otherwise the tarball is unpacked directly into installPath.
func extractCollection(
	col collection,
	tarPath, installPath string,
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
	// verifyExtractMarker, os.RemoveAll, MkdirAll, and a full unpack all sit
	// between this point and that one, so deferring the refusal would
	// guarantee installPath gets wiped and fully re-extracted for a value
	// that was never usable, before the failure is ever reported.
	// writeExtractMarker keeps its own guard regardless - that is its own
	// invariant, independent of any caller, not made redundant by this one.
	if !helpers.IsSHA256Hex(artifactSHA) {
		return fmt.Errorf("%w: %q", helpers.ErrMalformedArtifactSHA256, artifactSHA)
	}
	if verifyExtractMarker(runtime.Output, installPath, artifactSHA) {
		runtime.Output.Printf("⏭️ Skipping extraction, already done: %s/%s", col.Namespace, col.Name)
		return nil
	}

	_ = os.RemoveAll(installPath)
	if err := os.MkdirAll(installPath, helpers.DirMod); err != nil {
		return err
	}

	if err := unpack(tarPath, installPath, extractStore, artifactSHA); err != nil {
		return err
	}

	return writeExtractMarker(installPath, artifactSHA)
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
