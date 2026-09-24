package webhook

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/log/v2"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/supervisor"
	"github.com/stump-wtf/harness/internal/trigger"
	"github.com/stump-wtf/harness/internal/trigger/source"
)

// testToken is the bearer secret every fixture route uses.
const testToken = "tok-Ohb3eiqu-not-a-real-secret"

// fakeFirer records what reached the fire step and answers with fixed
// decisions, so a test can see whether a delivery fired at all.
type fakeFirer struct {
	mu        sync.Mutex
	events    []*trigger.Envelope
	decisions []source.Decision
}

func (f *fakeFirer) Fire(ev *trigger.Envelope) []source.Decision {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, ev)
	return f.decisions
}

func (f *fakeFirer) fired() []*trigger.Envelope {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*trigger.Envelope(nil), f.events...)
}

// bearerSource is an enabled bearer route named name.
func bearerSource(name string) core.WebhookSource {
	return core.WebhookSource{
		Name:      name,
		Verify:    core.VerifyBearer,
		Secret:    core.Secret(testToken),
		MaxBody:   core.DefaultWebhookMaxBody,
		RateLimit: core.DefaultWebhookRateLimit,
		Enabled:   true,
	}
}

// testConfig declares the given sources and, for each name in bound, a
// harness that binds webhook.<name>.
func testConfig(sources []core.WebhookSource, bound ...string) *core.Config {
	cfg := &core.Config{
		Harnesses: map[string]core.Harness{},
		Webhooks:  map[string]core.WebhookSource{},
	}
	for _, s := range sources {
		cfg.Webhooks[s.Name] = s
		cfg.WebhookOrder = append(cfg.WebhookOrder, s.Name)
	}
	for _, name := range bound {
		h := core.Harness{Name: "h-" + name, Triggers: []string{"webhook." + name}}
		cfg.Harnesses[h.Name] = h
		cfg.HarnessOrder = append(cfg.HarnessOrder, h.Name)
	}
	return cfg
}

// syncBuffer is a log sink safe to read while the server writes to it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// startServer binds a real listener on loopback and serves it until the test
// ends. The returned buffer holds the server's log.
func startServer(t *testing.T, opts Options, cfg *core.Config) (*Server, *syncBuffer) {
	t.Helper()
	buf := &syncBuffer{}
	if opts.Log == nil {
		opts.Log = log.NewWithOptions(buf, log.Options{Level: log.DebugLevel})
	}
	if opts.Addr == "" {
		opts.Addr = "127.0.0.1:0"
	}
	if opts.Firer == nil {
		opts.Firer = &fakeFirer{}
	}
	srv := New(opts, cfg)
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := srv.Serve(); err != nil {
			t.Errorf("Serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		<-done
	})
	return srv, buf
}

// client never follows redirects, so a 3xx is seen as itself.
var client = &http.Client{
	Timeout: 10 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// do sends one request and returns the response with its body read.
func do(t *testing.T, method, url string, hdr map[string]string, body string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, b
}

// bearer is the header set for an authentic delivery.
func bearer() map[string]string {
	return map[string]string{"Authorization": "Bearer " + testToken, "Content-Type": "application/json"}
}

// holdRequest opens a raw connection and sends a POST's headers plus part of
// its body, so the handler holds a concurrency slot while it waits for the
// rest. finish sends the remainder and returns the raw response.
func holdRequest(t *testing.T, addr, path, body string) (finish func() string, abort func()) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	head := "POST " + path + " HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer " + testToken +
		"\r\nContent-Type: application/json\r\nContent-Length: " + itoa(len(body)) + "\r\nConnection: close\r\n\r\n"
	half := len(body) / 2
	if _, err := io.WriteString(conn, head+body[:half]); err != nil {
		t.Fatal(err)
	}
	finish = func() string {
		if _, err := io.WriteString(conn, body[half:]); err != nil {
			t.Fatal(err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		b, _ := io.ReadAll(conn)
		_ = conn.Close()
		return string(b)
	}
	abort = func() { _ = conn.Close() }
	return finish, abort
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

// waitInFlight polls until the server holds n slots.
func waitInFlight(t *testing.T, srv *Server, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for srv.InFlight() != n {
		if time.Now().After(deadline) {
			t.Fatalf("in-flight = %d, want %d", srv.InFlight(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// decisions is a canned fan-out result covering every decision kind.
func decisions() []source.Decision {
	return []source.Decision{
		{Harness: "a", Kind: supervisor.DecisionStarted, RunID: 3},
		{Harness: "b", Kind: supervisor.DecisionQueued},
		{Harness: "c", Kind: supervisor.DecisionSkipped, RunID: 9},
		{Harness: "d", Err: "panic while firing: boom"},
	}
}
