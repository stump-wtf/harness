package tui

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gitea.stump.rocks/stump.wtf/harness/internal/config"
	"gitea.stump.rocks/stump.wtf/harness/internal/core"
	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// TestHarnessFormRoundTrip verifies the SPEC-0001 REQ "Harness Form" write path:
// a completed form serializes to TOML that config.Parse (the daemon's parser,
// ADR-0006) accepts, yielding an equivalent harness — so "the new harness lands
// in harness.toml, the daemon reloads, and it appears on the dashboard".
func TestHarnessFormRoundTrip(t *testing.T) {
	f := HarnessForm{
		Name:         "reduit-agent",
		Harness:      "crush",
		Args:         []string{"--yolo", "--data-dir", "/tmp/x"},
		Workdir:      "~/.local/share/reduit",
		EnvFile:      "~/.config/vault/secrets.env",
		RestartDelay: 5,
		Restart:      "on-failure",
		Backend:      "native",
		Description:  "the reduit agent",
		Enabled:      true,
	}
	if err := f.Validate(); err != nil {
		t.Fatalf("valid form rejected: %v", err)
	}

	body := AppendHarness([]byte("[harness.existing]\nharness = \"generic\"\n"), f)
	cfg, err := config.Parse(body, "harness.toml")
	if err != nil {
		t.Fatalf("config.Parse rejected form TOML: %v\n---\n%s", err, body)
	}

	h, ok := cfg.Harnesses["reduit-agent"]
	if !ok {
		t.Fatalf("harness not present after parse; got %v", cfg.HarnessOrder)
	}
	if h.Adapter != "crush" {
		t.Errorf("adapter = %q, want crush", h.Adapter)
	}
	if len(h.Args) != 3 || h.Args[0] != "--yolo" {
		t.Errorf("args round-trip wrong: %v", h.Args)
	}
	if h.RestartDelay.Seconds() != 5 {
		t.Errorf("restart_delay = %v, want 5s", h.RestartDelay)
	}
	if h.Restart != core.RestartOnFailure {
		t.Errorf("restart = %q, want on-failure", h.Restart)
	}
	if !h.Enabled {
		t.Error("enabled did not round-trip")
	}
	// The pre-existing harness must survive the append (non-destructive write).
	if _, ok := cfg.Harnesses["existing"]; !ok {
		t.Error("append clobbered the existing harness")
	}
}

// TestHarnessFormRoundTripPrompt: `n` can author a prompt-only harness — the
// form emits `prompt` and no cmd/args (ADR-0011 spawn-time synthesis), and the
// TOML re-parses into a prompt harness with the one-shot restart="no" default
// intact.
func TestHarnessFormRoundTripPrompt(t *testing.T) {
	f := HarnessForm{
		Name:        "deploy-check",
		Harness:     "crush",
		Prompt:      "check the deployments and report anything unhealthy",
		Workdir:     "~/src/my-project",
		Restart:     string(core.RestartNo),
		Backend:     "native",
		Description: "one-shot agent run",
	}
	if err := f.Validate(); err != nil {
		t.Fatalf("valid prompt form rejected: %v", err)
	}
	body := f.TOML()
	if !strings.Contains(body, "prompt = ") {
		t.Fatalf("TOML missing prompt key:\n%s", body)
	}
	if strings.Contains(body, "cmd = ") || strings.Contains(body, "args = ") {
		t.Fatalf("prompt harness TOML must not carry cmd/args:\n%s", body)
	}
	cfg, err := config.Parse([]byte(body), "harness.toml")
	if err != nil {
		t.Fatalf("config.Parse rejected form TOML: %v\n---\n%s", err, body)
	}
	h, ok := cfg.Harnesses["deploy-check"]
	if !ok {
		t.Fatalf("harness not present after parse; got %v", cfg.HarnessOrder)
	}
	if h.Prompt != f.Prompt {
		t.Errorf("Prompt = %q, want %q (multi-word prompt must survive verbatim)", h.Prompt, f.Prompt)
	}
	if h.Args != nil {
		t.Errorf("Args = %v, want empty (spawn-time synthesis)", h.Args)
	}
	if h.Restart != core.RestartNo {
		t.Errorf("Restart = %q, want %q (one-shot default)", h.Restart, core.RestartNo)
	}
}

// TestEditPromptHarnessRoundTrip: editing a prompt harness with `e` and saving
// unchanged round-trips the `prompt` key losslessly — parse no longer desugars
// prompt into cmd/args, so the pre-fill sees the real field and the rewrite
// emits it back.
func TestEditPromptHarnessRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "harness.toml")
	original := "[harness.deploy-check]\nharness = \"crush\"\nprompt = \"check the deployments and report anything unhealthy\"\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	sel := protocol.HarnessInfo{
		Name:   "deploy-check",
		Prompt: "check the deployments and report anything unhealthy",
	}
	fi := editInputsFor(path, sel)
	if fi.prompt != "check the deployments and report anything unhealthy" {
		t.Fatalf("prompt not pre-filled: %q", fi.prompt)
	}

	if fi.restart != string(core.RestartNo) {
		t.Errorf("restart pre-fill = %q, want %q (one-shot default)", fi.restart, core.RestartNo)
	}

	// Save unchanged: the rewritten table must carry the prompt key and parse
	// back to the identical harness.
	form := fi.toForm()
	if err := form.Validate(); err != nil {
		t.Fatalf("unchanged edit failed validation: %v", err)
	}
	body := []byte(removeHarnessTOML(original, form.Name))
	body = AppendHarness(body, form)
	if !strings.Contains(string(body), "prompt = ") {
		t.Fatalf("prompt key lost on save:\n%s", body)
	}

	cfg, err := config.Parse(body, "harness.toml")
	if err != nil {
		t.Fatalf("edited config did not parse: %v\n%s", err, body)
	}
	before, err := config.Parse([]byte(original), "harness.toml")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Harnesses["deploy-check"], before.Harnesses["deploy-check"]) {
		t.Errorf("unchanged edit not lossless:\n got %+v\nwant %+v",
			cfg.Harnesses["deploy-check"], before.Harnesses["deploy-check"])
	}
}

// TestHarnessFormRoundTripModel: `n` can author a prompt harness with a model
// selection — the form emits `model` beside `prompt` (config truth, never a
// synthesized --model arg, issue #57) and the TOML re-parses into the same
// harness with Cmd/Args still empty.
func TestHarnessFormRoundTripModel(t *testing.T) {
	f := HarnessForm{
		Name:    "deploy-check",
		Harness: "crush",
		Prompt:  "check the deployments and report anything unhealthy",
		Model:   "claude-opus-5",
		Backend: "native",
	}
	if err := f.Validate(); err != nil {
		t.Fatalf("valid prompt+model form rejected: %v", err)
	}
	body := f.TOML()
	if !strings.Contains(body, "model = \"claude-opus-5\"") {
		t.Fatalf("TOML missing model key:\n%s", body)
	}
	if strings.Contains(body, "cmd = ") || strings.Contains(body, "args = ") {
		t.Fatalf("prompt+model harness TOML must not carry cmd/args:\n%s", body)
	}
	cfg, err := config.Parse([]byte(body), "harness.toml")
	if err != nil {
		t.Fatalf("config.Parse rejected form TOML: %v\n---\n%s", err, body)
	}
	h, ok := cfg.Harnesses["deploy-check"]
	if !ok {
		t.Fatalf("harness not present after parse; got %v", cfg.HarnessOrder)
	}
	if h.Model != "claude-opus-5" {
		t.Errorf("Model = %q, want %q", h.Model, "claude-opus-5")
	}
	if h.Prompt != f.Prompt {
		t.Errorf("Prompt = %q, want %q", h.Prompt, f.Prompt)
	}
}

// TestEditModelHarnessRoundTrip: editing a model-bearing prompt harness with
// `e` and saving unchanged round-trips both keys losslessly — the pre-fill
// sees the real fields (no synthesized args to re-persist) and the rewrite
// emits them back, so args cannot grow and the model key cannot drop per edit
// cycle.
func TestEditModelHarnessRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "harness.toml")
	original := "[harness.deploy-check]\nharness = \"crush\"\nprompt = \"check the deployments\"\nmodel = \"claude-opus-5\"\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	sel := protocol.HarnessInfo{
		Name:   "deploy-check",
		Prompt: "check the deployments",
		Model:  "claude-opus-5",
	}
	fi := editInputsFor(path, sel)
	if fi.model != "claude-opus-5" {
		t.Fatalf("model not pre-filled: %q", fi.model)
	}
	if fi.prompt != "check the deployments" {
		t.Fatalf("prompt not pre-filled: %q", fi.prompt)
	}
	if fi.args != "" {
		t.Fatalf("args pre-filled for a prompt+model harness: %q (synthesized flags must not surface)", fi.args)
	}

	// Save unchanged: the rewritten table must carry both keys and parse back
	// to the identical harness.
	form := fi.toForm()
	if err := form.Validate(); err != nil {
		t.Fatalf("unchanged edit failed validation: %v", err)
	}
	body := []byte(removeHarnessTOML(original, form.Name))
	body = AppendHarness(body, form)
	if !strings.Contains(string(body), "model = ") {
		t.Fatalf("model key lost on save:\n%s", body)
	}

	cfg, err := config.Parse(body, "harness.toml")
	if err != nil {
		t.Fatalf("edited config did not parse: %v\n%s", err, body)
	}
	before, err := config.Parse([]byte(original), "harness.toml")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Harnesses["deploy-check"], before.Harnesses["deploy-check"]) {
		t.Errorf("unchanged edit not lossless:\n got %+v\nwant %+v",
			cfg.Harnesses["deploy-check"], before.Harnesses["deploy-check"])
	}
}

// TestHarnessFormRoundTripAutoAccept: `n` can author an unattended prompt
// harness — the form emits `auto_accept = true` beside `prompt` (config truth,
// never a synthesized --yolo arg, issue #58) and the TOML re-parses into the
// same harness with Cmd/Args still empty.
func TestHarnessFormRoundTripAutoAccept(t *testing.T) {
	f := HarnessForm{
		Name:       "deploy-check",
		Harness:    "crush",
		Prompt:     "check the deployments and report anything unhealthy",
		AutoAccept: true,
		Backend:    "native",
	}
	if err := f.Validate(); err != nil {
		t.Fatalf("valid prompt+auto_accept form rejected: %v", err)
	}
	body := f.TOML()
	if !strings.Contains(body, "auto_accept = true") {
		t.Fatalf("TOML missing auto_accept key:\n%s", body)
	}
	if strings.Contains(body, "cmd = ") || strings.Contains(body, "args = ") {
		t.Fatalf("prompt+auto_accept harness TOML must not carry cmd/args:\n%s", body)
	}
	cfg, err := config.Parse([]byte(body), "harness.toml")
	if err != nil {
		t.Fatalf("config.Parse rejected form TOML: %v\n---\n%s", err, body)
	}
	h, ok := cfg.Harnesses["deploy-check"]
	if !ok {
		t.Fatalf("harness not present after parse; got %v", cfg.HarnessOrder)
	}
	if !h.AutoAccept {
		t.Error("AutoAccept = false, want true")
	}
}

// TestEditAutoAcceptHarnessRoundTrip: editing an unattended prompt harness
// with `e` and saving unchanged round-trips both keys losslessly — the
// pre-fill sees the real fields (no synthesized args to re-persist) and the
// rewrite emits them back, so args cannot grow and the auto_accept key cannot
// drop per edit cycle.
func TestEditAutoAcceptHarnessRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "harness.toml")
	original := "[harness.deploy-check]\nharness = \"crush\"\nprompt = \"check the deployments\"\nauto_accept = true\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	sel := protocol.HarnessInfo{
		Name:       "deploy-check",
		Prompt:     "check the deployments",
		AutoAccept: true,
	}
	fi := editInputsFor(path, sel)
	if !fi.autoAccept {
		t.Fatal("auto_accept not pre-filled")
	}
	if fi.args != "" {
		t.Fatalf("args pre-filled for an unattended prompt harness: %q (synthesized flags must not surface)", fi.args)
	}

	// Save unchanged: the rewritten table must carry both keys and parse back
	// to the identical harness.
	form := fi.toForm()
	if err := form.Validate(); err != nil {
		t.Fatalf("unchanged edit failed validation: %v", err)
	}
	body := []byte(removeHarnessTOML(original, form.Name))
	body = AppendHarness(body, form)
	if !strings.Contains(string(body), "auto_accept = true") {
		t.Fatalf("auto_accept key lost on save:\n%s", body)
	}

	cfg, err := config.Parse(body, "harness.toml")
	if err != nil {
		t.Fatalf("edited config did not parse: %v\n%s", err, body)
	}
	before, err := config.Parse([]byte(original), "harness.toml")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Harnesses["deploy-check"], before.Harnesses["deploy-check"]) {
		t.Errorf("unchanged edit not lossless:\n got %+v\nwant %+v",
			cfg.Harnesses["deploy-check"], before.Harnesses["deploy-check"])
	}
}

// TestHarnessFormRoundTripMaxTurns: `n` can author a turn-budgeted prompt
// harness — the form emits `max_turns = <n>` beside `prompt` (config truth,
// never a synthesized --max-turns arg, issue #59) and the TOML re-parses into
// the same harness with Cmd/Args still empty.
func TestHarnessFormRoundTripMaxTurns(t *testing.T) {
	f := HarnessForm{
		Name:     "deploy-check",
		Harness:  "crush",
		Prompt:   "check the deployments and report anything unhealthy",
		MaxTurns: 7,
		Backend:  "native",
	}
	if err := f.Validate(); err != nil {
		t.Fatalf("valid prompt+max_turns form rejected: %v", err)
	}
	body := f.TOML()
	if !strings.Contains(body, "max_turns = 7") {
		t.Fatalf("TOML missing max_turns key:\n%s", body)
	}
	if strings.Contains(body, "cmd = ") || strings.Contains(body, "args = ") {
		t.Fatalf("prompt+max_turns harness TOML must not carry cmd/args:\n%s", body)
	}
	cfg, err := config.Parse([]byte(body), "harness.toml")
	if err != nil {
		t.Fatalf("config.Parse rejected form TOML: %v\n---\n%s", err, body)
	}
	h, ok := cfg.Harnesses["deploy-check"]
	if !ok {
		t.Fatalf("harness not present after parse; got %v", cfg.HarnessOrder)
	}
	if h.MaxTurns != 7 {
		t.Errorf("MaxTurns = %d, want 7", h.MaxTurns)
	}
}

// TestEditMaxTurnsHarnessRoundTrip: editing a turn-budgeted prompt harness
// with `e` and saving unchanged round-trips the key losslessly — the pre-fill
// sees the real field (no synthesized --max-turns arg to re-persist) and the
// rewrite emits it back, so args cannot grow and the max_turns key cannot
// drop per edit cycle.
func TestEditMaxTurnsHarnessRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "harness.toml")
	original := "[harness.deploy-check]\nharness = \"crush\"\nprompt = \"check the deployments\"\nmax_turns = 7\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	sel := protocol.HarnessInfo{
		Name:     "deploy-check",
		Prompt:   "check the deployments",
		MaxTurns: 7,
	}
	fi := editInputsFor(path, sel)
	if fi.maxTurns != "7" {
		t.Fatalf("max_turns not pre-filled: %q, want \"7\"", fi.maxTurns)
	}
	if fi.args != "" {
		t.Fatalf("args pre-filled for a prompt harness: %q (synthesized flags must not surface)", fi.args)
	}

	// Save unchanged: the rewritten table must carry the key and parse back to
	// the identical harness.
	form := fi.toForm()
	if form.MaxTurns != 7 {
		t.Fatalf("toForm MaxTurns = %d, want 7", form.MaxTurns)
	}
	if err := form.Validate(); err != nil {
		t.Fatalf("unchanged edit failed validation: %v", err)
	}
	body := []byte(removeHarnessTOML(original, form.Name))
	body = AppendHarness(body, form)
	if !strings.Contains(string(body), "max_turns = 7") {
		t.Fatalf("max_turns key lost on save:\n%s", body)
	}
}

// TestHarnessFormRoundTripSchedule: a scheduled prompt harness (issue #66)
// serializes its cron expression and re-parses into the same schedule.
func TestHarnessFormRoundTripSchedule(t *testing.T) {
	f := HarnessForm{
		Name:     "stumpcloud-sweep",
		Harness:  "crush",
		Prompt:   "check all services and report anything unhealthy",
		Schedule: "0 */6 * * *",
		Restart:  "no",
		Backend:  "native",
	}
	if err := f.Validate(); err != nil {
		t.Fatalf("valid prompt+schedule form rejected: %v", err)
	}
	body := f.TOML()
	if !strings.Contains(body, `schedule = "0 */6 * * *"`) {
		t.Fatalf("TOML missing schedule key:\n%s", body)
	}
	cfg, err := config.Parse([]byte(body), "harness.toml")
	if err != nil {
		t.Fatalf("config.Parse rejected form TOML: %v\n---\n%s", err, body)
	}
	h, ok := cfg.Harnesses["stumpcloud-sweep"]
	if !ok {
		t.Fatalf("harness not present after parse; got %v", cfg.HarnessOrder)
	}
	if h.Schedule != "0 */6 * * *" {
		t.Errorf("Schedule = %q, want %q", h.Schedule, "0 */6 * * *")
	}
}

// TestHarnessFormValidateScheduleRules pins that the form mirrors the parser's
// `schedule` constraints (issue #66). The save path writes harness.toml BEFORE
// the daemon parses it, so a combination Validate lets through would leave the
// file unparseable on disk and every later reload failing.
func TestHarnessFormValidateScheduleRules(t *testing.T) {
	base := func() HarnessForm {
		return HarnessForm{
			Name:     "sweep",
			Harness:  "crush",
			Prompt:   "check things",
			Schedule: "0 */6 * * *",
			Restart:  "no",
			Backend:  "native",
		}
	}
	tests := []struct {
		name string
		bend func(*HarnessForm)
	}{
		{"schedule without prompt", func(f *HarnessForm) { f.Prompt = "" }},
		{"schedule with enabled", func(f *HarnessForm) { f.Enabled = true }},
		{"schedule with restart always", func(f *HarnessForm) { f.Restart = "always" }},
		{"schedule with restart unless-stopped", func(f *HarnessForm) { f.Restart = "unless-stopped" }},
		{"invalid cron", func(f *HarnessForm) { f.Schedule = "not-a-cron" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := base()
			tc.bend(&f)
			if err := f.Validate(); err == nil {
				t.Fatalf("Validate accepted %s; the daemon parser rejects it, so the save would corrupt harness.toml", tc.name)
			}
		})
	}
}

// TestEditScheduledHarnessRoundTrip is the regression guard for the field this
// PR added: the edit save path deletes the whole `[harness.<name>]` table and
// re-renders it from the form, so a `schedule` the form does not carry is a
// recurring job silently deleted from harness.toml on the next unrelated edit.
func TestEditScheduledHarnessRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "harness.toml")
	original := strings.Join([]string{
		"[harness.stumpcloud-sweep]",
		`harness = "crush"`,
		`prompt = "check all services and report anything unhealthy"`,
		"auto_accept = true",
		`schedule = "0 */6 * * *"`,
		`description = "scheduled sweep"`,
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	// The daemon's HarnessInfo projection carries no Schedule, so the pre-fill
	// must come from the file (ADR-0006 file-is-truth).
	sel := protocol.HarnessInfo{
		Name:        "stumpcloud-sweep",
		Prompt:      "check all services and report anything unhealthy",
		AutoAccept:  true,
		Description: "scheduled sweep",
	}
	fi := editInputsFor(path, sel)
	if fi.schedule != "0 */6 * * *" {
		t.Fatalf("schedule not pre-filled from file: %q", fi.schedule)
	}

	// Edit only the description, as a user would, and save.
	fi.description = "scheduled sweep (every 6h)"
	form := fi.toForm()
	if form.Schedule != "0 */6 * * *" {
		t.Fatalf("toForm Schedule = %q, want %q", form.Schedule, "0 */6 * * *")
	}
	if err := form.Validate(); err != nil {
		t.Fatalf("edit failed validation: %v", err)
	}
	body := []byte(removeHarnessTOML(original, form.Name))
	body = AppendHarness(body, form)
	if !strings.Contains(string(body), `schedule = "0 */6 * * *"`) {
		t.Fatalf("schedule key lost on save (the cron job would silently stop firing):\n%s", body)
	}
	cfg, err := config.Parse(body, "harness.toml")
	if err != nil {
		t.Fatalf("rewritten config no longer parses: %v\n---\n%s", err, body)
	}
	if got := cfg.Harnesses["stumpcloud-sweep"].Schedule; got != "0 */6 * * *" {
		t.Errorf("Schedule after round-trip = %q, want %q", got, "0 */6 * * *")
	}
}

// TestEditPreservesOmittedFields is the regression guard for the SPEC-0001 REQ
// "Harness Form" scenario "e SHALL pre-fill from the existing harness": editing a
// harness must NOT drop the keys the daemon's HarnessInfo projection omits
// (args/workdir/env_file/restart_delay). The edit save path rewrites the whole
// `[harness.<name>]` table, so a partial pre-fill silently wiped those keys
// (data loss). editInputsFor loads the full table from the config file
// (file-is-truth, ADR-0006) to guarantee a lossless round-trip.
func TestEditPreservesOmittedFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "harness.toml")
	original := strings.Join([]string{
		"[harness.reduit-agent]",
		`harness = "crush"`,
		`args = ["--yolo", "--data-dir", "/tmp/x"]`,
		`workdir = "~/.local/share/reduit"`,
		`env_file = "~/.config/vault/secrets.env"`,
		"restart_delay = 5",
		`restart = "on-failure"`,
		`description = "the reduit agent"`,
		"enabled = true",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	// Simulate the lossy daemon projection the TUI would have on the dashboard:
	// name/cmd/backend/description/enabled only — no args/workdir/env_file/delay.
	sel := protocol.HarnessInfo{
		Name:        "reduit-agent",
		Adapter:     "crush",
		Description: "the reduit agent",
		Enabled:     true,
	}
	fi := editInputsFor(path, sel)
	if fi.args != "--yolo --data-dir /tmp/x" {
		t.Errorf("args not pre-filled from file: %q", fi.args)
	}
	if fi.workdir != "~/.local/share/reduit" {
		t.Errorf("workdir not pre-filled: %q", fi.workdir)
	}
	if fi.envFile != "~/.config/vault/secrets.env" {
		t.Errorf("env_file not pre-filled: %q", fi.envFile)
	}
	if fi.delay != "5" {
		t.Errorf("restart_delay not pre-filled: %q", fi.delay)
	}
	if fi.restart != "on-failure" {
		t.Errorf("restart not pre-filled: %q", fi.restart)
	}

	// Now drive the full edit-save path (change only the description) and confirm
	// the omitted keys survive into the reparsed config.
	form := fi.toForm()
	form.Description = "edited description"
	body := []byte(removeHarnessTOML(original, form.Name))
	body = AppendHarness(body, form)

	cfg, err := config.Parse(body, "harness.toml")
	if err != nil {
		t.Fatalf("edited config did not parse: %v\n%s", err, body)
	}
	h, ok := cfg.Harnesses["reduit-agent"]
	if !ok {
		t.Fatalf("harness lost after edit; got %v", cfg.HarnessOrder)
	}
	if len(h.Args) != 3 || h.Args[0] != "--yolo" {
		t.Errorf("args wiped by edit: %v", h.Args)
	}
	if h.Workdir != "~/.local/share/reduit" {
		t.Errorf("workdir wiped by edit: %q", h.Workdir)
	}
	if h.EnvFile != "~/.config/vault/secrets.env" {
		t.Errorf("env_file wiped by edit: %q", h.EnvFile)
	}
	if h.RestartDelay.Seconds() != 5 {
		t.Errorf("restart_delay wiped by edit: %v", h.RestartDelay)
	}
	if h.Restart != core.RestartOnFailure {
		t.Errorf("restart wiped by edit: %q", h.Restart)
	}
	if h.Description != "edited description" {
		t.Errorf("description edit did not take: %q", h.Description)
	}
}

// TestFormValidate covers the pre-write guard rails.
func TestFormValidate(t *testing.T) {
	if err := (HarnessForm{Harness: "generic"}).Validate(); err == nil {
		t.Error("missing name should fail")
	}
	if err := (HarnessForm{Name: "x", Harness: "cursor"}).Validate(); err == nil {
		t.Error("unknown harness kind should fail")
	}
	if err := (HarnessForm{Name: "x", Harness: "generic", Backend: "bogus"}).Validate(); err == nil {
		t.Error("bad backend should fail")
	}
	if err := (HarnessForm{Name: "x", Harness: "generic", RestartDelay: -1}).Validate(); err == nil {
		t.Error("negative delay should fail")
	}
	if err := (HarnessForm{Name: "x", Harness: "generic", Restart: "until-pigs-fly"}).Validate(); err == nil {
		t.Error("bad restart policy should fail")
	}
	if err := (HarnessForm{Name: "x", Harness: "crush", Prompt: "z", Args: []string{"a"}}).Validate(); err == nil {
		t.Error("prompt+args should fail (mutually exclusive)")
	}
	if err := (HarnessForm{Name: "x", Harness: "crush", Prompt: "z", Args: []string{"a"}}).Validate(); err == nil {
		t.Error("prompt+args should fail (args belong to cmd)")
	}
	if err := (HarnessForm{Name: "x", Harness: "crush", Prompt: "do the thing"}).Validate(); err != nil {
		t.Errorf("prompt-only form should validate: %v", err)
	}
	if err := (HarnessForm{Name: "x", Harness: "generic", Model: "m"}).Validate(); err == nil {
		t.Error("cmd+model should fail (model requires prompt)")
	}
	if err := (HarnessForm{Name: "x", Harness: "crush", Prompt: "p", Model: "a b"}).Validate(); err == nil {
		t.Error("multi-token model should fail")
	}
	if err := (HarnessForm{Name: "x", Harness: "crush", Prompt: "p", Model: "claude-opus-5"}).Validate(); err != nil {
		t.Errorf("prompt+model form should validate: %v", err)
	}
	if err := (HarnessForm{Name: "x", Harness: "generic", AutoAccept: true}).Validate(); err == nil {
		t.Error("cmd+auto_accept should fail (auto_accept requires prompt)")
	}
	if err := (HarnessForm{Name: "x", Harness: "crush", Prompt: "p", AutoAccept: true}).Validate(); err != nil {
		t.Errorf("prompt+auto_accept form should validate: %v", err)
	}
	// `harness` has no default: a form that never picked one must not quietly
	// become a crush agent on save.
	if err := (HarnessForm{Name: "x", Args: []string{"-c", "sleep 1"}}).Validate(); err == nil {
		t.Error("blank harness should fail (the key is required)")
	}
}

// TestRemoveHarnessTOML verifies delete drops exactly the target table and keeps
// the rest of the file (ADR-0006 file-is-truth; SPEC-0001 delete guard).
func TestRemoveHarnessTOML(t *testing.T) {
	src := strings.Join([]string{
		"[harness.keep]",
		"harness = \"generic\"",
		"",
		"[harness.drop]",
		"harness = \"generic\"",
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
	cfg, err := config.Parse([]byte(out), "harness.toml")
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
	fi := formInputs{name: " n ", harness: " crush ", args: "a b  c", delay: "7", backend: "native"}
	f := fi.toForm()
	if f.Name != "n" || f.Harness != "crush" {
		t.Errorf("trim failed: %+v", f)
	}
	if len(f.Args) != 3 {
		t.Errorf("args = %v, want 3", f.Args)
	}
	if f.RestartDelay != 7 {
		t.Errorf("delay = %d, want 7", f.RestartDelay)
	}
}
