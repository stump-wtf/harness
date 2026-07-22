package main

// Governing: SPEC-0002 REQ "Control Operations" / "Event Subscription" /
// "Attach Session" (the client verbs mirror the control plane 1:1) and SPEC-0003
// (the state glyphs list renders). ADR-0002 (the CLI is the supported
// programmatic surface, so --json output is a first-class contract).

import (
	"encoding/json"
	"fmt"
	"os"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"gitea.stump.rocks/stump.wtf/harness/internal/buildinfo"
	"gitea.stump.rocks/stump.wtf/harness/internal/client"
	"gitea.stump.rocks/stump.wtf/harness/internal/core"
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
	hs, err := c.List()
	if err != nil {
		return err
	}
	if o.json {
		return printJSON(hs)
	}
	t := NewTable(os.Stdout, "NAME", "STATE", "ENABLED", "RESTARTS", "PID", "DESCRIPTION")
	for _, h := range hs {
		t.Row(
			h.Name,
			t.stateCell(h.State),
			t.enabledCell(h.Enabled),
			fmt.Sprintf("%d", h.RestartCount),
			t.pidCell(h.PID),
			h.Description,
		)
	}
	return t.Flush()
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
	t.Row("cmd", t.faintPlain(h.Cmd))
	t.Row("backend", t.faintPlain(h.Backend))
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
	fmt.Print(ld.Text)
	if len(ld.Text) > 0 && ld.Text[len(ld.Text)-1] != '\n' {
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
	fmt.Print(ld.Text)
	prev := ld.Text
	for {
		time.Sleep(time.Second)
		ld, err := c.Logs(o.name, o.lines*4)
		if err != nil {
			return err
		}
		if len(ld.Text) > len(prev) && hasSuffixOverlap(ld.Text, prev) {
			fmt.Print(ld.Text[len(prev):])
		} else if ld.Text != prev {
			// Rotation/truncation broke continuity; reprint the whole tail.
			fmt.Print(ld.Text)
		}
		prev = ld.Text
	}
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
	t.Row("proto", t.faintPlain(di.ProtoVersion))
	t.Row("pid", fmt.Sprintf("%d", di.PID))
	t.Row("uptime", fmt.Sprintf("%ds", di.UptimeSeconds))
	t.Row("socket", t.faintPlain(di.Socket))
	t.Row("harnesses", fmt.Sprintf("%d", di.Harnesses))
	if di.ActiveProfile != "" {
		t.Row("profile", t.accentBold(di.ActiveProfile))
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
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	_, err := p.Run()
	return err
}
