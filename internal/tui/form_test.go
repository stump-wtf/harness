package tui

import (
	"strings"
	"testing"

	"gitea.stump.rocks/stump.wtf/harness/internal/config"
)

// TestHarnessFormRoundTrip verifies the SPEC-0001 REQ "Harness Form" write path:
// a completed form serializes to TOML that config.Parse (the daemon's parser,
// ADR-0006) accepts, yielding an equivalent harness — so "the new harness lands
// in harnessd.toml, the daemon reloads, and it appears on the dashboard".
func TestHarnessFormRoundTrip(t *testing.T) {
	f := HarnessForm{
		Name:         "reduit-agent",
		Cmd:          "crush",
		Args:         []string{"--yolo", "--data-dir", "/tmp/x"},
		Workdir:      "~/.local/share/reduit",
		EnvFile:      "~/.config/vault/secrets.env",
		RestartDelay: 5,
		Backend:      "native",
		Description:  "the reduit agent",
		Enabled:      true,
	}
	if err := f.Validate(); err != nil {
		t.Fatalf("valid form rejected: %v", err)
	}

	body := AppendHarness([]byte("[harness.existing]\ncmd = \"true\"\n"), f)
	cfg, err := config.Parse(body, "harnessd.toml")
	if err != nil {
		t.Fatalf("config.Parse rejected form TOML: %v\n---\n%s", err, body)
	}

	h, ok := cfg.Harnesses["reduit-agent"]
	if !ok {
		t.Fatalf("harness not present after parse; got %v", cfg.HarnessOrder)
	}
	if h.Cmd != "crush" {
		t.Errorf("cmd = %q, want crush", h.Cmd)
	}
	if len(h.Args) != 3 || h.Args[0] != "--yolo" {
		t.Errorf("args round-trip wrong: %v", h.Args)
	}
	if h.RestartDelay.Seconds() != 5 {
		t.Errorf("restart_delay = %v, want 5s", h.RestartDelay)
	}
	if !h.Enabled {
		t.Error("enabled did not round-trip")
	}
	// The pre-existing harness must survive the append (non-destructive write).
	if _, ok := cfg.Harnesses["existing"]; !ok {
		t.Error("append clobbered the existing harness")
	}
}

// TestFormValidate covers the pre-write guard rails.
func TestFormValidate(t *testing.T) {
	if err := (HarnessForm{Cmd: "x"}).Validate(); err == nil {
		t.Error("missing name should fail")
	}
	if err := (HarnessForm{Name: "x"}).Validate(); err == nil {
		t.Error("missing cmd should fail")
	}
	if err := (HarnessForm{Name: "x", Cmd: "y", Backend: "bogus"}).Validate(); err == nil {
		t.Error("bad backend should fail")
	}
	if err := (HarnessForm{Name: "x", Cmd: "y", RestartDelay: -1}).Validate(); err == nil {
		t.Error("negative delay should fail")
	}
}

// TestRemoveHarnessTOML verifies delete drops exactly the target table and keeps
// the rest of the file (ADR-0006 file-is-truth; SPEC-0001 delete guard).
func TestRemoveHarnessTOML(t *testing.T) {
	src := strings.Join([]string{
		"[harness.keep]",
		"cmd = \"a\"",
		"",
		"[harness.drop]",
		"cmd = \"b\"",
		"description = \"gone\"",
		"",
		"[profile.p]",
		"harnesses = [\"keep\"]",
		"",
	}, "\n")

	out := removeHarnessTOML(src, "drop")
	if strings.Contains(out, "harness.drop") || strings.Contains(out, "gone") {
		t.Fatalf("drop table survived:\n%s", out)
	}
	cfg, err := config.Parse([]byte(out), "harnessd.toml")
	if err != nil {
		t.Fatalf("post-delete config invalid: %v\n%s", err, out)
	}
	if _, ok := cfg.Harnesses["keep"]; !ok {
		t.Error("keep harness was lost")
	}
	if _, ok := cfg.Harnesses["drop"]; ok {
		t.Error("drop harness still parsed")
	}
	if _, ok := cfg.Profiles["p"]; !ok {
		t.Error("profile p was lost")
	}
}

// TestToFormParsesArgsAndDelay verifies the Huh string inputs convert to the
// typed form (space-split args, integer delay).
func TestToFormParsesArgsAndDelay(t *testing.T) {
	fi := formInputs{name: " n ", cmd: " c ", args: "a b  c", delay: "7", backend: "native"}
	f := fi.toForm()
	if f.Name != "n" || f.Cmd != "c" {
		t.Errorf("trim failed: %+v", f)
	}
	if len(f.Args) != 3 {
		t.Errorf("args = %v, want 3", f.Args)
	}
	if f.RestartDelay != 7 {
		t.Errorf("delay = %d, want 7", f.RestartDelay)
	}
}
