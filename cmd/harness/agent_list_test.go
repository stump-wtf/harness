package main

// Agent List / Describe Attribution / Doctor Agent Pins
//
// REQ-12 coverage: list shows staleness from the clone without fetching
// (short SHA in the table, full SHA and the declaring file in --json);
// describe attributes each effective key to the package or the operator's
// own table, unchanged for a hand-written harness and for an older daemon;
// the doctor row warns for a pin missing from disk under the daemon's
// last-good view.
//
// Governing: ADR-0044, SPEC-0026 REQ-12.
//
// @joestump-agent 10/02/2026 - Added for harness#815.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stump-wtf/harness/internal/cliui"
	"github.com/stump-wtf/harness/internal/protocol"
)

// List shows staleness without fetching: a clone three commits ahead marks
// the package newer, with the remote deleted so any fetch would fail.
func TestAgentListStalenessWithoutFetching(t *testing.T) {
	e := newAgentEnv(t)
	remote, work := agentRemote(t, map[string]string{"packages/pr-reviewer/package.toml": agentPkg})
	if _, _, err := e.run("stable", "add", "stump-wtf", remote); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.run("agent", "install", "stump-wtf/pr-reviewer", "--yes"); err != nil {
		t.Fatal(err)
	}
	oldSrc := installedSource(t, e.cfgPath, "pr-reviewer")

	// Three commits ahead of the pin.
	for i := 0; i < 3; i++ {
		if err := os.WriteFile(filepath.Join(work, "packages/pr-reviewer/notes.md"), []byte("note "+string(rune('a'+i))+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		agentGit(t, work, "add", "-A")
		agentGit(t, work, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qm", "advance")
	}
	agentGit(t, work, "push", "-q", remote, "main")
	if _, _, err := e.run("stable", "update", "stump-wtf"); err != nil {
		t.Fatal(err)
	}
	// The remote is gone: nothing may fetch.
	if err := os.RemoveAll(remote); err != nil {
		t.Fatal(err)
	}

	out, _, err := e.run("agent", "list")
	if err != nil {
		t.Fatalf("list must not fetch: %v", err)
	}
	if !strings.Contains(out, "pr-reviewer") || !strings.Contains(out, "yes") {
		t.Fatalf("the stale package must be marked newer:\n%s", out)
	}
	if !strings.Contains(out, oldSrc.SHA[:12]) || strings.Contains(out, oldSrc.SHA) {
		t.Fatalf("the table shows the short sha only:\n%s", out)
	}

	// --json carries the full sha and the declaring file.
	out, _, err = e.run("agent", "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{oldSrc.SHA, `"pin"`, e.cfgPath, `"newer_available": true`} {
		if !strings.Contains(out, want) {
			t.Fatalf("json must carry %q:\n%s", want, out)
		}
	}
}

// A pin at the clone's tip is not marked newer.
func TestAgentListUpToDate(t *testing.T) {
	e := newAgentEnv(t)
	remote, _ := agentRemote(t, map[string]string{"packages/pr-reviewer/package.toml": agentPkg})
	if _, _, err := e.run("stable", "add", "stump-wtf", remote); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.run("agent", "install", "stump-wtf/pr-reviewer", "--yes"); err != nil {
		t.Fatal(err)
	}

	out, _, err := e.run("agent", "list")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "yes") {
		t.Fatalf("a pin at the tip must not be marked newer:\n%s", out)
	}
}

// Describe attributes each field: one local override is marked local, every
// other key package; a hand-written harness and an older daemon render the
// value unchanged.
func TestDescribeMark(t *testing.T) {
	pkg := protocol.HarnessInfo{
		Source:      "stump-wtf/pr-reviewer@0000000000000000000000000000000000000000",
		PackageKeys: []string{"harness", "model", "mcp_config"},
		Adapter:     "claude-code",
		Model:       "big",
		MCPConfig:   "/etc/mcp.json",
	}
	mark := describeMark(pkg)
	if got := mark("model", "big"); got != "big (package)" {
		t.Errorf("a package key must be marked package, got %q", got)
	}
	if got := mark("mcp_config", "/etc/mcp.json"); got != "/etc/mcp.json (package)" {
		t.Errorf("a package key must be marked package, got %q", got)
	}
	if got := mark("workdir", "/srv"); got != "/srv (local)" {
		t.Errorf("an operator key must be marked local, got %q", got)
	}

	// A hand-written harness: no source, no markers.
	hand := protocol.HarnessInfo{Model: "big"}
	mark = describeMark(hand)
	if got := mark("model", "big"); got != "big" {
		t.Errorf("a hand-written harness renders unchanged, got %q", got)
	}

	// An older daemon: source but no package_keys on the wire.
	older := protocol.HarnessInfo{Source: pkg.Source, Model: "big"}
	mark = describeMark(older)
	if got := mark("model", "big"); got != "big" {
		t.Errorf("an older daemon renders without attribution, got %q", got)
	}
}

// The doctor row: a package-sourced harness whose pin is gone from disk
// under the daemon's view warns with its own JSON object; a healthy view
// stays silent.
func TestAgentPinsCheck(t *testing.T) {
	src := "stump-wtf/pr-reviewer@0000000000000000000000000000000000000000"

	missing := &stubAgentsClient{list: []protocol.HarnessInfo{
		{Name: "pr-reviewer", Source: src},
		{Name: "hand-written"},
	}}
	r := agentPinsCheck(missing)
	if r == nil {
		t.Fatal("a missing pin must produce a row")
	}
	if r.level != cliui.LevelWarn {
		t.Fatalf("the row must be a warn, got %v", r.level)
	}
	if !strings.Contains(r.detail, "pr-reviewer") || !strings.Contains(r.detail, src) {
		t.Fatalf("the row must name the harness and its source: %s", r.detail)
	}

	healthy := &stubAgentsClient{list: []protocol.HarnessInfo{{Name: "plain"}}}
	if r := agentPinsCheck(healthy); r != nil {
		t.Fatalf("a view with no package-sourced harness needs no row, got %+v", r)
	}

	// The row lands in the doctor JSON with its own object.
	rows := []check{*r, {name: "config", level: cliui.LevelSuccess, detail: "ok"}}
	dr := doctorResultFromRows(rows)
	if dr.AgentPins == nil || dr.AgentPins.Status != "warn" {
		t.Fatalf("the agent_pins row must carry its own JSON object: %+v", dr.AgentPins)
	}
}

type stubAgentsClient struct {
	list []protocol.HarnessInfo
}

func (s *stubAgentsClient) List() ([]protocol.HarnessInfo, error) { return s.list, nil }

// doctorResultFromRows mirrors emitDoctorJSON's row mapping for a small
// hand-built row set.
func doctorResultFromRows(rows []check) doctorResult {
	var res doctorResult
	for _, r := range rows {
		cr := checkResult{Status: r.level.String(), Name: r.name, Detail: r.detail, Hint: r.hint}
		if r.name == "agent_pins" {
			res.AgentPins = &cr
		}
	}
	return res
}

// The provenance flows the whole way: a config whose harness resolves a
// package records exactly the keys the manifest supplied — the manifest's
// harness key, never the locally overridden model.
func TestPackageKeysFlowFromConfig(t *testing.T) {
	e := newAgentEnv(t)
	manifest := `[package]
name = "pr-reviewer"
description = "reviews pull requests"

[harness]
harness = "claude-code"
model = "pkg-model"
`
	remote, _ := agentRemote(t, map[string]string{"packages/pr-reviewer/package.toml": manifest})
	if _, _, err := e.run("stable", "add", "stump-wtf", remote); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.run("agent", "install", "stump-wtf/pr-reviewer", "--yes"); err != nil {
		t.Fatal(err)
	}
	// The operator adds a prompt and overrides the model locally.
	data, err := os.ReadFile(e.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	busy := strings.Replace(string(data), "source = ", "prompt = \"review prs\"\nmodel = \"my-own\"\nsource = ", 1)
	if err := os.WriteFile(e.cfgPath, []byte(busy), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadGlobalConfig(e.cfgPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	h := cfg.Harnesses["pr-reviewer"]
	if h.PackageSource == "" {
		t.Fatal("the source must flow")
	}
	if len(h.PackageKeys) != 1 || h.PackageKeys[0] != "harness" {
		t.Fatalf("only the manifest-supplied harness key is the package's, got %v", h.PackageKeys)
	}
	if h.Model != "my-own" {
		t.Fatalf("the local override must win, got %q", h.Model)
	}
}
