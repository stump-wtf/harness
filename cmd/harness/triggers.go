package main

// Trigger Source Verb
//
// `harness triggers`: every declared `[channel.*]` and `[webhook.*]` source,
// what state it is in and for how long, when it last fired, what it dropped,
// and which harnesses it fires. It is the answer to "did anything hear the
// doorbell?" — the question an event harness that never runs raises, and
// that no other verb can answer, because a source that never fired leaves no
// run record to read.
//
// The table is the summary an operator scans; `--json` is the whole reply,
// every per-outcome counter included. Neither carries a credential: header
// names only, a channel URL without its query, and a last error the daemon
// scrubbed before it ever reached the wire.
//
// Governing: ADR-0021, ADR-0002 (the CLI is the supported programmatic
// surface, so --json is a contract); SPEC-0014 REQ "Trigger Visibility", REQ
// "Credential Resolution".
//
// @joestump 09/24/2026 - Introduced for stump.wtf/harness#476.

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/stump-wtf/harness/internal/client"
	"github.com/stump-wtf/harness/internal/protocol"
	"github.com/stump-wtf/harness/internal/schedfmt"
	"github.com/stump-wtf/harness/internal/trigger"
)

// cmdTriggers prints every declared trigger source.
func cmdTriggers(c *client.Client, o verbOpts) error {
	srcs, err := c.Triggers()
	if err != nil {
		return err
	}
	if o.json {
		return printJSON(srcs)
	}
	return printTriggersTable(os.Stdout, srcs, time.Now())
}

// printTriggersTable renders the sources against now, then a detail block per
// source: where it listens, its last error, and what it dropped. Those go
// under the table rather than in columns because each is a sentence or a URL,
// and a sentence in a column wraps every other row with it — piped output is
// only 80 columns wide.
func printTriggersTable(w io.Writer, srcs []protocol.TriggerSourceInfo, now time.Time) error {
	if len(srcs) == 0 {
		_, err := fmt.Fprintln(w, "no trigger sources (declare a [channel.*] or [webhook.*] table in harness.toml)")
		return err
	}
	t := NewTable(w, "SOURCE", "STATE", "FOR", "LAST EVENT", "FIRED", "HARNESSES")
	width := 0
	for _, s := range srcs {
		t.Row(
			s.Source,
			t.sourceStateCell(s.State),
			sourceForCell(s, now),
			agoCell(s.LastEvent, now),
			strconv.Itoa(s.Counters[string(trigger.OutcomeFired)]),
			orDash(strings.Join(s.Harnesses, ", ")),
		)
		width = max(width, len(s.Source))
	}
	if err := t.Flush(); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	for _, s := range srcs {
		lines := []string{endpointCell(s)}
		if s.Error != "" {
			lines = append(lines, "error: "+printable(s.Error))
		}
		if d := droppedBreakdown(s); d != "" {
			lines = append(lines, "dropped: "+d)
		}
		for i, line := range lines {
			label := ""
			if i == 0 {
				label = s.Source
			}
			if _, err := fmt.Fprintf(w, "%-*s  %s\n", width, label, line); err != nil {
				return err
			}
		}
	}
	return nil
}

// sourceStateCell colors a source state by what it asks of the operator:
// mint when it can hear, amber while it is trying or waiting on config, pink
// when it has given up at speed, dim when it is off on purpose.
func (t *Table) sourceStateCell(state string) string {
	label := orDash(state)
	switch trigger.SourceState(state) {
	case trigger.StateConnected, trigger.StateListening:
		return t.mintBold(label)
	case trigger.StateConnecting, trigger.StateBackoff, trigger.StateNoListener:
		return t.amberBold(label)
	case trigger.StateError:
		return t.accentBold(label)
	case trigger.StateDisabled, trigger.StateUnbound:
		return t.dimPlain(label)
	}
	return label
}

// sourceForCell is how long the source has been in its state. For a channel
// that is not connected it is how long it has been DOWN, not how long since
// the last backoff cycle — REQ "Trigger Visibility"'s "the time it left
// connected": ten minutes of outage reads "down 10m", not "3s".
func sourceForCell(s protocol.TriggerSourceInfo, now time.Time) string {
	if at, err := time.Parse(time.RFC3339, s.DownSince); err == nil {
		return "down " + schedfmt.ShortDuration(max(now.Sub(at), 0))
	}
	if at, err := time.Parse(time.RFC3339, s.Since); err == nil {
		return schedfmt.ShortDuration(max(now.Sub(at), 0))
	}
	return "—"
}

// agoCell renders an RFC 3339 stamp as "5m ago", or "—" for never.
func agoCell(stamp string, now time.Time) string {
	at, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return "—"
	}
	return schedfmt.ShortDuration(max(now.Sub(at), 0)) + " ago"
}

// droppedBreakdown names the non-zero dropped outcomes, in REQ "Trigger
// Metrics" order: "unauthorized 3, duplicate 1". Empty when nothing dropped.
func droppedBreakdown(s protocol.TriggerSourceInfo) string {
	var parts []string
	for _, o := range trigger.Outcomes {
		if o == trigger.OutcomeFired {
			continue
		}
		if n := s.Counters[string(o)]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", o, n))
		}
	}
	return strings.Join(parts, ", ")
}

// endpointCell is where a source listens: a channel's URL (query already
// removed daemon-side) and its header names, or a webhook's route, scheme and
// event allowlist.
func endpointCell(s protocol.TriggerSourceInfo) string {
	switch {
	case s.Path != "":
		out := "POST " + s.Path + " (" + s.Verify
		if len(s.Events) > 0 {
			out += "; " + strings.Join(s.Events, ", ")
		}
		return out + ")"
	case s.URL != "":
		if len(s.Headers) > 0 {
			return s.URL + " [" + strings.Join(s.Headers, ", ") + "]"
		}
		return s.URL
	}
	return "—"
}

// printable replaces control characters with '?'. A channel's last error can
// quote what its server said, and a server is not trusted to write escape
// sequences to the operator's terminal (the rule #146 set for logs).
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return '?'
		}
		return r
	}, s)
}

// orDash returns s, or an em dash for empty.
func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
