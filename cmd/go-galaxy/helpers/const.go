package helpers

import "time"

const (
	dirSuffix                   = ".cache/go-galaxy"
	defaultHomeDir              = "/root"
	defaultTimeout              = 30 * time.Second
	defaultServerURL            = "https://galaxy.ansible.com"
	defaultCollectionsPath      = ".collections"
	defaultRequirementsFilePath = "requirements.yml"
	// defaultVersion is used only when neither ldflags nor build info supply
	// a version (e.g. a build without module/VCS info embedded).
	defaultVersion = "unknown"
	defaultBuilder = "go"
)
