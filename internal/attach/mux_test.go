package attach

// Governing: SPEC-0002 REQ "Attach Session" (snapshot before live bytes;
// read-only drops input; smallest-attached-wins resize; close tears down only
// that session) and REQ "Backpressure Isolation" (a slow client never stalls
// Write and eventually gets a snapshot repaint instead of the backlog); ADR-0003
// (resize policy); ADR-0008 (read-only attach). Tests exercise the scenarios
// explicitly and run under -race.

import (
	"bytes"
	"fmt"
	"sync"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// snapshotPrefix is what renderScreen emits at the start of every repaint.
var snapshotPrefix = []byte("\x1b[0m\x1b[2J\x1b[H")

// collect drains a session's frames into a slice, guarded for -race.
type collector struct {
	mu     sync.Mutex
	frames [][]byte
}

func (c *collector) write(b []byte) error {
	cp := append([]byte(nil), b...)
	c.mu.Lock()
	c.frames = append(c.frames, cp)
	c.mu.Unlock()
	return nil
}

func (c *collector) all() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([][]byte, len(c.frames))
	copy(out, c.frames)
	return out
}

// waitForFrameContaining polls the collector until some frame contains sub, or
// fails after a timeout.
func waitForFrameContaining(t *testing.T, c *collector, sub []byte) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, f := range c.all() {
			if bytes.Contains(f, sub) {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no frame containing %q within timeout", sub)
}

// TestAttachSnapshotBeforeLive is SPEC-0002 REQ "Attach Session" scenario
// "Instant repaint on attach": a client receives a full screen snapshot before
// any live bytes.
func TestAttachSnapshotBeforeLive(t *testing.T) {
	m := newMux("h", 100, nil, nil)
	m.Write([]byte("hello\r\n")) // on screen before the attach

	c := &collector{}
	m.Attach(1, protocol.AttachRW, 80, 24, c.write)

	// The very first frame must be the screen snapshot, and it must already
	// contain the pre-attach content.
	deadline := time.Now().Add(2 * time.Second)
	for len(c.all()) == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	frames := c.all()
	if len(frames) == 0 {
		t.Fatal("no snapshot frame delivered")
	}
	if !bytes.HasPrefix(frames[0], snapshotPrefix) {
		t.Errorf("first frame is not a snapshot repaint: %q", frames[0])
	}
	if !bytes.Contains(frames[0], []byte("hello")) {
		t.Errorf("snapshot missing pre-attach content: %q", frames[0])
	}

	// Now live output must arrive after the snapshot.
	m.Write([]byte("world\r\n"))
	waitForFrameContaining(t, c, []byte("world"))
}

// TestReadOnlyDropsInput is SPEC-0002 REQ "Attach Session" scenario "Read-only
// attach": a ro session's keystrokes never reach the PTY, while a rw session's
// do (ADR-0008).
func TestReadOnlyDropsInput(t *testing.T) {
	var mu sync.Mutex
	var got [][]byte
	onInput := func(p []byte) {
		mu.Lock()
		got = append(got, append([]byte(nil), p...))
		mu.Unlock()
	}
	m := newMux("h", 100, nil, onInput)

	ro := m.Attach(1, protocol.AttachRO, 80, 24, func([]byte) error { return nil })
	ro.Input([]byte("secret-keys"))
	mu.Lock()
	if len(got) != 0 {
		mu.Unlock()
		t.Fatalf("read-only input reached the PTY: %q", got)
	}
	mu.Unlock()

	rw := m.Attach(2, protocol.AttachRW, 80, 24, func([]byte) error { return nil })
	rw.Input([]byte("real-keys"))
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || string(got[0]) != "real-keys" {
		t.Fatalf("read-write input = %q, want [real-keys]", got)
	}
}

// TestResizeSmallestWins is SPEC-0002 REQ "Attach Session" / ADR-0003: with
// several clients attached at different sizes, the smallest viewport wins, and
// detaching recomputes from the remaining clients.
func TestResizeSmallestWins(t *testing.T) {
	var mu sync.Mutex
	var sizes [][2]int
	onResize := func(cols, rows int) {
		mu.Lock()
		sizes = append(sizes, [2]int{cols, rows})
		mu.Unlock()
	}
	m := newMux("h", 100, onResize, nil)

	big := m.Attach(1, protocol.AttachRW, 120, 50, func([]byte) error { return nil })
	if c, r := m.Size(); c != 120 || r != 50 {
		t.Fatalf("one client: size = %dx%d, want 120x50", c, r)
	}
	small := m.Attach(2, protocol.AttachRW, 80, 24, func([]byte) error { return nil })
	if c, r := m.Size(); c != 80 || r != 24 {
		t.Fatalf("two clients: size = %dx%d, want 80x24 (smallest wins)", c, r)
	}
	_ = big

	// Resizing the smallest larger lets the other constrain each dimension.
	m.Resize(small, 200, 30)
	if c, r := m.Size(); c != 120 || r != 30 {
		t.Fatalf("after resize: size = %dx%d, want 120x30 (per-dimension min)", c, r)
	}

	// Detaching a client recomputes from the rest.
	m.Detach(small)
	if c, r := m.Size(); c != 120 || r != 50 {
		t.Fatalf("after detach: size = %dx%d, want 120x50", c, r)
	}
}

// TestCloseTearsDownOnlyThatSession: ATTACH_CLOSE removes just its session; the
// other sessions keep receiving live output (SPEC-0002 REQ "Attach Session").
func TestCloseTearsDownOnlyThatSession(t *testing.T) {
	m := newMux("h", 100, nil, nil)
	a := &collector{}
	b := &collector{}
	sa := m.Attach(1, protocol.AttachRW, 80, 24, a.write)
	m.Attach(2, protocol.AttachRW, 80, 24, b.write)
	if m.SessionCount() != 2 {
		t.Fatalf("SessionCount = %d, want 2", m.SessionCount())
	}
	m.Detach(sa)
	if m.SessionCount() != 1 {
		t.Fatalf("after detach SessionCount = %d, want 1", m.SessionCount())
	}
	m.Write([]byte("after-close\r\n"))
	waitForFrameContaining(t, b, []byte("after-close"))
}

// TestBackpressureCoalesce is SPEC-0002 REQ "Backpressure Isolation" scenario
// "Slow SSH client": a stalled client never blocks Write (the harness) or other
// clients, and eventually receives a snapshot repaint instead of the full
// backlog. Runs under -race.
func TestBackpressureCoalesce(t *testing.T) {
	m := newMux("h", 100, nil, nil)

	// Slow client: every write blocks until released, modelling a stalled
	// socket.
	release := make(chan struct{})
	slow := &collector{}
	slowWrite := func(b []byte) error {
		<-release
		return slow.write(b)
	}
	// Fast client: represents "all other clients continue at full speed".
	fast := &collector{}

	m.Attach(1, protocol.AttachRW, 80, 24, slowWrite)
	m.Attach(2, protocol.AttachRW, 80, 24, fast.write)

	const n = 2000
	done := make(chan struct{})
	go func() {
		for i := 0; i < n; i++ {
			m.Write([]byte(fmt.Sprintf("line-%04d\n", i)))
		}
		close(done)
	}()

	// The PTY reader (Write) must never block on the stalled client.
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		close(release) // avoid leaking the blocked pump
		t.Fatal("Write blocked on a slow client (backpressure isolation failed)")
	}

	// The fast client keeps up and sees the final line.
	waitForFrameContaining(t, fast, []byte("line-1999"))

	// Release the slow client and let its pump drain.
	close(release)
	time.Sleep(200 * time.Millisecond)

	frames := slow.all()
	if len(frames) >= n {
		t.Fatalf("slow client received %d frames; expected the backlog to be coalesced (<%d)", len(frames), n)
	}
	// It must have received at least one snapshot repaint (the coalesced state).
	sawSnapshot := false
	for _, f := range frames {
		if bytes.HasPrefix(f, snapshotPrefix) {
			sawSnapshot = true
			break
		}
	}
	if !sawSnapshot {
		t.Fatalf("slow client never received a snapshot repaint; got %d frames", len(frames))
	}
}

// resizeRecorder captures the onResize callback the Mux fires to drive the real
// PTY (smallest-attached-wins, ADR-0003). It's the seam a real terminal sees.
type resizeRecorder struct {
	mu    sync.Mutex
	calls [][2]int
}

func (r *resizeRecorder) onResize(cols, rows int) {
	r.mu.Lock()
	r.calls = append(r.calls, [2]int{cols, rows})
	r.mu.Unlock()
}

func (r *resizeRecorder) snapshot() [][2]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][2]int, len(r.calls))
	copy(out, r.calls)
	return out
}

func noopWrite([]byte) error { return nil }

// TestResizeDrivesPTYOnlyOnChange verifies the resize seam actually reaching the
// PTY: onResize fires exactly when the authoritative (smallest-attached) size
// changes, and stays silent otherwise — so a real terminal isn't churned with
// redundant SIGWINCHes and the PTY tracks the smallest viewport (ADR-0003).
func TestResizeDrivesPTYOnlyOnChange(t *testing.T) {
	rec := &resizeRecorder{}
	m := newMux("h", 100, rec.onResize, nil) // starts at the 80x24 default

	// Attaching at exactly the current size changes nothing → no onResize.
	a := m.Attach(1, protocol.AttachRW, 80, 24, noopWrite)
	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("attach at default size fired onResize %v, want none", got)
	}

	// Growing the only client resizes the PTY.
	m.Resize(a, 100, 40)
	if got := rec.snapshot(); len(got) != 1 || got[0] != [2]int{100, 40} {
		t.Fatalf("after grow: onResize=%v, want [[100 40]]", got)
	}

	// A bigger second client doesn't shrink the min → no onResize.
	m.Attach(2, protocol.AttachRW, 120, 50, noopWrite)
	if got := rec.snapshot(); len(got) != 1 {
		t.Fatalf("bigger client fired onResize %v, want unchanged", got)
	}

	// Re-applying the same size is a no-op.
	m.Resize(a, 100, 40)
	if got := rec.snapshot(); len(got) != 1 {
		t.Fatalf("no-op resize fired onResize %v, want unchanged", got)
	}

	// A smaller third client wins each dimension → onResize.
	c := m.Attach(3, protocol.AttachRW, 90, 30, noopWrite)
	if got := rec.snapshot(); len(got) != 2 || got[1] != [2]int{90, 30} {
		t.Fatalf("smaller client: onResize=%v, want last [90 30]", got)
	}

	// Detaching the smallest grows the PTY back to the next-smallest.
	m.Detach(c)
	if got := rec.snapshot(); len(got) != 3 || got[2] != [2]int{100, 40} {
		t.Fatalf("after detach: onResize=%v, want last [100 40]", got)
	}
}

// TestResizeRetainsSizeWithNoSessions: when the last client detaches the mux
// keeps the last authoritative size and does NOT fire a shrink-to-zero onResize
// — the harness process keeps its window until someone attaches again
// (applyResizeLocked: "with no sessions the last size is retained").
func TestResizeRetainsSizeWithNoSessions(t *testing.T) {
	rec := &resizeRecorder{}
	m := newMux("h", 100, rec.onResize, nil)

	a := m.Attach(1, protocol.AttachRW, 90, 30, noopWrite)
	if got := rec.snapshot(); len(got) != 1 || got[0] != [2]int{90, 30} {
		t.Fatalf("attach onResize=%v, want [[90 30]]", got)
	}
	m.Detach(a)
	if c, r := m.Size(); c != 90 || r != 30 {
		t.Fatalf("size after full detach = %dx%d, want 90x30 (retained)", c, r)
	}
	if got := rec.snapshot(); len(got) != 1 {
		t.Fatalf("full detach fired onResize %v, want none after the attach", got)
	}
}

// TestResizeIgnoresInvalidViewport: a session reporting a non-positive size
// (cols/rows <= 0, e.g. a client before its first WindowSizeMsg) is skipped by
// the smallest-attached-wins reducer, so it can't collapse the PTY to zero and
// blank every other viewer.
func TestResizeIgnoresInvalidViewport(t *testing.T) {
	rec := &resizeRecorder{}
	m := newMux("h", 100, rec.onResize, nil)

	m.Attach(1, protocol.AttachRW, 100, 40, noopWrite)
	base := rec.snapshot()
	if len(base) != 1 || base[0] != [2]int{100, 40} {
		t.Fatalf("first attach onResize=%v, want [[100 40]]", base)
	}

	// A zero-size and a negative-size client must not drive the min down.
	m.Attach(2, protocol.AttachRW, 0, 0, noopWrite)
	m.Attach(3, protocol.AttachRW, -5, 24, noopWrite)
	if c, r := m.Size(); c != 100 || r != 40 {
		t.Fatalf("size = %dx%d, want 100x40 (invalid sizes ignored)", c, r)
	}
	if got := rec.snapshot(); len(got) != 1 {
		t.Fatalf("invalid-size clients fired onResize %v, want unchanged", got)
	}
}
