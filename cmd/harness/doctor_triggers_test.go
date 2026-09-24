package main

// Governing: ADR-0021; SPEC-0014 REQ "Webhook Listener" (doctor flags a
// non-loopback bind without TLS, and no_listener), REQ "Credential
// Resolution" (doctor flags a readable env_file), REQ "Trigger Visibility".
// One golden case per condition, and an all-healthy case, so a silent doctor
// is shown to be able to speak (CLAUDE.md "A zero").
//
// @joestump 09/24/2026 - Introduced for stump.wtf/harness#476.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stump-wtf/harness/internal/cliui"
	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/protocol"
	"github.com/stump-wtf/harness/internal/trigger/channel/testserver"
)

// doctorTriggerConfig declares one webhook and one channel, both bound, with
// env files at the given modes.
func doctorTriggerConfig(t *testing.T, webhookMode, channelMode os.FileMode) *core.Config {
	t.Helper()
	dir := t.TempDir()
	write := func(name string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("K=v\n"), mode); err != nil {
			t.Fatal(err)
		}
		// WriteFile honours the umask; chmod pins the mode under test.
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	h := core.Harness{Name: "pr-review", Triggers: []string{"webhook.ci", "channel.sb"}}
	return &core.Config{
		Harnesses:    map[string]core.Harness{h.Name: h},
		HarnessOrder: []string{h.Name},
		Webhooks: map[string]core.WebhookSource{"ci": {
			Name: "ci", Verify: core.VerifyBearer, EnvFile: write("ci.env", webhookMode), Enabled: true,
		}},
		WebhookOrder: []string{"ci"},
		Channels: map[string]core.ChannelSource{"sb": {
			Name: "sb", URL: "https://sb.example/mcp", EnvFile: write("sb.env", channelMode), Enabled: true,
		}},
		ChannelOrder: []string{"sb"},
	}
}

// healthySources is a daemon's triggers reply with nothing wrong in it.
func healthySources() []protocol.TriggerSourceInfo {
	return []protocol.TriggerSourceInfo{
		{Source: "channel.sb", Kind: "channel", State: "connected"},
		{Source: "webhook.ci", Kind: "webhook", State: "listening"},
	}
}

func TestTriggersCheckGolden(t *testing.T) {
	loopback := &protocol.DaemonInfo{WebhookAddr: "127.0.0.1:9080"}

	cases := []struct {
		name string
		in   func(t *testing.T) triggerInputs
		want *check
	}{
		{
			name: "all healthy",
			in: func(t *testing.T) triggerInputs {
				return triggerInputs{cfg: doctorTriggerConfig(t, 0o600, 0o600), daemon: loopback, sources: healthySources()}
			},
			want: &check{name: "triggers", level: cliui.LevelSuccess, detail: "2 source(s) healthy"},
		},
		{
			name: "non-loopback webhook listener without TLS",
			in: func(t *testing.T) triggerInputs {
				return triggerInputs{cfg: doctorTriggerConfig(t, 0o600, 0o600),
					daemon: &protocol.DaemonInfo{WebhookAddr: "0.0.0.0:9080"}, sources: healthySources()}
			},
			want: &check{name: "triggers", level: cliui.LevelWarn,
				detail: "webhook listener on 0.0.0.0:9080 serves no TLS",
				hint:   "bind 127.0.0.1 behind a TLS-terminating proxy, or set [server] webhook_tls_cert_file and webhook_tls_key_file"},
		},
		{
			name: "webhook source with no listener",
			in: func(t *testing.T) triggerInputs {
				srcs := healthySources()
				srcs[1].State = "no_listener"
				return triggerInputs{cfg: doctorTriggerConfig(t, 0o600, 0o600), daemon: &protocol.DaemonInfo{}, sources: srcs}
			},
			want: &check{name: "triggers", level: cliui.LevelWarn,
				detail: "no listener serves webhook.ci",
				hint:   "set [server] webhook_listen (or --webhook-listen / HARNESS_WEBHOOK_LISTEN) and restart the daemon"},
		},
		{
			name: "group- or other-readable env_file",
			in: func(t *testing.T) triggerInputs {
				return triggerInputs{cfg: doctorTriggerConfig(t, 0o640, 0o600), daemon: loopback, sources: healthySources()}
			},
			want: &check{name: "triggers", level: cliui.LevelWarn,
				detail: "env_file readable by group or other: webhook.ci (<ci.env>)",
				hint:   "chmod 600 each env_file named"},
		},
		{
			name: "channel source in error",
			in: func(t *testing.T) triggerInputs {
				srcs := healthySources()
				srcs[0].State, srcs[0].Error = "error", "the server rejected the session credentials"
				return triggerInputs{cfg: doctorTriggerConfig(t, 0o600, 0o600), daemon: loopback, sources: srcs}
			},
			want: &check{name: "triggers", level: cliui.LevelError,
				detail: "channel.sb is in error: the server rejected the session credentials",
				hint:   "fix the channel's url or credential; `harness triggers` shows the last error"},
		},
		{
			name: "daemon down: the config alone says no listener",
			in: func(t *testing.T) triggerInputs {
				return triggerInputs{cfg: doctorTriggerConfig(t, 0o600, 0o600)}
			},
			want: &check{name: "triggers", level: cliui.LevelWarn,
				detail: "no listener serves webhook.ci",
				hint:   "set [server] webhook_listen (or --webhook-listen / HARNESS_WEBHOOK_LISTEN) and restart the daemon"},
		},
		{
			name: "nothing declared, nothing listening",
			in: func(t *testing.T) triggerInputs {
				return triggerInputs{cfg: &core.Config{}, daemon: &protocol.DaemonInfo{}}
			},
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.in(t)
			got := triggersCheck(in)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("triggersCheck = %+v, want no row", *got)
				}
				return
			}
			if got == nil {
				t.Fatal("triggersCheck = nil, want a row")
			}
			want := *tc.want
			if in.cfg != nil && len(in.cfg.Webhooks) > 0 {
				want.detail = strings.ReplaceAll(want.detail, "<ci.env>", in.cfg.Webhooks["ci"].EnvFile)
			}
			if *got != want {
				t.Errorf("triggersCheck =\n  %+v\nwant\n  %+v", *got, want)
			}
		})
	}
}

// TestDoctorTriggersRowFromARunningDaemon runs doctor itself against a daemon
// built by the daemon's own wiring, whose channel source the server refuses:
// the row comes from the state the daemon reports, over the triggers op.
func TestDoctorTriggersRowFromARunningDaemon(t *testing.T) {
	refusing := testserver.New(testserver.Options{Unauthorized: true})
	t.Cleanup(refusing.Close)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "harness.toml")
	if err := os.WriteFile(cfgPath, []byte(`
[channel.sb]
url = "`+refusing.URL+`"

[harness.pr-review]
harness = "claude-code"
prompt = "look"
triggers = ["channel.sb"]
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := visibilityConfig(t, refusing.URL)
	delete(cfg.Channels, "dead")
	cfg.ChannelOrder = []string{"sb"}
	d := startVisibilityDaemon(t, cfg)
	waitSource(t, d.sources, "channel.sb", "error")

	cliui.SetJSON(true)
	t.Cleanup(func() { cliui.SetJSON(false) })
	var code int
	out, _ := captureStdout(t, func() error {
		code = runDoctor(verbOpts{configPath: cfgPath, socket: d.socket})
		return nil
	})
	var res doctorResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("doctor --json: %v\n%s", err, out)
	}
	if res.Triggers == nil || res.Triggers.Status != cliui.LevelError.String() ||
		!strings.Contains(res.Triggers.Detail, "channel.sb is in error") {
		t.Fatalf("triggers row = %+v, want an error naming channel.sb\n%s", res.Triggers, out)
	}
	if code != 1 {
		t.Errorf("doctor exit = %d, want 1 for an error row", code)
	}
	assertNoSentinel(t, "doctor --json", out)
}
