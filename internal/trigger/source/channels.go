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
// A session reconnects on jittered backoff, re-initializes when the server
// forgets it, and retries an unfixable failure at the ceiling rather than at
// speed. It runs one `catch_up` firing when it returns from an outage long
// enough that the server's own re-rings cannot have covered it.
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
	"github.com/stump-wtf/harness/internal/scheduler"
	"github.com/stump-wtf/harness/internal/supervisor"
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
	// identity is what the session was built for. A reload compares against
	// it to decide whether the live session still serves the source, which is
	// what lets a byte-identical rewrite leave an open stream alone.
	identity identity
	// deliberate marks a close the reconciler ordered. REQ "Channel
	// Reconnection" says a reload-caused reconnect is not an outage, so the
	// replacement session's first connect must not be credited with one —
	// otherwise every token rotation would fire a catch-up run.
	deliberate bool
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
		m.startSessionLocked(ctx, ref, src, sessionSeed{downSince: m.now()})
	}
}

// sessionSeed carries what a session needs to know about the one it replaces.
//
// It exists for one requirement: "A reconnection caused by a reload SHALL NOT
// count as an outage for REQ Channel Catch-Up". A replacement session is a
// fresh goroutine with fresh locals, so without a seed its first connect would
// look exactly like the daemon's first connect — and every token rotation
// would fire a catch-up run on every bound harness.
type sessionSeed struct {
	// connectedBefore suppresses the first-connect catch-up.
	connectedBefore bool
	// downSince is when the source stopped being connected. A reload sets it
	// to now, so the replacement's outage measures zero.
	downSince time.Time
}

// startSessionLocked runs one source's session on its own goroutine.
func (m *Manager) startSessionLocked(ctx context.Context, ref string, src core.ChannelSource, seed sessionSeed) {
	sctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	m.sources[ref] = &sourceState{
		status:   Status{Source: ref, Kind: core.SourceKindChannel, State: trigger.StateConnecting, Since: time.Now()},
		cancel:   cancel,
		done:     done,
		identity: identityOf(src),
	}
	m.sessions.Add(1)
	go func() {
		defer m.sessions.Done()
		defer close(done)
		// `connecting` is reported from the session goroutine, before it
		// can report anything else, so a handler sees this source's changes
		// in the order they happened. A `go notify` from here would race the
		// session's own synchronous reports and could land after them.
		m.notifyStatus(ref)
		m.runSession(sctx, ref, src, seed)
	}()
}

// notifyStatus reports a source's current status to OnState, off the lock.
func (m *Manager) notifyStatus(ref string) {
	m.mu.Lock()
	st, notify := m.sources[ref], m.onState
	var status Status
	if st != nil {
		status = st.status
	}
	m.mu.Unlock()
	if st != nil && notify != nil {
		notify(status)
	}
}

// runSession is one source's connect loop: initialize, listen, and on any
// failure wait and try again until the context is cancelled.
//
// The loop owns three pieces of state that only make sense together, which is
// why they are locals rather than fields: the backoff, whether this is the
// first connect since the daemon started, and when the source was last
// disconnected. The last two are what decide a catch-up firing.
func (m *Manager) runSession(ctx context.Context, ref string, src core.ChannelSource, seed sessionSeed) {
	var (
		bo     = &channel.Backoff{Rand: m.rand}
		client *channel.Client
		// ready is whether client completed Initialize. Not SessionID() != "":
		// Initialize stores the session id BEFORE the capability check and
		// the initialized notification, so a failed initialize leaves one
		// behind, and a retry keyed on it would skip straight to the GET.
		ready       bool
		firstDone   = seed.connectedBefore // has this source ever connected?
		downSince   = seed.downSince       // when it stopped being connected
		everStarted bool                   // has an attempt run yet?
		// reinitSpent guards the one immediate re-initialize an expired
		// session earns. Cleared whenever a stream opens.
		reinitSpent bool
	)
	if downSince.IsZero() {
		downSince = m.now()
	}
	defer func() {
		if client != nil {
			closeClient(ctx, client)
		}
	}()

	for ctx.Err() == nil {
		if client == nil {
			client, ready = m.dial(src), false
		}
		if everStarted {
			m.setState(ref, trigger.StateConnecting, "")
		}
		everStarted = true

		openFor, err := m.attempt(ctx, ref, src, client, &ready, &firstDone, &downSince, bo)
		if ctx.Err() != nil {
			// A deliberate close — shutdown, or the reconciler ending this
			// session. Never an outage, so the next session's first connect
			// is not credited with one.
			return
		}
		if !ready {
			// Initialize failed part-way. Whatever session id it was handed
			// belongs to a half-built session — possibly one whose server
			// lacks the channel capability — so the retry starts from a fresh
			// client and re-runs every check. The half-built session is ended
			// best effort; the server may never hear of it again otherwise.
			if client.SessionID() != "" {
				closeClient(ctx, client)
			}
			client = nil
		}
		bo.ResetAfter(openFor)
		if openFor > 0 {
			// The stream opened, so the server is reachable and whatever
			// killed the session is worth one fast re-initialize.
			reinitSpent = false
		}

		state, delay := trigger.StateBackoff, bo.Next()
		reason := "the server closed the stream"
		if err != nil {
			reason = err.Error()
			if terminalState(err) == trigger.StateError {
				// A wrong credential, a server that is not a channel server,
				// a protocol violation: trying sooner will not fix it, and
				// hammering an auth endpoint is its own problem.
				state, delay = trigger.StateError, bo.Ceiling()
			}
		}
		if errors.Is(err, channel.ErrSessionExpired) {
			// The server has forgotten this session id. A fresh client is
			// the only way back: reusing this one would keep sending the
			// dead id and keep getting 404.
			client = nil
			// And it is worth going back AT ONCE rather than waiting out the
			// backoff. An expired session is not a connectivity failure —
			// the server answered, it simply does not know this id — so the
			// delay the ladder had reached describes an outage that is over.
			//
			// Found in a live run against a real Switchboard: after it
			// restarted, the daemon had escalated to a two-minute delay
			// during the outage, saw the 404 that means "re-initialize", and
			// then sat idle for two more minutes with the server back up.
			// REQ "Channel Reconnection" says to re-initialize; waiting out a
			// backoff earned by a different failure is not that.
			//
			// Once, though. A server that answers 404 to every session id
			// would otherwise spin: the second expiry in a row falls back to
			// the ladder, and a stream that opens in between earns the fast
			// path again.
			if !reinitSpent {
				reinitSpent = true
				delay = 0
				reason = "the session expired; re-initializing"
			}
		}
		m.setState(ref, state, reason)
		if delay == 0 {
			m.log.Info("channel session expired; re-initializing at once", "source", ref)
		} else {
			m.log.Warn("channel session down; retrying",
				"source", ref, "state", string(state), "retry_in", delay.String(), "err", reason)
		}

		if !m.sleep(ctx, delay) {
			return
		}
	}
}

// closeClient ends a session with a best-effort DELETE.
//
// On a context of its own: the session context is already cancelled by the
// time a shutdown gets here, and a DELETE on a cancelled context never leaves.
func closeClient(ctx context.Context, client *channel.Client) {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	_ = client.Close(dctx)
}

// attempt runs one connect-and-listen. It returns how long the stream stayed
// open (zero when it never did) and why it ended.
func (m *Manager) attempt(
	ctx context.Context,
	ref string,
	src core.ChannelSource,
	client *channel.Client,
	ready *bool,
	firstDone *bool,
	downSince *time.Time,
	bo *channel.Backoff,
) (time.Duration, error) {
	if !*ready {
		if err := client.Initialize(ctx); err != nil {
			return 0, err
		}
		*ready = true
	}

	var openedAt time.Time
	err := client.Listen(ctx, &sessionHandler{m: m, ref: ref, src: src}, func() {
		// The one place `connected` is set: the stream is open and about to
		// be read.
		openedAt = m.now()
		m.setState(ref, trigger.StateConnected, "")
		m.log.Info("channel connected", "source", ref, "attempts", bo.Attempts())
		m.onConnected(ref, src, firstDone, *downSince)
	})
	if openedAt.IsZero() {
		// Never opened: the source has been down continuously, so downSince
		// keeps its earlier value and a long outage stays long.
		return 0, err
	}
	open := m.now().Sub(openedAt)
	*downSince = m.now()
	return open, err
}

// onConnected decides whether this connection owes a catch-up firing.
//
// Two cases, and the second is the one with a number attached: the first
// connect after the daemon started, and a return from an outage longer than
// the scheduler's LateGrace.
//
// Reusing LateGrace rather than picking a threshold here is deliberate. A
// doorbell lost to a brief blip is re-rung by the server within minutes, so
// only a long gap or a restart can outlast the re-rings; and "late" then means
// one thing across the daemon rather than two numbers that drift. A flapping
// link cannot produce a run per flap either, because the backoff stretches the
// very outages this measures.
// Governing: SPEC-0014 REQ "Channel Catch-Up"; SPEC-0008 REQ "Missed Window
// Handling".
func (m *Manager) onConnected(ref string, src core.ChannelSource, firstDone *bool, downSince time.Time) {
	first := !*firstDone
	*firstDone = true
	outage := m.now().Sub(downSince)
	switch {
	case first:
		m.log.Info("channel connected for the first time since start; catching up", "source", ref)
	case outage > scheduler.LateGrace:
		m.log.Info("channel reconnected after an outage; catching up",
			"source", ref, "outage", outage.Round(time.Second).String())
	default:
		// A blip. The server's own re-rings cover it, and a catch-up run per
		// flap would be worse than the gap it papers over.
		return
	}
	m.catchUp(ref)
}

// catchUp starts one catch_up run for every bound harness that asked for one.
//
// The run carries NO event file: there is no event — the point is that
// something may have been missed, and only the agent can find out what. It
// goes through on_overlap like any other firing, so a catch-up that lands on a
// run already in flight is held or skipped rather than stacking.
// Governing: SPEC-0014 REQ "Channel Catch-Up".
func (m *Manager) catchUp(ref string) {
	cfg := m.config()
	for _, name := range cfg.BoundHarnesses(ref) {
		h, ok := cfg.Harnesses[name]
		if !ok || !h.CatchUp {
			continue
		}
		if _, ok := m.runner.StartRun(name, supervisor.RunRequest{
			Trigger: supervisor.TriggerCatchUp,
			Source:  ref,
		}); !ok {
			m.log.Warn("catch-up fired for a harness the daemon does not know", "harness", name, "source", ref)
			continue
		}
		m.log.Info("catch-up run started", "harness", name, "source", ref)
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
