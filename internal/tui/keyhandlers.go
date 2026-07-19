package tui

// Governing: SPEC-0001 REQ "Keybinding Registry" (every action resolves through
// keys.KeyMap), REQ "Dashboard" / "Attached Mode" / "Scrollback Substate" /
// "Harness Hop" / "Confirmation Guards" / "Zero And Error States". Keystrokes
// route by overlay first, then primary mode. In attached interactive substate,
// only the intercept keys (detach/hop/scrollback/palette) are captured; every
// other keystroke forwards straight to the harness PTY (scenario "Driving a
// live agent").

import (
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// onKey is the top-level keystroke router.
func (m *Model) onKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.overlay != overlayNone {
		return m.onOverlayKey(msg)
	}
	if m.mode == modeAttached {
		return m.onAttachedKey(msg)
	}
	return m.onDashboardKey(msg)
}

// onDashboardKey handles keys on the dashboard (and its zero-states).
func (m *Model) onDashboardKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// No-daemon zero-state: offer inline start (SPEC-0001 scenario "Daemon not
	// running") — s starts the daemon, q quits.
	if m.conn == startNoDaemon {
		switch {
		case key.Matches(msg, m.keys.Start):
			m.status = "starting harnessd…"
			return m, startDaemonCmd(m.opts, m.connectCmd())
		case key.Matches(msg, m.keys.Quit):
			m.quitting = true
			return m, tea.Quit
		}
		return m, nil
	}

	switch {
	case key.Matches(msg, m.keys.Quit):
		m.quitting = true
		m.stopReadLoop()
		return m, tea.Quit
	case key.Matches(msg, m.keys.Help):
		m.overlay = overlayHelp
		return m, nil
	case key.Matches(msg, m.keys.Palette):
		return m.openPalette()
	case key.Matches(msg, m.keys.Search):
		return m.openSearch()
	case key.Matches(msg, m.keys.Profile):
		return m.openProfileSwitcher()
	case key.Matches(msg, m.keys.New):
		return m.openForm(false)
	case key.Matches(msg, m.keys.Edit):
		return m.openForm(true)
	case key.Matches(msg, m.keys.ShowAll):
		m.showAll = !m.showAll
		m.clampSel()
		return m, m.peekCmd()
	case key.Matches(msg, m.keys.Up):
		m.moveSel(-1)
		return m, m.peekCmd()
	case key.Matches(msg, m.keys.Down):
		m.moveSel(1)
		return m, m.peekCmd()
	case key.Matches(msg, m.keys.Top):
		m.sel = 0
		return m, m.peekCmd()
	case key.Matches(msg, m.keys.Bot):
		m.sel = len(m.visible()) - 1
		m.clampSel()
		return m, m.peekCmd()
	case key.Matches(msg, m.keys.Attach):
		if sel, ok := m.selectedHarness(); ok {
			return m, m.attachTo(sel, 0)
		}
		return m, nil
	case key.Matches(msg, m.keys.Start):
		return m.guardedAction(ActionStart)
	case key.Matches(msg, m.keys.Stop):
		return m.guardedAction(ActionStop)
	case key.Matches(msg, m.keys.Restart):
		return m.guardedAction(ActionRestart)
	case key.Matches(msg, m.keys.Delete):
		return m.guardedAction(ActionDelete)
	case key.Matches(msg, m.keys.Logs):
		return m, m.peekCmd()
	}
	return m, nil
}

// moveSel moves the selection by delta, clamped.
func (m *Model) moveSel(delta int) {
	m.sel += delta
	m.clampSel()
}

// guardedAction either opens a confirm dialog (destructive) or performs the
// action immediately (SPEC-0001 REQ "Confirmation Guards"; scenario "Accidental
// stop": x on a running harness intercepts before anything is signaled).
func (m *Model) guardedAction(a Action) (tea.Model, tea.Cmd) {
	sel, ok := m.selectedHarness()
	if !ok {
		return m, nil
	}
	if needsConfirm(a, m.opts.SkipConfirm) {
		m.overlay = overlayConfirm
		m.confirm = confirmState{action: a, target: sel.Name, prompt: confirmPrompt(a, sel.Name)}
		return m, nil
	}
	return m, m.performAction(a, sel.Name)
}

// performAction dispatches the actual control op (or a config delete + reload).
func (m *Model) performAction(a Action, name string) tea.Cmd {
	if m.ctrl == nil {
		return nil
	}
	if a == ActionDelete {
		return m.deleteHarnessCmd(name)
	}
	return doAction(m.ctrl, a, name)
}

// --- attached mode -------------------------------------------------------

// onAttachedKey handles keys while attached.
func (m *Model) onAttachedKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.att == nil {
		m.mode = modeDashboard
		return m, nil
	}
	if m.att.substate == substateScrollback {
		return m.onScrollbackKey(msg)
	}

	// Interactive substate. Intercept the chords; forward everything else to the
	// PTY (unless read-only).
	switch {
	case key.Matches(msg, m.keys.Detach):
		return m, m.detach()
	case msg.Type == tea.KeyEscape:
		if m.att.pendingEsc {
			m.att.pendingEsc = false
			return m, m.detach()
		}
		m.att.pendingEsc = true
		return m, nil
	case key.Matches(msg, m.keys.HopPrev):
		return m, m.hopTo(-1)
	case key.Matches(msg, m.keys.HopNext):
		return m, m.hopTo(1)
	case key.Matches(msg, m.keys.Scrollback):
		m.att.enterScrollback(m.peekLines(), m.att.view.rows)
		return m, nil
	case key.Matches(msg, m.keys.Palette):
		return m.openPalette()
	}

	m.att.pendingEsc = false
	// Forward to the PTY (read-write only; a read-only attach ignores input,
	// SPEC-0001 REQ "Attached Mode" + ADR-0008).
	if !m.att.readOnly() && m.attach != nil {
		if b := keyToBytes(msg); len(b) > 0 {
			sid := m.att.sessionID
			data := b
			return m, func() tea.Msg { _ = m.attach.AttachInput(sid, data); return nil }
		}
	}
	return m, nil
}

// onScrollbackKey handles the frozen scrollback substate (SPEC-0001 REQ
// "Scrollback Substate").
func (m *Model) onScrollbackKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	sb := m.att.scroll
	if m.att.searchOn {
		switch msg.Type {
		case tea.KeyEscape:
			m.att.searchOn = false
			m.att.search.Blur()
			return m, nil
		case tea.KeyEnter:
			sb.search(m.att.search.Value())
			m.att.searchOn = false
			m.att.search.Blur()
			return m, nil
		}
		var cmd tea.Cmd
		m.att.search, cmd = m.att.search.Update(msg)
		sb.search(m.att.search.Value()) // live-preview matches
		return m, cmd
	}

	switch {
	case key.Matches(msg, m.keys.Live):
		m.att.exitScrollback()
		return m, nil
	case key.Matches(msg, m.keys.Up):
		sb.scrollBy(-1)
	case key.Matches(msg, m.keys.Down):
		sb.scrollBy(1)
	case key.Matches(msg, m.keys.PageUp):
		sb.scrollBy(-sb.height)
	case key.Matches(msg, m.keys.PageDown):
		sb.scrollBy(sb.height)
	case key.Matches(msg, m.keys.Top):
		sb.toTop()
	case key.Matches(msg, m.keys.Bot):
		sb.toBottom()
	case key.Matches(msg, m.keys.Search):
		m.att.searchOn = true
		m.att.search.Focus()
		m.att.search.SetValue("")
		return m, textinputBlink()
	case msg.String() == "n":
		sb.nextMatch()
	case msg.String() == "N":
		sb.prevMatch()
	}
	return m, nil
}

// hopTo hops the attach to the prev/next visible harness (SPEC-0001 REQ "Harness
// Hop"). direction is -1 (prev, `[`) or +1 (next, `]`).
func (m *Model) hopTo(direction int) tea.Cmd {
	v := m.visible()
	if len(v) < 2 || m.att == nil {
		return nil
	}
	cur := selectByName(v, m.att.name)
	if cur < 0 {
		cur = 0
	}
	next := hopIndex(cur, len(v), direction)
	m.sel = next
	return m.attachTo(v[next], direction)
}

// attachTo opens (or hops to) an attach session for info. A hop (direction!=0)
// closes the prior session, increments the session id, and kicks the spring.
func (m *Model) attachTo(info protocol.HarnessInfo, direction int) tea.Cmd {
	cols, rows := m.attachViewport()
	sid := sessionBase
	var closeCmd tea.Cmd
	if m.att != nil {
		prev := m.att.sessionID
		sid = prev + 1
		if m.attach != nil {
			closeCmd = func() tea.Msg { _ = m.attach.AttachClose(prev); return nil }
		}
	}
	mode := protocol.AttachRW
	m.att = newAttachState(info.Name, mode, sid, cols, rows)
	if direction != 0 {
		m.att.impulseHop(direction)
	}
	m.mode = modeAttached

	openCmd := tea.Cmd(nil)
	if m.attach != nil {
		name := info.Name
		openCmd = func() tea.Msg {
			_ = m.attach.AttachOpen(sid, name, cols, rows, mode)
			return nil
		}
	}
	return tea.Batch(closeCmd, openCmd)
}

// detach closes the attach session and returns to the Dashboard; the harness
// keeps running (SPEC-0001 scenario "Detach returns home").
func (m *Model) detach() tea.Cmd {
	var cmd tea.Cmd
	if m.att != nil && m.attach != nil {
		sid := m.att.sessionID
		cmd = func() tea.Msg { _ = m.attach.AttachClose(sid); return nil }
	}
	m.att = nil
	m.mode = modeDashboard
	return cmd
}

// peekLines returns the current peek text split into lines, used as the frozen
// scrollback buffer when entering the substate.
func (m *Model) peekLines() []string {
	return splitLines(m.peek.text)
}
