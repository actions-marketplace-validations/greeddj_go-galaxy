package config

import (
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/urfave/cli/v3"
)

// S3CacheConfig defines configuration for S3 cache backend.
//
// SecretKey and SessionToken are credentials and carry the Secret type, so
// no %v, %+v, %#v, JSON, or YAML rendering of this struct - or of the
// *Config that holds it - can print them. AccessKey deliberately stays a
// plain string: an AWS access key id travels in cleartext inside the
// Authorization header of every signed request by construction, so redacting
// it in logs prevents no disclosure while adding a third Reveal call site
// that buys nothing. That asymmetry is a decision, not an oversight.
type S3CacheConfig struct {
	SecretKey    Secret
	SessionToken Secret
	Endpoint     string
	Region       string
	Bucket       string
	Prefix       string
	AccessKey    string
	Enabled      bool
	PathStyle    bool
}

// loadS3CacheConfig builds S3 cache config from CLI flags.
func loadS3CacheConfig(c *cli.Command) (S3CacheConfig, error) {
	cfg := S3CacheConfig{
		Bucket:       c.String("s3-bucket"),
		Prefix:       c.String("s3-prefix"),
		Endpoint:     c.String("s3-endpoint"),
		Region:       c.String("s3-region"),
		AccessKey:    c.String("s3-access-key"),
		SecretKey:    NewSecret(c.String("s3-secret-key")),
		SessionToken: NewSecret(c.String("s3-session-token")),
	}

	if cfg.Bucket == "" {
		return cfg, nil
	}
	cfg.Enabled = true

	if cfg.AccessKey == "" || !cfg.SecretKey.IsSet() {
		return cfg, helpers.ErrS3EmptyCreds
	}

	cfg.PathStyle = !c.Bool("s3-path-style-disabled")

	return cfg, nil
}
