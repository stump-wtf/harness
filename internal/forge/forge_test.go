package forge

// Forge Error Tests
//
// Governing tests: SPEC-0025 REQ-12 (a StatusError names method, status and
// repo, and carries the body only as a bounded, credential-redacted summary)
// and ADR-0032's failure table (Transient decides
// "retry next tick, no comment" against "tell the author").

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestStatusErrorNamesMethodStatusRepo(t *testing.T) {
	err := &StatusError{Method: "SquashMerge", Status: 405, Repo: "stump.wtf/harness"}
	if got, want := err.Error(), "forge: SquashMerge on stump.wtf/harness: HTTP 405"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
	err = &StatusError{Method: "SquashMerge", Status: 405, Repo: "stump.wtf/harness", Body: `{"message":"please try again later"}`}
	if got, want := err.Error(), "forge: SquashMerge on stump.wtf/harness: HTTP 405: {\"message\":\"please try again later\"}"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

func TestSanitizeBody(t *testing.T) {
	cases := []struct {
		name  string
		token string
		body  string
		want  string
	}{
		{"empty", "tok", "", ""},
		{"plain message", "tok", `{"message":"please try again later"}`, `{"message":"please try again later"}`},
		{"token echoed", "tok-abc", `{"message":"bad header: token tok-abc"}`, `{"message":"bad header: token [redacted]"}`},
		{"json auth header", "sekret", `{"Authorization":"token xyz"}`, `{"Authorization":"[redacted]"}`},
		{"json api key", "sekret", `{"api_key":"abcd1234"}`, `{"api_key":"[redacted]"}`},
		{"plain auth header", "sekret", `Authorization: Bearer xyz123`, `Authorization: [redacted] [redacted]`},
		{"plain api key", "sekret", `api_key=abcd1234`, `api_key: [redacted]`},
		{"basic scheme", "sekret", `401 (Unauthorized) - Bearer abc.def-ghi`, `401 (Unauthorized) - Bearer [redacted]`},
	}
	for _, tc := range cases {
		if got := SanitizeBody(tc.token, []byte(tc.body)); got != tc.want {
			t.Errorf("%s: SanitizeBody = %q, want %q", tc.name, got, tc.want)
		}
	}
	long := strings.Repeat("a", BodyLimit+50)
	got := SanitizeBody("tok", []byte(long))
	if len(got) != BodyLimit+len("…") || !strings.HasSuffix(got, "…") {
		t.Errorf("long body: len = %d, want %d + ellipsis", len(got), BodyLimit)
	}
}

func TestTransient(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"500", &StatusError{Status: 500}, true},
		{"503 wrapped", fmt.Errorf("list: %w", &StatusError{Status: 503}), true},
		{"429", &StatusError{Status: 429}, true},
		{"404", &StatusError{Status: 404}, false},
		{"405 merge refused", &StatusError{Status: 405}, false},
		{"409", &StatusError{Status: 409}, false},
		{"conflict", fmt.Errorf("build: %w", ErrConflict), false},
		{"stale", ErrStale, false},
		{"not found", ErrNotFound, false},
		{"cancelled", context.Canceled, false},
		{"deadline", context.DeadlineExceeded, true},
		{"transport", errors.New("dial tcp: connection refused"), true},
	}
	for _, tc := range cases {
		if got := Transient(tc.err); got != tc.want {
			t.Errorf("%s: Transient = %v, want %v", tc.name, got, tc.want)
		}
	}
}
