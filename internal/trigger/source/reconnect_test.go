package source

// Reconnection, catch-up and reload reconciliation.
//
// The regression guard in TestNoChangeRewriteHoldsTheStream is the reason this
// file exists in the shape it does. It is aimed at the Crush watchdog's
// failure, which design.md records: a stream that was never observed open was
// treated as unhealthy, so a fresh health record on every connect became a
// once-a-minute rebuild loop on hosts whose streams were in fact delivering.
// Here the equivalent mistake is reconnecting on every reload — and chezmoi
// rewrites harness.toml byte-identically on a timer, so it would happen
// constantly and look like a flaky server.
//
// A fake clock drives the backoff and the catch-up threshold, so nothing here
// sleeps for longer than a scheduler tick.
//
// Governing: ADR-0021; SPEC-0014 REQ "Channel Reconnection", REQ "Channel
// Catch-Up", REQ "Source Reconciliation On Reload".
//
// @joestump 09/23/2026 - Introduced with SPEC-0014 channel reconnection (#474).

import (
	"context"
	"sync"
	"testing"
	"time"

	"charm.land/log/v2"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/supervisor"
	"github.com/stump-wtf/harness/internal/trigger"
	"github.com/stump-wtf/harness/internal/trigger/channel"
	"github.com/stump-wtf/harness/internal/trigger/channel/testserver"
)

// fakeClock is a manually advanced clock, plus a sleep that returns instantly
// and records what it was asked to wait.
//
// Recording rather than honouring the delay is the point: the SCHEDULE is what
// the requirement is about, and a test that actually slept 5 minutes to check
// a 5-minute ceiling would be untestable by construction.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	slept  []time.Duration
	onWait func(d time.Duration)
}

func newClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 9, 23, 9, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward, which is how an outage is produced.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) bool {
	c.mu.Lock()
	c.slept = append(c.slept, d)
	hook := c.onWait
	c.mu.Unlock()
	if hook != nil {
		hook(d)
	}
	return ctx.Err() == nil
}

func (c *fakeClock) Slept() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.slept...)
}

// stateLog records every state change a manager reports.
//
// Polling the CURRENT state cannot see a transition that came and went —
// with an instant fake Sleep, a source can go connected → backoff →
// connected between two polls — so every sequence assertion below reads this
// instead.
type stateLog struct {
	mu     sync.Mutex
	states []trigger.SourceState
}

func (l *stateLog) add(s Status) {
	l.mu.Lock()
	l.states = append(l.states, s.State)
	l.mu.Unlock()
}

func (l *stateLog) snapshot() []trigger.SourceState {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]trigger.SourceState(nil), l.states...)
}

// waitSubsequence waits until the recorded states contain want in order (not
// necessarily adjacently).
func (l *stateLog) waitSubsequence(t *testing.T, what string, want ...trigger.SourceState) {
	t.Helper()
	waitFor(t, 5*time.Second, what, func() bool {
		got, i := l.snapshot(), 0
		for _, s := range got {
			if s == want[i] {
				if i++; i == len(want) {
					return true
				}
			}
		}
		return false
	})
}

// reconnectManager builds a manager on a fake clock, recording its states.
func reconnectManager(t *testing.T, r Runner, cfg func() *core.Config, clk *fakeClock) (*Manager, *stateLog) {
	t.Helper()
	states := &stateLog{}
	m := New(Options{
		Runner:  r,
		Config:  cfg,
		Log:     log.New(discard{}),
		Now:     clk.Now,
		Sleep:   clk.Sleep,
		Rand:    func() float64 { return 0 }, // no jitter, so delays are exact
		OnState: states.add,
	})
	m.Start(context.Background())
	t.Cleanup(m.Close)
	return m, states
}

// configHolder is a *core.Config a test can swap while the manager is running.
//
// Options.Config is called from the session goroutines — by design, so a
// reload's change to a bound set applies from the next event — so a test that
// reassigns a plain variable is a data race the -race build catches. Holding
// it behind a mutex is the small price of a config that really is read
// concurrently.
type configHolder struct {
	mu  sync.Mutex
	cfg *core.Config
}

func holding(cfg *core.Config) *configHolder { return &configHolder{cfg: cfg} }

func (h *configHolder) get() *core.Config {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cfg
}

func (h *configHolder) set(cfg *core.Config) {
	h.mu.Lock()
	h.cfg = cfg
	h.mu.Unlock()
}

// catchUpCfg is channelCfg with catch_up on the named harnesses.
func catchUpCfg(url string, catchUp bool, bound ...string) *core.Config {
	cfg := channelCfg(url, true, bound...)
	for _, name := range bound {
		h := cfg.Harnesses[name]
		h.CatchUp = catchUp
		cfg.Harnesses[name] = h
	}
	return cfg
}

// TestServerRestartReconnectsWithoutOperatorAction covers the "Server restart"
// scenario: the stream closes, the source goes to backoff, and it returns to
// connected on its own.
func TestServerRestartReconnectsWithoutOperatorAction(t *testing.T) {
	srv := testserver.New(testserver.Options{})
	t.Cleanup(srv.Close)

	clk := newClock()
	cfg := channelCfg(srv.URL, true, "sweep")
	_, states := reconnectManager(t, &fakeRunner{}, func() *core.Config { return cfg }, clk)
	states.waitSubsequence(t, "the source connects", trigger.StateConnected)

	srv.CloseStreams()
	// connected → backoff → connected, with no operator action. Read off the
	// recorded sequence rather than by polling, because with an instant fake
	// Sleep the backoff can come and go between two polls.
	states.waitSubsequence(t, "the source reconnects on its own",
		trigger.StateConnected, trigger.StateBackoff, trigger.StateConnected)

	if slept := clk.Slept(); len(slept) == 0 || slept[0] != channel.BackoffBase {
		t.Errorf("first retry waited %v, want the %s base", slept, channel.BackoffBase)
	}
}

// TestRevokedTokenRetriesAtTheCeiling covers the "Revoked token" scenario: a
// 401 is `error`, and the retry rate is the ceiling rather than the base.
//
// A tight retry loop against an auth endpoint is its own problem, which is why
// this is a state of its own rather than ordinary backoff.
func TestRevokedTokenRetriesAtTheCeiling(t *testing.T) {
	srv := testserver.New(testserver.Options{Unauthorized: true})
	t.Cleanup(srv.Close)

	clk := newClock()
	cfg := channelCfg(srv.URL, true, "sweep")
	m, _ := reconnectManager(t, &fakeRunner{}, func() *core.Config { return cfg }, clk)
	waitState(t, m, "channel.sb", trigger.StateError)

	waitFor(t, 5*time.Second, "the retry is scheduled", func() bool { return len(clk.Slept()) > 0 })
	for i, d := range clk.Slept() {
		if d < channel.BackoffCeiling {
			t.Fatalf("retry %d waited %s, want at least the %s ceiling", i+1, d, channel.BackoffCeiling)
		}
	}
}

// TestDaemonStartFiresOneCatchUp covers the "Daemon restart" scenario: the
// first connect after start runs one catch_up firing per bound catch_up
// harness, carrying the source and no event.
func TestDaemonStartFiresOneCatchUp(t *testing.T) {
	srv := testserver.New(testserver.Options{})
	t.Cleanup(srv.Close)

	clk := newClock()
	r := &fakeRunner{}
	cfg := catchUpCfg(srv.URL, true, "one", "two")
	// A third harness binds the source but did NOT ask for catch-up, so a
	// blanket "fire everything bound" would fail here.
	cfg.Harnesses["no-catchup"] = core.Harness{Name: "no-catchup", Triggers: []string{"channel.sb"}}
	cfg.HarnessOrder = append(cfg.HarnessOrder, "no-catchup")

	m, _ := reconnectManager(t, r, func() *core.Config { return cfg }, clk)
	waitState(t, m, "channel.sb", trigger.StateConnected)

	waitFor(t, 5*time.Second, "both catch_up harnesses fire", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return len(r.calls) == 2
	})
	r.mu.Lock()
	calls := append([]call(nil), r.calls...)
	r.mu.Unlock()
	for _, c := range calls {
		if c.name == "no-catchup" {
			t.Error("a harness without catch_up got a catch-up run")
		}
		if c.req.Trigger != supervisor.TriggerCatchUp {
			t.Errorf("%s: trigger = %q, want catch_up", c.name, c.req.Trigger)
		}
		if c.req.Source != "channel.sb" {
			t.Errorf("%s: source = %q, want the source that connected", c.name, c.req.Source)
		}
		// No event file: there is no event. The point is that something MAY
		// have been missed, and only the agent can find out what.
		if c.req.Event != nil {
			t.Errorf("%s: a catch-up run carries an event", c.name)
		}
	}
}

// TestShortBlipDoesNotCatchUp and TestLongOutageCatchesUp are the two halves
// of the LateGrace threshold, driven by the fake clock.
func TestShortBlipDoesNotCatchUp(t *testing.T) {
	srv := testserver.New(testserver.Options{})
	t.Cleanup(srv.Close)

	clk := newClock()
	r := &fakeRunner{}
	cfg := catchUpCfg(srv.URL, true, "one")
	m, states := reconnectManager(t, r, func() *core.Config { return cfg }, clk)
	waitState(t, m, "channel.sb", trigger.StateConnected)
	waitFor(t, 5*time.Second, "the start catch-up fires", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return len(r.calls) == 1
	})

	// A 10-second outage: the server's own re-rings cover it.
	clk.onWait = func(time.Duration) { clk.Advance(10 * time.Second) }
	srv.CloseStreams()
	states.waitSubsequence(t, "the source reconnects",
		trigger.StateConnected, trigger.StateBackoff, trigger.StateConnected)

	// Give a catch-up that should not happen every chance to.
	time.Sleep(100 * time.Millisecond)
	r.mu.Lock()
	n := len(r.calls)
	r.mu.Unlock()
	if n != 1 {
		t.Errorf("a 10-second blip produced %d runs, want only the start catch-up", n)
	}
}

func TestLongOutageCatchesUp(t *testing.T) {
	srv := testserver.New(testserver.Options{})
	t.Cleanup(srv.Close)

	clk := newClock()
	r := &fakeRunner{}
	cfg := catchUpCfg(srv.URL, true, "one")
	m, states := reconnectManager(t, r, func() *core.Config { return cfg }, clk)
	waitState(t, m, "channel.sb", trigger.StateConnected)
	waitFor(t, 5*time.Second, "the start catch-up fires", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return len(r.calls) == 1
	})

	// 20 minutes down: past LateGrace, so the server's re-rings cannot have
	// covered it.
	clk.onWait = func(time.Duration) { clk.Advance(20 * time.Minute) }
	srv.CloseStreams()
	states.waitSubsequence(t, "the source reconnects",
		trigger.StateConnected, trigger.StateBackoff, trigger.StateConnected)

	waitFor(t, 5*time.Second, "the outage catch-up fires", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return len(r.calls) == 2
	})
	// Exactly one per bound harness, not one per reconnect attempt.
	time.Sleep(100 * time.Millisecond)
	r.mu.Lock()
	n := len(r.calls)
	r.mu.Unlock()
	if n != 2 {
		t.Errorf("a 20-minute outage produced %d runs, want the start catch-up plus exactly one more", n)
	}
}

// TestNoChangeRewriteHoldsTheStream is the regression guard this whole file is
// shaped around.
//
// It holds a DELIVERING stream across several reload cycles and asserts the
// fake server saw exactly ONE initialize. That is the Crush watchdog shape:
// treating a periodic signal as a reason to rebuild turned into a once-a-
// minute loop on hosts that were working fine. Here the periodic signal is
// chezmoi rewriting harness.toml byte-identically on a timer.
//
// A log line would not catch it — the daemon would happily log "connected"
// once a minute — so the assertion is on the server's own request count.
func TestNoChangeRewriteHoldsTheStream(t *testing.T) {
	srv := testserver.New(testserver.Options{})
	t.Cleanup(srv.Close)

	clk := newClock()
	r := &fakeRunner{}
	held := holding(channelCfg(srv.URL, true, "one"))
	m, _ := reconnectManager(t, r, held.get, clk)
	waitState(t, m, "channel.sb", trigger.StateConnected)

	sessionBefore := initializes(srv)
	for i := 0; i < 5; i++ {
		// A byte-identical rewrite: a NEW *core.Config with the same values,
		// which is what config.Load produces every time.
		next := channelCfg(srv.URL, true, "one")
		held.set(next)
		m.Reconcile(next)

		// The stream is still delivering across every cycle.
		srv.PushNotification(`{"content":"still here"}`)
		want := i + 1
		waitFor(t, 5*time.Second, "the doorbell still fires", func() bool {
			r.mu.Lock()
			defer r.mu.Unlock()
			return len(r.calls) == want
		})
	}

	if got := initializes(srv); got != sessionBefore {
		t.Errorf("a no-change reload re-initialized the session: %d initializes, want %d", got, sessionBefore)
	}
	if st, _ := m.StatusOf("channel.sb"); st.State != trigger.StateConnected {
		t.Errorf("state after the rewrites = %q, want connected throughout", st.State)
	}
}

// TestChangingTheBoundSetKeepsTheStream: a reload that adds a harness to a
// source's `triggers` must apply from the next event WITHOUT touching the
// stream. The bound set is re-read per firing precisely so this is possible.
func TestChangingTheBoundSetKeepsTheStream(t *testing.T) {
	srv := testserver.New(testserver.Options{})
	t.Cleanup(srv.Close)

	clk := newClock()
	r := &fakeRunner{}
	held := holding(channelCfg(srv.URL, true, "one"))
	m, _ := reconnectManager(t, r, held.get, clk)
	waitState(t, m, "channel.sb", trigger.StateConnected)
	before := initializes(srv)

	next := channelCfg(srv.URL, true, "one", "two")
	held.set(next)
	m.Reconcile(next)

	srv.PushNotification(`{"content":"x"}`)
	waitFor(t, 5*time.Second, "both harnesses fire", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return len(r.calls) == 2
	})
	if got := initializes(srv); got != before {
		t.Errorf("adding a bound harness re-initialized the session: %d initializes, want %d", got, before)
	}
}

// TestTokenRotationReconnectsWithoutACatchUp covers the "Token rotation"
// scenario, and the trap inside it: the reconnect is real, but it is NOT an
// outage, so it must not fire a catch-up. Without the session seed every
// rotation would run every bound catch_up harness.
func TestTokenRotationReconnectsWithoutACatchUp(t *testing.T) {
	srv := testserver.New(testserver.Options{})
	t.Cleanup(srv.Close)

	clk := newClock()
	r := &fakeRunner{}
	withToken := func(tok string) *core.Config {
		c := catchUpCfg(srv.URL, true, "one")
		src := c.Channels["sb"]
		src.Headers = map[string]core.Secret{"Authorization": core.Secret("Bearer " + tok)}
		c.Channels["sb"] = src
		return c
	}
	held := holding(withToken("old"))
	m, states := reconnectManager(t, r, held.get, clk)
	waitState(t, m, "channel.sb", trigger.StateConnected)
	waitFor(t, 5*time.Second, "the start catch-up fires", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return len(r.calls) == 1
	})
	before := initializes(srv)

	rotated := withToken("new")
	held.set(rotated)
	m.Reconcile(rotated)
	states.waitSubsequence(t, "the replacement session connects",
		trigger.StateConnected, trigger.StateConnecting, trigger.StateConnected)

	waitFor(t, 5*time.Second, "the session is rebuilt", func() bool { return initializes(srv) > before })
	// The new credential is on the wire.
	found := false
	for _, req := range srv.Requests() {
		if req.Header.Get("Authorization") == "Bearer new" {
			found = true
		}
	}
	if !found {
		t.Error("the rotated token never reached the server")
	}

	// And no catch-up: a reload-caused reconnect is not an outage.
	time.Sleep(100 * time.Millisecond)
	r.mu.Lock()
	n := len(r.calls)
	r.mu.Unlock()
	if n != 1 {
		t.Errorf("a token rotation produced %d runs, want only the start catch-up", n)
	}
}

// TestReconcileEndsRemovedDisabledAndUnboundSources covers the three ways a
// source stops having a session.
func TestReconcileEndsRemovedDisabledAndUnboundSources(t *testing.T) {
	cases := []struct {
		name  string
		after func(url string) *core.Config
		want  trigger.SourceState
	}{
		{"removed", func(string) *core.Config {
			return &core.Config{Harnesses: map[string]core.Harness{}}
		}, ""},
		{"disabled", func(url string) *core.Config { return channelCfg(url, false, "one") }, trigger.StateDisabled},
		{"unbound", func(url string) *core.Config { return channelCfg(url, true) }, trigger.StateUnbound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := testserver.New(testserver.Options{})
			t.Cleanup(srv.Close)

			clk := newClock()
			held := holding(channelCfg(srv.URL, true, "one"))
			m, _ := reconnectManager(t, &fakeRunner{}, held.get, clk)
			waitState(t, m, "channel.sb", trigger.StateConnected)

			next := tc.after(srv.URL)
			held.set(next)
			m.Reconcile(next)

			if tc.want == "" {
				if _, ok := m.StatusOf("channel.sb"); ok {
					t.Error("a removed source is still tracked")
				}
			} else {
				waitState(t, m, "channel.sb", tc.want)
			}
			// The session is gone: a DELETE went out, and a doorbell pushed
			// afterwards reaches nobody.
			saw := false
			for _, method := range srv.HTTPMethods() {
				if method == "DELETE" {
					saw = true
				}
			}
			if !saw {
				t.Error("the ended session did not send DELETE")
			}
		})
	}
}

// TestReconcileOnAClosedManagerDoesNothing: a reload racing shutdown must not
// resurrect a session the Close just ended.
func TestReconcileOnAClosedManagerDoesNothing(t *testing.T) {
	srv := testserver.New(testserver.Options{})
	t.Cleanup(srv.Close)

	clk := newClock()
	cfg := channelCfg(srv.URL, true, "one")
	m := New(Options{
		Runner: &fakeRunner{},
		Config: func() *core.Config { return cfg },
		Log:    log.New(discard{}),
		Now:    clk.Now, Sleep: clk.Sleep, Rand: func() float64 { return 0 },
	})
	m.Start(context.Background())
	waitState(t, m, "channel.sb", trigger.StateConnected)
	m.Close()

	before := len(srv.Requests())
	m.Reconcile(channelCfg(srv.URL, true, "one"))
	time.Sleep(100 * time.Millisecond)
	if got := len(srv.Requests()); got != before {
		t.Errorf("Reconcile after Close sent %d more requests", got-before)
	}
}

// initializes counts the initialize calls the server has seen — the number a
// no-change reload must not move.
func initializes(srv *testserver.Server) int {
	n := 0
	for _, m := range srv.RPCMethods() {
		if m == "initialize" {
			n++
		}
	}
	return n
}

// TestExpiredSessionReinitializesAtOnce is a regression test for a defect a
// LIVE run against a real Switchboard surfaced, and which no test here had
// caught.
//
// The sequence: Switchboard was stopped for 90 seconds, the daemon escalated
// its backoff to a two-minute delay against a refused connection, Switchboard
// came back, and the next attempt got the 404 that means "your session id is
// gone, re-initialize". The daemon then sat idle for two more minutes — with
// the server up the whole time — because it waited out a delay earned by a
// completely different failure.
//
// REQ "Channel Reconnection" says a 404 on a request carrying the session id
// SHALL re-initialize. Waiting out an unrelated backoff first is not that: the
// server answered, so there is no outage left to back off from.
//
// The fast path is spent once per streak, so a server that 404s every session
// id cannot spin — and a stream that opens in between earns it back.
func TestExpiredSessionReinitializesAtOnce(t *testing.T) {
	srv := testserver.New(testserver.Options{})
	t.Cleanup(srv.Close)

	clk := newClock()
	held := holding(channelCfg(srv.URL, true, "one"))
	_, states := reconnectManager(t, &fakeRunner{}, held.get, clk)
	states.waitSubsequence(t, "the source connects", trigger.StateConnected)

	// Escalate the backoff the way a real outage does, then expire the
	// session — the shape of a server restart.
	srv.ExpireSessions()
	srv.CloseStreams()

	states.waitSubsequence(t, "the source reconnects",
		trigger.StateConnected, trigger.StateBackoff, trigger.StateConnected)

	// The re-initialize must not have waited: every delay before the
	// reconnect is either the stream-closed backoff or the zero the expiry
	// earns, and at least one zero must be there.
	slept := clk.Slept()
	sawImmediate := false
	for _, d := range slept {
		if d == 0 {
			sawImmediate = true
		}
	}
	if !sawImmediate {
		t.Errorf("the expired session waited out a backoff instead of re-initializing at once: delays %v", slept)
	}
	if got := initializes(srv); got < 2 {
		t.Errorf("the session was not re-initialized: %d initializes", got)
	}
}

// TestRepeatedExpiryFallsBackToBackoff is the other half: the fast path is
// spent once, so a server that answers 404 to every session id cannot make the
// daemon spin.
func TestRepeatedExpiryFallsBackToBackoff(t *testing.T) {
	srv := testserver.New(testserver.Options{ExpireEveryStream: true})
	t.Cleanup(srv.Close)

	clk := newClock()
	held := holding(channelCfg(srv.URL, true, "one"))
	_, states := reconnectManager(t, &fakeRunner{}, held.get, clk)
	states.waitSubsequence(t, "the source keeps failing",
		trigger.StateBackoff, trigger.StateConnecting, trigger.StateBackoff)

	waitFor(t, 5*time.Second, "several attempts have been made", func() bool {
		return len(clk.Slept()) >= 4
	})
	zeros := 0
	for _, d := range clk.Slept() {
		if d == 0 {
			zeros++
		}
	}
	if zeros > 1 {
		t.Errorf("a server expiring every session produced %d immediate retries; the fast path is spent once", zeros)
	}
}

// TestFailedInitializeIsRetriedFromScratch: a server that answers initialize
// with a session id but without the channel capability must stay `error` on
// every retry, and must never be sent the standalone GET.
//
// Client.Initialize stores the Mcp-Session-Id before it checks the
// capability, so a retry that decided "already initialized" from
// SessionID() alone skipped the check, opened the stream against a server
// that has no doorbells, and reported it `connected` — the state the
// capability check exists to prevent (Scenario "Server without the channel
// capability").
func TestFailedInitializeIsRetriedFromScratch(t *testing.T) {
	srv := testserver.New(testserver.Options{NoChannelCapability: true})
	t.Cleanup(srv.Close)

	clk := newClock()
	held := holding(channelCfg(srv.URL, true, "one"))
	_, states := reconnectManager(t, &fakeRunner{}, held.get, clk)
	waitFor(t, 5*time.Second, "several retries have run", func() bool {
		return len(clk.Slept()) >= 4 && initializes(srv) >= 4
	})

	for _, s := range states.snapshot() {
		if s == trigger.StateConnected {
			t.Fatalf("a server without the channel capability was reported connected: %v", states.snapshot())
		}
	}
	for _, method := range srv.HTTPMethods() {
		if method == "GET" {
			t.Fatal("a server without the channel capability was sent the standalone GET")
		}
	}
}

// parkedManager is reconnectManager with a Sleep that PARKS until the session
// ends, so a test can hold a source in `backoff` for as long as it likes and
// then reload it — the state a real outage leaves a session in.
func parkedManager(t *testing.T, r Runner, cfg func() *core.Config, clk *fakeClock) (*Manager, *stateLog) {
	t.Helper()
	states := &stateLog{}
	m := New(Options{
		Runner: r,
		Config: cfg,
		Log:    log.New(discard{}),
		Now:    clk.Now,
		Sleep: func(ctx context.Context, _ time.Duration) bool {
			<-ctx.Done()
			return false
		},
		Rand:    func() float64 { return 0 },
		OnState: states.add,
	})
	m.Start(context.Background())
	t.Cleanup(m.Close)
	return m, states
}

// counts returns how many runs the fake runner has been asked for, and how
// many of them were catch-ups.
func (f *fakeRunner) counts() (all, catchUps int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c.req.Trigger == supervisor.TriggerCatchUp {
			catchUps++
		}
	}
	return len(f.calls), catchUps
}

// TestRotationDuringAnOutageStillCatchesUp and TestRotationAfterABlipDoesNot
// pin the seed a reload hands a replacement session. It must be the replaced
// session's OWN history — whether it ever connected, and when it went down —
// not a guess from its status. A reload-caused reconnect is not an outage, but
// it does not erase one that was already under way: rotating a revoked token
// after hours in `error` is exactly when doorbells were missed.
func TestRotationDuringAnOutageStillCatchesUp(t *testing.T) {
	srv := testserver.New(testserver.Options{})
	t.Cleanup(srv.Close)

	clk := newClock()
	r := &fakeRunner{}
	withToken := func(tok string) *core.Config {
		c := catchUpCfg(srv.URL, true, "one")
		src := c.Channels["sb"]
		src.Headers = map[string]core.Secret{"Authorization": core.Secret("Bearer " + tok)}
		c.Channels["sb"] = src
		return c
	}
	held := holding(withToken("old"))
	m, states := parkedManager(t, r, held.get, clk)
	waitState(t, m, "channel.sb", trigger.StateConnected)
	waitFor(t, 5*time.Second, "the start catch-up fires", func() bool { _, c := r.counts(); return c == 1 })

	// A doorbell arrives, so the source has events to its name.
	srv.PushNotification(`{"content":"x"}`)
	waitFor(t, 5*time.Second, "the doorbell fires", func() bool { n, _ := r.counts(); return n == 2 })

	// The stream drops and stays down for 20 minutes.
	srv.CloseStreams()
	states.waitSubsequence(t, "the source is down", trigger.StateConnected, trigger.StateBackoff)
	clk.Advance(20 * time.Minute)

	// The operator rotates the token mid-outage.
	rotated := withToken("new")
	held.set(rotated)
	m.Reconcile(rotated)
	states.waitSubsequence(t, "the replacement connects",
		trigger.StateBackoff, trigger.StateConnected)

	waitFor(t, 5*time.Second, "the outage catch-up fires", func() bool { _, c := r.counts(); return c == 2 })
}

func TestRotationAfterABlipDoesNotCatchUp(t *testing.T) {
	srv := testserver.New(testserver.Options{})
	t.Cleanup(srv.Close)

	clk := newClock()
	r := &fakeRunner{}
	withToken := func(tok string) *core.Config {
		c := catchUpCfg(srv.URL, true, "one")
		src := c.Channels["sb"]
		src.Headers = map[string]core.Secret{"Authorization": core.Secret("Bearer " + tok)}
		c.Channels["sb"] = src
		return c
	}
	held := holding(withToken("old"))
	m, states := parkedManager(t, r, held.get, clk)
	waitState(t, m, "channel.sb", trigger.StateConnected)
	waitFor(t, 5*time.Second, "the start catch-up fires", func() bool { _, c := r.counts(); return c == 1 })

	// A 10-second blip — no doorbells, so the status carries no events —
	// and the token rotates while the source is still in backoff.
	srv.CloseStreams()
	states.waitSubsequence(t, "the source is down", trigger.StateConnected, trigger.StateBackoff)
	clk.Advance(10 * time.Second)

	rotated := withToken("new")
	held.set(rotated)
	m.Reconcile(rotated)
	states.waitSubsequence(t, "the replacement connects",
		trigger.StateBackoff, trigger.StateConnected)

	time.Sleep(100 * time.Millisecond)
	if _, c := r.counts(); c != 1 {
		t.Errorf("a rotation after a 10-second blip produced %d catch-ups, want only the start one: the replacement mistook itself for the daemon's first connect", c)
	}
}

// TestNoChangeRewriteLeavesIdleSourcesAlone extends the no-change guarantee to
// the sources with no session. A byte-identical rewrite must not report a
// state change for a disabled or unbound source, nor reset its Since: that
// notification is the trigger_source_changed seam (#476), and chezmoi's timer
// would otherwise turn it into a periodic event meaning nothing.
func TestNoChangeRewriteLeavesIdleSourcesAlone(t *testing.T) {
	srv := testserver.New(testserver.Options{})
	t.Cleanup(srv.Close)

	build := func() *core.Config {
		c := channelCfg(srv.URL, true, "one")
		c.Channels["idle"] = core.ChannelSource{Name: "idle", URL: srv.URL + "/idle", Enabled: true}
		c.Channels["off"] = core.ChannelSource{Name: "off", URL: srv.URL + "/off", Enabled: false}
		c.ChannelOrder = append(c.ChannelOrder, "idle", "off")
		return c
	}
	var (
		mu       sync.Mutex
		notified = map[string]int{}
	)
	held := holding(build())
	clk := newClock()
	m := New(Options{
		Runner: &fakeRunner{},
		Config: held.get,
		Log:    log.New(discard{}),
		Now:    clk.Now, Sleep: clk.Sleep, Rand: func() float64 { return 0 },
		OnState: func(s Status) {
			mu.Lock()
			notified[s.Source]++
			mu.Unlock()
		},
	})
	m.Start(context.Background())
	t.Cleanup(m.Close)
	waitState(t, m, "channel.sb", trigger.StateConnected)
	waitState(t, m, "channel.idle", trigger.StateUnbound)
	waitState(t, m, "channel.off", trigger.StateDisabled)
	// setStatusLocked notifies on a goroutine; let those land.
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	before := map[string]int{"channel.idle": notified["channel.idle"], "channel.off": notified["channel.off"]}
	mu.Unlock()
	idleSince, _ := m.StatusOf("channel.idle")
	offSince, _ := m.StatusOf("channel.off")

	for i := 0; i < 3; i++ {
		next := build()
		held.set(next)
		m.Reconcile(next)
	}
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	for ref, n := range before {
		if notified[ref] != n {
			t.Errorf("%s: a no-change rewrite reported %d state changes", ref, notified[ref]-n)
		}
	}
	if st, _ := m.StatusOf("channel.idle"); !st.Since.Equal(idleSince.Since) {
		t.Error("channel.idle: a no-change rewrite reset Since")
	}
	if st, _ := m.StatusOf("channel.off"); !st.Since.Equal(offSince.Since) {
		t.Error("channel.off: a no-change rewrite reset Since")
	}
}

// TestConcurrentReloadsBuildOneSession: SIGHUP, the config watcher and the
// reload control op all reach Reconcile with no lock between them. Two
// reloads of the same rotation must still produce ONE replacement session,
// seeded from the one it replaced — not a second, unseeded session started by
// whichever reload arrived while the first was waiting for the old one to end.
func TestConcurrentReloadsBuildOneSession(t *testing.T) {
	for i := 0; i < 20; i++ {
		srv := testserver.New(testserver.Options{})
		clk := newClock()
		r := &fakeRunner{}
		withToken := func(tok string) *core.Config {
			c := catchUpCfg(srv.URL, true, "one")
			src := c.Channels["sb"]
			src.Headers = map[string]core.Secret{"Authorization": core.Secret("Bearer " + tok)}
			c.Channels["sb"] = src
			return c
		}
		held := holding(withToken("old"))
		m := New(Options{
			Runner: r, Config: held.get, Log: log.New(discard{}),
			Now: clk.Now, Sleep: clk.Sleep, Rand: func() float64 { return 0 },
		})
		m.Start(context.Background())
		waitState(t, m, "channel.sb", trigger.StateConnected)
		waitFor(t, 5*time.Second, "the start catch-up fires", func() bool { _, c := r.counts(); return c == 1 })
		before := initializes(srv)

		rotated := withToken("new")
		held.set(rotated)
		var wg sync.WaitGroup
		for j := 0; j < 2; j++ {
			wg.Add(1)
			go func() { defer wg.Done(); m.Reconcile(rotated) }()
		}
		wg.Wait()
		waitState(t, m, "channel.sb", trigger.StateConnected)
		time.Sleep(20 * time.Millisecond)

		got := initializes(srv) - before
		_, c := r.counts()
		m.Close()
		srv.Close()
		if got != 1 {
			t.Fatalf("iteration %d: two concurrent reloads of one rotation built %d sessions, want 1", i, got)
		}
		if c != 1 {
			t.Fatalf("iteration %d: two concurrent reloads produced %d catch-ups, want only the start one", i, c)
		}
	}
}

// TestCatchUpPanicCostsOneHarness: a catch-up starts on the session
// goroutine, inside the stream's onOpen, so a panic starting one harness must
// be contained the way Fire contains one — costing that harness only, not the
// other catch-ups, the session, or the daemon.
func TestCatchUpPanicCostsOneHarness(t *testing.T) {
	srv := testserver.New(testserver.Options{})
	t.Cleanup(srv.Close)

	clk := newClock()
	r := &fakeRunner{panicOn: map[string]bool{"one": true}}
	cfg := catchUpCfg(srv.URL, true, "one", "two")
	m, _ := reconnectManager(t, r, func() *core.Config { return cfg }, clk)
	waitState(t, m, "channel.sb", trigger.StateConnected)
	waitFor(t, 5*time.Second, "both catch-ups were attempted", func() bool { _, c := r.counts(); return c == 2 })

	// The session survived: a doorbell still fires the harness that did not
	// panic.
	srv.PushNotification(`{"content":"x"}`)
	waitFor(t, 5*time.Second, "the doorbell still fires", func() bool { n, _ := r.counts(); return n == 4 })
}
