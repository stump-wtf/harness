package attach

// Governing: SPEC-0002 REQ "Attach Session" (snapshot → scrollback tail → live,
// with no live byte ahead of the tail) and REQ "Backpressure Isolation" (the
// tail shares the session's bounded queue and coalesces like live output);
// ADR-0007 (byte-bounded ring, replayed in bounded frames).

import (
	"bytes"
	"fmt"
	"net"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/protocol"
)

// waitForFrames polls until the collector holds a frame containing sub. A
// snapshot counts, which is what a "the client saw this byte" wait wants; a
// wait about the replay must use waitForReplay instead.
func waitForFrames(t *testing.T, c *collector, sub string) [][]byte {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		frames := c.all()
		for _, f := range frames {
			if bytes.Contains(f, []byte(sub)) {
				return frames
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("no frame containing %q within timeout", sub)
	return nil
}

// waitForReplay polls until the replay — every frame after the screen
// snapshot — holds sub, then returns all frames. It deliberately avoids the
// two things a plain "some frame contains sub" wait checks:
//
// The snapshot is not part of the replay. The live writer races Attach, and
// when it wins, the newest live line is already on the screen the snapshot
// renders, so a wait over every frame is satisfied by frame 0 alone, while the
// pump has delivered no replay byte yet. That is #984: the caller then joins
// frames[1:], sees zero bytes, and reports an empty replay.
//
// Frames are storage chunks, not messages, so the bytes sought can straddle a
// frame boundary: the search runs over the replay joined, not one frame at a
// time.
func waitForReplay(t *testing.T, c *collector, sub string) [][]byte {
	t.Helper()
	needle := []byte(sub)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		frames := c.all()
		if len(frames) > 1 && bytes.Contains(bytes.Join(frames[1:], nil), needle) {
			return frames
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("replay never contained %q within timeout", sub)
	return nil
}

// TestAttachChunkedTailOrdering attaches while another goroutine is writing.
// Everything after the snapshot must be one contiguous, line-aligned run of the
// stream the writer produced: tail frames first, live frames after, nothing
// missing, repeated or reordered between them. Runs under -race, which also
// checks that sending views of the ring's storage never races the writer. The
// writer may also finish before Attach gets the lock; then the tail carries
// the live bytes and no live frame follows. Either way the replay must be one
// contiguous suffix of the stream, which is what is asserted.
func TestAttachChunkedTailOrdering(t *testing.T) {
	lim := RingLimits{Bytes: 64 << 10, chunkSize: 1 << 10}
	m := newMuxLimits("h", lim, nil, nil, nil)

	var (
		mu     sync.Mutex
		stream []byte
	)
	write := func(s string) {
		mu.Lock()
		stream = append(stream, s...)
		mu.Unlock()
		m.Write([]byte(s))
	}
	for i := range 3000 { // several budgets' worth: the ring has evicted
		write(fmt.Sprintf("hist %05d %s\r\n", i, strings.Repeat(".", i%40)))
	}

	// 150 live lines keep the queue short of queueCap even if the pump never
	// ran, so this test sees ordering, not coalescing.
	const live = 150
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range live {
			if i == live/3 {
				close(started)
			}
			write(fmt.Sprintf("live %05d\r\n", i))
		}
	}()
	<-started
	c := &collector{}
	s := m.Attach(1, protocol.AttachRO, 80, 24, c.write)
	defer m.Detach(s)
	<-done

	frames := waitForReplay(t, c, fmt.Sprintf("live %05d", live-1))
	if !bytes.HasPrefix(frames[0], snapshotPrefix) {
		t.Fatalf("first frame is not the snapshot: %q", frames[0][:min(len(frames[0]), 40)])
	}
	for i, f := range frames[1:] {
		if bytes.HasPrefix(f, snapshotPrefix) {
			t.Fatalf("frame %d is a snapshot: the session coalesced, which this test must not provoke", i+1)
		}
	}
	after := bytes.Join(frames[1:], nil)
	mu.Lock()
	all := append([]byte(nil), stream...)
	mu.Unlock()
	if !bytes.HasSuffix(all, after) {
		t.Fatal("tail + live is not a contiguous run of the written stream: a byte was lost, repeated or reordered")
	}
	if cut := len(all) - len(after); cut > 0 && all[cut-1] != '\n' {
		t.Fatalf("replay starts mid-line: %q", after[:min(len(after), 40)])
	}
	if !bytes.HasPrefix(after, []byte("hist ")) {
		t.Errorf("replay does not open on scrollback history: %q", after[:min(len(after), 40)])
	}
	if len(after) < lim.Bytes/2 {
		t.Errorf("replayed %d bytes; want most of the %d-byte ring", len(after), lim.Bytes)
	}
}

// TestAttachTailFramesBounded: every tail frame is at most one storage chunk,
// and opening a session does not allocate in proportion to the ring — the old
// Tail() copied all of it, twice counting the frame encoding.
func TestAttachTailFramesBounded(t *testing.T) {
	lim := RingLimits{Bytes: 32 << 20}
	m := newMuxLimits("h", lim, nil, nil, nil)
	line := benchLine(30 << 10)
	for range (20 << 20) / len(line) {
		m.ring.Write(line) // straight to the ring: the emulator adds seconds, not coverage
	}
	chunk := m.ring.lim.chunkSize

	var mu sync.Mutex
	var sizes []int
	block := make(chan struct{})
	record := func(p []byte) error {
		<-block // hold the pump so only Attach's own allocations are measured
		mu.Lock()
		sizes = append(sizes, len(p))
		mu.Unlock()
		return nil
	}

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	s := m.Attach(1, protocol.AttachRO, 80, 24, record)
	runtime.ReadMemStats(&after)
	close(block)
	defer m.Detach(s)

	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 1<<20 {
		t.Errorf("Attach allocated %d bytes with a 20 MiB ring; want well under 1 MiB (no ring copy)", alloc)
	} else {
		t.Logf("Attach allocated %d bytes with a %d-byte ring", alloc, m.ring.Len())
	}

	deadline := time.Now().Add(5 * time.Second)
	want := m.ring.Len()
	for {
		mu.Lock()
		got := 0
		for _, n := range sizes[min(1, len(sizes)):] {
			got += n
		}
		n := len(sizes)
		mu.Unlock()
		if got >= want {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("received %d of %d tail bytes in %d frames", got, want, n)
		}
		time.Sleep(2 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sizes)-1 > maxTailFrames {
		t.Errorf("%d tail frames, want <= %d", len(sizes)-1, maxTailFrames)
	}
	for i, n := range sizes[1:] {
		if n > chunk {
			t.Errorf("tail frame %d is %d bytes, over the %d-byte chunk", i, n, chunk)
		}
	}
}

// TestAttachTailOverMaxFrameSize is the end-to-end regression. A tail bigger
// than protocol.MaxFrameSize used to go out as one frame; WriteFrame refused
// it, the pump took the error for a dead client, and the session detached
// right after the snapshot, so the client never saw another byte. Through a
// real framed connection, live output must now arrive.
func TestAttachTailOverMaxFrameSize(t *testing.T) {
	m := newMuxLimits("h", RingLimits{Bytes: 32 << 20}, nil, nil, nil)
	line := benchLine(30 << 10)
	for range (20 << 20) / len(line) {
		m.ring.Write(line) // straight to the ring: the emulator adds seconds, not coverage
	}
	if m.ring.Len() <= protocol.MaxFrameSize {
		t.Fatalf("ring holds %d bytes; the test needs more than MaxFrameSize (%d)", m.ring.Len(), protocol.MaxFrameSize)
	}

	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	srv, cli := protocol.NewConn(a), protocol.NewConn(b)
	got := make(chan []byte, 1024)
	go func() {
		defer close(got)
		for {
			f, err := cli.ReadFrame()
			if err != nil {
				return
			}
			_, data, err := protocol.DecodeAttach(f.Payload)
			if err != nil {
				return
			}
			got <- data
		}
	}()
	var werr error
	var werrMu sync.Mutex
	s := m.Attach(1, protocol.AttachRO, 80, 24, func(p []byte) error {
		err := srv.WriteFrame(protocol.TypeAttachData, protocol.EncodeAttach(1, p))
		if err != nil {
			werrMu.Lock()
			werr = err
			werrMu.Unlock()
		}
		return err
	})
	defer m.Detach(s)

	m.Write([]byte("LIVE-MARKER\r\n"))
	deadline := time.After(10 * time.Second)
	for {
		select {
		case d, ok := <-got:
			if !ok {
				t.Fatal("stream closed before live output arrived")
			}
			if bytes.Contains(d, []byte("LIVE-MARKER")) {
				return
			}
		case <-deadline:
			werrMu.Lock()
			defer werrMu.Unlock()
			t.Fatalf("live output never reached the client (sessions left: %d, write error: %v)", m.SessionCount(), werr)
		}
	}
}

// TestAttachTailCoalescesForSlowClient: a client that stops reading while its
// tail is still queued is coalesced like any other slow client. Write never
// blocks, and once it drains the client gets a fresh snapshot instead of the
// whole backlog.
func TestAttachTailCoalescesForSlowClient(t *testing.T) {
	m := newMuxLimits("h", RingLimits{Bytes: 256 << 10, chunkSize: 4 << 10}, nil, nil, nil)
	for i := range 20000 {
		m.Write([]byte(fmt.Sprintf("hist %05d\r\n", i)))
	}
	release := make(chan struct{})
	slow := &collector{}
	s := m.Attach(1, protocol.AttachRO, 80, 24, func(p []byte) error {
		<-release
		return slow.write(p)
	})
	defer m.Detach(s)

	const n = 2 * queueCap
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range n {
			m.Write([]byte(fmt.Sprintf("live %05d\r\n", i)))
		}
	}()
	select {
	case <-done:
	case <-time.After(stallTimeout):
		close(release)
		t.Fatal("Write blocked behind a stalled client holding a queued tail")
	}
	close(release)

	frames := waitForFrames(t, slow, fmt.Sprintf("live %05d", n-1))
	snapshots := 0
	for _, f := range frames {
		if bytes.HasPrefix(f, snapshotPrefix) {
			snapshots++
		}
	}
	if snapshots < 2 {
		t.Errorf("got %d snapshots; want the attach snapshot plus a coalesced repaint", snapshots)
	}
	if tailFrames := len(m.ring.tailFrames()); len(frames) >= 1+tailFrames+n {
		t.Errorf("slow client received all %d frames; want the backlog coalesced", len(frames))
	}
}
