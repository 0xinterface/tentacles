// Package version exposes the tentacles build version. The value is a
// compile-time constant that packaging injects via -ldflags.
package version

// Version is the build version of tentacles. Override at build time:
//
//	go build -ldflags "-X github.com/0xinterface/tentacles/internal/version.Version=v0.1.0"
var Version = "dev"

// String returns the build version as a plain string.
func String() string { return Version }
