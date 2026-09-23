package channel

// The Streamable HTTP client.
//
// Deliberately written by hand rather than on modelcontextprotocol/go-sdk
// v1.7.0, which design.md records as hostile to this exact use in two ways,
// both found the hard way in the Crush fork's channel work:
//
//  1. It rejects unknown JSON-RPC methods — `notifications/claude/channel`
//     among them — before any handler or middleware runs, so a custom
//     notification can only be observed below the SDK's connection.
//  2. It opens the standalone SSE stream only when a type assertion on its own
//     connection type succeeds. Wrapping the connection to intercept
//     notifications therefore silently prevents the stream from ever opening,
//     and every doorbell is then rejected server-side as "stream not
//     connected".
//
// What is left is a few hundred lines: initialize, initialized, a GET, a
// DELETE, and replies to server requests.
//
// Governing: ADR-0021, ADR-0008; SPEC-0014 REQ "Channel Listener Session",
// REQ "Credential Resolution", Security Requirements § "Redirect Validation".
//
// @joestump 09/23/2026 - Introduced with the SPEC-0014 channel listener (#471).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"charm.land/log/v2"

	"github.com/stump-wtf/harness/internal/buildinfo"
	"github.com/stump-wtf/harness/internal/core"
)

// Sentinel errors for the failure modes a caller needs to tell apart rather
// than merely report. REQ "Error Handling Standards" names them, and the
// reconnection story (#474) keys its behaviour off exactly these: an auth
// failure or a missing capability retries at the ceiling, everything else
// backs off normally.
var (
	// ErrNoChannelCapability: the server answered initialize without
	// capabilities.experimental["claude/channel"]. Retrying at speed will not
	// help — the server is not a channel server.
	ErrNoChannelCapability = errors.New("server does not advertise the claude/channel capability")
	// ErrStreamRefused: the server answered the standalone GET with 405. It
	// has no server-initiated messages to send, so there is nothing to hear.
	ErrStreamRefused = errors.New("server refused the notification stream")
	// ErrSessionExpired: the server no longer knows this Mcp-Session-Id (404
	// on a request that carried one). The session must be re-initialized.
	ErrSessionExpired = errors.New("the MCP session expired")
	// ErrUnauthorized: 401 or 403. The credential is wrong or revoked, and a
	// fast retry loop against an auth endpoint is its own problem.
	ErrUnauthorized = errors.New("the server rejected the session credentials")
	// ErrProtocol: the server spoke something this client cannot follow.
	ErrProtocol = errors.New("the server violated the MCP Streamable HTTP protocol")
)

// ProtocolVersion is the MCP protocol version this client offers. Streamable
// HTTP is defined from 2025-03-26, which is the floor REQ "Channel Listener
// Session" sets.
const ProtocolVersion = "2025-03-26"

// ClientName identifies the daemon to a channel server. It appears in the
// server's own session list, so it is the daemon's name rather than a library
// name.
const ClientName = "harness"

// channelCapability is the experimental capability key a channel server must
// advertise, and which this client offers as a marker.
const channelCapability = "claude/channel"

// handshakeTimeout bounds initialize plus notifications/initialized. The
// transport's ResponseHeaderTimeout covers only the response head; a server
// that sends the head and then stalls would otherwise park the source in
// `connecting` for as long as the daemon runs.
const handshakeTimeout = 30 * time.Second

// replyTimeout bounds one reply to a server request. Replies are POSTed from
// the goroutine that reads doorbells, so an unbounded one would stop the
// stream being read.
const replyTimeout = 10 * time.Second

// maxDrain is the most of a response body this client reads only to discard
// it, so a keep-alive connection can be reused.
const maxDrain = 4096

// Options configure a Client.
type Options struct {
	// Source is the [channel.*] table this session serves.
	Source core.ChannelSource
	// HTTP is the transport. Defaults to a client with sane timeouts and
	// redirect validation; a caller supplying its own is responsible for
	// both.
	HTTP *http.Client
	// Log is where session events are reported. Credentials never reach it.
	Log *log.Logger
	// Now is the clock, for tests. Defaults to time.Now.
	Now func() time.Time
}

// Client is one listen-only MCP session.
type Client struct {
	src  core.ChannelSource
	http *http.Client
	log  *log.Logger

	mu        sync.Mutex
	sessionID string
	protocol  string
	lastID    string
}

// New builds a Client. It opens nothing until Initialize.
func New(opts Options) *Client {
	logger := opts.Log
	if logger == nil {
		logger = log.Default()
	}
	hc := opts.HTTP
	if hc == nil {
		hc = NewHTTPClient(opts.Source.URL)
	}
	return &Client{src: opts.Source, http: hc, log: logger}
}

// NewHTTPClient builds the transport a channel session uses: no redirect to a
// different origin, and timeouts that bound a connect without bounding the
// stream.
//
// The redirect rule is a security requirement, not hygiene. Go's default
// CheckRedirect copies most headers across a redirect, so a server (or anyone
// who can answer for it) could move the session's bearer credential to an
// origin the operator never named by answering `initialize` with a 302. Same
// scheme AND same host, or the redirect is refused.
//
// Timeout is deliberately unset: the whole point of the GET is a stream that
// stays open for hours. ResponseHeaderTimeout bounds the part that must be
// quick — waiting for the response head — and leaves the body unbounded.
// Governing: SPEC-0014 Security Requirements § "Redirect Validation".
func NewHTTPClient(rawURL string) *http.Client {
	base := core.NormalizeEndpoint(rawURL)
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			IdleConnTimeout:       90 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) == 0 {
				return nil
			}
			origin := via[0].URL
			if req.URL.Scheme != origin.Scheme || !strings.EqualFold(req.URL.Host, origin.Host) {
				return fmt.Errorf("%w: refused a redirect from %s to %s://%s (a redirect must not move the session's credentials to another origin)",
					ErrProtocol, base, req.URL.Scheme, req.URL.Host)
			}
			if len(via) >= 5 {
				return fmt.Errorf("%w: too many redirects", ErrProtocol)
			}
			return nil
		},
	}
}

// SessionID is the Mcp-Session-Id the server issued, or "" before initialize.
func (c *Client) SessionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessionID
}

// LastEventID is the most recent SSE event id the stream carried, for
// resumption (#474).
func (c *Client) LastEventID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastID
}

// initializeResult is the subset of the initialize response this client reads.
type initializeResult struct {
	ProtocolVersion string `json:"protocolVersion"`
	Capabilities    struct {
		Experimental map[string]json.RawMessage `json:"experimental"`
	} `json:"capabilities"`
	ServerInfo struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"serverInfo"`
}

// Initialize performs steps 1 to 3 of REQ "Channel Listener Session":
// initialize, the capability check, and notifications/initialized.
//
// The capability check is a hard refusal rather than a warning. A server
// without `claude/channel` has no doorbells to send, so a session to it would
// sit in `connected` forever and never fire — which looks healthier than being
// broken, and is worse.
func (c *Client) Initialize(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()

	body, err := json.Marshal(message{
		JSONRPC: jsonrpcVersion,
		ID:      json.RawMessage(`1`),
		Method:  "initialize",
		Params: mustJSON(map[string]any{
			"protocolVersion": ProtocolVersion,
			// The marker is OPTIONAL per the requirement. Offering it lets a
			// server that cares tell a channel consumer from an ordinary
			// tool client in its own logs.
			"capabilities": map[string]any{
				"experimental": map[string]any{channelCapability: map[string]any{}},
			},
			"clientInfo": map[string]any{"name": ClientName, "version": buildinfo.Version},
		}),
	})
	if err != nil {
		return fmt.Errorf("channel %s: encode initialize: %w", c.src.Name, err)
	}

	resp, err := c.post(ctx, body, false)
	if err != nil {
		return fmt.Errorf("channel %s: initialize: %w", c.src.Name, err)
	}
	// Closed, not drained: a server may answer on a stream and then hold it
	// open (the transport says it SHOULD close it, not MUST), and waiting for
	// its end would wait for as long as the server likes.
	defer discardBody(resp)

	// The session id arrives on the initialize response and rides every later
	// request.
	if id := resp.Header.Get("Mcp-Session-Id"); id != "" {
		c.mu.Lock()
		c.sessionID = id
		c.mu.Unlock()
	}

	msg, err := c.readOneMessage(resp)
	if err != nil {
		return fmt.Errorf("channel %s: initialize: %w", c.src.Name, err)
	}
	if msg.Error != nil {
		return fmt.Errorf("channel %s: initialize: %w: %s", c.src.Name, ErrProtocol, msg.Error.Message)
	}
	var res initializeResult
	if err := json.Unmarshal(msg.Result, &res); err != nil {
		return fmt.Errorf("channel %s: initialize: %w: result is not an initialize result", c.src.Name, ErrProtocol)
	}
	if _, ok := res.Capabilities.Experimental[channelCapability]; !ok {
		return fmt.Errorf("channel %s: %w (server %q %q offers %s)",
			c.src.Name, ErrNoChannelCapability, res.ServerInfo.Name, res.ServerInfo.Version, experimentalKeys(res.Capabilities.Experimental))
	}

	negotiated := res.ProtocolVersion
	if negotiated == "" {
		negotiated = ProtocolVersion
	}
	c.mu.Lock()
	c.protocol = negotiated
	c.mu.Unlock()

	initialized, err := json.Marshal(message{JSONRPC: jsonrpcVersion, Method: "notifications/initialized"})
	if err != nil {
		return fmt.Errorf("channel %s: encode initialized: %w", c.src.Name, err)
	}
	iresp, err := c.post(ctx, initialized, true)
	if err != nil {
		return fmt.Errorf("channel %s: notifications/initialized: %w", c.src.Name, err)
	}
	discardBody(iresp)

	c.log.Info("channel session initialized",
		"source", c.src.Name, "server", res.ServerInfo.Name, "protocol", negotiated)
	return nil
}

// Handler receives a validated doorbell. content and meta are opaque: the
// daemon stores them in the envelope and interprets nothing.
type Handler interface {
	// Notification is called once per valid `notifications/claude/channel`.
	Notification(content string, meta map[string]string)
	// Invalid is called for a notification that violated REQ "Channel
	// Notification Handling". It fires nothing; the reason is for the log and
	// the counter.
	Invalid(reason string)
}

// Listen opens the standalone GET stream and reads it until it ends, ctx is
// cancelled, or an error stops it. onOpen is called once the stream is being
// read — which is the ONLY moment a source may be reported `connected`.
//
// A clean end of stream returns nil. That is not success: it means the server
// closed, and the caller reconnects (#474).
func (c *Client) Listen(ctx context.Context, h Handler, onOpen func()) error {
	req, err := c.newRequest(ctx, http.MethodGet, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	if last := c.LastEventID(); last != "" {
		req.Header.Set("Last-Event-ID", last)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("channel %s: open stream: %w", c.src.Name, err)
	}
	defer discardBody(resp)

	if resp.StatusCode == http.StatusMethodNotAllowed {
		return fmt.Errorf("channel %s: %w (405 on the standalone GET)", c.src.Name, ErrStreamRefused)
	}
	if err := c.statusError(resp, "open stream"); err != nil {
		return err
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(strings.ToLower(ct), "text/event-stream") {
		return fmt.Errorf("channel %s: open stream: %w: content type %q is not text/event-stream", c.src.Name, ErrProtocol, ct)
	}

	// The stream is open and about to be read. Everything before this point
	// could succeed on a server with nothing to say; this is the moment
	// `connected` becomes true, and the reason the state machine is driven by
	// observed events rather than by a health poll.
	if onOpen != nil {
		onOpen()
	}

	dec := NewDecoder(resp.Body)
	for {
		ev, ok := dec.Next()
		if !ok {
			break
		}
		c.mu.Lock()
		c.lastID = dec.LastID()
		c.mu.Unlock()
		c.dispatch(ctx, ev, h)
	}
	if err := dec.Err(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("channel %s: read stream: %w", c.src.Name, err)
	}
	return ctx.Err()
}

// dispatch decodes one SSE event and acts on it.
func (c *Client) dispatch(ctx context.Context, ev Event, h Handler) {
	var msg message
	if err := json.Unmarshal([]byte(ev.Data), &msg); err != nil {
		// Not JSON-RPC at all. Logged and dropped: the payload is never
		// echoed, because a stream is as untrusted as any other input.
		c.log.Warn("channel message dropped: not valid JSON-RPC", "source", c.src.Name, "err", err.Error())
		if h != nil {
			h.Invalid("not valid JSON-RPC")
		}
		return
	}
	switch {
	case msg.isRequest():
		c.answer(ctx, msg)
	case msg.isNotification():
		if msg.Method != ChannelNotification {
			// Other notifications are ignored, not counted invalid: a server
			// is entitled to send them, and this client simply has no use
			// for them.
			c.log.Debug("channel notification ignored", "source", c.src.Name, "method", msg.Method)
			return
		}
		content, meta, err := parseChannelNotification(msg.Params)
		if err != nil {
			// The reason names the offending field and never the value.
			c.log.Warn("channel doorbell dropped", "source", c.src.Name, "err", err.Error())
			if h != nil {
				h.Invalid(err.Error())
			}
			return
		}
		if h != nil {
			h.Notification(content, meta)
		}
	default:
		// A response to a request we never sent. Nothing to do.
		c.log.Debug("channel response ignored", "source", c.src.Name)
	}
}

// answer replies to a server request. `ping` gets an empty result; everything
// else gets -32601, because this client serves nothing.
//
// The reply is POSTed and its body discarded unread when it is a stream or of
// unknown length: a server that answers a reply with a stream has
// misunderstood, and reading it would block the one goroutine that is meant
// to be reading doorbells. replyTimeout bounds the rest.
func (c *Client) answer(ctx context.Context, req message) {
	var reply message
	if req.Method == "ping" {
		reply = emptyResult(req.ID)
	} else {
		reply = notImplemented(req.ID, req.Method)
		c.log.Debug("channel refused a server request", "source", c.src.Name, "method", req.Method)
	}
	body, err := json.Marshal(reply)
	if err != nil {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, replyTimeout)
	defer cancel()
	resp, err := c.post(rctx, body, true)
	if err != nil {
		c.log.Debug("channel reply failed", "source", c.src.Name, "method", req.Method, "err", err.Error())
		return
	}
	discardBody(resp)
}

// Close ends the session with DELETE. It is best-effort: the session is over
// either way, and a server that has already forgotten it answers 404.
func (c *Client) Close(ctx context.Context) error {
	if c.SessionID() == "" {
		return nil
	}
	req, err := c.newRequest(ctx, http.MethodDelete, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("channel %s: delete session: %w", c.src.Name, err)
	}
	discardBody(resp)
	return nil
}

// post sends a JSON-RPC body. accepted is true for a fire-and-forget POST
// (notifications and replies), which a server answers 202 to.
func (c *Client) post(ctx context.Context, body []byte, accepted bool) (*http.Response, error) {
	req, err := c.newRequest(ctx, http.MethodPost, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	// Both, per the transport: a server may answer a POST with a single JSON
	// response or with a stream.
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if accepted && (resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusNoContent) {
		return resp, nil
	}
	if err := c.statusError(resp, "request"); err != nil {
		discardBody(resp)
		return nil, err
	}
	return resp, nil
}

// statusError maps a response status to one of the sentinels, or nil.
func (c *Client) statusError(resp *http.Response, what string) error {
	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("channel %s: %s: %w (%s)", c.src.Name, what, ErrUnauthorized, resp.Status)
	case resp.StatusCode == http.StatusNotFound && c.SessionID() != "":
		return fmt.Errorf("channel %s: %s: %w", c.src.Name, what, ErrSessionExpired)
	case resp.StatusCode >= 400:
		return fmt.Errorf("channel %s: %s: server answered %s", c.src.Name, what, resp.Status)
	}
	return nil
}

// newRequest builds a request carrying the source's headers, the session id
// and the negotiated protocol version.
//
// The source's headers are applied LAST so an operator's `Authorization`
// cannot be clobbered by a default — and the session headers are set before
// them for the same reason in the other direction: a source that set
// `Mcp-Session-Id` by hand would be overriding the server's own answer, which
// is never what they meant.
func (c *Client) newRequest(ctx context.Context, method string, body []byte) (*http.Request, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.src.URL, r)
	if err != nil {
		return nil, fmt.Errorf("channel %s: build %s request: %w", c.src.Name, method, err)
	}
	c.mu.Lock()
	sessionID, protocol := c.sessionID, c.protocol
	c.mu.Unlock()
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	if protocol != "" {
		req.Header.Set("MCP-Protocol-Version", protocol)
	}
	for name, value := range c.src.Headers {
		// Reveal at the point of use, and nowhere else. The value is a
		// core.Secret everywhere it is stored or formatted.
		req.Header.Set(name, value.Reveal())
	}
	return req, nil
}

// readOneMessage reads a single JSON-RPC response from a POST reply, whether
// the server answered with JSON or with a one-message stream.
func (c *Client) readOneMessage(resp *http.Response) (message, error) {
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.HasPrefix(ct, "text/event-stream") {
		dec := NewDecoder(resp.Body)
		for {
			ev, ok := dec.Next()
			if !ok {
				if err := dec.Err(); err != nil {
					return message{}, err
				}
				return message{}, fmt.Errorf("%w: the response stream carried no message", ErrProtocol)
			}
			var msg message
			if err := json.Unmarshal([]byte(ev.Data), &msg); err != nil {
				continue
			}
			if len(msg.Result) > 0 || msg.Error != nil {
				return msg, nil
			}
		}
	}
	var msg message
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxParamsBytes)).Decode(&msg); err != nil {
		return message{}, fmt.Errorf("%w: response is not JSON-RPC: %v", ErrProtocol, err)
	}
	return msg, nil
}

// discardBody closes a response this client has finished with. It drains the
// body first only when that is known to be short — a declared length within
// maxDrain, and not a stream — so the connection can be reused. Anything else
// is closed unread: draining a stream means waiting for the server to end it,
// and a server is free not to.
func discardBody(resp *http.Response) {
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if resp.ContentLength >= 0 && resp.ContentLength <= maxDrain && !strings.HasPrefix(ct, "text/event-stream") {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrain))
	}
	_ = resp.Body.Close()
}

// experimentalKeys renders the capability keys a server DID offer, so the
// error says what was found as well as what was missing.
func experimentalKeys(m map[string]json.RawMessage) string {
	if len(m) == 0 {
		return "no experimental capabilities"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return strings.Join(keys, ", ")
}

// mustJSON marshals a value that cannot fail to marshal (a literal map of
// strings), so the call sites above stay readable.
func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}
