// Package version exposes the build version injected at link time.
package version

// Version is set with -ldflags "-X github.com/mondeep31/agent-comp/internal/version.Version=...".
var Version = "dev"
