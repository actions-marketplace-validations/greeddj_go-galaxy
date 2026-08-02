package helpers

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
)

// ArtifactKeyFingerprintLen is the number of leading hex characters of
// sha256(serverBase) used as an artifact cache key's server-fingerprint
// prefix (see ArtifactKey). 12 hex characters (48 bits) makes an accidental
// collision between two distinct server bases astronomically unlikely while
// keeping the key short and legible in directory listings and S3 object
// listings.
const ArtifactKeyFingerprintLen = 12

// DepsCacheKeySeparator joins a scoped deps-cache key's server-base prefix
// from its "<ns>.<name>@<version>" suffix (see ScopedDepsCacheKey). It is
// "|", a byte that never appears in a URL scheme: every scoped key therefore
// starts with a scheme and contains this separator, while the pre-scoping
// key shape ("<ns>.<name>@<version>", no prefix at all) starts directly with
// a namespace character and never contains "|" (namespace/name/version are
// dotted, dashed, or numeric identifiers, never "|"-bearing). The two shapes
// can therefore never collide or be confused for one another, so no explicit
// migration of old-format keys is needed - see
// StoreSnapshotSchemaVersion, whose bump already drops any snapshot old
// enough to carry them.
const DepsCacheKeySeparator = "|"

// ScopedDepsCacheKey builds the deps-cache key for fqdnAtVersion (an
// "<ns>.<name>@<version>" pin) scoped to serverBase, the server that
// actually answered its metadata fetch. Without the serverBase prefix, two
// servers publishing the same namespace.name@version with different
// dependency graphs would collide on a single deps-cache entry, so whichever
// one resolved first would silently dictate the dependencies used for both.
// serverBase is
// expected to be the normalized server base a collection actually resolved
// from (MetadataProvider.recordBinding's bound base) - never a
// stale/unpinned collection source and never a bare configured server
// default, both of which could disagree with whichever server actually
// produced the cached entry. The URL is kept verbatim rather than hashed:
// Bolt keys allow arbitrary bytes, and a readable key in a debugger or a
// Bolt dump is worth more than the handful of bytes a hash would save.
func ScopedDepsCacheKey(serverBase, fqdnAtVersion string) string {
	return serverBase + DepsCacheKeySeparator + fqdnAtVersion
}

// ArtifactKey builds the cache key for a collection artifact tarball named
// filename, scoped to serverBase so two servers that publish the same
// namespace/name/version (and therefore the same tarball filename) never
// share a cache slot. Without the server scope, the key would be filename
// alone (percent-encoded), so two servers publishing the same name would
// collide on one cache entry with no metadata round trip able to catch it -
// isCacheHit would serve whichever artifact happened to land first,
// indefinitely.
//
// The key is "<fp>.<url.QueryEscape(filename)>": flat (contains no "/"), so
// both the local backend's filepath.Join(cacheDir, key) and the S3 backend's
// path.Join(prefix, key) keep their existing flat, single-level layout with
// no extra directory nesting and no new path-encoding rules. fp is the first
// ArtifactKeyFingerprintLen hex characters of sha256(serverBase): the same
// server always yields the same fp, and two distinct servers yield different
// ones (barring an astronomically unlikely sha256 collision in the
// truncated prefix). serverBase is expected to already be the server a
// collection actually resolved from (a resolved collection's Source, or a
// persisted InstalledEntry's Source for cleanup's purposes) - never a bare
// configured default - so the fp genuinely identifies the server that
// produced the cached bytes.
//
// Content-addressing the artifact store instead (keying by the artifact's
// own sha256) was considered and rejected: the sha is not known until after
// the download completes, which would destroy the metadata-free cache-hit
// fast path the install pipeline is built around (isCacheHit/Has must be
// answerable before any bytes are fetched).
func ArtifactKey(serverBase, filename string) string {
	sum := sha256.Sum256([]byte(serverBase))
	fp := hex.EncodeToString(sum[:])[:ArtifactKeyFingerprintLen]
	return fp + "." + url.QueryEscape(filename)
}

// IsScopedArtifactKey reports whether key already has the shape ArtifactKey
// produces: ArtifactKeyFingerprintLen lowercase hex characters followed by
// ".". This must move in lockstep with ArtifactKey - any change to that
// function's key shape (the fingerprint length, its hex case, or the "."
// separator) has to be mirrored here, since this is the only other place
// that asserts what ArtifactKey's output looks like.
//
// It exists because ArtifactKey's shape is not exclusive to keys ArtifactKey
// itself produced: url.QueryEscape leaves both "." and "-" unescaped, so a
// filename built from a walked namespace/name/version - none of which this
// codebase restricts to excluding "." - can coincide with it byte for byte.
// A caller comparing a candidate key against a live, server-scoped
// ArtifactKey entry (rather than merely checking its own construction)
// should use this predicate to recognize that shape before treating the
// candidate as safe to act on unconditionally.
func IsScopedArtifactKey(key string) bool {
	if len(key) <= ArtifactKeyFingerprintLen || key[ArtifactKeyFingerprintLen] != '.' {
		return false
	}
	for i := range ArtifactKeyFingerprintLen {
		c := key[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
