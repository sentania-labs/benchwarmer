// Package version carries the build version, set at link time.
package version

// Version is overridden by -ldflags at release build time.
var Version = "dev"
