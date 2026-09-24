package webhook

// Listener tests. Every one runs against a real bound listener over TCP, not a
// ResponseRecorder: several properties here — the header timeout, the
// concurrency slot, byte-identical responses, the absence of a redirect — live
// in net/http's server and connection handling, and a recorder would skip the
// very layer under test.
//
// Governing: SPEC-0014 REQ "Webhook Listener", REQ "Webhook Routes", REQ
// "Webhook Verification", REQ "Webhook Filtering", REQ "Webhook Responses",
// REQ "Source Reconciliation On Reload", Security Requirements.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
)

// TestBearerVerifier covers the `bearer` row: the scheme word matches in any
// case, the token matches exactly, and anything else is refused without the
// error echoing what was presented.
func TestBearerVerifier(t *testing.T) {
	v, err := NewVerifier(bearerSource("ci"))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		values []string
		ok     bool
	}{
		{"exact", []string{"Bearer " + testToken}, true},
		{"lowercase scheme", []string{"bearer " + testToken}, true},
		{"uppercase scheme", []string{"BEARER " + testToken}, true},
		{"surrounding space", []string{"  Bearer   " + testToken + "  "}, true},
		{"missing", nil, false},
		{"wrong token", []string{"Bearer wrong-" + testToken}, false},
		{"token prefix", []string{"Bearer " + testToken[:len(testToken)-1]}, false},
		{"token case", []string{"Bearer " + strings.ToUpper(testToken)}, false},
		{"empty token", []string{"Bearer "}, false},
		{"scheme only", []string{"Bearer"}, false},
		{"basic scheme", []string{"Basic " + testToken}, false},
		{"bare token", []string{testToken}, false},
		{"two headers", []string{"Bearer " + testToken, "Bearer " + testToken}, false},
	}
	for _, c := range cases {
		h := http.Header{}
		for _, v := range c.values {
			h.Add("Authorization", v)
		}
		err := v.Verify(h, nil)
		if (err == nil) != c.ok {
			t.Errorf("%s: Verify = %v, want ok=%v", c.name, err, c.ok)
			continue
		}
		if err != nil {
			if !errors.Is(err, ErrUnauthorized) {
				t.Errorf("%s: error %v does not wrap ErrUnauthorized", c.name, err)
			}
			if strings.Contains(err.Error(), testToken[4:]) || strings.Contains(err.Error(), "wrong-") {
				t.Errorf("%s: error %q echoes the presented value", c.name, err)
			}
		}
	}
}

// TestUnimplementedSchemeRefusesEveryDelivery pins fail-closed: the parser
// accepts `github` today, the verifier does not exist yet (#462), and a
// delivery to such a route must be a 401 — never a firing.
func TestUnimplementedSchemeRefusesEveryDelivery(t *testing.T) {
	src := bearerSource("gh")
	src.Verify = core.VerifyGitHub
	if _, err := NewVerifier(src); !errors.Is(err, ErrSchemeUnavailable) {
		t.Fatalf("NewVerifier(github) = %v, want ErrSchemeUnavailable", err)
	}
	empty := bearerSource("nosecret")
	empty.Secret = ""
	if _, err := NewVerifier(empty); !errors.Is(err, ErrSchemeUnavailable) {
		t.Fatalf("NewVerifier(empty secret) = %v, want ErrSchemeUnavailable", err)
	}

	f := &fakeFirer{}
	srv, logs := startServer(t, Options{Firer: f}, testConfig([]core.WebhookSource{src, empty}, "gh", "nosecret"))
	for _, name := range []string{"gh", "nosecret"} {
		// Even an empty Authorization, and even the "right" bearer token.
		for _, hdr := range []map[string]string{nil, bearer(), {"X-Hub-Signature-256": "sha256=00"}} {
			resp, _ := do(t, "POST", "http://"+srv.Addr()+"/hooks/"+name, hdr, `{}`)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s: status %d, want 401", name, resp.StatusCode)
			}
		}
	}
	if n := len(f.fired()); n != 0 {
		t.Errorf("%d deliveries fired through a route with no verifier", n)
	}
	if !strings.Contains(logs.String(), "refuses every delivery") {
		t.Error("building a refusing route logged no warning")
	}
}

// TestBearerDeliveryFires follows one authentic delivery to the fire step and
// checks the envelope and the 202: every decision kind, run_id present for
// started and skipped only, and no credential in the event.
func TestBearerDeliveryFires(t *testing.T) {
	src := bearerSource("ci")
	src.EventHeader, src.DeliveryHeader = "X-Event", "X-Delivery"
	f := &fakeFirer{decisions: decisions()}
	srv, _ := startServer(t, Options{Firer: f}, testConfig([]core.WebhookSource{src}, "ci"))

	hdr := bearer()
	hdr["X-Event"], hdr["X-Delivery"] = "push", "d-123"
	resp, body := do(t, "POST", "http://"+srv.Addr()+"/hooks/ci", hdr, `{"ref":"main"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d, body %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body is not JSON: %s", body)
	}
	if got["webhook"] != "ci" || got["event_id"] != "d-123" || got["decision"] != "fired" {
		t.Errorf("body = %s", body)
	}
	firings, _ := got["firings"].([]any)
	if len(firings) != 4 {
		t.Fatalf("firings = %s", body)
	}
	want := []struct {
		harness, decision string
		runID             float64 // 0 = must be absent
	}{{"a", "started", 3}, {"b", "queued", 0}, {"c", "skipped", 9}, {"d", "error", 0}}
	for i, w := range want {
		fm := firings[i].(map[string]any)
		id, has := fm["run_id"]
		switch {
		case fm["harness"] != w.harness || fm["decision"] != w.decision:
			t.Errorf("firing %d = %v, want %s/%s", i, fm, w.harness, w.decision)
		case w.runID == 0 && has:
			t.Errorf("firing %d (%s) carries run_id %v; it must be absent", i, w.decision, id)
		case w.runID != 0 && id != w.runID:
			t.Errorf("firing %d (%s) run_id = %v, want %v", i, w.decision, id, w.runID)
		}
	}
	if strings.Contains(string(body), "boom") {
		t.Error("the 202 leaked a firing's error detail")
	}

	evs := f.fired()
	if len(evs) != 1 {
		t.Fatalf("fired %d times", len(evs))
	}
	ev := evs[0]
	if err := ev.Validate(); err != nil {
		t.Fatalf("envelope invalid: %v", err)
	}
	if ev.Source != "webhook.ci" || ev.EventID != "d-123" || ev.Webhook.Event != "push" || ev.Webhook.Delivery != "d-123" {
		t.Errorf("envelope = %+v / %+v", ev, ev.Webhook)
	}
	if string(ev.Webhook.Body) != `{"ref":"main"}` {
		t.Errorf("body = %s, want it verbatim", ev.Webhook.Body)
	}
	enc, _ := ev.Encode()
	if strings.Contains(string(enc), testToken) || strings.Contains(strings.ToLower(string(enc)), "authorization") {
		t.Errorf("the event carries the credential:\n%s", enc)
	}
}

// TestUnusableDeliveryIDIsReplaced: a sender-supplied delivery ID becomes the
// run record's event_id only when it looks like an ID.
func TestUnusableDeliveryIDIsReplaced(t *testing.T) {
	src := bearerSource("ci")
	src.DeliveryHeader = "X-Delivery"
	f := &fakeFirer{}
	srv, _ := startServer(t, Options{Firer: f}, testConfig([]core.WebhookSource{src}, "ci"))
	for _, id := range []string{"", "has space", strings.Repeat("x", maxEventIDLen+1)} {
		hdr := bearer()
		if id != "" {
			hdr["X-Delivery"] = id
		}
		resp, body := do(t, "POST", "http://"+srv.Addr()+"/hooks/ci", hdr, `{}`)
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("status %d", resp.StatusCode)
		}
		var got struct {
			EventID string `json:"event_id"`
		}
		_ = json.Unmarshal(body, &got)
		if !strings.HasPrefix(got.EventID, "wh-") {
			t.Errorf("delivery id %q: event_id = %q, want a daemon-made one", id, got.EventID)
		}
	}
}

// TestUnauthorizedIsUniform: every way to fail verification gets the same
// 401 bytes, fires nothing, and is logged with route and peer but never with
// the presented value.
func TestUnauthorizedIsUniform(t *testing.T) {
	f := &fakeFirer{}
	srv, logs := startServer(t, Options{Firer: f}, testConfig([]core.WebhookSource{bearerSource("ci")}, "ci"))
	const presented = "Presented-Zu7ahwoh"
	var first []byte
	for _, auth := range []string{"", "Bearer " + presented, "Basic " + presented, "Bearer", presented} {
		hdr := map[string]string{}
		if auth != "" {
			hdr["Authorization"] = auth
		}
		resp, body := do(t, "POST", "http://"+srv.Addr()+"/hooks/ci", hdr, `{}`)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("auth %q: status %d", auth, resp.StatusCode)
		}
		if first == nil {
			first = body
		} else if !bytes.Equal(body, first) {
			t.Errorf("auth %q: body %q differs from %q", auth, body, first)
		}
	}
	if string(first) != `{"error":"unauthorized"}`+"\n" {
		t.Errorf("401 body = %q", first)
	}
	if n := len(f.fired()); n != 0 {
		t.Errorf("%d unauthenticated deliveries fired", n)
	}
	out := logs.String()
	if !strings.Contains(out, "webhook.ci") || !strings.Contains(out, "127.0.0.1:") {
		t.Errorf("the refusal log names neither route nor peer:\n%s", out)
	}
	if strings.Contains(out, presented) {
		t.Errorf("the log carries the presented value:\n%s", out)
	}
}

// rawDump is a response as bytes, with the Date header dropped — the only
// field that may legitimately differ between two otherwise identical answers.
func rawDump(t *testing.T, resp *http.Response) string {
	t.Helper()
	resp.Header.Del("Date")
	b, err := httputil.DumpResponse(resp, true)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestNotFoundIsByteIdentical: an unknown, a disabled and an unbound name get
// the same bytes — status line, headers and body — even for a delivery that
// would verify, so a route cannot be enumerated.
func TestNotFoundIsByteIdentical(t *testing.T) {
	disabled := bearerSource("off")
	disabled.Enabled = false
	cfg := testConfig([]core.WebhookSource{bearerSource("ci"), disabled, bearerSource("lonely")}, "ci", "off")
	f := &fakeFirer{}
	srv, _ := startServer(t, Options{Firer: f}, cfg)

	dump := func(name string) string {
		req, _ := http.NewRequest("POST", "http://"+srv.Addr()+"/hooks/"+name, strings.NewReader(`{}`))
		for k, v := range bearer() {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", name, resp.StatusCode)
		}
		return rawDump(t, resp)
	}
	unknown := dump("never-declared")
	for _, name := range []string{"off", "lonely"} {
		if got := dump(name); got != unknown {
			t.Errorf("404 for %q differs from an undeclared name's:\n--- %s\n%s\n--- undeclared\n%s", name, name, got, unknown)
		}
	}
	if !strings.Contains(unknown, `{"error":"not_found"}`) {
		t.Errorf("404 body:\n%s", unknown)
	}
	if n := len(f.fired()); n != 0 {
		t.Errorf("%d deliveries to unserved names fired", n)
	}
	// The same route, served, does answer — so the 404s above are about the
	// names, not a broken listener.
	if resp, _ := do(t, "POST", "http://"+srv.Addr()+"/hooks/ci", bearer(), `{}`); resp.StatusCode != http.StatusAccepted {
		t.Errorf("served route: status %d", resp.StatusCode)
	}
}

// TestRoutesAndMethods: only POST /hooks/<name> and GET /healthz exist, other
// methods get 405 with Allow, and nothing ever redirects.
func TestRoutesAndMethods(t *testing.T) {
	f := &fakeFirer{}
	srv, _ := startServer(t, Options{Firer: f}, testConfig([]core.WebhookSource{bearerSource("ci")}, "ci"))
	base := "http://" + srv.Addr()

	resp, body := do(t, "GET", base+"/healthz", nil, "")
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Errorf("GET /healthz = %d %q", resp.StatusCode, body)
	}
	for _, c := range []struct {
		method, path, allow string
	}{
		{"GET", "/hooks/ci", "POST"},
		{"PUT", "/hooks/ci", "POST"},
		{"DELETE", "/hooks/ci", "POST"},
		{"GET", "/hooks/never-declared", "POST"},
		{"POST", "/healthz", "GET"},
	} {
		resp, body := do(t, c.method, base+c.path, bearer(), "")
		if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != c.allow {
			t.Errorf("%s %s = %d Allow=%q, want 405 Allow=%s", c.method, c.path, resp.StatusCode, resp.Header.Get("Allow"), c.allow)
		}
		if string(body) != `{"error":"method_not_allowed"}`+"\n" {
			t.Errorf("%s %s body = %q", c.method, c.path, body)
		}
	}
	// Paths a ServeMux would clean and 301. Every one is a plain 404 here.
	for _, p := range []string{"/", "/hooks", "/hooks/", "/hooks/ci/", "/hooks//ci", "/hooks/./ci", "/hooks/../hooks/ci", "/hooks/ci/extra", "/HOOKS/ci", "/metrics", "/healthz/"} {
		resp, _ := do(t, "POST", base+p, bearer(), `{}`)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("POST %s = %d, want 404", p, resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); loc != "" {
			t.Errorf("POST %s redirected to %q", p, loc)
		}
	}
	if n := len(f.fired()); n != 0 {
		t.Errorf("%d requests to non-delivery paths fired", n)
	}
}

// TestBodyOverMaxBodyIs413BeforeVerify: the cap is enforced before the
// verifier runs — declared or chunked — and nothing fires.
func TestBodyOverMaxBodyIs413BeforeVerify(t *testing.T) {
	src := bearerSource("ci")
	src.MaxBody = 16
	var verified atomic.Int64
	spy := func(s core.WebhookSource) (Verifier, error) {
		inner, err := NewVerifier(s)
		if err != nil {
			return nil, err
		}
		return verifierFunc(func(h http.Header, b []byte) error {
			verified.Add(1)
			return inner.Verify(h, b)
		}), nil
	}
	f := &fakeFirer{}
	srv, _ := startServer(t, Options{Firer: f, newVerifier: spy}, testConfig([]core.WebhookSource{src}, "ci"))
	url := "http://" + srv.Addr() + "/hooks/ci"

	// Declared length over the cap.
	resp, body := do(t, "POST", url, bearer(), strings.Repeat("x", 17))
	if resp.StatusCode != http.StatusRequestEntityTooLarge || string(body) != `{"error":"payload_too_large"}`+"\n" {
		t.Errorf("declared: %d %q", resp.StatusCode, body)
	}
	// Chunked, so no Content-Length to check up front: the bounded reader
	// has to catch it.
	req, _ := http.NewRequest("POST", url, io.MultiReader(strings.NewReader(strings.Repeat("y", 40))))
	req.ContentLength = -1
	for k, v := range bearer() {
		req.Header.Set(k, v)
	}
	resp2, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("chunked: %d", resp2.StatusCode)
	}
	if n := verified.Load(); n != 0 {
		t.Errorf("the verifier ran %d times on oversized bodies", n)
	}
	if n := len(f.fired()); n != 0 {
		t.Errorf("%d oversized deliveries fired", n)
	}
	// At the cap exactly it is accepted — and verified.
	if resp, _ := do(t, "POST", url, bearer(), strings.Repeat("z", 16)); resp.StatusCode != http.StatusAccepted {
		t.Errorf("at the cap: %d", resp.StatusCode)
	}
	if verified.Load() != 1 {
		t.Errorf("verifier ran %d times, want 1", verified.Load())
	}
}

type verifierFunc func(http.Header, []byte) error

func (f verifierFunc) Verify(h http.Header, b []byte) error { return f(h, b) }

// TestEventsAllowlist: a verified delivery whose event is not listed is a 202
// "ignored" that fires nothing; a listed one fires; one with no event name is
// not in the list.
func TestEventsAllowlist(t *testing.T) {
	src := bearerSource("ci")
	src.EventHeader, src.Events = "X-Event", []string{"push"}
	f := &fakeFirer{}
	srv, _ := startServer(t, Options{Firer: f}, testConfig([]core.WebhookSource{src}, "ci"))
	url := "http://" + srv.Addr() + "/hooks/ci"
	for _, ev := range []string{"ping", "", "Push"} {
		hdr := bearer()
		if ev != "" {
			hdr["X-Event"] = ev
		}
		resp, body := do(t, "POST", url, hdr, `{}`)
		if resp.StatusCode != http.StatusAccepted || string(body) != `{"webhook":"ci","decision":"ignored"}`+"\n" {
			t.Errorf("event %q: %d %s", ev, resp.StatusCode, body)
		}
	}
	if n := len(f.fired()); n != 0 {
		t.Fatalf("%d filtered deliveries fired", n)
	}
	hdr := bearer()
	hdr["X-Event"] = "push"
	if resp, _ := do(t, "POST", url, hdr, `{}`); resp.StatusCode != http.StatusAccepted || len(f.fired()) != 1 {
		t.Errorf("listed event: %d, fired %d", resp.StatusCode, len(f.fired()))
	}
	// Filtering is after verification: an unlisted event with a bad token
	// is a 401, not an "ignored".
	hdr["Authorization"], hdr["X-Event"] = "Bearer nope", "ping"
	if resp, _ := do(t, "POST", url, hdr, `{}`); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unverified, unlisted: %d, want 401", resp.StatusCode)
	}
}

// TestSixtyFifthConcurrentRequestIs503 holds 64 deliveries mid-body — each
// holding a slot — and shows the 65th is refused with 503, then admitted once
// a slot frees.
func TestSixtyFifthConcurrentRequestIs503(t *testing.T) {
	f := &fakeFirer{}
	srv, _ := startServer(t, Options{Firer: f}, testConfig([]core.WebhookSource{bearerSource("ci")}, "ci"))
	if DefaultMaxConcurrent != 64 {
		t.Fatalf("DefaultMaxConcurrent = %d, REQ \"Webhook Listener\" says 64", DefaultMaxConcurrent)
	}
	var finishers []func() string
	for i := 0; i < DefaultMaxConcurrent; i++ {
		finish, abort := holdRequest(t, srv.Addr(), "/hooks/ci", `{"n":"0123456789"}`)
		t.Cleanup(abort)
		finishers = append(finishers, finish)
	}
	waitInFlight(t, srv, DefaultMaxConcurrent)

	resp, body := do(t, "POST", "http://"+srv.Addr()+"/hooks/ci", bearer(), `{}`)
	if resp.StatusCode != http.StatusServiceUnavailable || string(body) != `{"error":"unavailable"}`+"\n" {
		t.Fatalf("65th request: %d %q, want 503", resp.StatusCode, body)
	}
	if n := len(f.fired()); n != 0 {
		t.Fatalf("fired %d while all slots were held", n)
	}

	if out := finishers[0](); !strings.Contains(out, "202 Accepted") {
		t.Fatalf("a held request did not complete: %q", out)
	}
	waitInFlight(t, srv, DefaultMaxConcurrent-1)
	if resp, _ := do(t, "POST", "http://"+srv.Addr()+"/hooks/ci", bearer(), `{}`); resp.StatusCode != http.StatusAccepted {
		t.Errorf("after a slot freed: %d, want 202", resp.StatusCode)
	}
}

// TestSlowHeadersAreCutOff: a client that trickles header lines forever is
// disconnected at the header timeout, even though every line arrives well
// inside any per-read deadline. The timeout is shortened for the test; the
// production value is asserted separately.
func TestSlowHeadersAreCutOff(t *testing.T) {
	if DefaultReadHeaderTimeout != 10*time.Second || DefaultReadTimeout != 30*time.Second ||
		DefaultWriteTimeout != 30*time.Second || DefaultIdleTimeout != 60*time.Second || DefaultMaxHeaderBytes != 64<<10 {
		t.Fatal("the REQ \"Webhook Listener\" defaults changed")
	}
	const cutoff = 300 * time.Millisecond
	srv, _ := startServer(t, Options{ReadHeaderTimeout: cutoff}, testConfig([]core.WebhookSource{bearerSource("ci")}, "ci"))
	// The defaults the production server is built with, read back.
	if prod := New(Options{}, nil); prod.hs.ReadHeaderTimeout != DefaultReadHeaderTimeout || prod.hs.MaxHeaderBytes != DefaultMaxHeaderBytes ||
		prod.hs.ReadTimeout != DefaultReadTimeout || prod.hs.WriteTimeout != DefaultWriteTimeout || prod.hs.IdleTimeout != DefaultIdleTimeout {
		t.Fatalf("New(Options{}) did not apply the defaults: %+v", prod.hs)
	}

	conn, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	start := time.Now()
	if _, err := io.WriteString(conn, "POST /hooks/ci HTTP/1.1\r\nHost: x\r\n"); err != nil {
		t.Fatal(err)
	}
	closed := make(chan time.Duration, 1)
	go func() {
		_, _ = io.ReadAll(conn) // returns when the server hangs up
		closed <- time.Since(start)
	}()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	give := time.After(5 * time.Second)
	for i := 0; ; i++ {
		select {
		case took := <-closed:
			if took < cutoff {
				t.Errorf("cut off after %v, before the %v timeout", took, cutoff)
			}
			return
		case <-ticker.C:
			// Keep trickling: each line is well inside any single-read
			// deadline, so only a whole-header deadline stops this.
			_, _ = io.WriteString(conn, "X-Trickle-"+itoa(i)+": y\r\n")
		case <-give:
			t.Fatal("a client trickling headers was never cut off")
		}
	}
}

// TestSecurityHeadersOnEveryStatus: each status the listener can answer with
// carries the full header set, no HSTS over cleartext, and no cookie.
func TestSecurityHeadersOnEveryStatus(t *testing.T) {
	small := bearerSource("small")
	small.MaxBody = 4
	f := &fakeFirer{}
	srv, _ := startServer(t, Options{Firer: f, MaxConcurrent: 1},
		testConfig([]core.WebhookSource{bearerSource("ci"), small, bearerSource("busy")}, "ci", "small", "busy"))
	base := "http://" + srv.Addr()

	check := func(label string, resp *http.Response, wantStatus int) {
		t.Helper()
		if resp.StatusCode != wantStatus {
			t.Errorf("%s: status %d, want %d", label, resp.StatusCode, wantStatus)
		}
		for _, kv := range securityHeaders {
			if got := resp.Header.Get(kv[0]); got != kv[1] {
				t.Errorf("%s (%d): %s = %q, want %q", label, resp.StatusCode, kv[0], got, kv[1])
			}
		}
		if h := resp.Header.Get("Strict-Transport-Security"); h != "" {
			t.Errorf("%s: HSTS %q over cleartext", label, h)
		}
		if c := resp.Header.Values("Set-Cookie"); len(c) != 0 {
			t.Errorf("%s: sets a cookie %q", label, c)
		}
	}
	r, _ := do(t, "GET", base+"/healthz", nil, "")
	check("healthz", r, 200)
	r, _ = do(t, "POST", base+"/hooks/ci", bearer(), `{}`)
	check("fired", r, 202)
	r, _ = do(t, "POST", base+"/hooks/ci", nil, `{}`)
	check("unauthorized", r, 401)
	r, _ = do(t, "POST", base+"/hooks/nope", bearer(), `{}`)
	check("not found", r, 404)
	r, _ = do(t, "GET", base+"/hooks/ci", nil, "")
	check("method", r, 405)
	r, _ = do(t, "POST", base+"/hooks/small", bearer(), `{"too":"big"}`)
	check("too large", r, 413)

	finish, abort := holdRequest(t, srv.Addr(), "/hooks/busy", `{"hold":1}`)
	defer abort()
	waitInFlight(t, srv, 1)
	r, _ = do(t, "POST", base+"/hooks/ci", bearer(), `{}`)
	check("unavailable", r, 503)
	// And the held one, read off the wire rather than through a client.
	raw := finish()
	resp, err := http.ReadResponse(bufio.NewReader(strings.NewReader(raw)), nil)
	if err != nil {
		t.Fatalf("held response: %v\n%s", err, raw)
	}
	check("held", resp, 202)
}

// TestHTTPSOnlyWithHSTS: with both TLS files the listener speaks only HTTPS,
// and adds HSTS — to errors too.
func TestHTTPSOnlyWithHSTS(t *testing.T) {
	cert, key := selfSigned(t)
	srv, _ := startServer(t, Options{Settings: Settings{TLSCertFile: cert, TLSKeyFile: key}},
		testConfig([]core.WebhookSource{bearerSource("ci")}, "ci"))
	tlsClient := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // a throwaway self-signed test cert
	}}
	for _, c := range []struct {
		method, path string
		status       int
	}{{"GET", "/healthz", 200}, {"POST", "/hooks/nope", 404}, {"POST", "/hooks/ci", 401}} {
		req, _ := http.NewRequest(c.method, "https://"+srv.Addr()+c.path, strings.NewReader("{}"))
		resp, err := tlsClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", c.path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != c.status || resp.Header.Get("Strict-Transport-Security") != hstsValue {
			t.Errorf("%s %s: %d HSTS=%q", c.method, c.path, resp.StatusCode, resp.Header.Get("Strict-Transport-Security"))
		}
	}
	// Cleartext to the TLS port is not served.
	resp, err := client.Get("http://" + srv.Addr() + "/healthz")
	if err == nil {
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Errorf("cleartext /healthz answered 200 on an HTTPS listener: %q", b)
		}
	}
}

// TestBadKeyPairFailsListen: a TLS pair that does not load fails the bind,
// not the first delivery.
func TestBadKeyPairFailsListen(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.pem")
	if err := os.WriteFile(bad, []byte("not a pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := New(Options{Settings: Settings{Addr: "127.0.0.1:0", TLSCertFile: bad, TLSKeyFile: bad}, Firer: &fakeFirer{}}, nil)
	if err := srv.Listen(); err == nil {
		t.Fatal("Listen succeeded with an unloadable key pair")
	}
}

// TestReloadSwapsTableAtomically: after a reload, a route the new config
// disables answers 404 — but a request that started before the reload
// completes against the table it started with.
func TestReloadSwapsTableAtomically(t *testing.T) {
	f := &fakeFirer{}
	settings := Settings{Addr: "127.0.0.1:0"}
	srv, _ := startServer(t, Options{Settings: settings, Firer: f}, testConfig([]core.WebhookSource{bearerSource("ci")}, "ci"))

	finish, abort := holdRequest(t, srv.Addr(), "/hooks/ci", `{"in":"flight"}`)
	defer abort()
	waitInFlight(t, srv, 1)

	off := bearerSource("ci")
	off.Enabled = false
	if srv.Reload(testConfig([]core.WebhookSource{off}, "ci"), settings) {
		t.Error("an unchanged bind reported restart required")
	}
	if resp, _ := do(t, "POST", "http://"+srv.Addr()+"/hooks/ci", bearer(), `{}`); resp.StatusCode != http.StatusNotFound {
		t.Errorf("after the reload disabled the route: %d, want 404", resp.StatusCode)
	}
	if out := finish(); !strings.Contains(out, "202 Accepted") || !strings.Contains(out, `"decision":"fired"`) {
		t.Errorf("the in-flight request did not complete against its own table:\n%s", out)
	}
	if evs := f.fired(); len(evs) != 1 || evs[0].Source != "webhook.ci" {
		t.Errorf("fired = %d", len(evs))
	}
}

// TestReloadBindChangeKeepsOldListener: a changed address is reported as
// needing a restart, and the listener stays where it was.
func TestReloadBindChangeKeepsOldListener(t *testing.T) {
	srv, logs := startServer(t, Options{}, testConfig([]core.WebhookSource{bearerSource("ci")}, "ci"))
	old := srv.Addr()
	other, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	otherAddr := other.Addr().String()
	_ = other.Close()

	if !srv.Reload(testConfig([]core.WebhookSource{bearerSource("ci")}, "ci"), Settings{Addr: otherAddr}) {
		t.Error("a changed address did not report restart required")
	}
	if !strings.Contains(logs.String(), "restart the daemon") {
		t.Errorf("no restart-required log line:\n%s", logs)
	}
	if resp, _ := do(t, "GET", "http://"+old+"/healthz", nil, ""); resp.StatusCode != 200 {
		t.Errorf("old address stopped serving: %d", resp.StatusCode)
	}
	if c, err := net.DialTimeout("tcp", otherAddr, time.Second); err == nil {
		_ = c.Close()
		t.Errorf("something is listening on the new address %s", otherAddr)
	}
	cert, key := selfSigned(t)
	if !srv.Reload(nil, Settings{Addr: srv.Settings().Addr, TLSCertFile: cert, TLSKeyFile: key}) {
		t.Error("a TLS change did not report restart required")
	}
}

// TestShutdownStopsAccepting: after Shutdown nothing answers on the address.
func TestShutdownStopsAccepting(t *testing.T) {
	srv := New(Options{Settings: Settings{Addr: "127.0.0.1:0"}, Firer: &fakeFirer{}}, nil)
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve() }()
	addr := srv.Addr()
	if resp, _ := do(t, "GET", "http://"+addr+"/healthz", nil, ""); resp.StatusCode != 200 {
		t.Fatalf("healthz %d", resp.StatusCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), ShutdownGrace)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := <-done; err != nil {
		t.Errorf("Serve after Shutdown = %v, want nil", err)
	}
	if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		_ = c.Close()
		t.Error("the address still accepts connections after Shutdown")
	}
}

// selfSigned writes a throwaway certificate and key for 127.0.0.1.
func selfSigned(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

