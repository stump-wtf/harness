package config

// Pi, OMP And Transcript Binding Tests
//
// The `harness` enum accepts pi and omp as ordinary prompt adapters, and a
// command harness may bind an adapter's transcripts. These pin the load-time
// rules; every refusal is a located *config.Error.
//
// Governing: ADR-0023, SPEC-0017 REQ-4 "Transcript Binding", REQ-13 "Pi And
// OMP Adapters", REQ-17 (the amended enum).

import (
	"os"
	"path/filepath"
	"testing"
)

// pi and omp load as prompt one-shots and as resident harnesses, taking the
// agent keys every adapter takes. auto_accept on pi is the scenario "auto_accept
// is inert": it loads (the spawn test shows it adds nothing to the argv).
func TestPiAndOMPKindsLoad(t *testing.T) {
	cfg, err := Parse([]byte(`[harness.pi-fix]
harness = "pi"
prompt = "fix the build"
auto_accept = true
max_turns = 5

[harness.omp-review]
harness = "omp"
model = "openrouter/z-ai/glm-5.3-flash"
prompt = "review the open PRs"
schedule = "0 7 * * *"

[harness.omp-resident]
harness = "omp"
args = ["--continue"]
enabled = true
`), "t.toml")
	if err != nil {
		t.Fatalf("pi/omp harnesses did not load: %v", err)
	}
	if h := cfg.Harnesses["pi-fix"]; h.Adapter != "pi" || !h.AutoAccept || h.MaxTurns != 5 {
		t.Errorf("pi-fix = %+v", h)
	}
	if h := cfg.Harnesses["omp-review"]; h.Adapter != "omp" || h.Model != "openrouter/z-ai/glm-5.3-flash" || h.Schedule == "" {
		t.Errorf("omp-review = %+v", h)
	}
	if h := cfg.Harnesses["omp-resident"]; h.Adapter != "omp" || len(h.Args) != 1 {
		t.Errorf("omp-resident = %+v", h)
	}
}

// Every refusal names the enum, so pi and omp must appear in it.
func TestUnknownKindListsPiAndOMP(t *testing.T) {
	_, err := Parse([]byte("[harness.x]\nharness = \"gemini\"\n"), "t.toml")
	assertLocated(t, err, "t.toml", 1, `unknown harness kind "gemini"`, "pi", "omp", "command")
	_, err = Parse([]byte("[harness.x]\nargs = [\"x\"]\n"), "t.toml")
	assertLocated(t, err, "t.toml", 1, `missing required key "harness"`, "pi, omp")
}

// A command harness binds any adapter's transcripts; the value is carried as
// written and TrajectoryKind reports it.
func TestCommandTranscriptsLoads(t *testing.T) {
	for _, src := range []string{"claude-code", "crush", "codex", "pi", "omp"} {
		cfg, err := Parse([]byte(`[harness.hand]
harness = "command"
argv = ["claude", "-p", "sweep"]
transcripts = "`+src+`"
`), "t.toml")
		if err != nil {
			t.Fatalf("transcripts = %q did not load: %v", src, err)
		}
		h := cfg.Harnesses["hand"]
		if h.Transcripts != src || h.TrajectoryKind() != src || h.Adapter != "command" {
			t.Errorf("transcripts %q: Transcripts=%q TrajectoryKind=%q Adapter=%q", src, h.Transcripts, h.TrajectoryKind(), h.Adapter)
		}
	}
}

func TestTranscriptsRejections(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       []string
	}{
		// Scenario "Unknown transcript source": the error lists the values.
		{"unknown source", `harness = "command"
argv = ["gemini"]
transcripts = "gemini"`, []string{`unknown "transcripts" source "gemini"`, "claude-code, crush, codex, pi, omp"}},
		// Scenario "transcripts on an adapter kind", even naming itself.
		{"on crush", `harness = "crush"
prompt = "x"
transcripts = "crush"`, []string{`"transcripts" is only accepted on harness = "command"`, `"crush" harness binds its own`}},
		{"on pi", `harness = "pi"
transcripts = "claude-code"`, []string{`"transcripts" is only accepted on harness = "command"`}},
		{"on generic", `harness = "generic"
args = ["-c", "claude -p x"]
transcripts = "claude-code"`, []string{`"transcripts" is only accepted on harness = "command"`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := "[harness.ok]\nharness = \"command\"\nargv = [\"true\"]\n\n[harness.bad]\n" + tc.body + "\n"
			_, err := Parse([]byte(body), "t.toml")
			assertLocated(t, err, "t.toml", 5, append([]string{`harness "bad"`}, tc.want...)...)
		})
	}
}

// A project file is the other config front door, through the same
// registerHarness: it accepts the binding and refuses a bad one.
func TestTranscriptsInProjectFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harness.toml")
	if err := os.WriteFile(path, []byte("[harness.hand]\nharness = \"command\"\nargv = [\"pi\", \"--print\", \"x\"]\ntranscripts = \"pi\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	proj, err := LoadProject(path)
	if err != nil {
		t.Fatalf("project command harness with transcripts did not load: %v", err)
	}
	if got := proj.Config.Harnesses["hand"].Transcripts; got != "pi" {
		t.Errorf("Transcripts = %q, want pi", got)
	}
	if err := os.WriteFile(path, []byte("[harness.hand]\nharness = \"codex\"\ntranscripts = \"codex\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = LoadProject(path)
	assertLocated(t, err, path, 1, `"transcripts" is only accepted`)
}
