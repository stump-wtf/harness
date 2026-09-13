// Package sessionguard detects a crush session wedged on context-limit
// errors and archives the store so the next start begins a fresh session.
//
// A long-running crush harness keeps one session for its whole life. When
// that session outgrows the model's context window, every assistant turn
// fails immediately with a provider context-limit error while the process,
// the daemon, and every health surface look perfectly healthy — the harness
// accepts events and answers none of them (stump.wtf/harness#347). A
// restart does not help: crush resumes the oversized session from its
// store. The only recovery is to move the store aside so a fresh one is
// created, which is what Archive does.
//
// Detection is driven off the observed failure, never a message count: a
// session is stalled when every assistant turn inside the lookback window
// failed with a context-limit error. One big tool result can reach the
// limit far sooner than a chattier session, so a fixed count threshold is
// wrong in both directions.
package sessionguard

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// patterns are the substrings a crush assistant turn carries when the
// provider rejected the prompt for size. They span the two error shapes
// observed in the wild: the raw provider Bad Request body and litellm's
// typed ContextWindowExceededError. Matching is case-insensitive.
var patterns = []string{
	"exceeds max length",
	"contextwindowexceedederror",
	"context window exceeded",
	"prompt is too long",
	"maximum context length",
}

// GuardStatus is what Stalled observed in a store.
type GuardStatus struct {
	Stalled bool // every assistant turn in the window was a context error
	Turns   int  // assistant turns seen inside the window
	Errors  int  // of those, how many were context-limit errors
}

// StorePath resolves the crush project store for a harness working
// directory: <workdir>/.crush/crush.db, where crush writes unless a config
// relocates data_directory. Empty when no store exists (the harness is not
// crush, has never started, or keeps its store elsewhere) — callers skip
// the harness rather than guess at a relocated path.
func StorePath(workdir string) string {
	db := filepath.Join(workdir, ".crush", "crush.db")
	if _, err := os.Stat(db); err != nil {
		return ""
	}
	return db
}

// Stalled opens the store read-only and classifies the assistant turns
// inside the lookback window ending at now. Zero assistant turns in the
// window (an idle harness, or a store with no recent activity) is healthy:
// silence is not failure. Turns is the assistant-turn count, Errors the
// subset that failed with a context-limit error; Stalled is true when
// Errors > 0 and Errors == Turns.
func Stalled(dbPath string, lookback time.Duration, now time.Time) (GuardStatus, error) {
	var status GuardStatus
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=ro&_busy_timeout=5000", dbPath))
	if err != nil {
		return status, fmt.Errorf("sessionguard: open store: %w", err)
	}
	defer db.Close()

	since := now.Add(-lookback).Unix()
	rows, err := db.Query(
		`SELECT parts FROM messages WHERE role = 'assistant' AND created_at >= ? ORDER BY rowid`,
		since,
	)
	if err != nil {
		return status, fmt.Errorf("sessionguard: query messages: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var parts string
		if err := rows.Scan(&parts); err != nil {
			return status, fmt.Errorf("sessionguard: scan message: %w", err)
		}
		status.Turns++
		if isContextError(parts) {
			status.Errors++
		}
	}
	if err := rows.Err(); err != nil {
		return status, fmt.Errorf("sessionguard: iterate messages: %w", err)
	}
	status.Stalled = status.Turns > 0 && status.Errors == status.Turns
	return status, nil
}

// isContextError reports whether a message's parts blob carries a
// context-limit failure.
func isContextError(parts string) bool {
	lower := strings.ToLower(parts)
	for _, p := range patterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// Archive moves the store (and its WAL/SHM siblings, so a hot database
// cannot resurrect the oversized session) aside to
// <name>.oversized-<unix-nano>, preserving it — the archived store is the
// only record of what the worker was doing, and is evidence. Returns the
// primary archive path.
func Archive(dbPath string, now time.Time) (string, error) {
	suffix := fmt.Sprintf(".oversized-%d", now.UnixNano())
	archived := dbPath + suffix
	for _, src := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if _, err := os.Stat(src); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", fmt.Errorf("sessionguard: stat %s: %w", src, err)
		}
		if err := os.Rename(src, dbPath+suffix+strings.TrimPrefix(src, dbPath)); err != nil {
			return "", fmt.Errorf("sessionguard: archive %s: %w", src, err)
		}
	}
	return archived, nil
}
