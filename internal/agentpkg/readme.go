// The package README: the setup notes a stable's author ships at
// packages/<name>/README.md, saying what an operator must provide (env vars,
// tokens, logins) that the manifest cannot carry. `harness agent info`,
// install and upgrade show it, and a package without one draws the low
// package.no-readme scan finding.
//
// The README is untrusted text from a remote. It is read as data and never
// followed: no link is opened, no command is run, and no file it names is
// read. It reaches the terminal only through SanitizeTerminal, and the
// inline render is capped at ReadmeMaxLines unless the operator asks for
// the whole thing.
//
// Governing: ADR-0044 (agent package stables: a package is data, nothing
// from a remote is executed), SPEC-0026 REQ-2 (layout: README.md expected),
// REQ-5 (the package.no-readme finding).
//
// @joestump-agent 10/09/2026 - Added for harness#929.
package agentpkg

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const (
	// ReadmeFile is the package-relative path of a package's README.
	ReadmeFile = "README.md"

	// FindingNoReadme is the pattern id of the low finding a package
	// without a README draws (SPEC-0026 REQ-5).
	FindingNoReadme = "package.no-readme"

	// ReadmeMaxLines caps the inline render; --readme-full lifts it.
	ReadmeMaxLines = 200

	// ReadmeMaxBytes bounds how much of a README is read at all, with or
	// without --readme-full. A setup README is a page or two. A file past
	// 1 MiB is not one, and the operator can still read it on disk.
	ReadmeMaxBytes = 1 << 20
)

// Readme is a package's README, loaded for display.
type Readme struct {
	// Present is false when the package has no README.md as a regular
	// file. A symlinked README is not followed: it could point anywhere on
	// the operator's disk.
	Present bool
	// Text is the sanitized README (SanitizeTerminal), at most
	// ReadmeMaxBytes of it.
	Text string
	// Clipped reports that the file was longer than ReadmeMaxBytes.
	Clipped bool
	// Path is where the full README lives on disk for the operator to
	// read, or "" when the directory it came from is not kept (an install's
	// temp materialization).
	Path string
}

// HasReadme reports whether dir holds README.md as a regular file. It is
// the same test scan.Scan applies for package.no-readme, so install, info
// and stable lint agree on what counts.
func HasReadme(dir string) (bool, error) {
	fi, err := os.Lstat(filepath.Join(dir, ReadmeFile))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("agentpkg: stat %s: %w", ReadmeFile, err)
	}
	return fi.Mode().IsRegular(), nil
}

// LoadReadme reads dir's README.md, at most ReadmeMaxBytes of it, and
// sanitizes it for the terminal. keep says whether dir outlives this
// command; when it does, the result's Path names the file for the
// truncation footer.
func LoadReadme(dir string, keep bool) (Readme, error) {
	ok, err := HasReadme(dir)
	if err != nil || !ok {
		return Readme{}, err
	}
	path := filepath.Join(dir, ReadmeFile)
	f, err := os.Open(path)
	if err != nil {
		return Readme{}, fmt.Errorf("agentpkg: open %s: %w", ReadmeFile, err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, ReadmeMaxBytes+1))
	if err != nil {
		return Readme{}, fmt.Errorf("agentpkg: read %s: %w", ReadmeFile, err)
	}
	r := Readme{Present: true}
	if len(raw) > ReadmeMaxBytes {
		raw, r.Clipped = raw[:ReadmeMaxBytes], true
	}
	r.Text = SanitizeTerminal(string(raw))
	if keep {
		r.Path = path
	}
	return r, nil
}

// RenderReadme writes the README section of info and of the install and
// upgrade confirmation. The text is indented two spaces, like every other
// section, which sets it apart from Harness's own lines. Past
// ReadmeMaxLines it stops with a footer saying where the full text is,
// unless full is set. A package without a README gets one line naming the
// finding.
func RenderReadme(w io.Writer, r Readme, full bool) {
	if !r.Present {
		fmt.Fprintf(w, "%s: none — the package ships no setup README (%s)\n", ReadmeFile, FindingNoReadme)
		return
	}
	fmt.Fprintf(w, "%s (package text, shown as plain text; nothing in it is run):\n", ReadmeFile)
	lines := strings.Split(strings.TrimRight(r.Text, "\n"), "\n")
	shown := lines
	if !full && len(lines) > ReadmeMaxLines {
		shown = lines[:ReadmeMaxLines]
	}
	for _, line := range shown {
		if line == "" {
			fmt.Fprintln(w)
			continue
		}
		fmt.Fprintf(w, "  %s\n", line)
	}
	if len(shown) < len(lines) {
		where := "rerun with --readme-full to print it whole"
		if r.Path != "" {
			where = "full text at " + r.Path + ", or rerun with --readme-full"
		}
		fmt.Fprintf(w, "README truncated after %d of %d lines; %s\n", len(shown), len(lines), where)
	}
	if r.Clipped {
		fmt.Fprintf(w, "README is larger than %d bytes; only the first %d are shown\n", ReadmeMaxBytes, ReadmeMaxBytes)
	}
}
