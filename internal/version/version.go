// Package version identifies the running binary for metrics and `dboss version`.
package version

import "strings"

// Version is the build version: v<number of commits in main>, injected by `make build` and by
// the release workflow with -ldflags "-X dboss/internal/version.Version=v81". A plain
// `go build` leaves it at "dev", which is what marks a from-source binary.
var Version = Dev

// Dev is the version a build without the ldflag reports.
const Dev = "dev"

// String returns the build version rendered as v<a>.<b>.<c>, where b and c are the last two digits
// of the commit count and a is everything before them: v123 -> v1.2.3, v1123 -> v11.2.3. A shorter
// count is left-padded, so v5 is v0.0.5.
func String() string { return Format(Version) }

// Format renders a v<digits> version string as v<a>.<b>.<c>; anything else (dev, a dotted tag) is
// returned unchanged.
func Format(version string) string {
	count, ok := strings.CutPrefix(version, "v")
	if !ok || count == "" {
		return version
	}
	for _, digit := range count {
		if digit < '0' || digit > '9' {
			return version
		}
	}
	for len(count) < 3 {
		count = "0" + count
	}
	return "v" + count[:len(count)-2] + "." + count[len(count)-2:len(count)-1] + "." + count[len(count)-1:]
}
