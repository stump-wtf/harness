package tui

// Governing: SPEC-0001 (small cross-cutting helpers: PTY key encoding for
// attached forwarding, inline daemon start for the no-daemon zero-state, and
// TOML table deletion for the delete confirm — ADR-0006 file-is-truth).

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// keyToBytes encodes a keystroke into the bytes a PTY expects, so attached
// interactive input forwards faithfully (SPEC-0001 scenario "Driving a live
// agent"). It covers the common control keys and arrow escapes; printable runes
// pass through verbatim.
func keyToBytes(msg tea.KeyMsg) []byte {
	switch msg.Type {
	case tea.KeyRunes:
		return []byte(string(msg.Runes))
	case tea.KeySpace:
		return []byte{' '}
	case tea.KeyEnter:
		return []byte{'\r'}
	case tea.KeyTab:
		return []byte{'\t'}
	case tea.KeyBackspace:
		return []byte{0x7f}
	case tea.KeyDelete:
		return []byte("\x1b[3~")
	case tea.KeyEsc:
		return []byte{0x1b}
	case tea.KeyUp:
		return []byte("\x1b[A")
	case tea.KeyDown:
		return []byte("\x1b[B")
	case tea.KeyRight:
		return []byte("\x1b[C")
	case tea.KeyLeft:
		return []byte("\x1b[D")
	case tea.KeyHome:
		return []byte("\x1b[H")
	case tea.KeyEnd:
		return []byte("\x1b[F")
	case tea.KeyPgUp:
		return []byte("\x1b[5~")
	case tea.KeyPgDown:
		return []byte("\x1b[6~")
	}
	// Ctrl+A..Ctrl+Z map to 0x01..0x1a.
	if msg.Type >= tea.KeyCtrlA && msg.Type <= tea.KeyCtrlZ {
		return []byte{byte(msg.Type - tea.KeyCtrlA + 1)}
	}
	return nil
}

// textinputBlink is the textinput cursor-blink Cmd.
func textinputBlink() tea.Cmd { return textinput.Blink }

// splitLines splits text into display lines, dropping a single trailing empty
// line so a final newline doesn't add a blank row.
func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return lines
}

// startDaemonCmd launches `harness daemon` in the background for the no-daemon
// inline offer (SPEC-0001 scenario "Daemon not running"), then runs `then` (a
// redial).
func startDaemonCmd(opts Options, then tea.Cmd) tea.Cmd {
	return tea.Sequence(
		func() tea.Msg {
			cmd := exec.Command("harness", "daemon", "--socket", opts.Socket, "--config", opts.ConfigPath)
			cmd.Stdout, cmd.Stderr = nil, nil
			_ = cmd.Start()
			time.Sleep(600 * time.Millisecond) // give it a moment to bind the socket
			return nil
		},
		then,
	)
}

// deleteHarnessCmd removes a harness table from harness.toml and reloads the
// daemon (ADR-0006). If the file can't be read it surfaces the error via the
// reload result.
func (m *Model) deleteHarnessCmd(name string) tea.Cmd {
	path := m.opts.ConfigPath
	ctrl := m.ctrl
	return func() tea.Msg {
		body, err := os.ReadFile(path)
		if err != nil {
			return reloadResultMsg{err: err}
		}
		out := removeHarnessTOML(string(body), name)
		if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
			return reloadResultMsg{err: err}
		}
		hs, rerr := ctrl.Reload()
		return reloadResultMsg{harnesses: hs, err: rerr}
	}
}

// tableHeaderRe matches any TOML table header line.
var tableHeaderRe = regexp.MustCompile(`^\s*\[`)

// removeHarnessTOML drops the [harness.<name>] (or bare [<name>]) table and its
// body from a harness.toml source, up to the next table header or EOF. It is a
// line-oriented edit that preserves the rest of the file (ADR-0006).
func removeHarnessTOML(body, name string) string {
	lines := strings.Split(body, "\n")
	want := map[string]bool{
		"[harness." + name + "]": true,
		"[" + name + "]":         true,
	}
	var out []string
	skipping := false
	for _, ln := range lines {
		trimmed := strings.TrimSpace(ln)
		if tableHeaderRe.MatchString(ln) {
			// A new table header ends any skip and decides whether to start one.
			skipping = want[trimmed]
			if skipping {
				continue
			}
		}
		if skipping {
			continue
		}
		out = append(out, ln)
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n") + "\n"
}
