package switchboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// maxResponseBytes caps every response body at 1 MiB (SPEC-0018 "Security
// Requirements"): a hostile or broken endpoint cannot balloon memory.
const maxResponseBytes = 1 << 20

// NewHTTPClient builds the client every call in this package uses: no
// cross-origin redirects (a token must never hop off-origin), a sane
// timeout, nothing cached.
func NewHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > 0 && via[0].URL.Host != req.URL.Host {
				return http.ErrUseLastResponse
			}
			if len(via) > 4 {
				return errors.New("switchboard: too many redirects")
			}
			return nil
		},
	}
}

// statusError is a non-2xx response, carried so callers can branch on the
// status while error strings stay token-free (REQ-29). Bodies are
// server-authored validation text, summarized and bounded.
type statusError struct {
	status int
	header http.Header
	body   string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("switchboard: HTTP %d: %s", e.status, e.bodySummary())
}

func (e *statusError) bodySummary() string {
	const max = 300
	switch {
	case e.body == "":
		return "(no body)"
	case len(e.body) > max:
		return e.body[:max] + "…"
	default:
		return e.body
	}
}

// RetryAfter reports how long a 429 asks the caller to wait.
func (e *statusError) RetryAfter() (time.Duration, bool) {
	if e.status != http.StatusTooManyRequests {
		return 0, false
	}
	if v := e.header.Get("Retry-After"); v != "" {
		var secs int
		if _, err := fmt.Sscanf(v, "%d", &secs); err == nil && secs >= 0 {
			return time.Duration(secs) * time.Second, true
		}
		if t, err := http.ParseTime(v); err == nil {
			return time.Until(t), true
		}
	}
	return time.Second, true // an honest default rather than a hot loop
}

// do executes req, caps the body at 1 MiB, and classifies non-2xx as
// *statusError.
func do(ctx context.Context, client *http.Client, req *http.Request) ([]byte, *statusError, error) {
	body, _, se, err := doWithHeader(ctx, client, req)
	return body, se, err
}

// doWithHeader is do plus the response header, for the one caller (the MCP
// read) whose body format depends on the Content-Type.
func doWithHeader(ctx context.Context, client *http.Client, req *http.Request) ([]byte, http.Header, *statusError, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("switchboard: read body: %w", err)
	}
	if len(body) > maxResponseBytes {
		return nil, nil, nil, errors.New("switchboard: response exceeds the 1 MiB cap")
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, nil, &statusError{status: resp.StatusCode, header: resp.Header, body: string(body)}, nil
	}
	return body, resp.Header, nil, nil
}

// doWithRetry runs do with a single 429 Retry-After retry, so a burst
// against the operator API succeeds without every caller building a loop.
// The request must be replayable: callers build it from a byte slice.
func doWithRetry(ctx context.Context, client *http.Client, req *http.Request) ([]byte, error) {
	raw, se, err := do(ctx, client, req)
	if se == nil {
		return raw, err
	}
	if wait, ok := se.RetryAfter(); ok && wait > 0 && wait < 30*time.Second {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
		// Rebuild the body reader: NewRequest-shaped bodies are byte slices
		// in memory, so cloning with a fresh reader replays exactly.
		req2 := req.Clone(ctx)
		if req.GetBody != nil {
			if b, err := req.GetBody(); err == nil {
				req2.Body = b
			}
		}
		if raw2, se2, err2 := do(ctx, client, req2); se2 == nil {
			return raw2, err2
		} else {
			return nil, se2
		}
	}
	return nil, se
}

func fetchJSON(client *http.Client, req *http.Request, out any) error {
	body, err := doWithRetry(req.Context(), client, req)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("switchboard: decode response: %w", err)
	}
	return nil
}
