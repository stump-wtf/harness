package tui

// Governing: ADR-0002 (thin client; two independent connections) and
// client.go's contract that a Client is "not safe for concurrent control
// Calls".
//
// mutexController is the only thing standing between Bubble Tea's concurrent
// Cmds and a Client that cannot take concurrent calls. Every one of its 12
// methods was untested, because the model tests inject a fakeController
// directly and never go through the wrapper — so a method that forgot to take
// the lock, or forwarded to the wrong underlying call, would look fine
// everywhere.
//
// Two properties matter here and neither is checked anywhere else:
//
//  1. Each method forwards to its namesake with the same arguments and returns
//     the same results. A copy-paste slip (Stop calling c.Start) is invisible
//     to the model tests.
//  2. Calls actually serialize. That is the entire reason the type exists.

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// recordingController records the order and identity of calls, and can be made
// to block so overlap is detectable.
type recordingController struct {
	mu     sync.Mutex
	calls  []string
	errOut error

	// inFlight/maxInFlight detect overlap without timing games: if the mutex
	// works, maxInFlight never exceeds 1.
	inFlight    atomic.Int32
	maxInFlight atomic.Int32
	hold        chan struct{}
}

func (r *recordingController) note(name string) {
	n := r.inFlight.Add(1)
	for {
		cur := r.maxInFlight.Load()
		if n <= cur || r.maxInFlight.CompareAndSwap(cur, n) {
			break
		}
	}
	if r.hold != nil {
		<-r.hold
	}
	r.mu.Lock()
	r.calls = append(r.calls, name)
	r.mu.Unlock()
	r.inFlight.Add(-1)
}

func (r *recordingController) List() ([]protocol.HarnessInfo, error) {
	r.note("List")
	return []protocol.HarnessInfo{{Name: "only"}}, r.errOut
}

func (r *recordingController) Describe(name string) (protocol.HarnessInfo, error) {
	r.note("Describe:" + name)
	return protocol.HarnessInfo{Name: name}, r.errOut
}

func (r *recordingController) Start(name string) (protocol.HarnessInfo, error) {
	r.note("Start:" + name)
	return protocol.HarnessInfo{Name: name, State: "running"}, r.errOut
}

func (r *recordingController) Stop(name string) (protocol.HarnessInfo, error) {
	r.note("Stop:" + name)
	return protocol.HarnessInfo{Name: name, State: "stopped"}, r.errOut
}

func (r *recordingController) Restart(name string) (protocol.HarnessInfo, error) {
	r.note("Restart:" + name)
	return protocol.HarnessInfo{Name: name, State: "running"}, r.errOut
}

func (r *recordingController) Logs(name string, lines int) (protocol.LogsData, error) {
	r.note("Logs:" + name)
	return protocol.LogsData{Name: name, Text: "tail"}, r.errOut
}

func (r *recordingController) Profiles() ([]protocol.ProfileInfo, error) {
	r.note("Profiles")
	return []protocol.ProfileInfo{{Name: "p"}}, r.errOut
}

func (r *recordingController) UseProfile(name string) ([]protocol.ProfileInfo, error) {
	r.note("UseProfile:" + name)
	return nil, r.errOut
}

func (r *recordingController) Reload() ([]protocol.HarnessInfo, error) {
	r.note("Reload")
	return nil, r.errOut
}

func (r *recordingController) DaemonInfo() (protocol.DaemonInfo, error) {
	r.note("DaemonInfo")
	return protocol.DaemonInfo{Version: "v"}, r.errOut
}

func (r *recordingController) DaemonVersion() string {
	r.note("DaemonVersion")
	return "v-test"
}

func (r *recordingController) Close() error {
	r.note("Close")
	return r.errOut
}

func (r *recordingController) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

// TestMutexControllerForwardsEveryMethod drives all 12 methods and asserts each
// reached its namesake exactly once, in order. This is the copy-paste guard.
func TestMutexControllerForwardsEveryMethod(t *testing.T) {
	rec := &recordingController{}
	mc := newMutexController(rec)

	if _, err := mc.List(); err != nil {
		t.Fatal(err)
	}
	if _, err := mc.Describe("h"); err != nil {
		t.Fatal(err)
	}
	if _, err := mc.Start("h"); err != nil {
		t.Fatal(err)
	}
	if _, err := mc.Stop("h"); err != nil {
		t.Fatal(err)
	}
	if _, err := mc.Restart("h"); err != nil {
		t.Fatal(err)
	}
	if _, err := mc.Logs("h", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := mc.Profiles(); err != nil {
		t.Fatal(err)
	}
	if _, err := mc.UseProfile("p"); err != nil {
		t.Fatal(err)
	}
	if _, err := mc.Reload(); err != nil {
		t.Fatal(err)
	}
	if _, err := mc.DaemonInfo(); err != nil {
		t.Fatal(err)
	}
	_ = mc.DaemonVersion()
	if err := mc.Close(); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"List", "Describe:h", "Start:h", "Stop:h", "Restart:h", "Logs:h",
		"Profiles", "UseProfile:p", "Reload", "DaemonInfo", "DaemonVersion", "Close",
	}
	got := rec.seen()
	if len(got) != len(want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("call %d = %q, want %q — a method is forwarding to the wrong underlying call", i, got[i], want[i])
		}
	}
}

// TestMutexControllerReturnsUnderlyingValues pins that results are passed back
// rather than zeroed. A wrapper that swallowed the value would make every
// lifecycle op look like it returned an empty harness.
func TestMutexControllerReturnsUnderlyingValues(t *testing.T) {
	mc := newMutexController(&recordingController{})

	if hs, _ := mc.List(); len(hs) != 1 || hs[0].Name != "only" {
		t.Errorf("List() = %+v, want the underlying single harness", hs)
	}
	if h, _ := mc.Start("agent"); h.Name != "agent" || h.State != "running" {
		t.Errorf("Start() = %+v, want the underlying result", h)
	}
	if ld, _ := mc.Logs("agent", 5); ld.Text != "tail" {
		t.Errorf("Logs() = %+v, want the underlying text", ld)
	}
	if v := mc.DaemonVersion(); v != "v-test" {
		t.Errorf("DaemonVersion() = %q, want the underlying value", v)
	}
}

// TestMutexControllerPropagatesErrors pins that errors are not swallowed — a
// silently-dropped error would show the operator a success status for a failed
// op.
func TestMutexControllerPropagatesErrors(t *testing.T) {
	boom := errors.New("boom")
	mc := newMutexController(&recordingController{errOut: boom})

	if _, err := mc.List(); !errors.Is(err, boom) {
		t.Errorf("List() err = %v, want %v", err, boom)
	}
	if _, err := mc.Stop("h"); !errors.Is(err, boom) {
		t.Errorf("Stop() err = %v, want %v", err, boom)
	}
	if err := mc.Close(); !errors.Is(err, boom) {
		t.Errorf("Close() err = %v, want %v", err, boom)
	}
}

// TestMutexControllerSerializes is the property the type exists for. Many
// goroutines call concurrently while the underlying controller is held open;
// if the lock were missing, more than one would be inside at once.
//
// Detected by counting concurrent entries rather than by timing, so the test is
// deterministic rather than flaky under load.
func TestMutexControllerSerializes(t *testing.T) {
	rec := &recordingController{hold: make(chan struct{})}
	mc := newMutexController(rec)

	const n = 8
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, _ = mc.List()
		}()
	}

	// Let them proceed one at a time; the mutex is what makes that possible.
	go func() {
		for i := 0; i < n; i++ {
			rec.hold <- struct{}{}
		}
	}()
	wg.Wait()

	if got := rec.maxInFlight.Load(); got > 1 {
		t.Errorf("%d concurrent calls reached the underlying controller at once; mutexController must serialize them", got)
	}
	if got := len(rec.seen()); got != n {
		t.Errorf("underlying controller saw %d calls, want %d", got, n)
	}
}
