// agentpkg is the foundation of SPEC-0026 agent package stables: the strict
// package.toml manifest loader, the content-addressed pin store layout, and
// the `source` reference grammar a [harness.*] table carries. The daemon-side
// half is deliberately tiny — resolve `source` from local disk at config load
// and nothing else; every fetch, clone, scan and confirmation lives in the
// `harness agent` CLI tree (ADR-0044).
//
// Governing: ADR-0044 (agent package stables), SPEC-0026 REQ-3 (manifest
// schema), REQ-7 (source field and config-load resolution), Error Handling
// Standards.
package agentpkg

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// Sentinel errors callers distinguish (SPEC-0026 Error Handling Standards).
// Unknown-stable, unknown-package and diverged-clone have no producer in this
// foundation; the stable CLI stories (#811+) return them. They are defined
// here so every story shares one error vocabulary.
var (
	// ErrInvalidSource is wrapped by every parse failure of a `source` value.
	ErrInvalidSource = errors.New("invalid source")
	// ErrMissingPin names a source whose content-addressed directory is not
	// on local disk. Config load turns it into a located error instructing
	// `harness agent install`; the doctor row (#815) detects it outside load.
	ErrMissingPin = errors.New("missing local pin")
	// ErrUnknownStable names a stable absent from [stable.*].
	ErrUnknownStable = errors.New("unknown stable")
	// ErrUnknownPackage names a package absent from a stable's clone.
	ErrUnknownPackage = errors.New("unknown package")
	// ErrManifestViolation names a package.toml schema violation.
	ErrManifestViolation = errors.New("manifest schema violation")
	// ErrDivergedClone names a stable clone that cannot fast-forward.
	ErrDivergedClone = errors.New("diverged stable clone")
	// ErrBlockedFinding names an install or upgrade refused by a
	// high-severity scan finding. Only --force-unsafe with a re-typed
	// <stable>/<package> clears it (SPEC-0026 REQ-5).
	ErrBlockedFinding = errors.New("blocked high-severity finding")
)

// NamePattern is the shared stable and package name grammar (SPEC-0026 REQ-1,
// REQ-2).
var NamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// shaPattern is a full 40-character commit SHA; a pin is never a branch name.
var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Source is a parsed `source = "<stable>/<package>@<sha>"` reference.
type Source struct {
	Stable  string
	Package string
	SHA     string
}

// String reassembles the canonical form.
func (s Source) String() string {
	return s.Stable + "/" + s.Package + "@" + s.SHA
}

// ParseSource validates and splits a source reference. Every failure wraps
// ErrInvalidSource and names the offending part.
func ParseSource(v string) (Source, error) {
	at := lastIndexByte(v, '@')
	if at < 0 {
		return Source{}, fmt.Errorf("%w: %q: want <stable>/<package>@<40-char sha>", ErrInvalidSource, v)
	}
	ref, sha := v[:at], v[at+1:]
	slash := lastIndexByte(ref, '/')
	if slash < 0 {
		return Source{}, fmt.Errorf("%w: %q: want <stable>/<package>@<40-char sha>", ErrInvalidSource, v)
	}
	stable, pkg := ref[:slash], ref[slash+1:]
	if !NamePattern.MatchString(stable) {
		return Source{}, fmt.Errorf("%w: stable name %q must match %s", ErrInvalidSource, stable, NamePattern)
	}
	if !NamePattern.MatchString(pkg) {
		return Source{}, fmt.Errorf("%w: package name %q must match %s", ErrInvalidSource, pkg, NamePattern)
	}
	if !shaPattern.MatchString(sha) {
		return Source{}, fmt.Errorf("%w: pin %q must be a full 40-character commit sha, never a branch or tag", ErrInvalidSource, sha)
	}
	return Source{Stable: stable, Package: pkg, SHA: sha}, nil
}

func lastIndexByte(s string, b byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// StateHome mirrors supervisor.StateHome without importing it (that would
// cycle through the daemon; internal/protocol/socket.go does the same). Tests
// point XDG_STATE_HOME at a temp dir.
func StateHome() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return filepath.Join(".", "harness")
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "harness")
}

// InstalledRoot is the content-addressed pin store,
// $XDG_STATE_HOME/harness/agents/installed/ (SPEC-0026 REQ-6).
func InstalledRoot() string { return filepath.Join(StateHome(), "agents", "installed") }

// StableRoot is the local-clone store for added stables,
// $XDG_STATE_HOME/harness/agents/stables/ (SPEC-0026 REQ-1).
func StableRoot() string { return filepath.Join(StateHome(), "agents", "stables") }

// StableDir is a named stable's local clone.
func StableDir(name string) string { return filepath.Join(StableRoot(), name) }

// PinDir is the immutable directory holding one package at one exact commit.
// Entries are written once by install and never rewritten.
func PinDir(s Source) string {
	return filepath.Join(InstalledRoot(), s.Stable, s.Package, s.SHA)
}

// ManifestPath is package.toml inside a pin directory.
func ManifestPath(s Source) string { return filepath.Join(PinDir(s), "package.toml") }

// InstalledPackageDir turns a harness's PackageSource string into the
// directory the installed package's bundled files live in. It is what the run
// context's `{{package.dir}}` renders to, so a package can name a file it
// ships without hard-coding the content SHA, which breaks on every upgrade. A
// harness with no source, or a source string that does not parse, yields false
// and the caller says so: no path is invented. Config load has already refused
// a pin absent from disk (SPEC-0026 REQ-7), so this performs no disk access.
// PackageDir is the same idea on the other side of install: a package folder
// inside a stable's clone.
//
// Governing: ADR-0044; SPEC-0026 REQ-7; SPEC-0017 REQ-7 "Template Context".
func InstalledPackageDir(packageSource string) (string, bool) {
	if packageSource == "" {
		return "", false
	}
	src, err := ParseSource(packageSource)
	if err != nil {
		return "", false
	}
	return PinDir(src), true
}
