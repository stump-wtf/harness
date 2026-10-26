package tui

// Governing: SPEC-0001 scenario "Driving a live agent", ADR-0002 (thin client:
// the model writes attach frames itself, off the read loop). Issue #952
// (upstream stump-wtf/harness#27): Bubble Tea v2 runs every tea.Cmd returned
// from Update in its own goroutine, so a keystroke that writes from inside its
// Cmd races with the keystroke typed just before it, and the guest receives the
// bytes in whatever order the scheduler ran the goroutines. Update is the one
// place that sees input in typed order, so the order is recorded there and a
// single draining Cmd writes the queue front to back.

import (
	"sync"

	tea "charm.land/bubbletea/v2"
)

// attachInputFrame is one buffered write, bound to the session it was typed
// for and the connection that was current when it was typed. Binding both at
// enqueue time keeps a drain that outlives a hop from writing an old
// keystroke into the session the user switched to.
type attachInputFrame struct {
	sessionID uint32
	data      []byte
	send      func(sessionID uint32, data []byte) error
}

// attachInputQueue is the ordered write queue for attached input. Update owns
// the append; at most one draining Cmd exists at a time, and it is the only
// thing that pops.
type attachInputQueue struct {
	mu sync.Mutex
	// pending is the typed-order backlog, front next to be written.
	pending []attachInputFrame
	// draining records that a drain Cmd is live. It is cleared only by that
	// Cmd, and only while holding mu, so a push that finds it set knows its
	// bytes will be written by the drain already running.
	draining bool
}

// push appends one write in typed order and returns the Cmd that starts the
// drain, or nil when a drain is already in flight and will reach this frame.
func (q *attachInputQueue) push(sessionID uint32, data []byte, send func(uint32, []byte) error) tea.Cmd {
	q.mu.Lock()
	q.pending = append(q.pending, attachInputFrame{sessionID: sessionID, data: data, send: send})
	start := !q.draining
	if start {
		q.draining = true
	}
	q.mu.Unlock()
	if !start {
		return nil
	}
	return func() tea.Msg {
		q.drain()
		return nil
	}
}

// drain writes the backlog in order and releases the drain slot once it is
// empty. Setting draining under the same lock that finds the queue empty is
// what closes the window where a push could land after that check but before
// the flag was cleared, stranding the frame until the next keystroke.
func (q *attachInputQueue) drain() {
	for {
		q.mu.Lock()
		if len(q.pending) == 0 {
			q.pending = nil
			q.draining = false
			q.mu.Unlock()
			return
		}
		frame := q.pending[0]
		q.pending = q.pending[1:]
		q.mu.Unlock()

		if err := frame.send(frame.sessionID, frame.data); err != nil {
			// The wire is gone, so nothing queued behind this frame can land
			// either: dropping the backlog matches what a per-keystroke write
			// would have done a moment later, and keeps a dead session from
			// being re-hit once per buffered keystroke. This exit also releases
			// the drain slot, because this drain is finishing: leaving it
			// claimed would strand every keystroke after the failure, with no
			// writer left to hand them to.
			q.dropAndStop()
			return
		}
	}
}

// drop discards the backlog. The drain slot is left alone on purpose: a drain
// that is still writing clears it when it next finds the queue empty, so a
// push racing this call can never start a second writer alongside it.
func (q *attachInputQueue) drop() {
	q.mu.Lock()
	q.pending = nil
	q.mu.Unlock()
}

// dropAndStop discards the backlog and releases the drain slot. Only a drain
// may call it, on its way out: it owns the slot, so clearing it here cannot
// start a second writer, and clearing it is what lets the next keystroke start
// the one writer that will actually reach the wire.
func (q *attachInputQueue) dropAndStop() {
	q.mu.Lock()
	q.pending = nil
	q.draining = false
	q.mu.Unlock()
}

// queueAttachInput buffers keystrokes or pasted bytes for an attached session
// and returns the drain Cmd when this call has to start one.
func (m *Model) queueAttachInput(sessionID uint32, data []byte) tea.Cmd {
	if m.attach == nil {
		return nil
	}
	return m.attachInput.push(sessionID, data, m.attach.AttachInput)
}
