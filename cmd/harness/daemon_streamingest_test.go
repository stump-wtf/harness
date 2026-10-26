package main

// A Live Stream In The Daemon's Fan-Out — End-to-End Test
//
// Governing: issue #995 (slice 3 of #915); ADR-0033 "One activity stream per
// session", "One source per session".
//
// internal/observe's tests prove the claim and the fan-out over a fake store
// and a fake normalizer; internal/supervisor's prove a pipe run hands its
// masked lines to whoever installed the seam. What neither can show is the
// daemon's own wiring: that runDaemon's observer is the consumer the Manager
// is given, that a real `claude` one-shot's calls reach subscribers attributed
// to the harness while it is still running, and — the half that decides this
// design — that they arrive once. So the fake claude here streams two calls
// AND writes the transcript of the same session, the way the real CLI does,
// then holds stdout open no longer than a run that has finished printing. The
// poller is watching the whole time with a 10ms interval, so any duplicate
// from the store route would be delivered.
//
// @joestump-agent 10/26/2026 - Added with stream-fed ingest (harness#995).

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/attach"
	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/observe"
	"github.com/stump-wtf/harness/internal/supervisor"
	"github.com/stump-wtf/harness/internal/testwait"
)

// TestDaemonStreamIngestDeliversEachCallOnce drives the daemon's real seam: a
// claude-code prompt one-shot whose stream is the only source for its session,
// even though that session's transcript exists on disk.
func TestDaemonStreamIngestDeliversEachCallOnce(t *testing.T) {
	tmp := t.TempDir()
	// Discovery reads the daemon's environment; keep claude's store out of the
	// real home, where the machine's own sessions would be listed too.
	t.Setenv("HOME", tmp)
	work := filepath.Join(tmp, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	const sid = "11112222-3333-4444-8888-aaaabbbbcccc"
	fakeClaudeOnPath(t, work, sid)

	reg := attach.NewRegistry(100)
	opts := daemonManagerOptions(reg)
	opts.StatePath = filepath.Join(tmp, "state.json")
	opts.LogDir = filepath.Join(tmp, "logs")
	opts.Policy.StopGrace = 100 * time.Millisecond
	// A scheduled prompt one-shot, which is what runs on pipes and so is what
	// has a stream to hand: a resident harness keeps its PTY (RunsOnPipes).
	h := core.Harness{
		Name:      "oneshot",
		Adapter:   "claude-code",
		Prompt:    "triage the queue",
		Workdir:   work,
		Backend:   core.BackendNative,
		Restart:   core.RestartNo,
		Schedule:  "0 3 * * *",
		OnOverlap: core.OverlapSkip,
		Timeout:   45 * time.Minute,
		Enabled:   true,
	}
	cfg := &core.Config{
		Harnesses:    map[string]core.Harness{h.Name: h},
		HarnessOrder: []string{h.Name},
		Profiles:     map[string]core.Profile{},
	}
	mgr := supervisor.NewManager(cfg, opts)
	reg.SetController(mgr)
	t.Cleanup(mgr.Close)

	obsOpts := daemonObserverOptions()
	obsOpts.PollInterval = 10 * time.Millisecond
	obs := newDaemonObserver(mgr, obsOpts)
	// The wiring itself: a daemon whose observer exists but never installs the
	// seam would stream nothing, exactly as #315's daemon wired no Policy.
	startDaemonStreamIngest(obs, mgr)
	obs.Start()
	t.Cleanup(obs.Stop)
	ch, cancel := obs.Subscribe("test", 256)
	defer cancel()

	if d, ok := mgr.StartRun(h.Name, supervisor.RunRequest{Trigger: supervisor.TriggerManual}); !ok || d.Kind != supervisor.DecisionStarted {
		t.Fatalf("the one-shot did not start: %+v ok=%v", d, ok)
	}

	// Both calls, live, from the stream — before the run is over.
	seen := map[string]int{}
	var transcript int
	deadline := time.After(testwait.Budget(t, 30*time.Second))
	for len(seen) < 2 {
		select {
		case ev := <-ch:
			if ev.Kind != observe.KindTool || ev.Session.ID != sid {
				continue
			}
			if ev.Origin != observe.OriginStream {
				transcript++
				continue
			}
			if ev.Harness != h.Name || ev.Adapter != "claude-code" {
				t.Fatalf("call %q attributed to %q (%q), want %q (claude-code)", ev.Tool.Tool, ev.Harness, ev.Adapter, h.Name)
			}
			seen[ev.Tool.Tool]++
		case <-deadline:
			t.Fatalf("only %v arrived from the run's stream; stats %+v; runs %+v", seen, obs.Stats(), mgr.Runs(h.Name))
		}
	}
	// Both of the script's calls, and nothing else: a call counted twice would
	// land in this map twice, whichever route it came from.
	if want := []string{"Read", "Bash"}; !sameKeys(want, seen) {
		t.Fatalf("stream calls = %v, want Read and Bash once each", seen)
	}

	// The transcript half: the session is on disk with both calls while the run
	// is live, and the run outlives its own stream, so the poller also gets a
	// scan after the claim is released. Neither scan may deliver either call a
	// second time — while the run is live the claim keeps the poller from
	// tracking the session at all, and at its end the claim leaves a floor.
	waitRunIdle(t, mgr, h.Name)
	drainUntil(t, ch, 2*time.Second, func(ev observe.Event) {
		if ev.Kind != observe.KindTool || ev.Session.ID != sid {
			return
		}
		if ev.Origin != observe.OriginStream {
			transcript++
			return
		}
		seen[ev.Tool.Tool]++
	})
	if transcript != 0 {
		t.Errorf("%d tool call(s) arrived a second time from the transcript store (stats %+v)", transcript, obs.Stats())
	}
	for k, n := range seen {
		if n != 1 {
			t.Errorf("call %s delivered %d times, want exactly once", k, n)
		}
	}
	// The vacuity guard below is what makes this next line mean something:
	// the run's transcript holds the same two calls, and they were not
	// delivered a second time.
	if st := obs.Stats(); st.StreamItems < 2 {
		t.Errorf("StreamItems = %d, want the two streamed calls counted", st.StreamItems)
	}
	// The vacuity guard: the transcript the poller was kept away from really
	// holds the same two calls. Without it the duplicate check above would pass
	// on a run that wrote no transcript at all.
	body, err := os.ReadFile(filepath.Join(tmp, ".claude", "projects", "p", sid+".jsonl"))
	if err != nil {
		t.Fatalf("the one-shot wrote no transcript, so nothing was there to double-count: %v", err)
	}
	for _, want := range []string{`"toolu_1"`, `"toolu_2"`, `"tool_use"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("the transcript lacks %s, so the duplicate check proves nothing:\n%s", want, head(string(body), 600))
		}
	}
}

// head bounds a dump in a failure message.
func head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func sameKeys(want []string, got map[string]int) bool {
	if len(got) != len(want) {
		return false
	}
	for _, k := range want {
		if got[k] != 1 {
			return false
		}
	}
	return true
}

// waitRunIdle blocks until name has no process in flight.
func waitRunIdle(t *testing.T, mgr *supervisor.Manager, name string) {
	t.Helper()
	deadline := time.Now().Add(testwait.Budget(t, 30*time.Second))
	for {
		snap, ok := mgr.Snapshot(name)
		if !ok {
			t.Fatalf("unknown harness %q", name)
		}
		if snap.PID == 0 && snap.State != core.StateRunning {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the run never came off the process: %+v", snap)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// drainUntil reads whatever arrives for d, handing each event to f.
func drainUntil(t *testing.T, ch <-chan observe.Event, d time.Duration, f func(observe.Event)) {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case ev := <-ch:
			f(ev)
		case <-deadline:
			return
		}
	}
}

// fakeClaudeOnPath puts a `claude` first on PATH that behaves like a real
// structured one-shot: it announces its session on stdout, writes the same
// conversation to ~/.claude/projects, streams both tool calls, then stops
// writing to stdout while it finishes up.
func fakeClaudeOnPath(t *testing.T, work, sid string) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	store := `"$HOME/.claude/projects/p"`
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' '{"type":"system","subtype":"init","cwd":%q,"session_id":%q}'
sleep 1
mkdir -p %s
ts=$(date -u +%%FT%%TZ)
{
printf '%%s\n' "{\"type\":\"user\",\"sessionId\":%q,\"cwd\":%q,\"timestamp\":\"$ts\",\"message\":{\"role\":\"user\",\"content\":\"triage the queue\"}}"
printf '%%s\n' "{\"type\":\"assistant\",\"sessionId\":%q,\"cwd\":%q,\"timestamp\":\"$ts\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"Read\",\"input\":{\"file_path\":\"%s/note.txt\"}}]}}"
printf '%%s\n' "{\"type\":\"user\",\"sessionId\":%q,\"cwd\":%q,\"timestamp\":\"$ts\",\"message\":{\"role\":\"user\",\"content\":[{\"tool_use_id\":\"toolu_1\",\"type\":\"tool_result\",\"content\":\"hello\"}]}}"
printf '%%s\n' "{\"type\":\"assistant\",\"sessionId\":%q,\"cwd\":%q,\"timestamp\":\"$ts\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"tool_use\",\"id\":\"toolu_2\",\"name\":\"Bash\",\"input\":{\"command\":\"tea pr ls\"}}]}}"
printf '%%s\n' "{\"type\":\"user\",\"sessionId\":%q,\"cwd\":%q,\"timestamp\":\"$ts\",\"message\":{\"role\":\"user\",\"content\":[{\"tool_use_id\":\"toolu_2\",\"type\":\"tool_result\",\"content\":\"(none)\"}]}}"
} >> %s/`+sid+`.jsonl
sleep 2
printf '%%s\n' '{"type":"assistant","session_id":%q,"message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"%s/note.txt"}}]}}'
printf '%%s\n' '{"type":"user","session_id":%q,"message":{"role":"user","content":[{"tool_use_id":"toolu_1","type":"tool_result","content":"hello"}]}}'
printf '%%s\n' '{"type":"assistant","session_id":%q,"message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_2","name":"Bash","input":{"command":"tea pr ls"}}]}}'
printf '%%s\n' '{"type":"user","session_id":%q,"message":{"role":"user","content":[{"tool_use_id":"toolu_2","type":"tool_result","content":"(none)"}]}}'
printf '%%s\n' '{"type":"result","subtype":"success","is_error":false,"num_turns":3,"result":"triaged","session_id":%q}'
exec 1>&-
sleep 2
`, work, sid, store,
		sid, work, sid, work, work, sid, work, sid, work, sid, work, store,
		sid, work, sid, sid, sid, sid)
	if strings.Contains(script, "MISSING") {
		t.Fatal("the fake claude's script lost an argument to Sprintf")
	}
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}
