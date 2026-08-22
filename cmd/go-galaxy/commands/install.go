package commands

import (
	"context"
	"net/http"
	"net/url"

	"github.com/greeddj/go-galaxy/cmd/go-galaxy/cliflags"
	"github.com/greeddj/go-galaxy/internal/galaxy/collections"
	"github.com/greeddj/go-galaxy/internal/galaxy/config"
	"github.com/greeddj/go-galaxy/internal/galaxy/fetch"
	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/urfave/cli/v3"
)

// Install returns the CLI command that installs collections from requirements.
func Install() *cli.Command {
	flags := cliflags.CollectionFlags()
	flags = append(flags, cliflags.SignatureFlags()...)
	flags = append(flags, cliflags.S3Flags()...)

	return &cli.Command{
		Name:    "install",
		Aliases: []string{"i"},
		Usage:   "Install collections from requirements file",
		Flags:   flags,
		Action: func(ctx context.Context, c *cli.Command) error {
			return runCollectionCommand(ctx, c, collections.Start)
		},
	}
}

// newHTTPClient builds an HTTP client honoring offline mode, wiring each
// configured server's token and TLS policy into the transport chain so a
// request only ever carries a credential, or skips certificate verification,
// for the exact origin that server was configured for.
func newHTTPClient(cfg *config.Config) *http.Client {
	if cfg != nil && cfg.Offline {
		return fetch.NewOffline(cfg.Timeout)
	}
	return fetch.New(cfg.Timeout, serverAuths(cfg.Servers))
}

// serverAuths converts cfg.Servers into fetch's own ServerAuth view. This is
// the only call site of config.Secret.Reveal() for a Galaxy token: fetch
// cannot import config (config is a layer above it) and so cannot hold a
// Secret itself, only the plain token string handed to it once, here, at
// client-construction time.
//
// It satisfies the rule every Reveal call site is bound by - the plaintext is
// taken only where it is going onto the wire in that same statement, here an
// Authorization header - which is a predicate rather than a headcount. The S3
// cache client meets the same rule twice for its own credentials.
func serverAuths(servers []config.Server) []fetch.ServerAuth {
	auths := make([]fetch.ServerAuth, 0, len(servers))
	for _, s := range servers {
		parsed, err := url.Parse(s.URL)
		if err != nil {
			// s.URL was already normalized and validated by config; an
			// already-valid absolute URL string always reparses cleanly, so
			// this only guards against a future change to that invariant
			// rather than a case reachable today.
			continue
		}
		auths = append(auths, fetch.ServerAuth{
			Origin:      helpers.Origin(parsed),
			Token:       s.Token.Reveal(),
			InsecureTLS: s.InsecureSkipTLSVerify,
		})
	}
	return auths
}

// gitCredentials converts cfg.GitCredentials into gitsource's plain
// Credential view. This is the only call site of config.Secret.Reveal() for a
// git credential: gitsource sits below config and cannot hold a Secret, so
// the password, the key and its passphrase are handed over in the clear once,
// here, and the fetcher offers them to the remote as they are.
//
// It satisfies the same rule serverAuths does - the plaintext is taken only
// to build the value that goes onto the wire - and the result must never be
// printed, logged or persisted; gitsource.Credential's own doc comment
// states that nothing renders one.
func gitCredentials(cfg *config.Config) []gitsource.Credential {
	if cfg == nil {
		return nil
	}
	creds := make([]gitsource.Credential, 0, len(cfg.GitCredentials))
	for _, c := range cfg.GitCredentials {
		creds = append(creds, gitsource.Credential{
			URL:           c.URL,
			Username:      c.Username,
			Password:      c.Password.Reveal(),
			SSHKey:        []byte(c.SSHKeyPEM.Reveal()),
			SSHPassphrase: c.SSHPassphrase.Reveal(),
		})
	}
	return creds
}

// urlBindings converts cfg.URLCredentials into fetch's URLBinding view. This
// is the only call site of config.Secret.Reveal() for a url token, and it
// satisfies the same rule serverAuths and gitCredentials do: the plaintext
// is taken only to build the value the transport puts on the wire. The
// binding's origin comes from urlsource.Prefix.Origin(), which renders it
// exactly as helpers.Origin renders a request URL's, so the transport's
// match is a byte comparison.
func urlBindings(cfg *config.Config) []fetch.URLBinding {
	if cfg == nil {
		return nil
	}
	bindings := make([]fetch.URLBinding, 0, len(cfg.URLCredentials))
	for _, c := range cfg.URLCredentials {
		bindings = append(bindings, fetch.URLBinding{
			Origin:     c.URL.Origin(),
			PathPrefix: c.URL.Path,
			Token:      c.Token.Reveal(),
		})
	}
	return bindings
}
