package daemon

// Draining Over The Wire
//
// What an operator sees when they take a harness out of service, asserted on
// the bytes the daemon writes to its socket: `drain` answers the fresh state of
// every named harness, describe and list carry the draining/drained label next
// to the "drain" hold reason (so a client from before the verb still reads the
// gate), and `undrain` leaves no label behind. The two failure paths a
// packaging script branches on are here too: a typo is refused whole, and
// `--wait --timeout D` whose deadline passed answers drain_timeout while the
// gate stays armed, which is what makes a non-zero exit mean "do not restart
// yet".
//
// Governing: stump.wtf/harness#828; ADR-0062 (proposed); SPEC-0002 REQ "Control
// Operations".
//
// @joestump-agent 11/06/2026 - Added for stump.wtf/harness#828.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/client"
	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/protocol"
)

const drainTOML = `
[harness.busy]
harness = "generic"
args = ["-c", "sleep 0.2"]
`

// controlAnswer sends one control request and returns either the reply's data
// or the structured error the daemon answered with, so a test can assert on a
// failure instead of failing on it.
func controlAnswer(t *testing.T, c *client.Client, req protocol.ControlReq) (json.RawMessage, *protocol.ErrorMsg) {
	t.Helper()
	pc := c.Conn()
	req.ID = 9100
	if err := pc.WriteJSON(protocol.TypeControlReq, &req); err != nil {
		t.Fatalf("write %s: %v", req.Op, err)
	}
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		f, err := pc.ReadFrame()
		if err != nil {
			t.Fatalf("read %s reply: %v", req.Op, err)
		}
		switch f.Type {
		case protocol.TypeControlResp:
			var resp protocol.ControlResp
			if err := json.Unmarshal(f.Payload, &resp); err != nil {
				t.Fatalf("decode %s reply: %v", req.Op, err)
			}
			if resp.ID == req.ID {
				return resp.Data, nil
			}
		case protocol.TypeError:
			if em := errorFrom(t, f.Payload); em.ID == req.ID {
				return nil, em
			}
		case protocol.TypePing:
			_ = pc.WriteFrame(protocol.TypePong, nil)
		}
	}
}

// TestDrainAndUndrainOnTheWire covers the gate's whole surface on a harness
// with nothing running: the drain reply, describe's label and hold reason, the
// label's absence after undrain, and the two refusals.
func TestDrainAndUndrainOnTheWire(t *testing.T) {
	td := newTestDaemon(t, drainTOML)
	c := td.dial(t, nil)

	data, em := controlAnswer(t, c, protocol.ControlReq{Op: protocol.OpDrain, Names: []string{"busy"}})
	if em != nil {
		t.Fatalf("drain: error frame %s", em.Code)
	}
	var infos []protocol.HarnessInfo
	if err := json.Unmarshal(data, &infos); err != nil {
		t.Fatalf("decode drain reply: %v (%s)", err, data)
	}
	if len(infos) != 1 {
		t.Fatalf("drain reply = %d harnesses, want 1", len(infos))
	}
	if infos[0].Drain != "drained" {
		t.Errorf("drain reply label = %q, want drained", infos[0].Drain)
	}
	if len(infos[0].HoldReasons) != 1 || infos[0].HoldReasons[0] != core.HoldDrain.String() {
		t.Errorf("drain reply hold_reasons = %v, want [drain]", infos[0].HoldReasons)
	}

	described := fieldsOf(t, rawControl(t, c, protocol.ControlReq{Op: protocol.OpDescribe, Name: "busy"}))
	if got := string(described["drain"]); got != `"drained"` {
		t.Errorf(`describe drain = %s, want "drained"`, got)
	}
	if got := string(described["hold_reasons"]); got != `["drain"]` {
		t.Errorf(`describe hold_reasons = %s, want ["drain"]`, got)
	}

	// --wait with no name waits on whatever is already drained: nothing is up,
	// so the answer is a plain success and the gate is untouched.
	if _, em := controlAnswer(t, c, protocol.ControlReq{Op: protocol.OpDrain, Wait: true}); em != nil {
		t.Errorf("drain --wait with nothing running: error frame %s, want success", em.Code)
	}

	if _, em := controlAnswer(t, c, protocol.ControlReq{Op: protocol.OpUndrain, Names: []string{"busy"}}); em != nil {
		t.Fatalf("undrain: error frame %s", em.Code)
	}
	described = fieldsOf(t, rawControl(t, c, protocol.ControlReq{Op: protocol.OpDescribe, Name: "busy"}))
	if v, ok := described["drain"]; ok {
		t.Errorf("undrain left a drain label on the wire: %s", v)
	}
	if v, ok := described["hold_reasons"]; ok {
		t.Errorf("undrain left hold_reasons on the wire: %s", v)
	}

	// A name the daemon does not know is refused whole, and a drain that named
	// nothing and asked for no wait is a mistake, not a no-op.
	if _, em := controlAnswer(t, c, protocol.ControlReq{Op: protocol.OpDrain, Names: []string{"ghost"}}); em == nil || em.Code != protocol.ErrUnknownHarness {
		t.Errorf("drain of an unknown harness = %v, want unknown_harness", em)
	}
	if _, em := controlAnswer(t, c, protocol.ControlReq{Op: protocol.OpDrain}); em == nil || em.Code != protocol.ErrBadRequest {
		t.Errorf("drain with no name and no --wait = %v, want bad_request", em)
	}
	if _, em := controlAnswer(t, c, protocol.ControlReq{Op: protocol.OpUndrain}); em == nil || em.Code != protocol.ErrBadRequest {
		t.Errorf("undrain with no name = %v, want bad_request", em)
	}
}

// TestDrainWaitReportsTimeoutOnTheWire: the wait does not clear a run, and a
// deadline that passes is a structured error the script can gate on. The gate
// stays armed either way.
func TestDrainWaitReportsTimeoutOnTheWire(t *testing.T) {
	td := newTestDaemon(t, sleeperTOML)
	td.mgr.Start("sleeper")
	waitFor(t, "sleeper running", func() bool {
		snap, _ := td.mgr.Snapshot("sleeper")
		return snap.State == core.StateRunning
	})
	c := td.dial(t, nil)

	if _, em := controlAnswer(t, c, protocol.ControlReq{
		Op: protocol.OpDrain, Names: []string{"sleeper"}, Wait: true, Timeout: "100ms",
	}); em == nil || em.Code != protocol.ErrDrainTimeout {
		t.Errorf("drain --wait --timeout past its deadline = %v, want drain_timeout", em)
	}
	if snap, _ := td.mgr.Snapshot("sleeper"); !snap.Holds.Has(core.HoldDrain) {
		t.Error("the timed-out wait left the gate disarmed")
	}

	// A malformed duration is answered before anything is armed, so the
	// operator can fix the flag rather than the harness.
	if _, em := controlAnswer(t, c, protocol.ControlReq{
		Op: protocol.OpDrain, Names: []string{"sleeper"}, Wait: true, Timeout: "soon",
	}); em == nil || em.Code != protocol.ErrBadRequest {
		t.Errorf("drain --wait --timeout soon = %v, want bad_request", em)
	}

	td.mgr.Undrain([]string{"sleeper"})
	td.mgr.Stop("sleeper")
}
