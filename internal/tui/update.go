package tui

// Governing: SPEC-0001 (the whole mode machine + overlays + zero/error states).
// Update is the reactive core: async daemon messages (from the read loop and
// control Cmds) and keystrokes both flow here. Keystrokes route by the active
// overlay first, then the primary mode.

import (
	"context"
	"fmt"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"gitea.stump.rocks/stump.wtf/harness/internal/buildinfo"
	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
	"gitea.stump.rocks/stump.wtf/harness/internal/tui/chatroom"
	"github.com/stump-wtf/agent-trace/tail"
)

// Update implements tea.Model.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.help.SetWidth(msg.Width)
		if m.overlay == overlayForm {
			// Keep the Huh form bounded to the (resized) overlay viewport so it
			// scrolls rather than overflowing a short terminal (issue #25).
			return m, m.sizeForm()
		}
		if m.att != nil {
			cols, rows := m.attachViewport()
			m.att.view.resize(cols, rows)
			if m.attach != nil {
				_ = m.attach.AttachResize(m.att.sessionID, cols, rows)
			}
			if m.att.scroll != nil {
				// Scrollback geometry was frozen at entry; rebind it to the
				// new window or the old height renders more rows than the
				// window has and the alt-screen scrolls.
				m.att.scroll.height = m.scrollbackHeight()
				m.att.scroll.scrollBy(0) // re-clamp top against the new maxTop
			}
		}
		// Attach-only mode: if we deferred the auto-attach because the window
		// size wasn't known yet (m.w was 0 when onRefresh ran), retry now that
		// we have real dimensions. Without this, the daemon opens the attach
		// at the 80×24 fallback and the embedded terminal renders too narrow
		// until a later resize corrects it.
		if m.attachOnlyPending != "" && m.w > 0 && m.h > 0 && m.ctrl != nil {
			name := m.attachOnlyPending
			m.attachOnlyPending = ""
			if h := m.harnessByName(name); h != nil {
				return m, m.attachTo(*h, 0)
			}
			m.conn, m.connErr = startOtherErr, fmt.Errorf("no such harness: %s", name)
		}
		// The preview pane just changed size, so the guest it is sized to has
		// to follow (#200). Debounced like a selection change: dragging a
		// window edge emits a WindowSizeMsg per frame, and each one would
		// otherwise be a PTY resize and a SIGWINCH into a live agent.
		if m.mode == modeDashboard {
			return m, m.peekTargetChanged()
		}
		if m.mode == modeChatroom && m.chatroom != nil {
			m.chatroom.Update(msg)
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
		// Feed the emulator in every substate: scrollback reads from its
		// frozen copy, so writing through costs it nothing — while dropping
		// the bytes would leave the live view stale (and missing partial
		// escape sequences) when the user exits scrollback.
		if m.att != nil && msg.sessionID == m.att.sessionID {
			m.att.view.write(msg.data)
		}
		// The dashboard's preview is its own read-only session on the same
		// connection (#200), so route by id — ids are unique across both, and
		// a frame still in flight from a closed session matches neither.
		if m.peekView != nil && m.peekSess != 0 && msg.sessionID == m.peekSess {
			m.peekView.write(msg.data)
		}
		return m, waitForFrame(m.events)

	case peekSyncMsg:
		// Superseded by a newer target — this is the debounce (see peek.go).
		if msg.gen != m.peekGen {
			return m, nil
		}
		m.peekSyncedGen = msg.gen
		return m, m.syncPeekSession()

	case attachErrorMsg:
		m.status = msg.err.Error()
		return m, waitForFrame(m.events)

	case copyResultMsg:
		if msg.ok {
			m.status = "copied: " + msg.text
		} else {
			m.status = "nothing to copy"
		}
		return m, nil

	case disconnectMsg:
		return m.onDisconnect(msg)

	case tickMsg:
		return m.onTick()

	case spinner.TickMsg:
		// Keep the spinner spinning while any visible harness (or the
		// currently-attached one) is in a transient state. The View renders
		// the spinner frame in place of the static state glyph for those
		// rows, so the row reads as "alive" rather than frozen.
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		if !m.spinnerActive() {
			// Nothing left to animate — let the spinner rest until the next
			// transient state appears (avoids a perpetual ~120ms tick).
			return m, nil
		}
		return m, cmd
	case tea.KeyPressMsg:
		return m.onKey(msg)

	case chatroom.MsgEvent:
		if m.mode == modeChatroom && m.chatroom != nil {
			m.chatroom.Update(msg)
			return m, chatroom.WaitForEvents(m.chatroom)
		}
		// In dashboard mode, track last-action for the live field and
		// re-arm the dash watcher for the next event.
		m.trackLastAction(msg.Event)
		return m, m.dashEventCmd()

	case tea.MouseMsg:
		return m.onMouse(msg)

	case tea.BackgroundColorMsg:
		return m.onBackgroundColor(msg)

	case probeSizeMsg:
		// Fallback size detection: if Bubble Tea's own checkResize never
		// fired (stdout not detected as a TTY), use our direct probe. Only
		// apply if m.w is still 0 so we don't override a real WindowSizeMsg.
		if m.w == 0 && msg.w > 0 && msg.h > 0 {
			m.w, m.h = msg.w, msg.h
			m.help.SetWidth(msg.w)
			// Same deferred-attach logic as WindowSizeMsg: if we were waiting
			// for a size before opening the attach, do it now.
			if m.attachOnlyPending != "" && m.ctrl != nil {
				name := m.attachOnlyPending
				m.attachOnlyPending = ""
				if h := m.harnessByName(name); h != nil {
					return m, m.attachTo(*h, 0)
				}
				m.conn, m.connErr = startOtherErr, fmt.Errorf("no such harness: %s", name)
			}
		}
		return m, nil
	}

	// Route to the Huh form when it's open.
	if m.overlay == overlayForm && m.form != nil {
		return m.updateForm(msg)
	}
	return m, nil
}

// trackLastAction updates the dashboard's live activity field from a tail.Event.
func (m *Model) trackLastAction(ev tail.Event) {
	if m.lastActions == nil {
		m.lastActions = make(map[string]string)
	}
	key := string(ev.Session.Harness)
	action := chatroom.LastAction(ev)
	if action != "" {
		m.lastActions[key] = action + " " + chatroom.FormatTime(ev.Classified.Timestamp)
	}
}

// dashEventCmd is the tea.Cmd that reads the next event from the dashboard
// watcher. It returns a chatroom.MsgEvent so the main Update can handle it
// uniformly.
func (m *Model) dashEventCmd() tea.Cmd {
	if m.dashWatcher == nil {
		return nil
	}
	return func() tea.Msg {
		ev, ok := <-m.dashWatcher.Events()
		if !ok {
			return nil
		}
		return chatroom.MsgEvent{Event: ev}
	}
}

// startDashWatcher starts the background watcher for the dashboard's live
// activity field. Called once the daemon connection is established.
func (m *Model) startDashWatcher() tea.Cmd {
	if m.dashWatcher != nil {
		return nil
	}
	m.dashWatcher = tail.NewWatcherWithConfig(tail.DefaultWatchConfig(), tail.DefaultAdapters())
	ctx, cancel := context.WithCancel(context.Background())
	_ = cancel
	m.dashWatcher.Start(ctx)
	return m.dashEventCmd()
}

// stopDashWatcher tears down the dashboard watcher.
func (m *Model) stopDashWatcher() {
	if m.dashWatcher != nil {
		m.dashWatcher.Stop()
		m.dashWatcher = nil
	}
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
	cmds := []tea.Cmd{fetchState(m.ctrl), m.startReadLoop(), m.startDashWatcher()}
	// `harness attach <name>`: once we're connected and have a controller,
	// auto-attach to the named harness. We need the fresh state first to
	// resolve the HarnessInfo, so piggyback on refreshMsg's handling below by
	// setting the pending flag — onRefresh consumes it.
	return m, tea.Batch(cmds...)
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
	// Advisory build-skew banner (#181): recompute on every refresh so a
	// daemon restart (or a caught-up daemon) clears it on the next poll.
	if !m.skewDismissed {
		m.skewNotice = buildinfo.SkewNotice(m.daemon.Version, buildinfo.Version)
	}
	if prevName != "" {
		if i := selectByName(m.visible(), prevName); i >= 0 {
			m.sel = i
		}
	}
	m.clampSel()
	m.scrollListToSel()
	cmds := []tea.Cmd{m.peekTargetChanged(), m.maybeStartSpinner()}
	// `harness attach <name>`: first successful refresh after connect — find
	// the named harness and auto-attach. But ONLY if we already know the
	// window size (m.w > 0); otherwise the attach opens at the 80×24 fallback
	// and renders too narrow. If m.w is still 0, leave the pending flag set —
	// the WindowSizeMsg handler picks it up.
	if m.attachOnlyPending != "" && m.w > 0 && m.h > 0 {
		name := m.attachOnlyPending
		m.attachOnlyPending = ""
		if h := m.harnessByName(name); h != nil {
			cmds = append(cmds, m.attachTo(*h, 0))
		} else {
			m.conn, m.connErr = startOtherErr, fmt.Errorf("no such harness: %s", name)
		}
	}
	return m, tea.Batch(cmds...)
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
	m.scrollListToSel()
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
	m.scrollListToSel()
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
		// The polled tail keeps running even once the stream is driving the
		// pane. It is not duplicate traffic: peekView holds the visible screen,
		// while the `logs` tail is the 200 lines of HISTORY that peekLines()
		// hands to attached scrollback. Skipping it froze m.peek.text the
		// moment the session went live, so pressing ↵ after watching a harness
		// for two minutes and then scrolling back showed the buffer as it was
		// two minutes ago.
		cmds = append(cmds, m.peekCmd())
		// Reconcile on the tick as well as on change: the pane's geometry moves
		// with the status line and the banners, not only with the selection.
		//
		// Only once the target has settled, though. A tick that reconciles
		// while a debounce is still outstanding IS a reconcile on change, just
		// on a one-second grid — holding j would open a session on whatever row
		// the tick caught, resize that guest's PTY and SIGWINCH it, then tear it
		// down on the next one. That is the churn peekSettleDelay exists to
		// prevent.
		if m.peekSyncedGen == m.peekGen {
			cmds = append(cmds, m.syncPeekSession())
		}
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
