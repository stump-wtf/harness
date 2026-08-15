package attach

// Governing: SPEC-0002 REQ "Attach Session"; ADR-0003 (one Mux per harness, its
// resize callback drives the real PTY through the supervisor Manager). These
// tests exercise the Registry→Controller seam with a fake Controller, so the
// "a client resize reaches the PTY" wiring is covered without a real process.

import (
	"sync"
	"syscall"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// recordingController is a fake supervisor.Manager slice: it records the resize,
// input, and signal calls the Registry's muxes make.
type recordingController struct {
	mu      sync.Mutex
	resizes []resizeCall
	inputs  []inputCall
	signals []signalCall
}

type resizeCall struct {
	name       string
	cols, rows int
}
type inputCall struct {
	name string
	data string
}
type signalCall struct {
	name string
	sig  syscall.Signal
}

func (c *recordingController) Resize(name string, cols, rows int) bool {
	c.mu.Lock()
	c.resizes = append(c.resizes, resizeCall{name, cols, rows})
	c.mu.Unlock()
	return true
}

func (c *recordingController) WriteInput(name string, p []byte) bool {
	c.mu.Lock()
	c.inputs = append(c.inputs, inputCall{name, string(p)})
	c.mu.Unlock()
	return true
}

func (c *recordingController) SignalGroup(name string, sig syscall.Signal) bool {
	c.mu.Lock()
	c.signals = append(c.signals, signalCall{name, sig})
	c.mu.Unlock()
	return true
}

func (c *recordingController) resizeCalls() []resizeCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]resizeCall(nil), c.resizes...)
}

func (c *recordingController) inputCalls() []inputCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]inputCall(nil), c.inputs...)
}

func (c *recordingController) signalCalls() []signalCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]signalCall(nil), c.signals...)
}

// TestRegistryResizeReachesController is the end of the resize seam: an attach
// (and a subsequent smaller attach) on a registry-owned mux drives the wired
// Controller.Resize with the harness name and the smallest-attached size — this
// is what SIGWINCHes the real PTY.
func TestRegistryResizeReachesController(t *testing.T) {
	ctrl := &recordingController{}
	r := NewRegistry(100)
	r.SetController(ctrl)

	m := r.Mux("agent")
	m.Attach(1, protocol.AttachRW, 120, 40, noopWrite)
	m.Attach(2, protocol.AttachRW, 90, 30, noopWrite) // smaller → new min

	got := ctrl.resizeCalls()
	want := []resizeCall{{"agent", 120, 40}, {"agent", 90, 30}}
	if len(got) != len(want) {
		t.Fatalf("controller resizes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("resize[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

// TestRegistryReturnsSameMuxPerName: the registry hands back the SAME Mux for a
// name so attach state (screen/scrollback/sessions) survives a supervised
// respawn while the daemon lives (ADR-0007).
func TestRegistryReturnsSameMuxPerName(t *testing.T) {
	r := NewRegistry(100)
	a := r.Mux("agent")
	b := r.Mux("agent")
	if a != b {
		t.Fatal("registry returned different Mux instances for the same name")
	}
	if other := r.Mux("other"); other == a {
		t.Fatal("registry returned the same Mux for different names")
	}
}

// TestRegistryRoutesInputByMode: a read-write session's keystrokes reach the
// PTY via Controller.WriteInput; a read-only session's are dropped (ADR-0008).
func TestRegistryRoutesInputByMode(t *testing.T) {
	ctrl := &recordingController{}
	r := NewRegistry(100)
	r.SetController(ctrl)
	m := r.Mux("agent")

	rw := m.Attach(1, protocol.AttachRW, 80, 24, noopWrite)
	ro := m.Attach(2, protocol.AttachRO, 80, 24, noopWrite)

	m.Input(rw, []byte("hello"))
	m.Input(ro, []byte("dropped"))

	got := ctrl.inputCalls()
	if len(got) != 1 || got[0] != (inputCall{"agent", "hello"}) {
		t.Fatalf("controller inputs = %v, want [{agent hello}]", got)
	}
}

// TestRegistryNoControllerIsSafe: before SetController wires the Manager, a mux
// resize/input is a no-op rather than a nil-deref panic (SetController is called
// during daemon startup, but attach plumbing must tolerate the gap).
func TestRegistryNoControllerIsSafe(t *testing.T) {
	r := NewRegistry(100)
	m := r.Mux("agent") // no SetController
	s := m.Attach(1, protocol.AttachRW, 100, 40, noopWrite)
	m.Resize(s, 80, 24)
	m.Input(s, []byte("x"))
	// Reaching here without a panic is the assertion.
}

// TestRegistryRemoveDropsMux: SPEC-0004 REQ "Tear Down" — deregistering a
// project harness removes its Mux, so an up→down→up cycle gets a fresh
// emulator/scrollback instead of resurfacing the dead incarnation's screen,
// and churning project names cannot grow the registry without bound.
func TestRegistryRemoveDropsMux(t *testing.T) {
	r := NewRegistry(100)
	old := r.Mux("reduit/agent")
	if _, err := old.Write([]byte("stale scrollback\r\n")); err != nil {
		t.Fatal(err)
	}

	r.Remove("reduit/agent")

	if fresh := r.Mux("reduit/agent"); fresh == old {
		t.Fatal("Remove did not drop the Mux; stale scrollback would resurface on re-up")
	}
	// Removing an unknown name is a harmless no-op.
	r.Remove("never-registered")
}

// TestAttachNudgesGuestWithSIGWINCH is the #182 seam test: every attach arms a
// decaying SIGWINCH re-assert schedule against the wired Controller, so a
// resize the guest could not hear (no handler installed yet) is re-delivered.
// The schedule is shrunk here for determinism; the real one spans the guest
// boot window (see winchNudgeDelays).
func TestAttachNudgesGuestWithSIGWINCH(t *testing.T) {
	orig := winchNudgeDelays
	winchNudgeDelays = []time.Duration{5 * time.Millisecond, 5 * time.Millisecond}
	t.Cleanup(func() { winchNudgeDelays = orig })

	ctrl := &recordingController{}
	r := NewRegistry(100)
	r.SetController(ctrl)
	m := r.Mux("h")
	s := m.Attach(1, protocol.AttachRW, 100, 40, discard)
	defer s.Detach()

	deadline := time.Now().Add(2 * time.Second)
	for {
		calls := ctrl.signalCalls()
		if len(calls) >= 2 {
			for _, c := range calls {
				if c.name != "h" || c.sig != syscall.SIGWINCH {
					t.Fatalf("signal call = %+v, want SIGWINCH to h", c)
				}
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("attach delivered %d SIGWINCH nudges in 2s, want ≥2: %+v", len(calls), calls)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
