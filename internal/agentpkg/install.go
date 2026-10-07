// Install materialization: resolve a version in the stable's local clone to
// a full pin, extract the package at that commit by reading git objects
// (git archive — the clone's working tree is never checked out or touched),
// and place the result in the immutable, content-addressed store. An
// existing pin directory is already materialized and is never rewritten.
//
// Governing: ADR-0044 (agent package stables), SPEC-0026 REQ-6 (install:
// resolution and pinning), Error Handling Standards.
//
// @joestump-agent 10/02/2026 - Added for harness#813.
package agentpkg

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/stump-wtf/agent-trace/redact"
	"github.com/stump-wtf/harness/internal/gitcmd"
)

// shaRe matches a full 40-hex commit SHA.
var shaRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// ResolvePin resolves version ("" for the default branch tip) in stable's
// local clone to a full Source whose SHA is a 40-hex commit id, never a
// branch or tag name. A full SHA whose pin is already materialized in the
// store is returned without consulting the clone at all. Nothing here
// fetches (SPEC-0026 REQ-6): the clone answers as the last
// `stable update` left it.
func ResolvePin(stable, pkg, version string) (Source, error) {
	if !NamePattern.MatchString(stable) {
		return Source{}, fmt.Errorf("%w: stable name %q must match %s", ErrInvalidSource, stable, NamePattern)
	}
	if !NamePattern.MatchString(pkg) {
		return Source{}, fmt.Errorf("%w: package name %q must match %s", ErrInvalidSource, pkg, NamePattern)
	}
	if strings.HasPrefix(version, "-") {
		return Source{}, fmt.Errorf("%w: version %q must not begin with \"-\"", ErrInvalidSource, version)
	}
	if shaRe.MatchString(version) {
		if _, err := os.Stat(PinDir(Source{Stable: stable, Package: pkg, SHA: version})); err == nil {
			return Source{Stable: stable, Package: pkg, SHA: version}, nil
		}
	}
	dir := StableDir(stable)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return Source{}, fmt.Errorf("agentpkg: stable %q has no local clone at %s (run `harness agent stable add %s`): %w",
			stable, dir, stable, err)
	}
	ref := version
	if ref == "" {
		def, err := defaultBranch(dir)
		if err != nil {
			return Source{}, fmt.Errorf("agentpkg: stable %q: %w", stable, err)
		}
		ref = "origin/" + def
	}
	out, err := gitcmd.Output(dir, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return Source{}, fmt.Errorf("agentpkg: resolve %q in stable %q: %w", version, stable, err)
	}
	if !shaRe.MatchString(out) {
		return Source{}, fmt.Errorf("agentpkg: stable %q: %q resolved to %q, not a full commit sha", stable, version, out)
	}
	return Source{Stable: stable, Package: pkg, SHA: out}, nil
}

// Materialize extracts the package at s's commit from the stable's local
// clone into a fresh temp directory beside the pin's parent, by reading git
// objects with `git archive` — the clone's working tree and index are never
// touched. The extracted tree is made read-only so immutability is visible
// on disk; prune restores write permission before removing. The caller owns
// the temp directory: Place adopts it, and a refusal removes it.
func Materialize(s Source) (string, error) {
	// The temp directory lives at the store root, not beside the pin, so a
	// refused install never creates the pin's parent directories: the store
	// stays untouched until the gate passes.
	if err := os.MkdirAll(InstalledRoot(), 0o755); err != nil {
		return "", fmt.Errorf("agentpkg: create %s: %w", InstalledRoot(), err)
	}
	tmp, err := os.MkdirTemp(InstalledRoot(), ".materialize-*")
	if err != nil {
		return "", fmt.Errorf("agentpkg: temp dir for %s: %w", s, err)
	}
	tarball, err := gitcmd.Raw(StableDir(s.Stable), "archive", "--format=tar", s.SHA+":packages/"+s.Package)
	if err != nil {
		os.RemoveAll(tmp)
		return "", fmt.Errorf("agentpkg: materialize %s from stable %q: %s", s, s.Stable, redact.Redact(err.Error()))
	}
	if err := extractTar(tmp, tarball); err != nil {
		os.RemoveAll(tmp)
		return "", fmt.Errorf("agentpkg: materialize %s: %w", s, err)
	}
	if err := markReadOnly(tmp); err != nil {
		os.RemoveAll(tmp)
		return "", fmt.Errorf("agentpkg: materialize %s: %w", s, err)
	}
	return tmp, nil
}

// extractTar unpacks an in-memory tar tree into dir, refusing entries that
// escape it.
func extractTar(dir string, tarball []byte) error {
	tr := tar.NewReader(bytes.NewReader(tarball))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}
		name := filepath.Clean(hdr.Name)
		if name == "." || strings.HasPrefix(name, "..") || filepath.IsAbs(name) {
			return fmt.Errorf("tar entry %q escapes the extraction directory", hdr.Name)
		}
		target := filepath.Join(dir, name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := writeFileMode(target, tr, os.FileMode(hdr.Mode).Perm()); err != nil {
				return err
			}
		case tar.TypeSymlink:
			// A package is data; symlinks are a path out of the pin, so
			// they are refused rather than recreated.
			return fmt.Errorf("tar entry %q is a symlink; packages must not carry symlinks", hdr.Name)
		default:
			return fmt.Errorf("tar entry %q has unsupported type %c", hdr.Name, hdr.Typeflag)
		}
	}
}

func writeFileMode(path string, r io.Reader, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// markReadOnly walks dir making files 0444 and directories 0555, so a pin's
// immutability is visible: an attempt to edit bundled content fails the
// same way the store's never-rewrite rule does.
func markReadOnly(dir string) error {
	return filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		mode := os.FileMode(0o555)
		if !d.IsDir() {
			mode = 0o444
		}
		return os.Chmod(path, mode)
	})
}

// Discard removes a materialized temp directory, restoring write
// permission first (the tree is read-only by design). A refused install
// calls this so nothing is left behind.
func Discard(tmp string) error {
	if err := chmodTreeWritable(tmp); err != nil {
		return err
	}
	return os.RemoveAll(tmp)
}

// Place adopts a materialized temp directory as the pin for s. When the pin
// directory already exists it is reused untouched — never rewritten — and
// the temp directory is discarded; the boolean reports which happened.
func Place(s Source, tmp string) (bool, error) {
	pin := PinDir(s)
	if _, err := os.Stat(pin); err == nil {
		Discard(tmp)
		return true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("agentpkg: stat %s: %w", pin, err)
	}
	if err := os.MkdirAll(filepath.Dir(pin), 0o755); err != nil {
		return false, fmt.Errorf("agentpkg: place %s: %w", s, err)
	}
	// Renaming a directory needs write permission on it (rename updates
	// its ".." entry), so open the root briefly and close it again after
	// the rename lands it as the read-only pin.
	if err := os.Chmod(tmp, 0o755); err != nil {
		return false, fmt.Errorf("agentpkg: place %s: %w", s, err)
	}
	if err := os.Rename(tmp, pin); err != nil {
		return false, fmt.Errorf("agentpkg: place %s: %w", s, err)
	}
	if err := os.Chmod(pin, 0o555); err != nil {
		return false, fmt.Errorf("agentpkg: place %s: %w", s, err)
	}
	return false, nil
}

// PinStat reports the pin directory's identity, for the idempotency test
// that a repeated install neither rewrites nor replaces it.
func PinStat(s Source) (os.FileInfo, error) {
	return os.Stat(PinDir(s))
}

// BundledFilesIn lists the files under dir as slash-separated paths
// relative to it, sorted — the general form the clone-based BundledFiles
// and the store-based report share.
func BundledFilesIn(dir string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("agentpkg: list files in %s: %w", dir, err)
	}
	sort.Strings(files)
	return files, nil
}
