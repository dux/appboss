// Package version identifies the running binary for metrics and `dboss version`.
package version

// Version is the build version: v<number of commits in main>, injected by `make build` and by
// the release workflow with -ldflags "-X dboss/internal/version.Version=v81". A plain
// `go build` leaves it at "dev", which is what marks a from-source binary.
var Version = "dev"

// Dev is the version a build without the ldflag reports.
const Dev = "dev"

// String returns the build version.
func String() string { return Version }
