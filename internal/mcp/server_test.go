package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stump-wtf/harness/internal/protocol"
)

// session runs one session to EOF and returns what the server wrote.
func session(t *testing.T, d Daemon, allowControl bool, msgs ...string) string {
	t.Helper()
	var in strings.Builder
	for _, m := range msgs {
		in.WriteString(m)
		in.WriteString("\n")
	}
	var out bytes.Buffer
	s := New(d, allowControl, "v0.11.0-test")
	s.SetLogger(func(string, ...any) {})
	if err := s.Serve(context.Background(), strings.NewReader(in.String()), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	return strings.TrimSpace(out.String())
}

// serve drives one session and returns every message the server wrote, keyed by
// the request id it answers. A parse error answers id "null".
func serve(t *testing.T, d Daemon, allowControl bool, msgs ...string) map[string]map[string]any {
	t.Helper()
	answers := map[string]map[string]any{}
	body := session(t, d, allowControl, msgs...)
	if body == "" {
		return answers
	}
	dec := json.NewDecoder(strings.NewReader(body))
	for {
		var msg map[string]any
		if err := dec.Decode(&msg); err != nil {
			break
		}
		answers[idKey(msg["id"])] = msg
	}
	if len(answers) == 0 {
		t.Fatalf("no messages decoded from %q", body)
	}
	return answers
}

// idKey normalises a JSON id (number, string, or absent) into a map key.
func idKey(v any) string {
	switch id := v.(type) {
	case nil:
		return "null"
	case float64:
		return strings.TrimSuffix(formatFloat(id), ".0")
	case string:
		return id
	}
	return "?"
}

func formatFloat(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

func TestInitializeEchoesSupportedRevision(t *testing.T) {
	answers := serve(t, &fakeDaemon{}, false,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test"}}}`)
	res := answers["1"]["result"].(map[string]any)
	if res["protocolVersion"] != "2025-03-26" {
		t.Errorf("protocolVersion = %v, want the client's", res["protocolVersion"])
	}
	info := res["serverInfo"].(map[string]any)
	if info["name"] != ServerName || info["version"] != "v0.11.0-test" {
		t.Errorf("serverInfo = %v", info)
	}
	if res["capabilities"].(map[string]any)["tools"] == nil {
		t.Error("initialize did not advertise tools")
	}
}

func TestInitializeAnswersAnUnknownRevisionWithOurs(t *testing.T) {
	answers := serve(t, &fakeDaemon{}, false,
		`{"jsonrpc":"2.0","id":7,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`)
	res := answers["7"]["result"].(map[string]any)
	if res["protocolVersion"] != ProtocolVersions[0] {
		t.Errorf("protocolVersion = %v, want %v", res["protocolVersion"], ProtocolVersions[0])
	}
}

func TestToolsListMatchesTheServersGrant(t *testing.T) {
	readOnly := serve(t, &fakeDaemon{}, false, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	tools := readOnly["1"]["result"].(map[string]any)["tools"].([]any)
	if len(tools) != len(Tools(false)) {
		t.Fatalf("listed %d tools, want %d", len(tools), len(Tools(false)))
	}
	for _, raw := range tools {
		tool := raw.(map[string]any)
		name, _ := tool["name"].(string)
		if class, ok := ToolClass(name); !ok || class != "read" {
			t.Errorf("a read-only server listed %s (class %q)", name, class)
		}
		if tool["inputSchema"] == nil {
			t.Errorf("%s was listed without an input schema", name)
		}
	}

	control := serve(t, &fakeDaemon{}, true, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if n := len(control["1"]["result"].(map[string]any)["tools"].([]any)); n != len(Tools(true)) {
		t.Fatalf("listed %d tools with --allow-control, want %d", n, len(Tools(true)))
	}
}

func TestToolsCallRoundTrip(t *testing.T) {
	d := &fakeDaemon{harnesses: []protocol.HarnessInfo{{Name: "web", State: "running"}}}
	answers := serve(t, d, false,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"harness_list","arguments":{}}}`)
	msg := answers["3"]
	if msg["error"] != nil {
		t.Fatalf("transport error: %v", msg["error"])
	}
	res := msg["result"].(map[string]any)
	if res["isError"] == true {
		t.Fatalf("the call failed: %v", res)
	}
	content := res["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["type"] != "text" {
		t.Fatalf("content = %v", content)
	}
	if !strings.Contains(content[0].(map[string]any)["text"].(string), "web") {
		t.Errorf("text does not name the harness: %v", content[0])
	}
	if len(d.calls) != 1 || d.calls[0] != "list" {
		t.Fatalf("calls = %v, want one list", d.calls)
	}
}

func TestToolsCallRefusalStaysInsideTheResult(t *testing.T) {
	d := &fakeDaemon{}
	answers := serve(t, d, false,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"harness_stop","arguments":{"name":"web"}}}`)
	msg := answers["4"]
	if msg["error"] != nil {
		t.Fatalf("a refusal must be a tool result, not a transport error: %v", msg["error"])
	}
	if res := msg["result"].(map[string]any); res["isError"] != true {
		t.Fatalf("a refused write answered as a success: %v", res)
	}
	for _, c := range d.calls {
		if strings.HasPrefix(c, "stop") {
			t.Fatalf("the daemon was driven anyway: %v", d.calls)
		}
	}
}

func TestUnknownMethodAndBadJSON(t *testing.T) {
	answers := serve(t, &fakeDaemon{}, false,
		`{"jsonrpc":"2.0","id":5,"method":"resources/read"}`,
		`{not json`)
	errObj, ok := answers["5"]["error"].(map[string]any)
	if !ok || errObj["code"].(float64) != codeMethodNotFound {
		t.Fatalf("unknown method answered %v", answers["5"])
	}
	bad, ok := answers["null"]["error"].(map[string]any)
	if !ok {
		t.Fatal("a malformed line was ignored instead of answered")
	}
	if bad["code"].(float64) != codeParseError {
		t.Errorf("parse error code = %v", bad["code"])
	}
}

func TestPingIsAnswered(t *testing.T) {
	answers := serve(t, &fakeDaemon{}, false, `{"jsonrpc":"2.0","id":9,"method":"ping"}`)
	if answers["9"]["error"] != nil {
		t.Fatalf("ping failed: %v", answers["9"])
	}
}

func TestNotificationsGetNoAnswer(t *testing.T) {
	if out := session(t, &fakeDaemon{}, false, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); out != "" {
		t.Fatalf("a notification produced protocol output: %q", out)
	}
}

func TestRequestsAreAnsweredBeforeEOFReturns(t *testing.T) {
	// Every request is handled in its own goroutine so a waiting tool call
	// cannot stall the session; a client that closes right after its last
	// request must still get that answer.
	d := &fakeDaemon{harnesses: []protocol.HarnessInfo{{Name: "web", State: "running"}}}
	msgs := make([]string, 0, 25)
	for i := range 25 {
		msgs = append(msgs, `{"jsonrpc":"2.0","id":`+itoa(i)+`,"method":"tools/call","params":{"name":"harness_list"}}`)
	}
	answers := serve(t, d, false, msgs...)
	if len(answers) != len(msgs) {
		t.Fatalf("%d of %d requests were answered before the session closed", len(answers), len(msgs))
	}
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}
