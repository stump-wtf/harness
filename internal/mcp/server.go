package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"sync"
)

// ProtocolVersions are the MCP revisions this server speaks, newest first. A
// client that asks for one of them is served that revision; one that asks for
// anything else is answered with the newest, which is what the spec prescribes.
var ProtocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// ServerName is what a client shows as the server's identity.
const ServerName = "harness"

// maxLine bounds one inbound JSON-RPC message. Arguments are small, but a
// client may batch or echo, and a scanner limit lower than the payload would
// silently end the session.
const maxLine = 8 << 20

// JSON-RPC error codes the server can produce.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
)

// Server serves the tool surface over the stdio transport: newline-delimited
// JSON-RPC on stdin and stdout. Everything the server itself has to say (dial
// failures, dropped messages) goes to the logger, never to stdout, because
// stdout is the protocol.
type Server struct {
	daemon       Daemon
	allowControl bool
	version      string
	logf         func(string, ...any)

	// mu serializes writes so concurrent tool calls cannot interleave partial
	// lines on the protocol channel.
	mu sync.Mutex
}

// New builds a server for d. version is harness's own build version, reported
// in serverInfo. Without allowControl the write-class tools are neither listed
// nor callable.
func New(d Daemon, allowControl bool, version string) *Server {
	return &Server{daemon: d, allowControl: allowControl, version: version, logf: func(string, ...any) {}}
}

// SetLogger routes the server's own diagnostics; pass nil to silence them.
func (s *Server) SetLogger(fn func(string, ...any)) {
	if fn == nil {
		fn = func(string, ...any) {}
	}
	s.logf = fn
}

// request is one inbound JSON-RPC message.
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// response is one outbound JSON-RPC message.
type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Serve reads messages until EOF or ctx cancellation. A clean EOF is not an
// error: that is how a client closes the session.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	enc := json.NewEncoder(&lockedWriter{w: out, mu: &s.mu})
	var pending sync.WaitGroup
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64<<10), maxLine)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			s.writeError(enc, nil, codeParseError, "parse error: "+err.Error())
			continue
		}
		if req.Method == "" {
			s.writeError(enc, req.ID, codeInvalidRequest, "missing method")
			continue
		}
		// Notifications carry no id and expect no answer; a request that would
		// block (a waiting trigger call) must not stall the read loop, so it
		// runs in its own goroutine and answers when it is done.
		if len(req.ID) == 0 || string(req.ID) == "null" {
			s.handle(ctx, enc, req)
			continue
		}
		pending.Add(1)
		go func() {
			defer pending.Done()
			s.handle(ctx, enc, req)
		}()
	}
	// An answer still in flight at EOF must reach the pipe before the session
	// closes, or the client waits forever on a call that already succeeded.
	pending.Wait()
	if err := ctx.Err(); err != nil {
		return err
	}
	return sc.Err()
}

// handle answers exactly one message.
func (s *Server) handle(ctx context.Context, enc *json.Encoder, req request) {
	switch req.Method {
	case "initialize":
		s.writeResult(enc, req.ID, s.initialize(req.Params))
	case "notifications/initialized", "initialized", "notifications/cancelled":
		// No response to a notification.
	case "ping":
		s.writeResult(enc, req.ID, map[string]any{})
	case "tools/list":
		s.writeResult(enc, req.ID, s.listTools())
	case "tools/call":
		s.call(ctx, enc, req)
	default:
		s.writeError(enc, req.ID, codeMethodNotFound, "unknown method: "+req.Method)
	}
}

// initialize negotiates the protocol revision: the client's when we speak it,
// otherwise ours.
func (s *Server) initialize(params json.RawMessage) map[string]any {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(orEmpty(params), &p); err != nil {
		s.logf("initialize params: %v", err)
	}
	requested := p.ProtocolVersion
	version := ProtocolVersions[0]
	for _, v := range ProtocolVersions {
		if v == requested {
			version = v
			break
		}
	}
	if requested != "" && version != requested {
		s.logf("client asked for MCP %s; serving %s", requested, version)
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": ServerName, "version": s.version},
	}
}

// listTools reports the registry for this server's control setting.
func (s *Server) listTools() map[string]any {
	tools := tools(s.allowControl)
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		out = append(out, map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"inputSchema": t.InputSchema,
		})
	}
	return map[string]any{"tools": out}
}

// ToolClass reports a tool's read/write class, for docs and tests.
func ToolClass(name string) (string, bool) {
	for _, t := range Tools(true) {
		if t.Name == name {
			return string(t.Class), true
		}
	}
	return "", false
}

// call executes one tool and answers with its content. An unknown tool or a
// refusal is a tool result with isError, not a transport error: the model reads
// the reason and the session stays usable.
func (s *Server) call(ctx context.Context, enc *json.Encoder, req request) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(orEmpty(req.Params), &p); err != nil {
		s.writeError(enc, req.ID, codeInvalidParams, "invalid tools/call params: "+err.Error())
		return
	}
	if p.Name == "" {
		s.writeError(enc, req.ID, codeInvalidParams, "tools/call needs a tool name")
		return
	}
	res, err := Call(ctx, s.daemon, p.Name, p.Arguments, s.allowControl)
	if err != nil {
		s.writeError(enc, req.ID, codeInternal, err.Error())
		return
	}
	s.writeResult(enc, req.ID, res)
}

func (s *Server) writeResult(enc *json.Encoder, id json.RawMessage, result any) {
	if err := enc.Encode(response{JSONRPC: "2.0", ID: idOr(id), Result: result}); err != nil {
		s.logf("write: %v", err)
	}
}

func (s *Server) writeError(enc *json.Encoder, id json.RawMessage, code int, msg string) {
	s.logf("%d: %s", code, msg)
	if err := enc.Encode(response{JSONRPC: "2.0", ID: idOr(id), Error: &rpcError{Code: code, Message: msg}}); err != nil {
		s.logf("write: %v", err)
	}
}

// idOr gives a response an id when the request's is missing, since the
// transport requires one on a response.
func idOr(id json.RawMessage) json.RawMessage {
	if len(id) == 0 || string(id) == "null" {
		return json.RawMessage("null")
	}
	return id
}

// orEmpty treats absent params as an empty object so decoding never trips.
func orEmpty(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return json.RawMessage("{}")
	}
	return raw
}

// lockedWriter serializes encoders across concurrent handlers.
type lockedWriter struct {
	w  io.Writer
	mu *sync.Mutex
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w.Write(p)
}
