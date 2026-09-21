package hours

// Governing: ADR-0019, SPEC-0012 REQ "Operating Hours Key"; design.md §
// "A pure internal/hours package".
//
// @joestump-agent 09/21/2026 - Copied from
// internal/scheduler/scheduler_test.go's TestNoWallClockReadsOutsideClock
// (#381): this package has no clock.go seam at all (unlike the scheduler,
// which reads the wall clock from one file) — every temporal answer must come
// from the caller-supplied time.Time, so the forbidden list applies to every
// non-test file with no exception.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoWallClockReadsOutsideClock pins the injectable-clock contract: the
// package never reads the time or arms a timer itself, so In's behavior is
// entirely a function of its caller-supplied time.Time.
func TestNoWallClockReadsOutsideClock(t *testing.T) {
	forbidden := []string{
		"time.Now(", "time.Since(", "time.Until(", "time.Sleep(",
		"time.NewTimer(", "time.NewTicker(", "time.AfterFunc(", "time.After(", "time.Tick(",
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			code := strings.TrimSpace(line)
			if strings.HasPrefix(code, "//") {
				continue
			}
			for _, bad := range forbidden {
				if strings.Contains(code, bad) {
					t.Errorf("%s:%d calls %s — internal/hours must stay pure (no clock, no I/O)", file, i+1, bad)
				}
			}
		}
	}
}
