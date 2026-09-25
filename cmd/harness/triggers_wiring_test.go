package main

// The trigger-visibility wiring tests.
//
// They drive what THE DAEMON builds — startDaemonSources, beginDaemonWebhooks,
// daemon.NewServer with its Triggers, and wireTriggerVisibility — and read the
// result the way an operator does: over the socket, through the real client,
// rendered by the real verbs. A test that assembled the projection itself
// would pass against a daemon that never connected the source manager to the
// protocol server (#315).
//
// Governing: ADR-0021; SPEC-0014 REQ "Trigger Visibility", REQ "Credential
// Resolution", REQ "Error Handling Standards", REQ "Webhook Listener".
//
// @joestump 09/24/2026 - Introduced for stump.wtf/harness#476.

import (
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/attach"
	"github.com/stump-wtf/harness/internal/buildinfo"
	"github.com/stump-wtf/harness/internal/client"
	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/daemon"
	"github.com/stump-wtf/harness/internal/protocol"
	"github.com/stump-wtf/harness/internal/schedfmt"
	"github.com/stump-wtf/harness/internal/supervisor"
	"github.com/stump-wtf/harness/internal/trigger"
	"github.com/stump-wtf/harness/internal/trigger/channel/testserver"
	"github.com/stump-wtf/harness/internal/trigger/source"
)

// Sentinels that must never reach a reply, a rendering or an event: a channel
// header value, a query-string credential on a channel URL, and one on a URL
// the daemon cannot reach — whose failure message is where Go's HTTP client
// quotes the URL back.
const (
	visHeaderSentinel = "hdr-Oofae3ee-value"
	visQuerySentinel  = "qry-Ieph4ahb-token"
	visDeadSentinel   = "dead-Xoo9ieVo-token"
)

// visibilityDaemon is the daemon's own wiring over cfg, served on a socket.
type visibilityDaemon struct {
	mgr      *supervisor.Manager
	sources  *source.Manager
	webhooks *daemonWebhooks
	srv      *daemon.Server
	socket   string
}

func startVisibilityDaemon(t *testing.T, cfg *core.Config) *visibilityDaemon {
	t.Helper()
	mgr := newWiringManager(t, cfg)
	sources := startDaemonSources(mgr, nil)
	t.Cleanup(sources.Close)
	wireSourceReload(mgr, sources)
	webhooks := beginDaemonWebhooks(mgr, sources, "127.0.0.1:0")
	wireWebhookReload(mgr, webhooks)

	socket := filepath.Join(shortSockDir(t), "d.sock")
	srv := daemon.NewServer(daemon.Options{
		Manager:    mgr,
		Registry:   attach.NewRegistry(100),
		Triggers:   sources,
		SocketPath: socket,
		Version:    buildinfo.Version,
	})
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	t.Cleanup(srv.Close)
	wireTriggerVisibility(srv, sources, webhooks)
	webhooks.serve()
	t.Cleanup(webhooks.shutdown)
	return &visibilityDaemon{mgr: mgr, sources: sources, webhooks: webhooks, srv: srv, socket: socket}
}

func (d *visibilityDaemon) dial(t *testing.T, wants []string) *client.Client {
	t.Helper()
	c, err := client.Dial(d.socket, buildinfo.Version, wants)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// visibilityConfig: a live channel (the fake server, with a header and a
// query credential), a dead channel (a closed port, with a query credential),
// and a bearer webhook, bound by one triggered-only harness that runs
// nothing interesting.
func visibilityConfig(t *testing.T, liveURL string) *core.Config {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := ln.Addr().String()
	_ = ln.Close()

	h := core.Harness{
		Name: "pr-review", Adapter: "generic", Backend: core.BackendNative,
		Args: []string{"-c", "true"}, Restart: core.RestartNo,
		Triggers:  []string{"channel.sb", "channel.dead", "webhook.ci"},
		Timeout:   core.DefaultRunTimeout,
		OnOverlap: core.OverlapQueue,
		KeepRuns:  core.DefaultKeepRuns,
	}
	return &core.Config{
		Harnesses:    map[string]core.Harness{h.Name: h},
		Profiles:     map[string]core.Profile{},
		HarnessOrder: []string{h.Name},
		Channels: map[string]core.ChannelSource{
			"sb": {Name: "sb", URL: liveURL + "?token=" + visQuerySentinel, Enabled: true,
				Headers: map[string]core.Secret{"Authorization": core.Secret("Bearer " + visHeaderSentinel)}},
			"dead": {Name: "dead", URL: "http://" + deadAddr + "/mcp?token=" + visDeadSentinel, Enabled: true},
		},
		ChannelOrder: []string{"sb", "dead"},
		Webhooks: map[string]core.WebhookSource{"ci": {
			Name: "ci", Verify: core.VerifyBearer, Secret: core.Secret(wiringToken),
			EventHeader: "X-Event", Events: []string{"push"}, DeliveryHeader: "X-Delivery",
			MaxBody: core.DefaultWebhookMaxBody, RateLimit: core.DefaultWebhookRateLimit, Enabled: true,
		}},
		WebhookOrder: []string{"ci"},
	}
}

// waitSource polls the source manager until ref reaches want.
func waitSource(t *testing.T, sources *source.Manager, ref string, want trigger.SourceState) source.Status {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if st, ok := sources.StatusOf(ref); ok && st.State == want {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never reached %s: %+v", ref, want, sources.Status())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// assertNoSentinel fails when any credential sentinel appears in s.
func assertNoSentinel(t *testing.T, where, s string) {
	t.Helper()
	for _, sentinel := range []string{visHeaderSentinel, visQuerySentinel, visDeadSentinel, wiringToken} {
		if strings.Contains(s, sentinel) {
			t.Errorf("%s carries the credential %q:\n%s", where, sentinel, s)
		}
	}
}

// TestDaemonWiringTriggersReportsEverySource is REQ "Trigger Visibility"'s
// field list read back through the daemon's own wiring, and REQ "Credential
// Resolution"'s "never show a value" checked against every rendering.
func TestDaemonWiringTriggersReportsEverySource(t *testing.T) {
	live := testserver.New(testserver.Options{})
	t.Cleanup(live.Close)
	d := startVisibilityDaemon(t, visibilityConfig(t, live.URL))

	waitSource(t, d.sources, "channel.sb", trigger.StateConnected)
	dead := waitSource(t, d.sources, "channel.dead", trigger.StateBackoff)
	// The property the scrub protects, shown present upstream so its absence
	// below means something: the raw failure DOES quote the URL. The
	// sentinel is in the config's URL; a dial error echoes the request URL.
	if !strings.Contains(dead.Reason, "127.0.0.1") {
		t.Fatalf("the dead source's error does not name its endpoint, so the scrub check proves nothing: %q", dead.Reason)
	}

	// A delivery that fires: the counter and the last event come from real
	// traffic, not from a fixture.
	url := "http://" + d.webhooks.addr() + "/hooks/ci"
	if code, body := postHookEvent(t, url, "del-1", "push"); code != http.StatusAccepted {
		t.Fatalf("push delivery: %d %s", code, body)
	}

	// Let the run it started finish, so the listings below show the harness
	// between firings — the state REQ "Trigger Visibility" is about.
	deadline := time.Now().Add(15 * time.Second)
	for {
		runs := d.mgr.Runs("pr-review")
		if len(runs) == 1 && runs[0].Outcome != supervisor.OutcomeRunning {
			if st, _ := d.mgr.Snapshot("pr-review"); st.State == core.StateStopped {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the delivery's run never finished: %+v", runs)
		}
		time.Sleep(10 * time.Millisecond)
	}

	c := d.dial(t, nil)
	srcs, err := c.Triggers()
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]protocol.TriggerSourceInfo{}
	for _, s := range srcs {
		by[s.Source] = s
	}
	if len(srcs) != 3 {
		t.Fatalf("triggers = %d sources, want 3: %+v", len(srcs), srcs)
	}

	sb := by["channel.sb"]
	if sb.Kind != "channel" || sb.State != "connected" || sb.DownSince != "" || sb.Since == "" {
		t.Errorf("channel.sb = %+v, want a connected channel with a since and no down_since", sb)
	}
	if want := live.URL; sb.URL != want {
		t.Errorf("channel.sb url = %q, want %q (query removed)", sb.URL, want)
	}
	if len(sb.Headers) != 1 || sb.Headers[0] != "Authorization" {
		t.Errorf("channel.sb headers = %v, want the name Authorization", sb.Headers)
	}
	if len(sb.Harnesses) != 1 || sb.Harnesses[0] != "pr-review" {
		t.Errorf("channel.sb harnesses = %v", sb.Harnesses)
	}

	dd := by["channel.dead"]
	if dd.State != "backoff" || dd.Error == "" || dd.DownSince == "" {
		t.Errorf("channel.dead = %+v, want backoff with an error and a down_since", dd)
	}

	ci := by["webhook.ci"]
	if ci.Kind != "webhook" || ci.State != "listening" || ci.Path != "/hooks/ci" || ci.Verify != "bearer" ||
		len(ci.Events) != 1 || ci.Events[0] != "push" {
		t.Errorf("webhook.ci = %+v, want a listening bearer route with events [push]", ci)
	}
	if ci.Counters["fired"] != 1 || ci.LastEvent == "" {
		t.Errorf("webhook.ci counters = %v last_event = %q, want one firing and a last event", ci.Counters, ci.LastEvent)
	}
	for _, o := range trigger.Outcomes {
		if _, ok := ci.Counters[string(o)]; !ok {
			t.Errorf("webhook.ci counters lack %q: %v", o, ci.Counters)
		}
	}

	// Every rendering an operator can ask for, from this daemon's state.
	for _, verb := range []struct {
		name string
		fn   func(*client.Client, verbOpts) error
		o    verbOpts
	}{
		{"triggers", cmdTriggers, verbOpts{}},
		{"triggers --json", cmdTriggers, verbOpts{json: true}},
		{"describe", cmdDescribe, verbOpts{name: "pr-review"}},
		{"describe --json", cmdDescribe, verbOpts{name: "pr-review", json: true}},
		{"list", cmdList, verbOpts{}},
		{"jobs --json", cmdJobs, verbOpts{json: true}},
	} {
		out, err := captureStdout(t, func() error { return verb.fn(c, verb.o) })
		if err != nil {
			t.Fatalf("%s: %v", verb.name, err)
		}
		assertNoSentinel(t, verb.name, out)
		switch verb.name {
		case "triggers":
			for _, want := range []string{"channel.sb", "connected", "channel.dead", "backoff", "down ",
				"webhook.ci", "listening", "POST /hooks/ci (bearer; push)", "[Authorization]", "error: channel dead"} {
				if !strings.Contains(out, want) {
					t.Errorf("triggers table missing %q:\n%s", want, out)
				}
			}
		case "describe":
			for _, want := range []string{"armed", "channel.sb connected", "webhook.ci listening"} {
				if !strings.Contains(out, want) {
					t.Errorf("describe missing %q:\n%s", want, out)
				}
			}
			if strings.Contains(out, "disabled") || strings.Contains(out, "enabled") {
				t.Errorf("describe shows a triggered harness by its enabled flag:\n%s", out)
			}
		case "list":
			for _, want := range []string{"armed", schedfmt.TriggerGlyph, "channel.sb", "on event"} {
				if !strings.Contains(out, want) {
					t.Errorf("list missing %q:\n%s", want, out)
				}
			}
			if strings.Contains(out, "disabled") {
				t.Errorf("list shows a triggered harness as disabled:\n%s", out)
			}
		case "jobs --json":
			var jobs []protocol.JobInfo
			if err := json.Unmarshal([]byte(out), &jobs); err != nil {
				t.Fatalf("jobs --json: %v\n%s", err, out)
			}
			if len(jobs) != 1 || jobs[0].Name != "pr-review" || jobs[0].NextRun != "" || len(jobs[0].Triggers) != 3 {
				t.Errorf("jobs = %+v, want pr-review with its three triggers and no next window", jobs)
			}
		}
	}
}

// TestDaemonWiringTriggerSourceChangedPerTransition scripts connect → drop →
// backoff → connect against a real channel server and counts the
// trigger_source_changed events a subscriber receives. It counts, so it fails
// if the daemon stops emitting — not merely if it emits something wrong.
func TestDaemonWiringTriggerSourceChangedPerTransition(t *testing.T) {
	live := testserver.New(testserver.Options{})
	t.Cleanup(live.Close)
	cfg := visibilityConfig(t, live.URL)
	// Only the live channel: the dead one's backoff cycle would interleave.
	delete(cfg.Channels, "dead")
	cfg.ChannelOrder = []string{"sb"}
	h := cfg.Harnesses["pr-review"]
	h.Triggers = []string{"channel.sb"}
	cfg.Harnesses["pr-review"] = h
	d := startVisibilityDaemon(t, cfg)

	// The listener's live bind reaches daemon_info through the same wiring.
	ctl := d.dial(t, nil)
	di, err := ctl.DaemonInfo()
	if err != nil {
		t.Fatal(err)
	}
	if di.WebhookAddr == "" || di.WebhookAddr != d.webhooks.addr() || di.WebhookTLS {
		t.Errorf("daemon_info webhook = %q tls=%v, want the bound %q without TLS", di.WebhookAddr, di.WebhookTLS, d.webhooks.addr())
	}

	waitSource(t, d.sources, "channel.sb", trigger.StateConnected)
	sub := d.dial(t, []string{"events"})

	// Drop the stream: the source goes to backoff, then reconnects.
	live.CloseStreams()

	pc := sub.Conn()
	_ = sub.SetReadDeadline(time.Now().Add(20 * time.Second))
	var got []protocol.EventMsg
	for len(got) < 3 {
		f, err := pc.ReadFrame()
		if err != nil {
			t.Fatalf("after %d trigger_source_changed events: %v (got %+v)", len(got), err, got)
		}
		switch f.Type {
		case protocol.TypeEvent:
			assertNoSentinel(t, "event frame", string(f.Payload))
			var ev protocol.EventMsg
			if err := json.Unmarshal(f.Payload, &ev); err != nil {
				t.Fatal(err)
			}
			if ev.Kind == protocol.EvTriggerSourceChanged && ev.Source == "channel.sb" {
				got = append(got, ev)
			}
		case protocol.TypePing:
			_ = pc.WriteFrame(protocol.TypePong, nil)
		}
	}
	want := []string{"backoff", "connecting", "connected"}
	for i, ev := range got {
		if ev.State != want[i] || ev.SourceKind != "channel" {
			t.Errorf("event %d = %+v, want state %s kind channel (sequence %v)", i, ev, want[i], want)
		}
	}
	if got[0].Error == "" {
		t.Errorf("the backoff event carries no error: %+v", got[0])
	}
}

// postHookEvent delivers a bearer webhook with an event name.
func postHookEvent(t *testing.T, url, delivery, event string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest("POST", url, strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+wiringToken)
	req.Header.Set("X-Delivery", delivery)
	req.Header.Set("X-Event", event)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf [4096]byte
	n, _ := resp.Body.Read(buf[:])
	return resp.StatusCode, buf[:n]
}
