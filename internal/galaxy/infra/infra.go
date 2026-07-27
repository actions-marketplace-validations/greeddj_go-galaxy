package infra

import (
	"net/http"
	"os"
	"time"

	"github.com/greeddj/go-galaxy/internal/galaxy/config"
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
}

// New builds Infra with default helpers for time and temp paths.
func New(out output.Printer, httpClient *http.Client) *Infra {
	return &Infra{
		Output:  out,
		HTTP:    httpClient,
		Now:     time.Now,
		TempDir: os.TempDir,
		Metrics: &metrics.Counters{},
	}
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
		if cfg.AnsibleServerUsed {
			i.Output.Debugf("ansible.cfg %s: galaxy.server=%s", cfg.AnsibleConfigPath, cfg.Server)
		}
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
