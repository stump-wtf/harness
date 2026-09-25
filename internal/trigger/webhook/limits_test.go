package webhook

// Pipeline steps 6–8 over HTTP: the `events` allowlist, delivery-ID
// de-duplication and the per-route rate limit. Every test drives a real
// listener with bearer deliveries carrying delivery_header/event_header, and
// judges each delivery by whether it reached the firer — not by a log line.
// A controllable clock drives bucket refill and de-duplication expiry.
//
// Governing: SPEC-0014 REQ "Webhook Filtering", REQ "Webhook Rate Limit",
// REQ "Webhook Responses", REQ "Source Reconciliation On Reload", REQ
// "Concurrency Safety".

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/trigger"
)

// fakeClock is a settable Options.Now.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2031, 1, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// limitedSource is a bearer route with event and delivery headers and the
// given rate_limit.
func limitedSource(name, rateLimit string) core.WebhookSource {
	src := bearerSource(name)
	src.EventHeader, src.DeliveryHeader = "X-Event", "X-Delivery"
	rl, err := core.ParseRateLimit(rateLimit)
	if err != nil {
		panic(err)
	}
	src.RateLimit = rl
	return src
}

// deliver posts one authentic delivery; delivery "" sends no delivery header.
func deliver(t *testing.T, url, event, delivery string) (*http.Response, string) {
	t.Helper()
	hdr := bearer()
	if event != "" {
		hdr["X-Event"] = event
	}
	if delivery != "" {
		hdr["X-Delivery"] = delivery
	}
	resp, body := do(t, "POST", url, hdr, `{}`)
	return resp, string(body)
}

// TestRedeliveryIsDuplicate: a delivery ID that fired is answered `202
// duplicate` an hour later — naming the first firing's event_id — and fires
// nothing; after 24 hours it is forgotten and fires again. A delivery with no
// delivery header is never de-duplicated.
func TestRedeliveryIsDuplicate(t *testing.T) {
	clock := newFakeClock()
	f := &fakeFirer{}
	srv, _ := startServer(t, Options{Firer: f, Now: clock.Now}, testConfig([]core.WebhookSource{limitedSource("ci", "0")}, "ci"))
	url := "http://" + srv.Addr() + "/hooks/ci"

	if resp, body := deliver(t, url, "push", "abc"); resp.StatusCode != http.StatusAccepted || len(f.fired()) != 1 {
		t.Fatalf("first delivery: %d %s, fired %d", resp.StatusCode, body, len(f.fired()))
	}
	clock.Advance(time.Hour)
	resp, body := deliver(t, url, "push", "abc")
	if resp.StatusCode != http.StatusAccepted || body != `{"webhook":"ci","event_id":"abc","decision":"duplicate"}`+"\n" {
		t.Errorf("redelivery an hour later: %d %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("duplicate Content-Type = %q", ct)
	}
	if n := len(f.fired()); n != 1 {
		t.Fatalf("the redelivery fired: %d firings, want 1", n)
	}
	if n := srv.Outcomes().Count("webhook.ci", trigger.OutcomeDuplicate); n != 1 {
		t.Errorf("duplicate count = %d, want 1", n)
	}

	// Just inside the window it is still a duplicate; at 24 hours it is not.
	clock.Advance(DedupWindow - time.Hour - time.Second)
	if _, body := deliver(t, url, "push", "abc"); len(f.fired()) != 1 {
		t.Fatalf("a second before the window closed: fired (%s)", body)
	}
	clock.Advance(time.Second)
	if resp, body := deliver(t, url, "push", "abc"); resp.StatusCode != http.StatusAccepted || len(f.fired()) != 2 {
		t.Fatalf("24 hours on: %d %s, fired %d, want the ID forgotten", resp.StatusCode, body, len(f.fired()))
	}

	// No delivery header: nothing to de-duplicate on, so each one fires.
	for i := 0; i < 3; i++ {
		deliver(t, url, "push", "")
	}
	if n := len(f.fired()); n != 5 {
		t.Errorf("deliveries with no ID: %d firings total, want 5", n)
	}
}

// TestDuplicateOfUnusableIDNamesTheFirstEventID: when the sender's ID could
// not stand as an event_id and the firing got a daemon-made one, the duplicate
// still names the firing's event_id, not a fresh one.
func TestDuplicateOfUnusableIDNamesTheFirstEventID(t *testing.T) {
	f := &fakeFirer{}
	srv, _ := startServer(t, Options{Firer: f}, testConfig([]core.WebhookSource{limitedSource("ci", "0")}, "ci"))
	url := "http://" + srv.Addr() + "/hooks/ci"
	id := "has a space"
	deliver(t, url, "push", id)
	if len(f.fired()) != 1 {
		t.Fatal("first delivery did not fire")
	}
	first := f.fired()[0].EventID
	if _, body := deliver(t, url, "push", id); body != `{"webhook":"ci","event_id":"`+first+`","decision":"duplicate"}`+"\n" {
		t.Errorf("duplicate = %s, want event_id %q", body, first)
	}
	if n := len(f.fired()); n != 1 {
		t.Errorf("the duplicate fired: %d", n)
	}
}

// TestDedupKeepsTheLast1024: the 1025th distinct ID evicts the first, which
// then fires again; the second is still remembered.
func TestDedupKeepsTheLast1024(t *testing.T) {
	f := &fakeFirer{}
	srv, _ := startServer(t, Options{Firer: f}, testConfig([]core.WebhookSource{limitedSource("ci", "0")}, "ci"))
	url := "http://" + srv.Addr() + "/hooks/ci"
	for i := 0; i <= DedupCapacity; i++ {
		deliver(t, url, "push", fmt.Sprintf("d-%d", i))
	}
	if n := len(f.fired()); n != DedupCapacity+1 {
		t.Fatalf("%d distinct IDs fired %d times", DedupCapacity+1, n)
	}
	if _, body := deliver(t, url, "push", "d-0"); len(f.fired()) != DedupCapacity+2 {
		t.Errorf("the evicted first ID did not fire again: %s", body)
	}
	if _, body := deliver(t, url, "push", "d-2"); len(f.fired()) != DedupCapacity+2 {
		t.Errorf("a remembered ID fired: %s", body)
	}
}

// TestRateLimitBurst: with rate_limit = "2/m", three verified deliveries in
// the same instant fire two; the third is a 429 with Retry-After and fires
// nothing. Its retry, once a token has refilled, fires — which it could not
// if the 429 had put its ID in the de-duplication set.
func TestRateLimitBurst(t *testing.T) {
	clock := newFakeClock()
	f := &fakeFirer{}
	srv, _ := startServer(t, Options{Firer: f, Now: clock.Now}, testConfig([]core.WebhookSource{limitedSource("ci", "2/m")}, "ci"))
	url := "http://" + srv.Addr() + "/hooks/ci"

	for _, id := range []string{"d-1", "d-2"} {
		if resp, body := deliver(t, url, "push", id); resp.StatusCode != http.StatusAccepted {
			t.Fatalf("%s: %d %s", id, resp.StatusCode, body)
		}
	}
	resp, body := deliver(t, url, "push", "d-3")
	if resp.StatusCode != http.StatusTooManyRequests || body != `{"error":"rate_limited"}`+"\n" {
		t.Errorf("third delivery: %d %s, want 429 rate_limited", resp.StatusCode, body)
	}
	if ra := resp.Header.Get("Retry-After"); ra != "30" {
		t.Errorf("Retry-After = %q, want 30 (one token of 2/m)", ra)
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("the 429 lacks the security headers")
	}
	if n := len(f.fired()); n != 2 {
		t.Fatalf("fired %d, want 2", n)
	}
	if n := srv.Outcomes().Count("webhook.ci", trigger.OutcomeRateLimited); n != 1 {
		t.Errorf("rate_limited count = %d, want 1", n)
	}

	// Short of a whole token, still refused, with a shorter wait.
	clock.Advance(20 * time.Second)
	if resp, _ := deliver(t, url, "push", "d-3"); resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "10" {
		t.Errorf("20s on: %d Retry-After %q, want 429 / 10", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	clock.Advance(10 * time.Second)
	if resp, body := deliver(t, url, "push", "d-3"); resp.StatusCode != http.StatusAccepted || len(f.fired()) != 3 {
		t.Fatalf("the 429's retry after a refill: %d %s, fired %d — was it left out of the dedup set?", resp.StatusCode, body, len(f.fired()))
	}
	if f.fired()[2].EventID != "d-3" {
		t.Errorf("third firing is %q, want the retried d-3", f.fired()[2].EventID)
	}
	// The refill was one token, and the retry spent it.
	if resp, _ := deliver(t, url, "push", "d-4"); resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("after the retry spent the refill: %d, want 429", resp.StatusCode)
	}
}

// TestOnlyFiringsSpendTokens: 1000 forged deliveries, ignored events and
// duplicates spend nothing, so with a budget of two the real sender's second
// firing still goes through — and only the third is refused.
func TestOnlyFiringsSpendTokens(t *testing.T) {
	clock := newFakeClock()
	f := &fakeFirer{}
	src := limitedSource("ci", "2/h")
	src.Events = []string{"push"}
	srv, _ := startServer(t, Options{Firer: f, Now: clock.Now}, testConfig([]core.WebhookSource{src}, "ci"))
	url := "http://" + srv.Addr() + "/hooks/ci"

	if resp, _ := deliver(t, url, "push", "d-1"); resp.StatusCode != http.StatusAccepted || len(f.fired()) != 1 {
		t.Fatalf("first delivery: %d", resp.StatusCode)
	}
	forged := map[string]string{"Authorization": "Bearer forged", "X-Event": "push"}
	for i := 0; i < 1000; i++ {
		forged["X-Delivery"] = fmt.Sprintf("forged-%d", i)
		if resp, _ := do(t, "POST", url, forged, `{}`); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("forged delivery %d: %d, want 401", i, resp.StatusCode)
		}
	}
	for i := 0; i < 5; i++ {
		if _, body := deliver(t, url, "ping", fmt.Sprintf("p-%d", i)); body != `{"webhook":"ci","decision":"ignored"}`+"\n" {
			t.Fatalf("unlisted event: %s", body)
		}
		if _, body := deliver(t, url, "push", "d-1"); body != `{"webhook":"ci","event_id":"d-1","decision":"duplicate"}`+"\n" {
			t.Fatalf("redelivery: %s", body)
		}
	}
	if resp, body := deliver(t, url, "push", "d-2"); resp.StatusCode != http.StatusAccepted || len(f.fired()) != 2 {
		t.Fatalf("the good delivery after the flood: %d %s, fired %d", resp.StatusCode, body, len(f.fired()))
	}
	if resp, _ := deliver(t, url, "push", "d-3"); resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("third firing within the hour: %d, want 429", resp.StatusCode)
	}
	if n := len(f.fired()); n != 2 {
		t.Errorf("fired %d, want 2", n)
	}
	// An ignored delivery is not remembered either: listed later, the same
	// ID is not a duplicate.
	clock.Advance(time.Hour)
	if resp, body := deliver(t, url, "push", "p-0"); resp.StatusCode != http.StatusAccepted || len(f.fired()) != 3 {
		t.Errorf("an ID first seen as an ignored event: %d %s", resp.StatusCode, body)
	}
	got := srv.Outcomes().Snapshot()["webhook.ci"]
	if got[trigger.OutcomeIgnored] != 5 || got[trigger.OutcomeDuplicate] != 5 || got[trigger.OutcomeRateLimited] != 1 {
		t.Errorf("outcome counts = %v, want ignored 5, duplicate 5, rate_limited 1", got)
	}
}

// TestZeroRateLimitIsUnlimited: rate_limit = "0" never answers 429.
func TestZeroRateLimitIsUnlimited(t *testing.T) {
	f := &fakeFirer{}
	srv, _ := startServer(t, Options{Firer: f}, testConfig([]core.WebhookSource{limitedSource("ci", "0")}, "ci"))
	url := "http://" + srv.Addr() + "/hooks/ci"
	for i := 0; i < 200; i++ {
		if resp, _ := deliver(t, url, "push", fmt.Sprintf("d-%d", i)); resp.StatusCode != http.StatusAccepted {
			t.Fatalf("delivery %d under rate_limit 0: %d", i, resp.StatusCode)
		}
	}
	if n := len(f.fired()); n != 200 {
		t.Errorf("fired %d, want 200", n)
	}
}

// TestConcurrentCopiesFireOnce: many copies of one delivery arriving at once
// fire exactly once. De-duplication is checked and recorded under one lock,
// so no two copies can both see the ID as new.
func TestConcurrentCopiesFireOnce(t *testing.T) {
	f := &fakeFirer{}
	srv, _ := startServer(t, Options{Firer: f}, testConfig([]core.WebhookSource{limitedSource("ci", "0")}, "ci"))
	url := "http://" + srv.Addr() + "/hooks/ci"
	const copies = 32
	codes := make(chan int, copies)
	var wg sync.WaitGroup
	for i := 0; i < copies; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Not do/deliver: they t.Fatal, which only the test goroutine may.
			req, _ := http.NewRequest("POST", url, strings.NewReader(`{}`))
			for k, v := range bearer() {
				req.Header.Set(k, v)
			}
			req.Header.Set("X-Event", "push")
			req.Header.Set("X-Delivery", "same")
			resp, err := client.Do(req)
			if err != nil {
				codes <- 0
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			codes <- resp.StatusCode
		}()
	}
	wg.Wait()
	close(codes)
	for c := range codes {
		if c != http.StatusAccepted {
			t.Errorf("a copy got %d, want 202", c)
		}
	}
	if n := len(f.fired()); n != 1 {
		t.Errorf("%d concurrent copies fired %d times, want 1", copies, n)
	}
	if n := srv.Outcomes().Count("webhook.ci", trigger.OutcomeDuplicate); n != copies-1 {
		t.Errorf("duplicate count = %d, want %d", n, copies-1)
	}
}

// TestReloadKeepsLimitsOfAnUnchangedRoute: a reload that leaves a route's name
// and rate_limit alone keeps its bucket and de-duplication set, whatever else
// changed; one that changes rate_limit starts both afresh.
func TestReloadKeepsLimitsOfAnUnchangedRoute(t *testing.T) {
	clock := newFakeClock()
	f := &fakeFirer{}
	settings := Settings{Addr: "127.0.0.1:0"}
	src := limitedSource("ci", "1/h")
	srv, _ := startServer(t, Options{Settings: settings, Firer: f, Now: clock.Now}, testConfig([]core.WebhookSource{src}, "ci"))
	url := "http://" + srv.Addr() + "/hooks/ci"

	if resp, _ := deliver(t, url, "push", "d-1"); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first delivery: %d", resp.StatusCode)
	}
	if resp, _ := deliver(t, url, "push", "d-2"); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second delivery under 1/h: %d, want 429", resp.StatusCode)
	}

	// Same name and rate_limit, a fresh config object and a new description:
	// the bucket stays drained and d-1 stays remembered.
	same := limitedSource("ci", "1/h")
	same.Description = "rewritten by a config tool"
	srv.Reload(testConfig([]core.WebhookSource{same}, "ci"), settings)
	if _, body := deliver(t, url, "push", "d-1"); body != `{"webhook":"ci","event_id":"d-1","decision":"duplicate"}`+"\n" {
		t.Errorf("after an unchanged reload, d-1: %s, want duplicate", body)
	}
	if resp, _ := deliver(t, url, "push", "d-2"); resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("after an unchanged reload, d-2: %d, want 429 (the bucket must not refill)", resp.StatusCode)
	}
	if n := len(f.fired()); n != 1 {
		t.Fatalf("fired %d after the unchanged reload, want 1", n)
	}

	// A changed rate_limit: fresh bucket, fresh set.
	srv.Reload(testConfig([]core.WebhookSource{limitedSource("ci", "2/h")}, "ci"), settings)
	for _, id := range []string{"d-1", "d-2"} {
		if resp, body := deliver(t, url, "push", id); resp.StatusCode != http.StatusAccepted || body == `{"webhook":"ci","event_id":"`+id+`","decision":"duplicate"}`+"\n" {
			t.Errorf("after rate_limit changed, %s: %d %s, want a firing", id, resp.StatusCode, body)
		}
	}
	if resp, _ := deliver(t, url, "push", "d-3"); resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("third under the new 2/h: %d, want 429", resp.StatusCode)
	}
	if n := len(f.fired()); n != 3 {
		t.Errorf("fired %d, want 3", n)
	}
}
