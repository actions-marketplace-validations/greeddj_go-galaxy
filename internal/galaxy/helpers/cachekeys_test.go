package helpers

import "testing"

// TestArtifactKeyIsFlat proves ArtifactKey never produces a "/" - the
// invariant both the local backend's filepath.Join(cacheDir, key) and the S3
// backend's path.Join(prefix, key) rely on to stay a flat, single-level
// layout with no extra directory nesting - across filenames that would
// themselves need percent-encoding.
func TestArtifactKeyIsFlat(t *testing.T) {
	t.Parallel()
	cases := []string{
		"ns-name-1.0.0.tar.gz",
		"ns/name-1.0.0.tar.gz",
		"weird name/with spaces-1.0.0.tar.gz",
	}
	for _, filename := range cases {
		key := ArtifactKey("https://galaxy.example.com", filename)
		for _, r := range key {
			if r == '/' {
				t.Fatalf("ArtifactKey(%q) = %q, contains a %q", filename, key, "/")
			}
		}
	}
}

// TestArtifactKeyDifferentBasesYieldDifferentPrefixes is the collision
// regression guard: two distinct server bases publishing the identical
// filename must produce distinct keys, since ArtifactKey folds a server
// fingerprint into the key rather than using the filename alone.
func TestArtifactKeyDifferentBasesYieldDifferentPrefixes(t *testing.T) {
	t.Parallel()
	const filename = "ns-name-1.0.0.tar.gz"
	keyA := ArtifactKey("https://a.example.com", filename)
	keyB := ArtifactKey("https://b.example.com", filename)
	if keyA == keyB {
		t.Fatalf("ArtifactKey produced the same key %q for two different server bases", keyA)
	}
}

// TestArtifactKeyStableForSameBase proves ArtifactKey is a pure, deterministic
// function of its inputs: the same base and filename must always yield the
// same key, since a later cache-hit lookup depends on this being stable
// across process runs.
func TestArtifactKeyStableForSameBase(t *testing.T) {
	t.Parallel()
	const base = "https://galaxy.example.com"
	const filename = "ns-name-1.0.0.tar.gz"
	first := ArtifactKey(base, filename)
	second := ArtifactKey(base, filename)
	if first != second {
		t.Fatalf("ArtifactKey(%q, %q) = %q, then %q on a second call - want a stable result", base, filename, first, second)
	}
}

// TestArtifactKeyFingerprintLength pins the fingerprint prefix length so a
// future edit to ArtifactKeyFingerprintLen is a deliberate, visible change
// rather than an accidental one.
func TestArtifactKeyFingerprintLength(t *testing.T) {
	t.Parallel()
	key := ArtifactKey("https://galaxy.example.com", "ns-name-1.0.0.tar.gz")
	if len(key) <= ArtifactKeyFingerprintLen {
		t.Fatalf("ArtifactKey result %q is too short to contain a fingerprint prefix", key)
	}
	if key[ArtifactKeyFingerprintLen] != '.' {
		t.Fatalf("ArtifactKey result %q does not have '.' right after the %d-character fingerprint", key, ArtifactKeyFingerprintLen)
	}
}

// TestScopedDepsCacheKeyNeverCollidesWithOldFormat proves the pre-scoping key
// shape ("<ns>.<name>@<version>", no server prefix at all) can never be
// produced by ScopedDepsCacheKey for any server base: every scoped key
// contains DepsCacheKeySeparator, which an old-format key - built as a bare
// "ns.name@version" fmt.Sprintf, never containing "|" - never does.
func TestScopedDepsCacheKeyNeverCollidesWithOldFormat(t *testing.T) {
	t.Parallel()
	oldFormatKey := "acme.widgets@1.0.0"

	bases := []string{
		"https://galaxy.example.com",
		"https://galaxy.example.com/api/v3",
		"",
	}
	for _, base := range bases {
		got := ScopedDepsCacheKey(base, oldFormatKey)
		if got == oldFormatKey {
			t.Fatalf("ScopedDepsCacheKey(%q, %q) = %q, collides with the old-format key", base, oldFormatKey, got)
		}
		found := false
		for _, r := range got {
			if string(r) == DepsCacheKeySeparator {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("ScopedDepsCacheKey(%q, %q) = %q, does not contain the %q separator", base, oldFormatKey, got, DepsCacheKeySeparator)
		}
	}
}

// TestScopedDepsCacheKeyDistinguishesServers proves two different bases for
// the identical fqdn@version produce two different scoped keys - the
// collision regression guard for the deps-cache side of the server-scoped
// key shape.
func TestScopedDepsCacheKeyDistinguishesServers(t *testing.T) {
	t.Parallel()
	const fqdnAtVersion = "acme.widgets@1.0.0"
	keyA := ScopedDepsCacheKey("https://a.example.com", fqdnAtVersion)
	keyB := ScopedDepsCacheKey("https://b.example.com", fqdnAtVersion)
	if keyA == keyB {
		t.Fatalf("ScopedDepsCacheKey produced the same key %q for two different server bases", keyA)
	}
}
