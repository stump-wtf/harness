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
	"strings"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"

	"gitea.stump.rocks/stump.wtf/harness/internal/ansifold"
	"gitea.stump.rocks/stump.wtf/harness/internal/buildinfo"
	"gitea.stump.rocks/stump.wtf/harness/internal/client"
	"gitea.stump.rocks/stump.wtf/harness/internal/core"
	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
	"gitea.stump.rocks/stump.wtf/harness/internal/schedfmt"
	"gitea.stump.rocks/stump.wtf/harness/internal/tui"
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
		t.Row(
			h.Name,
			t.stateCell(h.State, h.Schedule),
			t.scheduleCell(h.Schedule),
			t.nextRunCell(h.Schedule, h.NextRun),
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
	t.Row("state", t.stateCell(h.State, h.Schedule))
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
	}
	if h.Model != "" {
		t.Row("model", t.faintPlain(h.Model))
	}
	if h.AutoAccept {
		t.Row("auto_accept", t.faintPlain("true"))
	}
	t.Row("backend", t.faintPlain(h.Backend))
	if h.Schedule != "" {
		t.Row("schedule", t.faintPlain(h.Schedule))
		if h.NextRun != "" {
			if next, err := time.Parse(time.RFC3339, h.NextRun); err == nil {
				t.Row("next run", t.faintPlain(fmt.Sprintf("%s (%s)", next.Format("Mon Jan 2 15:04"), nextRunSuffix(h.NextRun))))
			}
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
	printLogText(os.Stdout, ld.Text)
	for _, n := range ld.Notices {
		fmt.Fprintln(os.Stderr, "note: "+n)
	}
	return nil
}

// printLogText prints durable-log text made inert first, so escape payloads
// (DCS/sixel, OSC, cursor addressing) don't act on the user's terminal (#146 —
// acceptance criteria require no payload bytes reach `harness logs`).
func printLogText(w io.Writer, raw string) {
	text := inertLogText(raw)
	fmt.Fprint(w, text)
	if len(text) > 0 && text[len(text)-1] != '\n' {
		fmt.Fprintln(w)
	}
}

// followLogs re-fetches the tail on an interval and prints the new suffix.
func followLogs(c *client.Client, o verbOpts) error {
	ld, err := c.Logs(o.name, o.lines)
	if err != nil {
		return err
	}
	prev := inertLogText(ld.Text)
	fmt.Print(prev)
	for {
		time.Sleep(time.Second)
		ld, err := c.Logs(o.name, o.lines*4)
		if err != nil {
			return err
		}
		cur := inertLogText(ld.Text)
		if len(cur) > len(prev) && hasSuffixOverlap(cur, prev) {
			fmt.Print(cur[len(prev):])
		} else if cur != prev {
			// Rotation/truncation broke continuity; reprint the whole tail.
			fmt.Print(cur)
		}
		prev = cur
	}
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
	fmt.Printf("activated profile %q\n", o.name)
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
	fmt.Printf("reloaded — %d harnesses\n", len(hs))
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
	fmt.Fprintf(os.Stderr, "harness: daemon (pid %d) stopping\n", di.PID)
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
