package metrics

// Pi, OMP And Transcript Binding, Through The Real Observer
//
// Governing tests: SPEC-0017 REQ-4 scenario "A hand-built Claude Code argv is
// observed" and REQ-13 "Pi And OMP Adapters" (observer attribution, the
// SPEC-0013 reachability series, the root resolved through DiscoveryEnv).
//
// Like the outage test this drives the real path end to end: transcripts on
// disk, read by agent-trace's real readers inside the real observer with its
// production Sources and DiscoveryEnv, delivered to Metrics over a real
// subscription and scraped. The stores are reachable only through each
// harness's env_file — HOME points at an empty directory — so a count here
// also proves the env_file relocation was followed. A test that fed the
// collector a hand-built event would pass with the binding never wired into
// the observer's scopes at all.

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/observe"
	"github.com/stump-wtf/harness/internal/supervisor"
)

func appendFile(t *testing.T, path, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func writeEnvFile(t *testing.T, dir, kv string) string {
	t.Helper()
	path := filepath.Join(dir, "agent.env")
	if err := os.WriteFile(path, []byte(kv+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTranscriptBindingAndPiThroughTheRealObserver(t *testing.T) {
	start := time.Now().UTC().Truncate(time.Second)
	ts := func(d time.Duration) string { return start.Add(d).Format("2006-01-02T15:04:05.000Z") }
	root := t.TempDir()
	// Nothing lives under HOME: every store below is reachable only through
	// an env_file.
	t.Setenv("HOME", filepath.Join(root, "empty-home"))
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("PI_CODING_AGENT_DIR", "")
	c := &clock{t: start}
	since := supervisor.Snapshot{State: core.StateRunning, Enabled: true, PID: 4242, LastStarted: start.Add(-time.Hour)}

	// A command harness running Claude Code by hand, bound to its
	// transcripts, with CLAUDE_CONFIG_DIR relocated by its env_file.
	handWork := filepath.Join(root, "hand")
	claudeDir := filepath.Join(root, "claude-cfg")
	hand := core.Harness{Name: "hand", Adapter: core.AdapterCommand, Transcripts: "claude-code",
		Argv:    []string{"claude", "-p", "--verbose", "--output-format", "stream-json", "sweep"},
		Workdir: handWork, EnvFile: writeEnvFile(t, root, "CLAUDE_CONFIG_DIR="+claudeDir)}
	// A pi harness whose agent directory its env_file relocates.
	piWork := filepath.Join(root, "pi")
	piAgent := filepath.Join(root, "pi-agent")
	piDir := filepath.Join(root, "pi-env")
	if err := os.MkdirAll(piDir, 0o755); err != nil {
		t.Fatal(err)
	}
	pi := core.Harness{Name: "pi-worker", Adapter: core.AdapterPi, Prompt: "fix the build",
		Workdir: piWork, EnvFile: writeEnvFile(t, piDir, "PI_CODING_AGENT_DIR="+piAgent)}
	// Controls: the same argv unbound, and an omp harness, are unobserved.
	unbound := core.Harness{Name: "unbound", Adapter: core.AdapterCommand, Argv: hand.Argv, Workdir: filepath.Join(root, "unbound")}
	omp := core.Harness{Name: "omp-worker", Adapter: core.AdapterOMP, Prompt: "fix the build", Workdir: filepath.Join(root, "omp")}
	for _, d := range []string{handWork, piWork, unbound.Workdir, omp.Workdir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	src := newFakeSource()
	for _, h := range []core.Harness{hand, pi, unbound, omp} {
		src.add(h, since)
	}

	// Each session opens before the observer's first scan, so its opening is
	// history; the tool calls appended afterwards are live.
	ccPath := filepath.Join(claudeDir, "projects", "hand", "cc-hand.jsonl")
	appendFile(t, ccPath, fmt.Sprintf(`{"parentUuid":null,"isSidechain":false,"userType":"external","cwd":%q,"sessionId":"cc-hand","version":"2.0.0","type":"user","uuid":"u1","timestamp":%q,"message":{"role":"user","content":"sweep"}}`+"\n", handWork, ts(time.Second)))
	piPath := filepath.Join(piAgent, "sessions", "--pi--", "pi-1.jsonl")
	appendFile(t, piPath, fmt.Sprintf(`{"type":"session","version":3,"id":"pi-1","timestamp":%q,"cwd":%q}`+"\n"+
		`{"type":"message","id":"e1","parentId":null,"timestamp":%q,"message":{"role":"user","content":[{"type":"text","text":"fix the build"}]}}`+"\n",
		ts(time.Second), piWork, ts(time.Second)))

	obs := observe.New(src, observe.Options{
		PollInterval: 10 * time.Millisecond,
		Now:          c.Now,
		Since:        start,
		DaemonDir:    root,
		// Sources and DiscoveryEnv are left to their production defaults.
	})
	m := New(src, Options{Observer: obs, Now: c.Now})
	m.Start()
	obs.Start()
	t.Cleanup(func() {
		m.Close()
		obs.Stop()
	})
	eventually(t, "the observer's first scan", func() bool { return !obs.Stats().LastScan.IsZero() })

	c.Set(start.Add(10 * time.Second))
	appendFile(t, ccPath, fmt.Sprintf(`{"parentUuid":"u1","isSidechain":false,"userType":"external","cwd":%q,"sessionId":"cc-hand","version":"2.0.0","type":"assistant","uuid":"a1","timestamp":%q,"message":{"id":"msg_a1","type":"message","role":"assistant","model":"claude-test-model","content":[{"type":"tool_use","id":"call-1","name":"Read","input":{"file_path":"main.go"}}],"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":5}}}`+"\n"+
		`{"parentUuid":"a1","isSidechain":false,"userType":"external","cwd":%q,"sessionId":"cc-hand","version":"2.0.0","type":"user","uuid":"u2","timestamp":%q,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":"package main"}]}}`+"\n",
		handWork, ts(10*time.Second), handWork, ts(10*time.Second)))
	appendFile(t, piPath, fmt.Sprintf(`{"type":"message","id":"e2","parentId":"e1","timestamp":%q,"message":{"role":"assistant","model":"z-ai/glm-5.3-flash","content":[{"type":"toolCall","id":"call_1","name":"read","arguments":{"path":"Makefile"}}]}}`+"\n"+
		`{"type":"message","id":"e3","parentId":"e2","timestamp":%q,"message":{"role":"toolResult","toolCallId":"call_1","toolName":"read","content":[{"type":"text","text":"all: build"}],"isError":false}}`+"\n",
		ts(10*time.Second), ts(10*time.Second)))

	for _, name := range []string{"hand", "pi-worker"} {
		eventually(t, name+"'s tool call to count as a success", func() bool {
			v, _ := scrape(t, m).get("harness_model_calls_total", lbls("harness", name, "outcome", "success"))
			return v == 1
		})
	}
	fams := scrape(t, m)
	for _, name := range []string{"unbound", "omp-worker"} {
		if v, ok := fams.get("harness_model_calls_total", lbls("harness", name, "outcome", "success")); ok {
			t.Errorf("%s: model calls reported (%v); it is not observable, so the series must be absent", name, v)
		}
	}
	if st := obs.Stats(); len(st.ParseErrors) != 0 {
		t.Errorf("parse errors: %v", st.ParseErrors)
	}
}
