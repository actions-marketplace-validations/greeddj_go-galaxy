// Package infra holds Infra, the per-run container of runtime dependencies
// threaded through every subsystem: the operator-output printer, the shared
// HTTP client, the clock and temp-directory functions a test substitutes, and
// this run's metrics counters. Extending Infra is preferred over introducing a
// new global or widening an already-wide signature. New always allocates fresh
// counters, so nothing carries over between two runs in one process.
//
// It also carries the artifact-download, metadata-fetch, cache-state and
// signature-fetch budgets as test-only override fields. Read each one through
// its accessor - ArtifactDeadline, MetadataDeadline, StateDeadline,
// SignatureDeadline - never the field, so a nil Infra or a non-positive
// override falls back to the helpers constant by construction rather than by
// caller convention.
package infra

import (
	"net/http"
	"os"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/greeddj/go-galaxy/internal/galaxy/metrics"
	"github.com/greeddj/go-galaxy/internal/galaxy/output"
)

// Infra holds runtime dependencies such as IO and HTTP clients.
type Infra struct {
	Output  output.Printer
	HTTP    *http.Client
	Now     func() time.Time
	TempDir func() string
	// Metrics accumulates this run's artifact cache-hit/miss and
	// bytes-downloaded tallies. It belongs to this Infra, and therefore to
	// this run: New always allocates a fresh Counters, so totals never carry
	// over from a prior run sharing the same process.
	Metrics *metrics.Counters
	// ArtifactDownloadDeadline overrides helpers.ArtifactDownloadDeadline for
	// this run. It is test-only: production code never sets it, and nothing
	// wires it to a CLI flag, an environment variable, or an ansible.cfg key
	// (see helpers.ArtifactDownloadDeadline's own doc comment for why). Read
	// it only through ArtifactDeadline, never directly, so an unset or
	// nonsensical value structurally falls back to the real constant.
	ArtifactDownloadDeadline time.Duration
	// MetadataFetchDeadline overrides helpers.MetadataFetchDeadline for this
	// run. It is test-only, for the identical reason
	// ArtifactDownloadDeadline is: nothing wires it to a CLI flag, an
	// environment variable, or an ansible.cfg key. Read it only through
	// MetadataDeadline, never directly.
	MetadataFetchDeadline time.Duration
	// StateObjectDeadline overrides helpers.StateObjectDeadline for this run.
	// It is test-only, for the identical reason ArtifactDownloadDeadline is:
	// nothing wires it to a CLI flag, an environment variable, or an
	// ansible.cfg key. Read it only through StateDeadline, never directly.
	StateObjectDeadline time.Duration
	// SignatureFetchDeadline overrides helpers.SignatureFetchDeadline for this
	// run. It is test-only, for the identical reason ArtifactDownloadDeadline
	// is: nothing wires it to a CLI flag, an environment variable, or an
	// ansible.cfg key. Read it only through SignatureDeadline, never directly.
	SignatureFetchDeadline time.Duration
}

// New builds Infra with default helpers for time and temp paths.
func New(out output.Printer, httpClient *http.Client) *Infra {
	return &Infra{
		Output:                   out,
		HTTP:                     httpClient,
		Now:                      time.Now,
		TempDir:                  os.TempDir,
		Metrics:                  &metrics.Counters{},
		ArtifactDownloadDeadline: helpers.ArtifactDownloadDeadline,
		MetadataFetchDeadline:    helpers.MetadataFetchDeadline,
		StateObjectDeadline:      helpers.StateObjectDeadline,
		SignatureFetchDeadline:   helpers.SignatureFetchDeadline,
	}
}

// ArtifactDeadline returns the per-acquisition artifact download budget this
// run uses: i.ArtifactDownloadDeadline when it is set to a positive duration,
// or helpers.ArtifactDownloadDeadline otherwise. Every call site reads the
// budget through this method rather than the field directly, so a nil Infra,
// a zero-value Infra, or a nonsensical (non-positive) override all fall back
// to the real constant structurally, instead of by caller convention.
func (i *Infra) ArtifactDeadline() time.Duration {
	if i == nil || i.ArtifactDownloadDeadline <= 0 {
		return helpers.ArtifactDownloadDeadline
	}
	return i.ArtifactDownloadDeadline
}

// MetadataDeadline returns the per-request Galaxy metadata fetch budget this
// run uses: i.MetadataFetchDeadline when it is set to a positive duration, or
// helpers.MetadataFetchDeadline otherwise. Every call site reads the budget
// through this method rather than the field directly, mirroring
// ArtifactDeadline's own structural fallback.
func (i *Infra) MetadataDeadline() time.Duration {
	if i == nil || i.MetadataFetchDeadline <= 0 {
		return helpers.MetadataFetchDeadline
	}
	return i.MetadataFetchDeadline
}

// StateDeadline returns the per-operation cache-state budget this run uses:
// i.StateObjectDeadline when it is set to a positive duration, or
// helpers.StateObjectDeadline otherwise. Every call site reads the budget
// through this method rather than the field directly, mirroring
// ArtifactDeadline's own structural fallback.
func (i *Infra) StateDeadline() time.Duration {
	if i == nil || i.StateObjectDeadline <= 0 {
		return helpers.StateObjectDeadline
	}
	return i.StateObjectDeadline
}

// SignatureDeadline returns the per-collection signature-phase budget this run
// uses: i.SignatureFetchDeadline when it is set to a positive duration, or
// helpers.SignatureFetchDeadline otherwise. Every call site reads the budget
// through this method rather than the field directly, mirroring
// ArtifactDeadline's own structural fallback.
func (i *Infra) SignatureDeadline() time.Duration {
	if i == nil || i.SignatureFetchDeadline <= 0 {
		return helpers.SignatureFetchDeadline
	}
	return i.SignatureFetchDeadline
}

// DebugAnsibleConfig logs which settings were sourced from ansible.cfg, then
// the fully resolved server list (see debugServerList) - independent of
// whether ansible.cfg contributed anything at all, since the server list can
// equally come from CLI flags or env vars alone.
func (i *Infra) DebugAnsibleConfig(cfg *config.Config) {
	if i == nil || i.Output == nil || cfg == nil {
		return
	}
	if cfg.AnsibleConfigPath != "" {
		if cfg.AnsibleCollectionsPathUsed {
			i.Output.Debugf("ansible.cfg %s: defaults.collections_path=%s", cfg.AnsibleConfigPath, cfg.DownloadPath)
		}
		if cfg.AnsibleCacheDirUsed {
			i.Output.Debugf("ansible.cfg %s: galaxy.cache_dir=%s", cfg.AnsibleConfigPath, cfg.CacheDir)
		}
		if cfg.AnsibleServerUsed && !cfg.AnsibleServerEnvUsed {
			i.Output.Debugf("ansible.cfg %s: galaxy.server=%s", cfg.AnsibleConfigPath, cfg.Server)
		}
	}
	// Outside the block above on purpose: ANSIBLE_GALAXY_SERVER supplies this
	// value whether or not an ansible.cfg was found at all, and crediting the
	// file for it would name a source that did not provide it.
	if cfg.AnsibleServerEnvUsed {
		i.Output.Debugf("env ANSIBLE_GALAXY_SERVER: galaxy.server=%s", cfg.Server)
	}
	i.debugServerList(cfg.Servers)
}

// WarnConfig surfaces non-fatal configuration warnings collected while
// building cfg (e.g. an ignored collections_path entry). Config is built
// before the output printer exists, so these warnings are queued on cfg
// and drained here once a printer is available.
func (i *Infra) WarnConfig(cfg *config.Config) {
	if i == nil || i.Output == nil || cfg == nil {
		return
	}
	for _, w := range cfg.Warnings {
		i.Output.Warnf("%s", w)
	}
}

// debugServerList logs one line per resolved server this run will use: its
// server_list id (empty for the implicit single server), its URL, whether
// it carries a token, and whether TLS certificate verification is disabled
// for it - exactly what an operator debugging "why is it hitting the wrong
// server" needs. The token is rendered as a presence boolean only, never
// through config.Secret.Reveal, so raising verbosity can never turn this
// line into a credential leak.
func (i *Infra) debugServerList(servers []config.Server) {
	for _, s := range servers {
		i.Output.Debugf("galaxy server %q: url=%s token=%t insecure_skip_tls_verify=%t",
			s.ID, s.URL, s.Token.IsSet(), s.InsecureSkipTLSVerify)
	}
}
