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
func printHarnessTable(w io.Writer, hs []protocol.HarnessInfo) error {
	// The schedule columns are conditional. They cost 26 of the table's
	// 80-cell budget, which comes out of DESCRIPTION (the only flex column) —
	// rendered unconditionally they collapse it to ~3 cells and shred every
	// description into a column of two-letter fragments, on every listing, for
	// every user who has no scheduled harness at all. Show them when there is
	// something to show.
	scheduled := false
	for _, h := range hs {
		if h.Schedule != "" {
			scheduled = true
			break
		}
	}

	headers := []string{"NAME", "STATE"}
	if scheduled {
		headers = append(headers, "SCHEDULE", "NEXT")
	}
	headers = append(headers, "ENABLED", "RESTARTS", "PID", "DESCRIPTION")

	t := NewTable(w, headers...)
	for _, h := range hs {
		cells := []string{h.Name, t.stateCell(h.State)}
		if scheduled {
			cells = append(cells, scheduleCell(h.Schedule), nextRunCell(h.NextRun))
		}
		cells = append(cells,
			t.enabledCell(h.Enabled),
			fmt.Sprintf("%d", h.RestartCount),
			t.pidCell(h.PID),
			h.Description,
		)
		t.Row(cells...)
	}
	return t.Flush()
}

// scheduleCell renders the schedule column: the cron spec (or "—" for an
// always-on/manual harness). The spec is shown verbatim rather than
// humanized — "0 */6 * * *" is what the user wrote in harness.toml, so it
// stays greppable and matches the config round-trip.
func scheduleCell(spec string) string {
	if spec == "" {
		return "-"
	}
	return spec
}

// nextRunCell renders the NEXT column as a relative time from now ("in 3h",
// "in 12m", "due") so a glance answers "how long until it fires" without
// mental timezone math. Absolute time stays available via describe/--json.
func nextRunCell(nextRun string) string {
	if nextRun == "" {
		return "-"
	}
	next, err := time.Parse(time.RFC3339, nextRun)
	if err != nil {
		return "-"
	}
	d := time.Until(next)
	if d <= 0 {
		return "due"
	}
	return "in " + shortDuration(d)
}

// shortDuration compacts a duration the way an operator reads it: dropping
// fractional seconds and every zero unit, leading or trailing ("45s", "12m",
// "2h5m", "3d4h").
//
// Formatted by hand rather than through Duration.String(), which always
// carries the smaller units down to seconds — "12m0s", "10h45m0s". That is
// noise at a glance, and "in 10h45m0s" is 11 cells against the NEXT column's
// 10, so the table truncated it back to "in 10h45m…".
//
// Rounding is applied before the unit split so a carry lands in the right
// place: 59m45s reads "1h", not "60m".
func shortDuration(d time.Duration) string {
	if d < time.Minute {
		return d.Round(time.Second).String()
	}
	if d < 24*time.Hour {
		d = d.Round(time.Minute)
	} else {
		d = d.Round(time.Hour)
	}
	days := int(d / (24 * time.Hour))
	hours := int(d/time.Hour) % 24
	mins := int(d/time.Minute) % 60
	switch {
	case days > 0 && hours > 0:
		return fmt.Sprintf("%dd%dh", days, hours)
	case days > 0:
		return fmt.Sprintf("%dd", days)
	case hours > 0 && mins > 0:
		return fmt.Sprintf("%dh%dm", hours, mins)
	case hours > 0:
		return fmt.Sprintf("%dh", hours)
	default:
		return fmt.Sprintf("%dm", mins)
	}
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
	t.Row("state", t.stateCell(h.State))
	t.Row("enabled", t.enabledCell(h.Enabled))
	// A prompt harness has no configured cmd — show what the user wrote (the
	// prompt), not the synthesized agent argv (ADR-0011 spawn-time synthesis).
	if h.Prompt != "" {
		t.Row("prompt", t.faintPlain(h.Prompt))
	} else {
		t.Row("cmd", t.faintPlain(h.Cmd))
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
				t.Row("next run", t.faintPlain(fmt.Sprintf("%s (%s)", next.Format("Mon Jan 2 15:04"), nextRunCell(h.NextRun))))
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
	// --follow polls the tail and prints only newly appended bytes. JSON output
	// is a single snapshot (a stream of JSON blobs would not be scriptable).
	if o.follow && !o.json {
		return followLogs(c, o)
	}
	ld, err := c.Logs(o.name, o.lines)
	if err != nil {
		return err
	}
	if o.json {
		return printJSON(ld)
	}
	// The tail is raw PTY output. Make it inert before printing so escape
	// payloads (DCS/sixel, OSC, cursor addressing) don't act on the user's
	// terminal (#146 — acceptance criteria require no payload bytes reach
	// `harness logs`).
	text := strings.Join(ansifold.Lines(strings.Split(ld.Text, "\n")), "\n")
	fmt.Print(text)
	if len(text) > 0 && text[len(text)-1] != '\n' {
		fmt.Println()
	}
	return nil
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
	// The client half of the version picture (#181): daemon-info is where
	// build skew gets diagnosed, so show both sides and flag disagreement.
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
// its PID (fetched via daemon-info). This is the counterpart to
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
