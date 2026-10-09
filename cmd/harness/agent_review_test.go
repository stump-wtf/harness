package main

// The Upgrade Effective-Value Review (issue #882)
//
// REQ-8 as amended: a diff between the harness's current effective values
// and the candidate never auto-applies. --yes and unattended runs refuse
// loudly naming the changes; an interactive run chooses per change, and the
// choice is written — kept package values pinned onto the table, taken rows
// over local overrides removing them so the new pin's value applies.
//
// Governing: ADR-0044, SPEC-0026 REQ-8, REQ-4 (issue #882).
//
// @joestump-agent 10/02/2026 - Added for harness#882.
//
// @joestump-agent 10/04/2026 - Review regressions: a pin-relative
// system_prompt_file on a version-only bump, a local auto_accept = false
// kept through Enter, list values that print alike, and the mcp_allow
// union.
//
// @joestump-agent 10/04/2026 - A local override is a row only when the
// package moves its key, unit and end to end.
//
// @joestump-agent 10/04/2026 - A kept package path survives prune: an
// interactive upgrade driven through a pty, then prune, then config load.

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/xpty"
	"github.com/spf13/cobra"

	"github.com/stump-wtf/harness/internal/agentpkg"
	"github.com/stump-wtf/harness/internal/config/tomledit"
	"github.com/stump-wtf/harness/internal/core"
)

func reviewCmd(buf *bytes.Buffer) *cobra.Command {
	cmd := &cobra.Command{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	return cmd
}

func changeFor(t *testing.T, chgs []agentpkg.EffectiveChange, key string) agentpkg.EffectiveChange {
	t.Helper()
	for _, c := range chgs {
		if c.Key == key {
			return c
		}
	}
	t.Fatalf("no change for key %q in %v", key, chgs)
	return agentpkg.EffectiveChange{}
}

// The review rows are exactly the behavioral ones: a moved package value,
// a dropped one, a new key, a contradicted local override — and never a
// local override the new pin simply does not mention, or unchanged values,
// or package metadata.
func TestEffectiveChanges(t *testing.T) {
	h := &core.Harness{
		Name:             "pr-reviewer",
		Adapter:          "claude-code",
		Model:            "pkg-model",
		SystemPromptFile: "/pin/system.md",
		MCPAllow:         []string{"read"},
		SkillPaths:       []string{"/operator/skills"},
		PackageKeys:      []string{"harness", "model", "system_prompt_file"},
	}
	oldMan := &agentpkg.Manifest{Harness: agentpkg.HarnessValues{
		Harness:          "claude-code",
		Model:            "pkg-model",
		SystemPromptFile: "system.md",
	}}
	newMan := &agentpkg.Manifest{
		Package: agentpkg.PackageMeta{Version: "9.9.9"},
		Harness: agentpkg.HarnessValues{
			Harness:          "claude-code",
			Model:            "newer-model",
			Args:             []string{"--deep"},
			SystemPromptFile: "",
			MCPConfig:        "/pin/mcp.json",
			SkillPaths:       []string{"/pin/skills"},
		},
		Requests: agentpkg.Requests{MCPAllow: []string{"read", "write"}},
	}
	chgs := agentpkg.EffectiveChanges(h, oldMan, newMan)

	// skill_paths (issue #816): the review covers the key — a local
	// override the new pin contradicts is a conflict row.
	sk := changeFor(t, chgs, "skill_paths")
	if !sk.OldLocal || sk.Render(sk.Old) != "[/operator/skills]" || sk.Render(sk.New) != "[/pin/skills]" {
		t.Fatalf("skill_paths change shape wrong: %+v", sk)
	}

	// model moved with the pin (package-supplied).
	c := changeFor(t, chgs, "model")
	if c.Old != "pkg-model" || c.New != "newer-model" || c.OldLocal || c.Added {
		t.Fatalf("model change shape wrong: %+v", c)
	}
	// args is new with this version.
	c = changeFor(t, chgs, "args")
	if !c.Added || c.Old != nil {
		t.Fatalf("an introduced key must be marked added: %+v", c)
	}
	// system_prompt_file is dropped by the new pin.
	c = changeFor(t, chgs, "system_prompt_file")
	if c.Old == nil || c.New != nil || c.OldLocal {
		t.Fatalf("a dropped package value must be a removal row: %+v", c)
	}
	// mcp_config is new.
	changeFor(t, chgs, "mcp_config")
	// the requested write scope is not granted.
	c = changeFor(t, chgs, "mcp_allow")
	if c.Kind != "request" || c.Render(c.New) != "[read, write]" {
		t.Fatalf("the ungranted scope must be a request row: %+v", c)
	}
	// harness is unchanged; version metadata never appears.
	for _, c := range chgs {
		if c.Key == "harness" || c.Key == "version" {
			t.Fatalf("unchanged or metadata key must not be reviewed: %+v", c)
		}
	}

	// A local override the new pin contradicts is a conflict row; one the
	// pin does not mention is not a row at all.
	h2 := &core.Harness{
		Adapter:     "claude-code",
		Model:       "my-own",
		Workdir:     "/srv",
		PackageKeys: []string{"harness"},
	}
	harnessOnly := &agentpkg.Manifest{Harness: agentpkg.HarnessValues{Harness: "claude-code"}}
	chgs = agentpkg.EffectiveChanges(h2, harnessOnly, newMan)
	c = changeFor(t, chgs, "model")
	if !c.OldLocal || c.Old != "my-own" || c.New != "newer-model" {
		t.Fatalf("a contradicted override must be a conflict row: %+v", c)
	}
	for _, c := range chgs {
		if c.Key == "workdir" {
			t.Fatalf("an override the pin never mentions is not a row: %+v", c)
		}
	}

	// No behavioral difference: no rows at all.
	same := &core.Harness{Adapter: "claude-code", PackageKeys: []string{"harness"}}
	sameMan := &agentpkg.Manifest{Harness: agentpkg.HarnessValues{Harness: "claude-code"}}
	if chgs := agentpkg.EffectiveChanges(same, sameMan, sameMan); len(chgs) != 0 {
		t.Fatalf("no-op upgrade must have no review rows: %v", chgs)
	}
}

// The choice parser: Enter keeps everything, all takes everything, a comma
// list takes those rows, and garbage refuses.
func TestParseReviewChoice(t *testing.T) {
	take, err := parseReviewChoice("", 3)
	if err != nil || len(take) != 0 {
		t.Fatalf("empty answer keeps all, got %v %v", take, err)
	}
	take, err = parseReviewChoice("all", 3)
	if err != nil || len(take) != 3 {
		t.Fatalf("all takes all, got %v %v", take, err)
	}
	take, err = parseReviewChoice("1, 3", 3)
	if err != nil || len(take) != 2 || !take[0] || !take[2] {
		t.Fatalf("comma list takes those rows, got %v %v", take, err)
	}
	if _, err := parseReviewChoice("9", 3); err == nil {
		t.Fatal("an out-of-range row must refuse")
	}
	if _, err := parseReviewChoice("yes", 3); err == nil {
		t.Fatal("a non-numeric answer must refuse")
	}
}

// The apply mapping, on the real editor: keep pins the old value onto the
// table, take removes the local override, a taken request grants the scope.
func TestApplyReviewChoices(t *testing.T) {
	src := `[harness.reviewer]
model = "my-own"
args = ["--strict"]
source = "stump-wtf/pr-reviewer@0000000000000000000000000000000000000000"
`
	ed := tomledit.New([]byte(src))
	h := core.Harness{
		Adapter:  "claude-code",
		Model:    "my-own",
		Args:     []string{"--strict"},
		MCPAllow: []string{"read"},
	}
	newMan := &agentpkg.Manifest{
		Harness:  agentpkg.HarnessValues{Harness: "claude-code", Model: "newer-model"},
		Requests: agentpkg.Requests{MCPAllow: []string{"read", "write"}},
	}
	// model: take the package's (drop the local override);
	// args: keep the local override;
	// mcp_allow: grant the requested scope.
	take := map[int]bool{}
	oldMan := &agentpkg.Manifest{Harness: agentpkg.HarnessValues{Harness: "claude-code"}}
	chgs := agentpkg.EffectiveChanges(&h, oldMan, newMan)
	for i, c := range chgs {
		switch c.Key {
		case "model":
			take[i] = true
		case "args":
			// not taken (it is not a row at all: the new pin has no args)
		case "mcp_allow":
			take[i] = true
		}
	}
	var out bytes.Buffer
	if err := applyReviewChoices(reviewCmd(&out), ed, "reviewer", chgs, take); err != nil {
		t.Fatal(err)
	}
	data := string(ed.Bytes())
	if !strings.Contains(data, `source = "stump-wtf/pr-reviewer@`) {
		t.Fatalf("source untouched:\n%s", data)
	}
	if strings.Contains(data, "model") {
		t.Fatalf("taking the package's model must remove the local override:\n%s", data)
	}
	if !strings.Contains(data, `args = ["--strict"]`) {
		t.Fatalf("the untouched local override must stay byte-identical:\n%s", data)
	}
	if !strings.Contains(data, `mcp_allow = ["read", "write"]`) {
		t.Fatalf("the granted scope must be written:\n%s", data)
	}
}

// Keeping a package-supplied value pins it onto the table as an explicit
// override, so the choice is real on disk.
func TestApplyReviewChoicesKeepsPinned(t *testing.T) {
	src := `[harness.reviewer]
source = "stump-wtf/pr-reviewer@0000000000000000000000000000000000000000"
`
	ed := tomledit.New([]byte(src))
	h := core.Harness{
		Adapter:     "claude-code",
		Model:       "pkg-model",
		PackageKeys: []string{"harness", "model"},
	}
	oldMan := &agentpkg.Manifest{Harness: agentpkg.HarnessValues{Harness: "claude-code", Model: "pkg-model"}}
	newMan := &agentpkg.Manifest{Harness: agentpkg.HarnessValues{Harness: "claude-code", Model: "newer-model"}}
	chgs := agentpkg.EffectiveChanges(&h, oldMan, newMan)
	if len(chgs) != 1 {
		t.Fatalf("want the one model row, got %v", chgs)
	}
	var out bytes.Buffer
	if err := applyReviewChoices(reviewCmd(&out), ed, "reviewer", chgs, map[int]bool{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ed.Bytes()), `model = "pkg-model"`) {
		t.Fatalf("keeping must pin the old value:\n%s", ed.Bytes())
	}

	// Taking it instead writes nothing but leaves the source to supply the
	// new value: an explicit copy would turn the package's key into a
	// local override that never follows the package again.
	ed = tomledit.New([]byte(src))
	out.Reset()
	if err := applyReviewChoices(reviewCmd(&out), ed, "reviewer", chgs, map[int]bool{0: true}); err != nil {
		t.Fatal(err)
	}
	if string(ed.Bytes()) != src {
		t.Fatalf("taking a package value must not write it onto the table:\n%s", ed.Bytes())
	}
}

// --yes bombs out loudly on a reviewable diff: a moved package value
// refuses the upgrade instead of auto-applying, and the source is
// untouched. The same refusal answers an unattended run without --yes.
func TestAgentUpgradeReviewRefusesUnderYes(t *testing.T) {
	e := newAgentEnv(t)
	v1 := strings.Replace(agentPkg, `harness = "claude-code"`, `harness = "claude-code"
args = ["--deep"]`, 1)
	remote, work := agentRemote(t, map[string]string{"packages/pr-reviewer/package.toml": v1})
	if _, _, err := e.run("stable", "add", "stump-wtf", remote); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.run("agent", "install", "stump-wtf/pr-reviewer", "--yes"); err != nil {
		t.Fatal(err)
	}
	oldSrc := installedSource(t, e.cfgPath, "pr-reviewer")

	// The new pin moves the args.
	v2 := strings.Replace(agentPkg, `harness = "claude-code"`, `harness = "claude-code"
args = ["--deeper"]`, 1)
	if err := os.WriteFile(filepath.Join(work, "packages/pr-reviewer/package.toml"), []byte(v2), 0o644); err != nil {
		t.Fatal(err)
	}
	agentGit(t, work, "add", "-A")
	agentGit(t, work, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qm", "move args")
	agentGit(t, work, "push", "-q", remote, "main")
	if _, _, err := e.run("stable", "update", "stump-wtf"); err != nil {
		t.Fatal(err)
	}

	_, _, err := e.run("agent", "upgrade", "stump-wtf/pr-reviewer", "--yes")
	if err == nil || !strings.Contains(err.Error(), "may want to review") || !strings.Contains(err.Error(), "args: [--deep] -> [--deeper]") {
		t.Fatalf("--yes must refuse a reviewable diff naming it, got %v", err)
	}
	// Unattended without --yes refuses the same way.
	_, _, err = e.run("agent", "upgrade", "stump-wtf/pr-reviewer")
	if err == nil || !strings.Contains(err.Error(), "may want to review") {
		t.Fatalf("an unattended run must refuse a reviewable diff, got %v", err)
	}
	if src := installedSource(t, e.cfgPath, "pr-reviewer"); src.SHA != oldSrc.SHA {
		t.Fatalf("a refused upgrade must not move the source: %v -> %v", oldSrc.SHA, src.SHA)
	}
}

// Install writes the confirmed requested scope onto the table (issue #882):
// a package declaring scopes beyond the default lands them visibly.
func TestAgentInstallWritesConfirmedScope(t *testing.T) {
	e := newAgentEnv(t)
	manifest := strings.Replace(agentPkg, `[harness]
harness = "claude-code"`, `[harness]
harness = "claude-code"

[requests]
mcp_allow = ["read"]`, 1)
	remote, _ := agentRemote(t, map[string]string{"packages/pr-reviewer/package.toml": manifest})
	if _, _, err := e.run("stable", "add", "stump-wtf", remote); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.run("agent", "install", "stump-wtf/pr-reviewer", "--yes"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(e.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `mcp_allow = ["read"]`) {
		t.Fatalf("the confirmed scope must be written onto the table:\n%s", data)
	}
	// And it loads back as the effective grant.
	cfg, err := loadGlobalConfig(e.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Harnesses["pr-reviewer"].MCPAllow; len(got) != 1 || got[0] != "read" {
		t.Fatalf("the grant must be effective after reload: %v", got)
	}
}

// A pin-relative path the new pin leaves alone is not a change: the
// effective value is an absolute path under the old pin directory, so the
// review compares the two manifests instead. A version-only bump of such a
// package upgrades under --yes, end to end.
func TestAgentUpgradePinRelativePathIsNotAChange(t *testing.T) {
	e := newAgentEnv(t)
	v1 := strings.Replace(agentPkg, `harness = "claude-code"`, `harness = "claude-code"
system_prompt_file = "system.md"`, 1)
	remote, work := agentRemote(t, map[string]string{
		"packages/pr-reviewer/package.toml": v1,
		"packages/pr-reviewer/system.md":    "Review carefully.\n",
	})
	if _, _, err := e.run("stable", "add", "stump-wtf", remote); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.run("agent", "install", "stump-wtf/pr-reviewer", "--yes"); err != nil {
		t.Fatal(err)
	}
	// system_prompt_file needs a one-shot harness: the prompt is the
	// operator's, on the table.
	data, err := os.ReadFile(e.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	e.writeFile("harness.toml", strings.Replace(string(data), "source = ", "prompt = \"go\"\nsource = ", 1))
	oldSrc := installedSource(t, e.cfgPath, "pr-reviewer")

	if err := os.WriteFile(filepath.Join(work, "packages/pr-reviewer/package.toml"), []byte(strings.Replace(v1, "1.0.0", "1.1.0", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	agentGit(t, work, "add", "-A")
	agentGit(t, work, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qm", "bump")
	agentGit(t, work, "push", "-q", remote, "main")
	if _, _, err := e.run("stable", "update", "stump-wtf"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.run("agent", "upgrade", "stump-wtf/pr-reviewer", "--yes"); err != nil {
		t.Fatalf("a version-only bump must not be a review: %v", err)
	}
	newSrc := installedSource(t, e.cfgPath, "pr-reviewer")
	if newSrc.SHA == oldSrc.SHA {
		t.Fatal("the upgrade did not move the source")
	}
	after, err := os.ReadFile(e.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(after), "system_prompt_file") {
		t.Fatalf("nothing but the source may change:\n%s", after)
	}
	// And it resolves under the NEW pin directory.
	cfg, err := loadGlobalConfig(e.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.Harnesses["pr-reviewer"].SystemPromptFile, filepath.Join(agentpkg.PinDir(newSrc), "system.md"); got != want {
		t.Fatalf("system_prompt_file = %q, want %q", got, want)
	}

	// A pin that moves the path is still a row, shown against the
	// effective value.
	h := core.Harness{
		Adapter:          "claude-code",
		SystemPromptFile: "/pins/old/system.md",
		PackageKeys:      []string{"harness", "system_prompt_file"},
	}
	oldMan := &agentpkg.Manifest{Harness: agentpkg.HarnessValues{Harness: "claude-code", SystemPromptFile: "system.md"}}
	newMan := &agentpkg.Manifest{Harness: agentpkg.HarnessValues{Harness: "claude-code", SystemPromptFile: "prompts/review.md"}}
	c := changeFor(t, agentpkg.EffectiveChanges(&h, oldMan, newMan), "system_prompt_file")
	if c.Old != "/pins/old/system.md" || c.New != "prompts/review.md" || c.OldLocal || c.Added {
		t.Fatalf("a moved manifest path must be a package row: %+v", c)
	}
}

// A scalar at its zero is keepable, never "added": the loaded config cannot
// tell a local auto_accept = false from an unset key, so pressing Enter
// must pin false rather than let a new pin's true in — and must never
// overwrite the operator's explicit false.
func TestReviewKeepsALocalFalse(t *testing.T) {
	src := `[harness.reviewer]
source = "stump-wtf/pr-reviewer@0000000000000000000000000000000000000000"
auto_accept = false
`
	h := core.Harness{Adapter: "claude-code", PackageKeys: []string{"harness"}}
	yes, no := true, false
	oldMan := &agentpkg.Manifest{Harness: agentpkg.HarnessValues{Harness: "claude-code"}}
	newMan := &agentpkg.Manifest{Harness: agentpkg.HarnessValues{Harness: "claude-code", AutoAccept: &yes}}
	chgs := agentpkg.EffectiveChanges(&h, oldMan, newMan)
	c := changeFor(t, chgs, "auto_accept")
	if c.Added || c.Old != false || c.New != true {
		t.Fatalf("a scalar at its zero must be a keepable row: %+v", c)
	}

	// Enter keeps: the explicit false stays.
	ed := tomledit.New([]byte(src))
	var out bytes.Buffer
	if err := applyReviewChoices(reviewCmd(&out), ed, "reviewer", chgs, map[int]bool{}); err != nil {
		t.Fatal(err)
	}
	if got := string(ed.Bytes()); got != src {
		t.Fatalf("keeping must leave auto_accept = false:\n%s", got)
	}
	// With the key unset, keeping pins the zero so the new pin cannot flip it.
	bare := "[harness.reviewer]\nsource = \"stump-wtf/pr-reviewer@0000000000000000000000000000000000000000\"\n"
	ed = tomledit.New([]byte(bare))
	if err := applyReviewChoices(reviewCmd(&out), ed, "reviewer", chgs, map[int]bool{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ed.Bytes()), "auto_accept = false") {
		t.Fatalf("keeping an unset scalar must pin its zero:\n%s", ed.Bytes())
	}
	// Taking removes the local false so the pin's true applies.
	ed = tomledit.New([]byte(src))
	if err := applyReviewChoices(reviewCmd(&out), ed, "reviewer", chgs, map[int]bool{0: true}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ed.Bytes()), "auto_accept") {
		t.Fatalf("taking must remove the local false:\n%s", ed.Bytes())
	}

	// A new pin stating the zero changes nothing: no row.
	quiet := &agentpkg.Manifest{Harness: agentpkg.HarnessValues{Harness: "claude-code", AutoAccept: &no}}
	if chgs := agentpkg.EffectiveChanges(&h, oldMan, quiet); len(chgs) != 0 {
		t.Fatalf("an explicit zero equal to the effective value is not a change: %+v", chgs)
	}
	// A scalar the old pin supplied but the table also sets is local.
	withPin := &agentpkg.Manifest{Harness: agentpkg.HarnessValues{Harness: "claude-code", AutoAccept: &no}}
	c = changeFor(t, agentpkg.EffectiveChanges(&h, withPin, newMan), "auto_accept")
	if !c.OldLocal {
		t.Fatalf("a table value shadowing the old pin is the operator's: %+v", c)
	}
}

// Values compare structurally: two lists that print alike are still
// different arguments.
func TestEffectiveChangesComparesListsStructurally(t *testing.T) {
	h := core.Harness{Adapter: "claude-code", Args: []string{"a b"}, PackageKeys: []string{"args", "harness"}}
	oldMan := &agentpkg.Manifest{Harness: agentpkg.HarnessValues{Harness: "claude-code", Args: []string{"a b"}}}
	newMan := &agentpkg.Manifest{Harness: agentpkg.HarnessValues{Harness: "claude-code", Args: []string{"a", "b"}}}
	changeFor(t, agentpkg.EffectiveChanges(&h, oldMan, newMan), "args")
}

// The mcp_allow row grants the union: taking it can never revoke a scope
// the operator already granted.
func TestEffectiveChangesGrantsTheUnion(t *testing.T) {
	h := core.Harness{Adapter: "claude-code", MCPAllow: []string{"write"}, PackageKeys: []string{"harness"}}
	man := &agentpkg.Manifest{
		Harness:  agentpkg.HarnessValues{Harness: "claude-code"},
		Requests: agentpkg.Requests{MCPAllow: []string{"read", "write"}},
	}
	c := changeFor(t, agentpkg.EffectiveChanges(&h, man, man), "mcp_allow")
	if got := c.Render(c.New); got != "[write, read]" {
		t.Fatalf("the grant must keep the existing scope and add the missing one, got %s", got)
	}
	h.MCPAllow = []string{"read", "write"}
	if chgs := agentpkg.EffectiveChanges(&h, man, man); len(chgs) != 0 {
		t.Fatalf("a fully granted request is not a row: %+v", chgs)
	}
}

// A local override is a row only when the package moves its key: one the
// new pin leaves alone is the operator's settled choice, and one the new
// pin moves to the operator's own value agrees with it.
func TestReviewFlagsAnOverrideOnlyWhenThePackageMovesIt(t *testing.T) {
	h := core.Harness{Adapter: "claude-code", Model: "opus", PackageKeys: []string{"harness"}}
	pin := func(model string) *agentpkg.Manifest {
		return &agentpkg.Manifest{Harness: agentpkg.HarnessValues{Harness: "claude-code", Model: model}}
	}
	if chgs := agentpkg.EffectiveChanges(&h, pin("sonnet"), pin("sonnet")); len(chgs) != 0 {
		t.Fatalf("an override the package did not move is not a row: %+v", chgs)
	}
	c := changeFor(t, agentpkg.EffectiveChanges(&h, pin("sonnet"), pin("haiku")), "model")
	if !c.OldLocal || c.Old != "opus" || c.New != "haiku" {
		t.Fatalf("a package move under an override must be a conflict row: %+v", c)
	}
	if chgs := agentpkg.EffectiveChanges(&h, pin("sonnet"), pin("opus")); len(chgs) != 0 {
		t.Fatalf("a move to the operator's own value is not a row: %+v", chgs)
	}
}

// End to end: a version-only bump upgrades under --yes past a local
// override, leaving it byte-identical; a bump that moves the overridden
// key refuses, naming it.
func TestAgentUpgradeLocalOverrideBlocksOnlyOnAMove(t *testing.T) {
	e := newAgentEnv(t)
	v1 := strings.Replace(agentPkg, `harness = "claude-code"`, `harness = "claude-code"
model = "sonnet"`, 1)
	remote, work := agentRemote(t, map[string]string{"packages/pr-reviewer/package.toml": v1})
	if _, _, err := e.run("stable", "add", "stump-wtf", remote); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.run("agent", "install", "stump-wtf/pr-reviewer", "--yes"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(e.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	// model needs a one-shot harness: the prompt is the operator's too.
	e.writeFile("harness.toml", strings.Replace(string(data), "source = ", "prompt = \"go\"\nmodel = \"opus\"\nsource = ", 1))
	oldSrc := installedSource(t, e.cfgPath, "pr-reviewer")

	push := func(body, msg string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(work, "packages/pr-reviewer/package.toml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		agentGit(t, work, "add", "-A")
		agentGit(t, work, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qm", msg)
		agentGit(t, work, "push", "-q", remote, "main")
		if _, _, err := e.run("stable", "update", "stump-wtf"); err != nil {
			t.Fatal(err)
		}
	}

	push(strings.Replace(v1, "1.0.0", "1.1.0", 1), "bump")
	if _, _, err := e.run("agent", "upgrade", "stump-wtf/pr-reviewer", "--yes"); err != nil {
		t.Fatalf("an override the package did not move must not block --yes: %v", err)
	}
	midSrc := installedSource(t, e.cfgPath, "pr-reviewer")
	if midSrc.SHA == oldSrc.SHA {
		t.Fatal("the upgrade did not move the source")
	}
	after, err := os.ReadFile(e.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), `model = "opus"`) {
		t.Fatalf("the override must survive untouched:\n%s", after)
	}

	push(strings.Replace(strings.Replace(v1, "1.0.0", "1.2.0", 1), `model = "sonnet"`, `model = "haiku"`, 1), "move model")
	_, _, err = e.run("agent", "upgrade", "stump-wtf/pr-reviewer", "--yes")
	if err == nil || !strings.Contains(err.Error(), "model: opus -> haiku") {
		t.Fatalf("a package move under the override must refuse naming it, got %v", err)
	}
	if src := installedSource(t, e.cfgPath, "pr-reviewer"); src.SHA != midSrc.SHA {
		t.Fatalf("a refused upgrade must not move the source: %v -> %v", midSrc.SHA, src.SHA)
	}
}

// ttyStdin points os.Stdin at a pty slave and types lines into its master,
// so the upgrade's review and confirmation read them exactly as they would
// from a terminal. Canonical mode hands each read one line, so answers
// typed up front cannot run together.
func ttyStdin(t *testing.T, lines ...string) {
	t.Helper()
	ptmx, err := xpty.NewPty(80, 24)
	if err != nil {
		t.Skipf("no pty available: %v", err)
	}
	t.Cleanup(func() { _ = ptmx.Close() })
	// Slave(), not Name(): see TestRunIsInteractiveRequiresBothEnds.
	sl, ok := ptmx.(interface{ Slave() *os.File })
	if !ok {
		t.Skip("pty implementation exposes no slave handle")
	}
	orig := os.Stdin
	os.Stdin = sl.Slave()
	t.Cleanup(func() { os.Stdin = orig })
	for _, l := range lines {
		if _, err := ptmx.Write([]byte(l)); err != nil {
			t.Fatal(err)
		}
	}
}

// Keeping a moved package path pins the OLD pin's absolute path onto the
// table, so that pin is still in use though no source names it any more.
// Prune must keep it and say why, or the next config load fails on the
// missing prompt or MCP file. End to end: install, an interactive upgrade
// that keeps every current value, prune, load.
func TestAgentPruneKeepsAPinAKeptPathPointsInto(t *testing.T) {
	cases := []struct{ key, file, body string }{
		{"system_prompt_file", "system.md", "Review carefully.\n"},
		{"mcp_config", "mcp.json", "{\"mcpServers\": {}}\n"},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			e := newAgentEnv(t)
			v1 := strings.Replace(agentPkg, `harness = "claude-code"`, `harness = "claude-code"
`+tc.key+` = "`+tc.file+`"`, 1)
			remote, work := agentRemote(t, map[string]string{
				"packages/pr-reviewer/package.toml": v1,
				"packages/pr-reviewer/" + tc.file:   tc.body,
			})
			if _, _, err := e.run("stable", "add", "stump-wtf", remote); err != nil {
				t.Fatal(err)
			}
			if _, _, err := e.run("agent", "install", "stump-wtf/pr-reviewer", "--yes"); err != nil {
				t.Fatal(err)
			}
			// The persona keys need a one-shot: the prompt is the operator's.
			data, err := os.ReadFile(e.cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			e.writeFile("harness.toml", strings.Replace(string(data), "source = ", "prompt = \"go\"\nsource = ", 1))
			oldSrc := installedSource(t, e.cfgPath, "pr-reviewer")

			// The new pin moves the path.
			v2 := strings.Replace(v1, `"`+tc.file+`"`, `"v2/`+tc.file+`"`, 1)
			if err := os.WriteFile(filepath.Join(work, "packages/pr-reviewer/package.toml"), []byte(v2), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(work, "packages/pr-reviewer/v2"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(work, "packages/pr-reviewer/v2", tc.file), []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			agentGit(t, work, "add", "-A")
			agentGit(t, work, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qm", "move path")
			agentGit(t, work, "push", "-q", remote, "main")
			if _, _, err := e.run("stable", "update", "stump-wtf"); err != nil {
				t.Fatal(err)
			}

			// Enter keeps every current value; y confirms the upgrade.
			ttyStdin(t, "\n", "y\n")
			out, _, err := e.run("agent", "upgrade", "stump-wtf/pr-reviewer")
			if err != nil {
				t.Fatalf("interactive upgrade: %v\n%s", err, out)
			}
			newSrc := installedSource(t, e.cfgPath, "pr-reviewer")
			if newSrc.SHA == oldSrc.SHA {
				t.Fatalf("the upgrade did not move the source:\n%s", out)
			}
			kept := filepath.Join(agentpkg.PinDir(oldSrc), tc.file)
			after, err := os.ReadFile(e.cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(after), fmt.Sprintf("%s = %q", tc.key, kept)) {
				t.Fatalf("keeping must pin the old pin's path onto the table:\n%s", after)
			}
			if !strings.Contains(out, "prune keeps @"+oldSrc.SHA) {
				t.Fatalf("keeping a pin path must say it holds the old pin:\n%s", out)
			}

			out, _, err = e.run("agent", "prune")
			if err != nil {
				t.Fatalf("prune: %v", err)
			}
			if strings.Contains(out, "pruned "+oldSrc.String()) {
				t.Fatalf("prune removed a pin a kept %s points into:\n%s", tc.key, out)
			}
			if !strings.Contains(out, "kept "+oldSrc.String()) || !strings.Contains(out, "[harness.pr-reviewer] "+tc.key) {
				t.Fatalf("prune must say which key holds the pin:\n%s", out)
			}

			cfg, err := loadGlobalConfig(e.cfgPath)
			if err != nil {
				t.Fatalf("config load after prune: %v", err)
			}
			h := cfg.Harnesses["pr-reviewer"]
			got := map[string]string{"system_prompt_file": h.SystemPromptFile, "mcp_config": h.MCPConfig}[tc.key]
			if got != kept {
				t.Fatalf("%s = %q, want the kept %q", tc.key, got, kept)
			}
		})
	}
}

// The same property without a terminal, on the exact shape the review's
// keep writes: source on the new pin, the old pin's absolute path on a
// file key. Prune keeps the old pin and names the key; once the key stops
// pointing there, the next prune removes it.
func TestAgentPruneHonorsPathsIntoPins(t *testing.T) {
	e := newAgentEnv(t)
	remote, work := agentRemote(t, map[string]string{
		"packages/pr-reviewer/package.toml": agentPkg,
		"packages/pr-reviewer/system.md":    "Review carefully.\n",
	})
	if _, _, err := e.run("stable", "add", "stump-wtf", remote); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.run("agent", "install", "stump-wtf/pr-reviewer", "--yes"); err != nil {
		t.Fatal(err)
	}
	oldSrc := installedSource(t, e.cfgPath, "pr-reviewer")

	if err := os.WriteFile(filepath.Join(work, "packages/pr-reviewer/package.toml"), []byte(strings.Replace(agentPkg, "1.0.0", "1.1.0", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	agentGit(t, work, "add", "-A")
	agentGit(t, work, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qm", "bump")
	agentGit(t, work, "push", "-q", remote, "main")
	if _, _, err := e.run("stable", "update", "stump-wtf"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.run("agent", "install", "stump-wtf/pr-reviewer", "--replace", "--yes"); err != nil {
		t.Fatal(err)
	}
	newSrc := installedSource(t, e.cfgPath, "pr-reviewer")
	if newSrc.SHA == oldSrc.SHA {
		t.Fatal("the reinstall did not move the source")
	}

	kept := filepath.Join(agentpkg.PinDir(oldSrc), "system.md")
	table := fmt.Sprintf("[harness.pr-reviewer]\nsource = %q\nprompt = \"go\"\nsystem_prompt_file = %q\n", newSrc.String(), kept)
	e.writeFile("harness.toml", table)

	out, _, err := e.run("agent", "prune")
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if strings.Contains(out, "pruned "+oldSrc.String()) {
		t.Fatalf("prune removed a pin a table path points into:\n%s", out)
	}
	if !strings.Contains(out, "kept "+oldSrc.String()) || !strings.Contains(out, "[harness.pr-reviewer] system_prompt_file") {
		t.Fatalf("prune must say which key holds the pin:\n%s", out)
	}
	if _, err := loadGlobalConfig(e.cfgPath); err != nil {
		t.Fatalf("config load after prune: %v", err)
	}

	// With the key gone, nothing holds the old pin.
	e.writeFile("harness.toml", fmt.Sprintf("[harness.pr-reviewer]\nsource = %q\n", newSrc.String()))
	out, _, err = e.run("agent", "prune")
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if !strings.Contains(out, "pruned "+oldSrc.String()) {
		t.Fatalf("an unheld pin must be pruned:\n%s", out)
	}
	if strings.Contains(out, "pruned "+newSrc.String()) {
		t.Fatalf("the sourced pin must stay:\n%s", out)
	}
}
