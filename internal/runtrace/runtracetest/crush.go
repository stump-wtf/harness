// Package runtracetest builds agent tool stores on disk for tests of run
// correlation: a crush project database with sessions and messages in the shape
// crush writes them, and the projects.json registry that points at it.
//
// Governing: SPEC-0006 REQ "Run Correlation".
//
// @joestump-agent 09/11/2026 - Added for harness#302 and harness#89.
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
	db, err := sql.Open("sqlite", path)
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

// ToolCall is an assistant message's tool_call part.
func ToolCall(id, name string, input map[string]any) string {
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

func mustParts(part map[string]any) string {
	b, err := json.Marshal([]any{part})
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
