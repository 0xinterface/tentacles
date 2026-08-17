// Package version exposes the gh-runnerd build version. The value is a
// compile-time constant that packaging injects via -ldflags.
package version

// Version is the build version of gh-runnerd. Override at build time:
//
//	go build -ldflags "-X github.com/hkust/gh-runnerd/internal/version.Version=v0.1.0"
var Version = "dev"

// String returns the build version as a plain string.
func String() string { return Version }
