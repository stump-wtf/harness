package forge

// Forge Error Tests
//
// Governing tests: SPEC-0025 REQ-12 (a StatusError names method, status and
// repo, and nothing else) and ADR-0032's failure table (Transient decides
// "retry next tick, no comment" against "tell the author").

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestStatusErrorNamesMethodStatusRepo(t *testing.T) {
	err := &StatusError{Method: "SquashMerge", Status: 405, Repo: "stump.wtf/harness"}
	if got, want := err.Error(), "forge: SquashMerge on stump.wtf/harness: HTTP 405"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
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
