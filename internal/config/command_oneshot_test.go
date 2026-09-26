package config

// Command One-Shot And Argv Template Tests
//
// A `command` harness with `schedule` or `triggers` is a one-shot with no
// prompt, and its argv[1:] elements are templates over the operator and daemon
// tiers of the run context. These tests pin the load-time half: what loads,
// what every existing one-shot exclusion still refuses, and which template
// references fail the load because no render of them could ever succeed —
// each as a located *config.Error naming the element.
//
// Governing: ADR-0023, SPEC-0017 REQ-3 "Command Harness Modes And
// Exclusions", REQ-6 "Template Grammar", REQ-7 "Template Context", REQ-10
// "Untrusted Free Text" (argv half).

import (
	"errors"
	"slices"
	"testing"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/tmpl"
)

// TestScheduledCommandNeedsNoPrompt is REQ-3's "A scheduled script with no
// prompt": the config loads with the template verbatim, as a triggered
// one-shot defaulting to restart = "no", with the run keys' defaults.
func TestScheduledCommandNeedsNoPrompt(t *testing.T) {
	cfg, err := Parse([]byte(`[harness.report]
harness = "command"
argv = ["/usr/local/bin/report", "--run", "{{run.id}}"]
schedule = "0 6 * * *"
`), "t.toml")
	if err != nil {
		t.Fatalf("scheduled command harness with no prompt did not load: %v", err)
	}
	h := cfg.Harnesses["report"]
	if want := []string{"/usr/local/bin/report", "--run", "{{run.id}}"}; !slices.Equal(h.Argv, want) {
		t.Errorf("Argv = %q, want the template stored verbatim, never a rendering", h.Argv)
	}
	if !h.Triggered() || h.IsAgent() {
		t.Errorf("Triggered/IsAgent = %v/%v, want a triggered non-prompt harness", h.Triggered(), h.IsAgent())
	}
	if h.Restart != core.RestartNo {
		t.Errorf("Restart = %q, want the one-shot default \"no\"", h.Restart)
	}
	if h.Timeout != core.DefaultRunTimeout || h.OnOverlap != core.OverlapSkip || h.KeepRuns != core.DefaultKeepRuns {
		t.Errorf("run keys = %v/%q/%d, want SPEC-0008's defaults", h.Timeout, h.OnOverlap, h.KeepRuns)
	}
}

// TestTriggeredCommandNeedsNoPrompt is REQ-3's "A triggered command harness":
// `triggers` loads without a prompt source, takes the triggered defaults, and
// may reference {{run.source}} because every event firing has one.
func TestTriggeredCommandNeedsNoPrompt(t *testing.T) {
	cfg, err := Parse([]byte(`[channel.sb]
url = "https://sb.example.com/mcp/x"

[harness.ci]
harness = "command"
argv = ["/usr/local/bin/ci", "{{run.source}}", "{{run.trigger}}"]
triggers = ["channel.sb"]
`), "t.toml")
	if err != nil {
		t.Fatalf("triggered command harness with no prompt did not load: %v", err)
	}
	h := cfg.Harnesses["ci"]
	if h.Restart != core.RestartNo || h.OnOverlap != core.OverlapQueue {
		t.Errorf("Restart/OnOverlap = %q/%q, want no/queue", h.Restart, h.OnOverlap)
	}
}

// TestCommandModelNeedsItsPlaceholder is REQ-3's "model without a
// placeholder", from the accepting side: `model` loads when argv references
// {{model}}, and is stored for the render context.
func TestCommandModelNeedsItsPlaceholder(t *testing.T) {
	cfg, err := Parse([]byte(`[harness.pi]
harness = "command"
argv = ["pi", "--model", "{{model}}"]
model = "anthropic/claude"
`), "t.toml")
	if err != nil {
		t.Fatalf("model with {{model}} in argv did not load: %v", err)
	}
	if got := cfg.Harnesses["pi"].Model; got != "anthropic/claude" {
		t.Errorf("Model = %q", got)
	}
	// Optional form counts as a reference too.
	if _, err := Parse([]byte("[harness.pi]\nharness = \"command\"\nargv = [\"pi\", \"{{model?}}\"]\nmodel = \"m\"\n"), "t.toml"); err != nil {
		t.Errorf("model with {{model?}} did not load: %v", err)
	}
}

// TestCommandOneShotExclusionsStillApply is REQ-3's "One-shot exclusions
// still apply": dropping the prompt requirement drops nothing else.
func TestCommandOneShotExclusionsStillApply(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"schedule and enabled", `schedule = "0 6 * * *"
enabled = true`, `"schedule" and "enabled = true" are mutually exclusive`},
		{"schedule and restart always", `schedule = "0 6 * * *"
restart = "always"`, `"schedule" requires restart policy`},
		{"schedule and operating_hours", `schedule = "0 6 * * *"
operating_hours = "Mon-Fri 09:00-17:00"`, `"schedule" and "operating_hours" are mutually exclusive`},
		{"timeout on a resident", `timeout = "5m"`, `"timeout" requires "schedule" or "triggers"`},
		{"triggers and enabled", `triggers = ["channel.sb"]
enabled = true`, `"triggers" and "enabled = true" are mutually exclusive`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := "[channel.sb]\nurl = \"https://sb.example.com/mcp/x\"\n\n[harness.x]\nharness = \"command\"\nargv = [\"report\"]\n" + tc.body + "\n"
			_, err := Parse([]byte(body), "t.toml")
			assertLocated(t, err, "t.toml", 4, `harness "x"`, tc.want)
		})
	}
}

// TestCommandArgvTemplateRejections covers REQ-2's "A placeholder cannot
// choose the executable" from the template side, REQ-6's located grammar and
// unknown-path errors, REQ-7's "run.id on a resident harness" and the
// required-run.source-on-a-schedule rule, REQ-10's "A title never reaches
// argv", and the paths argv cannot reach yet. Every refusal names the element.
func TestCommandArgvTemplateRejections(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       []string
	}{
		{"run.id on a resident", `argv = ["report", "--run", "{{run.id}}"]`,
			[]string{`"argv[2]"`, "no run records"}},
		{"run.trigger on a resident", `argv = ["report", "{{run.trigger}}"]`,
			[]string{`"argv[1]"`, "no run records"}},
		{"run.source on a resident", `argv = ["report", "{{run.source}}"]`,
			[]string{`"argv[1]"`, "no run records"}},
		{"required run.source on a schedule", `argv = ["report", "{{run.source}}"]
schedule = "0 6 * * *"`, []string{`"argv[1]"`, "every scheduled firing", "{{run.source?}}"}},
		{"required run.source on schedule plus triggers", `argv = ["report", "--src={{run.source}}"]
schedule = "0 6 * * *"
triggers = ["channel.sb"]`, []string{`"argv[1]"`, "every scheduled firing"}},
		{"required model without model", `argv = ["pi", "{{model}}"]`,
			[]string{`"argv[1]"`, `"model" is not set`, "{{model?}}"}},
		{"untrusted form", `argv = ["x", "{{untrusted event.title}}"]`,
			[]string{`"argv[1]"`, "untrusted text is never permitted in argv"}},
		{"bare untrusted path", `argv = ["x", "{{event.body?}}"]`,
			[]string{`"argv[1]"`, "untrusted text is never permitted in argv"}},
		{"event path not yet", `argv = ["gh", "pr", "view", "{{event.number?}}"]`,
			[]string{`"argv[3]"`, "not available in argv yet"}},
		{"prompt not yet", `argv = ["tool", "--msg={{prompt}}"]`,
			[]string{`"argv[1]"`, "prompt delivery"}},
		{"unknown path", `argv = ["x", "a", "{{harness.nmae}}"]`,
			[]string{`"argv[2]" line 1, column 1`, `unknown template path "harness.nmae"`}},
		{"located column", `argv = ["x", "--at=  {{nope}}"]`,
			[]string{`"argv[1]" line 1, column 8`}},
		{"no functions", `argv = ["x", "{{printf \"%s\" run.id}}"]`,
			[]string{`"argv[1]"`, "malformed placeholder"}},
		{"unclosed", `argv = ["x", "{{run.id"]`,
			[]string{`"argv[1]"`, "no closing"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := "[channel.sb]\nurl = \"https://sb.example.com/mcp/x\"\n\n[harness.bad]\nharness = \"command\"\n" + tc.body + "\n"
			_, err := Parse([]byte(body), "t.toml")
			assertLocated(t, err, "t.toml", 4, append([]string{`harness "bad"`}, tc.want...)...)
		})
	}
}

// TestCommandArgvGrammarErrorIsSentinel: a grammar error in argv keeps the
// tmpl sentinel through the config layer, so a caller can errors.Is it.
func TestCommandArgvGrammarErrorIsSentinel(t *testing.T) {
	_, err := Parse([]byte("[harness.x]\nharness = \"command\"\nargv = [\"x\", \"{{ nope\"]\n"), "t.toml")
	if !errors.Is(err, tmpl.ErrGrammar) {
		t.Errorf("err = %v, want it to wrap tmpl.ErrGrammar", err)
	}
}

// TestCommandArgvTemplatesThatLoad: the resident-safe and optional forms load,
// and {{literal_open}} is literal text, not a reference.
func TestCommandArgvTemplatesThatLoad(t *testing.T) {
	for _, argv := range []string{
		`["report", "{{harness.name}}", "{{harness.workdir}}", "{{run.started_at}}", "{{run.date}}"]`,
		`["report", "{{run.id?}}", "{{run.trigger?}}", "{{run.source?}}", "{{model?}}"]`,
		`["report", "{{literal_open}}x}}", "}}"]`,
		`["report", "{{ harness.name }}"]`,
	} {
		if _, err := Parse([]byte("[harness.x]\nharness = \"command\"\nargv = "+argv+"\n"), "t.toml"); err != nil {
			t.Errorf("argv %s did not load: %v", argv, err)
		}
	}
}
