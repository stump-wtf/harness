package main

// Agent Package README
//
// End-to-end coverage of the package README through the real `harness
// agent` command tree on local bare repositories: info, install and upgrade
// show it sanitized and capped, --readme-full lifts the cap, the upgrade
// diff carries a README hunk only when the README changed, and a package
// without one draws the low package.no-readme finding and still installs.
//
// Governing: ADR-0044, SPEC-0026 REQ-2 (README.md expected), REQ-5 (the
// package.no-readme finding), REQ-8 (the upgrade diff).
//
// @joestump-agent 10/09/2026 - Added for harness#929.

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stump-wtf/harness/internal/agentpkg"
)

// hostileReadme is a setup README carrying the sequences a hostile stable
// would use: an OSC 8 hyperlink dressing a URL as other text, a screen
// clear, a window-title OSC, raw 8-bit C1 CSI and OSC, and a bare CR
// overprint. It runs past the inline cap.
func hostileReadme(lines int) string {
	var b strings.Builder
	b.WriteString("# pr-reviewer setup\x1b[2J\n")
	b.WriteString("Export GH_TOKEN, see \x1b]8;;https://evil.example\x1b\\the docs\x1b]8;;\x1b\\.\n")
	b.WriteString("\x1b]0;pwned\x07Title line\x9b2J\x9d0;x\x07 done\rOVERPRINT\n")
	for i := 4; i <= lines; i++ {
		fmt.Fprintf(&b, "setup line %d\n", i)
	}
	return b.String()
}

// assertNoControls fails when out carries an escape, a C1 byte, a CR or a
// BEL: the bytes a terminal would act on.
func assertNoControls(t *testing.T, out string) {
	t.Helper()
	if i := strings.IndexAny(out, "\x1b\x07\r"); i >= 0 {
		t.Fatalf("output carries a control byte at %d: %q", i, out[max(0, i-40):min(len(out), i+40)])
	}
	// A raw C1 byte is invalid UTF-8; an encoded one is a C1 rune.
	if !utf8.ValidString(out) {
		t.Fatalf("output is not valid UTF-8 (a raw C1 byte survived): %q", out)
	}
	for _, r := range out {
		if r >= 0x80 && r <= 0x9f {
			t.Fatalf("output carries C1 control %U", r)
		}
	}
}

// install shows the README in its confirmation: sanitized, capped with a
// footer, and whole under --readme-full. The real command runs against a
// fixture stable, and the README is the one git materialized.
func TestAgentInstallShowsReadme(t *testing.T) {
	e := newAgentEnv(t)
	remote, _ := agentRemote(t, map[string]string{
		"packages/pr-reviewer/package.toml": agentPkg,
		"packages/pr-reviewer/README.md":    hostileReadme(250),
	})
	if _, _, err := e.run("agent", "stable", "add", "stump-wtf", remote); err != nil {
		t.Fatal(err)
	}

	out, _, err := e.run("agent", "install", "stump-wtf/pr-reviewer", "--yes")
	if err != nil {
		t.Fatalf("install failed: %v (out: %s)", err, out)
	}
	assertNoControls(t, out)
	for _, want := range []string{
		"README.md (package text, shown as plain text; nothing in it is run):\n",
		"  # pr-reviewer setup\n",
		"  Export GH_TOKEN, see the docs.\n",
		"  Title line doneOVERPRINT\n",
		fmt.Sprintf("  setup line %d\n", agentpkg.ReadmeMaxLines),
		// A fresh install's materialization is discarded, so the footer
		// names no path.
		fmt.Sprintf("README truncated after %d of 250 lines; rerun with --readme-full to print it whole\n", agentpkg.ReadmeMaxLines),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("install output must contain %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, fmt.Sprintf("setup line %d\n", agentpkg.ReadmeMaxLines+1)) {
		t.Errorf("the inline README must stop at %d lines", agentpkg.ReadmeMaxLines)
	}
	if strings.Contains(out, agentpkg.FindingNoReadme) {
		t.Errorf("a package with a README draws no %s:\n%s", agentpkg.FindingNoReadme, out)
	}
	// The README renders before the findings, which stay next to the prompt.
	if strings.Index(out, "README.md (package text") > strings.Index(out, "scan findings:") {
		t.Errorf("the README must render before the findings:\n%s", out)
	}

	// Re-installing answers from the stored pin, which outlives the
	// command, so the footer names it; --readme-full prints it whole.
	src := installedSource(t, e.cfgPath, "pr-reviewer")
	out, _, err = e.run("agent", "install", "stump-wtf/pr-reviewer", "--yes")
	if err != nil {
		t.Fatal(err)
	}
	if want := "full text at " + filepath.Join(agentpkg.PinDir(src), "README.md"); !strings.Contains(out, want) {
		t.Errorf("a stored pin's footer must name %q:\n%s", want, out)
	}
	out, _, err = e.run("agent", "install", "stump-wtf/pr-reviewer", "--yes", "--readme-full")
	if err != nil {
		t.Fatal(err)
	}
	assertNoControls(t, out)
	if !strings.Contains(out, "  setup line 250\n") || strings.Contains(out, "README truncated") {
		t.Errorf("--readme-full must print the whole README:\n%s", out)
	}
}

// A package without a README draws the low package.no-readme finding on
// info and install, and still installs under --yes.
func TestAgentNoReadmeFinding(t *testing.T) {
	e := newAgentEnv(t)
	installFixture(t, e) // pr-reviewer, no README

	info, _, err := e.run("agent", "info", "stump-wtf/pr-reviewer")
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := e.run("agent", "install", "stump-wtf/pr-reviewer", "--yes")
	if err != nil {
		t.Fatalf("a missing README must not block the install: %v (out: %s)", err, out)
	}
	for name, o := range map[string]string{"info": info, "install": out} {
		if !strings.Contains(o, "  README.md  package.no-readme  low\n") {
			t.Errorf("%s must show the low package.no-readme finding:\n%s", name, o)
		}
		if !strings.Contains(o, "README.md: none") {
			t.Errorf("%s must say the package ships no README:\n%s", name, o)
		}
		// The fixture is unlicensed too: #932's line-bearing finding still
		// renders as file:line beside the file-level one.
		if !strings.Contains(o, "  package.toml:1  package.no-license  low\n") {
			t.Errorf("%s must show package.no-license as package.toml:1:\n%s", name, o)
		}
	}
	installedSource(t, e.cfgPath, "pr-reviewer")
}

// info renders the README after the manifest and the requests, from the
// local clone, sanitized; the footer names the clone's README.
func TestAgentInfoShowsReadme(t *testing.T) {
	e := newAgentEnv(t)
	remote, _ := agentRemote(t, map[string]string{
		"packages/pr-reviewer/package.toml": agentPkg,
		"packages/pr-reviewer/README.md":    hostileReadme(250),
	})
	if _, _, err := e.run("agent", "stable", "add", "stump-wtf", remote); err != nil {
		t.Fatal(err)
	}
	out, _, err := e.run("agent", "info", "stump-wtf/pr-reviewer")
	if err != nil {
		t.Fatal(err)
	}
	assertNoControls(t, out)
	req := strings.Index(out, "requests:\n")
	readme := strings.Index(out, "  Export GH_TOKEN, see the docs.\n")
	findings := strings.Index(out, "scan findings:\n")
	if req < 0 || readme < 0 || findings < 0 || req >= readme || readme >= findings {
		t.Fatalf("info must render the README after the requests and before the findings:\n%s", out)
	}
	clone := filepath.Join(agentpkg.PackageDir("stump-wtf", "pr-reviewer"), "README.md")
	if !strings.Contains(out, "full text at "+clone) {
		t.Errorf("info's footer must name the clone's README %s:\n%s", clone, out)
	}

	out, _, err = e.run("agent", "info", "stump-wtf/pr-reviewer", "--readme-full")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "  setup line 250\n") || strings.Contains(out, "README truncated") {
		t.Errorf("info --readme-full must print the whole README:\n%s", out)
	}
}

// upgrade shows a README diff, sanitized, when the README changed between
// pins, and no README hunk when it did not.
func TestAgentUpgradeReadmeDiff(t *testing.T) {
	e := newAgentEnv(t)
	remote, work := agentRemote(t, map[string]string{
		"packages/pr-reviewer/package.toml": agentPkg,
		"packages/pr-reviewer/README.md":    "# setup\nexport GH_TOKEN\n",
	})
	if _, _, err := e.run("agent", "stable", "add", "stump-wtf", remote); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.run("agent", "install", "stump-wtf/pr-reviewer", "--yes"); err != nil {
		t.Fatal(err)
	}
	u := &upgradeEnv{agentEnv: e, remote: remote, work: work}

	// The README changed: a sanitized hunk beside the manifest diff.
	u.pushCommit(t, map[string]string{
		"package.toml": strings.Replace(agentPkg, "1.0.0", "1.1.0", 1),
		"README.md":    "# setup\nexport GH_TOKEN\nexport \x1b]8;;https://evil.example\x1b\\TRIAGE_REPOS\x1b]8;;\x1b\\\x1b[2J\n",
	})
	if _, _, err := e.run("agent", "stable", "update", "stump-wtf"); err != nil {
		t.Fatal(err)
	}
	out, _, err := e.run("agent", "upgrade", "stump-wtf/pr-reviewer", "--yes")
	if err != nil {
		t.Fatalf("upgrade failed: %v (out: %s)", err, out)
	}
	assertNoControls(t, out)
	for _, want := range []string{
		"package.version: 1.0.0 -> 1.1.0",
		"--- a/README.md\n+++ b/README.md\n",
		"+export TRIAGE_REPOS\n",
		"  export TRIAGE_REPOS\n", // the candidate README in the report
	} {
		if !strings.Contains(out, want) {
			t.Errorf("upgrade output must contain %q:\n%s", want, out)
		}
	}

	// The README did not change: no README hunk.
	u.pushCommit(t, map[string]string{"package.toml": strings.Replace(agentPkg, "1.0.0", "1.2.0", 1)})
	if _, _, err := e.run("agent", "stable", "update", "stump-wtf"); err != nil {
		t.Fatal(err)
	}
	out, _, err = e.run("agent", "upgrade", "stump-wtf/pr-reviewer", "--yes")
	if err != nil {
		t.Fatalf("upgrade failed: %v (out: %s)", err, out)
	}
	if !strings.Contains(out, "package.version: 1.1.0 -> 1.2.0") {
		t.Fatalf("the second upgrade must run:\n%s", out)
	}
	if strings.Contains(out, "a/README.md") {
		t.Errorf("an unchanged README must not show in the diff:\n%s", out)
	}
}
