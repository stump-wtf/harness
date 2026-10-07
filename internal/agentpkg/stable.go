// Stable clone management and package discovery: the git half of SPEC-0026
// REQ-1 and REQ-2. Every function here runs in the CLI process only — the
// daemon never clones, fetches or reaches a stable's remote (ADR-0044).
// `stable add` clones through a temp directory and renames it into place so
// a failed clone leaves no half-stable behind; `stable update` is the one
// operation that fetches, and it fast-forwards or fails without touching the
// checkout. Discovery reads the clone as the last update left it and never
// touches the network.
//
// Remotes carry credentials, so every error message built here passes git
// output and the remote through redact.String before it can reach an
// operator (SPEC-0026 Error Handling Standards).
//
// Governing: ADR-0044 (agent package stables), SPEC-0026 REQ-1 (stable
// registration), REQ-2 (stable layout and local discovery), Error Handling
// Standards.
//
// @joestump-agent 10/02/2026 - Added for harness#811.
package agentpkg

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/stump-wtf/agent-trace/redact"
	"github.com/stump-wtf/harness/internal/gitcmd"
)

// PackagesDir is the directory inside a stable's clone holding its packages.
func PackagesDir(stable string) string {
	return filepath.Join(StableDir(stable), "packages")
}

// PackageDir is one package's directory inside a stable's clone.
func PackageDir(stable, pkg string) string {
	return filepath.Join(PackagesDir(stable), pkg)
}

// CloneStable clones remote into StableDir(name), replacing any directory
// already there. The clone lands in a temp directory first and is renamed
// into place only after it succeeds, so a failed clone leaves no
// stables/<name>/ directory behind (SPEC-0026 REQ-1: nothing is trusted
// until the clone exists). The returned head is the cloned commit.
func CloneStable(name, remote string) (string, error) {
	if !NamePattern.MatchString(name) {
		return "", fmt.Errorf("agentpkg: stable name %q must match %s", name, NamePattern)
	}
	if strings.TrimSpace(remote) == "" {
		return "", fmt.Errorf("agentpkg: stable %q: remote is required", name)
	}
	if err := os.MkdirAll(StableRoot(), 0o755); err != nil {
		return "", fmt.Errorf("agentpkg: create %s: %w", StableRoot(), err)
	}

	tmp, err := os.MkdirTemp(StableRoot(), ".clone-*")
	if err != nil {
		return "", fmt.Errorf("agentpkg: temp dir for stable %q: %w", name, err)
	}
	if out, err := gitcmd.Run(tmp, "clone", remote, tmp); err != nil {
		os.RemoveAll(tmp)
		return "", fmt.Errorf("agentpkg: clone stable %q from %s: %v: %s",
			name, redact.Redact(remote), err, redact.Redact(out))
	}
	head, err := gitcmd.Output(tmp, "rev-parse", "HEAD")
	if err != nil {
		os.RemoveAll(tmp)
		return "", fmt.Errorf("agentpkg: clone stable %q: %w", name, err)
	}
	if err := os.RemoveAll(StableDir(name)); err != nil {
		os.RemoveAll(tmp)
		return "", fmt.Errorf("agentpkg: replace stable %q: %w", name, err)
	}
	if err := os.Rename(tmp, StableDir(name)); err != nil {
		os.RemoveAll(tmp)
		return "", fmt.Errorf("agentpkg: place stable %q: %w", name, err)
	}
	return head, nil
}

// UpdateResult is the outcome of updating one stable's clone.
type UpdateResult struct {
	// Stable is the stable's name.
	Stable string
	// FastForwarded is true when HEAD advanced to a new commit.
	FastForwarded bool
	// Head is the commit HEAD names after the update.
	Head string
	// DefaultBranch is the clone's recorded default branch.
	DefaultBranch string
}

// UpdateStable fetches the stable's remote and fast-forwards its clone. It
// is the only operation in the tree that fetches (SPEC-0026 REQ-1). A
// non-fast-forward state fails with ErrDivergedClone before the checkout is
// touched: HEAD, the working tree and the index are left exactly as they
// were. A dirty clone is refused the same way.
func UpdateStable(name string) (UpdateResult, error) {
	res := UpdateResult{Stable: name}
	dir := StableDir(name)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return res, fmt.Errorf("agentpkg: stable %q: no clone at %s (run `harness agent stable add`): %w", name, dir, err)
	}

	def, err := defaultBranch(dir)
	if err != nil {
		return res, fmt.Errorf("agentpkg: stable %q: %w", name, err)
	}
	res.DefaultBranch = def
	if out, err := gitcmd.Run(dir, "fetch", "origin"); err != nil {
		return res, fmt.Errorf("agentpkg: fetch stable %q: %v: %s", name, err, redact.Redact(out))
	}

	head, err := gitcmd.Output(dir, "rev-parse", "HEAD")
	if err != nil {
		return res, fmt.Errorf("agentpkg: stable %q: %w", name, err)
	}
	remote, err := gitcmd.Output(dir, "rev-parse", "origin/"+def)
	if err != nil {
		return res, fmt.Errorf("agentpkg: stable %q: %w", name, err)
	}
	res.Head = head
	if head == remote {
		return res, nil
	}

	// Divergence is detected before anything mutates the checkout, so a
	// refusal leaves HEAD, the working tree and the index untouched.
	if out, err := gitcmd.Run(dir, "merge-base", "--is-ancestor", head, "origin/"+def); err != nil {
		var exitErr interface{ ExitCode() int }
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return res, fmt.Errorf("%w: stable %q: HEAD %s is not an ancestor of origin/%s %s; the remote history was rewritten",
				ErrDivergedClone, name, shortSHA(head), def, shortSHA(remote))
		}
		return res, fmt.Errorf("agentpkg: stable %q: check divergence: %v: %s", name, err, redact.Redact(out))
	}
	if status, err := gitcmd.Output(dir, "status", "--porcelain"); err != nil {
		return res, fmt.Errorf("agentpkg: stable %q: %w", name, err)
	} else if status != "" {
		return res, fmt.Errorf("%w: stable %q: the clone's working tree is dirty; refusing to fast-forward",
			ErrDivergedClone, name)
	}
	if out, err := gitcmd.Run(dir, "merge", "--ff-only", "origin/"+def); err != nil {
		return res, fmt.Errorf("agentpkg: fast-forward stable %q: %v: %s", name, err, redact.Redact(out))
	}
	res.Head = remote
	res.FastForwarded = true
	return res, nil
}

// CloneHead reports the commit a stable's clone has checked out, read-only.
func CloneHead(name string) (string, error) {
	return gitcmd.Output(StableDir(name), "rev-parse", "HEAD")
}

// Package is one package discovered in a stable's local clone. Manifest is
// nil when the package's manifest failed to load; Err carries that failure.
// A bad package never fails a whole discovery run (SPEC-0026 REQ-2).
type Package struct {
	// Stable is the stable the package was found in.
	Stable string
	// Name is the package directory's name.
	Name string
	// Manifest is the loaded package.toml, nil when Err is set.
	Manifest *Manifest
	// Err is why the manifest could not be loaded, nil when it was.
	Err error
}

// ListPackages reads every package under a stable's clone, from local disk
// only. An empty or missing packages/ directory is not an error; it simply
// yields zero packages.
func ListPackages(stable string) ([]Package, error) {
	root := PackagesDir(stable)
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("agentpkg: list packages of stable %q: %w", stable, err)
	}
	var out []Package
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		pkg := Package{Stable: stable, Name: name}
		if !NamePattern.MatchString(name) {
			pkg.Err = fmt.Errorf("package directory %q must match %s", name, NamePattern)
			out = append(out, pkg)
			continue
		}
		man, err := LoadManifest(filepath.Join(root, name, "package.toml"))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				err = fmt.Errorf("%w: no package.toml in %s", ErrUnknownPackage, filepath.Join(root, name))
			}
			pkg.Err = err
		} else {
			pkg.Manifest = man
		}
		out = append(out, pkg)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// LoadPackage reads one package's manifest from its stable's local clone,
// without installing anything (SPEC-0026 REQ-2).
func LoadPackage(stable, pkg string) (*Manifest, error) {
	if !NamePattern.MatchString(stable) {
		return nil, fmt.Errorf("%w: stable name %q must match %s", ErrInvalidSource, stable, NamePattern)
	}
	if !NamePattern.MatchString(pkg) {
		return nil, fmt.Errorf("%w: package name %q must match %s", ErrInvalidSource, pkg, NamePattern)
	}
	man, err := LoadManifest(filepath.Join(PackageDir(stable, pkg), "package.toml"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: package %q has no package.toml in stable %q's clone", ErrUnknownPackage, pkg, stable)
		}
		return nil, fmt.Errorf("agentpkg: load package %s/%s: %w", stable, pkg, err)
	}
	return man, nil
}

// BundledFiles lists the package's bundled files as slash-separated paths
// relative to its package directory, sorted. It reads the local clone only.
func BundledFiles(stable, pkg string) ([]string, error) {
	files, err := BundledFilesIn(PackageDir(stable, pkg))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: package %q is not present in stable %q's clone", ErrUnknownPackage, pkg, stable)
		}
		return nil, fmt.Errorf("agentpkg: list files of %s/%s: %w", stable, pkg, err)
	}
	return files, nil
}

// defaultBranch reads the clone's recorded default branch (origin/HEAD),
// falling back to "main".
func defaultBranch(dir string) (string, error) {
	ref, err := gitcmd.Output(dir, "symbolic-ref", "--short", "refs/remotes/origin/HEAD")
	if err != nil || ref == "" {
		return "main", nil
	}
	return strings.TrimPrefix(ref, "origin/"), nil
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// DefaultTip reports the default branch's tip commit in the stable's local
// clone, as of its last stable update — a read-only rev-parse, never a
// fetch (SPEC-0026 REQ-12: informational staleness only).
func DefaultTip(name string) (string, error) {
	dir := StableDir(name)
	def, err := defaultBranch(dir)
	if err != nil {
		return "", err
	}
	return gitcmd.Output(dir, "rev-parse", "--verify", "origin/"+def+"^{commit}")
}

// NewerThanPin reports whether the clone's default-branch tip is a commit
// the pin does not already name: the tip differs from the pin and the pin
// is its ancestor. Read-only, never a fetch; a missing clone answers false.
func NewerThanPin(name, pinSHA string) bool {
	tip, err := DefaultTip(name)
	if err != nil || tip == pinSHA {
		return false
	}
	dir := StableDir(name)
	out, err := gitcmd.Run(dir, "merge-base", "--is-ancestor", pinSHA, tip)
	if err != nil {
		// Exit 1 means the histories diverged rather than the clone moving
		// ahead: not "a newer version available" either way.
		_ = out
		return false
	}
	return true
}

// BundleSkillsDir returns the installed pin's bundled skills directory when
// the harness's pinned manifest requested it — `[requests].skill_paths =
// true` (SPEC-0026 REQ-10). It is the merge engine's lowest tier: only the
// skills bundled at that exact pinned commit, only for a harness whose
// effective configuration carries a source, and never for a manifest that
// left the request unset or false. A harness without a source, a missing
// pin or a missing manifest yields false, and the merge behaves exactly as
// SPEC-0006 defines it.
func BundleSkillsDir(packageSource string) (string, bool) {
	if packageSource == "" {
		return "", false
	}
	src, err := ParseSource(packageSource)
	if err != nil {
		return "", false
	}
	man, err := LoadManifest(ManifestPath(src))
	if err != nil {
		return "", false
	}
	if man.Requests.SkillPaths == nil || !*man.Requests.SkillPaths {
		return "", false
	}
	return filepath.Join(PinDir(src), "skills"), true
}
