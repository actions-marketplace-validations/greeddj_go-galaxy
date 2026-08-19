// Package cache constructs the cache backend a run uses. New is the single
// factory: it returns the S3 backend when an S3 bucket is configured and the
// filesystem backend otherwise, both behind the Backend interface declared in
// internal/galaxy/cache. A new backend implementation is selected here and
// nowhere else, so no caller ever names a concrete one.
package cache

import (
	"errors"

	"github.com/greeddj/go-galaxy/internal/cache/local"
	"github.com/greeddj/go-galaxy/internal/cache/s3"
	cacheManager "github.com/greeddj/go-galaxy/internal/galaxy/cache"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/infra"
)

var errHTTPClientNil = errors.New("http client is nil")

// New selects and constructs a cache backend based on configuration.
// A nil config is reported through helpers.ErrConfigIsNil so the failure
// classifies into the same exit code as every other nil-config refusal.
func New(cfg *config.Config, runtime *infra.Infra) (cacheManager.Backend, error) {
	if cfg == nil {
		return nil, helpers.ErrConfigIsNil
	}
	if cfg.S3Cache.Enabled {
		if runtime == nil || runtime.HTTP == nil {
			return nil, errHTTPClientNil
		}
		tempDir := ""
		if runtime.TempDir != nil {
			tempDir = runtime.TempDir()
		}
		return s3.New(cfg.S3Cache, runtime.HTTP, tempDir)
	}
	return local.New(cfg.CacheDir), nil
}
