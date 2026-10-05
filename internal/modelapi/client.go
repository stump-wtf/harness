// Package modelapi is the daemon's OpenAI-compatible chat-completions
// client (ADR-0036) and, on top of it, the gate-call layer every input
// screen goes through (ADR-0042, SPEC-0031).
//
// The client is one POST to <base_url>/chat/completions with redirects
// never followed, a bounded response body, and a single json_schema retry.
// Nothing in the daemon calls it yet; #838's guard kinds and #840's first
// waiter build on it.
//
// Governing: ADR-0036; ADR-0042; SPEC-0031 REQ-6, REQ-8.
package modelapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/stump-wtf/harness/internal/core"
)

// maxBodyBytes is the response body cap: 1 MiB plus one byte, so a body of
// exactly 1 MiB + 1 bytes is detected as over-cap rather than truncated
// into something a JSON decoder might still accept.
const maxBodyBytes = 1<<20 + 1

// ErrBodyTooLarge reports a response body at or beyond the cap. The caller
// classifies it as REQ-6's parse.
var ErrBodyTooLarge = errors.New("response body exceeds the 1 MiB cap")

// ErrDecode reports a 200 whose body is not the expected JSON. The caller
// classifies it as REQ-6's parse.
var ErrDecode = errors.New("response is not the expected JSON")

// StatusError reports a completed HTTP round trip whose status is not 200.
// The caller classifies it as REQ-6's http_status.
type StatusError struct {
	Code int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("model api answered %d", e.Code)
}

// Message is one chat message.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// JSONSchemaSpec is the `json_schema` half of a response_format.
type JSONSchemaSpec struct {
	Name   string         `json:"name"`
	Schema map[string]any `json:"schema"`
	Strict bool           `json:"strict,omitempty"`
}

// ResponseFormat is an optional response_format of type json_schema.
type ResponseFormat struct {
	Type       string          `json:"type"`
	JSONSchema *JSONSchemaSpec `json:"json_schema,omitempty"`
}

// Request is one chat-completions request. Optional keys are pointers so a
// nil is absent from the wire, while stream is always sent (false when the
// caller does not stream — this client never streams).
type Request struct {
	Model          string          `json:"model"`
	Messages       []Message       `json:"messages"`
	MaxTokens      int             `json:"max_tokens"`
	Temperature    *float64        `json:"temperature,omitempty"`
	N              int             `json:"n"`
	Stream         bool            `json:"stream"`
	Logprobs       *bool           `json:"logprobs,omitempty"`
	TopLogprobs    *int            `json:"top_logprobs,omitempty"`
	ResponseFormat *ResponseFormat `json:"response_format,omitempty"`
}

// TopLogprob is one token and its log probability.
type TopLogprob struct {
	Token   string  `json:"token"`
	Logprob float64 `json:"logprob"`
}

// ContentLogprobs is the logprob bundle of one emitted token.
type ContentLogprobs struct {
	Token       string       `json:"token"`
	TopLogprobs []TopLogprob `json:"top_logprobs"`
}

// ChoiceLogprobs is choices[].logprobs.
type ChoiceLogprobs struct {
	Content []ContentLogprobs `json:"content"`
}

// Choice is one choice of a completion.
type Choice struct {
	Message  Message         `json:"message"`
	Logprobs *ChoiceLogprobs `json:"logprobs"`
}

// Usage is the response's usage object; an absent object decodes to zero.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// Response is the part of a chat-completions response the daemon reads.
type Response struct {
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
}

// Client posts chat completions to one base URL. It is safe for concurrent
// use. An empty APIKey sends no Authorization header at all.
type Client struct {
	baseURL string
	apiKey  core.Secret
	hc      *http.Client
}

// New returns a client for baseURL (an http/https API root, typically a
// /v1 root; the client appends /chat/completions) authenticated with
// apiKey, which may be empty.
func New(baseURL string, apiKey core.Secret) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		hc: &http.Client{
			// A 3xx is a status to report, never a location to follow:
			// following one would send the credential to a host the
			// configuration never named.
			// Governing: SPEC-0031 REQ-6.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Do posts req and returns the parsed response. A json_schema request the
// server refuses (any non-200) is retried exactly once without
// response_format; a request that sent none is never retried. Context
// cancellation and expiry propagate to the caller untouched, as do
// connection-level failures; over-cap bodies report ErrBodyTooLarge and a
// non-200 final status reports *StatusError.
func (c *Client) Do(ctx context.Context, req *Request) (*Response, error) {
	resp, err := c.post(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK && req.ResponseFormat != nil {
		// The one retry: a gateway that refuses json_schema gets the same
		// request without it. Gate calls never send a response_format
		// (REQ-3, REQ-4), so they never take this path and REQ-6's
		// no-retry rule holds for them.
		resp2, err2 := c.post(ctx, withoutResponseFormat(req))
		_ = resp.Body.Close()
		if err2 != nil {
			return nil, err2
		}
		resp = resp2
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		// Any status other than 200, a redirect included (it was never
		// followed), is a status — never retried.
		return nil, &StatusError{Code: resp.StatusCode}
	}
	return decodeBody(resp.Body)
}

// withoutResponseFormat returns a shallow copy of req with
// response_format dropped.
func withoutResponseFormat(req *Request) *Request {
	cp := *req
	cp.ResponseFormat = nil
	return &cp
}

// post performs one round trip and returns the raw response with its body
// unread. A non-200 is NOT an error here: the caller decides whether it is
// retryable before the status is turned into a *StatusError.
func (c *Client) post(ctx context.Context, req *Request) (*http.Response, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if !c.apiKey.Empty() {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey.Reveal())
	}
	return c.hc.Do(httpReq)
}

// decodeBody reads body through the cap and decodes the JSON.
func decodeBody(r io.Reader) (*Response, error) {
	buf, err := io.ReadAll(io.LimitReader(r, maxBodyBytes))
	if err != nil {
		return nil, err
	}
	if len(buf) == maxBodyBytes {
		return nil, ErrBodyTooLarge
	}
	var resp Response
	if err := json.Unmarshal(buf, &resp); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDecode, err)
	}
	return &resp, nil
}
