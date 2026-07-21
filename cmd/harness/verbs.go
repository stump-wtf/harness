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
	"syscall"
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/buildinfo"
	"gitea.stump.rocks/stump.wtf/harness/internal/client"
	"gitea.stump.rocks/stump.wtf/harness/internal/core"
	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
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
			stateCell(h.State),
			enabledCell(h.Enabled),
			fmt.Sprintf("%d", h.RestartCount),
			pidCell(h.PID),
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
	t.Row(accentBold("name"), fmt.Sprintf("%s %s", stateGlyphOnly(h.State), h.Name))
	t.Row("state", stateCell(h.State))
	t.Row("enabled", enabledCell(h.Enabled))
	t.Row("cmd", faintPlain(h.Cmd))
	t.Row("backend", faintPlain(h.Backend))
	t.Row("restarts", fmt.Sprintf("%d", h.RestartCount))
	t.Row("last_exit", fmt.Sprintf("%d", h.LastExitCode))
	t.Row("flapping", flappingCell(h.Flapping))
	if h.ConfigChanged {
		t.Row("config", amberBold("changed — restart to apply"))
	}
	if h.PID > 0 {
		t.Row("pid", fmt.Sprintf("%d", h.PID))
	}
	if h.Description != "" {
		t.Row("description", dimItalic(h.Description))
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
			name = accentBold("* " + p.Name)
		}
		autostart := enabledCell(p.Autostart)
		t.Row(name, autostart, fmt.Sprintf("%v", p.Harnesses), dimItalic(p.Description))
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
	t.Row("version", accentBold(di.Version))
	t.Row("proto", faintPlain(di.ProtoVersion))
	t.Row("pid", fmt.Sprintf("%d", di.PID))
	t.Row("uptime", fmt.Sprintf("%ds", di.UptimeSeconds))
	t.Row("socket", faintPlain(di.Socket))
	t.Row("harnesses", fmt.Sprintf("%d", di.Harnesses))
	if di.ActiveProfile != "" {
		t.Row("profile", accentBold(di.ActiveProfile))
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

// cmdAttach streams a harness terminal: live output → stdout, stdin → the
// PTY (dropped server-side for --ro). Puts the local terminal in raw mode so
// keystrokes pass through faithfully (Ctrl-C, arrows, etc. go to the harness,
// not the local shell), and watches for the detach chord Ctrl-\ to cleanly
// close the session and restore the terminal. The TUI's attached mode
// (internal/tui/attached.go) is the richer surface with scrollback/hop/etc;
// this is the scriptable one-shot.
func cmdAttach(c *client.Client, o verbOpts) error {
	const sid = 1
	mode := protocol.AttachRW
	if o.ro {
		mode = protocol.AttachRO
	}
	if err := c.AttachOpen(sid, o.name, termWidth(), termHeight(), mode); err != nil {
		return err
	}

	// Raw mode so keystrokes (including Ctrl-C) pass through to the harness
	// untouched. Restore on return so a crash or detach doesn't leave the
	// user's terminal broken.
	prev, err := makeRaw(os.Stdin)
	if err == nil {
		defer restoreTerm(os.Stdin, prev)
	}
	done := make(chan struct{})

	// stdin → PTY (skip for read-only). Watch for the detach chord: Ctrl-\
	// (0x1c) cleanly closes the attach session and exits.
	if !o.ro {
		go func() {
			defer close(done)
			buf := make([]byte, 4096)
			for {
				n, err := os.Stdin.Read(buf)
				if n > 0 {
					if hasDetachChord(buf[:n]) {
						_ = c.AttachClose(sid)
						return
					}
					_ = c.AttachInput(sid, buf[:n])
				}
				if err != nil {
					return
				}
			}
		}()
	} else {
		defer close(done)
	}

	// Live frames → stdout.
	pc := c.Conn()
	for {
		select {
		case <-done:
			return nil
		default:
		}
		f, err := pc.ReadFrame()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		switch f.Type {
		case protocol.TypeAttachData:
			_, rest, derr := protocol.DecodeAttach(f.Payload)
			if derr == nil {
				_, _ = os.Stdout.Write(rest)
			}
		case protocol.TypePing:
			_ = pc.WriteFrame(protocol.TypePong, nil)
		case protocol.TypeError:
			return errorFrom(f.Payload)
		}
	}
}

// hasDetachChord reports whether the read buffer contains the detach byte
// (Ctrl-\ = 0x1c, also known as FS / File Separator). We scan the whole
// buffer because the byte may arrive co-encoded with other keystrokes.
func hasDetachChord(b []byte) bool {
	for _, c := range b {
		if c == 0x1c { // Ctrl-\
			return true
		}
	}
	return false
}

// errorFrom parses an ERROR frame payload.
func errorFrom(payload []byte) error {
	e := &protocol.ErrorMsg{}
	if err := json.Unmarshal(payload, e); err != nil {
		return fmt.Errorf("daemon error (unparseable): %v", err)
	}
	return e
}
