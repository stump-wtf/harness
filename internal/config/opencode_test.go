package config

// OpenCode Kind Tests
//
// `harness = "opencode"` is an ordinary prompt adapter: it takes the agent
// keys, and it appears in every enum the refusals list. A `command` harness
// may bind its transcripts.
//
// Governing: ADR-0033, issue #919; SPEC-0017 REQ-4 "Transcript Binding",
// REQ-17 (the amended enum).

import "testing"

func TestOpenCodeKindLoads(t *testing.T) {
	cfg, err := Parse([]byte(`[harness.oc-fix]
harness = "opencode"
model = "anthropic/claude-opus-5"
prompt = "fix the build"
auto_accept = true

[harness.oc-resident]
harness = "opencode"
args = ["serve"]
enabled = true
`), "t.toml")
	if err != nil {
		t.Fatalf("opencode harnesses did not load: %v", err)
	}
	if h := cfg.Harnesses["oc-fix"]; h.Adapter != "opencode" || h.Model != "anthropic/claude-opus-5" ||
		h.Prompt != "fix the build" || !h.AutoAccept {
		t.Errorf("oc-fix = %+v", h)
	}
	if h := cfg.Harnesses["oc-resident"]; h.Adapter != "opencode" || len(h.Args) != 1 {
		t.Errorf("oc-resident = %+v", h)
	}
}

func TestUnknownKindListsOpenCode(t *testing.T) {
	_, err := Parse([]byte("[harness.x]\nharness = \"gemini\"\n"), "t.toml")
	assertLocated(t, err, "t.toml", 1, `unknown harness kind "gemini"`, "opencode")
	_, err = Parse([]byte("[harness.x]\nargs = [\"x\"]\n"), "t.toml")
	assertLocated(t, err, "t.toml", 1, `missing required key "harness"`, "omp, opencode")
}

// transcripts = "opencode" binds a command harness, and the refusal of an
// unknown source lists opencode among the values.
func TestOpenCodeTranscriptsLoadAndRejections(t *testing.T) {
	cfg, err := Parse([]byte(`[harness.hand]
harness = "command"
argv = ["opencode", "--pure"]
transcripts = "opencode"
`), "t.toml")
	if err != nil {
		t.Fatalf("transcripts = opencode did not load: %v", err)
	}
	if h := cfg.Harnesses["hand"]; h.Transcripts != "opencode" || h.TrajectoryKind() != "opencode" {
		t.Errorf("hand: Transcripts=%q TrajectoryKind=%q", h.Transcripts, h.TrajectoryKind())
	}
	_, err = Parse([]byte(`[harness.hand]
harness = "command"
argv = ["x"]
transcripts = "gemini"
`), "t.toml")
	assertLocated(t, err, "t.toml", 1, `unknown "transcripts" source "gemini"`, "opencode")

	_, err = Parse([]byte(`[harness.oc]
harness = "opencode"
prompt = "x"
transcripts = "opencode"
`), "t.toml")
	assertLocated(t, err, "t.toml", 1, `"transcripts" is only accepted on harness = "command"`)
}
