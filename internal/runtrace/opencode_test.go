package runtrace

// OpenCode Sources And Attribution Tests
//
// Governing tests: ADR-0033 (agent-trace reads OpenCode, so an opencode
// harness is observed here), issue #919, and SPEC-0006 REQ "Run Correlation"
// for a store that is one database for the whole CLI.

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stump-wtf/agent-trace/tail"
	_ "modernc.org/sqlite"
)

// The store is the CLI's own fixed path under the harness's home, so an
// env_file that moves HOME moves discovery with it.
func TestSourcesOpenCodeFollowsHome(t *testing.T) {
	got, err := Sources(Scope{Adapter: "opencode", Env: map[string]string{"HOME": "/home/agent"}})
	if err != nil {
		t.Fatalf("Sources(opencode): %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Sources(opencode) = %#v, want one reader", got)
	}
	a, ok := got[0].(*tail.OpenCodeAdapter)
	if !ok || a.DBPath != "/home/agent/.opencode/opencode.db" {
		t.Fatalf("Sources(opencode) = %#v, want the reader at /home/agent/.opencode/opencode.db", got[0])
	}
}

// A missing database is no sessions rather than an error, so an opencode
// harness on a host without the CLI correlates to nothing and the scrollback
// record stays its trajectory.
func TestOpenCodeWithoutADatabaseDiscoversNothing(t *testing.T) {
	srcs, err := Sources(Scope{Adapter: "opencode", Env: map[string]string{"HOME": t.TempDir()}})
	if err != nil {
		t.Fatalf("Sources: %v", err)
	}
	metas, err := srcs[0].ListSessions(context.Background())
	if err != nil || len(metas) != 0 {
		t.Fatalf("ListSessions = %d sessions, err %v; want none and no error", len(metas), err)
	}
}

// The run's session is claimed by the opencode harness whose workdir the CLI
// recorded, and by no other kind.
func TestAttributeOpenCodeSession(t *testing.T) {
	home := t.TempDir()
	work := filepath.Join(home, "work")
	db := filepath.Join(home, ".opencode", "opencode.db")
	start := time.Now().Add(-time.Minute)
	writeOpenCodeSessionDB(t, db, work, "ses_attr1", start)

	scope := Scope{Name: "oc", Adapter: "opencode", Workdir: work,
		Env: map[string]string{"HOME": home}, Runs: []Window{{Start: start.Add(-time.Second)}}}
	attr, err := Attribute(context.Background(), scope, Window{Start: start.Add(-time.Second)}, nil, time.Now())
	if err != nil {
		t.Fatalf("Attribute: %v", err)
	}
	if len(attr.Sessions) != 1 || attr.Sessions[0].Meta.ID != "ses_attr1" {
		t.Fatalf("sessions = %+v, want the one recorded for this workdir", attr.Sessions)
	}
	if got := storeOf(attr.Sessions[0].Meta); got != filepath.Join(home, ".opencode") {
		t.Errorf("storeOf = %q, want the directory holding the database", got)
	}

	// A session recorded in another directory belongs to another project.
	other := Scope{Name: "elsewhere", Adapter: "opencode", Workdir: filepath.Join(home, "other"),
		Env: map[string]string{"HOME": home}, Runs: []Window{{Start: start.Add(-time.Second)}}}
	attr2, err := Attribute(context.Background(), other, Window{Start: start.Add(-time.Second)}, nil, time.Now())
	if err != nil {
		t.Fatalf("Attribute(other): %v", err)
	}
	if len(attr2.Sessions) != 0 {
		t.Errorf("sessions = %+v, want none for a different workdir", attr2.Sessions)
	}
}

func TestCouldWriteOpenCode(t *testing.T) {
	for _, tc := range []struct {
		adapter, kind string
		want          bool
	}{
		{"opencode", "opencode", true},
		{"opencode", "crush", false},
		{"crush", "opencode", false},
		{"generic", "opencode", true},
		{"command", "opencode", true},
	} {
		if got := CouldWrite(tc.adapter, tc.kind); got != tc.want {
			t.Errorf("CouldWrite(%s, %s) = %v, want %v", tc.adapter, tc.kind, got, tc.want)
		}
	}
}

// writeOpenCodeSessionDB records one root session with a single bash tool
// call, in the schema agent-trace's OpenCode reader queries; timestamps are
// Unix milliseconds, as OpenCode writes them.
func writeOpenCodeSessionDB(t *testing.T, path, cwd, id string, at time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
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
	if _, err := db.Exec(`INSERT INTO part VALUES ('part_1', 'msg_1', ?, ?, ?)`, id,
		`{"type":"tool","tool":"bash","callID":"call_1","state":{"status":"completed","input":{"command":"ls"}}}`, ms+200); err != nil {
		t.Fatalf("tool part: %v", err)
	}
}
