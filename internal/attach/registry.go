package attach

// Governing: SPEC-0002 REQ "Attach Session"; ADR-0003 (one x/vt emulator per
// harness); ADR-0007 (per-harness ring). The Registry owns one Mux per harness
// name and hands the supervisor Manager a WriterFor(name) tee target through the
// ManagerOptions.ExtraOutFor hook, closing the wiring the reviewer flagged as
// missing: raw PTY output now tees to BOTH the durable log and this vt ring.

import (
	"io"
	"sync"
)

// Controller is the slice of the supervisor Manager the attach layer needs to
// drive the real PTY: apply the smallest-attached-wins resize and deliver
// read-write keystrokes. *supervisor.Manager satisfies it.
type Controller interface {
	Resize(name string, cols, rows int) bool
	WriteInput(name string, p []byte) bool
}

// Registry maps harness name → Mux, creating each lazily on first use. It is
// safe for concurrent use.
type Registry struct {
	ringLines int

	mu    sync.Mutex
	muxes map[string]*Mux
	ctrl  Controller
}

// NewRegistry builds a registry whose muxes each keep ringLines of scrollback
// (DefaultRingLines when <=0).
func NewRegistry(ringLines int) *Registry {
	return &Registry{ringLines: ringLines, muxes: make(map[string]*Mux)}
}

// SetController wires the Manager the muxes call back into for PTY resize and
// input. Call it once, before the daemon starts harnesses or serves clients, so
// the callbacks are visible to the goroutines that later use them.
func (r *Registry) SetController(c Controller) {
	r.mu.Lock()
	r.ctrl = c
	r.mu.Unlock()
}

// Mux returns the Mux for name, creating it if needed. The same instance is
// returned for a name across process restarts of that harness, so attach state
// (screen + scrollback + sessions) survives a supervised process respawn while
// the daemon lives (ADR-0007).
func (r *Registry) Mux(name string) *Mux {
	r.mu.Lock()
	defer r.mu.Unlock()
	if m, ok := r.muxes[name]; ok {
		return m
	}
	m := newMux(name, r.ringLines,
		func(cols, rows int) {
			if c := r.controller(); c != nil {
				c.Resize(name, cols, rows)
			}
		},
		func(p []byte) {
			if c := r.controller(); c != nil {
				c.WriteInput(name, p)
			}
		},
	)
	r.muxes[name] = m
	return m
}

// WriterFor is the ManagerOptions.ExtraOutFor adapter: it returns the harness's
// Mux as an io.Writer so the supervisor tees raw PTY output into it.
func (r *Registry) WriterFor(name string) io.Writer { return r.Mux(name) }

// controller reads the wired controller under the lock.
func (r *Registry) controller() Controller {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ctrl
}
