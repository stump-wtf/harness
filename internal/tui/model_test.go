package tui

import (
	"os"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// --- fakes ---------------------------------------------------------------

// fakeController is an in-memory Controller recording mutating calls.
type fakeController struct {
	mu         sync.Mutex
	harnesses  []protocol.HarnessInfo
	profiles   []protocol.ProfileInfo
	stopCalls  []string
	startCalls []string
	rstCalls   []string
	useProfile string
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
func (f *fakeController) Logs(n string, lines int) (protocol.LogsData, error) {
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
}

func (f *fakeAttach) AttachOpen(sid uint32, name string, cols, rows int, mode protocol.AttachMode) error {
	f.opens = append(f.opens, name)
	return nil
}
func (f *fakeAttach) AttachInput(sid uint32, data []byte) error {
	f.inputs = append(f.inputs, append([]byte(nil), data...))
	return nil
}
func (f *fakeAttach) AttachResize(sid uint32, cols, rows int) error { return nil }
func (f *fakeAttach) AttachClose(sid uint32) error {
	f.closes = append(f.closes, sid)
	return nil
}
func (f *fakeAttach) Conn() *protocol.Conn { return nil }
func (f *fakeAttach) Close() error         { return nil }

// --- key helpers ---------------------------------------------------------

func runeKey(s string) tea.KeyMsg         { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }
func specialKey(t tea.KeyType) tea.KeyMsg { return tea.KeyMsg{Type: t} }

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

// TestDetachReturnsHome is the SPEC-0001 scenario "Detach returns home": Esc-Esc
// from attached mode returns to the Dashboard and never signals a stop (the
// harness keeps running).
func TestDetachReturnsHome(t *testing.T) {
	fc := &fakeController{harnesses: sampleHarnesses()}
	fa := &fakeAttach{}
	m := New(Options{})
	m.ctrl, m.attach = fc, fa
	m.harnesses = fc.harnesses
	m.mode = modeAttached
	m.att = newAttachState("crush-signal", protocol.AttachRW, 1, 80, 24)

	// First Esc arms; second Esc detaches.
	m.onKey(specialKey(tea.KeyEscape))
	if m.mode != modeAttached {
		t.Fatal("single Esc should not detach")
	}
	_, cmd := m.onKey(specialKey(tea.KeyEscape))
	drain(cmd)
	if m.mode != modeDashboard || m.att != nil {
		t.Fatal("Esc-Esc should return to the dashboard")
	}
	if len(fc.stopCalls) != 0 {
		t.Fatalf("detach must not stop the harness, stops=%v", fc.stopCalls)
	}
	if len(fa.closes) != 1 {
		t.Fatalf("detach should close the attach session, closes=%v", fa.closes)
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

	_, cmd := m.onKey(runeKey("]"))
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
