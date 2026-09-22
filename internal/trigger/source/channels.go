package source

// The channel half of the source manager: one listen-only MCP session per
// enabled, bound `[channel.*]` source, and the state each one reports.
//
// The state machine is driven ONLY by events the listener observes — the
// stream opened, the stream ended, a request failed — never by a poll that
// re-reads fresh state. design.md records why, from the Crush fork's
// stream-health watchdog: that watchdog registered a new health record on
// every connect, so treating "never observed open" as unhealthy became a
// self-sustaining rebuild loop, once a minute, on hosts whose streams were in
// fact delivering. Absence of evidence is not evidence of a dead stream.
//
// Reconnection, catch-up and reload reconciliation are #474. Here a session
// that fails or ends settles into its terminal state and stays there until the
// daemon restarts; that is the seam the next story replaces, and saying so is
// better than a retry loop written twice.
//
// Governing: ADR-0021; SPEC-0014 REQ "Channel Listener Session", REQ "Channel
// Notification Handling", REQ "Firing".
//
// @joestump 09/23/2026 - Introduced with the SPEC-0014 channel listener (#471).

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/trigger"
	"github.com/stump-wtf/harness/internal/trigger/channel"
)

// Status is one source's reported state, the shape `harness triggers` renders
// (#476) and the counters #480 will export.
type Status struct {
	// Source is the reference, e.g. "channel.sb".
	Source string
	// Kind is "channel" or "webhook".
	Kind string
	// State is the current state.
	State trigger.SourceState
	// Reason explains a StateError or StateBackoff. It names what failed,
	// never a credential or a payload.
	Reason string
	// Since is when the state last changed.
	Since time.Time
	// LastEvent is when this source last produced a firing, zero for never.
	LastEvent time.Time
	// Events counts valid events this source has produced.
	Events int
	// Invalid counts messages dropped for violating REQ "Channel
	// Notification Handling".
	Invalid int
}

// sourceState is the manager's mutable record for one source.
type sourceState struct {
	status Status
	cancel context.CancelFunc
	done   chan struct{}
}

// dialer opens a channel session. It is a field on the Manager so a test can
// substitute one without an HTTP server — but the tests in this package use a
// real one against a real httptest server instead, because the thing worth
// testing is whether the client speaks the protocol, and a fake dialer would
// assert only that the manager calls it.
type dialer func(core.ChannelSource) *channel.Client

// openChannels starts a session for every channel source that is enabled and
// bound, and records a state for the ones that are not.
//
// Called from Start, under m.mu.
func (m *Manager) openChannelsLocked(ctx context.Context) {
	cfg := m.config()
	for _, src := range cfg.OrderedChannels() {
		ref := core.SourceKindChannel + "." + src.Name
		switch {
		case !src.Enabled:
			m.setStatusLocked(ref, core.SourceKindChannel, trigger.StateDisabled, "")
			continue
		case len(cfg.BoundHarnesses(ref)) == 0:
			// Not an error: a source declared before the harness that will
			// bind it is an ordinary state on the way to a working config.
			// Opening a session anyway would hold a server-side consumer slot
			// and swallow doorbells nothing could act on.
			m.setStatusLocked(ref, core.SourceKindChannel, trigger.StateUnbound, "")
			continue
		}
		m.startSessionLocked(ctx, ref, src)
	}
}

// startSessionLocked runs one source's session on its own goroutine.
func (m *Manager) startSessionLocked(ctx context.Context, ref string, src core.ChannelSource) {
	sctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	m.sources[ref] = &sourceState{
		status: Status{Source: ref, Kind: core.SourceKindChannel, State: trigger.StateConnecting, Since: time.Now()},
		cancel: cancel,
		done:   done,
	}
	if m.onState != nil {
		status := m.sources[ref].status
		notify := m.onState
		go notify(status)
	}
	m.sessions.Add(1)
	go func() {
		defer m.sessions.Done()
		defer close(done)
		m.runSession(sctx, ref, src)
	}()
}

// runSession initializes, listens, and records what happened.
func (m *Manager) runSession(ctx context.Context, ref string, src core.ChannelSource) {
	client := m.dial(src)
	defer func() {
		// Best effort, and on a context of its own: the session context is
		// already cancelled by the time a shutdown gets here, and a DELETE on
		// a cancelled context never leaves.
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		_ = client.Close(dctx)
	}()

	if err := client.Initialize(ctx); err != nil {
		if ctx.Err() != nil {
			return
		}
		m.setState(ref, terminalState(err), err.Error())
		m.log.Warn("channel session failed", "source", ref, "err", err.Error())
		return
	}

	err := client.Listen(ctx, &sessionHandler{m: m, ref: ref, src: src}, func() {
		// The one place `connected` is set: the stream is open and about to
		// be read.
		m.setState(ref, trigger.StateConnected, "")
		m.log.Info("channel connected", "source", ref)
	})
	switch {
	case ctx.Err() != nil:
		// A deliberate close — shutdown, or the source going away. Not an
		// outage, and #474 must not treat it as one for catch-up purposes.
		return
	case err != nil:
		m.setState(ref, terminalState(err), err.Error())
		m.log.Warn("channel stream ended", "source", ref, "err", err.Error())
	default:
		// A clean end of stream: the server closed. Not success — there is
		// nothing to hear until a reconnect (#474).
		m.setState(ref, trigger.StateBackoff, "the server closed the stream")
		m.log.Info("channel stream closed by the server", "source", ref)
	}
}

// terminalState maps a session failure to the state it should report.
//
// The split is what #474's retry policy keys off: `error` is a failure that
// retrying at speed will not fix — a server that is not a channel server, a
// credential that is wrong — and `backoff` is everything else, which is
// usually the network.
func terminalState(err error) trigger.SourceState {
	switch {
	case errors.Is(err, channel.ErrNoChannelCapability),
		errors.Is(err, channel.ErrStreamRefused),
		errors.Is(err, channel.ErrUnauthorized),
		errors.Is(err, channel.ErrProtocol):
		return trigger.StateError
	default:
		return trigger.StateBackoff
	}
}

// sessionHandler turns a doorbell into a firing.
type sessionHandler struct {
	m   *Manager
	ref string
	src core.ChannelSource
}

// Notification fans one doorbell out to every bound harness.
//
// content and meta are stored in the envelope exactly as received and are not
// interpreted, routed on, or rewritten. That is REQ "Channel Notification
// Handling", and it is the same rule the webhook body follows for the same
// reason: whoever can ring the doorbell should not be able to decide what the
// agent does about it.
func (h *sessionHandler) Notification(content string, meta map[string]string) {
	ev := &trigger.Envelope{
		Version:    trigger.EnvelopeVersion,
		Kind:       trigger.KindChannel,
		Source:     h.ref,
		EventID:    newEventID(),
		ReceivedAt: time.Now().UTC(),
		Channel:    &trigger.ChannelEvent{Content: content, Meta: meta},
	}
	h.m.noteEvent(h.ref)
	h.m.Fire(ev)
}

// Invalid records a dropped message. It fires nothing.
func (h *sessionHandler) Invalid(reason string) { h.m.noteInvalid(h.ref) }

// closeChannels ends every session and waits for them.
func (m *Manager) closeChannels() {
	m.mu.Lock()
	states := make([]*sourceState, 0, len(m.sources))
	for _, st := range m.sources {
		states = append(states, st)
	}
	m.mu.Unlock()
	for _, st := range states {
		if st.cancel != nil {
			st.cancel()
		}
	}
	m.sessions.Wait()
}

// setState records a state change, with its reason.
func (m *Manager) setState(ref string, state trigger.SourceState, reason string) {
	m.mu.Lock()
	st := m.sources[ref]
	if st == nil || (st.status.State == state && st.status.Reason == reason) {
		m.mu.Unlock()
		return
	}
	st.status.State, st.status.Reason, st.status.Since = state, reason, time.Now()
	status := st.status
	notify := m.onState
	m.mu.Unlock()
	// Outside the lock: a handler is free to call back into Status without
	// deadlocking, and a slow one cannot hold up the session goroutine's
	// own lock.
	if notify != nil {
		notify(status)
	}
}

// setStatusLocked records a state for a source with no session. Caller holds
// m.mu.
func (m *Manager) setStatusLocked(ref, kind string, state trigger.SourceState, reason string) {
	m.sources[ref] = &sourceState{status: Status{
		Source: ref, Kind: kind, State: state, Reason: reason, Since: time.Now(),
	}}
	if m.onState != nil {
		// Deferred off the lock for the reason setState explains.
		status := m.sources[ref].status
		notify := m.onState
		go notify(status)
	}
}

func (m *Manager) noteEvent(ref string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if st := m.sources[ref]; st != nil {
		st.status.Events++
		st.status.LastEvent = time.Now()
	}
}

func (m *Manager) noteInvalid(ref string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if st := m.sources[ref]; st != nil {
		st.status.Invalid++
	}
}

// Status returns a snapshot of every source's state, sorted by reference so a
// caller renders it deterministically.
func (m *Manager) Status() []Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Status, 0, len(m.sources))
	for _, st := range m.sources {
		out = append(out, st.status)
	}
	sortStatus(out)
	return out
}

// StatusOf returns one source's state, and whether the manager knows it.
func (m *Manager) StatusOf(ref string) (Status, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.sources[ref]
	if st == nil {
		return Status{}, false
	}
	return st.status, true
}

// sortStatus orders statuses by source reference.
func sortStatus(s []Status) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j].Source < s[j-1].Source; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// eventIDs makes the daemon-generated ids a channel doorbell needs: the
// transport carries no delivery id of its own, and a run record with no
// event_id would make two firings indistinguishable in a history.
var eventIDs struct {
	sync.Mutex
	n int
}

func newEventID() string {
	eventIDs.Lock()
	defer eventIDs.Unlock()
	eventIDs.n++
	return "ch-" + time.Now().UTC().Format("20060102T150405.000") + "-" + itoa(eventIDs.n)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}
