package buildinfo

const (
	// defaultVersion is used only when neither ldflags nor build info supply
	// a version (e.g. a build without module/VCS info embedded).
	defaultVersion = "unknown"
	defaultBuilder = "go"
)
