package main

// Governing tests: SPEC-0011 REQ "Scratchpad Creation" — the positional
// dispatch matrix (kind word vs. generic command vs. --kind override) and the
// --name slug override.

import (
	"reflect"
	"testing"
)

func TestScratchpadDefKindDispatch(t *testing.T) {
	cases := []struct {
		name    string
		kind    string
		nameArg string
		args    []string
		adapter string
		want    []string
	}{
		{"agent kind with args", "", "", []string{"claude", "opus-5"}, "claude-code", []string{"opus-5"}},
		{"crush bare", "", "", []string{"crush"}, "crush", []string{}},
		{"generic command", "", "", []string{"htop"}, "generic", []string{"-c", "htop"}},
		{"generic command with flag", "", "", []string{"htop", "-t"}, "generic", []string{"-c", "htop -t"}},
		{"kind override forces generic", "generic", "", []string{"crush"}, "generic", []string{"-c", "crush"}},
		{"kind override keeps args", "codex", "", []string{"--yolo"}, "codex", []string{"--yolo"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			def, _ := scratchpadDef(tc.kind, tc.nameArg, tc.args)
			if def.Harness != tc.adapter {
				t.Errorf("adapter = %q, want %q", def.Harness, tc.adapter)
			}
			if !reflect.DeepEqual(def.Args, tc.want) {
				t.Errorf("args = %v, want %v", def.Args, tc.want)
			}
		})
	}
}

// TestRunCmdFlagParsing pins the SetInterspersed(false) behavior: flags after
// the first positional are left in args for the invoked command (the
// `harness run htop -t` regression), while run's own flags still parse when
// they lead the invocation.
func TestRunCmdFlagParsing(t *testing.T) {
	cases := []struct {
		name     string
		argv     []string
		wantArgs []string
		kind     string
	}{
		{"flag after positional stays an arg", []string{"htop", "-t"}, []string{"htop", "-t"}, ""},
		{"leading flags still parse", []string{"--kind", "codex", "model-x"}, []string{"model-x"}, "codex"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newRunCmd(&globalOpts{})
			if err := cmd.ParseFlags(tc.argv); err != nil {
				t.Fatalf("ParseFlags(%v): %v", tc.argv, err)
			}
			got := cmd.Flags().Args()
			if !reflect.DeepEqual(got, tc.wantArgs) {
				t.Errorf("args = %v, want %v", got, tc.wantArgs)
			}
			if k, _ := cmd.Flags().GetString("kind"); k != tc.kind {
				t.Errorf("kind = %q, want %q", k, tc.kind)
			}
		})
	}
}

func TestScratchpadDefNameOverride(t *testing.T) {
	def, slug := scratchpadDef("", "mypad", []string{"claude"})
	if def.Name != "mypad" || slug != "mypad" {
		t.Errorf("name override not honored: def.Name=%q slug=%q", def.Name, slug)
	}
}
