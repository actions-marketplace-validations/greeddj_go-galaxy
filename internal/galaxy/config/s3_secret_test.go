package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/urfave/cli/v3"
	"gopkg.in/yaml.v3"
)

// The two plaintexts the fixture carries. They are distinct strings so a
// rendering that leaks one but not the other cannot pass.
const (
	s3SecretPlaintext  = "top-secret"
	s3SessionPlaintext = "session-secret"
)

// s3RedactionFixture builds an S3CacheConfig carrying both credentials, plus
// the ordinary non-secret fields, so every assertion below runs against a
// struct shaped like the one a real run holds.
func s3RedactionFixture() S3CacheConfig {
	return S3CacheConfig{
		AccessKey:    "AKIAEXAMPLE",
		SecretKey:    NewSecret(s3SecretPlaintext),
		SessionToken: NewSecret(s3SessionPlaintext),
		Bucket:       "b",
		Region:       "r",
	}
}

// TestS3CacheConfigRedactsSecrets drives every serialization path a Secret is
// responsible for closing, against the whole struct rather than the fields in
// isolation - which is the point: what leaks a credential in practice is a
// debug print or a marshaled dump of the config that happens to contain one.
//
// Two killing mutations were run, and they fail different sets of rows,
// which is why every row is asserted with Errorf rather than Fatalf.
//
// Declaring SecretKey as a plain string again fails every rendering at once -
// there is no wrapper left to redact anything - reporting, among the rest,
//
//	%+v output contains the plaintext secret key: {SecretKey:top-secret
//	SessionToken:[REDACTED] Endpoint: Region:r Bucket:b Prefix:
//	AccessKey:AKIAEXAMPLE Enabled:false PathStyle:false}
//
// Deleting Secret.GoString instead - keeping the type, removing the one
// method %#v consults - fails the %#v rows and only those:
//
//	%#v output contains the plaintext secret key:
//	config.S3CacheConfig{SecretKey:config.Secret{value:"top-secret"}, ...}
//
// That second one is what earns %#v its own row: it is the single verb that
// reflects into an unexported field regardless of String, and the only
// rendering the other four cannot stand in for.
func TestS3CacheConfigRedactsSecrets(t *testing.T) {
	t.Parallel()
	cfg := s3RedactionFixture()

	// Encoding this struct is the subject of the test, so both encoders are
	// exempted here rather than avoided. gosec names AccessKey, which is
	// deliberately not a Secret - an access key id rides in cleartext in every
	// signed request's Authorization header anyway, per S3CacheConfig's own
	// doc comment. musttag wants serialization tags on the struct, which it
	// has none of because production never encodes it; adding them to satisfy
	// a test would be inventing an API this type does not offer.
	//nolint:gosec,musttag // see above
	jsonBytes, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	//nolint:gosec,musttag // same fixture, same reasons as the json.Marshal above
	yamlBytes, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}

	renderings := map[string]string{
		"%v":   fmt.Sprintf("%v", cfg),
		"%+v":  fmt.Sprintf("%+v", cfg),
		"%#v":  fmt.Sprintf("%#v", cfg),
		"json": string(jsonBytes),
		"yaml": string(yamlBytes),
	}
	for verb, out := range renderings {
		if strings.Contains(out, s3SecretPlaintext) {
			t.Errorf("%s output contains the plaintext secret key: %s", verb, out)
		}
		if strings.Contains(out, s3SessionPlaintext) {
			t.Errorf("%s output contains the plaintext session token: %s", verb, out)
		}
	}
}

// TestS3CacheConfigSecretsAreStillReadable is the positive control for the
// test above, on the identical fixture: the credentials really are held, they
// are simply not printable. Without it, "the plaintext was not found" would
// be indistinguishable from a fixture whose fields are empty.
func TestS3CacheConfigSecretsAreStillReadable(t *testing.T) {
	t.Parallel()
	cfg := s3RedactionFixture()

	if got := cfg.SecretKey.Reveal(); got != s3SecretPlaintext {
		t.Errorf("SecretKey.Reveal() = %q, want %q", got, s3SecretPlaintext)
	}
	if got := cfg.SessionToken.Reveal(); got != s3SessionPlaintext {
		t.Errorf("SessionToken.Reveal() = %q, want %q", got, s3SessionPlaintext)
	}
}

// newS3Cmd builds a cli.Command carrying only the S3 flags loadS3CacheConfig
// reads, so a test can drive that loader without standing up the whole CLI.
func newS3Cmd(t *testing.T, args []string) *cli.Command {
	t.Helper()

	var captured *cli.Command
	cmd := &cli.Command{
		Name: "go-galaxy",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "s3-bucket"},
			&cli.StringFlag{Name: "s3-prefix"},
			&cli.StringFlag{Name: "s3-endpoint"},
			&cli.StringFlag{Name: "s3-region"},
			&cli.StringFlag{Name: "s3-access-key"},
			&cli.StringFlag{Name: "s3-secret-key"},
			&cli.StringFlag{Name: "s3-session-token"},
			&cli.BoolFlag{Name: "s3-path-style-disabled"},
		},
		Action: func(_ context.Context, c *cli.Command) error {
			captured = c
			return nil
		},
	}
	if err := cmd.Run(context.Background(), append([]string{"go-galaxy"}, args...)); err != nil {
		t.Fatalf("cmd.Run() error = %v, want nil", err)
	}
	return captured
}

// TestLoadS3CacheConfigRequiresCredentials pins the emptiness check that
// moved from a string comparison to Secret.IsSet: a bucket with no secret key
// is still refused, and one with both credentials is still accepted and
// carries the secret through. The accepting row is what keeps the refusing
// one honest - without it, IsSet always reporting false would look identical.
func TestLoadS3CacheConfigRequiresCredentials(t *testing.T) {
	t.Parallel()

	t.Run("missing secret key is refused", func(t *testing.T) {
		t.Parallel()
		c := newS3Cmd(t, []string{"--s3-bucket=b", "--s3-access-key=x"})

		_, err := loadS3CacheConfig(c)

		if !errors.Is(err, helpers.ErrS3EmptyCreds) {
			t.Fatalf("loadS3CacheConfig = %v, want errors.Is helpers.ErrS3EmptyCreds", err)
		}
	})

	t.Run("both credentials are accepted", func(t *testing.T) {
		t.Parallel()
		c := newS3Cmd(t, []string{"--s3-bucket=b", "--s3-access-key=x", "--s3-secret-key=" + s3SecretPlaintext})

		cfg, err := loadS3CacheConfig(c)
		if err != nil {
			t.Fatalf("loadS3CacheConfig = %v, want nil", err)
		}
		if !cfg.Enabled {
			t.Fatalf("Enabled = false, want true for a configured bucket")
		}
		if got := cfg.SecretKey.Reveal(); got != s3SecretPlaintext {
			t.Fatalf("SecretKey.Reveal() = %q, want %q", got, s3SecretPlaintext)
		}
	})
}
