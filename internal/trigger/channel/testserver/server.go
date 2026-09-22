// Package testserver is a fake MCP Streamable HTTP channel server that records
// every request it receives.
//
// The recording is the point. The interesting claims about the channel
// listener are NEGATIVE — it never calls a tool, never lists prompts, never
// follows a redirect to another origin — and a negative claim can only be
// checked against a log of what was actually sent. A test that asserted on the
// client's own state would pass against a client that sent whatever it liked.
//
// Governing: ADR-0021; SPEC-0014 REQ "Channel Listener Session", REQ "Channel
// Notification Handling".
//
// @joestump 09/23/2026 - Introduced with the SPEC-0014 channel listener (#471).
package testserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
)

// Request is one request the server received.
type Request struct {
	// Method is the HTTP method.
	Method string
	// RPCMethod is the JSON-RPC method for a POST, empty otherwise.
	RPCMethod string
	// Header is a copy of the request's headers.
	Header http.Header
	// Body is the raw request body.
	Body []byte
}

// Options configure a Server.
type Options struct {
	// NoChannelCapability makes initialize answer without
	// capabilities.experimental["claude/channel"].
	NoChannelCapability bool
	// RefuseStream makes the standalone GET answer 405.
	RefuseStream bool
	// Unauthorized makes every request answer 401.
	Unauthorized bool
	// SessionID is the Mcp-Session-Id to issue. Defaults to "sess-1".
	SessionID string
	// StreamResponses answers POSTs with a one-message SSE stream instead of
	// JSON, which is the other shape the transport allows.
	StreamResponses bool
}

// Server is a fake channel server.
type Server struct {
	*httptest.Server

	opts Options

	mu       sync.Mutex
	requests []Request
	// streams is the set of open GET stream writers, so a test can push a
	// notification into a live stream.
	streams []chan string
	opened  chan struct{}
}

// New starts a fake channel server. Close it with Close.
func New(opts Options) *Server {
	if opts.SessionID == "" {
		opts.SessionID = "sess-1"
	}
	s := &Server{opts: opts, opened: make(chan struct{}, 8)}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

// Requests returns a copy of everything received so far.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Request, len(s.requests))
	copy(out, s.requests)
	return out
}

// RPCMethods returns the JSON-RPC methods received, in order.
func (s *Server) RPCMethods() []string {
	var out []string
	for _, r := range s.Requests() {
		if r.RPCMethod != "" {
			out = append(out, r.RPCMethod)
		}
	}
	return out
}

// HTTPMethods returns the HTTP methods received, in order.
func (s *Server) HTTPMethods() []string {
	var out []string
	for _, r := range s.Requests() {
		out = append(out, r.Method)
	}
	return out
}

// StreamOpened blocks until a GET stream is opened, or returns false if the
// server closes first.
func (s *Server) StreamOpened() <-chan struct{} { return s.opened }

// Push writes a raw SSE `data:` payload to every open stream.
func (s *Server) Push(data string) {
	s.mu.Lock()
	streams := make([]chan string, len(s.streams))
	copy(streams, s.streams)
	s.mu.Unlock()
	for _, ch := range streams {
		select {
		case ch <- data:
		default:
		}
	}
}

// PushNotification writes a `notifications/claude/channel` with the given raw
// params JSON.
func (s *Server) PushNotification(params string) {
	s.Push(fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/claude/channel","params":%s}`, params))
}

// PushRequest writes a server request expecting a reply.
func (s *Server) PushRequest(id int, method string) {
	s.Push(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q}`, id, method))
}

// CloseStreams ends every open GET stream, as a server restart would.
func (s *Server) CloseStreams() {
	s.mu.Lock()
	streams := s.streams
	s.streams = nil
	s.mu.Unlock()
	for _, ch := range streams {
		close(ch)
	}
}

func (s *Server) record(r *http.Request, body []byte) {
	req := Request{Method: r.Method, Header: r.Header.Clone(), Body: body}
	if len(body) > 0 {
		var m struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &m)
		req.RPCMethod = m.Method
	}
	s.mu.Lock()
	s.requests = append(s.requests, req)
	s.mu.Unlock()
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	var body []byte
	if r.Body != nil {
		buf := make([]byte, 0, 1024)
		tmp := make([]byte, 1024)
		for {
			n, err := r.Body.Read(tmp)
			buf = append(buf, tmp[:n]...)
			if err != nil {
				break
			}
		}
		body = buf
	}
	s.record(r, body)

	if s.opts.Unauthorized {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	switch r.Method {
	case http.MethodPost:
		s.handlePost(w, body)
	case http.MethodGet:
		s.handleGet(w, r)
	case http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Server) handlePost(w http.ResponseWriter, body []byte) {
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	_ = json.Unmarshal(body, &m)
	if m.Method != "initialize" {
		// A notification or a reply: accepted, no body.
		w.WriteHeader(http.StatusAccepted)
		return
	}

	caps := map[string]any{}
	if !s.opts.NoChannelCapability {
		caps["experimental"] = map[string]any{"claude/channel": map[string]any{}}
	} else {
		caps["experimental"] = map[string]any{"something/else": map[string]any{}}
	}
	result, _ := json.Marshal(map[string]any{
		"protocolVersion": "2025-03-26",
		"capabilities":    caps,
		"serverInfo":      map[string]any{"name": "fake-switchboard", "version": "0.0.1"},
	})
	resp, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(m.ID), "result": json.RawMessage(result)})

	w.Header().Set("Mcp-Session-Id", s.opts.SessionID)
	if s.opts.StreamResponses {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "data: %s\n\n", resp)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(resp)
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	if s.opts.RefuseStream {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	ch := make(chan string, 16)
	s.mu.Lock()
	s.streams = append(s.streams, ch)
	s.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	select {
	case s.opened <- struct{}{}:
	default:
	}

	for {
		select {
		case data, open := <-ch:
			if !open {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}
