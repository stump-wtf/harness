package webhook

// Outcome counting on a served route, for harness_trigger_events_total.
//
// Governing: SPEC-0014 REQ "Trigger Metrics", REQ "Webhook Verification",
// REQ "Webhook Listener".
//
// @joestump 09/24/2026 - Introduced with the SPEC-0014 trigger metrics (#480).

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/trigger"
)

// failingBody is a request body whose read fails partway, the way a sender
// that drops mid-upload does.
type failingBody struct{ sent bool }

func (b *failingBody) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		return copy(p, `{"partial":`), nil
	}
	return 0, errors.New("connection reset by peer")
}

func (b *failingBody) Close() error { return nil }

// TestRefusalsAreCountedByOutcome: every refusal on a served route counts
// under its outcome — 401 unauthorized, 413 too_large whether the length was
// declared or discovered, an unreadable body invalid — against the counters
// the daemon shares (Options.Outcomes). An unserved name counts nothing at
// all, so no request can create a source. And the server never counts
// `fired`: the Firer does, so the shared counters see each firing once.
func TestRefusalsAreCountedByOutcome(t *testing.T) {
	shared := &trigger.OutcomeCounters{}
	src := bearerSource("ci")
	src.MaxBody = 64
	f := &fakeFirer{}
	cfg := testConfig([]core.WebhookSource{src, bearerSource("off")}, "ci")
	srv, _ := startServer(t, Options{Firer: f, Outcomes: shared}, cfg)
	if srv.Outcomes() != shared {
		t.Fatal("the server did not keep the counters it was handed")
	}
	base := "http://" + srv.Addr()
	url := base + "/hooks/ci"

	if resp, _ := do(t, "POST", url, map[string]string{"Authorization": "Bearer wrong"}, `{}`); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", resp.StatusCode)
	}
	big := strings.Repeat("x", 100)
	if resp, _ := do(t, "POST", url, bearer(), big); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("declared oversize body: %d", resp.StatusCode)
	}
	// No Content-Length: the cap is found while reading.
	req, err := http.NewRequest("POST", url, io.MultiReader(strings.NewReader(big)))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range bearer() {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge || req.ContentLength != 0 {
		t.Fatalf("chunked oversize body: %d (content length %d)", resp.StatusCode, req.ContentLength)
	}
	// A body that fails mid-read, driven through the handler directly: a real
	// client cannot be made to fail its own upload on cue.
	rreq := httptest.NewRequest("POST", "/hooks/ci", nil)
	rreq.Body, rreq.ContentLength = &failingBody{}, -1
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, rreq)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unreadable body: %d", rec.Code)
	}
	// Unserved names: unknown, and declared but disabled/unbound.
	for _, name := range []string{"nope", "off"} {
		if resp, _ := do(t, "POST", base+"/hooks/"+name, bearer(), `{}`); resp.StatusCode != http.StatusNotFound {
			t.Fatalf("/hooks/%s: %d, want 404", name, resp.StatusCode)
		}
	}
	if resp, _ := do(t, "POST", url, bearer(), `{}`); resp.StatusCode != http.StatusAccepted || len(f.fired()) != 1 {
		t.Fatalf("authentic delivery: %d, fired %d", resp.StatusCode, len(f.fired()))
	}

	want := map[trigger.Outcome]uint64{
		trigger.OutcomeUnauthorized: 1,
		trigger.OutcomeTooLarge:     2,
		trigger.OutcomeInvalid:      1,
	}
	for _, o := range trigger.Outcomes {
		if got := shared.Count("webhook.ci", o); got != want[o] {
			t.Errorf("webhook.ci %s = %d, want %d", o, got, want[o])
		}
	}
	snap := shared.Snapshot()
	if len(snap) != 1 {
		t.Errorf("counted sources = %v, want webhook.ci only: an unserved name made an entry", snap)
	}
}
