package source

// The channel half of the source manager, driven against a real fake
// Streamable HTTP server rather than a stub dialer.
//
// A stub would assert only that the manager calls it. What is worth testing
// here is the whole path — a doorbell on a real stream becoming a firing with
// an envelope — and the states an operator reads to tell "off" from "broken".
//
// Governing: ADR-0021; SPEC-0014 REQ "Channel Listener Session", REQ "Channel
// Notification Handling", REQ "Firing".
//
// @joestump 09/23/2026 - Introduced with the SPEC-0014 channel listener (#471).

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/log/v2"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/supervisor"
	"github.com/stump-wtf/harness/internal/trigger"
	"github.com/stump-wtf/harness/internal/trigger/channel/testserver"
)

// channelCfg builds a config with one channel source and the harnesses that
// bind it.
func channelCfg(url string, enabled bool, bound ...string) *core.Config {
	cfg := &core.Config{
		Harnesses: map[string]core.Harness{},
		Channels: map[string]core.ChannelSource{
			"sb": {Name: "sb", URL: url, Enabled: enabled},
		},
		ChannelOrder: []string{"sb"},
	}
	for _, name := range bound {
		cfg.Harnesses[name] = core.Harness{Name: name, Triggers: []string{"channel.sb"}}
		cfg.HarnessOrder = append(cfg.HarnessOrder, name)
	}
	return cfg
}

func startManager(t *testing.T, r Runner, cfg *core.Config) *Manager {
	t.Helper()
	m := New(Options{Runner: r, Config: func() *core.Config { return cfg }, Log: log.New(discard{})})
	m.Start(context.Background())
	t.Cleanup(m.Close)
	return m
}

func waitState(t *testing.T, m *Manager, ref string, want trigger.SourceState) Status {
	t.Helper()
	var got Status
	waitFor(t, 5*time.Second, "source "+ref+" reaches "+string(want), func() bool {
		st, ok := m.StatusOf(ref)
		got = st
		return ok && st.State == want
	})
	return got
}

// TestConnectedOnlyWhileTheStreamIsRead is the state the whole design turns
// on. `connected` is set when the GET stream opens and is being read — never
// when initialize merely succeeded.
func TestConnectedOnlyWhileTheStreamIsRead(t *testing.T) {
	srv := testserver.New(testserver.Options{})
	// Registered BEFORE the manager's cleanup so it runs AFTER it: cleanups
	// are LIFO, and httptest.Server.Close waits for outstanding requests —
	// which the open GET stream is. Closing the server first deadlocks.
	t.Cleanup(srv.Close)

	r := &fakeRunner{}
	m := startManager(t, r, channelCfg(srv.URL, true, "sweep"))

	st := waitState(t, m, "channel.sb", trigger.StateConnected)
	if st.Kind != core.SourceKindChannel {
		t.Errorf("kind = %q", st.Kind)
	}
	if st.Since.IsZero() {
		t.Error("the state change carries no time")
	}
}

// TestRefusedStreamIsErrorNotConnected covers "Initialized, but no stream":
// initialize succeeds, the GET is refused, and the source must NOT be
// connected. A source stuck in `connected` with no stream looks healthier
// than one that is broken, and is worse.
func TestRefusedStreamIsErrorNotConnected(t *testing.T) {
	srv := testserver.New(testserver.Options{RefuseStream: true})
	// Registered BEFORE the manager's cleanup so it runs AFTER it: cleanups
	// are LIFO, and httptest.Server.Close waits for outstanding requests —
	// which the open GET stream is. Closing the server first deadlocks.
	t.Cleanup(srv.Close)

	m := startManager(t, &fakeRunner{}, channelCfg(srv.URL, true, "sweep"))
	st := waitState(t, m, "channel.sb", trigger.StateError)
	if !strings.Contains(st.Reason, "stream") {
		t.Errorf("reason = %q, want it to name the refused stream", st.Reason)
	}
}

// TestMissingCapabilityIsError covers "Server without the channel capability":
// state error, a reason naming it, and no GET.
func TestMissingCapabilityIsError(t *testing.T) {
	srv := testserver.New(testserver.Options{NoChannelCapability: true})
	// Registered BEFORE the manager's cleanup so it runs AFTER it: cleanups
	// are LIFO, and httptest.Server.Close waits for outstanding requests —
	// which the open GET stream is. Closing the server first deadlocks.
	t.Cleanup(srv.Close)

	m := startManager(t, &fakeRunner{}, channelCfg(srv.URL, true, "sweep"))
	st := waitState(t, m, "channel.sb", trigger.StateError)
	if !strings.Contains(st.Reason, "claude/channel") {
		t.Errorf("reason = %q, want it to name the missing capability", st.Reason)
	}
	for _, method := range srv.HTTPMethods() {
		if method == "GET" {
			t.Error("a session with no channel capability opened the stream anyway")
		}
	}
}

// TestUnboundSourceOpensNothing covers the "Unbound source" scenario. Opening
// a session anyway would hold a consumer slot on the server and swallow
// doorbells nothing could act on — a channel server rings ONE session per
// endpoint.
func TestUnboundSourceOpensNothing(t *testing.T) {
	srv := testserver.New(testserver.Options{})
	// Registered BEFORE the manager's cleanup so it runs AFTER it: cleanups
	// are LIFO, and httptest.Server.Close waits for outstanding requests —
	// which the open GET stream is. Closing the server first deadlocks.
	t.Cleanup(srv.Close)

	m := startManager(t, &fakeRunner{}, channelCfg(srv.URL, true))
	st := waitState(t, m, "channel.sb", trigger.StateUnbound)
	if st.Reason != "" {
		t.Errorf("reason = %q, want none: unbound is not an error", st.Reason)
	}
	// Give a session that should not exist every chance to appear.
	time.Sleep(150 * time.Millisecond)
	if reqs := srv.Requests(); len(reqs) != 0 {
		t.Errorf("an unbound source sent %d requests: %v", len(reqs), srv.HTTPMethods())
	}
}

// TestDisabledSourceOpensNothing: `enabled = false` keeps the table and
// connects nothing.
func TestDisabledSourceOpensNothing(t *testing.T) {
	srv := testserver.New(testserver.Options{})
	// Registered BEFORE the manager's cleanup so it runs AFTER it: cleanups
	// are LIFO, and httptest.Server.Close waits for outstanding requests —
	// which the open GET stream is. Closing the server first deadlocks.
	t.Cleanup(srv.Close)

	m := startManager(t, &fakeRunner{}, channelCfg(srv.URL, false, "sweep"))
	waitState(t, m, "channel.sb", trigger.StateDisabled)
	time.Sleep(150 * time.Millisecond)
	if reqs := srv.Requests(); len(reqs) != 0 {
		t.Errorf("a disabled source sent %d requests", len(reqs))
	}
}

// TestDoorbellFiresEveryBoundHarness is the end-to-end path: a notification on
// a real stream becomes one firing per bound harness, with trigger `channel`
// and an envelope carrying the payload verbatim.
func TestDoorbellFiresEveryBoundHarness(t *testing.T) {
	srv := testserver.New(testserver.Options{})
	// Registered BEFORE the manager's cleanup so it runs AFTER it: cleanups
	// are LIFO, and httptest.Server.Close waits for outstanding requests —
	// which the open GET stream is. Closing the server first deadlocks.
	t.Cleanup(srv.Close)

	r := &fakeRunner{}
	m := startManager(t, r, channelCfg(srv.URL, true, "one", "two"))
	waitState(t, m, "channel.sb", trigger.StateConnected)

	srv.PushNotification(`{"content":"PR #9 opened","meta":{"todo_id":"t1"}}`)

	waitFor(t, 5*time.Second, "both harnesses fire", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return len(r.calls) == 2
	})

	r.mu.Lock()
	calls := append([]call(nil), r.calls...)
	r.mu.Unlock()
	if calls[0].name != "one" || calls[1].name != "two" {
		t.Errorf("fan-out order = %s, %s; want config order", calls[0].name, calls[1].name)
	}
	for _, c := range calls {
		if c.req.Trigger != supervisor.TriggerChannel {
			t.Errorf("%s: trigger = %q, want channel", c.name, c.req.Trigger)
		}
		if c.req.Source != "channel.sb" {
			t.Errorf("%s: source = %q", c.name, c.req.Source)
		}
		ev := c.req.Event
		if ev == nil || ev.Channel == nil {
			t.Fatalf("%s: no channel envelope", c.name)
		}
		if ev.Channel.Content != "PR #9 opened" || ev.Channel.Meta["todo_id"] != "t1" {
			t.Errorf("%s: envelope = %+v, want the payload verbatim", c.name, ev.Channel)
		}
		// A channel doorbell carries no delivery id of its own, so the daemon
		// makes one — without it two firings would be indistinguishable in a
		// run history.
		if ev.EventID == "" {
			t.Errorf("%s: the envelope has no event id", c.name)
		}
		if err := ev.Validate(); err != nil {
			t.Errorf("%s: the envelope does not validate: %v", c.name, err)
		}
	}
	// Both harnesses got the SAME event id: it is one event, fanned out.
	if calls[0].req.Event.EventID != calls[1].req.Event.EventID {
		t.Error("the two firings carry different event ids; they are one event")
	}

	st, _ := m.StatusOf("channel.sb")
	if st.Events != 1 || st.LastEvent.IsZero() {
		t.Errorf("status = %+v, want one event counted", st)
	}
}

// TestInvalidDoorbellIsCountedAndFiresNothing covers the "Malformed
// notification" scenario at the manager level: counted `invalid`, and no
// firing.
func TestInvalidDoorbellIsCountedAndFiresNothing(t *testing.T) {
	srv := testserver.New(testserver.Options{})
	// Registered BEFORE the manager's cleanup so it runs AFTER it: cleanups
	// are LIFO, and httptest.Server.Close waits for outstanding requests —
	// which the open GET stream is. Closing the server first deadlocks.
	t.Cleanup(srv.Close)

	r := &fakeRunner{}
	m := startManager(t, r, channelCfg(srv.URL, true, "one"))
	waitState(t, m, "channel.sb", trigger.StateConnected)

	srv.PushNotification(`{"content":"ok","meta":{"todo_id":7}}`)
	waitFor(t, 5*time.Second, "the invalid doorbell is counted", func() bool {
		st, _ := m.StatusOf("channel.sb")
		return st.Invalid == 1
	})

	r.mu.Lock()
	n := len(r.calls)
	r.mu.Unlock()
	if n != 0 {
		t.Errorf("an invalid doorbell fired %d harnesses", n)
	}
	// And the stream survived: a valid doorbell after it still fires.
	srv.PushNotification(`{"content":"after"}`)
	waitFor(t, 5*time.Second, "the next doorbell fires", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return len(r.calls) == 1
	})
	st, _ := m.StatusOf("channel.sb")
	if st.Events != 1 || st.Invalid != 1 {
		t.Errorf("status = %+v, want 1 event and 1 invalid", st)
	}
}

// TestCloseEndsTheSession: shutdown cancels the stream, sends DELETE, and
// returns only once the session goroutine is done — so nothing is left
// reading a socket after the daemon thinks it has stopped.
func TestCloseEndsTheSession(t *testing.T) {
	srv := testserver.New(testserver.Options{})
	// Registered BEFORE the manager's cleanup so it runs AFTER it: cleanups
	// are LIFO, and httptest.Server.Close waits for outstanding requests —
	// which the open GET stream is. Closing the server first deadlocks.
	t.Cleanup(srv.Close)

	m := New(Options{
		Runner: &fakeRunner{},
		Config: func() *core.Config { return channelCfg(srv.URL, true, "one") },
		Log:    log.New(discard{}),
	})
	m.Start(context.Background())
	waitState(t, m, "channel.sb", trigger.StateConnected)

	done := make(chan struct{})
	go func() { m.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return: a session goroutine is still running")
	}

	saw := false
	for _, method := range srv.HTTPMethods() {
		if method == "DELETE" {
			saw = true
		}
	}
	if !saw {
		t.Error("Close did not end the session with DELETE")
	}
}

// TestServerClosingTheStreamIsBackoffNotConnected: a clean end of stream is
// not success. Leaving the source `connected` would tell an operator
// doorbells are arriving when nothing is listening.
func TestServerClosingTheStreamIsBackoffNotConnected(t *testing.T) {
	srv := testserver.New(testserver.Options{})
	// Registered BEFORE the manager's cleanup so it runs AFTER it: cleanups
	// are LIFO, and httptest.Server.Close waits for outstanding requests —
	// which the open GET stream is. Closing the server first deadlocks.
	t.Cleanup(srv.Close)

	m := startManager(t, &fakeRunner{}, channelCfg(srv.URL, true, "one"))
	waitState(t, m, "channel.sb", trigger.StateConnected)

	srv.CloseStreams()
	st := waitState(t, m, "channel.sb", trigger.StateBackoff)
	if !strings.Contains(st.Reason, "closed") {
		t.Errorf("reason = %q, want it to say the server closed the stream", st.Reason)
	}
}

// TestNeverReportsConnectedWithoutAStream watches the state SEQUENCE, not the
// final state, and is the reason Options.OnState exists.
//
// A snapshot cannot tell these apart: a source that briefly reported
// `connected` before settling on `error` looks identical to one that never
// did. And "briefly connected" is not cosmetic — #476 will surface it to an
// operator and #480 will count it, so a source that flickers through
// `connected` on its way to a failure would report doorbells as reachable
// when nothing was ever listening.
func TestNeverReportsConnectedWithoutAStream(t *testing.T) {
	cases := []struct {
		name string
		opts testserver.Options
	}{
		{"the server refuses the stream", testserver.Options{RefuseStream: true}},
		{"the server has no channel capability", testserver.Options{NoChannelCapability: true}},
		{"the server rejects the credentials", testserver.Options{Unauthorized: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := testserver.New(tc.opts)
			t.Cleanup(srv.Close)

			var (
				mu     sync.Mutex
				states []trigger.SourceState
			)
			cfg := channelCfg(srv.URL, true, "sweep")
			m := New(Options{
				Runner: &fakeRunner{},
				Config: func() *core.Config { return cfg },
				Log:    log.New(discard{}),
				OnState: func(s Status) {
					mu.Lock()
					states = append(states, s.State)
					mu.Unlock()
				},
			})
			m.Start(context.Background())
			t.Cleanup(m.Close)

			waitState(t, m, "channel.sb", trigger.StateError)

			mu.Lock()
			seen := append([]trigger.SourceState(nil), states...)
			mu.Unlock()
			for _, s := range seen {
				if s == trigger.StateConnected {
					t.Errorf("the source passed through connected on its way to error: %v", seen)
				}
			}
			// The control: the hook fired at all, so the absence above is an
			// absence of `connected` rather than an absence of observations.
			if len(seen) == 0 {
				t.Fatal("no state changes were observed, so the check above proves nothing")
			}
		})
	}

	// And the positive control: a working server DOES pass through connected,
	// so the assertion is not passing because nothing ever reports it.
	srv := testserver.New(testserver.Options{})
	t.Cleanup(srv.Close)
	var (
		mu     sync.Mutex
		states []trigger.SourceState
	)
	cfg := channelCfg(srv.URL, true, "sweep")
	m := New(Options{
		Runner: &fakeRunner{},
		Config: func() *core.Config { return cfg },
		Log:    log.New(discard{}),
		OnState: func(s Status) {
			mu.Lock()
			states = append(states, s.State)
			mu.Unlock()
		},
	})
	m.Start(context.Background())
	t.Cleanup(m.Close)
	waitState(t, m, "channel.sb", trigger.StateConnected)

	// OnState runs after the state is recorded, off the lock, so StatusOf can
	// show `connected` a moment before the hook has seen it. Wait for the
	// hook rather than reading it once.
	var seen []trigger.SourceState
	deadline := time.Now().Add(5 * time.Second)
	found := false
	for !found && time.Now().Before(deadline) {
		mu.Lock()
		seen = append([]trigger.SourceState(nil), states...)
		mu.Unlock()
		for _, s := range seen {
			if s == trigger.StateConnected {
				found = true
			}
		}
		if !found {
			time.Sleep(5 * time.Millisecond)
		}
	}
	if !found {
		t.Errorf("a healthy source never reported connected: %v", seen)
	}
}

// TestStateChangesArriveInOrder: OnState is the seam #476's
// trigger_source_changed hangs off, so a consumer takes the LAST change it
// saw as the current state. A slow handler for the first change must not let
// a later one overtake it.
func TestStateChangesArriveInOrder(t *testing.T) {
	srv := testserver.New(testserver.Options{})
	t.Cleanup(srv.Close)

	var (
		mu     sync.Mutex
		states []trigger.SourceState
	)
	cfg := channelCfg(srv.URL, true, "sweep")
	m := New(Options{
		Runner: &fakeRunner{},
		Config: func() *core.Config { return cfg },
		Log:    log.New(discard{}),
		OnState: func(s Status) {
			if s.State == trigger.StateConnecting {
				// A handler that takes a while, as one that writes to a
				// socket may.
				time.Sleep(200 * time.Millisecond)
			}
			mu.Lock()
			states = append(states, s.State)
			mu.Unlock()
		},
	})
	m.Start(context.Background())
	t.Cleanup(m.Close)

	var seen []trigger.SourceState
	waitFor(t, 5*time.Second, "two state changes observed", func() bool {
		mu.Lock()
		defer mu.Unlock()
		seen = append([]trigger.SourceState(nil), states...)
		return len(seen) >= 2
	})
	if seen[0] != trigger.StateConnecting || seen[1] != trigger.StateConnected {
		t.Errorf("state changes arrived as %v, want [connecting connected]", seen)
	}
}
