package agentpkg

// Governing: ADR-0044, SPEC-0026 REQ-2 (README.md expected; rendered as
// plain text), REQ-5 (package.no-readme).
//
// @joestump-agent 10/09/2026 - Added for harness#929.

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readmeLines(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	return b.String()
}

func TestLoadReadme(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		r, err := LoadReadme(t.TempDir(), true)
		if err != nil || r.Present {
			t.Fatalf("no README.md must load as absent, got %+v, %v", r, err)
		}
	})
	t.Run("sanitized and located", func(t *testing.T) {
		dir := t.TempDir()
		writeTestFile(t, filepath.Join(dir, ReadmeFile), "# Setup\x1b[2J\nExport \x1b]8;;https://evil.example\x1b\\TOKEN\x1b]8;;\x1b\\\r\n")
		r, err := LoadReadme(dir, true)
		if err != nil {
			t.Fatal(err)
		}
		if !r.Present || r.Text != "# Setup\nExport TOKEN\n" {
			t.Fatalf("README must load sanitized, got %+v", r)
		}
		if r.Path != filepath.Join(dir, ReadmeFile) {
			t.Fatalf("a kept directory names the README's path, got %q", r.Path)
		}
		if r2, _ := LoadReadme(dir, false); r2.Path != "" {
			t.Fatalf("a discarded directory names no path, got %q", r2.Path)
		}
	})
	t.Run("symlink is not followed", func(t *testing.T) {
		dir := t.TempDir()
		secret := filepath.Join(t.TempDir(), "secret")
		writeTestFile(t, secret, "do not show\n")
		if err := os.Symlink(secret, filepath.Join(dir, ReadmeFile)); err != nil {
			t.Fatal(err)
		}
		r, err := LoadReadme(dir, true)
		if err != nil || r.Present || strings.Contains(r.Text, "do not show") {
			t.Fatalf("a symlinked README must not be followed, got %+v, %v", r, err)
		}
		if ok, _ := HasReadme(dir); ok {
			t.Fatal("a symlinked README does not count as present")
		}
	})
	t.Run("clipped past the byte cap", func(t *testing.T) {
		dir := t.TempDir()
		writeTestFile(t, filepath.Join(dir, ReadmeFile), strings.Repeat("x", ReadmeMaxBytes+10))
		r, err := LoadReadme(dir, true)
		if err != nil {
			t.Fatal(err)
		}
		if !r.Clipped || len(r.Text) != ReadmeMaxBytes {
			t.Fatalf("README past %d bytes must be clipped, got clipped=%v len=%d", ReadmeMaxBytes, r.Clipped, len(r.Text))
		}
	})
}

func TestRenderReadme(t *testing.T) {
	render := func(r Readme, full bool) string {
		var buf bytes.Buffer
		RenderReadme(&buf, r, full)
		return buf.String()
	}

	t.Run("absent names the finding", func(t *testing.T) {
		out := render(Readme{}, false)
		if !strings.Contains(out, "README.md: none") || !strings.Contains(out, FindingNoReadme) {
			t.Fatalf("an absent README must say so and name %s:\n%s", FindingNoReadme, out)
		}
	})

	t.Run("short README prints whole, indented", func(t *testing.T) {
		out := render(Readme{Present: true, Text: "# Title\n\nbody\n"}, false)
		want := "README.md (package text, shown as plain text; nothing in it is run):\n  # Title\n\n  body\n"
		if out != want {
			t.Fatalf("got:\n%q\nwant:\n%q", out, want)
		}
	})

	t.Run("capped at ReadmeMaxLines with a footer naming the path", func(t *testing.T) {
		text := readmeLines(ReadmeMaxLines + 50)
		out := render(Readme{Present: true, Text: text, Path: "/pins/x/README.md"}, false)
		if !strings.Contains(out, fmt.Sprintf("  line %d\n", ReadmeMaxLines)) {
			t.Fatalf("line %d must print", ReadmeMaxLines)
		}
		if strings.Contains(out, fmt.Sprintf("  line %d\n", ReadmeMaxLines+1)) {
			t.Fatalf("line %d must not print", ReadmeMaxLines+1)
		}
		footer := fmt.Sprintf("README truncated after %d of %d lines; full text at /pins/x/README.md, or rerun with --readme-full\n", ReadmeMaxLines, ReadmeMaxLines+50)
		if !strings.HasSuffix(out, footer) {
			t.Fatalf("missing footer %q:\n%s", footer, out[len(out)-200:])
		}
	})

	t.Run("capped with no kept path points at --readme-full", func(t *testing.T) {
		out := render(Readme{Present: true, Text: readmeLines(ReadmeMaxLines + 1)}, false)
		if !strings.Contains(out, "rerun with --readme-full to print it whole") || strings.Contains(out, "full text at") {
			t.Fatalf("footer must not name a path that will not exist:\n%s", out[len(out)-200:])
		}
	})

	t.Run("full prints everything", func(t *testing.T) {
		out := render(Readme{Present: true, Text: readmeLines(ReadmeMaxLines + 50)}, true)
		if !strings.Contains(out, fmt.Sprintf("  line %d\n", ReadmeMaxLines+50)) || strings.Contains(out, "truncated") {
			t.Fatal("--readme-full must print the whole README with no footer")
		}
	})

	t.Run("exactly at the cap is not truncated", func(t *testing.T) {
		out := render(Readme{Present: true, Text: readmeLines(ReadmeMaxLines)}, false)
		if strings.Contains(out, "truncated") {
			t.Fatal("a README of exactly ReadmeMaxLines lines is not truncated")
		}
	})
}

// The README renders between the requests and the findings, so the findings
// and the no-guarantee statement stay closest to the prompt; a file-level
// finding renders without a line number.
func TestRenderReportShowsReadme(t *testing.T) {
	in := ReportInput{
		Ref:         "stump-wtf/pr-reviewer",
		ManifestRaw: []byte("[package]\nname = \"pr-reviewer\"\n"),
		Man:         &Manifest{},
		Readme:      Readme{Present: true, Text: "Set TRIAGE_REPOS before starting.\n"},
	}
	var buf bytes.Buffer
	RenderReport(&buf, in)
	out := buf.String()
	req := strings.Index(out, "requests:\n")
	readme := strings.Index(out, "  Set TRIAGE_REPOS before starting.\n")
	findings := strings.Index(out, "scan findings:\n")
	if req < 0 || readme < 0 || findings < 0 || req >= readme || readme >= findings {
		t.Fatalf("README must render after requests and before findings:\n%s", out)
	}

	in.Readme = Readme{}
	in.Findings = []Finding{
		{File: ReadmeFile, PatternID: FindingNoReadme, Severity: SeverityLow},
		{File: "package.toml", Line: 1, PatternID: FindingNoLicense, Severity: SeverityLow},
	}
	buf.Reset()
	RenderReport(&buf, in)
	out = buf.String()
	if !strings.Contains(out, "README.md: none") {
		t.Fatalf("an absent README must say so:\n%s", out)
	}
	if !strings.Contains(out, "  README.md  package.no-readme  low\n") || strings.Contains(out, "README.md:0") {
		t.Fatalf("a file-level finding renders without a line number:\n%s", out)
	}
	// A finding with a line, such as #932's package.no-license, keeps it.
	if !strings.Contains(out, "  package.toml:1  package.no-license  low\n") {
		t.Fatalf("a line-bearing finding renders as file:line:\n%s", out)
	}
}

// The upgrade diff shows a README hunk when the README changed between pins
// and none when it did not, and no line of it carries a terminal control
// sequence from the package.
func TestPinDiffReadme(t *testing.T) {
	man := &Manifest{Package: PackageMeta{Name: "x"}}
	oldDir, newDir := t.TempDir(), t.TempDir()
	writeTestFile(t, filepath.Join(oldDir, "package.toml"), "[package]\nname = \"x\"\n")
	writeTestFile(t, filepath.Join(newDir, "package.toml"), "[package]\nname = \"x\"\n")
	writeTestFile(t, filepath.Join(oldDir, ReadmeFile), "# x\nexport A\n")
	writeTestFile(t, filepath.Join(newDir, ReadmeFile), "# x\nexport A\n")

	lines, err := PinDiff(oldDir, newDir, man, man)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(lines, "\n"), ReadmeFile) {
		t.Fatalf("an unchanged README must not appear in the diff:\n%s", strings.Join(lines, "\n"))
	}

	writeTestFile(t, filepath.Join(newDir, ReadmeFile), "# x\nexport A\nexport \x1b]8;;https://evil.example\x1b\\B\x1b]8;;\x1b\\\x1b[2J\n")
	lines, err = PinDiff(oldDir, newDir, man, man)
	if err != nil {
		t.Fatal(err)
	}
	out := strings.Join(lines, "\n")
	if !strings.Contains(out, "--- a/README.md\n+++ b/README.md") || !strings.Contains(out, "\n+export B") {
		t.Fatalf("a changed README must show as a sanitized hunk:\n%q", out)
	}
	if strings.ContainsAny(out, "\x1b\x07") {
		t.Fatalf("the diff must carry no escape bytes:\n%q", out)
	}
}

func writeTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
