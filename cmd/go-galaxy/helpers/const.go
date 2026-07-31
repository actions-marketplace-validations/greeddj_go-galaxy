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
	// defaultVersion is used only when neither ldflags nor build info supply
	// a version (e.g. a build without module/VCS info embedded).
	defaultVersion = "unknown"
	defaultBuilder = "go"
)
