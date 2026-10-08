package budget

// Governing: ADR-0027; design.md § "A new internal/budget package owns
// admission and counters": the package is pure, like internal/hours, so its
// answers are a function of the caller's instant alone. Copied from
// internal/hours/guard_test.go.
//
// @joestump 10/04/2026 - Added for stump.wtf/harness#470.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoWallClockReads pins the injectable-clock contract: nothing in the
// package reads the time or arms a timer.
func TestNoWallClockReads(t *testing.T) {
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
					t.Errorf("%s:%d calls %s — internal/budget must stay pure (no clock, no I/O)", file, i+1, bad)
				}
			}
		}
	}
}
