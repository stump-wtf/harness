package testwait

import (
	"testing"
	"time"
)

// TestBudgetScalesWithinTheDeadline: a wait the -timeout has room for grows to
// maxScale times its size, and one it has no room for is left as asked, never
// shrunk. `go test` always sets a deadline (10m by default), which is what
// makes the first case reachable here.
func TestBudgetScalesWithinTheDeadline(t *testing.T) {
	dl, ok := t.Deadline()
	if !ok {
		t.Skip("no -timeout, so no deadline to scale against")
	}
	left := time.Until(dl) - headroom
	if left < maxScale*time.Second {
		t.Skipf("only %v left before -timeout", left)
	}
	if got := Budget(t, time.Second); got != maxScale*time.Second {
		t.Errorf("Budget(1s) = %v, want %v", got, maxScale*time.Second)
	}
	huge := time.Until(dl) + time.Hour
	if got := Budget(t, huge); got != huge {
		t.Errorf("Budget(%v) = %v, want it unchanged: a wait is never shortened", huge, got)
	}
}

// TestUntilReturnsOnceTheConditionHolds, without waiting out the budget.
func TestUntilReturnsOnceTheConditionHolds(t *testing.T) {
	n := 0
	began := time.Now()
	Until(t, time.Minute, time.Millisecond, "the third poll", func() bool {
		n++
		return n == 3
	})
	if n != 3 {
		t.Fatalf("polled %d times, want 3", n)
	}
	if took := time.Since(began); took > 10*time.Second {
		t.Errorf("took %v to see a condition that held on the third poll", took)
	}
}
