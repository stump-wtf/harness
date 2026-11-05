package adapter

// OpenCode Adapter Tests
//
// Governing tests: ADR-0033 (an agent is read by agent-trace first, then gets
// an adapter here), ADR-0011, issue #919; SPEC-0017 REQ-4 "Transcript Binding"
// for `transcripts = "opencode"`.
//
// The session database fixture is written from agent-trace's queries against
// the OpenCode schema, not recorded from a running binary (none is installed
// on the lane host). Replace it with a recorded database when one is to hand,
// as pi_test.go records for Pi and OMP.

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stump-wtf/agent-trace/tail"
	_ "modernc.org/sqlite"

	"github.com/stump-wtf/harness/internal/core"
)

func TestOpenCodePromptArgv(t *testing.T) {
	cases := []struct {
		name     string
		opts     core.AgentOpts
		wantArgs []string
	}{
		{"with model", core.AgentOpts{Model: "anthropic/claude-opus-5"},
			[]string{"run", "--model", "anthropic/claude-opus-5", "fix the build"}},
		{"without model", core.AgentOpts{}, []string{"run", "fix the build"}},
		// auto_accept maps to --auto, which the CLI documents as approving
		// the permissions that are not explicitly denied. max_turns and
		// quiet are accepted by the config and put nothing on the argv:
		// OpenCode has neither a turn budget nor a quiet switch.
		{"auto_accept is --auto; the rest stay inert", core.AgentOpts{AutoAccept: true, MaxTurns: 40, Quiet: true},
			[]string{"run", "--auto", "fix the build"}},
		{"model with auto_accept", core.AgentOpts{AutoAccept: true, Model: "openrouter/qwen/qwen3.8-flash-next"},
			[]string{"run", "--auto", "--model", "openrouter/qwen/qwen3.8-flash-next", "fix the build"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			exe, args := (&OpenCode{}).PromptCommand("fix the build", c.opts)
			if exe != "opencode" || !slices.Equal(args, c.wantArgs) {
				t.Fatalf("PromptCommand = %q %q, want %q %q", exe, args, "opencode", c.wantArgs)
			}
		})
	}
}

func TestOpenCodeRegistered(t *testing.T) {
	r := NewRegistry()
	a, err := r.Get(core.AdapterOpenCode)
	if err != nil {
		t.Fatalf("Get(opencode): %v", err)
	}
	if a.Name() != core.AdapterOpenCode || a.Executable() != "opencode" {
		t.Errorf("Name/Executable = %q/%q, want opencode/opencode", a.Name(), a.Executable())
	}
	if got := r.Resolve(core.Harness{Adapter: core.AdapterOpenCode}); got != a {
		t.Errorf("Resolve(opencode) = %s, want the registered adapter", got.Name())
	}
	if !slices.Contains(core.HarnessKinds, core.AdapterOpenCode) {
		t.Error("opencode is not a harness kind, so no config can name it")
	}
	if !slices.Contains(core.TranscriptSources, core.AdapterOpenCode) {
		t.Error("opencode is not a transcripts source")
	}
}

// The database is the CLI's own fixed path under the harness's home: there is
// no environment variable Harness may relocate it by.
func TestOpenCodeSessionPath(t *testing.T) {
	a := &OpenCode{}
	if got := a.SessionPath("/home/agent"); got != "/home/agent/.opencode/opencode.db" {
		t.Errorf("SessionPath = %q, want the CLI's own database path", got)
	}
	if got := a.SessionPath(""); got != "" {
		t.Errorf("SessionPath(\"\") = %q, want empty when there is no home", got)
	}
	t.Setenv("HOME", "/home/agent")
	ta, ok := a.TailAdapter().(*tail.OpenCodeAdapter)
	if !ok || ta.DBPath != a.SessionPath("/home/agent") {
		t.Fatalf("TailAdapter = %#v, want an OpenCodeAdapter at %q", a.TailAdapter(), a.SessionPath("/home/agent"))
	}
}

// A command harness bound with transcripts = "opencode" is discovered,
// correlated and counted as an opencode harness, while spawn still execs its
// own argv.
func TestOpenCodeTranscriptsBinding(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	r := NewRegistry()
	h := core.Harness{Adapter: core.AdapterCommand, Argv: []string{"opencode", "--print"}, Transcripts: core.AdapterOpenCode}
	if got := r.TrajectoryAdapter(h); got.Name() != core.AdapterOpenCode {
		t.Errorf("TrajectoryAdapter = %s, want opencode", got.Name())
	}
	if r.TrajectoryAdapter(h).TailAdapter() == nil {
		t.Error("a bound opencode command harness has no tail adapter")
	}
	if got := r.Resolve(h).Name(); got != core.AdapterCommand {
		t.Errorf("Resolve = %s; a binding must never change what spawns", got)
	}
	if err := core.CheckTranscripts(core.AdapterCommand, core.AdapterOpenCode); err != nil {
		t.Errorf("CheckTranscripts(command, opencode) = %v, want nil", err)
	}
	if err := core.CheckTranscripts(core.AdapterOpenCode, core.AdapterOpenCode); err == nil {
		t.Error("transcripts on an opencode harness loaded; an adapter kind binds its own source")
	}
}

// TestOpenCodeSessionsAreReadByAgentTrace pins that the agent-trace this
// daemon is built against parses the OpenCode session database the adapter
// names. If a bump stops parsing it, an opencode harness is no longer
// observed: flip metrics.Observable and the docs back until it reads again.
func TestOpenCodeSessionsAreReadByAgentTrace(t *testing.T) {
	db := filepath.Join(t.TempDir(), "opencode.db")
	work := filepath.Join(t.TempDir(), "work")
	now := time.Now()
	writeOpenCodeSessionDB(t, db, work, "ses_obs1", now)

	metas, err := tail.OpenCodeAdapter{DBPath: db}.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(metas) != 1 {
		t.Fatalf("sessions = %d, want the one recorded", len(metas))
	}
	want := db + "/ses_obs1"
	if metas[0].Path != want || metas[0].Harness != tail.HarnessOpenCode {
		t.Errorf("session key = %q/%v, want %q as an opencode session", metas[0].Path, metas[0].Harness, want)
	}
	events, _, meta, err := tail.OpenCodeAdapter{DBPath: db}.Parse(context.Background(), want)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(events) != 1 || meta.Cwd != work {
		t.Fatalf("events = %d, cwd = %q, want one tool call recorded at %q", len(events), meta.Cwd, work)
	}
}

// writeOpenCodeSessionDB records one root session with a single bash tool
// call, in the schema agent-trace's OpenCode reader queries. OpenCode stores
// timestamps as Unix milliseconds.
func writeOpenCodeSessionDB(t *testing.T, path, cwd, id string, at time.Time) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	for _, stmt := range []string{
		`CREATE TABLE session (id TEXT PRIMARY KEY, title TEXT, directory TEXT, parent_id TEXT,
		                     model TEXT, time_created INTEGER, time_updated INTEGER)`,
		`CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT, data TEXT, time_created INTEGER)`,
		`CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT, session_id TEXT, data TEXT, time_created INTEGER)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	ms := at.UnixMilli()
	if _, err := db.Exec(`INSERT INTO session VALUES (?, 'work', ?, NULL, ?, ?, ?)`,
		id, cwd, `{"id":"anthropic/claude-opus-5","providerID":"anthropic"}`, ms, ms+1000); err != nil {
		t.Fatalf("session row: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO message VALUES ('msg_1', ?, '{"role":"user"}', ?)`, id, ms); err != nil {
		t.Fatalf("user message: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO message VALUES ('msg_2', ?, '{"role":"assistant"}', ?)`, id, ms+500); err != nil {
		t.Fatalf("assistant message: %v", err)
	}
	part := `{"type":"tool","tool":"bash","callID":"call_1","state":{"status":"completed","input":{"command":"ls"}}}`
	if _, err := db.Exec(`INSERT INTO part VALUES ('part_1', 'msg_2', ?, ?, ?)`, id, part, ms+600); err != nil {
		t.Fatalf("tool part: %v", err)
	}
}
