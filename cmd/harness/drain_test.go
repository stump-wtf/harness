package main

// The Drain Verbs In The CLI
//
// Two things are pinned here: the verbs exist in the command tree with the
// flags the packaging script needs, and the flag combinations that cannot mean
// anything are refused before the daemon is dialed, so a typo in a script
// never arms a gate the operator did not ask for.
//
// Governing: stump.wtf/harness#828; ADR-0062 (proposed).
//
// @joestump-agent 11/06/2026 - Added for stump.wtf/harness#828.

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestDrainVerbsAreRegistered keeps the pair in the tree with their flags: the
// upgrade recipe in docs/usage/cli.md types every one of them.
func TestDrainVerbsAreRegistered(t *testing.T) {
	root := newRootCmd()
	want := map[string][]string{
		"drain":   {"all", "discard", "wait", "timeout"},
		"undrain": {"all"},
	}
	for _, verb := range []string{"drain", "undrain"} {
		var cmd *cobra.Command
		for _, c := range root.Commands() {
			if c.Name() == verb {
				cmd = c
			}
		}
		if cmd == nil {
			t.Fatalf("harness %s is not in the command tree", verb)
		}
		if cmd.Short == "" {
			t.Errorf("harness %s has no short description for help", verb)
		}
		for _, flag := range want[verb] {
			if cmd.Flags().Lookup(flag) == nil {
				t.Errorf("harness %s is missing --%s", verb, flag)
			}
		}
	}
}

// TestDrainRefusesImpossibleFlags: the two combinations that have no meaning
// are refused locally, with no daemon in sight.
func TestDrainRefusesImpossibleFlags(t *testing.T) {
	bin := buildHarnessBinary(t)
	env, _ := isolatedEnv(t)

	cases := []struct {
		args []string
		want string
	}{
		{[]string{"drain", "--timeout", "30m"}, "--timeout"},
		{[]string{"drain", "x", "--discard", "--wait"}, "--discard"},
		{[]string{"drain", "x", "--wait", "--timeout", "soon"}, "--timeout"},
		{[]string{"undrain"}, "requires a harness/profile name"},
	}
	for _, c := range cases {
		out, code := runCLI(t, bin, env, c.args...)
		if code == 0 {
			t.Errorf("`harness %s` exited 0:\n%s", strings.Join(c.args, " "), out)
		}
		if !strings.Contains(out, c.want) {
			t.Errorf("`harness %s` did not name %q:\n%s", strings.Join(c.args, " "), c.want, out)
		}
	}
}
