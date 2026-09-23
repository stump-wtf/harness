package channel

// Channel client tests, driven against the fake Streamable HTTP server in
// ./testserver. Every assertion that matters reads that server's REQUEST LOG,
// because the claims worth checking are negative — no tools/list, no
// tools/call, no redirect to another origin — and a negative claim cannot be
// checked from the client's own state.
//
// Governing: ADR-0021, ADR-0008; SPEC-0014 REQ "Channel Listener Session",
// REQ "Channel Notification Handling", REQ "Credential Resolution", Security
// Requirements § "Redirect Validation".
//
// @joestump 09/23/2026 - Introduced with the SPEC-0014 channel listener (#471).

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/log/v2"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/trigger/channel/testserver"
)

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func quietLog() *log.Logger { return log.New(discard{}) }

// recorder is a Handler that collects what it was given.
type recorder struct {
	mu      sync.Mutex
	content []string
	meta    []map[string]string
	invalid []string
	got     chan struct{}
}

func newRecorder() *recorder { return &recorder{got: make(chan struct{}, 32)} }

func (r *recorder) Notification(content string, meta map[string]string) {
	r.mu.Lock()
	r.content = append(r.content, content)
	r.meta = append(r.meta, meta)
	r.mu.Unlock()
	select {
	case r.got <- struct{}{}:
	default:
	}
}

func (r *recorder) Invalid(reason string) {
	r.mu.Lock()
	r.invalid = append(r.invalid, reason)
	r.mu.Unlock()
	select {
	case r.got <- struct{}{}:
	default:
	}
}

func (r *recorder) snapshot() ([]string, []map[string]string, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.content...), append([]map[string]string(nil), r.meta...), append([]string(nil), r.invalid...)
}

// await waits for n handler calls.
func (r *recorder) await(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-r.got:
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for handler call %d of %d", i+1, n)
		}
	}
}

func source(url string, headers map[string]core.Secret) core.ChannelSource {
	return core.ChannelSource{Name: "sb", URL: url, Headers: headers, Enabled: true}
}

// listen runs a session in the background and returns a stop function.
func listen(t *testing.T, c *Client, h Handler) (opened chan struct{}, stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	opened = make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = c.Listen(ctx, h, func() {
			select {
			case opened <- struct{}{}:
			default:
			}
		})
	}()
	return opened, func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("Listen did not return after its context was cancelled")
		}
	}
}

// TestSessionSendsOnlyWhatItIsAllowedTo is the listen-only contract, checked
// against the server's request log. A client that helpfully listed tools would
// pass every behavioural test in this file and fail only this one.
func TestSessionSendsOnlyWhatItIsAllowedTo(t *testing.T) {
	srv := testserver.New(testserver.Options{})
	defer srv.Close()

	c := New(Options{Source: source(srv.URL, nil), Log: quietLog()})
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	rec := newRecorder()
	opened, stop := listen(t, c, rec)
	select {
	case <-opened:
	case <-time.After(3 * time.Second):
		t.Fatal("the stream never opened")
	}
	stop()
	if err := c.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	rpc := srv.RPCMethods()
	want := []string{"initialize", "notifications/initialized"}
	if strings.Join(rpc, ",") != strings.Join(want, ",") {
		t.Errorf("JSON-RPC methods sent = %v, want exactly %v", rpc, want)
	}
	for _, m := range rpc {
		// Named explicitly, so a failure says which forbidden call was made.
		switch m {
		case "tools/list", "tools/call", "prompts/list", "resources/list", "resources/read":
			t.Errorf("the listener sent %s; it is listen-only", m)
		}
	}
	http := srv.HTTPMethods()
	if len(http) != 4 || http[0] != "POST" || http[1] != "POST" || http[2] != "GET" || http[3] != "DELETE" {
		t.Errorf("HTTP methods = %v, want POST POST GET DELETE", http)
	}
}

// TestSessionCarriesItsHeadersAndSessionID: the source's resolved headers go
// on EVERY request, and the session id and protocol version ride the ones
// after initialize.
func TestSessionCarriesItsHeadersAndSessionID(t *testing.T) {
	srv := testserver.New(testserver.Options{SessionID: "sess-xyz"})
	defer srv.Close()

	c := New(Options{
		Source: source(srv.URL, map[string]core.Secret{"Authorization": core.Secret("Bearer tok-1")}),
		Log:    quietLog(),
	})
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if c.SessionID() != "sess-xyz" {
		t.Errorf("SessionID = %q, want the id the server issued", c.SessionID())
	}
	rec := newRecorder()
	opened, stop := listen(t, c, rec)
	<-opened
	stop()

	reqs := srv.Requests()
	if len(reqs) < 3 {
		t.Fatalf("only %d requests reached the server", len(reqs))
	}
	for i, r := range reqs {
		if got := r.Header.Get("Authorization"); got != "Bearer tok-1" {
			t.Errorf("request %d (%s): Authorization = %q, want the source's header on every request", i, r.Method, got)
		}
		if i == 0 {
			continue // initialize has no session yet
		}
		if got := r.Header.Get("Mcp-Session-Id"); got != "sess-xyz" {
			t.Errorf("request %d (%s): Mcp-Session-Id = %q", i, r.Method, got)
		}
		if got := r.Header.Get("MCP-Protocol-Version"); got == "" {
			t.Errorf("request %d (%s): no MCP-Protocol-Version", i, r.Method)
		}
	}
	if got := reqs[2].Header.Get("Accept"); !strings.Contains(got, "text/event-stream") {
		t.Errorf("the GET's Accept = %q, want text/event-stream", got)
	}
}

// TestMissingChannelCapabilityIsRefusedBeforeTheStream covers the scenario
// "Server without the channel capability": state error, a reason naming the
// capability, and NO GET opened.
//
// The "no GET" half is the one that matters. A client that reported the error
// and opened the stream anyway would hold a consumer slot on a server that has
// nothing to send it.
func TestMissingChannelCapabilityIsRefusedBeforeTheStream(t *testing.T) {
	srv := testserver.New(testserver.Options{NoChannelCapability: true})
	defer srv.Close()

	c := New(Options{Source: source(srv.URL, nil), Log: quietLog()})
	err := c.Initialize(context.Background())
	if !errors.Is(err, ErrNoChannelCapability) {
		t.Fatalf("Initialize = %v, want ErrNoChannelCapability", err)
	}
	if !strings.Contains(err.Error(), "claude/channel") {
		t.Errorf("error %q does not name the missing capability", err)
	}
	// The error also says what the server DID offer, which is what an
	// operator needs to tell "wrong URL" from "old server".
	if !strings.Contains(err.Error(), "something/else") {
		t.Errorf("error %q does not say what the server offered", err)
	}
	for _, m := range srv.HTTPMethods() {
		if m == "GET" {
			t.Error("the client opened the stream despite the missing capability")
		}
	}
}

// TestRefusedStreamIsNotConnected covers "Initialized, but no stream": a 405
// on the GET is ErrStreamRefused, and onOpen is never called — so nothing
// can report the source connected.
func TestRefusedStreamIsNotConnected(t *testing.T) {
	srv := testserver.New(testserver.Options{RefuseStream: true})
	defer srv.Close()

	c := New(Options{Source: source(srv.URL, nil), Log: quietLog()})
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	openedCalled := false
	err := c.Listen(context.Background(), newRecorder(), func() { openedCalled = true })
	if !errors.Is(err, ErrStreamRefused) {
		t.Fatalf("Listen = %v, want ErrStreamRefused", err)
	}
	if openedCalled {
		t.Error("onOpen fired for a stream the server refused: nothing may report connected")
	}
}

func TestUnauthorizedIsItsOwnError(t *testing.T) {
	srv := testserver.New(testserver.Options{Unauthorized: true})
	defer srv.Close()

	c := New(Options{Source: source(srv.URL, nil), Log: quietLog()})
	if err := c.Initialize(context.Background()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Initialize = %v, want ErrUnauthorized", err)
	}
}

// TestDoorbellsReachTheHandler covers REQ "Channel Notification Handling"'s
// happy path, and that content and meta arrive verbatim.
func TestDoorbellsReachTheHandler(t *testing.T) {
	srv := testserver.New(testserver.Options{})
	defer srv.Close()

	c := New(Options{Source: source(srv.URL, nil), Log: quietLog()})
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	rec := newRecorder()
	opened, stop := listen(t, c, rec)
	<-opened
	defer stop()

	srv.PushNotification(`{"content":"PR #9 opened","meta":{"todo_id":"t1","queue":"reviews"}}`)
	rec.await(t, 1)

	content, meta, invalid := rec.snapshot()
	if len(content) != 1 || content[0] != "PR #9 opened" {
		t.Errorf("content = %v", content)
	}
	if meta[0]["todo_id"] != "t1" || meta[0]["queue"] != "reviews" {
		t.Errorf("meta = %v, want the payload verbatim", meta[0])
	}
	if len(invalid) != 0 {
		t.Errorf("a valid doorbell was counted invalid: %v", invalid)
	}
}

// TestMalformedNotificationsAreDroppedAndNamed covers the rejections. Each
// case's reason must name the offending field, because the operator reading
// the log is trying to work out what their server sent.
func TestMalformedNotificationsAreDroppedAndNamed(t *testing.T) {
	srv := testserver.New(testserver.Options{})
	defer srv.Close()

	c := New(Options{Source: source(srv.URL, nil), Log: quietLog()})
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	rec := newRecorder()
	opened, stop := listen(t, c, rec)
	<-opened
	defer stop()

	cases := []struct {
		params string
		want   string
	}{
		{`{"content":"ok","meta":{"todo_id":7}}`, "meta.todo_id"},
		{`{"content":7}`, "content"},
		{`{"meta":{"a":"b"}}`, "content"},
		{`{"content":"ok","meta":"nope"}`, "meta"},
		{`{"content":"` + strings.Repeat("x", 70*1024) + `"}`, "limit"},
	}
	for _, tc := range cases {
		srv.PushNotification(tc.params)
	}
	rec.await(t, len(cases))

	content, _, invalid := rec.snapshot()
	if len(content) != 0 {
		t.Errorf("a malformed notification fired: %v", content)
	}
	if len(invalid) != len(cases) {
		t.Fatalf("invalid reasons = %v, want %d", invalid, len(cases))
	}
	for i, tc := range cases {
		if !strings.Contains(invalid[i], tc.want) {
			t.Errorf("case %d: reason %q does not name %q", i, invalid[i], tc.want)
		}
	}

	// And the control: a valid doorbell after all that still fires, so the
	// stream survived every rejection.
	srv.PushNotification(`{"content":"still here"}`)
	rec.await(t, 1)
	content, _, _ = rec.snapshot()
	if len(content) != 1 || content[0] != "still here" {
		t.Errorf("the stream did not survive the rejections: %v", content)
	}
}

// TestOtherNotificationsAreIgnoredNotCountedInvalid: a server is entitled to
// send notifications this client has no use for, and treating them as
// protocol violations would make an ordinary server look broken.
func TestOtherNotificationsAreIgnoredNotCountedInvalid(t *testing.T) {
	srv := testserver.New(testserver.Options{})
	defer srv.Close()

	c := New(Options{Source: source(srv.URL, nil), Log: quietLog()})
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	rec := newRecorder()
	opened, stop := listen(t, c, rec)
	<-opened
	defer stop()

	srv.Push(`{"jsonrpc":"2.0","method":"notifications/progress","params":{"n":1}}`)
	srv.PushNotification(`{"content":"after"}`)
	rec.await(t, 1)

	content, _, invalid := rec.snapshot()
	if len(invalid) != 0 {
		t.Errorf("an unrelated notification was counted invalid: %v", invalid)
	}
	if len(content) != 1 || content[0] != "after" {
		t.Errorf("content = %v, want only the doorbell", content)
	}
}

// TestServerRequestsAreAnswered covers the two replies REQ "Channel Listener
// Session" requires: an empty result for ping, -32601 for anything else.
func TestServerRequestsAreAnswered(t *testing.T) {
	srv := testserver.New(testserver.Options{})
	defer srv.Close()

	c := New(Options{Source: source(srv.URL, nil), Log: quietLog()})
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	rec := newRecorder()
	opened, stop := listen(t, c, rec)
	<-opened
	defer stop()

	srv.PushRequest(7, "ping")
	srv.PushRequest(8, "sampling/createMessage")

	var pingReply, otherReply string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, r := range srv.Requests() {
			body := string(r.Body)
			if strings.Contains(body, `"id":7`) {
				pingReply = body
			}
			if strings.Contains(body, `"id":8`) {
				otherReply = body
			}
		}
		if pingReply != "" && otherReply != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(pingReply, `"result"`) {
		t.Errorf("ping reply = %q, want an empty result", pingReply)
	}
	if !strings.Contains(otherReply, "-32601") {
		t.Errorf("reply to an unimplemented method = %q, want -32601", otherReply)
	}
	// Answering rather than staying silent is the point: a server that gets
	// no reply cannot tell a client that will not do something from one that
	// has stopped reading.
	if otherReply == "" {
		t.Error("an unimplemented server request went unanswered")
	}
}

// TestRedirectToAnotherOriginIsRefused is the Security Requirements §
// "Redirect Validation" guard. Go's default CheckRedirect would carry the
// session's Authorization to the new origin.
func TestRedirectToAnotherOriginIsRefused(t *testing.T) {
	var second struct {
		mu   sync.Mutex
		hits int
		auth string
	}
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		second.mu.Lock()
		second.hits++
		second.auth = r.Header.Get("Authorization")
		second.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer evil.Close()

	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL, http.StatusFound)
	}))
	defer first.Close()

	c := New(Options{
		Source: source(first.URL, map[string]core.Secret{"Authorization": core.Secret("Bearer tok-1")}),
		Log:    quietLog(),
	})
	err := c.Initialize(context.Background())
	if err == nil {
		t.Fatal("the client followed a redirect to another origin")
	}
	if !strings.Contains(err.Error(), "redirect") {
		t.Errorf("error %q does not say the redirect was refused", err)
	}

	second.mu.Lock()
	hits, auth := second.hits, second.auth
	second.mu.Unlock()
	if hits != 0 {
		t.Errorf("the second host was reached %d times", hits)
	}
	if auth != "" {
		t.Error("the session credential reached the redirect target")
	}
}

// TestRedirectWithinTheSameOriginIsFollowed is the control for the guard
// above: a same-origin redirect is ordinary, and refusing every redirect
// would break a server that simply normalizes its own path.
func TestRedirectWithinTheSameOriginIsFollowed(t *testing.T) {
	var mux http.ServeMux
	srv := httptest.NewServer(&mux)
	defer srv.Close()
	inner := testserver.New(testserver.Options{})
	defer inner.Close()

	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/mcp/", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/mcp/", func(w http.ResponseWriter, r *http.Request) {
		// Proxy to the fake channel server's handler by re-dialling it.
		req, _ := http.NewRequest(r.Method, inner.URL, r.Body)
		req.Header = r.Header.Clone()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				_, _ = w.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
	})

	c := New(Options{Source: source(srv.URL+"/mcp", nil), Log: quietLog()})
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("a same-origin redirect was refused: %v", err)
	}
}

// TestPostResponseMayBeAStream: the transport lets a server answer a POST
// with JSON or with a one-message stream, and a client that handled only the
// first would fail against half the servers out there.
func TestPostResponseMayBeAStream(t *testing.T) {
	srv := testserver.New(testserver.Options{StreamResponses: true})
	defer srv.Close()

	c := New(Options{Source: source(srv.URL, nil), Log: quietLog()})
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize against a streaming server: %v", err)
	}
}

// stallingServer answers initialize as a one-event SSE stream that it then
// holds open, the way a server that never closes its POST response streams
// would; answers `notifications/initialized` and every reply POST the same
// way; and serves a GET stream the test can push into.
//
// MCP says a server SHOULD close a POST's stream after the response. SHOULD is
// not MUST, and the daemon cannot let a server that doesn't wedge the session.
type stallingServer struct {
	*httptest.Server
	push    chan string
	release chan struct{}
}

func newStallingServer(t *testing.T) *stallingServer {
	t.Helper()
	s := &stallingServer{push: make(chan string, 8), release: make(chan struct{})}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f := w.(http.Flusher)
		switch r.Method {
		case http.MethodPost:
			body := make([]byte, 4096)
			n, _ := r.Body.Read(body)
			w.Header().Set("Mcp-Session-Id", "sess-stall")
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if strings.Contains(string(body[:n]), `"initialize"`) {
				_, _ = w.Write([]byte(`data: {"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26","capabilities":{"experimental":{"claude/channel":{}}}}}` + "\n\n"))
			}
			f.Flush()
		case http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			f.Flush()
			for {
				select {
				case d := <-s.push:
					_, _ = w.Write([]byte("data: " + d + "\n\n"))
					f.Flush()
				case <-s.release:
					return
				case <-r.Context().Done():
					return
				}
			}
		default:
			w.WriteHeader(http.StatusNoContent)
			return
		}
		// Hold the POST's stream open until the test ends.
		select {
		case <-s.release:
		case <-r.Context().Done():
		}
	}))
	// Release before Close: httptest.Server.Close waits for the handlers.
	t.Cleanup(func() { close(s.release); s.Close() })
	return s
}

// TestInitializeDoesNotWaitForTheServerToCloseItsStream: initialize must
// return once it has the response, not once the server closes the stream the
// response arrived on. Otherwise a server that holds POST streams open parks
// the source in `connecting` forever.
func TestInitializeDoesNotWaitForTheServerToCloseItsStream(t *testing.T) {
	srv := newStallingServer(t)
	c := New(Options{Source: source(srv.URL, nil), Log: quietLog()})

	done := make(chan error, 1)
	go func() { done <- c.Initialize(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Initialize: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Initialize is still waiting for the server to close a response stream it already read the answer from")
	}
}

// TestAStalledReplyDoesNotStopTheStream: answering a server `ping` happens on
// the goroutine that reads doorbells, so a reply POST whose response never
// ends must not stall it.
func TestAStalledReplyDoesNotStopTheStream(t *testing.T) {
	srv := newStallingServer(t)
	c := New(Options{Source: source(srv.URL, nil), Log: quietLog()})

	initDone := make(chan error, 1)
	go func() { initDone <- c.Initialize(context.Background()) }()
	select {
	case err := <-initDone:
		if err != nil {
			t.Fatalf("Initialize: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Initialize never returned")
	}

	rec := newRecorder()
	opened, stop := listen(t, c, rec)
	<-opened
	defer stop()

	srv.push <- `{"jsonrpc":"2.0","id":7,"method":"ping"}`
	srv.push <- `{"jsonrpc":"2.0","method":"notifications/claude/channel","params":{"content":"after the ping"}}`
	select {
	case <-rec.got:
	case <-time.After(5 * time.Second):
		t.Fatal("the doorbell after a ping never arrived: the reply's response stalled the reader")
	}
	if content, _, _ := rec.snapshot(); len(content) != 1 || content[0] != "after the ping" {
		t.Errorf("content = %v", content)
	}
}

// TestNullIsNotAString: encoding/json unmarshals a JSON null into a string as
// a silent no-op, so without an explicit check `content: null` and a null meta
// value would pass as "" and fire. REQ "Channel Notification Handling" says
// they MUST be strings.
func TestNullIsNotAString(t *testing.T) {
	for _, tc := range []struct{ params, want string }{
		{`{"content":null}`, "content"},
		{`{"content":"ok","meta":{"todo_id":null}}`, "meta.todo_id"},
	} {
		_, _, err := parseChannelNotification([]byte(tc.params))
		if err == nil {
			t.Errorf("%s was accepted", tc.params)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: reason %q does not name %q", tc.params, err, tc.want)
		}
	}
	// Control: the same shapes with strings are fine.
	if _, _, err := parseChannelNotification([]byte(`{"content":"","meta":{"todo_id":""}}`)); err != nil {
		t.Errorf("empty strings were rejected: %v", err)
	}
}

// TestBatchedMessagesAreEachDispatched: the protocol version this client
// offers (2025-03-26) obliges it to accept JSON-RPC batches. A batch of two
// doorbells is two firings, not one `invalid`.
func TestBatchedMessagesAreEachDispatched(t *testing.T) {
	srv := testserver.New(testserver.Options{})
	defer srv.Close()

	c := New(Options{Source: source(srv.URL, nil), Log: quietLog()})
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	rec := newRecorder()
	opened, stop := listen(t, c, rec)
	<-opened
	defer stop()

	srv.Push(`[{"jsonrpc":"2.0","method":"notifications/claude/channel","params":{"content":"one"}},` +
		`{"jsonrpc":"2.0","method":"notifications/claude/channel","params":{"content":"two"}}]`)
	rec.await(t, 2)

	content, _, invalid := rec.snapshot()
	if len(invalid) != 0 {
		t.Errorf("a batch was counted invalid: %v", invalid)
	}
	if len(content) != 2 || content[0] != "one" || content[1] != "two" {
		t.Errorf("content = %v, want [one two]", content)
	}
}
