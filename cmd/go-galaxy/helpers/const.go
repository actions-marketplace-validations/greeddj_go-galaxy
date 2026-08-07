package helpers

import (
	galaxyhelpers "github.com/greeddj/go-galaxy/internal/galaxy/helpers"
)

const (
	dirSuffix      = ".cache/go-galaxy"
	defaultHomeDir = "/root"
	// defaultTimeout is only what --timeout advertises as its default; the
	// value the config layer actually falls back to is
	// galaxyhelpers.FetchDefaultTimeout, so the flag's help text is derived
	// from that same constant rather than restating it.
	defaultTimeout              = galaxyhelpers.FetchDefaultTimeout
	defaultServerURL            = "https://galaxy.ansible.com"
	defaultCollectionsPath      = ".collections"
	defaultRequirementsFilePath = "requirements.yml"
	// envRequirementsFileAnsible occupies ansible's namespace without being a
	// name ansible defines. The prefix promises drop-in fidelity, and this one
	// has nothing to be faithful to: ansible-core declares no
	// requirements-file option at all, and ansible-galaxy takes that path only
	// as -r/--role-file. It is kept
	// regardless: pipelines already set it, and dropping it would not fail
	// them, it would silently install whatever requirements.yml the working
	// directory happens to hold. Nor could a run warn about the change, since
	// hash, tree and explain never build a *config.Config to warn from. So it
	// is documented as an extension rather than as parity, and a later reader
	// must not "restore parity" by deleting it - there is no parity to
	// restore.
	envRequirementsFileAnsible = "ANSIBLE_GALAXY_REQUIREMENTS_FILE"
	// defaultVersion is used only when neither ldflags nor build info supply
	// a version (e.g. a build without module/VCS info embedded).
	defaultVersion = "unknown"
	defaultBuilder = "go"
)
