package main

// Top-Level Stable Command
//
// `harness stable` is the stable trust ledger's spelling; `harness agent
// stable` is its hidden alias (harness#937). These tests drive both
// spellings through the real root command against one local bare-repository
// stable and require byte-identical stdout, stderr and errors, so scripts
// written against the old spelling (stump.wtf/harness-stable's CI among
// them) keep working unchanged.
//
// Governing: ADR-0044, SPEC-0026 REQ-1 (stable registration), REQ-12 (CLI
// visibility).
//
// @joestump-agent 10/09/2026 - Added for harness#937.

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/stump-wtf/harness/internal/agentpkg"
)

// stableStep is one recorded invocation: what the command printed and how
// it ended, with the per-run state directory normalized out.
type stableStep struct {
	args            string
	out, errOut     string
	err             string
	unknownSentinel bool
}

// stableTranscript runs the same add/list/update/remove sequence, plus its
// failure paths, under one spelling of the stable tree. prefix is the argv
// that names the tree: {"stable"} or {"agent", "stable"}.
func stableTranscript(t *testing.T, prefix []string, remote string) []stableStep {
	t.Helper()
	e := newAgentEnv(t)
	norm := func(s string) string { return strings.ReplaceAll(s, e.stateDir, "$STATE") }
	var steps []stableStep
	run := func(args ...string) {
		t.Helper()
		out, errOut, err := e.run(append(append([]string{}, prefix...), args...)...)
		steps = append(steps, stableStep{
			args:            strings.Join(args, " "),
			out:             norm(out),
			errOut:          norm(errOut),
			err:             norm(errToString(err)),
			unknownSentinel: errors.Is(err, agentpkg.ErrUnknownStable),
		})
	}

	run("list")                     // nothing registered yet
	run("update")                   // nothing to update
	run("add", "stump-wtf", remote) // clone, then write the table
	run("add", "stump-wtf", remote) // already registered: refuses
	run("add", "both", remote, "--public", "--private")
	run("list")                // name, remote, visibility, head
	run("update")              // every stable: up to date
	run("update", "stump-wtf") // one stable: up to date
	run("update", "ghost")     // unknown stable
	run("remove", "stump-wtf") // drop the table, keep the clone
	run("remove", "ghost")     // unknown stable
	run("list")                // empty again

	// The sequence must have exercised the paths it claims to, or identical
	// transcripts would prove nothing.
	if !strings.Contains(steps[2].out, "stump-wtf added") || steps[2].err != "" {
		t.Fatalf("%v add did not succeed: %+v", prefix, steps[2])
	}
	if !strings.Contains(steps[5].out, "head ") {
		t.Fatalf("%v list did not show the clone head: %+v", prefix, steps[5])
	}
	if !steps[8].unknownSentinel || !steps[10].unknownSentinel {
		t.Fatalf("%v unknown-stable paths did not fail with ErrUnknownStable", prefix)
	}
	return steps
}

// Both spellings drive the same subtree: every verb's stdout, stderr and
// error are identical, failure paths included (#937).
func TestStableSpellingsBehaveIdentically(t *testing.T) {
	remote, _ := agentRemote(t, agentFixture())

	var top, alias []stableStep
	t.Run("harness stable", func(t *testing.T) {
		top = stableTranscript(t, []string{"stable"}, remote)
	})
	t.Run("harness agent stable", func(t *testing.T) {
		alias = stableTranscript(t, []string{"agent", "stable"}, remote)
	})
	if t.Failed() {
		return
	}
	if len(top) != len(alias) {
		t.Fatalf("transcripts differ in length: %d vs %d", len(top), len(alias))
	}
	for i := range top {
		if top[i] != alias[i] {
			t.Errorf("step %d (%s) differs:\n  harness stable:       %+v\n  harness agent stable: %+v",
				i, top[i].args, top[i], alias[i])
		}
	}
}

// The hint strings name the new spelling, never the alias.
func TestStableHintsNameTopLevelCommand(t *testing.T) {
	e := newAgentEnv(t)
	out, _, err := e.run("stable", "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "harness stable add NAME REMOTE") || strings.Contains(out, "agent stable") {
		t.Fatalf("empty-list hint must name `harness stable add`: %s", out)
	}

	remote, _ := agentRemote(t, agentFixture())
	if _, _, err := e.run("stable", "add", "stump-wtf", remote); err != nil {
		t.Fatal(err)
	}
	_, _, err = e.run("stable", "add", "stump-wtf", remote)
	if err == nil || !strings.Contains(err.Error(), "harness stable update stump-wtf") {
		t.Fatalf("re-add hint must name `harness stable update`, got %v", err)
	}
}

// subcommandNames lists a command's children; visible restricts it to the
// ones help would show.
func subcommandNames(c *cobra.Command, visible bool) []string {
	var names []string
	for _, sub := range c.Commands() {
		if visible && !sub.IsAvailableCommand() {
			continue
		}
		names = append(names, sub.Name())
	}
	sort.Strings(names)
	return names
}

func containsName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// `stable` is a visible top-level command and a hidden child of `agent`,
// and both instances carry the same verbs — so a verb added to the
// constructor appears under both spellings (#937).
func TestStableTreeWiring(t *testing.T) {
	root := newRootCmd()
	if !containsName(subcommandNames(root, true), "stable") {
		t.Fatalf("root must offer a visible `stable`: %v", subcommandNames(root, true))
	}
	agent, _, err := root.Find([]string{"agent"})
	if err != nil {
		t.Fatal(err)
	}
	if containsName(subcommandNames(agent, true), "stable") {
		t.Fatalf("`agent stable` must be hidden from agent's help: %v", subcommandNames(agent, true))
	}
	if !containsName(subcommandNames(agent, false), "stable") {
		t.Fatal("`agent stable` must still be registered as an alias")
	}

	top, _, err := root.Find([]string{"stable"})
	if err != nil || top.Name() != "stable" || top.Parent() != root {
		t.Fatalf("root.Find(stable) = %v, %v", top, err)
	}
	alias, _, err := root.Find([]string{"agent", "stable"})
	if err != nil || alias.Name() != "stable" || alias.Parent() != agent {
		t.Fatalf("root.Find(agent stable) = %v, %v", alias, err)
	}
	if !alias.Hidden || top.Hidden {
		t.Fatalf("hidden: top=%v alias=%v; want false/true", top.Hidden, alias.Hidden)
	}
	if a, b := strings.Join(subcommandNames(top, false), ","), strings.Join(subcommandNames(alias, false), ","); a != b {
		t.Fatalf("verbs differ: harness stable {%s}, harness agent stable {%s}", a, b)
	}
}

// helpOutput runs the real root command with args and returns what the help
// renderer wrote, which goes to os.Stderr rather than cobra's writers.
func helpOutput(t *testing.T, args ...string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "help")
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stderr
	os.Stderr = f
	defer func() { os.Stderr = saved }()

	root := newRootCmd()
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		t.Fatalf("%v: %v", args, err)
	}
	data, err := os.ReadFile(filepath.Clean(f.Name()))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// `harness --help` lists the stable verbs under their new spelling, and
// neither it nor `harness agent --help` mentions the hidden alias.
func TestStableHelpListing(t *testing.T) {
	rootHelp := helpOutput(t, "--help")
	for _, want := range []string{"stable add NAME REMOTE", "stable remove NAME", "stable update [NAME]", "stable list"} {
		if !strings.Contains(rootHelp, want) {
			t.Errorf("harness --help missing %q", want)
		}
	}
	if !strings.Contains(rootHelp, "agent install") {
		t.Fatalf("harness --help did not render the command table: %s", rootHelp)
	}
	agentHelp := helpOutput(t, "agent", "--help")
	if agentHelp == "" {
		t.Fatal("harness agent --help printed nothing")
	}
	for name, out := range map[string]string{"harness --help": rootHelp, "harness agent --help": agentHelp} {
		if strings.Contains(out, "agent stable") {
			t.Errorf("%s lists the hidden `agent stable` alias", name)
		}
	}
}
