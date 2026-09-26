// Package testwait sizes the polling waits of integration tests that drive
// real processes, sockets and PTYs, so a loaded CI runner slows them down
// instead of failing them.
//
// Tests only. A wait is still bounded: by a multiple of what the test asked
// for, and by the test binary's own -timeout, so a genuine hang fails on the
// test's budget with the test's message rather than running past it.
package testwait

import (
	"testing"
	"time"
)

// maxScale caps how far Budget stretches a wait. A wait this many times its
// idle-machine size is not a slow machine.
const maxScale = 4

// headroom is what Budget leaves before the test binary's -timeout, so a stuck
// wait fails with its own description rather than -timeout's goroutine dump.
const headroom = 10 * time.Second

// Budget is how long a test should wait for something that takes want on an
// idle machine: up to maxScale times want, when the test binary's -timeout
// leaves room for it, and never less than want.
//
// The fixed waits these tests used were sized against an idle machine, and
// `make check` runs every package at once, under -race, on a shared runner.
// On 2026-09-25 (CI run 13777) that ran the supervisor package for 530s
// against 26s locally, and 5s and 10s polls failed correct code throughout.
func Budget(t *testing.T, want time.Duration) time.Duration {
	t.Helper()
	if dl, ok := t.Deadline(); ok {
		if left := time.Until(dl) - headroom; left > want {
			return min(left, maxScale*want)
		}
	}
	return want
}

// Until polls cond every interval until it holds, and fails the test with
// desc if it does not within Budget(t, want).
func Until(t *testing.T, want, interval time.Duration, desc string, cond func() bool) {
	t.Helper()
	budget := Budget(t, want)
	deadline := time.Now().Add(budget)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for: %s", budget, desc)
		}
		time.Sleep(interval)
	}
}
