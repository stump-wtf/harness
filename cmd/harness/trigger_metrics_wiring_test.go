package main

// The trigger-metrics wiring test.
//
// It drives what THE DAEMON builds — beginDaemonMetrics/attachTriggers/serve,
// startDaemonSources, beginDaemonWebhooks/serve and both reload hooks — over
// the Manager daemonManagerOptions builds, sends real deliveries through the
// real webhook listener and real doorbells over a real channel session to the
// fake MCP server, and reads the answers off the real /metrics listener. A
// test that assembled the collector, the counters or the listener itself
// would pass against a daemon that never shared them (#315): the webhook
// server silently counting into its own counters is exactly the wiring bug
// the refusal outcomes below would miss.
//
// Governing: ADR-0021; SPEC-0014 REQ "Trigger Metrics", REQ "Source
// Reconciliation On Reload"; SPEC-0013 REQ-5, REQ-6.
//
// @joestump 09/24/2026 - Introduced with the SPEC-0014 trigger metrics (#480).

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/trigger"
	"github.com/stump-wtf/harness/internal/trigger/channel/testserver"
)

// Distinctive strings a delivery or doorbell carries, none of which may ever
// appear in the exposition.
const (
	tmEventName = "evt-push-Xae3oo"
	tmOtherName = "evt-issue-Ohm4ah"
	tmBodyText  = "build 42 failed Jei9ee"
	tmDoorbell  = "PR #9 opened Thoo3a"
	tmDelivery  = "dlv-Quo7ee"
	tmBadRoute  = "nope-Vai2ie"
)

// triggerMetricsConfig declares webhook.<hook> (bound by ci-run) and, when
// withChannel, channel.sb at url (bound by pr-review).
func triggerMetricsConfig(hook, url string, withChannel bool) *core.Config {
	oneShot := func(name, ref string) core.Harness {
		return core.Harness{
			Name: name, Adapter: "generic", Backend: core.BackendNative,
			Args: []string{"-c", "exit 0"}, Restart: core.RestartNo,
			Triggers: []string{ref}, Timeout: core.DefaultRunTimeout,
			OnOverlap: core.OverlapQueue, KeepRuns: core.DefaultKeepRuns,
		}
	}
	cfg := &core.Config{
		Harnesses: map[string]core.Harness{}, Profiles: map[string]core.Profile{},
		Webhooks: map[string]core.WebhookSource{hook: {
			Name: hook, Verify: core.VerifyBearer, Secret: core.Secret(wiringToken),
			EventHeader: "X-Event", DeliveryHeader: "X-Delivery", Events: []string{tmEventName},
			MaxBody: 256, RateLimit: core.RateLimit{Count: 2, Window: time.Hour}, Enabled: true,
		}},
		WebhookOrder: []string{hook},
	}
	add := func(h core.Harness) {
		cfg.Harnesses[h.Name] = h
		cfg.HarnessOrder = append(cfg.HarnessOrder, h.Name)
	}
	add(oneShot("ci-run", "webhook."+hook))
	if withChannel {
		add(oneShot("pr-review", "channel.sb"))
		cfg.Channels = map[string]core.ChannelSource{"sb": {Name: "sb", URL: url, Enabled: true}}
		cfg.ChannelOrder = []string{"sb"}
	}
	return cfg
}

// hook POSTs one delivery and returns the status code.
func hook(t *testing.T, url, auth, event, delivery, body string) int {
	t.Helper()
	req, err := http.NewRequest("POST", url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if event != "" {
		req.Header.Set("X-Event", event)
	}
	if delivery != "" {
		req.Header.Set("X-Delivery", delivery)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

// triggerSeries lists every harness_trigger_* series' source label value.
func triggerSources(fams map[string]*dto.MetricFamily) map[string]bool {
	out := map[string]bool{}
	for name, fam := range fams {
		if !strings.HasPrefix(name, "harness_trigger_") {
			continue
		}
		for _, m := range fam.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "source" {
					out[lp.GetValue()] = true
				}
			}
		}
	}
	return out
}

// assertZeroInitialized checks ref reports all seven outcomes at zero.
func assertZeroInitialized(t *testing.T, fams map[string]*dto.MetricFamily, ref string) {
	t.Helper()
	for _, o := range trigger.Outcomes {
		if v, ok := sample(fams, "harness_trigger_events_total", "source", ref, "outcome", string(o)); !ok || v != 0 {
			t.Errorf("harness_trigger_events_total{source=%q,outcome=%q} = %v (present %v), want 0", ref, o, v, ok)
		}
	}
}

func TestDaemonTriggerMetricsCountRealDeliveries(t *testing.T) {
	srv := testserver.New(testserver.Options{})
	// First, so it closes last: httptest.Server.Close waits for the open GET
	// stream, which only the source manager's Close ends.
	t.Cleanup(srv.Close)

	start := time.Now()
	mgr := newWiringManager(t, triggerMetricsConfig("ci", srv.URL, true))

	l, err := daemonMetricsListener(core.ServerConfig{MetricsListen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	dm := beginDaemonMetrics(mgr, l)
	t.Cleanup(dm.Stop)
	sources := startDaemonSources(mgr)
	t.Cleanup(sources.Close)
	dm.attachTriggers(sources)
	wireSourceReload(mgr, sources)
	webhooks := beginDaemonWebhooks(mgr, sources, "127.0.0.1:0")
	wireWebhookReload(mgr, webhooks)
	webhooks.serve()
	t.Cleanup(webhooks.shutdown)
	dm.serve(nil, nil)
	if dm.srv == nil || webhooks.addr() == "" {
		t.Fatal("the metrics or webhook listener did not bind")
	}
	metricsURL := "http://" + dm.srv.Addr() + "/metrics"
	hookURL := "http://" + webhooks.addr() + "/hooks/ci"

	waitScrape := func(what string, cond func(map[string]*dto.MetricFamily) bool) map[string]*dto.MetricFamily {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for {
			fams := scrapeURL(t, metricsURL)
			if cond(fams) {
				return fams
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s; sources %+v", what, sources.Status())
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	up := func(fams map[string]*dto.MetricFamily, ref, kind string) float64 {
		v, ok := sample(fams, "harness_trigger_source_up", "source", ref, "kind", kind)
		if !ok {
			return -1
		}
		return v
	}

	// 1. Two declared sources, nothing delivered yet: the full label set,
	// every outcome at zero, from the first scrape the channel is up in.
	fams := waitScrape("channel.sb to connect", func(f map[string]*dto.MetricFamily) bool { return up(f, "channel.sb", "channel") == 1 })
	if v := up(fams, "webhook.ci", "webhook"); v != 1 {
		t.Errorf("webhook.ci source_up = %v, want 1 (listening)", v)
	}
	assertZeroInitialized(t, fams, "webhook.ci")
	assertZeroInitialized(t, fams, "channel.sb")
	if n := len(fams["harness_trigger_events_total"].GetMetric()); n != 2*len(trigger.Outcomes) {
		t.Errorf("harness_trigger_events_total has %d series, want %d", n, 2*len(trigger.Outcomes))
	}
	if v, ok := sample(fams, "harness_trigger_reconnects_total", "source", "channel.sb"); !ok || v != 0 {
		t.Errorf("channel.sb reconnects = %v (present %v), want 0", v, ok)
	}
	if _, ok := sample(fams, "harness_trigger_reconnects_total", "source", "webhook.ci"); ok {
		t.Error("a webhook source reports reconnects")
	}
	if _, ok := fams["harness_trigger_last_event_timestamp"]; ok {
		t.Error("a last-event timestamp before any event")
	}

	// 2. Real deliveries, one per outcome the listener decides.
	body := `{"msg":"` + tmBodyText + `"}`
	for _, c := range []struct {
		what                  string
		auth, event, delivery string
		body                  string
		want                  int
	}{
		{"wrong token", "Bearer wrong", tmEventName, tmDelivery + "-0", body, http.StatusUnauthorized},
		{"over max_body", "Bearer " + wiringToken, tmEventName, tmDelivery + "-0", strings.Repeat("x", 300), http.StatusRequestEntityTooLarge},
		{"event not listed", "Bearer " + wiringToken, tmOtherName, tmDelivery + "-0", body, http.StatusAccepted},
		{"fires", "Bearer " + wiringToken, tmEventName, tmDelivery + "-1", body, http.StatusAccepted},
		{"redelivery", "Bearer " + wiringToken, tmEventName, tmDelivery + "-1", body, http.StatusAccepted},
		{"fires again", "Bearer " + wiringToken, tmEventName, tmDelivery + "-2", body, http.StatusAccepted},
		{"over rate_limit", "Bearer " + wiringToken, tmEventName, tmDelivery + "-3", body, http.StatusTooManyRequests},
	} {
		if got := hook(t, hookURL, c.auth, c.event, c.delivery, c.body); got != c.want {
			t.Fatalf("%s: status %d, want %d", c.what, got, c.want)
		}
	}
	if got := hook(t, "http://"+webhooks.addr()+"/hooks/"+tmBadRoute, "Bearer "+wiringToken, tmEventName, tmDelivery+"-4", body); got != http.StatusNotFound {
		t.Fatalf("unknown route: %d, want 404", got)
	}
	// And real doorbells: one malformed, one good.
	srv.PushNotification(`{"content":"x","meta":{"todo_id":7}}`)
	srv.PushNotification(`{"content":"` + tmDoorbell + `","meta":{"todo_id":"t-1"}}`)

	fams = waitScrape("both doorbells to be counted", func(f map[string]*dto.MetricFamily) bool {
		fired, _ := sample(f, "harness_trigger_events_total", "source", "channel.sb", "outcome", "fired")
		invalid, _ := sample(f, "harness_trigger_events_total", "source", "channel.sb", "outcome", "invalid")
		return fired == 1 && invalid == 1
	})
	want := map[string]map[trigger.Outcome]float64{
		"webhook.ci": {
			trigger.OutcomeFired: 2, trigger.OutcomeIgnored: 1, trigger.OutcomeDuplicate: 1,
			trigger.OutcomeUnauthorized: 1, trigger.OutcomeTooLarge: 1, trigger.OutcomeRateLimited: 1,
		},
		"channel.sb": {trigger.OutcomeFired: 1, trigger.OutcomeInvalid: 1},
	}
	for ref, outcomes := range want {
		for _, o := range trigger.Outcomes {
			if v, _ := sample(fams, "harness_trigger_events_total", "source", ref, "outcome", string(o)); v != outcomes[o] {
				t.Errorf("harness_trigger_events_total{source=%q,outcome=%q} = %v, want %v", ref, o, v, outcomes[o])
			}
		}
		last, ok := sample(fams, "harness_trigger_last_event_timestamp", "source", ref)
		if lo, hi := float64(start.Unix()-1), float64(time.Now().Unix()+1); !ok || last < lo || last > hi {
			t.Errorf("%s last event = %v (present %v), want within [%v, %v]", ref, last, ok, lo, hi)
		}
	}
	if got := triggerSources(fams); len(got) != 2 || !got["webhook.ci"] || !got["channel.sb"] {
		t.Errorf("trigger series carry sources %v, want webhook.ci and channel.sb only", got)
	}

	// `fired` is a count of real firings: each one is a run record.
	deadline := time.Now().Add(15 * time.Second)
	for len(mgr.Runs("ci-run")) != 2 || len(mgr.Runs("pr-review")) != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("fired counts 2 and 1, runs are %d and %d", len(mgr.Runs("ci-run")), len(mgr.Runs("pr-review")))
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Nothing a sender controls reached a label: not the body, the event
	// name, the delivery ID, the doorbell's text or an unserved route.
	resp, err := http.Get(metricsURL)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	for _, s := range []string{tmBodyText, tmEventName, tmOtherName, tmDelivery, tmDoorbell, tmBadRoute, wiringToken} {
		if strings.Contains(string(raw), s) {
			t.Errorf("the exposition carries sender-controlled text %q", s)
		}
	}

	// 3. The channel's stream drops: source_up reads 0 while the session is
	// in backoff (at least channel.BackoffBase), then 1 again once it is back
	// — a real transition the session observed, and one reconnect.
	srv.CloseStreams()
	waitScrape("channel.sb source_up to read 0", func(f map[string]*dto.MetricFamily) bool { return up(f, "channel.sb", "channel") == 0 })
	fams = waitScrape("channel.sb to reconnect", func(f map[string]*dto.MetricFamily) bool { return up(f, "channel.sb", "channel") == 1 })
	if v, _ := sample(fams, "harness_trigger_reconnects_total", "source", "channel.sb"); v != 1 {
		t.Errorf("channel.sb reconnects = %v, want 1", v)
	}

	// 4. A reload renames the webhook and removes the channel: neither old
	// name keeps a series, and the new name is zero-initialized.
	mgr.Reload(triggerMetricsConfig("ci2", srv.URL, false))
	fams = scrapeURL(t, metricsURL)
	if got := triggerSources(fams); len(got) != 1 || !got["webhook.ci2"] {
		t.Errorf("after the reload, trigger series carry sources %v, want webhook.ci2 only", got)
	}
	assertZeroInitialized(t, fams, "webhook.ci2")
	if v := up(fams, "webhook.ci2", "webhook"); v != 1 {
		t.Errorf("webhook.ci2 source_up = %v, want 1", v)
	}
}
