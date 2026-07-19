package tui

// Governing: SPEC-0001 (the whole mode machine + overlays + zero/error states).
// Update is the reactive core: async daemon messages (from the read loop and
// control Cmds) and keystrokes both flow here. Keystrokes route by the active
// overlay first, then the primary mode.

import (
	tea "github.com/charmbracelet/bubbletea"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// Update implements tea.Model.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.help.Width = msg.Width
		if m.att != nil {
			cols, rows := m.attachViewport()
			m.att.view.resize(cols, rows)
			if m.attach != nil {
				_ = m.attach.AttachResize(m.att.sessionID, cols, rows)
			}
		}
		return m, nil

	case connectedMsg:
		return m.onConnected(msg)

	case refreshMsg:
		return m.onRefresh(msg)

	case logsMsg:
		if sel, ok := m.selectedHarness(); ok && sel.Name == msg.name {
			m.peek = msg
		}
		return m, nil

	case opResultMsg:
		return m.onOpResult(msg)

	case reloadResultMsg:
		return m.onReloadResult(msg)

	case profileSwitchMsg:
		return m.onProfileSwitch(msg)

	case eventMsg:
		return m.onEvent(msg)

	case attachDataMsg:
		if m.att != nil && msg.sessionID == m.att.sessionID && m.att.substate == substateInteractive {
			m.att.view.write(msg.data)
		}
		return m, waitForFrame(m.events)

	case attachErrorMsg:
		m.status = msg.err.Error()
		return m, waitForFrame(m.events)

	case disconnectMsg:
		return m.onDisconnect(msg)

	case tickMsg:
		return m.onTick()

	case tea.KeyMsg:
		return m.onKey(msg)
	}

	// Route to the Huh form when it's open.
	if m.overlay == overlayForm && m.form != nil {
		return m.updateForm(msg)
	}
	return m, nil
}

// onConnected wires up (or classifies the failure of) the daemon connection.
func (m *Model) onConnected(msg connectedMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.conn = classifyDialErr(msg.err)
		m.connErr = msg.err
		return m, nil
	}
	m.ctrl, m.attach = msg.ctrl, msg.attach
	m.conn = startOK
	m.reconn = false
	return m, tea.Batch(fetchState(m.ctrl), m.startReadLoop())
}

// onRefresh installs a fresh snapshot, keeping the selection pinned to the same
// harness by name where possible.
func (m *Model) onRefresh(msg refreshMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.status = msg.err.Error()
		return m, nil
	}
	prevName := ""
	if sel, ok := m.selectedHarness(); ok {
		prevName = sel.Name
	}
	m.harnesses = msg.harnesses
	m.profiles = msg.profiles
	m.daemon = msg.daemon
	if prevName != "" {
		if i := selectByName(m.visible(), prevName); i >= 0 {
			m.sel = i
		}
	}
	m.clampSel()
	return m, m.peekCmd()
}

// onOpResult reports the outcome of a lifecycle action and refreshes.
func (m *Model) onOpResult(msg opResultMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.status = msg.err.Error()
		return m, nil
	}
	m.status = string(msg.action) + " " + msg.name + " → " + msg.info.State
	return m, fetchState(m.ctrl)
}

// onReloadResult applies a reload; a parse failure keeps last-good config and
// raises the non-fatal banner (SPEC-0001 scenario "Bad config reload").
func (m *Model) onReloadResult(msg reloadResultMsg) (tea.Model, tea.Cmd) {
	if b := reloadBanner(msg.err); b != "" {
		m.banner = b
		return m, nil
	}
	if msg.err != nil {
		m.status = msg.err.Error()
		return m, nil
	}
	m.banner = ""
	m.harnesses = msg.harnesses
	m.clampSel()
	return m, fetchState(m.ctrl)
}

// onProfileSwitch applies a profile switch result.
func (m *Model) onProfileSwitch(msg profileSwitchMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.status = msg.err.Error()
		return m, nil
	}
	m.profiles = msg.profiles
	m.sel = 0
	if len(msg.toStart) > 0 {
		m.status = "started " + joinNames(msg.toStart)
	}
	m.clampSel()
	return m, fetchState(m.ctrl)
}

// onEvent reacts to a pushed lifecycle event. A config-reload event may carry a
// parse failure that raises the banner; everything else just refreshes state.
func (m *Model) onEvent(msg eventMsg) (tea.Model, tea.Cmd) {
	cmd := waitForFrame(m.events)
	switch msg.ev.Kind {
	case protocol.EvConfigReload:
		// A successful reload clears the banner; the daemon signals a failed
		// parse via a reload_failed error on the control path, handled there.
		m.banner = ""
		return m, tea.Batch(cmd, fetchState(m.ctrl))
	default:
		return m, tea.Batch(cmd, fetchState(m.ctrl))
	}
}

// onDisconnect shows the reconnecting overlay (harnesses are fine; only the view
// dropped, ADR-0002) and schedules a reconnect attempt.
func (m *Model) onDisconnect(msg disconnectMsg) (tea.Model, tea.Cmd) {
	if !isDisconnect(msg.err) && msg.err != errChannelClosed {
		m.status = msg.err.Error()
	}
	m.stopReadLoop()
	m.reconn = true
	m.ctrl = nil
	m.attach = nil
	// The periodic tick (onTick) retries the connection while reconn is set, so
	// no separate timer is needed here.
	return m, nil
}

// onTick advances animations and periodically refreshes the peek pane, then
// re-arms the tick.
func (m *Model) onTick() (tea.Model, tea.Cmd) {
	cmds := []tea.Cmd{tick()}
	if m.reconn {
		// Retry the connection while disconnected.
		cmds = append(cmds, m.connectCmd())
	}
	if m.att != nil && m.att.animate() {
		// keep ticking to finish the hop animation (tick already re-armed)
	}
	if m.mode == modeDashboard && m.overlay == overlayNone && m.conn == startOK {
		cmds = append(cmds, m.peekCmd())
	}
	return m, tea.Batch(cmds...)
}

// peekCmd fetches the read-only tail for the current selection (the live peek
// pane, SPEC-0001 REQ "Dashboard").
func (m *Model) peekCmd() tea.Cmd {
	if m.ctrl == nil {
		return nil
	}
	sel, ok := m.selectedHarness()
	if !ok {
		return nil
	}
	return fetchLogs(m.ctrl, sel.Name, peekLines)
}
