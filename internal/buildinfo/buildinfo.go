// Package buildinfo holds the version stamped into the executable.
package buildinfo

// Version identifies this build in the UI, the API, MCP and diagnostics.
// Release builds override it with
// -ldflags "-X github.com/liljestk/VIbeClean/internal/buildinfo.Version=<version>".
var Version = "1.4.0"
