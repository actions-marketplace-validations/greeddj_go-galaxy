package helpers

// SHA256HexLen is the length of a sha256 digest written as lowercase hex
// (32 bytes -> 64 hex chars).
const SHA256HexLen = 64

// IsSHA256Hex reports whether s is exactly SHA256HexLen lowercase hexadecimal
// characters, the only shape this project ever writes or trusts for a
// sha256 digest.
//
// Lowercase-only is not an arbitrary restriction that could reject an
// honest deployment: each of resolveArtifactSHA's two validated routes is
// separately gated against uppercase before this predicate ever sees it.
// Route one, meta.Artifact.Sha256 (raw Galaxy API JSON): verifyDownloadSHA
// already compares a server's value to this process's own computed hex
// digest with `==`, not strings.EqualFold, so a server emitting uppercase
// can never complete a fresh download - and therefore can never have
// populated the cache whose hit path is the only place uppercase digest text
// could otherwise reach this check. Route two, artifactMeta["sha256"] (a
// cache sidecar): on the local backend, local.Artifacts.Fetch gates the
// sidecar with this exact predicate before ever surfacing it; on the S3
// backend, s3.verifyArtifactSHA gates it with `==` against this process's
// own computed digest, not strings.EqualFold: using EqualFold there would
// let an uppercase x-amz-meta-sha256 value reach this far. Rejecting
// uppercase here closes the same door a second time on both
// routes, rather than opening a new one - and is what lets both backends
// agree on one canonical form for what a valid cached digest looks like.
func IsSHA256Hex(s string) bool {
	if len(s) != SHA256HexLen {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
