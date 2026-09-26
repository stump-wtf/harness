// The live preview's readable mode for claude-code harnesses (issue #13).
//
// A headless claude-code harness runs `claude -p --verbose
// --output-format stream-json`, and everything that program prints lands on
// the harness's PTY — which the dashboard's preview mirrors faithfully. For
// that backend the mirror is a wall of JSON objects: heartbeat pings every
// thirty seconds, cache-token bookkeeping, parent_tool_use_id plumbing, with
// the one line a human cares about (a tool call carrying its human-written
// description) buried among them. From the dashboard there is no way to tell
// "quiet but fine" from "actually stuck" without parsing JSON by eye.
//
// The fix stays inside the adapter, never in the daemon and never in the TUI:
// ADR-0021's "payloads are opaque" holds precisely because no generic layer
// interprets a backend's bytes. The claude-code adapter knows it is talking
// to Claude Code and may interpret its own stream. It does so through the
// PeekFormatter extension below: a stateful, line-oriented transformer that
// the preview runs the guest's bytes through before they reach the emulator.
// Every other adapter simply has no formatter, and the preview keeps its
// byte-faithful mirror.
//
// Governing: ADR-0011 (agent adapters), ADR-0021 (backend payloads are
// opaque), SPEC-0001 REQ "Dashboard" (live preview).
//
// Rendering rules (issue #13):
//   - tool_progress / task_progress entries carrying heartbeat:true collapse
//     into a single in-place line "[N heartbeats over Xs]" that updates as
//     each ping lands — a ticking count is what separates "quiet but fine"
//     from "actually stuck".
//   - an assistant tool_use block leads with its human-written
//     input.description, falling back to the tool name.
//   - assistant text blocks are the agent's own commentary and are shown as
//     plain text.
//   - anything unmodeled (unknown event kinds, non-JSON terminal output,
//     escape sequences) passes through byte-for-byte, so the transform can
//     never lose or corrupt output it does not understand.
//
// @joestump-agent 09/26/2026 - Implemented the stream-json peek formatter
// for the claude-code adapter, plus the TUI wiring and tests.
package adapter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// PeekFormatter transforms a guest's raw PTY bytes into the bytes the live
// preview should render instead. It is inherently stateful — stream-json
// lines split across reads, and the heartbeat tally spans many lines — so an
// implementation must be used for exactly one session and reset (or replaced)
// when that session resets.
type PeekFormatter interface {
	FormatPTY(chunk []byte) []byte
}

// PeekFormatterProvider is the optional Adapter extension an adapter
// implements when it can render its own output readably for the dashboard's
// preview. The interface is optional so the adapters that have nothing to
// say stay byte-faithful by construction.
type PeekFormatterProvider interface {
	PeekFormatter() PeekFormatter
}

// PeekFormatterFor resolves the preview formatter for an adapter name, or
// nil when that adapter renders nothing: unknown names, Generic, and every
// adapter that has not opted in keep the preview's raw mirror.
func PeekFormatterFor(name string) PeekFormatter {
	a, err := NewRegistry().Get(name)
	if err != nil {
		return nil
	}
	if p, ok := a.(PeekFormatterProvider); ok {
		return p.PeekFormatter()
	}
	return nil
}

// PeekFormatter implements the extension for claude-code: its stream is
// Claude Code's stream-json, whose shape the issue documents.
func (a *ClaudeCode) PeekFormatter() PeekFormatter {
	return &streamJSONRenderer{}
}

// streamJSONLineCap bounds the partial-line buffer. A guest that writes
// endlessly without ever emitting a newline (a full-screen TUI redraw, or
// binary traffic) must not pin memory or wedge the renderer; past the cap
// the buffered bytes pass through untouched and the line buffer restarts.
const streamJSONLineCap = 64 << 10

// streamJSONRenderer is the claude-code PeekFormatter. It buffers partial
// lines across chunks, renders the modeled Claude Code event kinds, and
// passes everything else through verbatim.
type streamJSONRenderer struct {
	buf []byte

	// Heartbeat tally. A heartbeat paints a tally line once and then
	// rewrites it in place on every ping (the cursor stays at the line's
	// end — no trailing newline — until the run ends), so N pings cost one
	// line of screen, not N.
	hbCount   int
	hbElapsed float64
	hbLive    bool
	hbLen     int
}

// streamEvent is the envelope of every Claude Code stream-json line. Only
// the fields the preview renders are named; everything else in each object
// is ignored, and an object that fails to unmarshal at all passes through
// raw rather than being dropped.
type streamEvent struct {
	Type string `json:"type"`

	// assistant / user messages.
	Message *struct {
		Content []streamBlock `json:"content"`
	} `json:"message,omitempty"`

	// tool_progress / task_progress.
	Heartbeat          bool    `json:"heartbeat"`
	ElapsedTimeSeconds float64 `json:"elapsed_time_seconds"`
	ToolName           string  `json:"tool_name"`

	// result.
	Subtype      string  `json:"subtype"`
	IsError      bool    `json:"is_error"`
	DurationMs   int64   `json:"duration_ms"`
	NumTurns     int     `json:"num_turns"`
	TotalCostUsd float64 `json:"total_cost_usd"`
}

// streamBlock is one content block of an assistant message.
type streamBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// blockInput carries the tool_use input fields the preview reads.
type blockInput struct {
	Description string `json:"description"`
}

// FormatPTY renders a chunk of the guest's PTY output.
func (r *streamJSONRenderer) FormatPTY(chunk []byte) []byte {
	var out []byte
	r.buf = append(r.buf, chunk...)
	for {
		i := bytes.IndexByte(r.buf, '\n')
		if i < 0 {
			break
		}
		line := r.buf[:i]
		r.buf = r.buf[i+1:]
		out = append(out, r.renderLine(line)...)
	}
	if len(r.buf) > streamJSONLineCap {
		// The tally line, if one is live, must not bleed into the
		// passthrough bytes: close it first.
		out = append(out, r.closeTally()...)
		out = append(out, r.buf...)
		r.buf = r.buf[:0]
	}
	return out
}

// renderLine renders one complete PTY line. The trailing \r of a CRLF pair
// is stripped before anything looks at the content.
func (r *streamJSONRenderer) renderLine(line []byte) []byte {
	line = bytes.TrimRight(line, "\r")
	if len(bytes.TrimSpace(line)) == 0 {
		return nil
	}
	if line[0] != '{' {
		// Terminal escape output, a shell error, anything that is not
		// JSON: the preview is a faithful mirror for it.
		return r.passthrough(line)
	}
	var ev streamEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		// Looks like JSON but is not parseable: passthrough keeps the
		// bytes on screen rather than eating them.
		return r.passthrough(line)
	}
	switch ev.Type {
	case "tool_progress", "task_progress":
		if ev.Heartbeat {
			return r.renderHeartbeat(ev)
		}
		// A progress event without the heartbeat flag is not modeled;
		// showing it raw beats guessing at its shape.
		return r.passthrough(line)
	case "assistant":
		return r.renderAssistant(ev)
	case "user", "system", "stream_event":
		// Tool results, init banners and partial-delta plumbing are the
		// noise the preview exists to hide.
		return r.closeTally()
	case "result":
		return append(r.closeTally(), r.renderResult(ev)...)
	default:
		return r.passthrough(line)
	}
}

// passthrough closes any live heartbeat tally and copies the line through.
func (r *streamJSONRenderer) passthrough(line []byte) []byte {
	out := r.closeTally()
	out = append(out, line...)
	return append(out, '\n')
}

// closeTally terminates a heartbeat run: the cursor has been sitting at the
// tally line's end, so moving off costs exactly one newline.
func (r *streamJSONRenderer) closeTally() []byte {
	if !r.hbLive {
		return nil
	}
	r.hbLive, r.hbCount, r.hbElapsed, r.hbLen = false, 0, 0, 0
	return []byte{'\n'}
}

// renderHeartbeat paints or rewrites the single tally line. The first ping
// paints it; every later ping moves the cursor back with \r and overwrites
// the previous count, padding to erase the longer text it replaced.
func (r *streamJSONRenderer) renderHeartbeat(ev streamEvent) []byte {
	r.hbCount++
	r.hbElapsed = ev.ElapsedTimeSeconds
	text := []byte(r.tallyText())
	if r.hbLive {
		out := []byte{'\r'}
		out = append(out, text...)
		if len(text) < r.hbLen {
			out = append(out, bytes.Repeat([]byte{' '}, r.hbLen-len(text))...)
		}
		r.hbLen = len(text)
		return out
	}
	r.hbLive = true
	r.hbLen = len(text)
	// No trailing newline: the cursor must stay on this line for the \r
	// updates to land on it.
	return text
}

// tallyText renders the collapsed heartbeat line. The span shown is the
// latest ping's elapsed_time_seconds — the tool has been running that long.
func (r *streamJSONRenderer) tallyText() string {
	noun := "heartbeats"
	if r.hbCount == 1 {
		noun = "heartbeat"
	}
	return fmt.Sprintf("[%d %s over %s]", r.hbCount, noun, humanSeconds(r.hbElapsed))
}

// renderAssistant turns an assistant message into readable lines: text
// blocks verbatim, tool_use blocks led by their human-written description.
func (r *streamJSONRenderer) renderAssistant(ev streamEvent) []byte {
	out := r.closeTally()
	if ev.Message == nil {
		return out
	}
	for _, b := range ev.Message.Content {
		switch b.Type {
		case "text":
			out = append(out, strings.TrimRight(b.Text, "\n")...)
			out = append(out, '\n')
		case "tool_use":
			var in blockInput
			_ = json.Unmarshal(b.Input, &in)
			lead := in.Description
			if lead == "" {
				lead = b.Name
			}
			out = append(out, "▸ "...)
			out = append(out, lead...)
			out = append(out, '\n')
		}
	}
	return out
}

// renderResult summarizes the run's terminal event on one line.
func (r *streamJSONRenderer) renderResult(ev streamEvent) []byte {
	mark := "✓"
	if ev.IsError {
		mark = "✖"
	}
	line := mark + " result"
	if ev.Subtype != "" {
		line += " · " + ev.Subtype
	}
	if ev.NumTurns > 0 {
		line += fmt.Sprintf(" · %d turns", ev.NumTurns)
	}
	if ev.DurationMs > 0 {
		line += " · " + humanSeconds(float64(ev.DurationMs)/1000)
	}
	if ev.TotalCostUsd > 0 {
		line += fmt.Sprintf(" · $%.2f", ev.TotalCostUsd)
	}
	return append([]byte(line), '\n')
}

// humanSeconds renders a duration in seconds the way a human reads it:
// seconds under a minute, minutes and seconds under an hour, hours above.
func humanSeconds(s float64) string {
	whole := int(s)
	switch {
	case whole < 60:
		return fmt.Sprintf("%ds", whole)
	case whole < 3600:
		return fmt.Sprintf("%dm%02ds", whole/60, whole%60)
	default:
		return fmt.Sprintf("%dh%02dm", whole/3600, (whole%3600)/60)
	}
}
