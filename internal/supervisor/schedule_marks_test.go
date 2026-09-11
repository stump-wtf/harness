package supervisor

// Governing: ADR-0007 (state.json), ADR-0013; SPEC-0008 REQ "Missed Window
// Handling"; issue #117.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
)

func readSchedules(t *testing.T, path string) map[string]persistedSchedule {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var doc persistedState
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse state: %v", err)
	}
	return doc.Schedules
}

// TestScheduleMarksSurviveRestart verifies a mark is on disk the moment
// UpdateScheduleMarks returns (no debounce), restores into a new Manager,
// drops marks for harnesses no longer configured, and can be forgotten.
func TestScheduleMarksSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	cfg := &core.Config{
		Harnesses: map[string]core.Harness{
			"sweep": {Name: "sweep", Prompt: "sweep", Schedule: "0 3 * * *", Restart: core.RestartNo},
		},
		HarnessOrder: []string{"sweep"},
	}
	opts := ManagerOptions{StatePath: statePath, LogDir: filepath.Join(dir, "logs")}

	m := NewManager(cfg, opts)
	decided := time.Date(2026, 9, 11, 3, 0, 0, 0, time.UTC)
	ran := decided.Add(2 * time.Second)
	if err := m.UpdateScheduleMarks(map[string]ScheduleMark{
		"sweep": {Spec: "0 3 * * *", DecidedThrough: decided, LastRunAt: ran},
		"ghost": {Spec: "@daily", DecidedThrough: decided},
	}, nil); err != nil {
		t.Fatalf("UpdateScheduleMarks: %v", err)
	}

	onDisk := readSchedules(t, statePath)
	if got, ok := onDisk["sweep"]; !ok || !got.DecidedThrough.Equal(decided) || got.LastRunAt == nil || !got.LastRunAt.Equal(ran) {
		t.Errorf("state.json schedules[sweep] = %+v (ok=%v), want the mark written synchronously", got, ok)
	}
	if _, ok := onDisk["ghost"]; ok {
		t.Error("a mark for an unconfigured harness was persisted")
	}
	m.Close()

	m2 := NewManager(cfg, opts)
	defer m2.Close()
	if err := m2.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	got, ok := m2.LoadScheduleMark("sweep")
	if !ok || got.Spec != "0 3 * * *" || !got.DecidedThrough.Equal(decided) || !got.LastRunAt.Equal(ran) {
		t.Errorf("restored mark = %+v (ok=%v)", got, ok)
	}

	if err := m2.UpdateScheduleMarks(nil, []string{"sweep"}); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if _, ok := m2.LoadScheduleMark("sweep"); ok {
		t.Error("forgotten mark still loads")
	}
	if _, ok := readSchedules(t, statePath)["sweep"]; ok {
		t.Error("forgotten mark still on disk")
	}
}
