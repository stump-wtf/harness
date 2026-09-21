package observe

// Orphaned Tool Call Fallback
//
// agent-trace's ParseSince never lets its watermark pass a tool call that has
// no result yet, which is what keeps a long-running call from being dropped.
// But a call that will never get a result — crush killed mid-call by a daemon
// restart or a crash, then resumed into the same session — pins the watermark
// below it for good, and everything the session writes afterwards is withheld
// forever: the provider error marks this package exists to surface included.
// tail.Watcher has the same flaw. The real fix (release a call a later turn has
// superseded) belongs upstream in agent-trace; this is the observer-side
// fallback that keeps harness correct until then.
//
// Detection is layered so the healthy path pays nothing:
//
//  1. A read is "held" when ParseSince returns the watermark it was given and
//     nothing else. Every idle session is held, so this alone means nothing.
//  2. It is suspect only if the session's EndedAt moved since the watermark
//     was last seen held — something was written. That is free: it is the
//     metadata ParseSince already returned.
//  3. A suspect session is checked cheaply: are there two or more records
//     past the watermark? A tool call legitimately in flight, and a row crush
//     is still streaming into, are each one record. One query or one bounded
//     file read.
//  4. Only then, and at most once per StallCheckInterval per session, a full
//     Parse: the session is stalled when it holds a mark (a user message, a
//     provider error) timestamped after everything ParseSince has returned.
//     No in-flight call is ever followed by a mark, so this is the proof that
//     the session moved on without the call.
//
// Recovery skips the first record past the watermark — the orphaned call's —
// and resumes ParseSince just after it, repeating while the full Parse still
// shows marks nothing has returned. The items that follow are then delivered by
// ParseSince itself, through the same floors, baseline, attribution and
// redaction as any other read, exactly once, in seq order, and numbered with
// the positional seq a full Parse gives them: an unresolved call consumes no
// seq in agent-trace, so skipping it changes no number after it. That keeps
// seq stable across daemon lifetimes, which the telemetry export (#391) keys
// on.
//
// The orphaned call itself is not delivered. It never ran to completion, and
// it has no stable seq to carry: a full Parse flushes unresolved calls after
// every resolved one, so the number it would get moves each time the session
// grows. Anything else sharing its record goes with it — in practice nothing,
// since crush writes a call's row and its results' row separately and a killed
// turn's row ends in the call. A call whose record precedes the orphan's and
// whose result lands after it is dropped too; crush never writes that
// interleaving.
//
// Once agent-trace releases superseded calls itself, ParseSince returns those
// marks and advances, so step 4 never finds a mark newer than what it
// returned and nothing is ever skipped: the fallback retires by construction,
// with no flag to remove.
//
// Governing: issue #390; ADR-0007 (a stalled session must not stall the
// observer: every step here is bounded by SourceTimeout and rate-limited).
//
// @joestump-agent 09/21/2026 - Added: orphaned tool calls pinned a resumed
// session's watermark forever.

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/stump-wtf/agent-trace/classify"
	"github.com/stump-wtf/agent-trace/tail"
	_ "modernc.org/sqlite" // agent-trace's crush driver; registered here too for crushRecordsAfter
)

// DefaultStallCheckInterval bounds how often a suspect session is fully
// parsed. A stalled session is live by definition, so without a bound every
// write to it would re-parse the whole transcript.
const DefaultStallCheckInterval = 30 * time.Second

// maxStallSkips bounds the records one check may skip. Each is an orphaned
// call; a session with more resumes past them at the next check.
const maxStallSkips = 8

// errNoRecordReader is returned for an adapter whose watermark encoding this
// package does not know; such a session gets no stall fallback.
var errNoRecordReader = errors.New("no record reader for this adapter")

// unstick runs after a held incremental read. ended is the session's EndedAt
// from that read (zero when the adapter reported none). It returns the items
// recovered past an orphaned call, having advanced st's cursor over them.
func (o *Observer) unstick(ctx context.Context, st *session, ip tail.IncrementalParser, ended, now time.Time) []item {
	if !ended.After(st.pinEnded) {
		return nil
	}
	past, err := recordsAfter(ctx, st.adapter, st.path, st.watermark, 2)
	if err != nil || len(past) < 2 {
		// Quiet, one legitimately open record, or no way to look: nothing to
		// do until the session writes again.
		st.pinEnded = ended
		return nil
	}
	if !st.checkedAt.IsZero() && now.Sub(st.checkedAt) < o.opts.StallCheckInterval {
		return nil // pinEnded stays behind, so a later poll looks again
	}
	st.checkedAt, st.pinEnded = now, ended
	o.count(func(s *Stats) { s.StallChecks++ })
	_, marks, _, err := st.adapter.Parse(ctx, st.path)
	if err != nil || !marksAfter(marks, st.lastTS) {
		return nil
	}
	o.count(func(s *Stats) { s.Stalls++ })
	o.log.Debug("agent observer: session stalled behind an orphaned tool call", "session", st.id)
	var out []item
	for i := 0; i < maxStallSkips && marksAfter(marks, st.lastTS); i++ {
		next, err := recordsAfter(ctx, st.adapter, st.path, st.watermark, 1)
		if err != nil || len(next) == 0 {
			break
		}
		events, mks, meta, wm, err := ip.ParseSince(ctx, st.path, next[0], st.nextSeq)
		if err != nil || wm < next[0] {
			break // a rewrite or a failed read; the normal path deals with it
		}
		o.count(func(s *Stats) { s.OrphansSkipped++ })
		st.watermark = wm
		st.nextSeq += len(events)
		got := merge(events, mks)
		st.note(got)
		out = append(out, got...)
		if t, ok := parseTime(meta.EndedAt); ok {
			st.pinEnded = t
		}
	}
	return out
}

// note records the newest timestamp among items read from st.
func (st *session) note(items []item) {
	for _, it := range items {
		if t, ok := parseTime(it.ts); ok && t.After(st.lastTS) {
			st.lastTS = t
		}
	}
}

// marksAfter reports a mark timestamped strictly after t. Second resolution
// makes a mark in the same second as the newest item read invisible here; the
// session's next write brings it back.
func marksAfter(marks []classify.Mark, t time.Time) bool {
	for _, m := range marks {
		if ts, ok := parseTime(m.Timestamp); ok && ts.After(t) {
			return true
		}
	}
	return false
}

// recordsAfter returns up to n watermarks past wm, the k-th resuming just
// after the k-th record past wm. It knows the encodings agent-trace documents
// for the adapters harness resolves: a crush watermark is a messages.rowid, a
// Claude Code or Codex watermark is the byte offset of a JSONL record boundary.
// It dispatches on the adapter's declared harness, not its Go type, so a
// wrapped adapter keeps the fallback.
func recordsAfter(ctx context.Context, a tail.Adapter, path string, wm int64, n int) ([]int64, error) {
	switch a.Harness() {
	case tail.HarnessCrush:
		return crushRecordsAfter(ctx, path, wm, n)
	case tail.HarnessClaudeCode, tail.HarnessCodex:
		return jsonlRecordsAfter(path, wm, n)
	}
	return nil, errNoRecordReader
}

// crushRecordsAfter lists the rowids of the session's next n messages. path is
// agent-trace's "<db>/<session id>".
func crushRecordsAfter(ctx context.Context, path string, wm int64, n int) ([]int64, error) {
	i := strings.LastIndex(path, "/")
	if i < 0 {
		return nil, fmt.Errorf("not a crush session path: %s", path)
	}
	dbPath, sessionID := path[:i], path[i+1:]
	// sql.Open is lazy and the first query would create a missing file.
	if _, err := os.Stat(dbPath); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	rows, err := db.QueryContext(ctx,
		`SELECT rowid FROM messages WHERE session_id = ? AND rowid > ? ORDER BY rowid LIMIT ?`, sessionID, wm, n)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var row int64
		if err := rows.Scan(&row); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// jsonlRecordsAfter returns the offsets just past the next n complete lines
// after wm. A trailing line with no newline is still being written and is not
// a record yet. Lines are skipped over, never held: a tool result line can be
// megabytes.
func jsonlRecordsAfter(path string, wm int64, n int) ([]int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(wm, io.SeekStart); err != nil {
		return nil, err
	}
	r := bufio.NewReaderSize(f, 64<<10)
	pos := wm
	var out []int64
	for len(out) < n {
		chunk, err := r.ReadSlice('\n')
		pos += int64(len(chunk))
		switch {
		case err == nil:
			out = append(out, pos)
		case errors.Is(err, bufio.ErrBufferFull):
			// The line continues past the buffer; keep scanning it.
		case errors.Is(err, io.EOF):
			return out, nil
		default:
			return out, err
		}
	}
	return out, nil
}
