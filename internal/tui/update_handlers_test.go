package tui

// Governing: SPEC-0001 (dashboard state), ADR-0002 (the client survives a
// daemon restart and reconnects on its own).
//
// Six of the ten Update sub-handlers had no coverage at all: onRefresh,
// onOpResult, onProfileSwitch, onEvent, onDisconnect and onTick. Between them
// they own selection stability, the reconnect lifecycle, and every status the
// operator reads to know what the daemon just did — i.e. most of what makes the
// dashboard trustworthy while things are moving.
//
// These go through Update() with real message values rather than calling the
// handlers directly, so the message-type routing in the switch is covered too:
// a handler wired to the wrong case would still pass a direct-call test.

import (
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// refreshWith builds a refreshMsg carrying the given harnesses, mirroring what
// fetchState produces.
func refreshWith(hs []protocol.HarnessInfo) refreshMsg {
	return refreshMsg{harnesses: hs, profiles: sampleProfiles()}
}

// TestRefreshPinsSelectionByName is the behavior that keeps the dashboard
// usable while the fleet changes underneath it. The daemon returns harnesses in
// config order; if a harness above the cursor disappears (or one is inserted),
// holding the *index* would silently move the selection onto a different
// harness — and the next `x` would stop the wrong one. Selection is therefore
// re-resolved by name on every refresh.
func TestRefreshPinsSelectionByName(t *testing.T) {
	m := baseModel(120, 40)
	// sampleProfiles() marks a profile Active, which filters visible(); show
	// everything so the selection math is about the refresh, not the filter.
	m.showAll = true
	all := sampleHarnesses()
	if len(all) < 3 {
		t.Fatalf("fixture needs >= 3 harnesses, got %d", len(all))
	}

	// Select the last one, then drop the FIRST harness from the next refresh.
	m.sel = len(all) - 1
	want := all[len(all)-1].Name

	m.Update(refreshWith(all[1:]))

	got, ok := m.selectedHarness()
	if !ok {
		t.Fatal("no selection after refresh")
	}
	if got.Name != want {
		t.Errorf("selection moved to %q after a harness above it disappeared; want %q pinned by name",
			got.Name, want)
	}
}

// TestRefreshClampsWhenSelectionDisappears covers the other half: when the
// selected harness is gone entirely there is no name to pin to, so the index
// must be clamped into range rather than left dangling past the end.
func TestRefreshClampsWhenSelectionDisappears(t *testing.T) {
	m := baseModel(120, 40)
	m.showAll = true
	all := sampleHarnesses()
	m.sel = len(all) - 1

	// Refresh with only the first harness — the selected one is gone.
	m.Update(refreshWith(all[:1]))

	if m.sel < 0 || m.sel >= len(m.visible()) {
		t.Errorf("sel = %d is out of range for %d visible harnesses", m.sel, len(m.visible()))
	}
	if _, ok := m.selectedHarness(); !ok {
		t.Error("no harness selected after the previous selection disappeared")
	}
}

// TestRefreshSurfacesError pins that a failed refresh reports itself instead of
// blanking the dashboard. Replacing the harness list with an empty one on a
// transient error would read as "everything is gone".
func TestRefreshSurfacesError(t *testing.T) {
	m := baseModel(120, 40)
	before := len(m.harnesses)

	m.Update(refreshMsg{err: errors.New("boom")})

	if m.status == "" {
		t.Error("a failed refresh set no status; the operator gets no signal")
	}
	if len(m.harnesses) != before {
		t.Errorf("a failed refresh replaced the harness list (%d -> %d); last-good data should survive",
			before, len(m.harnesses))
	}
}

// TestOpResultReportsOutcome pins the status line after a lifecycle op. This is
// the only confirmation the operator gets that a start/stop/restart landed, and
// it must name both the harness and the state it reached.
func TestOpResultReportsOutcome(t *testing.T) {
	m := baseModel(120, 40)
	m.Update(opResultMsg{
		action: ActionStart,
		name:   "reduit-agent",
		info:   protocol.HarnessInfo{Name: "reduit-agent", State: "running"},
	})
	if m.status == "" {
		t.Fatal("no status after a successful op")
	}
	for _, want := range []string{"reduit-agent", "running"} {
		if !containsStr(m.status, want) {
			t.Errorf("status %q does not mention %q", m.status, want)
		}
	}
}

// TestOpResultSurfacesError pins the failure path — a refused stop must say so
// rather than silently doing nothing.
func TestOpResultSurfacesError(t *testing.T) {
	m := baseModel(120, 40)
	m.Update(opResultMsg{action: ActionStop, name: "reduit-agent", err: errors.New("permission denied")})
	if !containsStr(m.status, "permission denied") {
		t.Errorf("status %q does not carry the error", m.status)
	}
}

// TestDisconnectEntersReconnecting pins ADR-0002's promise. On a daemon
// restart the client must drop its now-dead handles and flip into the
// reconnecting state; holding a stale ctrl would make the next keypress
// operate on a closed connection.
func TestDisconnectEntersReconnecting(t *testing.T) {
	m := baseModel(120, 40)
	if m.reconn {
		t.Fatal("fixture already reconnecting")
	}

	m.Update(disconnectMsg{err: errChannelClosed})

	if !m.reconn {
		t.Error("disconnect did not enter the reconnecting state")
	}
	if m.ctrl != nil {
		t.Error("disconnect left a stale controller; the next op would use a dead connection")
	}
	if m.attach != nil {
		t.Error("disconnect left a stale attach handle")
	}
}

// TestDisconnectQuietOnExpectedClose pins the noise filter: a clean channel
// close is the normal shape of a daemon going away and must not paint an error
// on the status line, while an unexpected error must.
func TestDisconnectQuietOnExpectedClose(t *testing.T) {
	m := baseModel(120, 40)
	m.Update(disconnectMsg{err: errChannelClosed})
	if m.status != "" {
		t.Errorf("expected-close disconnect set status %q, want quiet", m.status)
	}

	// isDisconnect() classifies anything mentioning closed/reset/EOF/broken
	// pipe as an ordinary daemon-went-away, so pick an error that is genuinely
	// not a disconnect — that is the case the operator must be told about.
	m2 := baseModel(120, 40)
	m2.Update(disconnectMsg{err: errors.New("permission denied")})
	if m2.status == "" {
		t.Error("a non-disconnect error set no status; it would vanish silently")
	}
}

// TestTickRetriesWhileReconnecting pins the reconnect loop. onTick is the only
// thing that retries the connection, so if it stops issuing a connect command
// while reconn is set, a client that lost its daemon never comes back — it just
// sits on the reconnecting overlay forever.
func TestTickRetriesWhileReconnecting(t *testing.T) {
	m := baseModel(120, 40)
	m.reconn = true

	_, cmd := m.Update(tickMsg{})
	if cmd == nil {
		t.Fatal("tick produced no command while reconnecting")
	}
	// Count the batched commands rather than running them: executing the batch
	// would block on the real 1s tick. A reconnecting tick must queue strictly
	// more than the idle one (the re-armed tick plus the connect retry).
	reconnecting := batchLen(cmd)

	idle := baseModel(120, 40)
	idle.reconn = false
	_, idleCmd := idle.Update(tickMsg{})
	if batchLen(idleCmd) >= reconnecting {
		t.Errorf("reconnecting tick queued %d commands, idle queued %d; the connect retry is missing",
			reconnecting, batchLen(idleCmd))
	}
}

// batchLen reports how many commands a tea.Batch carries, without running them.
// A non-batch command counts as one.
func batchLen(cmd tea.Cmd) int {
	if cmd == nil {
		return 0
	}
	if batch, ok := cmd().(tea.BatchMsg); ok {
		return len(batch)
	}
	return 1
}

// TestTickRearmsItself pins that the periodic tick keeps running when
// connected. The tick also drives the hop animation and the peek refresh, so a
// tick that fails to re-arm quietly freezes both.
func TestTickRearmsItself(t *testing.T) {
	m := baseModel(120, 40)
	m.reconn = false
	_, cmd := m.Update(tickMsg{})
	if cmd == nil {
		t.Fatal("tick did not re-arm itself; periodic refresh stops here")
	}
}

// TestProfileSwitchReportsStartedMembers pins the status after a profile
// switch. The switch is non-destructive — it starts the profile's stopped
// members — and the operator needs to see which ones actually moved.
func TestProfileSwitchReportsStartedMembers(t *testing.T) {
	m := baseModel(120, 40)
	m.Update(profileSwitchMsg{profiles: sampleProfiles(), toStart: []string{"reduit-agent", "mirror"}})
	if m.status == "" {
		t.Fatal("profile switch set no status")
	}
	for _, want := range []string{"reduit-agent", "mirror"} {
		if !containsStr(m.status, want) {
			t.Errorf("status %q does not name started member %q", m.status, want)
		}
	}
}

// TestProfileSwitchSurfacesError pins the failure path.
func TestProfileSwitchSurfacesError(t *testing.T) {
	m := baseModel(120, 40)
	m.Update(profileSwitchMsg{err: errors.New("no such profile")})
	if !containsStr(m.status, "no such profile") {
		t.Errorf("status %q does not carry the error", m.status)
	}
}

// TestReloadSuccessAppliesHarnesses pins the success half of onReloadResult.
// Only the failure half (the banner) was covered; a reload that succeeds must
// actually swap in the new harness list, or the dashboard keeps showing the
// pre-reload config while claiming success.
func TestReloadSuccessAppliesHarnesses(t *testing.T) {
	m := baseModel(120, 40)
	m.showAll = true
	fresh := []protocol.HarnessInfo{{Name: "brand-new", State: "stopped"}}

	m.Update(reloadResultMsg{harnesses: fresh})

	if m.banner != "" {
		t.Errorf("successful reload raised a banner: %q", m.banner)
	}
	if len(m.harnesses) != 1 || m.harnesses[0].Name != "brand-new" {
		t.Errorf("reload did not apply the new harness list: %+v", m.harnesses)
	}
	if m.sel < 0 || m.sel >= len(m.visible()) {
		t.Errorf("sel = %d out of range after the list shrank", m.sel)
	}
}
