package gitfetch

import (
	"fmt"
	"sync"

	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gogitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"

	"github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

const (
	protocolFile  = "file"
	protocolGit   = "git"
	protocolHTTP  = "http"
	protocolHTTPS = "https"
	protocolSSH   = "ssh"
)

// hardenOnce guards the one-time edit of go-git's process-global state. The
// registry and the ssh config reader are globals by go-git's design, so the
// guard has to be one too; every Fetcher runs it from its constructor and the
// edit is idempotent.
//
//nolint:gochecknoglobals // go-git's transport registry is process-global; this Once is its single editor
var hardenOnce sync.Once

// harden removes the two transports this tool never uses from go-git's
// registry and stops go-git from consulting ~/.ssh/config. The file transport
// execs git-upload-pack, which would break the no-external-process property
// for any URL that reached it; the git transport is unauthenticated plaintext
// TCP. Both are already refused by gitsource.ParseURL, and this is the second
// lock on the same door. The ssh config rewrite is switched off because a
// Hostname or Port entry in a developer's file would silently redirect which
// host a run connects to and verifies the host key of.
func harden() {
	hardenOnce.Do(func() {
		client.InstallProtocol(protocolFile, nil)
		client.InstallProtocol(protocolGit, nil)
		gogitssh.DefaultSSHConfig = nil
	})
}

// transportFor picks the transport for an endpoint. http and https run on
// this Fetcher's own client, wrapped by go-git's smart-HTTP transport; ssh
// runs on go-git's default ssh transport, whose auth and host-key policy are
// set per session by authFor. Anything else is refused, which cannot happen
// after gitsource.ParseURL but is checked rather than assumed.
func (f *Fetcher) transportFor(ep *transport.Endpoint) (transport.Transport, error) {
	switch ep.Protocol {
	case protocolHTTP, protocolHTTPS:
		return githttp.NewClient(f.httpClient), nil
	case protocolSSH:
		return gogitssh.DefaultClient, nil
	default:
		return nil, fmt.Errorf("%w: protocol %q is not one this tool speaks", helpers.ErrInvalidGitURL, ep.Protocol)
	}
}
