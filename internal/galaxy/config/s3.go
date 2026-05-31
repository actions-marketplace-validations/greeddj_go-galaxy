package config

import (
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/urfave/cli/v3"
)

// S3CacheConfig defines configuration for S3 cache backend.
type S3CacheConfig struct {
	Endpoint     string
	Region       string
	Bucket       string
	Prefix       string
	AccessKey    string
	SecretKey    string
	SessionToken string
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
		SecretKey:    c.String("s3-secret-key"),
		SessionToken: c.String("s3-session-token"),
	}

	if cfg.Bucket == "" {
		return cfg, nil
	}
	cfg.Enabled = true

	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return cfg, helpers.ErrS3EmptyCreds
	}

	cfg.PathStyle = !c.Bool("s3-path-style-disabled")

	return cfg, nil
}
