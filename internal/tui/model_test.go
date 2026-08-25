package tui

import (
	"os"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// --- fakes ---------------------------------------------------------------

// fakeController is an in-memory Controller recording mutating calls.
type fakeController struct {
	mu           sync.Mutex
	harnesses    []protocol.HarnessInfo
	profiles     []protocol.ProfileInfo
	stopCalls    []string
	startCalls   []string
	rstCalls     []string
	enableCalls  []string
	disableCalls []string
	useProfile   string
	// logCalls counts `logs` fetches. The preview stops polling once its
	// attach session is streaming the same screen (#200), and a counter is the
	// only way to see traffic that is supposed to STOP.
	logCalls int
}

func (f *fakeController) List() ([]protocol.HarnessInfo, error) { return f.harnesses, nil }
func (f *fakeController) Describe(n string) (protocol.HarnessInfo, error) {
	return protocol.HarnessInfo{Name: n}, nil
}
func (f *fakeController) Start(n string) (protocol.HarnessInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startCalls = append(f.startCalls, n)
	return protocol.HarnessInfo{Name: n, State: "starting"}, nil
}
func (f *fakeController) Stop(n string) (protocol.HarnessInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopCalls = append(f.stopCalls, n)
	return protocol.HarnessInfo{Name: n, State: "stopping"}, nil
}
func (f *fakeController) Restart(n string) (protocol.HarnessInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rstCalls = append(f.rstCalls, n)
	return protocol.HarnessInfo{Name: n, State: "starting"}, nil
}
func (f *fakeController) Enable(n string) (protocol.HarnessInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enableCalls = append(f.enableCalls, n)
	return protocol.HarnessInfo{Name: n, State: "starting", Enabled: true}, nil
}
func (f *fakeController) Disable(n string) (protocol.HarnessInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disableCalls = append(f.disableCalls, n)
	return protocol.HarnessInfo{Name: n, State: "stopping", Enabled: false}, nil
}
func (f *fakeController) Logs(n string, lines int) (protocol.LogsData, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logCalls++
	return protocol.LogsData{Name: n, Text: "log line\n"}, nil
}
func (f *fakeController) Profiles() ([]protocol.ProfileInfo, error) { return f.profiles, nil }
func (f *fakeController) UseProfile(n string) ([]protocol.ProfileInfo, error) {
	f.mu.Lock()
	f.useProfile = n
	f.mu.Unlock()
	out := make([]protocol.ProfileInfo, len(f.profiles))
	copy(out, f.profiles)
	for i := range out {
		out[i].Active = out[i].Name == n
	}
	return out, nil
}
func (f *fakeController) Reload() ([]protocol.HarnessInfo, error) { return f.harnesses, nil }
func (f *fakeController) DaemonInfo() (protocol.DaemonInfo, error) {
	return protocol.DaemonInfo{Version: "test"}, nil
}
func (f *fakeController) DaemonVersion() string { return "test" }
func (f *fakeController) Close() error          { return nil }

// fakeAttach records attach data-plane calls; Conn() is unused by these tests.
type fakeAttach struct {
	opens  []string
	inputs [][]byte
	closes []uint32
	// openSizes/resizes record the viewport the client asks the daemon for: the
	// size at which each session was opened, and every subsequent resize. The
	// daemon sizes the harness's real PTY from these, so they are the client
	// half of "the harness fills the window and follows it" (ADR-0003).
	openSizes []viewport
	resizes   []viewport
	// openModes records rw/ro per open. The dashboard's preview must open
	// read-only (ADR-0008, #200) — a preview that accepted input would forward
	// the user's list navigation into the guest.
	openModes []protocol.AttachMode
	// order is the sequence of calls as they reached the wire ("open",
	// "close", "resize"). Attaching has to close the preview session BEFORE
	// opening the full-window one, or smallest-attached-wins clamps the guest
	// to the pane; only ordering shows that, not the per-call slices.
	order []string
}

// viewport is a recorded cols×rows the client reported for a session.
type viewport struct {
	sid        uint32
	cols, rows int
}

func (f *fakeAttach) AttachOpen(sid uint32, name string, cols, rows int, mode protocol.AttachMode) error {
	f.opens = append(f.opens, name)
	f.openSizes = append(f.openSizes, viewport{sid, cols, rows})
	f.openModes = append(f.openModes, mode)
	f.order = append(f.order, "open")
	return nil
}
func (f *fakeAttach) AttachInput(sid uint32, data []byte) error {
	f.inputs = append(f.inputs, append([]byte(nil), data...))
	return nil
}
func (f *fakeAttach) AttachResize(sid uint32, cols, rows int) error {
	f.resizes = append(f.resizes, viewport{sid, cols, rows})
	f.order = append(f.order, "resize")
	return nil
}
func (f *fakeAttach) AttachClose(sid uint32) error {
	f.closes = append(f.closes, sid)
	f.order = append(f.order, "close")
	return nil
}
func (f *fakeAttach) Conn() *protocol.Conn { return nil }
func (f *fakeAttach) Close() error         { return nil }

// --- key helpers ---------------------------------------------------------

// runeKey builds a printable keystroke. Bubble Tea v2 carries the characters
// in Text (v1 used Runes), and coalesces a burst of them into one message —
// which is exactly what the multi-rune expansion in onDashboardKey handles.
func runeKey(s string) tea.KeyPressMsg {
	k := tea.KeyPressMsg{Text: s}
	if r := []rune(s); len(r) > 0 {
		k.Code = r[0]
	}
	return k
}

// specialKey builds a non-printable keystroke from a v2 key code
// (tea.KeyEnter, tea.KeyPgUp, …). Text stays empty, as it does on a real one.
func specialKey(code rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: code} }

// ctrlKey builds a Ctrl-modified keystroke. v2 dropped the KeyCtrlA..Z key
// types in favour of a modifier on the base key.
func ctrlKey(code rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: code, Mod: tea.ModCtrl}
}

// drain runs a tea.Cmd (if non-nil) and returns its message, recursively
// executing any batched sub-commands (tea.Batch wraps them in a BatchMsg the
// runtime would otherwise fan out).
func drain(cmd tea.Cmd) tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			drain(c)
		}
		return nil
	}
	return msg
}

// --- tests ---------------------------------------------------------------

// TestNoDaemonState verifies the dial failure lands the TUI in the no-daemon
// offer instead of erroring out (SPEC-0001 scenario "Daemon not running").
func TestNoDaemonState(t *testing.T) {
	m := New(Options{})
	m.Update(connectedMsg{err: os.ErrNotExist})
	if m.conn != startNoDaemon {
		t.Fatalf("conn = %v, want startNoDaemon", m.conn)
	}
	if !containsStr(m.viewNoDaemon(), "start the daemon") {
		t.Error("no-daemon view should offer to start the daemon")
	}
}

// TestStopConfirmIntercepts is the SPEC-0001 scenario "Accidental stop":
// pressing x on a running harness opens a confirm dialog BEFORE anything is
// signaled; only after confirming does the stop reach the daemon.
func TestStopConfirmIntercepts(t *testing.T) {
	fc := &fakeController{harnesses: sampleHarnesses()}
	m := New(Options{})
	m.ctrl = fc
	m.harnesses = fc.harnesses
	m.sel = 0 // crush-signal, running

	// Press x → confirm overlay, nothing signaled.
	model, _ := m.onKey(runeKey("x"))
	m = model.(*Model)
	if m.overlay != overlayConfirm {
		t.Fatalf("overlay = %v, want overlayConfirm", m.overlay)
	}
	if len(fc.stopCalls) != 0 {
		t.Fatalf("stop was signaled before confirm: %v", fc.stopCalls)
	}

	// Confirm with Enter → stop is signaled for the selected harness.
	_, cmd := m.onKey(specialKey(tea.KeyEnter))
	drain(cmd)
	if len(fc.stopCalls) != 1 || fc.stopCalls[0] != "crush-signal" {
		t.Fatalf("stop calls = %v, want [crush-signal]", fc.stopCalls)
	}
}

// TestSkipConfirmSetting verifies the --yes-style setting bypasses the guard.
func TestSkipConfirmSetting(t *testing.T) {
	fc := &fakeController{harnesses: sampleHarnesses()}
	m := New(Options{SkipConfirm: true})
	m.ctrl = fc
	m.harnesses = fc.harnesses
	m.sel = 0

	_, cmd := m.onKey(runeKey("x"))
	if m.overlay == overlayConfirm {
		t.Fatal("skip-confirm should not open a dialog")
	}
	drain(cmd)
	if len(fc.stopCalls) != 1 {
		t.Fatalf("stop should fire immediately with skip-confirm, calls=%v", fc.stopCalls)
	}
}

// TestCopyYanksSelectedName verifies the copy verb (y) emits an OSC52
// clipboard write for the selected harness's name and reports it back via the
// status line. Mouse-drag selection is unavailable while the TUI holds the
// mouse (MouseModeCellMotion), so the explicit copy key is how a name leaves
// the cockpit. The OSC52 write itself goes to stderr and is a no-op in the
// test environment; we assert on the resulting copyResultMsg.
func TestCopyYanksSelectedName(t *testing.T) {
	fc := &fakeController{harnesses: sampleHarnesses()}
	m := New(Options{})
	m.ctrl = fc
	m.harnesses = fc.harnesses
	m.sel = 1 // claude-src

	_, cmd := m.onKey(runeKey("y"))
	msg := drain(cmd)
	res, ok := msg.(copyResultMsg)
	if !ok {
		t.Fatalf("copy cmd returned %T, want copyResultMsg", msg)
	}
	if !res.ok || res.text != "claude-src" {
		t.Fatalf("copyResultMsg = %+v, want {text:claude-src ok:true}", res)
	}

	// Route the message back through Update so the status line confirms.
	model, _ := m.Update(res)
	m = model.(*Model)
	if m.status != "copied: claude-src" {
		t.Fatalf("status = %q, want %q", m.status, "copied: claude-src")
	}
}

// TestCopyNoSelection verifies the copy verb is a no-op when no harness is
// selected (e.g. an empty dashboard) rather than panicking.
func TestCopyNoSelection(t *testing.T) {
	m := New(Options{})
	m.ctrl = &fakeController{}
	m.harnesses = nil
	if _, cmd := m.onKey(runeKey("y")); cmd != nil {
		t.Fatalf("copy with no selection should return a nil cmd, got %v", cmd)
	}
}

// TestDetachReturnsHome is the SPEC-0001 scenario "Detach returns home":
// detaching from attached mode returns to the Dashboard and never signals a
// stop (the harness keeps running). The detach chord itself (Ctrl-b d) is
// exercised via the key-binding registry; this test validates the detach
// *action* — the state transitions, the close call, and the no-stop guarantee.
func TestDetachReturnsHome(t *testing.T) {
	fc := &fakeController{harnesses: sampleHarnesses()}
	fa := &fakeAttach{}
	m := New(Options{})
	m.ctrl, m.attach = fc, fa
	m.harnesses = fc.harnesses
	m.mode = modeAttached
	m.att = newAttachState("crush-signal", protocol.AttachRW, 1, 80, 24)

	cmd := m.detach()
	drain(cmd)
	if m.mode != modeDashboard || m.att != nil {
		t.Fatal("detach should return to the dashboard")
	}
	if len(fc.stopCalls) != 0 {
		t.Fatalf("detach must not stop the harness, stops=%v", fc.stopCalls)
	}
	if len(fa.closes) != 1 {
		t.Fatalf("detach should close the attach session, closes=%v", fa.closes)
	}
}

// TestPrefixChordDetach exercises the Ctrl-b d two-key sequence through the
// real onKey path. This pins the critical fix: Bubbles' key.Matches does NOT
// match sequential-key chords, so we implement our own prefix state machine
// (prefixArmed). Without it, Ctrl-b d silently never detaches.
func TestPrefixChordDetach(t *testing.T) {
	fc := &fakeController{harnesses: sampleHarnesses()}
	fa := &fakeAttach{}
	m := New(Options{})
	m.ctrl, m.attach = fc, fa
	m.harnesses = fc.harnesses
	m.mode = modeAttached
	m.att = newAttachState("crush-signal", protocol.AttachRW, 1, 80, 24)

	// First key: Ctrl-b — should arm the prefix, NOT detach yet.
	m.onKey(ctrlKey('b'))
	if !m.att.prefixArmed {
		t.Fatal("Ctrl-b should arm the prefix")
	}
	if m.mode != modeAttached {
		t.Fatal("Ctrl-b alone must not detach")
	}

	// Second key: d — should detach now.
	_, cmd := m.onKey(runeKey("d"))
	drain(cmd)
	if m.att != nil && m.opts.AttachOnly == "" {
		// In non-attach-only mode, detach returns to dashboard (att is nil).
		// (In attach-only mode att is also nilled; the check below covers both.)
	}
	if m.mode != modeDashboard {
		t.Fatal("Ctrl-b d should detach to the dashboard")
	}
	if m.att != nil {
		t.Fatal("detach should clear attach state")
	}
	if len(fa.closes) != 1 {
		t.Fatalf("Ctrl-b d should close the session, closes=%v", fa.closes)
	}
}

// TestPrefixChordBareKeyForwarded confirms that a bare letter (not preceded by
// Ctrl-b) is forwarded to the PTY, not intercepted. This is the whole point of
// the prefix model: bare keys always reach the agent.
func TestPrefixChordBareKeyForwarded(t *testing.T) {
	fc := &fakeController{harnesses: sampleHarnesses()}
	fa := &fakeAttach{}
	m := New(Options{})
	m.ctrl, m.attach = fc, fa
	m.harnesses = fc.harnesses
	m.mode = modeAttached
	m.att = newAttachState("crush-signal", protocol.AttachRW, 1, 80, 24)
	inputsBefore := len(fa.inputs)

	// Bare 's' — should go to the PTY, NOT trigger start.
	_, cmd := m.onKey(runeKey("s"))
	drain(cmd)
	if m.att.prefixArmed {
		t.Fatal("bare 's' should not arm the prefix")
	}
	if len(fa.inputs) != inputsBefore+1 {
		t.Fatalf("bare 's' should be forwarded to the PTY, inputs=%v", fa.inputs)
	}
}

// TestHopSwitchesAttached is the SPEC-0001 scenario "One-keystroke hop": `]`
// while attached to A switches the attach to the next harness with the ribbon
// (spring flash) updated, without returning to the Dashboard.
func TestHopSwitchesAttached(t *testing.T) {
	fc := &fakeController{harnesses: sampleHarnesses()}
	fa := &fakeAttach{}
	m := New(Options{})
	m.ctrl, m.attach = fc, fa
	m.harnesses = fc.harnesses
	m.mode = modeAttached
	m.att = newAttachState("crush-signal", protocol.AttachRW, 1, 80, 24)

	// Hop chord (^b l) is a two-key sequence that's hard to synthesize in a
	// unit test; call hopTo directly to validate the hop action itself. The
	// chord → hopTo wiring is covered by the keys package's binding table.
	cmd := m.hopTo(1)
	drain(cmd)

	if m.mode != modeAttached {
		t.Fatal("hop must stay attached")
	}
	if m.att.name != "claude-src" {
		t.Fatalf("hopped to %q, want claude-src (next)", m.att.name)
	}
	if m.att.flash == 0 {
		t.Error("hop should kick the ribbon flash / spring")
	}
	if len(fa.opens) == 0 || fa.opens[len(fa.opens)-1] != "claude-src" {
		t.Fatalf("hop should open an attach to claude-src, opens=%v", fa.opens)
	}
}

// TestReadOnlyIgnoresInput is the SPEC-0001 scenario "Read-only badge": a
// read-only attach ignores keystrokes (ADR-0008), while a read-write attach
// forwards them to the PTY.
func TestReadOnlyIgnoresInput(t *testing.T) {
	fc := &fakeController{harnesses: sampleHarnesses()}

	// Read-only: input dropped.
	ro := newModelAttached(fc, protocol.AttachRO)
	_, cmd := ro.m.onKey(runeKey("a"))
	drain(cmd)
	if len(ro.fa.inputs) != 0 {
		t.Fatalf("read-only attach forwarded input: %v", ro.fa.inputs)
	}

	// Read-write: input forwarded to the PTY.
	rw := newModelAttached(fc, protocol.AttachRW)
	_, cmd = rw.m.onKey(runeKey("a"))
	drain(cmd)
	if len(rw.fa.inputs) != 1 || string(rw.fa.inputs[0]) != "a" {
		t.Fatalf("read-write attach should forward 'a', got %v", rw.fa.inputs)
	}
}

// TestConfigParseBanner is the SPEC-0001 scenario "Bad config reload": a
// reload_failed result raises the non-fatal banner (last-good config kept).
func TestConfigParseBanner(t *testing.T) {
	m := New(Options{})
	m.Update(reloadResultMsg{err: &protocol.ErrorMsg{Code: protocol.ErrReload, Message: "harness.toml:12: bad"}})
	if m.banner == "" || !containsStr(m.banner, ":12:") {
		t.Fatalf("banner = %q, want a last-good parse-location banner", m.banner)
	}
}

// TestProfileSwitchStartsStopped exercises the two-step switcher end to end: pick
// a profile, accept "start stopped", and the resulting Cmd starts only the
// profile's stopped members (SPEC-0001 scenario "Non-destructive switch").
func TestProfileSwitchStartsStopped(t *testing.T) {
	fc := &fakeController{
		harnesses: []protocol.HarnessInfo{
			{Name: "up", State: "running"},
			{Name: "down", State: "stopped"},
		},
		profiles: []protocol.ProfileInfo{
			{Name: "P", Harnesses: []string{"up", "down"}},
		},
	}
	m := New(Options{})
	m.ctrl = fc
	m.harnesses = fc.harnesses
	m.profiles = fc.profiles

	m.openProfileSwitcher()
	m.onKey(specialKey(tea.KeyEnter)) // choose profile 0 → askStart
	if !m.prof.askStart {
		t.Fatal("selecting a profile should ask about starting stopped members")
	}
	_, cmd := m.onKey(runeKey("y")) // accept start-stopped
	drain(cmd)

	if fc.useProfile != "P" {
		t.Fatalf("expected UseProfile(P), got %q", fc.useProfile)
	}
	if len(fc.startCalls) != 1 || fc.startCalls[0] != "down" {
		t.Fatalf("start calls = %v, want [down] (only the stopped member)", fc.startCalls)
	}
}

// TestPaletteExecuteRestart drives the palette scenario in the model: open,
// type "rest redu", Enter — the restart reaches the daemon for reduit-agent.
func TestPaletteExecuteRestart(t *testing.T) {
	fc := &fakeController{harnesses: sampleHarnesses()}
	m := New(Options{})
	m.ctrl = fc
	m.harnesses = fc.harnesses
	m.profiles = nil

	m.openPalette()
	for _, r := range "rest redu" {
		m.onKey(runeKey(string(r)))
	}
	_, cmd := m.onKey(specialKey(tea.KeyEnter))
	drain(cmd)
	if len(fc.rstCalls) != 1 || fc.rstCalls[0] != "reduit-agent" {
		t.Fatalf("palette restart calls = %v, want [reduit-agent]", fc.rstCalls)
	}
}

// TestScrollbackEntryFromAttached verifies Ctrl-b [ enters the frozen scrollback
// substate (SPEC-0001 REQ "Scrollback Substate").
func TestScrollbackEntryFromAttached(t *testing.T) {
	fc := &fakeController{harnesses: sampleHarnesses()}
	ma := newModelAttached(fc, protocol.AttachRW)
	ma.m.peek = logsMsg{name: "crush-signal", text: "one\ntwo\nthree\n"}

	_, _ = ma.m.onKey(specialKey(tea.KeyPgUp)) // PgUp enters scrollback
	if ma.m.att.substate != substateScrollback {
		t.Fatal("PgUp should enter scrollback substate")
	}
	// q returns to live.
	_, _ = ma.m.onKey(runeKey("q"))
	if ma.m.att.substate != substateInteractive {
		t.Fatal("q should return to live")
	}
}

// --- helpers -------------------------------------------------------------

type attachedFixture struct {
	m  *Model
	fa *fakeAttach
}

func newModelAttached(fc *fakeController, mode protocol.AttachMode) attachedFixture {
	fa := &fakeAttach{}
	m := New(Options{})
	m.ctrl, m.attach = fc, fa
	m.harnesses = fc.harnesses
	m.mode = modeAttached
	m.att = newAttachState("crush-signal", mode, 1, 80, 24)
	return attachedFixture{m: m, fa: fa}
}

func containsStr(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestPrefixChordHelp verifies Ctrl-b ? opens the keymap overlay from attached
// mode (and that a bare `?` is instead forwarded to the PTY, not intercepted).
func TestPrefixChordHelp(t *testing.T) {
	fc := &fakeController{harnesses: sampleHarnesses()}
	fa := &fakeAttach{}
	m := New(Options{})
	m.ctrl, m.attach = fc, fa
	m.harnesses = fc.harnesses
	m.mode = modeAttached
	m.att = newAttachState("crush-signal", protocol.AttachRW, 1, 80, 24)

	// Bare `?` must reach the agent, not open help.
	_, cmd := m.onKey(runeKey("?"))
	drain(cmd)
	if m.overlay == overlayHelp {
		t.Fatal("bare ? must not open help in attached mode")
	}
	if len(fa.inputs) != 1 || string(fa.inputs[0]) != "?" {
		t.Fatalf("bare ? should be forwarded to the PTY, inputs=%v", fa.inputs)
	}

	// Ctrl-b ? opens the keymap overlay.
	m.onKey(ctrlKey('b'))
	if !m.att.prefixArmed {
		t.Fatal("Ctrl-b should arm the prefix")
	}
	m.onKey(runeKey("?"))
	if m.overlay != overlayHelp {
		t.Fatalf("Ctrl-b ? should open the keymap overlay, overlay=%v", m.overlay)
	}
	// The ? that opened help must not have leaked to the PTY.
	if len(fa.inputs) != 1 {
		t.Fatalf("prefixed ? must not be forwarded to the PTY, inputs=%v", fa.inputs)
	}
}

// mouseGrabbed reports whether the model is currently asking the terminal for
// mouse reporting. Under Bubble Tea v2 that is a field on the rendered View
// rather than an Enable/Disable command, so the grab is asserted by rendering a
// frame — which is also what the terminal actually acts on.
func mouseGrabbed(m *Model) bool {
	return m.View().MouseMode == tea.MouseModeCellMotion
}

// shiftClick is a shift-held left click; shiftWheelUp / wheelUp are wheel
// events. v2 splits mouse events into one message type per kind.
func shiftClick() tea.MouseClickMsg {
	return tea.MouseClickMsg{Button: tea.MouseLeft, Mod: tea.ModShift}
}
func click() tea.MouseClickMsg { return tea.MouseClickMsg{Button: tea.MouseLeft} }
func wheelUp() tea.MouseWheelMsg {
	return tea.MouseWheelMsg{Button: tea.MouseWheelUp}
}
func shiftWheelUp() tea.MouseWheelMsg {
	return tea.MouseWheelMsg{Button: tea.MouseWheelUp, Mod: tea.ModShift}
}

// TestShiftMouseReleasesMouseGrab verifies shift+click in attached interactive
// mode releases the TUI's mouse grab for native terminal text selection (#49):
// the rendered frame stops requesting mouse reporting, repeated shift events
// from the same gesture keep it released, and the next key press restores the
// grab while still forwarding the key to the PTY.
func TestShiftMouseReleasesMouseGrab(t *testing.T) {
	fx := newModelAttached(&fakeController{harnesses: sampleHarnesses()}, protocol.AttachRW)
	m, fa := fx.m, fx.fa

	if !mouseGrabbed(m) {
		t.Fatal("the TUI should hold the mouse grab before any shift gesture")
	}

	// Shift+click releases the grab — the next frame stops asking for mouse
	// reporting, which is what hands selection back to the terminal.
	_, _ = m.onMouse(shiftClick())
	if !m.mouseReleased {
		t.Fatal("shift+click should set mouseReleased=true")
	}
	if mouseGrabbed(m) {
		t.Fatal("shift+click must release the mouse grab")
	}

	// A queued shift event from the same gesture keeps it released rather than
	// flapping the mode back on.
	_, _ = m.onMouse(shiftClick())
	if mouseGrabbed(m) {
		t.Fatal("shift+mouse while already released must keep the grab released")
	}

	// The next key press restores the grab AND still reaches the PTY — the
	// recovery must not eat the keystroke.
	_, cmd := m.onKey(runeKey("a"))
	drain(cmd) // the PTY write is deferred into the returned Cmd
	if m.mouseReleased {
		t.Fatal("key press after shift-mouse should clear mouseReleased")
	}
	if !mouseGrabbed(m) {
		t.Fatal("key press after shift-mouse must restore the mouse grab")
	}
	if len(fa.inputs) != 1 || string(fa.inputs[0]) != "a" {
		t.Fatalf("the re-enabling key must still be forwarded to the PTY, inputs=%v", fa.inputs)
	}
}

// TestShiftMouseReenableCoversScrollback verifies the re-enable is
// router-level (#49): a key pressed while the model sits in the scrollback
// substate — reachable via a queued non-shift wheel event after the release —
// still restores the mouse grab rather than being swallowed by the substate's
// early return.
func TestShiftMouseReenableCoversScrollback(t *testing.T) {
	fx := newModelAttached(&fakeController{harnesses: sampleHarnesses()}, protocol.AttachRW)
	m := fx.m

	_, _ = m.onMouse(shiftClick())
	// A buffered non-shift wheel event arrives while the grab is released and
	// enters scrollback with the flag still set.
	_, _ = m.onMouse(wheelUp())
	if m.att.substate != substateScrollback {
		t.Fatal("wheel-up should enter scrollback")
	}
	// Any scrollback key must still restore the mouse grab.
	_, _ = m.onKey(runeKey("j"))
	if m.mouseReleased {
		t.Fatal("scrollback key press should clear mouseReleased")
	}
	if !mouseGrabbed(m) {
		t.Fatal("scrollback key press must restore the mouse grab")
	}
}

// TestNonShiftMouseDoesNotReleaseGrab verifies regular (non-shift) mouse events
// keep the grab and their original behavior: a plain click emits nothing, and a
// plain wheel-up still enters scrollback.
func TestNonShiftMouseDoesNotReleaseGrab(t *testing.T) {
	fx := newModelAttached(&fakeController{harnesses: sampleHarnesses()}, protocol.AttachRW)
	m := fx.m

	_, _ = m.onMouse(click())
	if m.mouseReleased {
		t.Fatal("non-shift click should not set mouseReleased")
	}
	if !mouseGrabbed(m) {
		t.Fatal("non-shift click must not release the mouse grab")
	}

	_, _ = m.onMouse(wheelUp())
	if m.att.substate != substateScrollback {
		t.Fatal("non-shift wheel-up should still enter scrollback")
	}
}

// TestShiftWheelKeepsScrollback verifies shift+wheel is exempt from the
// passthrough: it must keep entering scrollback (its pre-#49 behavior) rather
// than silently killing the wheel — terminals set the shift bit on wheel
// events too, and releasing the grab there buys nothing (alt screen has no
// native scrollback to hand the wheel to).
func TestShiftWheelKeepsScrollback(t *testing.T) {
	fx := newModelAttached(&fakeController{harnesses: sampleHarnesses()}, protocol.AttachRW)
	m := fx.m

	_, _ = m.onMouse(shiftWheelUp())
	if m.mouseReleased {
		t.Fatal("shift+wheel must not release the mouse grab")
	}
	if !mouseGrabbed(m) {
		t.Fatal("shift+wheel must not release the mouse grab")
	}
	if m.att.substate != substateScrollback {
		t.Fatal("shift+wheel-up should enter scrollback like a plain wheel-up")
	}
}

// TestWheelScrollbackDisarmsPrefix verifies an armed Ctrl-b prefix does not
// survive a wheel-up into scrollback: the first key typed after exiting
// scrollback must reach the PTY, not be intercepted as a prefix chord (which
// would fire detach/start/restart on a plain keystroke).
func TestWheelScrollbackDisarmsPrefix(t *testing.T) {
	fx := newModelAttached(&fakeController{harnesses: sampleHarnesses()}, protocol.AttachRW)
	m, fa := fx.m, fx.fa

	_, _ = m.onKey(ctrlKey('b')) // arm the prefix
	_, _ = m.onMouse(wheelUp())
	if m.att.substate != substateScrollback {
		t.Fatal("wheel-up should enter scrollback")
	}
	if m.att.prefixArmed {
		t.Fatal("entering scrollback must disarm the prefix")
	}
	_, _ = m.onKey(runeKey("q")) // exit scrollback
	_, cmd := m.onKey(runeKey("d"))
	drain(cmd)
	if len(fa.inputs) != 1 || string(fa.inputs[0]) != "d" {
		t.Fatalf("d after scrollback must be forwarded to the PTY, not dispatched as a chord, inputs=%v", fa.inputs)
	}
}

// TestMultiRuneKeyExpansion verifies that coalesced multi-rune key messages are
// expanded and dispatched per rune on the dashboard (issue #145). Holding 'j'
// delivers "jjj" in one read; the selection must advance by 3, not 0.
func TestMultiRuneKeyExpansion(t *testing.T) {
	fc := &fakeController{harnesses: sampleHarnesses()}
	m := New(Options{})
	m.ctrl = fc
	m.harnesses = fc.harnesses
	m.w, m.h = 120, 40
	m.conn = startOK
	m.sel = 0

	// "jjj" in one message → selection advances by 3.
	model, _ := m.onKey(runeKey("jjj"))
	m = model.(*Model)
	if m.sel != 3 {
		t.Errorf("sel after 'jjj' = %d, want 3", m.sel)
	}

	// "kkk" → back 3.
	model, _ = m.onKey(runeKey("kkk"))
	m = model.(*Model)
	if m.sel != 0 {
		t.Errorf("sel after 'kkk' = %d, want 0", m.sel)
	}

	// Mixed: "jjk" → net +1.
	model, _ = m.onKey(runeKey("jjk"))
	m = model.(*Model)
	if m.sel != 1 {
		t.Errorf("sel after 'jjk' = %d, want 1", m.sel)
	}

	// Unbound rune in the middle: "jqj" → q is unbound on dashboard, j+j = +2.
	model, _ = m.onKey(runeKey("jqj"))
	m = model.(*Model)
	if m.sel != 3 {
		t.Errorf("sel after 'jqj' = %d, want 3", m.sel)
	}
}

// TestMultiRuneGuardedActionCollapses verifies that a multi-rune message
// containing a destructive key opens the confirm overlay only once — holding
// 'x' must not queue N stop confirmations (issue #145 AC: guarded actions).
func TestMultiRuneGuardedActionCollapses(t *testing.T) {
	fc := &fakeController{harnesses: sampleHarnesses()}
	m := New(Options{})
	m.ctrl = fc
	m.harnesses = fc.harnesses
	m.w, m.h = 120, 40
	m.conn = startOK
	m.sel = 0

	// "xxx" → confirm overlay opens once, no additional dispatches.
	model, _ := m.onKey(runeKey("xxx"))
	m = model.(*Model)
	if m.overlay != overlayConfirm {
		t.Fatalf("overlay = %v, want overlayConfirm", m.overlay)
	}
}

// TestMultiRuneAttachedPassthrough verifies that in attached interactive
// substate, a multi-rune KeyMsg forwards all runes in a single PTY write —
// typing "hello" into an agent must not become five separate writes.
func TestMultiRuneAttachedPassthrough(t *testing.T) {
	fa := &fakeAttach{}
	m := baseModel(120, 40)
	m.mode = modeAttached
	m.att = newAttachState("crush-signal", protocol.AttachRW, sessionBase, 80, 24)
	m.attach = fa

	model, cmd := m.onKey(runeKey("hello"))
	m = model.(*Model)
	drain(cmd)

	if len(fa.inputs) != 1 {
		t.Fatalf("expected 1 PTY write, got %d: %v", len(fa.inputs), fa.inputs)
	}
	if string(fa.inputs[0]) != "hello" {
		t.Errorf("PTY write = %q, want %q", fa.inputs[0], "hello")
	}
}

// TestMultiRuneSearchOverlayNotDoubleExpanded verifies that multi-rune
// keystrokes sent to the search overlay (or any overlay with a textinput) are
// NOT expanded per-rune. The overlay path routes through onOverlayKey, not
// onDashboardKey, so textinput/huh receive the intact message. Double-expanding
// here would turn "hello" into "hhheeellllllooo" (issue #145 routing check).
func TestMultiRuneSearchOverlayNotDoubleExpanded(t *testing.T) {
	m := baseModel(120, 40)
	m.openSearch()

	model, _ := m.onKey(runeKey("hello"))
	m = model.(*Model)
	if got := m.search.Value(); got != "hello" {
		t.Errorf("search input after multi-rune 'hello' = %q, want %q", got, "hello")
	}
}

// TestBackgroundColorMsgSwitchesTheme verifies the day/night theme follows the
// terminal's reported background (SPEC-0001 REQ "State Presentation"). Lip Gloss
// v2 resolves a light/dark pair eagerly instead of deferring to a renderer, so
// the palette is only correct if the Model rebuilds it when Bubble Tea answers
// the tea.RequestBackgroundColor issued in Init. Over SSH this is the *client's*
// background, which a server-side probe could never see (ADR-0008).
func TestBackgroundColorMsgSwitchesTheme(t *testing.T) {
	m := New(Options{})
	if !m.theme.IsDark() {
		t.Fatal("a fresh Model should default to the night theme")
	}
	nightGlyph := m.theme.RenderGlyph(core.StateRunning)

	// A white background reports light.
	m.Update(tea.BackgroundColorMsg{Color: lipgloss.Color("#ffffff")})
	if m.theme.IsDark() {
		t.Fatal("a light terminal background should select the day theme")
	}
	if dayGlyph := m.theme.RenderGlyph(core.StateRunning); dayGlyph == nightGlyph {
		t.Errorf("day and night themes rendered identically (%q) — the palette did not switch", dayGlyph)
	}

	// And back to dark.
	m.Update(tea.BackgroundColorMsg{Color: lipgloss.Color("#000000")})
	if !m.theme.IsDark() {
		t.Fatal("a dark terminal background should select the night theme")
	}
}

// TestViewDeclaresTerminalFeatures verifies the Model asks for the alt screen
// and mouse reporting on every frame. Bubble Tea v2 removed the WithAltScreen /
// WithMouseCellMotion program options in favour of these View fields, so if the
// View stopped declaring them the cockpit would render inline over the user's
// scrollback and lose wheel scrolling entirely.
func TestViewDeclaresTerminalFeatures(t *testing.T) {
	v := baseModel(80, 24).View()
	if !v.AltScreen {
		t.Error("the cockpit must render in the alternate screen buffer")
	}
	if v.MouseMode != tea.MouseModeCellMotion {
		t.Errorf("MouseMode = %v, want MouseModeCellMotion", v.MouseMode)
	}
}

// TestToggleEnablesAndDisables covers the `t` key both ways. The feature
// shipped with the fakes recording Enable/Disable and nothing asserting them.
func TestToggleEnablesAndDisables(t *testing.T) {
	cases := []struct {
		name        string
		enabled     bool
		wantEnable  []string
		wantDisable []string
	}{
		{name: "disabled harness is enabled", enabled: false, wantEnable: []string{"backup-watch"}},
		{name: "enabled harness is disabled", enabled: true, wantDisable: []string{"backup-watch"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := &fakeController{harnesses: []protocol.HarnessInfo{
				{Name: "backup-watch", State: "stopped", Enabled: tc.enabled},
			}}
			m := New(Options{})
			m.ctrl = fc
			m.harnesses = fc.harnesses

			_, cmd := m.onKey(runeKey("t"))
			drain(cmd)

			fc.mu.Lock()
			defer fc.mu.Unlock()
			if got := strings.Join(fc.enableCalls, ","); got != strings.Join(tc.wantEnable, ",") {
				t.Errorf("enableCalls = %v, want %v", fc.enableCalls, tc.wantEnable)
			}
			if got := strings.Join(fc.disableCalls, ","); got != strings.Join(tc.wantDisable, ",") {
				t.Errorf("disableCalls = %v, want %v", fc.disableCalls, tc.wantDisable)
			}
		})
	}
}

// TestToggleRefusesScheduledHarness: a scheduled one-shot is always
// Enabled=false (config rejects `schedule` with `enabled = true`), so the
// toggle would read it as "disabled" and enable it — starting the run now and
// persisting an intent that Autostart honors at the next daemon boot, landing
// the daemon in exactly the state the config parser refuses to load.
func TestToggleRefusesScheduledHarness(t *testing.T) {
	fc := &fakeController{harnesses: []protocol.HarnessInfo{
		{Name: "sweep", State: "stopped", Enabled: false, Schedule: "0 */6 * * *"},
	}}
	m := New(Options{})
	m.ctrl = fc
	m.harnesses = fc.harnesses

	_, cmd := m.onKey(runeKey("t"))
	drain(cmd)

	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.enableCalls) != 0 || len(fc.disableCalls) != 0 {
		t.Fatalf("scheduled harness was toggled: enable=%v disable=%v", fc.enableCalls, fc.disableCalls)
	}
	if !strings.Contains(m.status, "scheduled") {
		t.Errorf("status = %q, want it to explain the refusal", m.status)
	}
}

// TestRenderRowLabelsDisabledAndScheduled: "(disabled)" must not be hung on a
// cron job that is merely waiting to fire.
func TestRenderRowLabelsDisabledAndScheduled(t *testing.T) {
	m := New(Options{})
	m.w, m.h = 100, 40

	disabled := m.renderRow(protocol.HarnessInfo{Name: "backup-watch", State: "stopped"}, false)
	if !strings.Contains(disabled, "(disabled)") {
		t.Errorf("disabled row = %q, want the disabled label", disabled)
	}
	sched := m.renderRow(protocol.HarnessInfo{Name: "sweep", State: "stopped", Schedule: "0 */6 * * *"}, false)
	if strings.Contains(sched, "(disabled)") {
		t.Errorf("scheduled row = %q, must not read as disabled", sched)
	}
	if !strings.Contains(sched, "(scheduled)") {
		t.Errorf("scheduled row = %q, want the scheduled label", sched)
	}
}

// TestRenderRowIdleForScheduledStopped: the dashboard must phrase a resting
// cron job the same way `harness list` does (#268). Both read schedfmt, so a
// divergence here is a wiring bug, not a styling preference.
func TestRenderRowIdleForScheduledStopped(t *testing.T) {
	m := New(Options{})
	m.w, m.h = 100, 40

	sched := m.renderRow(protocol.HarnessInfo{Name: "sweep", State: "stopped", Schedule: "0 */6 * * *"}, false)
	if !strings.Contains(sched, "idle") {
		t.Errorf("scheduled stopped row = %q, want it to read idle", sched)
	}
	if strings.Contains(sched, "stopped") {
		t.Errorf("scheduled stopped row = %q, must not still say stopped", sched)
	}

	// An unscheduled harness that is stopped really is stopped.
	plain := m.renderRow(protocol.HarnessInfo{Name: "backup-watch", State: "stopped"}, false)
	if !strings.Contains(plain, "stopped") {
		t.Errorf("unscheduled stopped row = %q, want it to still say stopped", plain)
	}
	if strings.Contains(plain, "idle") {
		t.Errorf("unscheduled stopped row = %q, must not be relabelled idle", plain)
	}

	// A scheduled run that failed keeps its own word — it did fail.
	failed := m.renderRow(protocol.HarnessInfo{Name: "sweep", State: "failed", Schedule: "0 */6 * * *"}, false)
	if !strings.Contains(failed, "failed") {
		t.Errorf("scheduled failed row = %q, want it to still say failed", failed)
	}
}
