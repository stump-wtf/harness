package scheduler

import (
	"sync"
	"sync/atomic"
	"testing"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
)

// TestSchedulerApplyNoSchedules verifies Apply with no scheduled harnesses
// registers nothing.
func TestSchedulerApplyNoSchedules(t *testing.T) {
	var fired int32
	s := New(func(string) { atomic.AddInt32(&fired, 1) })
	defer s.Close()

	cfg := &core.Config{
		Harnesses: map[string]core.Harness{
			"a": {Name: "a", Prompt: "test"},
		},
		HarnessOrder: []string{"a"},
	}
	s.Apply(cfg)

	if len(s.entries) != 0 {
		t.Errorf("expected 0 entries, got %d", len(s.entries))
	}
}

// TestSchedulerApplyWithSchedule verifies a scheduled harness gets an entry.
func TestSchedulerApplyWithSchedule(t *testing.T) {
	var fired int32
	s := New(func(string) { atomic.AddInt32(&fired, 1) })
	defer s.Close()

	cfg := &core.Config{
		Harnesses: map[string]core.Harness{
			"a": {Name: "a", Prompt: "test", Schedule: "0 */6 * * *"},
		},
		HarnessOrder: []string{"a"},
	}
	s.Apply(cfg)

	if len(s.entries) != 1 {
		t.Errorf("expected 1 entry, got %d", len(s.entries))
	}
}

// TestSchedulerInvalidScheduleSkipped verifies an invalid cron expression is
// skipped without error.
func TestSchedulerInvalidScheduleSkipped(t *testing.T) {
	s := New(func(string) {})
	defer s.Close()

	cfg := &core.Config{
		Harnesses: map[string]core.Harness{
			"a": {Name: "a", Prompt: "test", Schedule: "not-a-cron"},
		},
		HarnessOrder: []string{"a"},
	}
	s.Apply(cfg)

	if len(s.entries) != 0 {
		t.Errorf("expected 0 entries for invalid schedule, got %d", len(s.entries))
	}
}

// TestSchedulerReapplyRemovesEntry verifies re-applying a config without a
// schedule removes the entry.
func TestSchedulerReapplyRemovesEntry(t *testing.T) {
	s := New(func(string) {})
	defer s.Close()

	cfgWith := &core.Config{
		Harnesses: map[string]core.Harness{
			"a": {Name: "a", Prompt: "test", Schedule: "0 */6 * * *"},
		},
		HarnessOrder: []string{"a"},
	}
	s.Apply(cfgWith)
	if len(s.entries) != 1 {
		t.Fatalf("expected 1 entry after first Apply, got %d", len(s.entries))
	}

	cfgWithout := &core.Config{
		Harnesses: map[string]core.Harness{
			"a": {Name: "a", Prompt: "test"},
		},
		HarnessOrder: []string{"a"},
	}
	s.Apply(cfgWithout)
	if len(s.entries) != 0 {
		t.Errorf("expected 0 entries after re-Apply, got %d", len(s.entries))
	}
}

// TestSchedulerFiresCallback verifies the scheduler actually fires the start
// callback on schedule (using a fast interval for testing).
func TestSchedulerFiresCallback(t *testing.T) {
	var fired int64
	var wg sync.WaitGroup
	wg.Add(1)
	s := New(func(name string) {
		if name == "worker" {
			atomic.AddInt64(&fired, 1)
			wg.Done()
		}
	})

	cfg := &core.Config{
		Harnesses: map[string]core.Harness{
			"worker": {Name: "worker", Prompt: "test", Schedule: "@every 1s"},
		},
		HarnessOrder: []string{"worker"},
	}
	s.Apply(cfg)
	s.Start()
	defer s.Close()

	wg.Wait()
	if atomic.LoadInt64(&fired) < 1 {
		t.Error("expected at least 1 firing")
	}
}
