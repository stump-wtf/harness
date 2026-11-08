package trajectory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stump-wtf/agent-trace/tail"
	"github.com/stump-wtf/harness/internal/adapter"
	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/runtrace"
)

// The cases SPEC-0006 REQ "Run Correlation" asks of the trajectory read path:
// a native session is reported only when a run of this harness covers its
// start, in this harness's workdir, with no sibling that could equally have
// written it. The rule itself is runtrace's; these tests hold the facade's
// read path to it (#89).

// day is the date every fixture below shares, so the times read as one shift's
// worth of runs.
var day = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

func at(hour, min int) time.Time {
	return day.Add(time.Duration(hour)*time.Hour + time.Duration(min)*time.Minute)
}

// sessionAt writes a Claude Code transcript under the current HOME that
// started at started in cwd, and returns its path. Start time and cwd are the
// two signals the rule can see, so a fixture sets exactly those.
func sessionAt(t *testing.T, id, cwd string, started time.Time) string {
	t.Helper()
	dir := (&tail.ClaudeCodeAdapter{}).SessionDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "session-"+id+".jsonl")
	body := fmt.Sprintf(`{"type":"user","timestamp":%q,"sessionId":%q,"cwd":%q,"message":{"role":"user","content":"hello"}}`,
		started.Format(time.RFC3339), id, cwd)
	if err := os.WriteFile(path, []byte(body+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// listedIDs lists the harness's sessions and returns the reported ids.
func listedIDs(t *testing.T, svc *Service, cfg *core.Config, name string) []string {
	t.Helper()
	sessions, err := svc.List(cfg, name)
	if err != nil {
		t.Fatalf("List(%s): %v", name, err)
	}
	ids := make([]string, 0, len(sessions))
	for _, s := range sessions {
		if s.Source != "native" {
			t.Fatalf("List(%s) reported %s with source %q, want native", name, s.ID, s.Source)
		}
		ids = append(ids, s.ID)
	}
	return ids
}

func hasID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func TestListReportsOnlySessionsInsideARun(t *testing.T) {
	// SPEC-0006 REQ "Run Correlation" scenario "Sessions outside the run or
	// the workdir are not attributed": a session that started before the run
	// is not this harness's, however good its working directory looks.
	withFakeHome(t)
	cwd := t.TempDir()
	sessionAt(t, "inside", cwd, at(10, 30))
	sessionAt(t, "before", cwd, at(9, 0))
	sessionAt(t, "after", cwd, at(12, 0))

	svc := NewService(adapter.NewRegistryWithDefaults())
	svc.SetRunWindows(runsByHarness(map[string][]runtrace.Window{
		"agent": {{Start: at(10, 0), End: at(11, 0)}},
	}))
	cfg := configWith(core.Harness{Name: "agent", Adapter: "claude-code", Workdir: cwd, HarvestTrajectory: true})

	ids := listedIDs(t, svc, cfg, "agent")
	if !hasID(ids, "inside") {
		t.Fatalf("List = %v, want the session that started inside the run", ids)
	}
	for _, unwanted := range []string{"before", "after"} {
		if hasID(ids, unwanted) {
			t.Fatalf("List = %v, want %s excluded: it started outside every run", ids, unwanted)
		}
	}
}

func TestListExcludesSessionFromAnotherWorkdir(t *testing.T) {
	// A session started during the run in a different directory is a different
	// project to every tool here, so it is not this harness's — and Get, which
	// serves only what List reports, refuses its path.
	withFakeHome(t)
	cwd := t.TempDir()
	elsewhere := t.TempDir()
	sessionAt(t, "mine", cwd, at(10, 30))
	foreign := sessionAt(t, "theirs", elsewhere, at(10, 30))

	svc := NewService(adapter.NewRegistryWithDefaults())
	svc.SetRunWindows(runsByHarness(map[string][]runtrace.Window{
		"agent": {{Start: at(10, 0), End: at(11, 0)}},
	}))
	cfg := configWith(core.Harness{Name: "agent", Adapter: "claude-code", Workdir: cwd, HarvestTrajectory: true})

	if ids := listedIDs(t, svc, cfg, "agent"); !hasID(ids, "mine") || hasID(ids, "theirs") {
		t.Fatalf("List = %v, want only the session in %s", ids, cwd)
	}
	if traj, err := svc.Get(cfg, "agent", foreign); !errors.Is(err, ErrUnknownSession) {
		t.Fatalf("Get(foreign path): err = %v, traj = %+v, want ErrUnknownSession", err, traj)
	}
}

func TestListExcludesSessionTwoHarnessesCouldHaveWritten(t *testing.T) {
	// Siblings sharing a working directory and running at overlapping times
	// cannot be told apart by anything a transcript records, so the session is
	// excluded for both rather than guessed at.
	withFakeHome(t)
	cwd := t.TempDir()
	sessionAt(t, "contested", cwd, at(10, 20))

	svc := NewService(adapter.NewRegistryWithDefaults())
	svc.SetRunWindows(runsByHarness(map[string][]runtrace.Window{
		"agent": {{Start: at(10, 0), End: at(11, 0)}},
		"twin":  {{Start: at(10, 10), End: at(11, 30)}},
	}))
	cfg := configWith(
		core.Harness{Name: "agent", Adapter: "claude-code", Workdir: cwd, HarvestTrajectory: true},
		core.Harness{Name: "twin", Adapter: "claude-code", Workdir: cwd, HarvestTrajectory: true},
	)

	for _, name := range []string{"agent", "twin"} {
		if ids := listedIDs(t, svc, cfg, name); hasID(ids, "contested") {
			t.Fatalf("List(%s) = %v, want the contested session excluded", name, ids)
		}
	}
}

func TestListAttributesSessionWhenSiblingRunsAreStaggered(t *testing.T) {
	// The same two harnesses, run one after the other: the window says whose
	// it was, so the session is reported again.
	withFakeHome(t)
	cwd := t.TempDir()
	sessionAt(t, "mine", cwd, at(10, 20))

	svc := NewService(adapter.NewRegistryWithDefaults())
	svc.SetRunWindows(runsByHarness(map[string][]runtrace.Window{
		"agent": {{Start: at(10, 0), End: at(11, 0)}},
		"twin":  {{Start: at(11, 0), End: at(12, 0)}},
	}))
	cfg := configWith(
		core.Harness{Name: "agent", Adapter: "claude-code", Workdir: cwd, HarvestTrajectory: true},
		core.Harness{Name: "twin", Adapter: "claude-code", Workdir: cwd, HarvestTrajectory: true},
	)

	if ids := listedIDs(t, svc, cfg, "agent"); !hasID(ids, "mine") {
		t.Fatalf("List(agent) = %v, want the session the staggered window credits to it", ids)
	}
	if ids := listedIDs(t, svc, cfg, "twin"); hasID(ids, "mine") {
		t.Fatalf("List(twin) = %v, want only its own runs' sessions", ids)
	}
}

func TestListAcrossSeveralRuns(t *testing.T) {
	// List reports the harness's history, meaning every run it has had, newest
	// first — with nothing from the gaps between them.
	withFakeHome(t)
	cwd := t.TempDir()
	sessionAt(t, "first", cwd, at(9, 0))
	sessionAt(t, "second", cwd, at(15, 0))
	sessionAt(t, "gap", cwd, at(12, 0))

	svc := NewService(adapter.NewRegistryWithDefaults())
	svc.SetRunWindows(runsByHarness(map[string][]runtrace.Window{
		"agent": {{Start: at(8, 30), End: at(9, 30)}, {Start: at(14, 30), End: at(15, 30)}},
	}))
	cfg := configWith(core.Harness{Name: "agent", Adapter: "claude-code", Workdir: cwd, HarvestTrajectory: true})

	ids := listedIDs(t, svc, cfg, "agent")
	if len(ids) != 2 || ids[0] != "second" || ids[1] != "first" {
		t.Fatalf("List = %v, want [second first]", ids)
	}
}

func TestListWithoutRunRecordsReportsNoNativeSessions(t *testing.T) {
	// Fail closed: with no record of when the harness ran, nothing in a
	// machine-global store can be credited to it, and the honest answer is an
	// empty list rather than the whole machine.
	withFakeHome(t)
	cwd := t.TempDir()
	path := sessionAt(t, "old", cwd, at(10, 30))

	svc := NewService(adapter.NewRegistryWithDefaults())
	svc.SetScrollbackDir(t.TempDir())
	cfg := configWith(core.Harness{Name: "agent", Adapter: "claude-code", Workdir: cwd, HarvestTrajectory: true})

	if ids := listedIDs(t, svc, cfg, "agent"); len(ids) != 0 {
		t.Fatalf("List = %v, want no sessions without run records", ids)
	}
	if _, err := svc.Get(cfg, "agent", path); !errors.Is(err, ErrUnknownSession) {
		t.Fatalf("Get: err = %v, want ErrUnknownSession for a session nothing attributes", err)
	}
}

func TestListScrollbackNeedsNoRunRecords(t *testing.T) {
	// The fail-closed rule is about a shared native store. A harness with no
	// native transcript has its own scrollback file, which is not shared with
	// anybody, so it is still reported without run windows (ADR-0007).
	dir := t.TempDir()
	svc := NewService(adapter.NewRegistryWithDefaults())
	svc.SetScrollbackDir(dir)
	if err := os.WriteFile(filepath.Join(dir, "worker.log"), []byte("[worker]"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := configWith(core.Harness{Name: "worker", Adapter: "generic", HarvestTrajectory: true})

	sessions, err := svc.List(cfg, "worker")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 1 || sessions[0].Source != "scrollback" {
		t.Fatalf("List = %+v, want the scrollback record", sessions)
	}
}

func TestListUnreadablePeerHistoryHidesTheSession(t *testing.T) {
	// A sibling whose run history cannot be read is treated as having run
	// throughout: peers can only ever hide a session, so the unknown is
	// answered against exposure.
	withFakeHome(t)
	cwd := t.TempDir()
	sessionAt(t, "maybe", cwd, at(10, 20))

	svc := NewService(adapter.NewRegistryWithDefaults())
	svc.SetRunWindows(func(name string) (RunRecord, error) {
		if name == "twin" {
			return RunRecord{}, errors.New("lifecycle log unreadable")
		}
		return RunRecord{Runs: []runtrace.Window{{Start: at(10, 0), End: at(11, 0)}}}, nil
	})
	cfg := configWith(
		core.Harness{Name: "agent", Adapter: "claude-code", Workdir: cwd, HarvestTrajectory: true},
		core.Harness{Name: "twin", Adapter: "claude-code", Workdir: cwd, HarvestTrajectory: true},
	)

	if ids := listedIDs(t, svc, cfg, "agent"); hasID(ids, "maybe") {
		t.Fatalf("List = %v, want the session hidden while a sibling's history is unreadable", ids)
	}
}

func TestListFailsOnUnreadableOwnRuns(t *testing.T) {
	// An empty record and an unreadable one are different answers: the second
	// is an error, not a silently empty trajectory.
	withFakeHome(t)
	cwd := t.TempDir()
	svc := NewService(adapter.NewRegistryWithDefaults())
	svc.SetRunWindows(func(string) (RunRecord, error) { return RunRecord{}, errors.New("boom") })
	cfg := configWith(core.Harness{Name: "agent", Adapter: "claude-code", Workdir: cwd, HarvestTrajectory: true})

	if _, err := svc.List(cfg, "agent"); err == nil {
		t.Fatal("List: want the run-record error surfaced")
	}
}

func TestCoalesceWindows(t *testing.T) {
	// Touching windows cost one store pass and cover the same instants; a gap
	// between runs must stay a gap, or sessions nobody ran would be listed.
	runs := []runtrace.Window{
		{Start: at(10, 0), End: at(10, 30)},
		{Start: at(10, 29), End: at(10, 40)}, // overlaps the first
		{Start: at(10, 40), End: at(10, 50)}, // touches it inside slack
		{Start: at(11, 30), End: at(11, 40)}, // a separate run
		{Start: time.Time{}},                 // an unknown run vouches for nothing
		{Start: at(9, 0), End: at(9, 30)},    // out of order
	}
	got := coalesceWindows(runs)
	want := []runtrace.Window{
		{Start: at(9, 0), End: at(9, 30)},
		{Start: at(10, 0), End: at(10, 50)},
		{Start: at(11, 30), End: at(11, 40)},
	}
	if len(got) != len(want) {
		t.Fatalf("coalesceWindows = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("coalesceWindows[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestCoalesceWindowsOpenRunAbsorbs(t *testing.T) {
	// A run still going covers everything after its start, so later windows
	// add nothing to scan.
	got := coalesceWindows([]runtrace.Window{
		{Start: at(9, 0)},
		{Start: at(10, 0), End: at(10, 30)},
	})
	if len(got) != 1 || !got[0].Start.Equal(at(9, 0)) || !got[0].End.IsZero() {
		t.Fatalf("coalesceWindows = %+v, want one open window from 09:00", got)
	}
}

func TestCoalesceWindowsKeepsALiveRunOpen(t *testing.T) {
	// A harness restarted inside the slack of the previous run's end is the
	// same case seen from the other end: merging it into the closed window
	// must leave the result open, or nothing covers the run in progress and
	// the sessions it is writing now would be listed by neither window.
	got := coalesceWindows([]runtrace.Window{
		{Start: at(9, 0), End: at(9, 30)},
		{Start: at(9, 30)},
	})
	if len(got) != 1 || !got[0].Start.Equal(at(9, 0)) || !got[0].End.IsZero() {
		t.Fatalf("coalesceWindows = %+v, want one open window from 09:00", got)
	}
}
