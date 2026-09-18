// Package version identifies the running binary for metrics and `appboss version`.
package version

import "runtime/debug"

// Version is the release version. A release build sets it with
// -ldflags "-X app-boss/internal/version.Version=v0.1.0".
var Version = "dev"

// String returns the injected version, or the module version or short VCS revision from the
// build, so a from-source binary still identifies itself.
func String() string {
	if Version != "dev" {
		return Version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		if info.Main.Version != "" && info.Main.Version != "(devel)" {
			return info.Main.Version
		}
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" && len(setting.Value) >= 7 {
				return setting.Value[:7]
			}
		}
	}
	return Version
}
