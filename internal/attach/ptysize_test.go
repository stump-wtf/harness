package attach_test

// Governing: ADR-0003 (the native backend owns PTY sizing; the attach layer's
// smallest-attached-wins viewport is authoritative) and SPEC-0002 REQ "Attach
// Session". These are end-to-end sizing tests across the real seam — a real
// child process under a real PTY, driven through Registry → Manager →
// Supervisor — because that seam is exactly where the "the harness renders into
// an 80×24 box in the corner of a full-size window" bug lived: every layer's
// own unit tests passed while the child never learned the window size.
//
// The child reports its view of the world with `stty size` (rows cols), which
// reads TIOCGWINSZ from the PTY the daemon allocated — the same value the app
// inside a harness lays itself out against.

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/attach"
	"gitea.stump.rocks/stump.wtf/harness/internal/core"
	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
	"gitea.stump.rocks/stump.wtf/harness/internal/supervisor"
)

// sizeProbeScript polls its PTY dimensions, so a reading reflects the size the
// PTY actually has (TIOCGWINSZ) rather than whether a signal arrived.
const sizeProbeScript = `while true; do stty size; sleep 0.2; done`

// winchProbeScript reports ONLY on SIGWINCH. A polling probe cannot tell a PTY
// that was resized from one that was resized *and told the app about it* — and
// an app that is never told never repaints, which looks exactly like the PTY
// never having been resized at all.
const winchProbeScript = `trap 'echo WINCH-$(stty size | tr " " x)' WINCH; while true; do sleep 0.1; done`

// sizeRig is a wired daemon core: attach Registry ↔ supervisor Manager, with one
// probe harness.
type sizeRig struct {
	t    *testing.T
	reg  *attach.Registry
	mgr  *supervisor.Manager
	mux  *attach.Mux
	mu   sync.Mutex
	seen strings.Builder
}

func newSizeRig(t *testing.T) *sizeRig { return newRig(t, sizeProbeScript) }

func newRig(t *testing.T, script string) *sizeRig {
	t.Helper()
	tmp := t.TempDir()
	cfg := &core.Config{
		Harnesses: map[string]core.Harness{"probe": {
			Name:    "probe",
			Cmd:     "sh",
			Args:    []string{"-c", script},
			Workdir: tmp,
			Backend: core.BackendNative,
		}},
		HarnessOrder: []string{"probe"},
	}
	r := &sizeRig{t: t}
	r.reg = attach.NewRegistry(200)
	r.mgr = supervisor.NewManager(cfg, supervisor.ManagerOptions{
		StatePath:    filepath.Join(tmp, "state.json"),
		LogDir:       filepath.Join(tmp, "logs"),
		ExtraOutFor:  r.reg.WriterFor,
		DropExtraOut: r.reg.Remove,
		SizeFor:      r.reg.SizeFor,
	})
	r.reg.SetController(r.mgr)
	r.mux = r.reg.Mux("probe")
	t.Cleanup(r.mgr.Close)
	return r
}

// attachAt opens a session at cols×rows whose output accumulates in the rig.
func (r *sizeRig) attachAt(id uint32, cols, rows int) *attach.Session {
	return r.mux.Attach(id, protocol.AttachRW, cols, rows, func(p []byte) error {
		r.mu.Lock()
		r.seen.Write(p)
		r.mu.Unlock()
		return nil
	})
}

// forget drops everything the session has reported so far, so the next
// wantSize can only be satisfied by a fresh report.
func (r *sizeRig) forget() {
	r.mu.Lock()
	r.seen.Reset()
	r.mu.Unlock()
}

// drain waits out the replay a fresh attach queues ahead of the live stream
// (screen snapshot + scrollback tail, SPEC-0002 REQ "Attach Session") and then
// forgets it. Without this a size assertion could be satisfied by history — the
// ring still holds the *previous* process's output, which reported the size we
// are trying to prove the *new* process has.
func (r *sizeRig) drain() {
	time.Sleep(500 * time.Millisecond)
	r.forget()
}

// wantSize waits for the child to report rows×cols on its PTY.
func (r *sizeRig) wantSize(cols, rows int, what string) {
	r.t.Helper()
	want := strings.NewReplacer().Replace(itoa(rows) + " " + itoa(cols))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		got := r.seen.String()
		r.mu.Unlock()
		if strings.Contains(got, want+"\r\n") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	r.mu.Lock()
	got := r.seen.String()
	r.mu.Unlock()
	r.t.Fatalf("%s: child never reported a %dx%d PTY (want %q); it reported: %q",
		what, cols, rows, want, lastLines(got, 6))
}

// TestAttachSizesChildPTY is the baseline: attaching a client at its window size
// resizes the running child's PTY to match, so the app inside fills the window.
func TestAttachSizesChildPTY(t *testing.T) {
	r := newSizeRig(t)
	r.mgr.Start("probe")

	sess := r.attachAt(1, 200, 50)
	defer sess.Detach()
	r.wantSize(200, 50, "attach")
}

// TestRestartWhileAttachedKeepsViewport guards the regression: a harness
// (re)started while a client is attached used to be spawned at the 80×24
// default and never corrected — the mux's recorded size already equalled the
// client viewport, so its smallest-attached-wins policy saw no change and never
// pushed a new size at the fresh PTY. The app inside then rendered into an 80×24
// box in the corner of a full-size window until the user resized the terminal.
func TestRestartWhileAttachedKeepsViewport(t *testing.T) {
	r := newSizeRig(t)
	r.mgr.Start("probe")

	sess := r.attachAt(1, 200, 50)
	defer sess.Detach()
	r.wantSize(200, 50, "initial attach")

	r.forget()
	r.mgr.Restart("probe")
	r.wantSize(200, 50, "after restart while attached")
}

// TestReattachAfterRespawnResizesChild covers the same hazard from the other
// side: the harness respawns while nobody is attached, then the same-size client
// comes back. The Mux outlives the process and still remembers the old viewport,
// so the reattach must push the size at the new PTY rather than short-circuit on
// "nothing changed".
func TestReattachAfterRespawnResizesChild(t *testing.T) {
	r := newSizeRig(t)
	r.mgr.Start("probe")

	sess := r.attachAt(1, 200, 50)
	r.wantSize(200, 50, "initial attach")
	sess.Detach()

	// Respawn with nobody watching, then come back at the very same size.
	r.mgr.Restart("probe")
	sess2 := r.attachAt(2, 200, 50)
	defer sess2.Detach()
	r.drain() // the replayed history reports the *old* process's size
	r.wantSize(200, 50, "reattach after respawn")
}

// TestResizeFollowsClientWindow verifies the live path: growing and shrinking
// the client's window moves the child's PTY with it.
func TestResizeFollowsClientWindow(t *testing.T) {
	r := newSizeRig(t)
	r.mgr.Start("probe")

	sess := r.attachAt(1, 120, 30)
	defer sess.Detach()
	r.wantSize(120, 30, "attach")

	r.forget()
	sess.Resize(200, 50)
	r.wantSize(200, 50, "grow")

	r.forget()
	sess.Resize(90, 20)
	r.wantSize(90, 20, "shrink")
}

// TestResizeReachesAppAsSIGWINCH: resizing the client's window must not only
// change the PTY's dimensions but signal the app inside, since a full-screen TUI
// (crush, vim, an agent CLI) only re-lays-out when it is told. Without the
// signal the app keeps painting its old geometry into the corner of a
// correctly-sized window.
func TestResizeReachesAppAsSIGWINCH(t *testing.T) {
	r := newRig(t, winchProbeScript)
	r.mgr.Start("probe")

	sess := r.attachAt(1, 120, 30)
	defer sess.Detach()
	time.Sleep(400 * time.Millisecond) // let the child install its trap

	sess.Resize(200, 50) // the user drags the window wider
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		got := r.seen.String()
		r.mu.Unlock()
		if strings.Contains(got, "WINCH-50x200") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	r.mu.Lock()
	got := r.seen.String()
	r.mu.Unlock()
	t.Fatalf("app was never signalled about the new size; it reported: %q", lastLines(got, 4))
}

// itoa is a dependency-free strconv.Itoa for positive ints.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// lastLines returns the trailing n newline-separated chunks of s, for failure
// messages (the leading screen snapshot is noise).
func lastLines(s string, n int) string {
	parts := strings.Split(strings.TrimRight(s, "\r\n"), "\n")
	if len(parts) > n {
		parts = parts[len(parts)-n:]
	}
	return strings.Join(parts, "\n")
}
