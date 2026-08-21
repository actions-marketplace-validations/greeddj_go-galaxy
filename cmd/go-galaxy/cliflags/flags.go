// Package cliflags declares the urfave/cli flag sets the commands share, and
// the defaults those flags advertise. It owns the CLI surface only: what a
// flag is named, what it accepts, and which environment variables feed it.
// Turning a parsed command into configuration belongs to
// internal/galaxy/config, which is why nothing here reads a value back.
package cliflags

import (
	"os"
	"path/filepath"
	"runtime"

	galaxyhelpers "github.com/greeddj/go-galaxy/internal/galaxy/helpers"
	"github.com/urfave/cli/v3"
)

// CommonFlags defines shared CLI flags for all commands.
func CommonFlags() []cli.Flag {
	return []cli.Flag{
		&cli.BoolFlag{
			Name:    "verbose",
			Usage:   "Verbose output",
			Sources: cli.EnvVars("GO_GALAXY_VERBOSE"),
		},
		&cli.BoolFlag{
			Name:    "quiet",
			Aliases: []string{"q"},
			Usage:   "Suppress progress and log lines; results, warnings and errors still print. Ignored when --verbose is also set",
			Sources: cli.EnvVars("GO_GALAXY_QUIET"),
		},
		&cli.BoolFlag{
			Name:    "dry-run",
			Usage:   "Enable dry-run mode",
			Sources: cli.EnvVars("GO_GALAXY_DRY_RUN"),
		},
		&cli.StringFlag{
			Name:    "cache-dir",
			Usage:   "Local cache directory",
			Value:   defaultCacheDir(),
			Sources: cli.EnvVars("GO_GALAXY_CACHE_DIR", "ANSIBLE_GALAXY_CACHE_DIR"),
		},
	}
}

// CollectionFlags defines CLI flags for collection install behavior.
func CollectionFlags() []cli.Flag {
	flags := collectionPathFlags()
	flags = append(flags, collectionBehaviorFlags()...)
	flags = append(flags, lockfileAndMetricsFlags()...)
	return flags
}

func collectionPathFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			// ANSIBLE_GALAXY_SERVER is deliberately NOT a source here, and is
			// read in internal/galaxy/config instead. ansible treats it as the
			// env spelling of the [galaxy] server key - the fallback used only
			// when no server_list and no -s apply - while a flag source makes
			// it outrank server_list entirely, since urfave/cli exposes no way
			// to tell a CLI-set flag from an env-set one: Command.IsSet returns
			// FlagBase.hasBeenSet, which Set (CLI parsing) and PostParse (value
			// source) write identically, and ValueSourceChain.LookupWithSource
			// is not reachable through the cli.Flag interface.
			Name:    "server",
			Usage:   "Galaxy server URL",
			Value:   defaultServerURL,
			Sources: cli.EnvVars("GO_GALAXY_SERVER"),
		},
		&cli.StringFlag{
			Name: "token",
			Usage: "Galaxy API token for the configured server; only valid when a single server is in effect " +
				"(configure per-server tokens in [galaxy_server.<id>] when using server_list)",
			Sources: cli.EnvVars("GO_GALAXY_TOKEN"),
		},
		&cli.StringFlag{
			Name: "timeout",
			// Names the semantics, not just the format: this bounds a lack of
			// progress, and an operator who reads it as a cap on the whole
			// transfer sets it far too low for a large collection.
			Usage: "No-progress budget: wait for response headers, and gap between body reads. " +
				"Not a total-transfer cap. Seconds (e.g. 60) or Go duration (e.g. 90s, 1m30s)",
			Value: defaultTimeout.String(),
			// Source order is precedence: urfave/cli takes the first name in
			// the chain that is set, so a name that was already effective
			// keeps it and reordering these is a behavior change rather than
			// a tidy-up. The flag-name-shaped spelling is listed second so
			// that the GO_GALAXY_<FLAG_NAME> form every flag in this file
			// accepts has no exception: behind the name that shipped first,
			// which keeps the precedence it had, and ahead of the ANSIBLE_
			// spelling, which is where every other GO_GALAXY_ name here sits.
			Sources: cli.EnvVars("GO_GALAXY_SERVER_TIMEOUT", "GO_GALAXY_TIMEOUT", "ANSIBLE_GALAXY_SERVER_TIMEOUT"),
		},
		&cli.StringFlag{
			Name:    "download-path",
			Aliases: []string{"p"},
			Usage:   "Path to download collections to",
			Value:   defaultCollectionsPath,
			// Source order is precedence, for the reason given on the timeout
			// flag above.
			Sources: cli.EnvVars("GO_GALAXY_COLLECTIONS_PATH", "GO_GALAXY_DOWNLOAD_PATH", "ANSIBLE_COLLECTIONS_PATH"),
		},
		&cli.StringFlag{
			Name:  "roles-path",
			Usage: "Path to install roles to",
			Value: defaultRolesPath,
			// Source order is precedence, for the reason given on the timeout
			// flag above.
			Sources: cli.EnvVars("GO_GALAXY_ROLES_PATH", "ANSIBLE_ROLES_PATH"),
		},
		&cli.StringFlag{
			Name: "requirements-file",
			// --role-file is ansible-galaxy's own spelling of this flag for
			// the install command; one file names both collections and roles
			// there as it does here.
			Aliases: []string{"r", "role-file"},
			Usage:   "Path to requirements.yml file",
			Value:   defaultRequirementsFilePath,
			Sources: cli.EnvVars("GO_GALAXY_REQUIREMENTS_FILE", envRequirementsFileAnsible),
		},
		&cli.StringFlag{
			Name: "ansible-config",
			Usage: "Path to ansible.cfg file; if unset, discovered in ansible's order " +
				"($ANSIBLE_CONFIG, ./ansible.cfg, ~/.ansible.cfg, /etc/ansible/ansible.cfg)",
			Sources: cli.EnvVars("GO_GALAXY_ANSIBLE_CONFIG"),
		},
	}
}

func collectionBehaviorFlags() []cli.Flag {
	return []cli.Flag{
		&cli.IntFlag{
			Name: "workers",
			Usage: "Number of concurrent workers; accepted from 1 up to the CPU this process may use " +
				"(at least 2), and outside that range the derived default is used instead",
			// Value is load-bearing for an acceptance, not just a default.
			// urfave marks a declared-but-empty env var as set while skipping
			// the parse for it, so a CI block exporting GO_GALAXY_WORKERS=
			// reaches applyWorkers (internal/galaxy/config/config.go) through
			// its IsSet branch and is judged against that branch's range
			// rather than skipped by its gate. What c.Int reads there is this
			// Value, and that it lands inside the range is arithmetic rather
			// than a coincidence of one machine's CPU count. Write p for
			// runtime.GOMAXPROCS(0): the ceiling is
			// MaxAcceptedInstallWorkers(p) = max(p, MinDefaultInstallWorkers),
			// call it X, and this Value is DefaultInstallWorkers(p), which is
			// that very X under a min() with MaxDefaultInstallWorkers - so
			// min(X, MaxDefaultInstallWorkers) <= X puts Value at or below the
			// ceiling for every p. The lower end is that same max() again,
			// with MaxDefaultInstallWorkers sitting above
			// MinDefaultInstallWorkers so the min() cannot cut beneath it:
			// Value is never below MinDefaultInstallWorkers and so never below
			// 1. Remove this field and that same shape reads 0, which is
			// outside the range in the other direction and warns.
			Value:   galaxyhelpers.DefaultInstallWorkers(runtime.GOMAXPROCS(0)),
			Sources: cli.EnvVars("GO_GALAXY_WORKERS"),
		},
		&cli.IntFlag{
			Name:    "download-workers",
			Usage:   "Number of concurrent artifact downloads and cache presence probes; these wait on the network, not the CPU",
			Value:   galaxyhelpers.DefaultDownloadWorkers(runtime.GOMAXPROCS(0)),
			Sources: cli.EnvVars("GO_GALAXY_DOWNLOAD_WORKERS"),
		},
		&cli.BoolFlag{
			Name:    "no-cache",
			Usage:   "Disable local caching",
			Sources: cli.EnvVars("GO_GALAXY_NO_CACHE"),
		},
		&cli.BoolFlag{
			Name: "refresh",
			Usage: "Re-resolve against the Galaxy servers instead of reusing cached metadata or the previous " +
				"resolution; a cached artifact and its own version-specific metadata are still reused even " +
				"if the server changed them - use --no-cache to force those too. Ignored with --offline, " +
				"and wherever resolution comes from the lockfile rather than the servers",
			Sources: cli.EnvVars("GO_GALAXY_REFRESH"),
		},
		&cli.BoolFlag{
			Name:    "clear-cache",
			Usage:   "Clear local cache before installing",
			Sources: cli.EnvVars("GO_GALAXY_CLEAR_CACHE"),
		},
		&cli.BoolFlag{
			Name:    "no-deps",
			Usage:   "Do not install dependencies",
			Sources: cli.EnvVars("GO_GALAXY_NO_DEPS"),
		},
		&cli.BoolFlag{
			Name:    "offline",
			Usage:   "Disallow any network access; only cached state may be used",
			Sources: cli.EnvVars("GO_GALAXY_OFFLINE"),
		},
	}
}

func lockfileAndMetricsFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name:    "lock-file",
			Usage:   "Path to lockfile (default: requirements.lock.yml next to requirements file)",
			Sources: cli.EnvVars("GO_GALAXY_LOCK_FILE"),
		},
		&cli.BoolFlag{
			Name:    "frozen",
			Usage:   "Fail if lockfile is missing or does not match resolved requirements",
			Sources: cli.EnvVars("GO_GALAXY_FROZEN"),
		},
		&cli.StringFlag{
			Name:    "metrics-file",
			Usage:   "Write a JSON metrics report to this path on completion",
			Sources: cli.EnvVars("GO_GALAXY_METRICS_FILE"),
		},
	}
}

// LockInspectFlags defines the two flags shared by the read-only lockfile
// inspection commands (hash, tree, explain): where to find the requirements
// file and, optionally, an override lockfile path.
func LockInspectFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name:    "requirements-file",
			Aliases: []string{"r", "role-file"},
			Usage:   "Path to requirements.yml",
			Value:   defaultRequirementsFilePath,
			Sources: cli.EnvVars("GO_GALAXY_REQUIREMENTS_FILE", envRequirementsFileAnsible),
		},
		&cli.StringFlag{
			Name:    "lock-file",
			Usage:   "Path to lockfile (default: requirements.lock.yml beside requirements file)",
			Sources: cli.EnvVars("GO_GALAXY_LOCK_FILE"),
		},
	}
}

// SignatureFlags defines the CLI flags for collection signature verification.
//
// It is a separate constructor rather than part of CollectionFlags because not
// every collection command can honor these: lock and outdated verify nothing,
// and mounting the flags on them would advertise four settings those commands
// would silently ignore. A command mounts this set exactly when it verifies.
//
// These flags and their environment variables are the whole configuration
// surface for signature verification: ansible.cfg deliberately configures none
// of it, even though ansible itself reads all four settings from a [galaxy]
// section. This program cannot establish whether a discovered ansible.cfg was
// authored by the operator or by the repository under test, and a setting that
// can relax a verification check must not come from a file whose author is
// unknown.
//
// Four shapes make that question undecidable here, and the second is the one
// that reads safe and is not: a repository supplies ./ansible.cfg; a workflow
// setting ANSIBLE_CONFIG normally names a repository-relative path, so the
// operator picks the variable while the checkout picks the file; a repository
// that also supplies the workflow picks both; and on a self-hosted runner a job
// can leave ~/.ansible.cfg behind for every later job, which outlives the
// checkout that dropped it.
//
// Each proxy for the authorship question leaks somewhere different, which is
// why none is used. Keying on the discovery slot misses the second shape above.
// Keying on the resolved path sitting under the working directory misses a
// symlinked target and a run whose working directory is not the checkout.
// Keying on file ownership misses the runner case entirely, since the earlier
// job wrote the file as the same user. An environment variable is not immune to
// a hostile workflow either, but it is set by whoever configured the run rather
// than by whatever the checkout happened to contain.
func SignatureFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name:    "keyring",
			Usage:   "Path to the OpenPGP keyring collection signatures are verified against; unset means no verification",
			Sources: cli.EnvVars("GO_GALAXY_KEYRING", "ANSIBLE_GALAXY_GPG_KEYRING"),
		},
		&cli.StringFlag{
			Name: "required-valid-signature-count",
			Usage: "How many signatures must verify: a non-negative count or 'all', optionally prefixed with '+' " +
				"to also require that at least one signature verified",
			Value: galaxyhelpers.DefaultRequiredValidSignatureCount,
			Sources: cli.EnvVars(
				"GO_GALAXY_REQUIRED_VALID_SIGNATURE_COUNT",
				"ANSIBLE_GALAXY_REQUIRED_VALID_SIGNATURE_COUNT",
			),
		},
		&cli.StringSliceFlag{
			Name:  "ignore-signature-status-code",
			Usage: "Signature failure status code to tolerate (repeatable), e.g. BADSIG or NO_PUBKEY",
			// The go-galaxy name is singular and the ansible one plural, and
			// neither can be made to match the other: the first is derived from
			// this flag's own name, the second is ansible's own variable.
			Sources: cli.EnvVars(
				"GO_GALAXY_IGNORE_SIGNATURE_STATUS_CODE",
				"ANSIBLE_GALAXY_IGNORE_SIGNATURE_STATUS_CODES",
			),
		},
		&cli.BoolFlag{
			// ANSIBLE_GALAXY_DISABLE_GPG_VERIFY is deliberately NOT a source
			// here, and is read in internal/galaxy/config instead, in the
			// precedence slot directly below this flag. urfave/cli parses a bool
			// source with Go's own bool grammar, which rejects the yes/no and
			// on/off spellings ansible accepts - and rejects them by aborting the
			// command, so an environment already exporting one for ansible would
			// make every go-galaxy run fail. See resolveDisableGPGVerify
			// (internal/galaxy/config/signature.go) for the full argument,
			// including why this is the only one of the four that needs it.
			Name:    "disable-gpg-verify",
			Usage:   "Skip signature verification even when a keyring is configured",
			Sources: cli.EnvVars("GO_GALAXY_DISABLE_GPG_VERIFY"),
		},
	}
}

// S3Flags defines CLI flags for S3 cache configuration.
func S3Flags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{
			Name:    "s3-bucket",
			Usage:   "S3 bucket name for caching, if defined enables S3 caching instead of local cache-dir",
			Sources: cli.EnvVars("GO_GALAXY_S3_BUCKET"),
		},
		&cli.StringFlag{
			Name:    "s3-region",
			Usage:   "S3 region for caching",
			Sources: cli.EnvVars("GO_GALAXY_S3_REGION"),
		},
		&cli.StringFlag{
			Name:    "s3-prefix",
			Usage:   "S3 prefix for caching",
			Sources: cli.EnvVars("GO_GALAXY_S3_PREFIX"),
		},
		&cli.StringFlag{
			Name:    "s3-access-key",
			Usage:   "S3 access key for caching",
			Sources: cli.EnvVars("GO_GALAXY_S3_ACCESS_KEY", "AWS_ACCESS_KEY_ID"),
		},
		&cli.StringFlag{
			Name:    "s3-secret-key",
			Usage:   "S3 secret key for caching",
			Sources: cli.EnvVars("GO_GALAXY_S3_SECRET_KEY", "AWS_SECRET_ACCESS_KEY"),
		},
		&cli.StringFlag{
			Name:    "s3-endpoint",
			Usage:   "S3 endpoint for caching",
			Sources: cli.EnvVars("GO_GALAXY_S3_ENDPOINT"),
		},
		&cli.StringFlag{
			Name:    "s3-session-token",
			Usage:   "S3 session token for caching",
			Sources: cli.EnvVars("GO_GALAXY_S3_SESSION_TOKEN", "AWS_SESSION_TOKEN"),
		},
		&cli.BoolFlag{
			Name: "s3-path-style-disabled",
			Usage: "Use virtual-hosted-style S3 addressing (<bucket>.<endpoint>/<key>) " +
				"instead of the default path style (<endpoint>/<bucket>/<key>)",
			Sources: cli.EnvVars("GO_GALAXY_S3_PATH_STYLE_DISABLED"),
		},
	}
}

// defaultCacheDir returns the default cache directory path.
func defaultCacheDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(defaultHomeDir, dirSuffix)
	}
	return filepath.Join(home, dirSuffix)
}
