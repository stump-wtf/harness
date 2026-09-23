// Package runtracetest builds agent tool stores on disk for tests of run
// correlation: a crush project database with sessions and messages in the shape
// crush writes them, and the projects.json registry that points at it.
//
// Governing: SPEC-0006 REQ "Run Correlation".
//
// @joestump-agent 09/11/2026 - Added for harness#302 and harness#89.
//
// @joestump-agent 09/21/2026 - AppendCrushMessages and RewriteLastCrushMessage,
// so a test can grow a live session between observer ticks (harness#390).
//
// @joestump-agent 09/23/2026 - ToolCall writes the step's finish part, the
// shape real crush writes and agent-trace v0.4.0 waits for.
package runtracetest

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite" // the driver agent-trace reads crush stores with
)

// CrushMessage is one message row. Parts is the JSON array crush stores; build
// it with ToolCall, ToolResult or FinishError.
type CrushMessage struct {
	Role  string
	At    time.Time
	Parts string
}

// CrushSession is one session row and its messages.
type CrushSession struct {
	ID       string
	Created  time.Time
	Updated  time.Time
	Messages []CrushMessage
}

// WriteCrushDB creates (or extends) a crush database at path. Timestamps are
// stored as whole Unix seconds, which is what crush actually writes.
func WriteCrushDB(tb testing.TB, path string, sessions ...CrushSession) {
	tb.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		tb.Fatal(err)
	}
	db, err := sql.Open("sqlite", writerDSN(path))
	if err != nil {
		tb.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS sessions (
			id TEXT PRIMARY KEY,
			parent_session_id TEXT,
			title TEXT NOT NULL DEFAULT '',
			message_count INTEGER NOT NULL DEFAULT 0,
			prompt_tokens INTEGER NOT NULL DEFAULT 0,
			completion_tokens INTEGER NOT NULL DEFAULT 0,
			cost REAL NOT NULL DEFAULT 0.0,
			updated_at INTEGER NOT NULL,
			created_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS messages (
			id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			role TEXT NOT NULL,
			parts TEXT NOT NULL DEFAULT '[]',
			model TEXT,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			finished_at INTEGER
		)`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			tb.Fatal(err)
		}
	}
	for _, s := range sessions {
		updated := s.Updated
		if updated.IsZero() {
			updated = s.Created
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO sessions (id, title, message_count, created_at, updated_at) VALUES (?, 'Untitled Session', ?, ?, ?)`,
			s.ID, len(s.Messages), s.Created.Unix(), updated.Unix()); err != nil {
			tb.Fatal(err)
		}
		for i, m := range s.Messages {
			if _, err := db.ExecContext(ctx,
				`INSERT INTO messages (id, session_id, role, parts, model, created_at, updated_at) VALUES (?, ?, ?, ?, 'test/model', ?, ?)`,
				fmt.Sprintf("%s-%d", s.ID, i), s.ID, m.Role, m.Parts, m.At.Unix(), m.At.Unix()); err != nil {
				tb.Fatal(err)
			}
		}
	}
}

// AppendCrushMessages adds messages to an existing session the way a live
// crush does: each row is a new insert (so its rowid follows every row already
// there), and the session's updated_at moves to the newest message — crush's
// message-count trigger touches the session row on every insert, which fires
// its updated_at trigger. That is what discovery's activity filter reads.
func AppendCrushMessages(tb testing.TB, path, sessionID string, msgs ...CrushMessage) {
	tb.Helper()
	db, err := sql.Open("sqlite", writerDSN(path))
	if err != nil {
		tb.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE session_id = ?`, sessionID).Scan(&n); err != nil {
		tb.Fatal(err)
	}
	for i, m := range msgs {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO messages (id, session_id, role, parts, model, created_at, updated_at) VALUES (?, ?, ?, ?, 'test/model', ?, ?)`,
			fmt.Sprintf("%s-%d", sessionID, n+i), sessionID, m.Role, m.Parts, m.At.Unix(), m.At.Unix()); err != nil {
			tb.Fatal(err)
		}
		if _, err := db.ExecContext(ctx,
			`UPDATE sessions SET message_count = message_count + 1, updated_at = MAX(updated_at, ?) WHERE id = ?`,
			m.At.Unix(), sessionID); err != nil {
			tb.Fatal(err)
		}
	}
}

// RewriteLastCrushMessage replaces the parts of a session's newest message in
// place, the way crush streams into an assistant row: the row is updated, not
// re-inserted, so neither its rowid nor the session's updated_at moves.
func RewriteLastCrushMessage(tb testing.TB, path, sessionID, parts string) {
	tb.Helper()
	db, err := sql.Open("sqlite", writerDSN(path))
	if err != nil {
		tb.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(context.Background(),
		`UPDATE messages SET parts = ? WHERE rowid = (SELECT MAX(rowid) FROM messages WHERE session_id = ?)`,
		parts, sessionID); err != nil {
		tb.Fatal(err)
	}
}

// writerDSN opens a store for writing with a busy timeout, as crush does: a
// test that grows a session while an observer is polling it must wait out the
// reader's shared lock rather than fail with SQLITE_BUSY.
func writerDSN(path string) string {
	return "file:" + path + "?_pragma=busy_timeout(5000)"
}

// ToolCall is an assistant message's parts for one tool call: the tool_call
// part and the finish part crush writes into the row when the model's step
// ends, before the tool runs. A call orphaned by a kill mid-call has both;
// what it lacks is the tool message holding its result. agent-trace from
// v0.4.0 holds its read cursor below an assistant row with no finish, so a
// fixture without one never delivers.
func ToolCall(id, name string, input map[string]any) string {
	in, _ := json.Marshal(input)
	return mustParts(
		map[string]any{"type": "tool_call", "data": map[string]any{
			"id": id, "name": name, "input": string(in), "finished": true,
		}},
		map[string]any{"type": "finish", "data": map[string]any{"reason": "tool_use"}},
	)
}

// UnfinishedToolCall is ToolCall without the finish part: the row a crush
// killed while the model was still streaming its step leaves behind. From
// agent-trace v0.4.0 it is the orphan shape that holds ParseSince's cursor;
// a finished call with no result is released once the session writes past it.
func UnfinishedToolCall(id, name string, input map[string]any) string {
	in, _ := json.Marshal(input)
	return mustParts(map[string]any{"type": "tool_call", "data": map[string]any{
		"id": id, "name": name, "input": string(in), "finished": true,
	}})
}

// ToolResult is a tool message's tool_result part.
func ToolResult(callID, content string) string {
	return mustParts(map[string]any{"type": "tool_result", "data": map[string]any{
		"tool_call_id": callID, "content": content,
	}})
}

// FinishError is the finish part crush writes when a turn dies on a provider
// error.
func FinishError(message, details string) string {
	return mustParts(map[string]any{"type": "finish", "data": map[string]any{
		"reason": "error", "message": message, "details": details,
	}})
}

func mustParts(parts ...map[string]any) string {
	b, err := json.Marshal(parts)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// Project is one projects.json registry entry.
type Project struct {
	Path         string
	DataDir      string
	LastAccessed time.Time
}

// WriteProjects writes a crush projects.json registry at path, followed by
// trailing — bytes appended after the document, which is the shape a
// concurrent rewrite by two crush processes leaves behind.
func WriteProjects(tb testing.TB, path, trailing string, projects ...Project) {
	tb.Helper()
	type entry struct {
		Path         string `json:"path"`
		DataDir      string `json:"data_dir"`
		LastAccessed string `json:"last_accessed"`
	}
	doc := struct {
		Projects []entry `json:"projects"`
	}{}
	for _, p := range projects {
		doc.Projects = append(doc.Projects, entry{p.Path, p.DataDir, p.LastAccessed.UTC().Format(time.RFC3339Nano)})
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		tb.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		tb.Fatal(err)
	}
	if err := os.WriteFile(path, append(b, trailing...), 0o600); err != nil {
		tb.Fatal(err)
	}
}
