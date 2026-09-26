package main

// Governing tests: SPEC-0003 REQ "Operator Notification" — the doctor's
// `notify` row for each way the hook can be off, broken, or out of step with
// the daemon.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/attach"
	"github.com/stump-wtf/harness/internal/buildinfo"
	"github.com/stump-wtf/harness/internal/cliui"
	"github.com/stump-wtf/harness/internal/config"
	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/daemon"
	"github.com/stump-wtf/harness/internal/notify"
	"github.com/stump-wtf/harness/internal/protocol"
	"github.com/stump-wtf/harness/internal/supervisor"
)

func notifyCfg(t *testing.T, mode os.FileMode) *core.Config {
	t.Helper()
	script := filepath.Join(t.TempDir(), "notify.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"), mode); err != nil {
		t.Fatal(err)
	}
	return &core.Config{Notify: core.NotifyConfig{
		Command: []string{script}, Events: core.DefaultNotifyEvents, Timeout: time.Second, Cooldown: time.Minute,
	}}
}

func TestNotifyCheckRows(t *testing.T) {
	daemonInfo := func(n *protocol.NotifyInfo) *protocol.DaemonInfo { return &protocol.DaemonInfo{Notify: n} }
	for _, tc := range []struct {
		name  string
		cfg   *core.Config
		di    *protocol.DaemonInfo
		old   bool // the daemon predates ProtoMinor 15
		level cliui.Level
		want  string
	}{
		{"off", &core.Config{}, daemonInfo(nil), false, cliui.LevelSuccess, "off"},
		// A 1.14 daemon omits Notify whatever it can do: its silence is
		// "unknown", not "configured but not running it".
		{"old daemon", notifyCfg(t, 0o755), &protocol.DaemonInfo{ProtoVersion: "1.14"}, true, cliui.LevelWarn, "predates notify (needs 1.15)"},
		{"old daemon, off", &core.Config{}, &protocol.DaemonInfo{ProtoVersion: "1.14"}, true, cliui.LevelSuccess, "off"},
		{"removed but still running", &core.Config{}, daemonInfo(&protocol.NotifyInfo{Command: "/old.sh"}), false, cliui.LevelWarn, "no longer configures"},
		{"not executable", notifyCfg(t, 0o644), daemonInfo(nil), false, cliui.LevelError, "not executable"},
		{"daemon not running it", notifyCfg(t, 0o755), daemonInfo(nil), false, cliui.LevelWarn, "not running it"},
		{"daemon down", notifyCfg(t, 0o755), nil, false, cliui.LevelSuccess, "failed, flapping"},
		{"no deliveries", notifyCfg(t, 0o755), daemonInfo(&protocol.NotifyInfo{Command: "/x"}), false, cliui.LevelSuccess, "no deliveries yet"},
		{"last failed", notifyCfg(t, 0o755), daemonInfo(&protocol.NotifyInfo{Command: "/x", Last: &protocol.NotifyDelivery{
			Event: "failed", Harness: "claude-rc", Result: "timeout", Error: "killed after 15s", At: time.Now().Format(time.RFC3339),
		}}), false, cliui.LevelWarn, "last: timeout (failed claude-rc)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := notifyCheck(tc.cfg, tc.di, !tc.old)
			if row.name != "notify" || row.level != tc.level || !strings.Contains(row.detail, tc.want) {
				t.Fatalf("row = %+v, want level %v containing %q", row, tc.level, tc.want)
			}
			if row.level != cliui.LevelSuccess && row.hint == "" {
				t.Errorf("a %v row has no hint", row.level)
			}
		})
	}
}

// `harness doctor --notify-test` against the daemon's own Server: the row
// reports the hook, and the test really ran it.
func TestDoctorNotifyTestRunsTheDaemonHook(t *testing.T) {
	cliui.SetJSON(true)
	t.Cleanup(func() { cliui.SetJSON(false) })
	tmp := t.TempDir()
	nc, out := newNotifyHook(t)
	configPath := filepath.Join(tmp, "harness.toml")
	toml := fmt.Sprintf("[harness.demo]\nharness = \"generic\"\nenabled = false\n\n[notify]\ncommand = [%q, %q]\n", nc.Command[0], nc.Command[1])
	if err := os.WriteFile(configPath, []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(shortSockDir(t), "d.sock")
	reg := attach.NewRegistry(1000)
	mgr := supervisor.NewManager(cfg, supervisor.ManagerOptions{
		StatePath: filepath.Join(tmp, "state.json"), LogDir: filepath.Join(tmp, "logs"), ExtraOutFor: reg.WriterFor,
	})
	reg.SetController(mgr)
	n := startDaemonNotify(mgr, notify.Options{})
	srv := daemon.NewServer(daemon.Options{
		Manager: mgr, Registry: reg, Notifier: n.d,
		SocketPath: socket, ConfigPath: configPath, Version: buildinfo.Version,
	})
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	t.Cleanup(func() { srv.Close(); n.Close(); mgr.Close() })

	var code int
	stdout, _ := captureStd(t, func() {
		code = runDoctorWith(verbOpts{socket: socket, configPath: configPath, json: true}, true)
	})
	if code != 0 {
		t.Fatalf("doctor exit = %d, want 0:\n%s", code, stdout)
	}
	var res doctorResult
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("doctor --json: %v\n%s", err, stdout)
	}
	if res.Notify == nil || res.Notify.Status != "ok" || !strings.Contains(res.Notify.Detail, nc.Command[0]) {
		t.Fatalf("notify row = %+v", res.Notify)
	}
	if res.NotifyTest == nil || res.NotifyTest.Status != "ok" {
		t.Fatalf("notify_test row = %+v", res.NotifyTest)
	}
	d := waitHookEvent(t, out, core.NotifyTest)
	if d.payload.Host == "" || !strings.Contains(d.env, "HARNESS_NOTIFY_EVENT=test") {
		t.Fatalf("test delivery = %+v\n%s", d.payload, d.env)
	}

	// The next doctor run reports that delivery as the last one.
	stdout, _ = captureStd(t, func() {
		runDoctorWith(verbOpts{socket: socket, configPath: configPath, json: true}, false)
	})
	res = doctorResult{}
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatal(err)
	}
	if res.Notify == nil || !strings.Contains(res.Notify.Detail, "last: ok (test)") || res.NotifyTest != nil {
		t.Fatalf("notify row after a delivery = %+v (test row %+v)", res.Notify, res.NotifyTest)
	}
}
