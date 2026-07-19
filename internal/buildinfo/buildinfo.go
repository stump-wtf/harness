// Package buildinfo carries version metadata shared by both binaries.
//
// Governing: ADR-0001 (Fang gives an automatic --version once the CLI is
// fleshed out; this is the value it reports). Version is overridable at link
// time with -ldflags "-X .../internal/buildinfo.Version=v1.2.3".
package buildinfo

// Version is the build version; "dev" for unreleased builds.
var Version = "dev"
