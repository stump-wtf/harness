package supervisor

// Governing tests: SPEC-0003 REQ "Crash-Loop Detection" (capped exponential
// backoff) and "Backoff Give-Up"; ADR-0005.

import (
	"testing"
	"time"
)

func TestBackoffCappedExponential(t *testing.T) {
	p := Policy{BackoffBase: 1 * time.Second, BackoffCap: 8 * time.Second}.normalize()
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{attempt: 0, want: 1 * time.Second}, // clamped to attempt 1
		{attempt: 1, want: 1 * time.Second},
		{attempt: 2, want: 2 * time.Second},
		{attempt: 3, want: 4 * time.Second},
		{attempt: 4, want: 8 * time.Second},  // hits cap
		{attempt: 5, want: 8 * time.Second},  // stays capped
		{attempt: 10, want: 8 * time.Second}, // stays capped, no overflow
	}
	for _, tc := range tests {
		if got := p.backoff(tc.attempt); got != tc.want {
			t.Errorf("backoff(%d) = %v, want %v", tc.attempt, got, tc.want)
		}
	}
}

func TestPolicyNormalizeFillsDefaults(t *testing.T) {
	got := Policy{}.normalize()
	if got.CrashWindow <= 0 || got.CrashThreshold <= 0 || got.BackoffBase <= 0 ||
		got.BackoffCap <= 0 || got.StopGrace <= 0 {
		t.Fatalf("normalize left a zero field: %+v", got)
	}
	if got.BackoffCap < got.BackoffBase {
		t.Fatalf("cap %v < base %v", got.BackoffCap, got.BackoffBase)
	}
}

func TestPolicyNormalizeCapBelowBase(t *testing.T) {
	got := Policy{BackoffBase: 10 * time.Second, BackoffCap: 1 * time.Second}.normalize()
	if got.BackoffCap < got.BackoffBase {
		t.Fatalf("cap not raised to base: cap=%v base=%v", got.BackoffCap, got.BackoffBase)
	}
}
