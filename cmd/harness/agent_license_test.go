package main

// Agent Package License
//
// SPEC-0026 REQ-3 coverage through the real command tree on local bare
// repositories: info, the install confirmation and `agent list` (table and
// --json) show [package].license; a package without one reads "none
// declared" with the package.no-license low finding and still installs; an
// unknown SPDX ID fails the install naming it; and an upgrade that changes
// the license calls it out as a terms change.
//
// Governing: ADR-0044, SPEC-0026 REQ-3, REQ-5, REQ-8, REQ-12.
//
// @joestump-agent 10/09/2026 - Added for harness#932.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stump-wtf/harness/internal/agentpkg"
)

const licensedPkg = `[package]
name = "pr-reviewer"
version = "1.0.0"
description = "reviews pull requests"
license = "MIT OR Apache-2.0"

[harness]
harness = "claude-code"
`

func TestAgentLicenseShownEverywhere(t *testing.T) {
	e := newAgentEnv(t)
	remote, _ := agentRemote(t, map[string]string{"packages/pr-reviewer/package.toml": licensedPkg})
	if _, _, err := e.run("agent", "stable", "add", "stump-wtf", remote); err != nil {
		t.Fatal(err)
	}

	out, _, err := e.run("agent", "info", "stump-wtf/pr-reviewer")
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	if !strings.Contains(out, "  license MIT OR Apache-2.0\n") {
		t.Fatalf("info must show the license:\n%s", out)
	}
	if strings.Contains(out, agentpkg.FindingNoLicense) {
		t.Fatalf("a licensed package has no %s finding:\n%s", agentpkg.FindingNoLicense, out)
	}

	out, _, err = e.run("agent", "install", "stump-wtf/pr-reviewer", "--yes")
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if !strings.Contains(out, "license: MIT OR Apache-2.0\n") {
		t.Fatalf("the install confirmation must show the license:\n%s", out)
	}

	out, _, err = e.run("agent", "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out, "LICENSE") || !strings.Contains(out, "MIT OR Apache-2.0") {
		t.Fatalf("the list table must show the license:\n%s", out)
	}

	out, _, err = e.run("agent", "list", "--json")
	if err != nil {
		t.Fatalf("list --json: %v", err)
	}
	var rows []agentListRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("decode list --json: %v\n%s", err, out)
	}
	if len(rows) != 1 || rows[0].License != "MIT OR Apache-2.0" {
		t.Fatalf("list --json must carry the pinned license: %+v", rows)
	}
	if !strings.Contains(out, `"license": "MIT OR Apache-2.0"`) {
		t.Fatalf("the JSON key is license:\n%s", out)
	}
}

// No license: info, install and list say so, the low finding shows, and the
// install still proceeds under --yes (the finding never blocks).
func TestAgentNoLicenseIsALowFinding(t *testing.T) {
	e := newAgentEnv(t)
	remote, _ := agentRemote(t, agentFixture())
	if _, _, err := e.run("agent", "stable", "add", "stump-wtf", remote); err != nil {
		t.Fatal(err)
	}

	out, _, err := e.run("agent", "info", "stump-wtf/pr-reviewer")
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	for _, want := range []string{"  license none declared (package.no-license)\n", "package.toml:1  package.no-license  low"} {
		if !strings.Contains(out, want) {
			t.Fatalf("info must show %q:\n%s", want, out)
		}
	}

	out, _, err = e.run("agent", "install", "stump-wtf/pr-reviewer", "--yes")
	if err != nil {
		t.Fatalf("a missing license must not block install: %v", err)
	}
	for _, want := range []string{"license: none declared (package.no-license)\n", "package.toml:1  package.no-license  low"} {
		if !strings.Contains(out, want) {
			t.Fatalf("the install confirmation must show %q:\n%s", want, out)
		}
	}

	out, _, err = e.run("agent", "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"license": ""`) {
		t.Fatalf("an unlicensed pin lists an empty license:\n%s", out)
	}
}

// An unknown SPDX ID fails the install naming it, and writes nothing.
func TestAgentInstallUnknownLicenseFails(t *testing.T) {
	e := newAgentEnv(t)
	bad := strings.Replace(licensedPkg, `"MIT OR Apache-2.0"`, `"MIT OR Bogus-1.0"`, 1)
	remote, _ := agentRemote(t, map[string]string{"packages/pr-reviewer/package.toml": bad})
	if _, _, err := e.run("agent", "stable", "add", "stump-wtf", remote); err != nil {
		t.Fatal(err)
	}
	_, _, err := e.run("agent", "install", "stump-wtf/pr-reviewer", "--yes")
	if err == nil || !strings.Contains(err.Error(), `unknown SPDX license ID "Bogus-1.0"`) {
		t.Fatalf("an unknown license ID must fail the install naming it, got %v", err)
	}
	out, _, _ := e.run("agent", "list", "--json")
	if strings.Contains(out, "pr-reviewer") {
		t.Fatalf("a refused install writes no table:\n%s", out)
	}
}

// An upgrade whose license changes calls it out as a terms change in the
// diff the operator reviews before confirming (REQ-8).
func TestAgentUpgradeCallsOutLicenseChange(t *testing.T) {
	u := newUpgradedEnv(t)
	u.pushCommit(t, map[string]string{"package.toml": strings.Replace(licensedPkg, `"MIT OR Apache-2.0"`, `"GPL-3.0-only"`, 1)})
	if _, _, err := u.run("agent", "stable", "update", "stump-wtf"); err != nil {
		t.Fatal(err)
	}
	out, _, err := u.run("agent", "upgrade", "stump-wtf/pr-reviewer", "--yes")
	if err != nil {
		t.Fatalf("upgrade: %v (out: %s)", err, out)
	}
	for _, want := range []string{
		"package.license: unset -> GPL-3.0-only  (license change: the package's terms changed)",
		"license: GPL-3.0-only\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("the upgrade must show %q:\n%s", want, out)
		}
	}

	// A second license change between two licensed pins.
	u.pushCommit(t, map[string]string{"package.toml": strings.Replace(licensedPkg, `"MIT OR Apache-2.0"`, `"MIT"`, 1)})
	if _, _, err := u.run("agent", "stable", "update", "stump-wtf"); err != nil {
		t.Fatal(err)
	}
	out, _, err = u.run("agent", "upgrade", "stump-wtf/pr-reviewer", "--yes")
	if err != nil {
		t.Fatalf("upgrade: %v (out: %s)", err, out)
	}
	if !strings.Contains(out, "package.license: GPL-3.0-only -> MIT  (license change: the package's terms changed)") {
		t.Fatalf("the upgrade must call out the license change:\n%s", out)
	}
}
