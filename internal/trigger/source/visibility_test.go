package source

// What the manager reports about itself: credential-free reasons, ordered
// state changes, counters that outlive a reload, and a down-since that marks
// when an outage began rather than its latest cycle.
//
// Governing: SPEC-0014 REQ "Trigger Visibility", REQ "Error Handling
// Standards", REQ "Trigger Metrics".

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/log/v2"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/trigger"
)

func TestScrubReasonRemovesQueryAndHeaderValues(t *testing.T) {
	src := core.ChannelSource{
		Name:    "sb",
		URL:     "https://sb.example/mcp/x?token=Zai5quoh&v=2",
		Headers: map[string]core.Secret{"Authorization": core.Secret("Bearer ohx3Chah")},
	}
	// What Go's HTTP client actually says, plus a hypothetical echo of a
	// header value — the two ways a credential could reach a reason.
	raw := `channel sb: initialize: Post "https://sb.example/mcp/x?token=Zai5quoh&v=2": dial tcp: refused; server said Bearer ohx3Chah`
	got := scrubReason(raw, src)
	for _, secret := range []string{"Zai5quoh", "ohx3Chah", "token="} {
		if strings.Contains(got, secret) {
			t.Errorf("scrubbed reason still carries %q: %s", secret, got)
		}
	}
	// It is still a useful error: the endpoint and the cause survive.
	if !strings.Contains(got, "https://sb.example/mcp/x") || !strings.Contains(got, "refused") {
		t.Errorf("scrubbing removed the useful part: %s", got)
	}

	if got := StripQuery("https://sb.example/mcp?t=1#frag"); got != "https://sb.example/mcp" {
		t.Errorf("StripQuery = %q", got)
	}
	if got := StripQuery("::bad?secret"); strings.Contains(got, "secret") {
		t.Errorf("StripQuery on an unparseable URL kept the query: %q", got)
	}
}

// TestWebhookStateChangesArriveInOrder: `no_listener` at Start and `listening`
// a moment later, when the listener binds, reach a subscriber in that order
// every time. Each change used to be delivered on a goroutine of its own, so a
// subscriber could end on `no_listener` for a source that was being served.
func TestWebhookStateChangesArriveInOrder(t *testing.T) {
	for i := 0; i < 50; i++ {
		var (
			mu   sync.Mutex
			seen []trigger.SourceState
		)
		cfg := webhookConfig(map[string]bool{"ci": true}, "ci")
		m := New(Options{
			Runner: &fakeRunner{},
			Config: func() *core.Config { return cfg },
			Log:    log.New(discard{}),
			OnState: func(s Status) {
				mu.Lock()
				seen = append(seen, s.State)
				mu.Unlock()
			},
		})
		m.Start(context.Background())
		m.SetWebhookListening(true)
		waitFor(t, 5*time.Second, "two webhook state changes", func() bool {
			mu.Lock()
			defer mu.Unlock()
			return len(seen) >= 2
		})
		mu.Lock()
		got := append([]trigger.SourceState(nil), seen...)
		mu.Unlock()
		m.Close()
		if got[0] != trigger.StateNoListener || got[1] != trigger.StateListening {
			t.Fatalf("iteration %d: changes arrived as %v, want [no_listener listening]", i, got)
		}
	}
}

// TestCountersSurviveAReload: a reload that replaces a source's record — here,
// disabling and re-enabling it — keeps its counters and last event. REQ
// "Trigger Visibility" counts "since daemon start".
func TestCountersSurviveAReload(t *testing.T) {
	cfg := webhookConfig(map[string]bool{"ci": true}, "ci")
	current := cfg
	m := New(Options{Runner: &fakeRunner{}, Config: func() *core.Config { return current }, Log: log.New(discard{})})
	m.Start(context.Background())
	t.Cleanup(m.Close)

	m.Fire(webhookEvent("webhook.ci", "d-1"))
	m.NoteOutcome("webhook.ci", trigger.OutcomeUnauthorized)

	current = webhookConfig(map[string]bool{"ci": false}, "ci")
	m.Reconcile(current)
	current = cfg
	m.Reconcile(current)

	st, ok := m.StatusOf("webhook.ci")
	if !ok {
		t.Fatal("webhook.ci is not reported")
	}
	if st.Counts[trigger.OutcomeFired] != 1 || st.Counts[trigger.OutcomeUnauthorized] != 1 || st.LastEvent.IsZero() {
		t.Errorf("after two reloads: counts=%v last_event=%v, want one fired and one unauthorized", st.Counts, st.LastEvent)
	}
	for _, o := range trigger.Outcomes {
		if _, ok := st.Counts[o]; !ok {
			t.Errorf("counts lack %q, zeros included: %v", o, st.Counts)
		}
	}
}

// TestDownSinceMarksTheStartOfAnOutage: a source that cycles backoff →
// connecting → backoff keeps the time it left connected, while Since moves
// with every cycle. That time is REQ "Trigger Visibility"'s "when it left
// connected".
func TestDownSinceMarksTheStartOfAnOutage(t *testing.T) {
	m := New(Options{Runner: &fakeRunner{}, Log: log.New(discard{})})
	m.mu.Lock()
	m.sources["channel.sb"] = &sourceState{status: Status{Source: "channel.sb", Kind: "channel", State: trigger.StateConnecting}}
	m.mu.Unlock()

	m.setState("channel.sb", trigger.StateConnected, "")
	if st, _ := m.StatusOf("channel.sb"); !st.DownSince.IsZero() {
		t.Fatalf("connected, but down since %v", st.DownSince)
	}
	m.setState("channel.sb", trigger.StateBackoff, "refused")
	first, _ := m.StatusOf("channel.sb")
	if first.DownSince.IsZero() {
		t.Fatal("left connected, but no down-since")
	}
	time.Sleep(5 * time.Millisecond)
	m.setState("channel.sb", trigger.StateConnecting, "")
	m.setState("channel.sb", trigger.StateBackoff, "refused again")
	later, _ := m.StatusOf("channel.sb")
	if !later.DownSince.Equal(first.DownSince) {
		t.Errorf("down-since moved with the cycle: %v → %v", first.DownSince, later.DownSince)
	}
	if !later.Since.After(first.Since) {
		t.Errorf("since did not move: %v → %v (so the check above proves nothing)", first.Since, later.Since)
	}
}
