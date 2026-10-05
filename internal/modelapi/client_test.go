package modelapi

// Client Tests
//
// Governing tests: ADR-0036 and SPEC-0031 #837 — the wire shape is asserted
// key by key against what an httptest server actually received (absent keys
// included), a redirect is never followed, a body at the cap is detected,
// the one json_schema retry happens exactly once without response_format,
// and a call answered 500 is never retried.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stump-wtf/harness/internal/core"
)

// capturingServer records every request it receives.
type capturingServer struct {
	srv      *httptest.Server
	hits     atomic.Int64
	requests chan capturedRequest
}

type capturedRequest struct {
	Authorization string
	Body          map[string]any
}

func newCapturingServer(t *testing.T, respond func(w http.ResponseWriter, n int64) error) *capturingServer {
	t.Helper()
	cs := &capturingServer{requests: make(chan capturedRequest, 16)}
	cs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := cs.hits.Add(1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		cs.requests <- capturedRequest{Authorization: r.Header.Get("Authorization"), Body: body}
		if err := respond(w, n); err != nil {
			t.Errorf("respond: %v", err)
		}
	}))
	t.Cleanup(cs.srv.Close)
	return cs
}

func okBody() string {
	return `{"model":"m1","choices":[{"message":{"role":"assistant","content":"yes"},"logprobs":{"content":[{"token":"yes","top_logprobs":[{"token":"yes","logprob":-0.1}]}]}}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`
}

func TestClientWireShapeKeyByKey(t *testing.T) {
	cs := newCapturingServer(t, func(w http.ResponseWriter, _ int64) error {
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(okBody()))
		return err
	})
	temp := 0.0
	logprobs := true
	top := 20
	c := New(cs.srv.URL, "sk-test-1")
	resp, err := c.Do(context.Background(), &Request{
		Model:       "shield",
		Messages:    []Message{{Role: "user", Content: "hello"}},
		MaxTokens:   1,
		Temperature: &temp,
		N:           1,
		Stream:      false,
		Logprobs:    &logprobs,
		TopLogprobs: &top,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Model != "m1" || len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "yes" ||
		resp.Usage.PromptTokens != 3 || resp.Usage.CompletionTokens != 1 {
		t.Fatalf("response = %+v", resp)
	}
	if len(resp.Choices[0].Logprobs.Content) != 1 || len(resp.Choices[0].Logprobs.Content[0].TopLogprobs) != 1 {
		t.Fatalf("logprobs = %+v", resp.Choices[0].Logprobs)
	}

	req := <-cs.requests
	// Assert the decoded JSON the server received, key by key, including
	// the keys that must be absent.
	want := map[string]any{
		"model":        "shield",
		"max_tokens":   float64(1),
		"temperature":  float64(0),
		"n":            float64(1),
		"stream":       false,
		"logprobs":     true,
		"top_logprobs": float64(20),
	}
	for k, v := range want {
		got, ok := req.Body[k]
		if !ok {
			t.Fatalf("request lacks %q", k)
		}
		if got != v {
			t.Fatalf("request[%q] = %v, want %v", k, got, v)
		}
	}
	for _, k := range []string{"response_format", "tools", "tool_choice"} {
		if _, ok := req.Body[k]; ok {
			t.Fatalf("request carries %q, which the kinds never send", k)
		}
	}
	msgs, ok := req.Body["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("messages = %#v", req.Body["messages"])
	}
	m := msgs[0].(map[string]any)
	if m["role"] != "user" || m["content"] != "hello" {
		t.Fatalf("messages[0] = %#v", m)
	}
	if req.Authorization != "Bearer sk-test-1" {
		t.Fatalf("Authorization = %q", req.Authorization)
	}
}

func TestClientNoKeySendsNoAuthorization(t *testing.T) {
	cs := newCapturingServer(t, func(w http.ResponseWriter, _ int64) error {
		_, err := w.Write([]byte(okBody()))
		return err
	})
	c := New(cs.srv.URL, "")
	if _, err := c.Do(context.Background(), &Request{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}, N: 1}); err != nil {
		t.Fatal(err)
	}
	req := <-cs.requests
	if req.Authorization != "" {
		t.Fatalf("Authorization = %q, want none with no key configured", req.Authorization)
	}
}

func TestClientRedirectNeverFollowed(t *testing.T) {
	var targetHits atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits.Add(1)
		_, _ = w.Write([]byte(okBody()))
	}))
	defer target.Close()

	var sourceHits atomic.Int64
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sourceHits.Add(1)
		w.Header().Set("Location", target.URL+"/chat/completions")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	c := New(source.URL, "sk-test-1")
	_, err := c.Do(context.Background(), &Request{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}, N: 1})
	var se *StatusError
	if !errors.As(err, &se) || se.Code != http.StatusTemporaryRedirect {
		t.Fatalf("err = %v, want StatusError 307", err)
	}
	if sourceHits.Load() != 1 {
		t.Fatalf("source hits = %d, want 1 (a refusal is not a retry for a call with no response_format)", sourceHits.Load())
	}
	if targetHits.Load() != 0 {
		t.Fatalf("redirect target was hit %d times; a redirect must never be followed", targetHits.Load())
	}
}

func TestClientBodyCap(t *testing.T) {
	cs := newCapturingServer(t, func(w http.ResponseWriter, _ int64) error {
		big := make([]byte, 1<<20+1) // 1 MiB + 1 byte, over the cap
		for i := range big {
			big[i] = 'a'
		}
		_, err := w.Write(big)
		return err
	})
	c := New(cs.srv.URL, "")
	_, err := c.Do(context.Background(), &Request{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}, N: 1})
	if !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("err = %v, want ErrBodyTooLarge (1 MiB + 1 byte is over the cap)", err)
	}
}

func TestClientBodyExactly1MiBDecodes(t *testing.T) {
	// A body of exactly 1 MiB is within the cap: the extra byte exists so
	// the cap DETECTS oversize bodies rather than truncating them.
	const pre = `{"model":"m1","choices":[{"message":{"content":"`
	const post = `"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
	pad := 1<<20 - len(pre) - len(post)
	cs := newCapturingServer(t, func(w http.ResponseWriter, _ int64) error {
		body := pre + strings.Repeat("a", pad) + post
		if len(body) != 1<<20 {
			return errors.New("test body is not exactly 1 MiB")
		}
		_, err := w.Write([]byte(body))
		return err
	})
	c := New(cs.srv.URL, "")
	resp, err := c.Do(context.Background(), &Request{Model: "m1", Messages: []Message{{Role: "user", Content: "x"}}, N: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("response = %+v", resp)
	}
}

func TestClientJSONSchemaRetryExactlyOnce(t *testing.T) {
	cs := newCapturingServer(t, func(w http.ResponseWriter, n int64) error {
		if n == 1 {
			// A gateway that refuses json_schema outright.
			w.WriteHeader(http.StatusBadRequest)
			_, err := w.Write([]byte(`{"error":{"message":"response_format not supported"}}`))
			return err
		}
		_, err := w.Write([]byte(okBody()))
		return err
	})
	c := New(cs.srv.URL, "")
	_, err := c.Do(context.Background(), &Request{
		Model:          "m",
		Messages:       []Message{{Role: "user", Content: "x"}},
		N:              1,
		ResponseFormat: &ResponseFormat{Type: "json_schema", JSONSchema: &JSONSchemaSpec{Name: "out", Schema: map[string]any{"type": "object"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := cs.hits.Load(); got != 2 {
		t.Fatalf("server saw %d requests, want 2 (refused once, retried exactly once)", got)
	}
	first := <-cs.requests
	if _, ok := first.Body["response_format"]; !ok {
		t.Fatal("first request carried no response_format")
	}
	second := <-cs.requests
	if _, ok := second.Body["response_format"]; ok {
		t.Fatal("retry carried response_format; it must be dropped")
	}
}

func TestClientNoRetryWithoutResponseFormat(t *testing.T) {
	cs := newCapturingServer(t, func(w http.ResponseWriter, _ int64) error {
		w.WriteHeader(http.StatusInternalServerError)
		return nil
	})
	c := New(cs.srv.URL, "")
	_, err := c.Do(context.Background(), &Request{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}, N: 1})
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 500 {
		t.Fatalf("err = %v, want StatusError 500", err)
	}
	if got := cs.hits.Load(); got != 1 {
		t.Fatalf("server saw %d requests, want 1 (never retried)", got)
	}
}

func TestClientInvalidJSONIsDecodeError(t *testing.T) {
	cs := newCapturingServer(t, func(w http.ResponseWriter, _ int64) error {
		_, err := w.Write([]byte("not json at all"))
		return err
	})
	c := New(cs.srv.URL, "")
	_, err := c.Do(context.Background(), &Request{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}, N: 1})
	if !errors.Is(err, ErrDecode) {
		t.Fatalf("err = %v, want ErrDecode", err)
	}
}

// TestClientCoreSecretKey keeps the credential a core.Secret end to end:
// the client takes the typed secret, and nothing here renders it.
func TestClientCoreSecretKey(t *testing.T) {
	cs := newCapturingServer(t, func(w http.ResponseWriter, _ int64) error {
		_, err := w.Write([]byte(okBody()))
		return err
	})
	c := New(cs.srv.URL, core.Secret("sk-typed-1"))
	if _, err := c.Do(context.Background(), &Request{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}, N: 1}); err != nil {
		t.Fatal(err)
	}
	req := <-cs.requests
	if req.Authorization != "Bearer sk-typed-1" {
		t.Fatalf("Authorization = %q", req.Authorization)
	}
}
