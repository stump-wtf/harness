package tui

// The SPEC-0014 `triggers` round-trip. The `e` save path rewrites the whole
// [harness.<name>] table, so a key the form does not carry is a key the next
// unrelated edit silently deletes — and a deleted `triggers` is a harness that
// keeps looking healthy on the dashboard while never firing again.
//
// The source tables are the other half, and the sharper one: they are separate
// top-level tables the form has no business touching, so the test below edits
// only a description and then asserts the [channel.*] and [webhook.*] text is
// BYTE-identical, comments and key order included.
//
// Governing: ADR-0021; SPEC-0014 REQ "Triggers Round-Trip Through Config
// Writers", REQ "Triggers Key", REQ "Triggered Harness Exclusions", REQ
// "Overlap Default For Triggered Harnesses".
//
// @joestump 09/22/2026 - Introduced with the SPEC-0014 config schema (#454).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stump-wtf/harness/internal/config"
	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/protocol"
)

func triggeredForm() HarnessForm {
	f := NewHarnessForm()
	f.Name = "pr-review"
	f.Harness = "claude-code"
	f.Prompt = "review the pull request"
	f.Triggers = []string{"webhook.gitea-pr"}
	return f
}

// loadWithSources writes sourcesTOML plus body into a temp dir alongside the
// env file the sources reference, and loads it the way the daemon would. The
// real Load path is the point: a relative `env_file` resolves against the
// declaring file, which an in-memory Parse with a bare filename cannot do.
func loadWithSources(t *testing.T, body string) *core.Config {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hooks.env"),
		[]byte("GITEA_HOOK_SECRET=abc123\nSB_TOKEN=def456\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "harness.toml")
	full := sourcesTOML + "\n" + body
	if err := os.WriteFile(path, []byte(full), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("rendered TOML does not parse: %v\n%s", err, full)
	}
	return cfg
}

// TestHarnessFormRoundTripTriggers: `triggers` serializes and re-parses, and
// the on_overlap default that follows it is not written out.
func TestHarnessFormRoundTripTriggers(t *testing.T) {
	f := triggeredForm()
	f.Triggers = []string{"webhook.gitea-pr", "channel.sb"}
	if err := f.Validate(); err != nil {
		t.Fatalf("valid form rejected: %v", err)
	}
	cfg := loadWithSources(t, f.TOML())
	h := cfg.Harnesses["pr-review"]
	if len(h.Triggers) != 2 || h.Triggers[0] != "webhook.gitea-pr" || h.Triggers[1] != "channel.sb" {
		t.Errorf("Triggers round-tripped as %v", h.Triggers)
	}
	if h.OnOverlap != core.OverlapQueue {
		t.Errorf("OnOverlap = %q, want the triggered default %q", h.OnOverlap, core.OverlapQueue)
	}
	// `queue` is the default for a triggered harness, so writing it would
	// grow a key the operator never typed.
	if strings.Contains(f.TOML(), "on_overlap") {
		t.Errorf("the triggered on_overlap default was written:\n%s", f.TOML())
	}

	// And the converse: an explicit `skip`, which IS a departure from the
	// triggered default, must survive. Comparing against the schedule
	// default instead would silently drop it.
	explicit := triggeredForm()
	explicit.OnOverlap = "skip"
	if !strings.Contains(explicit.TOML(), `on_overlap = "skip"`) {
		t.Errorf("an explicit skip on a triggered harness was dropped:\n%s", explicit.TOML())
	}
	cfg2 := loadWithSources(t, explicit.TOML())
	if cfg2.Harnesses["pr-review"].OnOverlap != core.OverlapSkip {
		t.Errorf("explicit skip round-tripped as %q", cfg2.Harnesses["pr-review"].OnOverlap)
	}
}

// TestHarnessFormTriggersValidate: the form mirrors the parser, so it cannot
// write a combination the daemon would refuse to load — which would leave
// harness.toml unparseable on disk until someone hand-edited it.
func TestHarnessFormTriggersValidate(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*HarnessForm)
		want   string
	}{
		{"no prompt", func(f *HarnessForm) { f.Prompt = ""; f.Args = []string{"true"}; f.Harness = "generic" }, "triggers requires prompt"},
		{"enabled", func(f *HarnessForm) { f.Enabled = true }, "mutually exclusive"},
		{"respawning restart", func(f *HarnessForm) { f.Restart = "always" }, "restart no or on-failure"},
		{"malformed reference", func(f *HarnessForm) { f.Triggers = []string{"sb"} }, "invalid triggers entry"},
		{"duplicate reference", func(f *HarnessForm) { f.Triggers = []string{"webhook.gh", "webhook.gh"} }, "twice"},
		{"catch_up with only a webhook", func(f *HarnessForm) { f.CatchUp = true }, "catch_up requires"},
		// A resident hours-gated harness: operating_hours alone does not make
		// catch_up meaningful, and TOML() would silently drop the key.
		{"catch_up on a resident operating_hours harness", func(f *HarnessForm) {
			f.Triggers = nil
			f.OperatingHours = "Mon-Fri 09:00-17:00"
			f.CatchUp = true
		}, "catch_up requires"},
		{"hours_shutdown on a triggered harness", func(f *HarnessForm) {
			f.OperatingHours = "Mon-Fri 09:00-17:00"
			f.HoursShutdown = "immediate"
		}, "not accepted on a triggered harness"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := triggeredForm()
			tc.mutate(&f)
			err := f.Validate()
			if err == nil {
				t.Fatalf("form accepted a combination the parser rejects")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// sourcesTOML is the source half of the fixture below, kept as its own
// constant so the byte-identity assertion has something exact to compare
// against. The comments and the deliberately non-alphabetical key order are
// the point: a writer that round-tripped these tables through a TOML encoder
// would reproduce the VALUES and destroy everything else.
const sourcesTOML = `# The Gitea webhook that fires pull-request reviews.
[webhook.gitea-pr]
verify   = "gitea"
secret   = "${GITEA_HOOK_SECRET}"   # resolved from env_file, never inline
env_file = "hooks.env"
events   = ["pull_request"]
max_body = "2MiB"

[channel.sb]
url     = "https://sb.example.com/mcp/x"
headers = { Authorization = "Bearer ${SB_TOKEN}" }
# Deliberately after headers, and deliberately not aligned with it.
env_file = "hooks.env"
`

// TestEditingADescriptionLeavesSourceTablesUntouched is the behavioral half of
// REQ "Triggers Round-Trip Through Config Writers". It drives the real save
// path — removeHarnessTOML followed by AppendHarness, exactly what
// saveHarnessCmd does — over a file carrying both source kinds, changing only
// the description.
//
// Asserting on the WRITTEN FILE rather than on the re-parsed config is
// load-bearing. A parse-and-compare would pass against a writer that
// re-emitted the source tables from the parsed values: same meaning, every
// comment gone, `${SB_TOKEN}` replaced by the resolved token. The only check
// that catches that is byte equality on the text.
func TestEditingADescriptionLeavesSourceTablesUntouched(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hooks.env"),
		[]byte("GITEA_HOOK_SECRET=abc123\nSB_TOKEN=def456\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "harness.toml")
	original := sourcesTOML + `
[harness.pr-review]
harness = "claude-code"
prompt = "review the pull request"
triggers = ["webhook.gitea-pr", "channel.sb"]
timeout = "20m"
description = "before"
`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	// The pre-fill the `e` form builds, then the one edit an operator made.
	fi := editInputsFor(path, protocol.HarnessInfo{Name: "pr-review"})
	form := fi.toForm()
	if len(form.Triggers) != 2 {
		t.Fatalf("the edit pre-fill lost triggers: %v", form.Triggers)
	}
	form.Description = "after"
	if err := form.Validate(); err != nil {
		t.Fatalf("pre-filled form does not validate: %v", err)
	}

	body := []byte(removeHarnessTOML(string(original), form.Name))
	body = AppendHarness(body, form)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Byte-identical source tables, comments and spacing included.
	if !strings.Contains(string(written), sourcesTOML) {
		t.Errorf("the source tables were rewritten.\n--- want verbatim ---\n%s\n--- got file ---\n%s", sourcesTOML, written)
	}
	// The resolved credentials must not have been written back into the file
	// in place of their references — the worst possible form of "rewrote the
	// table", since it commits a secret.
	for _, secret := range []string{"abc123", "def456"} {
		if strings.Contains(string(written), secret) {
			t.Errorf("a resolved credential was written into harness.toml")
		}
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("the rewritten file does not parse: %v\n%s", err, written)
	}
	h := cfg.Harnesses["pr-review"]
	if h.Description != "after" {
		t.Errorf("Description = %q, want the edit to have applied", h.Description)
	}
	if len(h.Triggers) != 2 || h.Triggers[0] != "webhook.gitea-pr" || h.Triggers[1] != "channel.sb" {
		t.Errorf("Triggers = %v, want both preserved across the edit", h.Triggers)
	}
	if h.Timeout.String() != "20m0s" {
		t.Errorf("Timeout = %v, want the run key preserved", h.Timeout)
	}
	if len(cfg.Channels) != 1 || len(cfg.Webhooks) != 1 {
		t.Errorf("sources after the edit: %d channels, %d webhooks", len(cfg.Channels), len(cfg.Webhooks))
	}
}
