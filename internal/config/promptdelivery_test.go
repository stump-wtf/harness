package config

// Command Prompt Delivery Tests
//
// A `command` harness may carry a prompt, delivered by prompt_delivery:
// {{prompt}} in argv, a stdin pipe, or a 0600 file. These tests pin the
// load-time half of SPEC-0017 REQ-12: which combinations load (and what they
// store), and which fail as a located *config.Error because the prompt would
// be dropped or the delivery contradicts the argv. The spawn half, which
// checks what the child actually received, is in the supervisor package.
//
// Governing: ADR-0023, SPEC-0017 REQ-12 "Prompt Delivery".

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stump-wtf/harness/internal/core"
)

// TestCommandPromptDeliveryLoads covers every delivery path that loads, and
// that the harness keeps the configured value (empty for the default) with
// argv and prompt verbatim.
func TestCommandPromptDeliveryLoads(t *testing.T) {
	for _, tc := range []struct {
		name, body   string
		wantDelivery string
		wantEff      string
	}{
		{"argv by default", `argv = ["omp", "--print", "{{prompt}}"]
prompt = "hello world"`, "", core.PromptDeliveryArgv},
		{"argv explicit", `argv = ["tool", "--msg={{prompt}}"]
prompt = "hello"
prompt_delivery = "argv"`, "argv", core.PromptDeliveryArgv},
		{"optional placeholder counts", `argv = ["tool", "{{prompt?}}"]
prompt = "hello"`, "", core.PromptDeliveryArgv},
		{"stdin", `argv = ["tool", "--read-stdin"]
prompt = "hello"
prompt_delivery = "stdin"`, "stdin", core.PromptDeliveryStdin},
		{"file with placeholder", `argv = ["tool", "--instructions", "{{prompt_file}}"]
prompt = "hello"
prompt_delivery = "file"`, "file", core.PromptDeliveryFile},
		{"file by environment only", `argv = ["tool"]
prompt = "hello"
prompt_delivery = "file"`, "file", core.PromptDeliveryFile},
		{"scheduled stdin", `argv = ["tool"]
prompt = "hello"
prompt_delivery = "stdin"
schedule = "0 6 * * *"`, "stdin", core.PromptDeliveryStdin},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Parse([]byte("[harness.c]\nharness = \"command\"\n"+tc.body+"\n"), "t.toml")
			if err != nil {
				t.Fatalf("did not load: %v", err)
			}
			h := cfg.Harnesses["c"]
			if h.PromptDelivery != tc.wantDelivery {
				t.Errorf("PromptDelivery = %q, want %q (stored as written)", h.PromptDelivery, tc.wantDelivery)
			}
			if got := core.EffectivePromptDelivery(h.Argv, h.PromptDelivery); got != tc.wantEff {
				t.Errorf("effective delivery = %q, want %q", got, tc.wantEff)
			}
			if h.Prompt != "hello" && h.Prompt != "hello world" {
				t.Errorf("Prompt = %q, want it verbatim", h.Prompt)
			}
			if h.Restart != core.RestartNo {
				t.Errorf("Restart = %q, want the one-shot default %q", h.Restart, core.RestartNo)
			}
		})
	}
}

// TestCommandPromptFileSourceWithStdin: prompt_file is a prompt source like
// prompt, stored as its resolved PATH, never inlined (ADR-0018).
func TestCommandPromptFileSourceWithStdin(t *testing.T) {
	dir := t.TempDir()
	pf := filepath.Join(dir, "p.md")
	if err := os.WriteFile(pf, []byte("do the thing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Parse([]byte("[harness.c]\nharness = \"command\"\nargv = [\"tool\"]\nprompt_file = \""+pf+"\"\nprompt_delivery = \"stdin\"\n"), "t.toml")
	if err != nil {
		t.Fatalf("did not load: %v", err)
	}
	h := cfg.Harnesses["c"]
	if h.PromptFile != pf || h.Prompt != "" {
		t.Errorf("PromptFile = %q, Prompt = %q; want the path %q and no inlined text", h.PromptFile, h.Prompt, pf)
	}
	if !slices.Equal(h.Argv, []string{"tool"}) {
		t.Errorf("Argv = %q", h.Argv)
	}
}

// TestCommandPromptDeliveryRejections is REQ-12's validation list, each case
// a located error naming the harness. "nothing delivers" is the scenario
// "A prompt nothing delivers": the error states that the prompt would be
// dropped.
func TestCommandPromptDeliveryRejections(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       []string
	}{
		{"nothing delivers", `harness = "command"
argv = ["omp", "--print"]
prompt = "hello"`, []string{"the prompt would be dropped"}},
		{"explicit argv without placeholder", `harness = "command"
argv = ["omp", "--print"]
prompt = "hello"
prompt_delivery = "argv"`, []string{"the prompt would be dropped"}},
		{"{{prompt}} under stdin", `harness = "command"
argv = ["omp", "{{prompt}}"]
prompt = "hello"
prompt_delivery = "stdin"`, []string{`"argv[1]"`, `{{prompt}} is only available with prompt_delivery = "argv"`}},
		{"{{prompt}} under file", `harness = "command"
argv = ["omp", "{{prompt}}"]
prompt = "hello"
prompt_delivery = "file"`, []string{`"argv[1]"`, `{{prompt}} is only available`}},
		{"{{prompt_file}} under argv default", `harness = "command"
argv = ["omp", "{{prompt}}", "{{prompt_file}}"]
prompt = "hello"`, []string{`"argv[2]"`, `{{prompt_file}} is only available with prompt_delivery = "file"`}},
		{"{{prompt_file}} under stdin", `harness = "command"
argv = ["omp", "{{prompt_file}}"]
prompt = "hello"
prompt_delivery = "stdin"`, []string{`"argv[1]"`, `{{prompt_file}} is only available`}},
		{"delivery without prompt", `harness = "command"
argv = ["omp"]
prompt_delivery = "stdin"`, []string{`"prompt_delivery" is set`, `no "prompt" or "prompt_file"`}},
		{"{{prompt}} without prompt", `harness = "command"
argv = ["omp", "{{prompt}}"]`, []string{`"argv[1]"`, `has no "prompt" or "prompt_file"`}},
		{"{{prompt_file}} without prompt", `harness = "command"
argv = ["omp", "{{prompt_file}}"]`, []string{`"argv[1]"`, `has no "prompt" or "prompt_file"`}},
		{"unknown delivery", `harness = "command"
argv = ["omp"]
prompt = "hello"
prompt_delivery = "pty"`, []string{`"prompt_delivery" "pty" is not one of argv, stdin, file`}},
		{"blank delivery", `harness = "command"
argv = ["omp", "{{prompt}}"]
prompt = "hello"
prompt_delivery = " "`, []string{`"prompt_delivery" must not be blank`}},
		{"delivery on claude-code", `harness = "claude-code"
prompt = "hello"
prompt_delivery = "stdin"`, []string{`"prompt_delivery" is only accepted on harness = "command"`}},
		{"delivery on generic", `harness = "generic"
args = ["-c", "true"]
prompt_delivery = "file"`, []string{`"prompt_delivery" is only accepted on harness = "command"`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := "[harness.ok]\nharness = \"command\"\nargv = [\"true\"]\n\n[harness.bad]\n" + tc.body + "\n"
			_, err := Parse([]byte(body), "t.toml")
			assertLocated(t, err, "t.toml", 5, append([]string{`harness "bad"`}, tc.want...)...)
		})
	}
}
