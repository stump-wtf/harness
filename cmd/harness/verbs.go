package main

// Governing: SPEC-0002 REQ "Control Operations" / "Event Subscription" /
// "Attach Session" (the client verbs mirror the control plane 1:1) and SPEC-0003
// (the state glyphs list renders). ADR-0002 (the CLI is the supported
// programmatic surface, so --json output is a first-class contract).

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/stump-wtf/harness/internal/ansifold"
	"github.com/stump-wtf/harness/internal/buildinfo"
	"github.com/stump-wtf/harness/internal/client"
	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/protocol"
	"github.com/stump-wtf/harness/internal/schedfmt"
	"github.com/stump-wtf/harness/internal/tui"
)

// printJSON writes v as indented JSON.
func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// stateGlyph returns the SPEC-0003 status glyph for a state string.
// Deprecated: prefer stateGlyphOnly (which colors it). Kept for the
// lifecycle verb output.
func stateGlyph(state string) string { return core.State(state).Glyph() }

func cmdList(c *client.Client, o verbOpts) error {
	return renderHarnessList(c, o, "")
}

// waitingCell phrases the stuck-at-prompt marker: what the harness is waiting
// for (the matched pattern, when one was identified) and how long its screen
// has been silent — the two facts an operator needs to trust it.
func waitingCell(h protocol.HarnessInfo) string {
	idle := ""
	if h.IdleMs > 0 {
		idle = " · screen idle " + schedfmt.ShortDuration(time.Duration(h.IdleMs)*time.Millisecond)
	}
	if h.WaitingFor != "" {
		return "waiting for input (" + h.WaitingFor + ")" + idle
	}
	return "waiting for input" + idle
}

// cmdCapture prints a harness's current terminal screen without an interactive
// TTY (issue #735; ADR-0040). Plain text is the default — one row per line,
// trailing blank rows trimmed, exactly what a sweep greps. --ansi emits the
// styled repaint an attach client's first frame carries, for a consumer that
// writes it to a real terminal. --json is the full CaptureData.
func cmdCapture(c *client.Client, o verbOpts) error {
	cd, err := c.Capture(o.name, o.ansi)
	if err != nil {
		return err
	}
	if o.json {
		return printJSON(cd)
	}
	if o.ansi {
		_, err = os.Stdout.WriteString(cd.Ansi)
		return err
	}
	_, err = fmt.Fprintln(os.Stdout, cd.Text)
	return err
}

// renderHarnessList is the single fetch-and-render tail shared by `list` and
// `ps` (SPEC-0004: ps is a plain alias for list outside a project), with an
// optional provenance filter for the project-scoped ps path. One code path
// keeps the two verbs' output contract from silently diverging.
func renderHarnessList(c *client.Client, o verbOpts, project string) error {
	hs, err := c.List()
	if err != nil {
		return err
	}
	if project != "" {
		hs = filterProjectHarnesses(hs, project)
	}
	if o.json {
		return printJSON(hs)
	}
	return printHarnessTable(os.Stdout, hs)
}

// printHarnessTable renders the shared harness status table used by `list`,
// `ps`, and the `up` one-shot status output (SPEC-0004 REQ "Bring Up"), so
// every listing surface stays one product.
//
// A schedule is a FIELD, not prose. SCHEDULE holds the cadence ("daily 09:00
// UTC") and NEXT the countdown ("in 2h"), both derived from config and the
// live scheduler, so they are styled and aligned like any other column and a
// harness that says nothing about its schedule in its description still shows
// one. They used to be spliced into DESCRIPTION, and the cadence was
// highlighted only where it happened to appear VERBATIM in text the operator
// had written — so rewording a description silently unhighlighted it, and the
// rest was freetext pretending to be data (#331). DESCRIPTION is now only the
// operator's own words.
func printHarnessTable(w io.Writer, hs []protocol.HarnessInfo) error {
	t := NewTable(w, "NAME", "STATE", "SCHEDULE", "NEXT", "RESTARTS", "DESCRIPTION")
	for _, h := range hs {
		// A stalled session (issue #347) reads healthy in every process
		// signal; the marker is the one place the truth shows in `list`.
		state := t.stateCell(h.State, h.Schedule, h.Held, h.ClosingUntil != "")
		if h.SessionStalled {
			state += " ⚠ session stalled"
		}
		// A harness frozen at an interactive prompt also reads healthy —
		// `running`, CPU quiet, log silent (the prompt never scrolled). The
		// waiting marker is the one place that truth shows (issue #735;
		// ADR-0040).
		if h.Waiting {
			state += " ⏸ " + waitingCell(h)
		}
		t.Row(
			h.Name,
			state,
			t.scheduleCell(h.Schedule, h.OperatingHours),
			t.nextRunCell(h),
			fmt.Sprintf("%d", h.RestartCount),
			t.dimPlain(h.Description),
		)
	}
	return t.Flush()
}

// nextRunSuffix renders a human-readable next-run time ("in 3h", "in 12m",
// "due") for `describe`, which prints it in parentheses after the absolute
// time. The listing table has its own (*Table).nextRunCell: that one owns a
// whole column and styles it, this one is a bare parenthetical.
func nextRunSuffix(nextRun string) string {
	if s := schedfmt.NextIn(nextRun); s != "" {
		return s
	}
	return "-"
}

// formatArgv renders an argv as the TOML array an operator wrote, each element
// Go-quoted so embedded spaces, quotes and control bytes stay visible and the
// boundaries between arguments are unambiguous.
func formatArgv(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = strconv.Quote(a)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func cmdDescribe(c *client.Client, o verbOpts) error {
	h, err := c.Describe(o.name)
	if err != nil {
		return err
	}
	if o.json {
		return printJSON(h)
	}
	t := NewTable(os.Stdout, "FIELD", "VALUE")
	t.Row(t.accentBold("name"), fmt.Sprintf("%s %s", t.stateGlyphOnly(h.State), h.Name))
	// Pass the schedule: without it describe renders "stopped" in pink for the
	// same harness `harness list` shows as amber "armed" (#268, #331). schedfmt exists
	// so the surfaces cannot phrase one harness two ways.
	t.Row("state", t.stateCell(h.State, h.Schedule, h.Held, h.ClosingUntil != ""))
	// Stuck-at-prompt projection (issue #735; ADR-0040): a full-screen prompt
	// never reaches the durable log, so this row is the only truthful answer
	// to "why has this harness printed nothing for an hour?".
	if h.Waiting {
		t.Row("waiting", t.amberBold(waitingCell(h)))
	}
	// A scheduled harness is always enabled = false (SPEC-0008 REQ "Schedule
	// Exclusions"), so printing "enabled no" says nothing true about it: the
	// schedule is its intent. Show whether it is armed instead (#331).
	if h.Schedule != "" {
		t.Row("armed", t.faintPlain("yes"))
	} else {
		t.Row("enabled", t.enabledCell(h.Enabled))
	}
	// A prompt harness has no configured cmd — show what the user wrote (the
	// prompt), not the synthesized agent argv (ADR-0011 spawn-time synthesis).
	switch {
	case h.Prompt != "":
		t.Row("prompt", t.faintPlain(h.Prompt))
	case h.PromptFile != "":
		// The path is the useful cell: the instruction lives in that file and
		// is read at spawn, so printing it here would be both huge and stale
		// (ADR-0018).
		t.Row("prompt_file", t.faintPlain(h.PromptFile))
	default:
		t.Row("harness", t.faintPlain(h.Adapter))
		// A command harness's argv is what runs, so it is the row an operator
		// came for. Each element is quoted, TOML-style, because the element
		// boundaries are the point: "a b" is one argument, not two, and no
		// shell ever re-splits it. Governing: SPEC-0017 REQ-16 "Visibility".
		if len(h.Argv) > 0 {
			t.Row("argv", t.faintPlain(formatArgv(h.Argv)))
		}
	}
	if h.Model != "" {
		t.Row("model", t.faintPlain(h.Model))
	}
	if h.AutoAccept {
		t.Row("auto_accept", t.faintPlain("true"))
	}
	// SPEC-0018 REQ-11: the claude-code one-shot persona keys, shown when set.
	if h.SystemPromptFile != "" {
		t.Row("system_prompt_file", t.faintPlain(h.SystemPromptFile))
	}
	if h.MCPConfig != "" {
		t.Row("mcp_config", t.faintPlain(h.MCPConfig))
	}
	if len(h.AllowedTools) > 0 {
		t.Row("allowed_tools", t.faintPlain(strings.Join(h.AllowedTools, ", ")))
	}
	t.Row("backend", t.faintPlain(h.Backend))
	switch {
	case h.Schedule != "":
		t.Row("schedule", t.faintPlain(h.Schedule))
		if h.NextRun != "" {
			if next, err := time.Parse(time.RFC3339, h.NextRun); err == nil {
				t.Row("next run", t.faintPlain(fmt.Sprintf("%s (%s)", next.Format("Mon Jan 2 15:04"), nextRunSuffix(h.NextRun))))
			}
		}
	case h.OperatingHours != "":
		// SPEC-0012 REQ "Operating Hours Visibility": describe carries the raw
		// expression (zone-trimmed the same way `list`'s SCHEDULE column is),
		// the effective close mode, the next transition in the same wording
		// NEXT uses, and the lease end when one is active.
		t.Row("operating_hours", t.faintPlain(schedfmt.HoursExprLabel(h.OperatingHours, schedfmt.DaemonZoneName())))
		if h.HoursShutdown != "" {
			t.Row("hours_shutdown", t.faintPlain(h.HoursShutdown))
		}
		if next := schedfmt.HoursNext(h.Held, h.ClosingUntil != "", h.LeaseUntil, h.ClosingUntil, h.HoursNext); next != "" {
			t.Row("next", t.faintPlain(next))
		}
		if h.LeaseUntil != "" {
			t.Row("lease_until", t.faintPlain(h.LeaseUntil))
		}
	}
	t.Row("restarts", fmt.Sprintf("%d", h.RestartCount))
	t.Row("last_exit", fmt.Sprintf("%d", h.LastExitCode))
	t.Row("flapping", t.flappingCell(h.Flapping))
	if h.ConfigChanged {
		t.Row("config", t.amberBold("changed — restart to apply"))
	}
	if h.PID > 0 {
		t.Row("pid", fmt.Sprintf("%d", h.PID))
	}
	if h.Description != "" {
		t.Row("description", t.dimItalic(h.Description))
	}
	if h.AttachViewport != "" {
		t.Row("attach viewport", t.faintPlain(h.AttachViewport))
	}
	if err := t.Flush(); err != nil {
		return err
	}
	return printAttachSessions(os.Stdout, h.AttachSessions)
}

// printAttachSessions renders the live attach sessions under a describe's
// FIELD/VALUE table (#183). The session(s) flagged as setting the
// smallest-attached-wins minimum are highlighted, because that row is the
// answer to "why is my guest 80 columns wide?" — the clamping session.
func printAttachSessions(w io.Writer, sessions []protocol.AttachSessionInfo) error {
	if len(sessions) == 0 {
		return nil
	}
	t := NewTable(w, "SESSION", "MODE", "VIEWPORT", "AGE", "MIN")
	for _, s := range sessions {
		// 0×0 is "unknown", not a real viewport (#183): a client that could
		// not detect its size attaches without one so it cannot clamp anyone.
		size := "unknown"
		if s.Cols > 0 && s.Rows > 0 {
			size = fmt.Sprintf("%dx%d", s.Cols, s.Rows)
		}
		age := "unknown"
		if created, err := time.Parse(time.RFC3339, s.CreatedAt); err == nil {
			age = time.Since(created).Round(time.Second).String()
		}
		marker := ""
		if s.SetsMin {
			marker = t.amberBold("≤ clamps guest")
		}
		t.Row(fmt.Sprintf("%d", s.ID), s.Mode, size, age, marker)
	}
	return t.Flush()
}

func cmdLogs(c *client.Client, o verbOpts) error {
	// Without --raw, logs is the structured activity view of the latest run
	// (logs.go; #302). A generic harness, or a daemon too old to know the
	// view, answers with the durable log tail, which prints exactly as --raw.
	if !o.raw {
		return cmdLogEvents(c, o, os.Stdout)
	}
	// --follow polls the tail and prints only newly appended bytes. JSON output
	// is a single snapshot (a stream of JSON blobs would not be scriptable).
	if o.follow && !o.json {
		return followLogs(c, o)
	}
	fetch := c.Logs
	if o.run > 0 {
		// One run of a scheduled harness reads that run's own log (#120).
		fetch = func(name string, lines int) (protocol.LogsData, error) {
			return c.RunLogs(name, o.run, lines)
		}
	}
	ld, err := fetch(o.name, o.lines)
	if err != nil {
		return err
	}
	if o.json {
		return printJSON(ld)
	}
	writeLogText(newRawWriter(os.Stdout, logStyleFor(os.Stdout)), ld.Text)
	for _, n := range ld.Notices {
		fmt.Fprintln(os.Stderr, "note: "+n)
	}
	return nil
}

// printLogText prints durable-log text made inert first, so escape payloads
// (DCS/sixel, OSC, cursor addressing) don't act on the user's terminal (#146 —
// acceptance criteria require no payload bytes reach `harness logs`). It is
// the plain form, byte for byte what a pipe reads.
func printLogText(w io.Writer, raw string) {
	writeLogText(newRawWriter(w, nil), raw)
}

// writeLogText is printLogText through a raw writer, which styles the
// daemon's own lines when it is styled.
func writeLogText(rw *rawWriter, raw string) {
	text := inertLogText(raw)
	rw.write(text)
	if len(text) > 0 && text[len(text)-1] != '\n' {
		rw.write("\n")
	}
}

// followLogs re-fetches the tail on an interval and prints the new suffix.
func followLogs(c *client.Client, o verbOpts) error {
	fetch := func(lines int) (string, error) {
		ld, err := c.Logs(o.name, lines)
		return ld.Text, err
	}
	return followRawLogs(newRawWriter(os.Stdout, logStyleFor(os.Stdout)), fetch, o.lines, func() bool {
		time.Sleep(time.Second)
		return true
	})
}

// followRawLogs prints the tail, then re-fetches it until wait reports false,
// printing only the newly appended suffix.
func followRawLogs(rw *rawWriter, fetch func(lines int) (string, error), lines int, wait func() bool) error {
	text, err := fetch(lines)
	if err != nil {
		return err
	}
	prev := inertLogText(text)
	rw.write(prev)
	for wait() {
		text, err := fetch(lines * 4)
		if err != nil {
			return err
		}
		cur := inertLogText(text)
		if len(cur) > len(prev) && hasSuffixOverlap(cur, prev) {
			rw.write(cur[len(prev):])
		} else if cur != prev {
			// Rotation/truncation broke continuity; reprint the whole tail.
			rw.restart()
			rw.write(cur)
		}
		prev = cur
	}
	return nil
}

// inertLogText filters raw PTY bytes from the daemon log through ansifold so
// escape payloads are suppressed in `harness logs` output (#146).
func inertLogText(raw string) string {
	return strings.Join(ansifold.Lines(strings.Split(raw, "\n")), "\n")
}

// hasSuffixOverlap reports whether cur begins with prev (the common streaming
// case where new bytes were appended).
func hasSuffixOverlap(cur, prev string) bool {
	return len(cur) >= len(prev) && cur[:len(prev)] == prev
}

func cmdProfiles(c *client.Client, o verbOpts) error {
	ps, err := c.Profiles()
	if err != nil {
		return err
	}
	if o.json {
		return printJSON(ps)
	}
	t := NewTable(os.Stdout, "NAME", "AUTOSTART", "HARNESSES", "DESCRIPTION")
	for _, p := range ps {
		name := p.Name
		if p.Active {
			name = t.accentBold("* " + p.Name)
		}
		autostart := t.enabledCell(p.Autostart)
		t.Row(name, autostart, fmt.Sprintf("%v", p.Harnesses), t.dimItalic(p.Description))
	}
	return t.Flush()
}

func cmdUseProfile(c *client.Client, o verbOpts) error {
	ps, err := c.UseProfile(o.name)
	if err != nil {
		return err
	}
	if o.json {
		return printJSON(ps)
	}
	emit(os.Stdout, fmt.Sprintf("activated profile %q\n", o.name), func(s lifecycleStyle) string {
		return s.renderUseProfile(o.name, ps)
	})
	return nil
}

func cmdReload(c *client.Client, o verbOpts) error {
	hs, err := c.Reload()
	if err != nil {
		return err
	}
	if o.json {
		return printJSON(hs)
	}
	emit(os.Stdout, fmt.Sprintf("reloaded — %d harnesses\n", len(hs)), func(s lifecycleStyle) string {
		return s.renderReload(hs)
	})
	return nil
}

func cmdDaemonInfo(c *client.Client, o verbOpts) error {
	di, err := c.DaemonInfo()
	if err != nil {
		return err
	}
	if o.json {
		return printJSON(di)
	}
	t := NewTable(os.Stdout, "FIELD", "VALUE")
	t.Row("version", t.accentBold(di.Version))
	// The client half of the version picture (#181): `daemon status` is
	// where build skew gets diagnosed, so show both sides and flag disagreement.
	t.Row("client", t.faintPlain(buildinfo.Version))
	if notice := buildinfo.SkewNotice(di.Version, buildinfo.Version); notice != "" {
		t.Row("skew", t.amberBold(notice))
	}
	t.Row("proto", t.faintPlain(di.ProtoVersion))
	t.Row("pid", fmt.Sprintf("%d", di.PID))
	t.Row("uptime", fmt.Sprintf("%ds", di.UptimeSeconds))
	t.Row("socket", t.faintPlain(di.Socket))
	t.Row("harnesses", fmt.Sprintf("%d", di.Harnesses))
	if di.ActiveProfile != "" {
		profileLabel := di.ActiveProfile
		if di.ProfileResolved != nil && !*di.ProfileResolved {
			profileLabel = fmt.Sprintf("%s (unresolved)", di.ActiveProfile)
		}
		t.Row("profile", t.accentBold(profileLabel))
	}
	return t.Flush()
}

// cmdStopDaemon asks the running daemon to shut down by sending SIGTERM to
// its PID (fetched via `daemon status`). This is the counterpart to
// `harness daemon --detach`: the pair gives you stop-daemon → daemon --detach
// as a clean restart cycle. The daemon's own signal handler does the graceful
// shutdown (close socket, stop harnesses, flush state).
func cmdStopDaemon(o verbOpts) error {
	c, err := client.Dial(o.socket, buildinfo.Version, nil)
	if err != nil {
		return err
	}
	defer c.Close()
	di, err := c.DaemonInfo()
	if err != nil {
		return err
	}
	if di.PID <= 0 {
		return fmt.Errorf("daemon reported PID %d — cannot stop", di.PID)
	}
	p, err := os.FindProcess(di.PID)
	if err != nil {
		return fmt.Errorf("find daemon process %d: %w", di.PID, err)
	}
	if err := p.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("signal daemon %d: %w", di.PID, err)
	}
	emit(os.Stderr, fmt.Sprintf("harness: daemon (pid %d) stopping\n", di.PID), func(s lifecycleStyle) string {
		return s.renderDaemonStopping(di.PID)
	})
	return nil
}

// cmdAttach launches the embedded-terminal surface for a harness. It reuses
// the cockpit TUI's attached mode (internal/tui) via its AttachOnly option,
// so the CLI one-shot gets the same full-window x/vt terminal, 1-line status
// bar, Bubbles-help key bindings, and tmux-style detach chords as the
// dashboard — no separate raw-pipe code path to drift from the TUI's
// behavior. Governing: SPEC-0001 REQ "Attached Mode", ADR-0003 (embedded
// terminal).
func cmdAttach(o verbOpts) error {
	m := tui.New(tui.Options{
		Socket:      o.socket,
		ConfigPath:  o.configPath,
		Version:     buildinfo.Version,
		ReadOnly:    o.ro,
		AttachOnly:  o.name,
		SkipConfirm: true,
	})
	// Alt screen and mouse reporting are View fields under Bubble Tea v2 (see
	// tui.Model.View), not program options.
	p := tea.NewProgram(m)
	_, err := p.Run()
	return err
}
