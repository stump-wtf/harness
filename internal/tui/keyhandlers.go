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

// onMouse handles mouse events. In attached mode, mouse-wheel up enters
// scrollback (so scrolling "just works" when you scroll the terminal region);
// in scrollback, wheel up/down navigates. Mouse events on the status bar are
// ignored — it's a display surface, not interactive. On the dashboard, wheel
// scrolls the list.
func (m *Model) onMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if m.mode == modeAttached && m.att != nil {
		return m.onAttachedMouse(msg)
	}
	// Dashboard: wheel scrolls the harness list.
	if m.mode == modeDashboard {
		switch msg.Type { //nolint:exhaustive
		case tea.MouseWheelUp:
			m.moveSel(-1)
			return m, m.peekCmd()
		case tea.MouseWheelDown:
			m.moveSel(1)
			return m, m.peekCmd()
		}
	}
	return m, nil
}

// onAttachedMouse handles mouse events while in attached mode. Wheel-up in
// the interactive substate enters scrollback; wheel in scrollback navigates.
func (m *Model) onAttachedMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	switch msg.Type { //nolint:exhaustive
	case tea.MouseWheelUp:
		if m.att.substate == substateInteractive {
			// Scroll up from live → enter scrollback at the bottom.
			m.att.enterScrollback(m.peekLines(), m.scrollbackHeight())
			return m, nil
		}
		// Already in scrollback: scroll up.
		m.att.scroll.scrollBy(-1)
		return m, nil
	case tea.MouseWheelDown:
		if m.att.substate == substateScrollback {
			m.att.scroll.scrollBy(1)
			return m, nil
		}
	}
	return m, nil
}

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
			m.status = "starting daemon…"
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
//
// All harness intercepts use a prefix model (like tmux): press Ctrl-b (the
// prefix), then the command key. Bubbles' key.Matches does NOT support
// sequential-key chords — "ctrl+b d" in a Binding is matched against a single
// KeyMsg's .String(), but Ctrl-b and d arrive as two separate KeyMsgs — so we
// implement the prefix state machine ourselves: Ctrl-b arms prefixArmed, the
// next key is intercepted as a harness command. Every other keystroke goes
// straight to the PTY. This means bare s/r/[/]/etc. always reach the agent.
func (m *Model) onAttachedKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.att == nil {
		m.mode = modeDashboard
		return m, nil
	}
	if m.att.substate == substateScrollback {
		return m.onScrollbackKey(msg)
	}

	// If the prefix is armed, intercept the next key as a harness command.
	// PgUp is a special case: it enters scrollback without the prefix (it's a
	// dedicated key with no agent-TUI collision risk).
	if m.att.prefixArmed {
		m.att.prefixArmed = false
		return m, m.dispatchPrefixKey(msg)
	}

	// Ctrl-b arms the prefix (only in read-write mode — read-only viewers
	// don't need intercepts, their keys are dropped anyway).
	if !m.att.readOnly() && msg.Type == tea.KeyCtrlB {
		m.att.prefixArmed = true
		return m, nil
	}

	// PgUp enters scrollback without the prefix.
	if key.Matches(msg, m.keys.Scrollback) {
		m.att.enterScrollback(m.peekLines(), m.scrollbackHeight())
		return m, nil
	}

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

// dispatchPrefixKey handles the keystroke that follows the Ctrl-b prefix. It
// resolves the command key to an action, or cancels (no-op) if the key isn't
// a known chord — in which case the user mistyped and we don't forward the
// stray key to the PTY (tmux behavior: unknown prefix command = cancel).
func (m *Model) dispatchPrefixKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "d":
		return m.detach()
	case "s":
		return m.performAction(ActionStart, m.att.name)
	case "r":
		return m.performAction(ActionRestart, m.att.name)
	case "h":
		return m.hopTo(-1)
	case "l":
		return m.hopTo(1)
	case "[":
		m.att.enterScrollback(m.peekLines(), m.scrollbackHeight())
		return nil
	}
	// Unknown prefix command — cancel silently (tmux behavior).
	return nil
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
	// A read-only Model (e.g. a read-only remote SSH session, ADR-0008) opens
	// every attach as AttachRO so the daemon drops this client's keystrokes;
	// otherwise the local default is the interactive read-write attach.
	mode := protocol.AttachRW
	if m.opts.ReadOnly {
		mode = protocol.AttachRO
	}
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
// keeps running (SPEC-0001 scenario "Detach returns home"). In attach-only
// mode (the `harness attach <name>` CLI verb) there's no dashboard to return
// to, so detach quits the program.
func (m *Model) detach() tea.Cmd {
	var cmd tea.Cmd
	if m.att != nil && m.attach != nil {
		sid := m.att.sessionID
		cmd = func() tea.Msg { _ = m.attach.AttachClose(sid); return nil }
	}
	m.att = nil
	if m.opts.AttachOnly != "" {
		// Attach-only mode: detach = quit. Tear down cleanly.
		m.quitting = true
		return tea.Batch(cmd, tea.Quit)
	}
	m.mode = modeDashboard
	return cmd
}

// peekLines returns the current peek text split into lines, used as the frozen
// scrollback buffer when entering the substate.
func (m *Model) peekLines() []string {
	return splitLines(m.peek.text)
}
