// Package channel is the daemon's listen-only MCP Streamable HTTP client: it
// dials out to a server that advertises the Claude Code Channels capability,
// holds one session, and reads the standalone GET stream. Every
// `notifications/claude/channel` on that stream is a firing.
//
// Listen-only is the contract, not a simplification. The daemon never calls a
// tool, lists tools, prompts or resources, or sends any request beyond
// `initialize`, `notifications/initialized`, the GET, the `DELETE`, and
// replies to server requests. Claiming work is the spawned agent's job, with
// its own credentials; a supervisor that could also claim would be a second
// consumer on one endpoint.
//
// It dials OUT, which is what makes it the path for a laptop behind NAT: no
// inbound port, no public URL, no reverse proxy.
//
// This file is the SSE decoder. It is small, it is the only thing between a
// network socket and the JSON-RPC layer, and it is fuzzed — a decoder that
// mis-frames a field turns one doorbell into none, or into two.
//
// Governing: ADR-0021; SPEC-0014 REQ "Channel Listener Session", REQ "Channel
// Notification Handling".
//
// @joestump 09/23/2026 - Introduced with the SPEC-0014 channel listener (#471).
package channel

import (
	"bufio"
	"io"
	"strings"
)

// Event is one decoded server-sent event.
type Event struct {
	// ID is the event's `id:` field, carried so a reconnect can resume with
	// Last-Event-ID (#474). Empty when the server issued none.
	ID string
	// Name is the `event:` field. The MCP Streamable HTTP transport uses the
	// default (`message`) for JSON-RPC payloads, so this is kept for
	// completeness rather than dispatched on.
	Name string
	// Data is the event's data, with multiple `data:` lines joined by "\n"
	// as the SSE grammar requires.
	Data string
}

// Decoder reads server-sent events from a stream.
//
// It implements the parts of the SSE grammar an MCP stream uses, and no more:
// `event`, `data`, `id`, comments, and the blank line that dispatches. Two
// details are easy to get wrong and both are load-bearing here:
//
//   - `data` accumulates across lines and is joined with "\n". A JSON payload
//     the server chose to wrap would otherwise arrive as its last line only,
//     and parse as garbage.
//   - A field's value has ONE leading space stripped, not all whitespace. The
//     grammar says exactly one, and a payload that legitimately begins with
//     two spaces is not ours to trim.
type Decoder struct {
	sc *bufio.Scanner
	// lastID persists across events: the SSE grammar says an event with no
	// `id:` keeps the last one seen, which is what makes Last-Event-ID
	// resumption work across a burst.
	lastID string
	err    error
}

// maxLine bounds one SSE line. REQ "Channel Notification Handling" caps a
// notification's params at 64 KiB; this is generously above that so the cap is
// enforced where the requirement puts it — with a log line and an `invalid`
// count — rather than as a silent read error here.
const maxLine = 1 << 20

// NewDecoder reads events from r.
func NewDecoder(r io.Reader) *Decoder {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxLine)
	return &Decoder{sc: sc}
}

// Next returns the next event, or false when the stream ends. Err reports why.
//
// An event with no `data` is skipped rather than returned: the grammar says a
// dispatch with an empty data buffer fires nothing, and an SSE keep-alive
// comment produces exactly that shape.
func (d *Decoder) Next() (Event, bool) {
	var (
		data  strings.Builder
		name  string
		id    string
		hasID bool
		any   bool
	)
	for d.sc.Scan() {
		line := strings.TrimSuffix(d.sc.Text(), "\r")
		if line == "" {
			if data.Len() == 0 {
				// A dispatch with no data fires nothing (a keep-alive, or a
				// stray blank line). Reset and keep reading.
				name, id, hasID, any = "", "", false, false
				continue
			}
			if hasID {
				d.lastID = id
			}
			return Event{ID: d.lastID, Name: name, Data: data.String()}, true
		}
		if strings.HasPrefix(line, ":") {
			continue // a comment; the usual shape of an SSE keep-alive
		}
		field, value, ok := strings.Cut(line, ":")
		if !ok {
			// A line with no colon is a field with an empty value. Only
			// `data` can carry meaning that way, and an empty data line
			// contributes a newline.
			field, value = line, ""
		}
		// Exactly one leading space, per the grammar.
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "data":
			if any {
				data.WriteByte('\n')
			}
			data.WriteString(value)
			any = true
		case "event":
			name = value
		case "id":
			// The grammar ignores an id containing a NUL.
			if !strings.ContainsRune(value, 0) {
				id, hasID = value, true
			}
		}
	}
	d.err = d.sc.Err()
	return Event{}, false
}

// Err reports why the stream ended, or nil for a clean end of stream.
func (d *Decoder) Err() error { return d.err }

// LastID is the most recent `id:` the stream carried, for Last-Event-ID
// resumption (#474).
func (d *Decoder) LastID() string { return d.lastID }
