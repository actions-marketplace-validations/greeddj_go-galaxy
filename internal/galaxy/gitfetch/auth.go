package gitfetch

import (
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gogitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/greeddj/go-galaxy/internal/galaxy/gitsource"
	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

const sshDefaultPort = "22"

// authFor builds the go-git auth method for u from the credential bound to it.
// Over http(s) a bound username and password become Basic auth (GitHub and
// GitLab take a token as the Basic password, never as a Bearer token) and no
// binding means an anonymous session. Over ssh a bound key is parsed into a
// signer, and no binding means the agent SSH_AUTH_SOCK names; neither is
// helpers.ErrGitSSHNoCredential. Every ssh method is wrapped so the dial is
// bounded and the host key is checked against known_hosts with the algorithm
// list that database holds for the host.
func authFor(u gitsource.URL, cred gitsource.Credential) (transport.AuthMethod, error) {
	switch u.Scheme {
	case protocolHTTP, protocolHTTPS:
		if cred.Password == "" {
			return nil, nil //nolint:nilnil // nil auth is go-git's spelling of an anonymous session
		}
		return &githttp.BasicAuth{Username: cred.Username, Password: cred.Password}, nil
	case protocolSSH:
		return sshAuthFor(u, cred)
	default:
		return nil, fmt.Errorf("%w: scheme %q", helpers.ErrInvalidGitURL, u.Scheme)
	}
}

func sshAuthFor(u gitsource.URL, cred gitsource.Credential) (transport.AuthMethod, error) {
	var (
		method interface {
			gogitssh.AuthMethod
			helper() *gogitssh.HostKeyCallbackHelper
		}
		err error
	)
	if len(cred.SSHKey) > 0 {
		keys, keyErr := gogitssh.NewPublicKeys(u.User, cred.SSHKey, cred.SSHPassphrase)
		if keyErr != nil {
			return nil, fmt.Errorf("%w: the ssh key bound to %s does not parse: %w",
				helpers.ErrGitCredentialInvalid, u.Origin(), keyErr)
		}
		method = publicKeys{keys}
	} else {
		agent, agentErr := gogitssh.NewSSHAgentAuth(u.User)
		if agentErr != nil {
			return nil, fmt.Errorf("%w: no key is bound for %s and no agent answers on SSH_AUTH_SOCK: %w",
				helpers.ErrGitSSHNoCredential, u.Origin(), agentErr)
		}
		method = agentKeys{agent}
	}
	db, err := gogitssh.NewKnownHostsDb()
	if err != nil {
		return nil, fmt.Errorf("%w: loading known_hosts: %w", helpers.ErrGitTransportFailed, err)
	}
	port := u.Port
	if port == "" {
		port = sshDefaultPort
	}
	hostWithPort := net.JoinHostPort(strings.Trim(u.Host, "[]"), port)
	h := method.helper()
	h.HostKeyCallback = db.HostKeyCallback()
	h.HostKeyAlgorithms = db.HostKeyAlgorithms(hostWithPort)
	return timedAuth{AuthMethod: method}, nil
}

// publicKeys and agentKeys expose the embedded HostKeyCallbackHelper of the
// two go-git auth types through one accessor, so sshAuthFor can set the
// host-key policy on either without repeating itself.
type publicKeys struct{ *gogitssh.PublicKeys }

func (p publicKeys) helper() *gogitssh.HostKeyCallbackHelper {
	return &p.HostKeyCallbackHelper
}

type agentKeys struct{ *gogitssh.PublicKeysCallback }

func (a agentKeys) helper() *gogitssh.HostKeyCallbackHelper {
	return &a.HostKeyCallbackHelper
}

// timedAuth bounds the ssh dial. go-git dials with context.Background and
// only the client config's Timeout, so without this a black-holed host would
// be bounded by nothing but the operating system's connect timeout.
type timedAuth struct {
	gogitssh.AuthMethod
}

func (a timedAuth) ClientConfig() (*ssh.ClientConfig, error) {
	cfg, err := a.AuthMethod.ClientConfig()
	if err != nil {
		return nil, err
	}
	cfg.Timeout = helpers.FetchDialContextTimeout
	return cfg, nil
}

// classifyTransportError maps a go-git or ssh failure onto this tool's two
// wire sentinels. A refused credential (401, 403, an ssh authentication
// failure) and a host key known_hosts does not vouch for are
// helpers.ErrGitAuthFailed; an empty remote is helpers.ErrGitRefNotFound,
// since it advertises nothing to resolve; a repository the remote does not
// know is helpers.ErrGitTransportFailed naming that (GitHub answers 404 for a
// private repository and a missing one alike, so it is not an auth verdict);
// everything else is helpers.ErrGitTransportFailed. A context error is
// returned unchanged so a deadline or an interrupt keeps its own class.
func classifyTransportError(err error, display string) error {
	if err == nil {
		return nil
	}
	if isContextError(err) {
		return err
	}
	var keyErr *knownhosts.KeyError
	switch {
	case errors.Is(err, transport.ErrAuthenticationRequired), errors.Is(err, transport.ErrAuthorizationFailed):
		return fmt.Errorf("%w: %s: %w", helpers.ErrGitAuthFailed, display, err)
	case errors.As(err, &keyErr):
		return fmt.Errorf("%w: %s: host key is not vouched for by known_hosts: %w", helpers.ErrGitAuthFailed, display, err)
	case strings.Contains(err.Error(), "unable to authenticate"):
		return fmt.Errorf("%w: %s: %w", helpers.ErrGitAuthFailed, display, err)
	case errors.Is(err, transport.ErrEmptyRemoteRepository):
		return fmt.Errorf("%w: %s advertises no refs", helpers.ErrGitRefNotFound, display)
	case errors.Is(err, transport.ErrRepositoryNotFound):
		return fmt.Errorf("%w: %s: repository not found (or not readable with this credential)", helpers.ErrGitTransportFailed, display)
	default:
		return fmt.Errorf("%w: %s: %w", helpers.ErrGitTransportFailed, display, err)
	}
}
