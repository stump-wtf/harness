package daemon

// Pipe-Run Streams In The Logs View
//
// A structured one-shot has no terminal (ADR-0033 "Structured one-shots run
// on pipes"), so its run log holds only the daemon's lifecycle lines and the
// agent's stderr. Its stdout is the run's stream file, <id>.stream.jsonl.
// `harness logs NAME --run N --raw` and `harness trigger --wait` read a run
// through opLogsRun, so for such a run the reply's text is the stream's
// rendering, and once the run has ended the run log follows it, each under a
// tail-style header.
//
// The rendering is the readable one: agent-trace's StreamParser turns the
// masked records into the same entries `harness logs` renders for a traced
// run — the session, each classified tool call, the marks, and the run's
// result — and logview formats them (#915's design decision, 2026-10-25).
// Normalizing the stream is agent-trace's job (SPEC-0006: the daemon SHALL
// NOT parse the stream itself); this file only reads the file and maps what
// the parser returns onto entries, the way runtrace does for a session.
//
// A stream that parses to nothing — a harness whose adapter declares no
// stream, a file that is not one, an empty run — falls back to the raw tail,
// bounded: each line shown is cut at streamLineShown, with a note of what was
// cut, and the whole tail at streamTailBytes, oldest lines dropped first. The
// file on disk stays whole. It is read front to back holding one line at a
// time, which works for a compressed, sealed stream as well as a plain one.
//
// The order is for trigger --wait, which prints only what extends the text it
// has already printed. The stream grows while the run is live, and the run log
// block is appended once, after the run ends, so every poll's text extends the
// last one's.
//
// Governing: ADR-0033 "Structured one-shots run on pipes", "Normalization
// belongs to agent-trace" and "What reads the records"; #915's design
// decision (harness logs for a pipe run renders the stream's parsed entries,
// not the raw JSON, #279); SPEC-0017 REQ-18; SPEC-0008 REQ "Per-Run Logs";
// ADR-0008 (readers mask too).
//
// @joestump-agent 09/28/2026 - Added for
// https://github.com/stump-wtf/harness/issues/18.
//
// @joestump-agent 10/26/2026 - Readable rendering (#993, slice 1 of the #915
// split): the stream parses through agent-trace's StreamParser into logview
// entries; the raw tail is the fallback.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/stump-wtf/agent-trace/classify"
	"github.com/stump-wtf/agent-trace/redact"
	"github.com/stump-wtf/agent-trace/tail"

	"github.com/stump-wtf/harness/internal/adapter"
	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/logview"
	"github.com/stump-wtf/harness/internal/protocol"
	"github.com/stump-wtf/harness/internal/runtrace"
	"github.com/stump-wtf/harness/internal/sealedlog"
)

const (
	// streamLineShown is the most of one stream line the logs view prints.
	streamLineShown = 64 << 10
	// streamTailBytes bounds a stream tail, well inside a protocol frame.
	streamTailBytes = 8 << 20
)

// openRunArtifact opens a run artifact for reading, plain or sealed
// (compressed). It is the one place a reader opens a stream file, so reading a
// sealed one is a change here and nowhere else.
var openRunArtifact = func(path string) (io.ReadCloser, error) { return sealedlog.Open(path) }

// streamParser is the agent-trace parser that reads h's prompt stream — the
// same declaration that sent the run to pipes (supervisor.RunsOnPipes) — or
// nil when h's adapter declares no structured stream.
func streamParser(h core.Harness) tail.StreamParser {
	a := adapter.NewRegistryWithDefaults().Resolve(h)
	if adapter.PromptStreamOf(a) == "" {
		return nil
	}
	if p, ok := a.TailAdapter().(tail.StreamParser); ok {
		return p
	}
	return nil
}

// pipeStreamEntries renders a pipe run's stream file through agent-trace's
// StreamParser into the entries `harness logs` prints for a traced run: the
// session, the classified events and marks in stream order, and the run's
// result. runStart is the run record's start, the fallback stamp for a record
// that carries no timestamp.
//
// ok is false when h declares no stream, the file is missing, or the stream
// parsed to nothing — the caller then keeps the raw tail. A stream that
// failed partway still returns what parsed: a half-run is worth more than no
// run. Every field passes through redact again, as runtrace does for a
// session: the file was masked at write time, and rules may have been added
// since (ADR-0008).
func pipeStreamEntries(ctx context.Context, h core.Harness, streamPath string, runStart time.Time) ([]protocol.LogEntry, bool) {
	p := streamParser(h)
	if p == nil || streamPath == "" {
		return nil, false
	}
	f, err := openRunArtifact(streamPath)
	if err != nil {
		return nil, false
	}
	defer f.Close()

	var (
		sess   tail.SessionMeta
		events []classify.Event
		marks  []classify.Mark
		result *tail.StreamResult
		perr   error
	)
	meta, res, perr := p.ParseStream(ctx, f, tail.StreamHandler{
		Meta:  func(m tail.SessionMeta) { sess = m },
		Event: func(e classify.Event) { events = append(events, e) },
		Mark:  func(mk classify.Mark) { marks = append(marks, mk) },
	})
	if meta.ID != "" {
		sess = meta
	}
	result = res
	if perr != nil && len(events) == 0 && len(marks) == 0 && result == nil && sess.ID == "" {
		return nil, false
	}

	started := runStart
	if t, ok := sess.Started(); ok {
		started = t
	}
	sid := sess.ID
	if sid == "" {
		sid = filepath.Base(strings.TrimSuffix(streamPath, ".stream.jsonl"))
	}
	last := started
	out := make([]protocol.LogEntry, 0, len(events)+len(marks)+2)
	if sess.ID != "" {
		out = append(out, protocol.LogEntry{
			ID:      "stream/" + sid + "/session",
			Time:    started.Format(time.RFC3339Nano),
			Kind:    protocol.LogEntrySession,
			Action:  "session",
			Summary: redact.Redact(sessionSummaryLine(sess)),
			Session: sid,
		})
	}
	for _, ev := range events {
		t := streamStamp(ev.Timestamp, started)
		if t.After(last) {
			last = t
		}
		out = append(out, protocol.LogEntry{
			ID:      fmt.Sprintf("stream/%s/tool/%d", sid, ev.Seq),
			Time:    t.Format(time.RFC3339Nano),
			Kind:    protocol.LogEntryTool,
			Action:  ev.Action,
			Tool:    ev.Tool,
			Target:  redact.Redact(runtrace.PrimaryTarget(ev)),
			Summary: redact.Redact(runtrace.ToolSummary(ev.Summary)),
			Session: sid,
			Error:   ev.IsError,
		})
	}
	for i, mk := range marks {
		t := streamStamp(mk.Timestamp, started)
		if t.After(last) {
			last = t
		}
		out = append(out, protocol.LogEntry{
			ID:      fmt.Sprintf("stream/%s/mark/%d", sid, i),
			Time:    t.Format(time.RFC3339Nano),
			Kind:    protocol.LogEntryMark,
			Action:  mk.Type,
			Summary: redact.Redact(mk.Note),
			Session: sid,
			Error:   mk.Type == "error",
		})
	}
	if result != nil {
		out = append(out, protocol.LogEntry{
			ID:      "stream/" + sid + "/result",
			Time:    last.Format(time.RFC3339Nano),
			Kind:    protocol.LogEntryMark,
			Action:  "result",
			Summary: redact.Redact(resultSummary(result)),
			Session: sid,
			Error:   result.IsError,
		})
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// sessionSummaryLine names a session the way an operator scans for it: short
// id, then the agent, the model and the title when the stream recorded them.
// It is the stream's counterpart of runtrace's sessionSummary.
func sessionSummaryLine(m tail.SessionMeta) string {
	id := m.ID
	if len(id) > 8 {
		id = id[:8]
	}
	parts := []string{id, string(m.Harness)}
	if m.Model != "" {
		parts = append(parts, m.Model)
	}
	if m.Title != "" {
		parts = append(parts, m.Title)
	}
	return strings.Join(parts, " · ")
}

// resultSummary is the run's final stream record as one line: the agent's
// outcome, the turns and duration it recorded, the cost when it has one, and
// its final answer or first error. The whole answer is in the stream file.
func resultSummary(r *tail.StreamResult) string {
	out := r.Subtype
	if out == "" {
		out = "no result"
	}
	if r.Turns > 0 {
		out += fmt.Sprintf(" · %d turns", r.Turns)
	}
	if r.DurationMS > 0 {
		out += " · " + (time.Duration(r.DurationMS) * time.Millisecond).Round(time.Second).String()
	}
	if r.CostUSD > 0 {
		out += fmt.Sprintf(" · $%.2f", r.CostUSD)
	}
	switch {
	case r.Text != "":
		out += ": " + r.Text
	case r.IsError && len(r.Errors) > 0:
		out += ": " + r.Errors[0]
	}
	return out
}

// streamStamp parses an event's RFC 3339 timestamp, falling back to the
// run's start the way runtrace does for a session.
func streamStamp(ts string, fallback time.Time) time.Time {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, ts); err == nil {
			return t
		}
	}
	return fallback
}

// readStreamTail returns the last lines of a run's stream file, bounded and
// masked, and whether the file exists.
func readStreamTail(path string, lines int) (string, bool) {
	if path == "" {
		return "", false
	}
	f, err := openRunArtifact(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	tail := streamTail(f, lines, streamLineShown, streamTailBytes)
	// Written masked; masked again here, as redactTail does for logs, to
	// cover anything a rule added since. JSONLine is idempotent.
	var mask redact.Lines
	for i, ln := range tail {
		tail[i] = mask.JSONLine(ln)
	}
	if len(tail) == 0 {
		return "", true
	}
	return strings.Join(tail, "\n") + "\n", true
}

// streamTail reads r to its end and returns its last n lines (every line when
// n <= 0), each cut at lineMax bytes, dropping the oldest while the total
// exceeds budget. It holds at most one line and the kept tail at a time.
func streamTail(r io.Reader, n, lineMax, budget int) []string {
	br := bufio.NewReaderSize(r, 64<<10)
	var tail []string
	size := 0
	for {
		kept, total, err := nextLineCapped(br, lineMax)
		if total > 0 || err == nil {
			s := string(kept)
			if total > len(kept) {
				s += fmt.Sprintf(" …[%d more bytes; the whole line is in the stream file]", total-len(kept))
			}
			tail = append(tail, s)
			size += len(s)
			for len(tail) > 1 && ((n > 0 && len(tail) > n) || size > budget) {
				size -= len(tail[0])
				tail[0] = ""
				tail = tail[1:]
			}
		}
		if err != nil {
			return tail
		}
	}
}

// nextLineCapped reads one line, keeping at most max bytes of it, and reports
// its full length. err is set at the end of the stream, where kept is the
// final line if it had no newline.
func nextLineCapped(br *bufio.Reader, max int) (kept []byte, total int, err error) {
	for {
		frag, e := br.ReadSlice('\n')
		end := e == nil
		if end {
			frag = frag[:len(frag)-1]
		}
		total += len(frag)
		if room := max - len(kept); room > 0 {
			kept = append(kept, frag[:min(room, len(frag))]...)
		}
		if end {
			return kept, total, nil
		}
		if !errors.Is(e, bufio.ErrBufferFull) {
			return kept, total, e
		}
	}
}

// pipeRunText is what the logs view shows of a pipe run: its raw stream and,
// once the run has ended, its run log after it. The raw form is the fallback
// for a stream that parses to nothing.
func pipeRunText(streamPath, stream, logPath, runLog string, ended bool) string {
	text := "==> " + streamPath + " <==\n" + stream
	if !ended || runLog == "" {
		return text
	}
	return text + "\n==> " + logPath + " <==\n" + runLog
}

// pipeRunEntries is the readable form of pipeRunText: the stream's parsed
// entries, one logview line each, and the run log after it.
func pipeRunEntries(streamPath string, entries []protocol.LogEntry, logPath, runLog string, ended bool) string {
	var b strings.Builder
	b.WriteString("==> " + streamPath + " <==\n")
	for _, e := range entries {
		b.WriteString(logview.FormatEntry(e))
		b.WriteString("\n")
	}
	if ended && runLog != "" {
		b.WriteString("\n==> " + logPath + " <==\n")
		b.WriteString(runLog)
	}
	return b.String()
}
