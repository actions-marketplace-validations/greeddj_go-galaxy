module github.com/greeddj/go-galaxy

go 1.26.5

require (
	github.com/Masterminds/semver/v3 v3.4.0
	github.com/briandowns/spinner v1.23.2
	github.com/klauspost/pgzip v1.2.6
	github.com/psvmcc/hub v0.0.12
	github.com/urfave/cli/v3 v3.10.1
	go.etcd.io/bbolt v1.5.0
	go.yaml.in/yaml/v3 v3.0.5
)

require (
	github.com/BurntSushi/toml v1.6.0 // indirect
	github.com/fatih/color v1.19.0 // indirect
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/klauspost/compress v1.19.1 // indirect
	github.com/kr/pretty v0.3.1 // indirect
	github.com/mattn/go-colorable v0.1.15 // indirect
	github.com/mattn/go-isatty v0.0.23 // indirect
	github.com/rogpeppe/go-internal v1.15.0 // indirect
	golang.org/x/exp/typeparams v0.0.0-20260718201538-764159d718ef // indirect
	golang.org/x/mod v0.38.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/telemetry v0.0.0-20260717140457-bdb89881bb75 // indirect
	golang.org/x/term v0.45.0 // indirect
	golang.org/x/tools v0.48.0 // indirect
	golang.org/x/vuln v1.6.0 // indirect
	gopkg.in/check.v1 v1.0.0-20201130134442-10cb98267c6c // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	honnef.co/go/tools v0.7.0 // indirect
)

tool (
	golang.org/x/tools/go/analysis/passes/fieldalignment/cmd/fieldalignment
	golang.org/x/vuln/cmd/govulncheck
	honnef.co/go/tools/cmd/staticcheck
)
