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
	"text/tabwriter"
	"time"

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
func stateGlyph(state string) string { return core.State(state).Glyph() }

func cmdList(c *client.Client, o verbOpts) error {
	hs, err := c.List()
	if err != nil {
		return err
	}
	if o.json {
		return printJSON(hs)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "  NAME\tSTATE\tENABLED\t↻\tPID\tDESCRIPTION")
	for _, h := range hs {
		fmt.Fprintf(w, "%s %s\t%s\t%s\t%d\t%s\t%s\n",
			stateGlyph(h.State), h.Name, h.State, yesno(h.Enabled), h.RestartCount, pid(h.PID), h.Description)
	}
	return w.Flush()
}

func cmdDescribe(c *client.Client, o verbOpts) error {
	h, err := c.Describe(o.name)
	if err != nil {
		return err
	}
	if o.json {
		return printJSON(h)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintf(w, "name\t%s %s\n", stateGlyph(h.State), h.Name)
	fmt.Fprintf(w, "state\t%s\n", h.State)
	fmt.Fprintf(w, "enabled\t%s\n", yesno(h.Enabled))
	fmt.Fprintf(w, "cmd\t%s\n", h.Cmd)
	fmt.Fprintf(w, "backend\t%s\n", h.Backend)
	fmt.Fprintf(w, "restarts\t%d\n", h.RestartCount)
	fmt.Fprintf(w, "last_exit\t%d\n", h.LastExitCode)
	fmt.Fprintf(w, "flapping\t%s\n", yesno(h.Flapping))
	if h.ConfigChanged {
		fmt.Fprintf(w, "config\tchanged — restart to apply\n")
	}
	if h.PID > 0 {
		fmt.Fprintf(w, "pid\t%d\n", h.PID)
	}
	if h.Description != "" {
		fmt.Fprintf(w, "description\t%s\n", h.Description)
	}
	return w.Flush()
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
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "  NAME\tAUTOSTART\tHARNESSES\tDESCRIPTION")
	for _, p := range ps {
		marker := " "
		if p.Active {
			marker = "*"
		}
		fmt.Fprintf(w, "%s %s\t%s\t%v\t%s\n", marker, p.Name, yesno(p.Autostart), p.Harnesses, p.Description)
	}
	return w.Flush()
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
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintf(w, "version\t%s\n", di.Version)
	fmt.Fprintf(w, "proto\t%s\n", di.ProtoVersion)
	fmt.Fprintf(w, "pid\t%d\n", di.PID)
	fmt.Fprintf(w, "uptime\t%ds\n", di.UptimeSeconds)
	fmt.Fprintf(w, "socket\t%s\n", di.Socket)
	fmt.Fprintf(w, "harnesses\t%d\n", di.Harnesses)
	if di.ActiveProfile != "" {
		fmt.Fprintf(w, "profile\t%s\n", di.ActiveProfile)
	}
	return w.Flush()
}

// cmdAttach streams a harness terminal in cooked mode: live output → stdout,
// stdin → the PTY (dropped server-side for --ro). It is a minimal demonstration
// of the data plane; the styled, raw-mode attach lives in the TUI (later
// package). Ctrl-C ends it.
func cmdAttach(c *client.Client, o verbOpts) error {
	const sid = 1
	mode := protocol.AttachRW
	if o.ro {
		mode = protocol.AttachRO
	}
	if err := c.AttachOpen(sid, o.name, 80, 24, mode); err != nil {
		return err
	}
	// stdin → PTY (skip for read-only).
	if !o.ro {
		go func() {
			buf := make([]byte, 4096)
			for {
				n, err := os.Stdin.Read(buf)
				if n > 0 {
					_ = c.AttachInput(sid, buf[:n])
				}
				if err != nil {
					return
				}
			}
		}()
	}
	// Live frames → stdout.
	pc := c.Conn()
	for {
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

// errorFrom parses an ERROR frame payload.
func errorFrom(payload []byte) error {
	e := &protocol.ErrorMsg{}
	if err := json.Unmarshal(payload, e); err != nil {
		return fmt.Errorf("daemon error (unparseable): %v", err)
	}
	return e
}

func yesno(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func pid(p int) string {
	if p <= 0 {
		return "-"
	}
	return fmt.Sprintf("%d", p)
}
