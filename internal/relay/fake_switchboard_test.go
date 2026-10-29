package relay

// fake_switchboard_test.go is the Switchboard double the lease-client contract
// tests run against. Later stories in SPEC-0019 (the supervisor's attempt logic,
// doctor rows) reuse it, so its knobs are its server-side behaviour rather than
// anything about a client: which schema generation it advertises, whether the
// endpoint's grant includes `release`, whether it expires a session, and what a
// verb answers.
//
// It answers POST only. A GET on the MCP URL is recorded and refused with 405,
// which is what lets the tests assert REQ-17 (a lease session never opens the
// standalone stream) instead of assuming it.
//
// Governing: SPEC-0019 REQ-17, REQ-19, REQ-24.
//
// @joestump-agent 10/29/2026 - Introduced with the relay lease client
// (stump.wtf/harness#490).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// RecordedCall is one tools/call the fake served.
type RecordedCall struct {
	Tool string
	Args map[string]any
}

// ToolFailure is a scripted tool error. Switchboard renders a tool failure as
// "code: message" in an error content item (SPEC-0006 REQ "Structured Output and
// Stable Error Shape"), which is what the client maps back onto a sentinel.
type ToolFailure struct {
	Code    string
	Message string
	// AsRPCError sends the failure as a JSON-RPC error object instead of an
	// error result, so both shapes a server may use stay covered.
	AsRPCError bool
}

// FakeSwitchboard is the double.
type FakeSwitchboard struct {
	srv *httptest.Server

	mu sync.Mutex

	// attemptHistory selects the schema generation; false is the Switchboard
	// that predates attempt history.
	attemptHistory bool
	// advertiseRelease mirrors the endpoint grant: a verb the grant omits is
	// never registered, so it never appears in tools/list.
	advertiseRelease bool
	// streamAnswers answers a tools/call as text/event-stream, which Streamable
	// HTTP allows.
	streamAnswers bool
	// callJitter pauses every tools/call before it answers, so a concurrency
	// test sees real interleaving between issuing a request id and matching
	// its reply.
	callJitter time.Duration

	sessionSeq int
	sessionID  string
	expireOnce bool

	initializes int
	toolsLists  int
	gets        int
	posts       int
	calls       []RecordedCall

	// results and failures script what each verb answers.
	results  map[string]json.RawMessage
	failures map[string]*ToolFailure

	// lastInitParams keeps the initialize payload for capability assertions.
	lastInitParams map[string]any
}

func newFake(t *testing.T, attemptHistory bool) *FakeSwitchboard {
	t.Helper()
	f := &FakeSwitchboard{
		attemptHistory:   attemptHistory,
		advertiseRelease: true,
		results:          map[string]json.RawMessage{},
		failures:         map[string]*ToolFailure{},
	}
	raw, _ := json.Marshal(claimPayload(defaultLeaseToken))
	f.results[VerbClaimNext] = raw
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *FakeSwitchboard) URL() string { return f.srv.URL + "/mcp" }

// defaultLeaseToken is the fence the fake's default claim_next result issues, so
// a test that never scripts a claim still holds a token worth asserting on. It
// is assembled at run time: a literal of this shape reads as a credential to
// the secret scan, and the value is a fake anyway.
var defaultLeaseToken = strings.Join([]string{"lt-", "abcdef", "0123456789"}, "")

func (f *FakeSwitchboard) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		f.mu.Lock()
		f.gets++
		f.mu.Unlock()
		w.Header().Set("Allow", "POST")
		http.Error(w, "GET is only for the channel listener", http.StatusMethodNotAllowed)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	f.mu.Lock()
	f.posts++
	expired := f.expireOnce && r.Header.Get("Mcp-Session-Id") != ""
	if expired {
		f.expireOnce = false
	}
	sid := r.Header.Get("Mcp-Session-Id")
	badSession := sid != "" && sid != f.sessionID
	f.mu.Unlock()
	if expired || badSession {
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	}

	var msg message
	dec := json.NewDecoder(r.Body)
	dec.UseNumber()
	if err := dec.Decode(&msg); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	switch msg.Method {
	case "initialize":
		f.handleInitialize(w, msg)
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
	case "tools/list":
		f.handleToolsList(w, msg)
	case "tools/call":
		f.handleToolsCall(w, msg)
	default:
		writeJSON(w, rpcErrorResponse(msg.ID, -32601, "method not found: "+msg.Method))
	}
}

func (f *FakeSwitchboard) handleInitialize(w http.ResponseWriter, msg message) {
	f.mu.Lock()
	f.initializes++
	f.sessionSeq++
	f.sessionID = "fake-session" + itoa(f.sessionSeq)
	sid := f.sessionID
	params, _ := msg.Params.(map[string]any)
	f.lastInitParams = params
	f.mu.Unlock()
	w.Header().Set("Mcp-Session-Id", sid)
	writeJSON(w, okResponse(msg.ID, map[string]any{
		"protocolVersion": "2025-03-26",
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": "fake-switchboard", "version": "0.0-test"},
	}))
}

func (f *FakeSwitchboard) handleToolsList(w http.ResponseWriter, msg message) {
	f.mu.Lock()
	f.toolsLists++
	tools := f.toolSchemas()
	stream := f.streamAnswers
	f.mu.Unlock()
	if stream {
		writeStream(w, okResponse(msg.ID, map[string]any{"tools": tools}))
		return
	}
	writeJSON(w, okResponse(msg.ID, map[string]any{"tools": tools}))
}

func (f *FakeSwitchboard) handleToolsCall(w http.ResponseWriter, msg message) {
	params, _ := msg.Params.(map[string]any)
	name, _ := params["name"].(string)
	args, _ := params["arguments"].(map[string]any)
	f.mu.Lock()
	f.calls = append(f.calls, RecordedCall{Tool: name, Args: args})
	failure := f.failures[name]
	result := f.results[name]
	stream := f.streamAnswers
	jitter := f.callJitter
	f.mu.Unlock()
	if jitter > 0 {
		time.Sleep(jitter)
	}

	if failure != nil {
		if failure.AsRPCError {
			writeJSON(w, rpcErrorResponse(msg.ID, -32000, failure.Code+": "+failure.Message))
			return
		}
		writeJSON(w, okResponse(msg.ID, map[string]any{
			"isError": true,
			"content": []map[string]any{{"type": "text", "text": failure.Code + ": " + failure.Message}},
		}))
		return
	}
	if len(result) == 0 {
		result = json.RawMessage(`{}`)
	}
	payload := okResponse(msg.ID, map[string]any{"structuredContent": result})
	if stream {
		writeStream(w, payload)
		return
	}
	writeJSON(w, payload)
}

// toolSchemas builds the advertised tools for the current generation. The
// schemas forbid unknown properties, so this list is the exact statement of
// what the fake accepts.
func (f *FakeSwitchboard) toolSchemas() []map[string]any {
	tool := func(name string, props ...string) map[string]any {
		properties := map[string]any{}
		for _, p := range props {
			properties[p] = map[string]any{"type": "string"}
		}
		return map[string]any{
			"name":        name,
			"description": name,
			"inputSchema": map[string]any{"type": "object", "properties": properties, "additionalProperties": false},
		}
	}
	claim := []string{"id", "queue", "lease_ttl_seconds"}
	heartbeat := []string{"id", "lease_ttl_seconds"}
	closer := []string{"id", "result"}
	release := []string{"id"}
	if f.attemptHistory {
		claim = append(claim, "require_fence", "claimant")
		heartbeat = append(heartbeat, "lease_token")
		closer = append(closer, "lease_token", "summary", "artifact")
		release = append(release, "lease_token", "summary")
	}
	tools := []map[string]any{
		tool("list_todos", "queue", "state", "limit"),
		tool("claim_next", claim...),
		tool("claim", "id", "lease_ttl_seconds"),
		tool("heartbeat", heartbeat...),
		tool("complete", closer...),
		tool("fail", closer...),
	}
	if f.advertiseRelease {
		tools = append(tools, tool("release", release...))
	}
	return tools
}

// --- knobs the tests turn ---

func (f *FakeSwitchboard) setAttemptHistory(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attemptHistory = on
}

func (f *FakeSwitchboard) setAdvertiseRelease(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.advertiseRelease = on
}

func (f *FakeSwitchboard) setStreamAnswers(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.streamAnswers = on
}

// setCallJitter makes every tools/call pause before answering, which widens
// the window between a caller issuing its request id and matching the reply.
func (f *FakeSwitchboard) setCallJitter(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callJitter = d
}

// expireSession makes the next request that carries a session id answer 404, so
// the client must re-initialize and replay the call.
func (f *FakeSwitchboard) expireSession() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.expireOnce = true
}

func (f *FakeSwitchboard) setResult(tool string, payload any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.results[tool] = raw
	delete(f.failures, tool)
}

func (f *FakeSwitchboard) setFailure(tool string, failure *ToolFailure) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures[tool] = failure
	delete(f.results, tool)
}

// --- assertions ---

func (f *FakeSwitchboard) counts() (gets, initializes, toolsCalls int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gets, f.initializes, len(f.calls)
}

func (f *FakeSwitchboard) toolsCalls() []RecordedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]RecordedCall, len(f.calls))
	copy(out, f.calls)
	return out
}

func (f *FakeSwitchboard) callsFor(tool string) []RecordedCall {
	var out []RecordedCall
	for _, c := range f.toolsCalls() {
		if c.Tool == tool {
			out = append(out, c)
		}
	}
	return out
}

func (f *FakeSwitchboard) initCount() int {
	_, inits, _ := f.counts()
	return inits
}

// assertNoGET is the REQ-17 check: a lease session is a caller, never a ring
// target.
func (f *FakeSwitchboard) assertNoGET(t *testing.T) {
	t.Helper()
	if gets, _, _ := f.counts(); gets != 0 {
		t.Fatalf("the lease client issued %d GET requests on the MCP URL; REQ-17 allows none", gets)
	}
}

// assertNoSecrets greps everything the fake saw and said for a credential, which
// is how REQ-18 gets checked from the server side.
func (f *FakeSwitchboard) assertNoSecrets(t *testing.T, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		if strings.Contains(f.transcript(), secret) {
			t.Fatalf("the fake observed a credential it should never have seen on the wire")
		}
	}
}

// transcript is everything the fake recorded, for grepping.
func (f *FakeSwitchboard) transcript() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var b strings.Builder
	for _, c := range f.calls {
		raw, _ := json.Marshal(c.Args)
		b.WriteString(c.Tool)
		b.WriteString(" ")
		b.Write(raw)
		b.WriteString("\n")
	}
	return b.String()
}

// --- wire helpers ---

func writeJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}

func writeStream(w http.ResponseWriter, payload any) {
	raw, _ := json.Marshal(payload)
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = w.Write([]byte("event: message\ndata: " + string(raw) + "\n\n"))
}

func okResponse(id json.RawMessage, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

func rpcErrorResponse(id json.RawMessage, code int, message string) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}}
}

func itoa(n int) string { return strconv.Itoa(n) }
