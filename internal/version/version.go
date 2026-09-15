// Package version exposes the tentacles build version.
package version

import "runtime/debug"

// Version overrides the module version when set at link time.
var Version = "dev"

// String returns the link-time override, module version, or "dev" for a
// local build.
func String() string {
	if Version != "dev" {
		return Version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" || info.Main.Version == "(devel)" {
		return Version
	}
	return info.Main.Version
}
