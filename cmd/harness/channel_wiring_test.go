package main

// The channel-listener wiring test.
//
// It drives the source manager THE DAEMON builds (startDaemonSources) against
// a real fake MCP server and the Manager daemonManagerOptions builds, and
// follows one doorbell all the way to a started run with its own `0600` event
// file. Both halves have to be the daemon's own, for the #315 reason: a test
// that assembled either would pass against a daemon that wired neither.
//
// This is the laptop-behind-NAT path end to end: the daemon dials out, so
// there is no inbound port anywhere in this test.
//
// Governing: ADR-0021; SPEC-0014 REQ "Channel Listener Session", REQ "Channel
// Notification Handling", REQ "Firing", REQ "Event Delivery To The Run".
//
// @joestump 09/23/2026 - Introduced with the SPEC-0014 channel listener (#471).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/attach"
	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/supervisor"
	"github.com/stump-wtf/harness/internal/trigger"
	"github.com/stump-wtf/harness/internal/trigger/channel/testserver"
)

func TestDaemonWiringFiresAHarnessFromAChannelDoorbell(t *testing.T) {
	srv := testserver.New(testserver.Options{})
	// Registered before the manager's cleanup so it runs after it: cleanups
	// are LIFO, and httptest.Server.Close waits for the open GET stream.
	t.Cleanup(srv.Close)

	tmp := t.TempDir()
	reg := attach.NewRegistry(100)
	opts := daemonManagerOptions(reg)
	opts.StatePath = filepath.Join(tmp, "state.json")
	opts.LogDir = filepath.Join(tmp, "logs")
	opts.JobsDir = filepath.Join(tmp, "jobs")
	opts.Policy.StopGrace = 200 * time.Millisecond
	fields := filepath.Join(tmp, "fields")

	h := core.Harness{
		Name: "pr-review", Adapter: "generic", Backend: core.BackendNative,
		// The value is teed to a sidecar file as well as printed: the run's
		// log interleaves the daemon's own lifecycle lines with the PTY's
		// output, and under load a "state changed" line lands inside the
		// hard-wrapped path, so the log cannot be where it is read.
		Args:    []string{"-c", `printf 'F:EVENT=[%s]\n' "${HARNESS_EVENT_FILE-unset}" | tee '` + fields + `'`},
		Restart: core.RestartNo,

		Triggers:  []string{"channel.sb"},
		Timeout:   core.DefaultRunTimeout,
		OnOverlap: core.OverlapQueue,
		KeepRuns:  core.DefaultKeepRuns,
	}
	cfg := &core.Config{
		Harnesses:    map[string]core.Harness{h.Name: h},
		Profiles:     map[string]core.Profile{},
		HarnessOrder: []string{h.Name},
		Channels:     map[string]core.ChannelSource{"sb": {Name: "sb", URL: srv.URL, Enabled: true}},
		ChannelOrder: []string{"sb"},
	}

	mgr := supervisor.NewManager(cfg, opts)
	t.Cleanup(mgr.Close)
	reg.SetController(mgr)
	if err := mgr.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	sources := startDaemonSources(mgr)
	t.Cleanup(sources.Close)

	deadline := time.Now().Add(10 * time.Second)
	for {
		st, ok := sources.StatusOf("channel.sb")
		if ok && st.State == trigger.StateConnected {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the channel source never connected: %+v", sources.Status())
		}
		time.Sleep(10 * time.Millisecond)
	}

	const sentinel = "PR #9 opened Chai8aeYai"
	srv.PushNotification(`{"content":"` + sentinel + `","meta":{"todo_id":"t-1"}}`)

	var rec supervisor.RunRecord
	deadline = time.Now().Add(15 * time.Second)
	for {
		runs := mgr.Runs("pr-review")
		if len(runs) == 1 && runs[0].Outcome != supervisor.OutcomeRunning {
			rec = runs[0]
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the doorbell never produced a finished run: %+v", runs)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rec.Outcome != supervisor.OutcomeSuccess {
		t.Fatalf("run outcome = %s", rec.Outcome)
	}
	if rec.Trigger != supervisor.TriggerChannel || rec.Source != "channel.sb" {
		t.Errorf("record = %+v, want trigger channel and source channel.sb", rec)
	}
	if rec.EventID == "" {
		t.Error("the run record carries no event id")
	}

	eventFile := strings.TrimSuffix(mgr.RunLogPath("pr-review", rec.RunID), ".log") + ".event.json"
	info, err := os.Stat(eventFile)
	if err != nil {
		t.Fatalf("no event file at %s: %v", eventFile, err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("event file mode = %o, want 600", perm)
	}
	body, err := os.ReadFile(eventFile)
	if err != nil {
		t.Fatal(err)
	}
	env, err := trigger.ParseEnvelope(body, 0)
	if err != nil {
		t.Fatalf("the event file does not parse: %v", err)
	}
	if env.Channel == nil || env.Channel.Content != sentinel || env.Channel.Meta["todo_id"] != "t-1" {
		t.Errorf("event file = %+v, want the doorbell verbatim", env.Channel)
	}

	// The spawned process was told where the file is, and the doorbell's text
	// reached that file and nothing else.
	runLog, err := os.ReadFile(mgr.RunLogPath("pr-review", rec.RunID))
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(fields)
	if err != nil {
		t.Fatalf("the spawned process wrote no fields file: %v", err)
	}
	if want := "F:EVENT=[" + eventFile + "]"; !strings.Contains(string(got), want) {
		t.Errorf("the spawned process did not receive HARNESS_EVENT_FILE:\nwant %q\ngot  %q", want, got)
	}
	if strings.Contains(string(runLog), sentinel) {
		t.Error("the doorbell's text reached the run's log")
	}

	// No inbound port was opened anywhere: this is the dial-out path.
	if cfg.Server.WebhookListen != "" {
		t.Error("the channel path should need no listener")
	}
}
