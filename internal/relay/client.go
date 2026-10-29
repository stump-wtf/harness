// Package relay holds the client the supervisor uses to talk to a Switchboard
// lease source.
//
// It is deliberately narrow: one MCP Streamable HTTP session per lease source,
// used only for `tools/list` and `tools/call`. The session is POST-only: it
// never opens the standalone GET notification stream, so Switchboard never
// treats Harness as a doorbell target, and it never advertises the
// `claude/channel` capability. The channel listener (SPEC-0014) is the thing
// that hears doorbells; a lease client that also listened would be a second
// ring target for the same URL.
//
// Before any drain verb is sent the client reads the server's own tool schemas
// and checks them against the attempt-history contract below. A server whose
// schemas predate attempt history is marked unsupported and refused outright:
// there is no reduced argument set, no unfenced claim, and no fallback, because
// the fence is what stops an agent holding the same endpoint credential from
// closing another attempt's todo.
//
// Governing: ADR-0025 (supervisor-held leases and relay attempts); SPEC-0019
// REQ-17 (Lease Client Session), REQ-18 (Credential and Token Handling),
// REQ-19 (Required Switchboard Capabilities), REQ-24 (Error Handling
// Standards).
//
// @joestump-agent 10/29/2026 - Introduced with the relay lease client
// (stump.wtf/harness#490).
package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"charm.land/log/v2"

	"github.com/stump-wtf/harness/internal/buildinfo"
	"github.com/stump-wtf/harness/internal/trigger/channel"
)

// Drain verbs, by the name Switchboard registers them under. Anything outside
// this set is not this client's business: the lease client calls no other tool.
const (
	VerbClaimNext = "claim_next"
	VerbHeartbeat = "heartbeat"
	VerbComplete  = "complete"
	VerbFail      = "fail"
	VerbRelease   = "release"
)

// requiredArguments is the attempt-history contract this client insists on,
// keyed by verb. Switchboard's tool schemas forbid unknown properties, so an
// argument listed here is a statement of what the server will actually accept;
// reading the schemas is exact, and reading the server's version string would
// not be (SPEC-0019 REQ-19).
var requiredArguments = map[string][]string{
	VerbClaimNext: {"require_fence", "claimant"},
	VerbHeartbeat: {"lease_token"},
	VerbComplete:  {"lease_token", "summary", "artifact"},
	VerbFail:      {"lease_token", "summary", "artifact"},
}

// optionalVerbArguments covers the verbs an endpoint may or may not be granted.
// `release` itself stays optional: its absence is a grant, not a version
// difference (SPEC-0019 REQ-14). When it IS advertised, these arguments must be
// in its schema.
var optionalVerbArguments = map[string][]string{
	VerbRelease: {"lease_token", "summary"},
}

// requiredVerbs is the drain surface a lease source must advertise for the
// session to be usable at all.
var requiredVerbs = []string{VerbClaimNext, VerbHeartbeat, VerbComplete, VerbFail}

// Sentinel errors a caller branches on. The Switchboard codes are distinct from
// the transport failures: a `conflict` means the attempt lost its lease and must
// follow the conflict path at once, while a transport error is retried under
// REQ-7/REQ-10 (SPEC-0019 REQ-24).
var (
	ErrConflict  = errors.New("switchboard reports a lease conflict")
	ErrNotFound  = errors.New("switchboard has no such todo")
	ErrForbidden = errors.New("switchboard refused: outside this endpoint's grant")
	ErrInvalid   = errors.New("switchboard rejected the arguments")

	// ErrLeaseSourceUnsupported: the server's tool schemas lack the
	// attempt-history arguments this client will not work without. Every
	// ClaimNext is refused while the source is in this state; nothing is
	// silently downgraded.
	ErrLeaseSourceUnsupported = errors.New("the lease source predates Switchboard attempt history")
	// ErrReleaseNotAdvertised: this endpoint's grant does not include `release`.
	ErrReleaseNotAdvertised = errors.New("the lease source does not advertise release")

	// ErrSessionExpired: the server no longer recognises this Mcp-Session-Id, so
	// the session must be re-initialized (REQ-17).
	ErrSessionExpired = errors.New("the MCP session expired")
	// ErrUnauthorized: 401 or 403, meaning the credential is wrong or revoked.
	ErrUnauthorized = errors.New("the lease source rejected the session credentials")
	// ErrProtocol: the server spoke something this client cannot follow.
	ErrProtocol = errors.New("the lease source violated the MCP Streamable HTTP protocol")
	// ErrTransport: the exchange failed below the protocol (connection reset,
	// timeout, a 5xx). It is retryable; a Switchboard code is not.
	ErrTransport = errors.New("the lease source could not be reached")
)

// switchboardCodes maps the stable machine codes Switchboard renders in a tool
// error (SPEC-0006 REQ "Structured Output and Stable Error Shape") onto the
// sentinels above.
var switchboardCodes = map[string]error{
	"conflict":  ErrConflict,
	"not_found": ErrNotFound,
	"forbidden": ErrForbidden,
	"invalid":   ErrInvalid,
}

const (
	// callTimeout bounds one POST. Unlike the channel listener's stream, every
	// lease call is a request/response exchange, so an unbounded read would
	// just park the attempt.
	callTimeout = 60 * time.Second
	// handshakeTimeout bounds initialize plus notifications/initialized.
	handshakeTimeout = 30 * time.Second
	// maxResponseBytes caps a response body this client reads. A drain verb
	// answers with one todo row and its attempt history; 4 MiB is far above any
	// honest one.
	maxResponseBytes = 4 << 20
	// maxListPages bounds tools/list cursor following, so a server with a
	// self-referential cursor cannot keep the capability check looping.
	maxListPages = 10
	// maxErrorTextBytes caps the server-supplied text that reaches an error.
	maxErrorTextBytes = 200
	// redaction is what a secret becomes in anything this client says.
	redaction = "[redacted]"
)

// jsonrpcVersion is the only version this client speaks.
const jsonrpcVersion = "2.0"

// message is one JSON-RPC frame. ID present plus Method is a request; Method
// alone is a notification; ID plus Result or Error is a response.
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  any             `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// rpcError is a JSON-RPC error object.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Options configures a lease source's Client.
type Options struct {
	// Name is the lease source's name from `[queue.*]`; it prefixes every error
	// and log line so an operator can tell two sources apart.
	Name string
	// URL is the source's Streamable HTTP MCP endpoint.
	URL string
	// Headers are the source's resolved headers (its endpoint credential).
	// Values never reach a log line or an error string (REQ-18).
	Headers map[string]string
	// HTTP is the transport. Defaults to the same validated transport the
	// channel listener uses: same-origin redirects only, bounded connect, no
	// whole-request timeout that would cut a slow tool call.
	HTTP *http.Client
	// Log is where session events are reported. Defaults to log.Default().
	Log *log.Logger
	// Timeout bounds one request. Defaults to callTimeout.
	Timeout time.Duration
}

// Client is one POST-only MCP session against one lease source. It is safe for
// concurrent use: the session state is guarded, and one call never holds a lock
// across an HTTP exchange.
type Client struct {
	name    string
	url     string
	headers map[string]string
	http    *http.Client
	log     *log.Logger
	timeout time.Duration

	mu        sync.Mutex
	sessionID string
	protocol  string
	requestID int
	// secrets are the strings that must never appear in an error or a log line:
	// the source's header values and the lease token currently held.
	secrets []string

	stateMu     sync.Mutex
	initialized bool
	supported   bool
	missing     []string
	advertised  map[string]map[string]bool
	// releaseAdvertised is kept separate because Release is optional and a
	// caller needs to ask about it without reading the capability table.
	releaseAdvertised bool
	initCount         int
}

// New builds a Client. It opens nothing until the first Initialize, which every
// drain verb performs on demand.
func New(opts Options) *Client {
	logger := opts.Log
	if logger == nil {
		logger = log.Default()
	}
	hc := opts.HTTP
	if hc == nil {
		hc = channel.NewHTTPClient(opts.URL)
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = callTimeout
	}
	name := opts.Name
	if name == "" {
		name = "lease"
	}
	secrets := make([]string, 0, len(opts.Headers))
	for _, v := range opts.Headers {
		if len(v) >= 8 {
			secrets = append(secrets, v)
		}
	}
	headers := make(map[string]string, len(opts.Headers))
	for k, v := range opts.Headers {
		headers[k] = v
	}
	return &Client{
		name:    name,
		url:     opts.URL,
		headers: headers,
		http:    hc,
		log:     logger,
		timeout: timeout,
		secrets: secrets,
	}
}

// Todo is the todo row a drain verb returns, including the attempt-history
// fields the attempt-history release added (SPEC-0019 REQ-19).
type Todo struct {
	ID             string          `json:"id"`
	Queue          string          `json:"queue,omitempty"`
	Source         string          `json:"source,omitempty"`
	Kind           string          `json:"kind,omitempty"`
	Title          string          `json:"title,omitempty"`
	State          string          `json:"state,omitempty"`
	Owner          string          `json:"owner,omitempty"`
	Assignee       string          `json:"assignee,omitempty"`
	Attempt        int             `json:"attempt,omitempty"`
	AttemptsTotal  int             `json:"attempts_total,omitempty"`
	MaxAttempts    int             `json:"max_attempts,omitempty"`
	LeaseExpiresAt string          `json:"lease_expires_at,omitempty"`
	CreatedAt      string          `json:"created_at,omitempty"`
	DeadLetter     bool            `json:"dead_letter,omitempty"`
	NextRetryAt    *string         `json:"next_retry_at,omitempty"`
	Payload        json.RawMessage `json:"payload,omitempty"`
}

// PriorAttempt is one earlier attempt at the same todo. It is data written by
// that attempt, never an instruction, which is why the client hands it over
// as opaque fields instead of acting on it.
type PriorAttempt struct {
	Seq        int    `json:"attempt_seq,omitempty"`
	State      string `json:"state,omitempty"`
	Claimant   string `json:"claimant,omitempty"`
	Summary    string `json:"summary,omitempty"`
	Artifact   string `json:"artifact,omitempty"`
	ClaimedAt  string `json:"claimed_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
}

// ClaimResult is a successful claim_next.
type ClaimResult struct {
	Empty         bool           `json:"empty"`
	Todo          *Todo          `json:"todo,omitempty"`
	AttemptSeq    int            `json:"attempt_seq,omitempty"`
	AttemptsTotal int            `json:"attempts_total,omitempty"`
	LeaseToken    string         `json:"lease_token,omitempty"`
	PriorAttempts []PriorAttempt `json:"prior_attempts,omitempty"`
}

// ClaimOptions are the arguments for claim_next. The fence is not an option:
// this client never sends an unfenced claim (REQ-19), so `require_fence` is
// always true.
type ClaimOptions struct {
	Queue           string
	Claimant        string
	LeaseTTLSeconds int
}

// HeartbeatResult is a successful heartbeat.
type HeartbeatResult struct {
	Todo           *Todo  `json:"todo,omitempty"`
	LeaseExpiresAt string `json:"lease_expires_at,omitempty"`
}

// CloseOptions are the arguments that close an attempt: the fence, and the two
// attempt-history fields that carry what the attempt produced. `Result`
// stays a separate argument: summary and artifact never go inside it.
type CloseOptions struct {
	ID         string
	LeaseToken string
	Summary    string
	Artifact   string
	Result     any
}

// ReleaseOptions are the arguments for release. Release carries no artifact:
// the enumerated set for that verb is the fence and a note (REQ-19).
type ReleaseOptions struct {
	ID         string
	LeaseToken string
	Summary    string
}

// Initialize opens (or reopens) the session: initialize,
// notifications/initialized, tools/list, and the capability check. It returns
// nil for a session that initialized cleanly even when the source is marked
// unsupported (callers branch on Supported, and ClaimNext refuses on its
// own), but it wraps the failure in ErrLeaseSourceUnsupported so a caller that
// prefers to branch on the error can do that too.
//
// One ERROR is logged per initialize that comes back unsupported, naming every
// missing verb argument (REQ-19).
func (c *Client) Initialize(ctx context.Context) error {
	c.stateMu.Lock()
	c.initialized = false
	c.stateMu.Unlock()

	if err := c.handshake(ctx); err != nil {
		return err
	}
	tools, err := c.listTools(ctx)
	if err != nil {
		c.invalidateSession()
		return err
	}
	missing, advertisedRelease := checkCapabilities(tools)

	c.stateMu.Lock()
	c.advertised = tools
	c.missing = missing
	c.supported = len(missing) == 0
	c.releaseAdvertised = advertisedRelease
	c.initialized = true
	c.initCount++
	initCount := c.initCount
	c.stateMu.Unlock()

	if len(missing) > 0 {
		c.log.Error("lease source does not support attempt history",
			"source", c.name, "missing", strings.Join(missing, " "), "initialize", initCount)
		return fmt.Errorf("relay %s: initialize: %w: missing %s",
			c.name, ErrLeaseSourceUnsupported, strings.Join(missing, " "))
	}
	c.log.Info("lease session initialized", "source", c.name, "protocol", c.protocolVersion(), "initialize", initCount)
	return nil
}

// Supported reports whether the last initialize found every argument the drain
// verbs need. It is false before the first initialize.
func (c *Client) Supported() bool {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.supported
}

// MissingArguments lists the `verb.argument` pairs the server's schemas do not
// declare, sorted so a doctor row and a test are stable.
func (c *Client) MissingArguments() []string {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	out := make([]string, len(c.missing))
	copy(out, c.missing)
	return out
}

// Advertises reports whether the server registered a verb.
func (c *Client) Advertises(verb string) bool {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	_, ok := c.advertised[verb]
	return ok
}

// ReleaseAdvertised reports whether the endpoint's grant includes `release`.
func (c *Client) ReleaseAdvertised() bool {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.releaseAdvertised
}

// SessionID is the Mcp-Session-Id the server issued, or "" before initialize.
func (c *Client) SessionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessionID
}

func (c *Client) protocolVersion() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.protocol
}

// ClaimNext takes the next todo from the source's granted queues. The claim is
// always fenced, and the returned LeaseToken is what every later verb for that
// attempt must carry.
func (c *Client) ClaimNext(ctx context.Context, opts ClaimOptions) (ClaimResult, error) {
	args := map[string]any{"require_fence": true}
	setString(args, "queue", opts.Queue)
	setString(args, "claimant", opts.Claimant)
	setInt(args, "lease_ttl_seconds", opts.LeaseTTLSeconds)

	raw, err := c.call(ctx, VerbClaimNext, args)
	if err != nil {
		return ClaimResult{}, err
	}
	var res ClaimResult
	if err := decode(raw, &res); err != nil {
		return ClaimResult{}, err
	}
	if res.LeaseToken != "" {
		c.rememberSecret(res.LeaseToken)
	}
	return res, nil
}

// Heartbeat extends the attempt's lease. Every heartbeat carries the fence
// token its schema declares (REQ-19); an empty token is a caller error, not
// something to send unfenced.
func (c *Client) Heartbeat(ctx context.Context, id, leaseToken string, leaseTTLSeconds int) (HeartbeatResult, error) {
	if leaseToken == "" {
		return HeartbeatResult{}, c.err(VerbHeartbeat, "a heartbeat without a lease token is never sent", ErrInvalid)
	}
	args := map[string]any{"id": id, "lease_token": leaseToken}
	setInt(args, "lease_ttl_seconds", leaseTTLSeconds)

	raw, err := c.call(ctx, VerbHeartbeat, args)
	if err != nil {
		return HeartbeatResult{}, err
	}
	var res HeartbeatResult
	if err := decode(raw, &res); err != nil {
		return HeartbeatResult{}, err
	}
	if res.LeaseExpiresAt == "" && res.Todo != nil {
		res.LeaseExpiresAt = res.Todo.LeaseExpiresAt
	}
	return res, nil
}

// Complete closes the attempt as done.
func (c *Client) Complete(ctx context.Context, opts CloseOptions) error {
	_, err := c.closeAttempt(ctx, VerbComplete, opts)
	return err
}

// Fail closes the attempt as failed; Switchboard decides retry versus
// dead-letter from its own attempt budget (REQ-16), which this client never
// infers.
func (c *Client) Fail(ctx context.Context, opts CloseOptions) error {
	_, err := c.closeAttempt(ctx, VerbFail, opts)
	return err
}

// Release hands the todo back to the queue without a verdict. It fails with
// ErrReleaseNotAdvertised, and it sends nothing, when the endpoint grant has
// no release verb.
func (c *Client) Release(ctx context.Context, opts ReleaseOptions) error {
	if !c.Advertises(VerbRelease) {
		if err := c.ensureSession(ctx); err != nil {
			return err
		}
		if !c.Advertises(VerbRelease) {
			return c.err(VerbRelease, "the endpoint grant does not include release", ErrReleaseNotAdvertised)
		}
	}
	if opts.LeaseToken == "" {
		return c.err(VerbRelease, "a release without a lease token is never sent", ErrInvalid)
	}
	args := map[string]any{"id": opts.ID, "lease_token": opts.LeaseToken}
	setString(args, "summary", opts.Summary)
	_, err := c.call(ctx, VerbRelease, args)
	return err
}

// closeAttempt sends complete or fail with the fence and the
// attempt-history fields in their own arguments.
func (c *Client) closeAttempt(ctx context.Context, verb string, opts CloseOptions) (json.RawMessage, error) {
	if opts.LeaseToken == "" {
		return nil, c.err(verb, "closing an attempt without a lease token is never sent", ErrInvalid)
	}
	args := map[string]any{"id": opts.ID, "lease_token": opts.LeaseToken}
	setString(args, "summary", opts.Summary)
	setString(args, "artifact", opts.Artifact)
	if opts.Result != nil {
		args["result"] = opts.Result
	}
	return c.call(ctx, verb, args)
}

// call performs one tools/call, initializing the session if needed and refusing
// an unsupported source before anything goes on the wire.
func (c *Client) call(ctx context.Context, verb string, args map[string]any) (json.RawMessage, error) {
	if err := c.ensureSession(ctx); err != nil {
		return nil, err
	}
	if !c.Supported() {
		return nil, c.err(verb, "refused: the source is marked unsupported",
			ErrLeaseSourceUnsupported, "missing", strings.Join(c.MissingArguments(), " "))
	}
	id := c.nextRequestID()
	payload, err := json.Marshal(message{
		JSONRPC: jsonrpcVersion,
		ID:      id,
		Method:  "tools/call",
		Params:  map[string]any{"name": verb, "arguments": args},
	})
	if err != nil {
		return nil, c.err(verb, "encode request", err)
	}
	body, err := c.postWithReinit(ctx, payload, string(id))
	if err != nil {
		return nil, c.err(verb, "", err)
	}
	return c.toolPayload(verb, body)
}

// ensureSession initializes once, lazily; later calls reuse the session until
// the server expires it.
func (c *Client) ensureSession(ctx context.Context) error {
	c.stateMu.Lock()
	ready := c.initialized
	c.stateMu.Unlock()
	if ready {
		return nil
	}
	return c.Initialize(ctx)
}

// handshake performs initialize plus notifications/initialized. The capability
// check is the caller's, after tools/list.
func (c *Client) handshake(ctx context.Context) error {
	params := map[string]any{
		"protocolVersion": channel.ProtocolVersion,
		// No `experimental.claude/channel`: Harness is not a doorbell target
		// from this session (REQ-17).
		"capabilities": map[string]any{"tools": map[string]any{}},
		"clientInfo":   map[string]any{"name": channel.ClientName, "version": buildinfo.Version},
	}
	id := c.nextRequestID()
	body, err := json.Marshal(message{
		JSONRPC: jsonrpcVersion,
		ID:      id,
		Method:  "initialize",
		Params:  params,
	})
	if err != nil {
		return c.err("initialize", "encode request", err)
	}

	ctxh, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	resp, err := c.postRequest(ctxh, body)
	if err != nil {
		return c.err("initialize", "", err)
	}
	defer discardBody(resp)

	if resp.StatusCode == http.StatusNotFound && c.SessionID() != "" {
		return c.err("initialize", "", ErrSessionExpired)
	}
	if err := c.statusError(resp, "initialize"); err != nil {
		return err
	}
	if id := resp.Header.Get("Mcp-Session-Id"); id != "" {
		c.mu.Lock()
		c.sessionID = id
		c.mu.Unlock()
	}
	raw, stream, err := readBody(resp)
	if err != nil {
		return c.err("initialize", "read response", err)
	}
	raw, err = responseFor(raw, string(id), stream)
	if err != nil {
		return c.err("initialize", "", err)
	}
	var msg message
	if err := json.Unmarshal(raw, &msg); err != nil {
		return c.err("initialize", "decode response", ErrProtocol)
	}
	if msg.Error != nil {
		return c.err("initialize", c.redact(msg.Error.Message), ErrProtocol)
	}
	var res struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(msg.Result, &res); err != nil {
		return c.err("initialize", "result is not an initialize result", ErrProtocol)
	}
	if res.ProtocolVersion != "" {
		c.mu.Lock()
		c.protocol = res.ProtocolVersion
		c.mu.Unlock()
	}

	notify, err := json.Marshal(message{JSONRPC: jsonrpcVersion, Method: "notifications/initialized"})
	if err != nil {
		return c.err("notifications/initialized", "encode notification", err)
	}
	nctx, cancel2 := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel2()
	nresp, err := c.postRequest(nctx, notify)
	if err != nil {
		return c.err("notifications/initialized", "", err)
	}
	defer discardBody(nresp)
	if nresp.StatusCode != http.StatusAccepted && nresp.StatusCode != http.StatusNoContent && nresp.StatusCode != http.StatusOK {
		if err := c.statusError(nresp, "notifications/initialized"); err != nil {
			return err
		}
	}
	return nil
}

// listTools reads the advertised tools and each verb's input schema, following
// cursors up to a bounded number of pages.
func (c *Client) listTools(ctx context.Context) (map[string]map[string]bool, error) {
	tools := map[string]map[string]bool{}
	cursor := ""
	for page := 0; page < maxListPages; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		id := c.nextRequestID()
		body, err := json.Marshal(message{
			JSONRPC: jsonrpcVersion,
			ID:      id,
			Method:  "tools/list",
			Params:  params,
		})
		if err != nil {
			return nil, c.err("tools/list", "encode request", err)
		}
		raw, err := c.postWithReinit(ctx, body, string(id))
		if err != nil {
			return nil, c.err("tools/list", "", err)
		}
		var msg message
		if err := json.Unmarshal(raw, &msg); err != nil {
			return nil, c.err("tools/list", "decode response", ErrProtocol)
		}
		if msg.Error != nil {
			return nil, c.err("tools/list", c.redact(msg.Error.Message), ErrProtocol)
		}
		var res struct {
			Tools []struct {
				Name        string          `json:"name"`
				InputSchema json.RawMessage `json:"inputSchema"`
			} `json:"tools"`
			NextCursor string `json:"nextCursor,omitempty"`
		}
		if err := json.Unmarshal(msg.Result, &res); err != nil {
			return nil, c.err("tools/list", "result is not a tools/list result", ErrProtocol)
		}
		for _, t := range res.Tools {
			tools[t.Name] = propertyNames(t.InputSchema)
		}
		if res.NextCursor == "" || res.NextCursor == cursor {
			return tools, nil
		}
		cursor = res.NextCursor
	}
	return tools, nil
}

// checkCapabilities compares the advertised schemas against the contract and
// returns the sorted list of missing `verb.argument` pairs, plus whether
// `release` was advertised at all.
func checkCapabilities(tools map[string]map[string]bool) (missing []string, releaseAdvertised bool) {
	for _, verb := range requiredVerbs {
		missing = appendMissing(missing, verb, tools[verb], requiredArguments[verb])
	}
	verbArgs, ok := tools[VerbRelease]
	if ok {
		releaseAdvertised = true
		missing = appendMissing(missing, VerbRelease, verbArgs, optionalVerbArguments[VerbRelease])
	}
	sort.Strings(missing)
	return missing, releaseAdvertised
}

func appendMissing(missing []string, verb string, props map[string]bool, required []string) []string {
	for _, arg := range required {
		if !props[arg] {
			missing = append(missing, verb+"."+arg)
		}
	}
	return missing
}

// propertyNames reads an inputSchema's declared properties. A schema with no
// properties object declares nothing, which is the same as declaring no
// arguments.
func propertyNames(schema json.RawMessage) map[string]bool {
	names := map[string]bool{}
	var s struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(schema, &s); err != nil {
		return names
	}
	for name := range s.Properties {
		names[name] = true
	}
	return names
}

// toolPayload extracts the JSON payload of a tools/call result: the structured
// content when the server sends it, otherwise the first text item that decodes
// as JSON. A tool error becomes one of the mapped sentinels.
func (c *Client) toolPayload(verb string, raw json.RawMessage) (json.RawMessage, error) {
	var msg message
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil, c.err(verb, "decode response", ErrProtocol)
	}
	if msg.Error != nil {
		return nil, c.mapError(verb, msg.Error.Message, msg.Error.Data)
	}
	var res struct {
		IsError           bool            `json:"isError"`
		StructuredContent json.RawMessage `json:"structuredContent"`
		Content           []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(msg.Result, &res); err != nil {
		return nil, c.err(verb, "result is not a tools/call result", ErrProtocol)
	}
	if res.IsError {
		var data json.RawMessage
		if len(res.StructuredContent) > 0 {
			data = res.StructuredContent
		}
		text := ""
		for _, item := range res.Content {
			if item.Type == "text" || item.Type == "" {
				text = item.Text
				break
			}
		}
		return nil, c.mapError(verb, text, data)
	}
	if len(res.StructuredContent) > 0 {
		return res.StructuredContent, nil
	}
	for _, item := range res.Content {
		if item.Type != "text" && item.Type != "" {
			continue
		}
		var probe json.RawMessage
		if err := json.Unmarshal([]byte(item.Text), &probe); err == nil {
			return probe, nil
		}
	}
	return nil, c.err(verb, "the result carried no JSON payload", ErrProtocol)
}

// mapError turns a Switchboard tool failure into a sentinel. The server renders
// errors as "code: message", and may repeat the code in structured content;
// neither the header values nor the lease token survive the trip into the
// returned error (REQ-18).
func (c *Client) mapError(verb, text string, data json.RawMessage) error {
	code := ""
	msg := c.redact(boundText(text))
	if i := strings.Index(msg, ":"); i > 0 && i <= len("not_found") {
		if candidate := strings.TrimSpace(msg[:i]); switchboardCodes[candidate] != nil {
			code = candidate
			msg = strings.TrimSpace(msg[i+1:])
		}
	}
	if code == "" && len(data) > 0 {
		var structured struct {
			Code string `json:"code"`
		}
		if json.Unmarshal(data, &structured) == nil && switchboardCodes[structured.Code] != nil {
			code = structured.Code
		}
	}
	if msg == "" {
		msg = "the tool reported a failure with no message"
	}
	if sentinel, ok := switchboardCodes[code]; ok {
		return c.err(verb, msg, sentinel)
	}
	return c.err(verb, msg, fmt.Errorf("switchboard %s", codeOrUnknown(code)))
}

func codeOrUnknown(code string) string {
	if code == "" {
		return "error"
	}
	return code
}

// postWithReinit POSTs a request, re-initializing and retrying once when the
// server no longer recognises the session (REQ-17).
func (c *Client) postWithReinit(ctx context.Context, body []byte, id string) (json.RawMessage, error) {
	raw, err := c.postFor(ctx, body, id)
	if errors.Is(err, ErrSessionExpired) {
		c.invalidateSession()
		if err := c.Initialize(ctx); err != nil {
			return nil, err
		}
		return c.postFor(ctx, body, id)
	}
	return raw, err
}

// postFor sends one JSON-RPC request and returns the raw response body, which
// is either a JSON document or an event stream carrying the reply.
func (c *Client) postFor(ctx context.Context, body []byte, id string) (json.RawMessage, error) {
	resp, err := c.postRequest(ctx, body)
	if err != nil {
		return nil, err
	}
	defer discardBody(resp)
	if err := c.statusError(resp, "request"); err != nil {
		return nil, err
	}
	raw, stream, err := readBody(resp)
	if err != nil {
		return nil, c.err("request", "read response", err)
	}
	return responseFor(raw, id, stream)
}

// postRequest builds and sends the POST. This client sends no other method:
// there is no GET anywhere in it, which is what keeps the source from treating
// Harness as a doorbell target (REQ-17).
func (c *Client) postRequest(ctx context.Context, body []byte) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, c.err("request", "build request", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if id := c.SessionID(); id != "" {
		req.Header.Set("Mcp-Session-Id", id)
	}
	if p := c.protocolVersion(); p != "" {
		req.Header.Set("MCP-Protocol-Version", p)
	}
	// Operator headers are applied last, so a source's own Authorization
	// cannot be clobbered by anything this client sets, and a hand-set
	// Mcp-Session-Id or MCP-Protocol-Version deliberately wins.
	for name, value := range c.headers {
		req.Header.Set(name, value)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		cancel()
		var protoErr *http.ProtocolError
		if errors.As(err, &protoErr) {
			return nil, c.err("request", "transport", ErrProtocol)
		}
		return nil, fmt.Errorf("relay %s: request: %w: %w", c.name, ErrTransport, err)
	}
	resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

// statusError maps a response status onto a sentinel, or nil.
func (c *Client) statusError(resp *http.Response, what string) error {
	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return c.err(what, resp.Status, ErrUnauthorized)
	case resp.StatusCode == http.StatusNotFound && c.SessionID() != "":
		return c.err(what, "", ErrSessionExpired)
	case resp.StatusCode >= 400:
		return c.err(what, "server answered "+resp.Status, ErrTransport)
	}
	return nil
}

func (c *Client) invalidateSession() {
	c.mu.Lock()
	c.sessionID = ""
	c.mu.Unlock()
	c.stateMu.Lock()
	c.initialized = false
	c.stateMu.Unlock()
}

func (c *Client) rememberSecret(s string) {
	if len(s) < 8 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, known := range c.secrets {
		if known == s {
			return
		}
	}
	c.secrets = append(c.secrets, s)
}

// redact removes every credential and token this client has handled from a
// string about to become an error message or a log value (REQ-18).
func (c *Client) redact(s string) string {
	c.mu.Lock()
	secrets := c.secrets
	c.mu.Unlock()
	for _, secret := range secrets {
		if strings.Contains(s, secret) {
			s = strings.ReplaceAll(s, secret, redaction)
		}
	}
	return s
}

// err wraps a failure with the source name and the operation, which is what
// REQ-24 asks a reader to be able to grep for. The cause is wrapped, not
// repeated, so the chain stays readable and errors.Is still works.
func (c *Client) err(op, detail string, cause error, keyvals ...any) error {
	prefix := fmt.Sprintf("relay %s: %s", c.name, op)
	if detail != "" {
		prefix += ": " + detail
	}
	for i := 0; i+1 < len(keyvals); i += 2 {
		prefix += fmt.Sprintf(" %v=%v", keyvals[i], keyvals[i+1])
	}
	return fmt.Errorf("%s: %w", prefix, cause)
}

func (c *Client) nextRequestID() json.RawMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requestID++
	return json.RawMessage(fmt.Sprintf("%d", c.requestID))
}

// responseFor finds the JSON-RPC reply to id in a body that is either a single
// JSON document or a Streamable HTTP event stream.
func responseFor(raw json.RawMessage, id string, stream bool) (json.RawMessage, error) {
	if !stream {
		return raw, nil
	}
	var found json.RawMessage
	var data []string
	flush := func() {
		if len(data) == 0 {
			return
		}
		payload := []byte(strings.Join(data, "\n"))
		data = data[:0]
		var msg struct {
			ID json.RawMessage `json:"id"`
		}
		if json.Unmarshal(payload, &msg) == nil && string(msg.ID) == id {
			found = json.RawMessage(payload)
		}
	}
	for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	flush()
	if found == nil {
		return nil, fmt.Errorf("the response carried no reply to request %s: %w", id, ErrProtocol)
	}
	return found, nil
}

func readBody(resp *http.Response) ([]byte, bool, error) {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, false, err
	}
	stream := false
	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt == "text/event-stream" {
		stream = true
	}
	return raw, stream, nil
}

func discardBody(resp *http.Response) {
	if resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
}

// cancelOnClose releases the request context when the body is closed, so a
// kept-alive connection is not pinned by an unfinished context.
type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelOnClose) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

func decode(raw json.RawMessage, out any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}
	if err := json.Unmarshal(trimmed, out); err != nil {
		return fmt.Errorf("relay: decode response: %w: %w", ErrProtocol, err)
	}
	return nil
}

func setString(args map[string]any, key, value string) {
	if value != "" {
		args[key] = value
	}
}

func setInt(args map[string]any, key string, value int) {
	if value != 0 {
		args[key] = value
	}
}

// boundText caps a server-supplied message so a hostile or broken server cannot
// put a whole response body into an error string. It cuts on a rune boundary.
func boundText(s string) string {
	if len(s) <= maxErrorTextBytes {
		return s
	}
	cut := s[:maxErrorTextBytes]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + "…"
}
