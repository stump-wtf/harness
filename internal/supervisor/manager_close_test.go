package supervisor

import (
	"testing"

	"github.com/stump-wtf/harness/internal/core"
)

// Close drains the run ledger, which is how a buffered line reaches disk. A
// caller that wants a drained ledger closes and may still have a t.Cleanup
// close again — that must be a no-op, not a panic.
func TestCloseIsIdempotent(t *testing.T) {
	cfg := &core.Config{Harnesses: map[string]core.Harness{}, Profiles: map[string]core.Profile{}}
	m := newTestManager(t, cfg)
	m.Close()
	m.Close() // panicked with "close of closed channel" before closeOnce
	m.Close()
}
