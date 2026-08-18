package scheduler

import (
	"sync"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
)

// TestSchedulerApplyNoSchedules verifies Apply with no scheduled harnesses
// registers nothing.
func TestSchedulerApplyNoSchedules(t *testing.T) {
	s := New(func(string) {})
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
	s := New(func(string) {})
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
// skipped without error (defense in depth: config validation rejects these at
// parse time, so the scheduler only logs).
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

// TestSchedulerReapplyKeepsUnchangedEntry verifies an unchanged schedule
// survives a re-Apply with the SAME cron entry — the reconcile must not
// rebuild it, or interval schedules (@every N) would restart their countdown
// on every config reload and could starve forever under periodic rewrites.
func TestSchedulerReapplyKeepsUnchangedEntry(t *testing.T) {
	s := New(func(string) {})
	defer s.Close()

	cfg := &core.Config{
		Harnesses: map[string]core.Harness{
			"a": {Name: "a", Prompt: "test", Schedule: "@every 6h"},
		},
		HarnessOrder: []string{"a"},
	}
	s.Apply(cfg)
	first, ok := s.entries["a"]
	if !ok {
		t.Fatal("entry not registered")
	}

	s.Apply(cfg) // unchanged config: entry must be preserved, not rebuilt
	second, ok := s.entries["a"]
	if !ok {
		t.Fatal("entry lost on re-Apply")
	}
	if first.id != second.id {
		t.Errorf("unchanged entry was rebuilt: id %d -> %d", first.id, second.id)
	}
}

// TestSchedulerReapplyReplacesChangedEntry verifies a changed spec re-registers
// the entry under the new schedule.
func TestSchedulerReapplyReplacesChangedEntry(t *testing.T) {
	s := New(func(string) {})
	defer s.Close()

	cfg := &core.Config{
		Harnesses: map[string]core.Harness{
			"a": {Name: "a", Prompt: "test", Schedule: "0 */6 * * *"},
		},
		HarnessOrder: []string{"a"},
	}
	s.Apply(cfg)
	first := s.entries["a"]

	changed := &core.Config{
		Harnesses: map[string]core.Harness{
			"a": {Name: "a", Prompt: "test", Schedule: "0 */3 * * *"},
		},
		HarnessOrder: []string{"a"},
	}
	s.Apply(changed)
	second, ok := s.entries["a"]
	if !ok {
		t.Fatal("entry lost after spec change")
	}
	if second.spec != "0 */3 * * *" {
		t.Errorf("spec = %q, want %q", second.spec, "0 */3 * * *")
	}
	if first.id == second.id {
		t.Error("changed entry kept its old cron registration")
	}
}

// TestSchedulerFiresCallback verifies the scheduler actually fires the start
// callback on schedule (using a fast interval for testing). fired is closed
// exactly once — the callback may legitimately fire again before Close.
func TestSchedulerFiresCallback(t *testing.T) {
	fired := make(chan struct{})
	var once sync.Once
	s := New(func(name string) {
		if name == "worker" {
			once.Do(func() { close(fired) })
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

	<-fired
}

// TestSchedulerNextFire verifies NextFire resolves the cron entry's next
// firing time once started, and reports false for unscheduled harnesses or
// before Start (cron has not computed a firing time yet).
func TestSchedulerNextFire(t *testing.T) {
	s := New(func(string) {})
	defer s.Close()

	cfg := &core.Config{
		Harnesses: map[string]core.Harness{
			"a": {Name: "a", Prompt: "test", Schedule: "0 */6 * * *"},
			"b": {Name: "b", Prompt: "test"},
		},
		HarnessOrder: []string{"a", "b"},
	}
	s.Apply(cfg)

	if _, ok := s.NextFire("b"); ok {
		t.Error("unscheduled harness must report no next fire")
	}
	if _, ok := s.NextFire("missing"); ok {
		t.Error("unknown harness must report no next fire")
	}
	if _, ok := s.NextFire("a"); ok {
		t.Error("before Start the entry has no resolved firing time")
	}

	s.Start()
	next, ok := s.NextFire("a")
	if !ok {
		t.Fatal("scheduled harness must report a next fire after Start")
	}
	if !next.After(time.Now()) {
		t.Errorf("next fire %v is not in the future", next)
	}

	// A reload that keeps the spec unchanged must keep the phase — the
	// reported next fire cannot move (Apply leaves the entry untouched).
	before, _ := s.NextFire("a")
	s.Apply(cfg)
	after, _ := s.NextFire("a")
	if !before.Equal(after) {
		t.Errorf("unchanged schedule moved next fire: %v -> %v", before, after)
	}
}
