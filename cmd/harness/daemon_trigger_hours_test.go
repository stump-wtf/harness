package main

// Daemon Wiring: Operating Hours On A Triggered Harness
//
// The firing gate is only real if the daemon wires it: the source manager
// must judge firings on the scheduler's clock, and the scheduler must be
// handed a Gate that can settle skips into a catch-up. A test that assembled
// either half would pass against a daemon that wired neither (#315), so this
// drives the daemon's own startDaemonScheduler and startDaemonSources against
// the Manager daemonManagerOptions builds and real processes, with one fake
// clock (stubClock, a scheduler.Clock) substituted into both.
//
// Everything asserted is something the daemon DID: records read back from
// state.json on disk, lines the spawned processes themselves wrote, and a run
// record's outcome — never a log line.
//
// Governing: ADR-0021, ADR-0019; SPEC-0014 REQ "Operating Hours On Triggered
// Harnesses", REQ "Overlap Skip Coalescing"; SPEC-0012 REQ "Gate Evaluation";
// issue #315.
//
// @joestump 09/23/2026 - Added for stump.wtf/harness#484.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/attach"
	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/hours"
	"github.com/stump-wtf/harness/internal/supervisor"
	"github.com/stump-wtf/harness/internal/trigger"
	"github.com/stump-wtf/harness/internal/trigger/channel/testserver"
)

// persistedRuns decodes name's run history out of the ledger on disk.
//
// It reads the ledger, not state.json. Since the run history moved to the
// append-only ledger (#639/#444), state.json holds each harness's id
// allocator alone; its record list is a one-boot legacy field the first
// daemon with the ledger drops after importing it. Reading state.json here
// finds no file, or an empty list.
//
// A record is a fold: the ledger appends an `opened` line, `updated`
// checkpoints and a `closed` (or a `decided` for a run that started nothing),
// so the last line for a (harness, run_id) is the record. Fold by id rather
// than assuming one line each.
//
// It DRAINS FIRST, by closing mgr: a coalesced skip's growing count rides on
// `updated` lines the ledger buffers on purpose (CoalesceRun appends with
// sync=false so a burst of firings does not cost one fsync each), so the file
// is behind by whatever is still queued. Without the drain this read saw 46 of
// 50. Manager.Close is idempotent, so the test's own t.Cleanup is still safe.
func persistedRuns(t *testing.T, statePath, name string, mgr *supervisor.Manager) []supervisor.RunRecord {
	t.Helper()
	mgr.Close() // drain the ledger's queue; the tail is not on disk until it does
	dir := filepath.Join(filepath.Dir(statePath), "ledger")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read ledger dir %s: %v (run history lives in the ledger now)", dir, err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	type folded struct {
		outcome, reason, trigger, source string
		coalesced                        int
	}
	byID := map[int]folded{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		f, err := os.Open(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("open ledger %s: %v", e.Name(), err)
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			var line struct {
				Harness   string `json:"harness"`
				RunID     int    `json:"run_id"`
				Outcome   string `json:"outcome"`
				Reason    string `json:"reason"`
				Trigger   string `json:"trigger"`
				Source    string `json:"source"`
				Coalesced int    `json:"coalesced"`
			}
			if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
				t.Fatalf("decode ledger line in %s: %v", e.Name(), err)
			}
			if line.Harness != name || line.RunID == 0 {
				continue
			}
			prev := byID[line.RunID]
			if line.Outcome != "" {
				prev.outcome = line.Outcome
			}
			if line.Reason != "" {
				prev.reason = line.Reason
			}
			if line.Trigger != "" {
				prev.trigger = line.Trigger
			}
			if line.Source != "" {
				prev.source = line.Source
			}
			if line.Coalesced != 0 {
				prev.coalesced = line.Coalesced
			}
			byID[line.RunID] = prev
		}
		if err := sc.Err(); err != nil {
			t.Fatalf("scan ledger %s: %v", e.Name(), err)
		}
		_ = f.Close()
	}

	ids := make([]int, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	out := make([]supervisor.RunRecord, len(ids))
	for i, id := range ids {
		f := byID[id]
		r := supervisor.RunRecord{
			RunID: id, Outcome: supervisor.RunOutcome(f.outcome),
			Reason: supervisor.RunReason(f.reason), Source: f.source,
			// The ledger carries the running count, so this is the real
			// coalesced value and the assertion below tests it.
			Coalesced: f.coalesced,
		}
		if f.trigger != "" {
			r.Trigger = supervisor.RunTrigger(f.trigger)
		}
		out[i] = r
	}
	return out
}

// markerLines is what the harness's processes wrote, one line per spawn.
func markerLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(b))
}

func TestDaemonGatesFiringsOnOperatingHours(t *testing.T) {
	tmp := t.TempDir()
	statePath := filepath.Join(tmp, "state.json")
	reg := attach.NewRegistry(100)
	opts := daemonManagerOptions(reg)
	opts.StatePath = statePath
	opts.LogDir = filepath.Join(tmp, "logs")
	opts.JobsDir = filepath.Join(tmp, "jobs")
	opts.Policy.StopGrace = 200 * time.Millisecond

	const expr = "TZ=UTC Mon-Fri 09:00-18:00"
	e, err := hours.Parse(expr)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(tmp, "marker")
	// quick: every spawn writes its trigger and exits.
	quick := core.Harness{
		Name: "pr-review", Adapter: "generic", Backend: core.BackendNative,
		Args:    []string{"-c", `echo "$HARNESS_RUN_TRIGGER" >> '` + marker + `'`},
		Restart: core.RestartNo,

		Triggers: []string{"webhook.gh"}, Timeout: core.DefaultRunTimeout,
		OnOverlap: core.OverlapQueue, KeepRuns: 500,
		OperatingHours: expr, HoursExpr: e, CatchUp: true,
	}
	// long: runs until its timeout, to show a run in flight at the close is
	// left to it rather than to the gate.
	long := core.Harness{
		Name: "long", Adapter: "generic", Backend: core.BackendNative,
		Args:    []string{"-c", "while true; do sleep 0.02; done"},
		Restart: core.RestartNo,

		Triggers: []string{"webhook.long"}, Timeout: 1500 * time.Millisecond,
		OnOverlap: core.OverlapQueue, KeepRuns: 500,
		OperatingHours: expr, HoursExpr: e,
	}
	cfg := &core.Config{
		Harnesses:    map[string]core.Harness{quick.Name: quick, long.Name: long},
		HarnessOrder: []string{quick.Name, long.Name},
		Profiles:     map[string]core.Profile{},
		Webhooks: map[string]core.WebhookSource{
			"gh":   {Name: "gh", Verify: core.VerifyGitea, MaxBody: core.DefaultWebhookMaxBody},
			"long": {Name: "long", Verify: core.VerifyGitea, MaxBody: core.DefaultWebhookMaxBody},
		},
		WebhookOrder: []string{"gh", "long"},
	}

	mgr := supervisor.NewManager(cfg, opts)
	t.Cleanup(mgr.Close)
	reg.SetController(mgr)
	if err := mgr.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	// Saturday afternoon. One clock for both halves: the daemon's own
	// scheduler and source manager.
	clock := &stubClock{now: time.Date(2026, 9, 26, 14, 0, 0, 0, time.UTC), ticks: make(chan time.Time)}
	sched := startDaemonScheduler(mgr, cfg, clock)
	t.Cleanup(sched.Close)
	sources := startDaemonSources(mgr, clock)
	t.Cleanup(sources.Close)

	set := func(at time.Time) {
		clock.mu.Lock()
		clock.now = at
		clock.mu.Unlock()
	}
	// fire is a fake webhook source: it stamps the receive time from the
	// clock, as a listener does, and fans the event out through the daemon's
	// source manager.
	n := 0
	fire := func(src string) trigger.Envelope {
		n++
		id := fmt.Sprintf("d-%d", n)
		ev := trigger.Envelope{
			Version: trigger.EnvelopeVersion, Kind: trigger.KindWebhook,
			Source: src, EventID: id, ReceivedAt: clock.Now().UTC(),
			Webhook: &trigger.WebhookEvent{Event: "pull_request", Delivery: id, ContentType: "application/json"},
		}
		ev.Webhook.SetBody("application/json", []byte(`{"n":1}`))
		return ev
	}

	// --- A weekend of deliveries: 50 firings, one record, no process. ---
	for i := 0; i < 50; i++ {
		ev := fire("webhook.gh")
		d := sources.Fire(&ev)
		if len(d) != 1 || d[0].Kind != supervisor.DecisionSkipped || d[0].Reason != supervisor.ReasonOutsideHours {
			t.Fatalf("saturday firing %d: decisions = %+v, want skipped / outside_hours", i, d)
		}
	}
	if got := markerLines(t, marker); len(got) != 0 {
		t.Fatalf("processes ran out of hours: %q", got)
	}

	// --- A manual trigger is not gated. ---
	if d, ok := mgr.StartRun(quick.Name, supervisor.RunRequest{Trigger: supervisor.TriggerManual}); !ok || d.Kind != supervisor.DecisionStarted {
		t.Fatalf("manual trigger on a saturday = %+v, want started", d)
	}
	waitUntil(t, "the manual run ran", func() bool { return len(markerLines(t, marker)) == 1 })

	// --- Monday 09:00: exactly one catch_up run, on the first in-hours tick. ---
	set(time.Date(2026, 9, 28, 8, 59, 59, 0, time.UTC))
	clock.ticks <- clock.Now()
	set(time.Date(2026, 9, 28, 9, 0, 0, 0, time.UTC))
	clock.ticks <- clock.Now()
	waitUntil(t, "the catch-up run ran", func() bool { return len(markerLines(t, marker)) == 2 })
	for i := 1; i <= 5; i++ {
		set(time.Date(2026, 9, 28, 9, 0, i, 0, time.UTC))
		clock.ticks <- clock.Now()
	}
	waitUntil(t, "the catch-up run is recorded finished", func() bool {
		rs := mgr.Runs(quick.Name)
		return len(rs) == 3 && rs[2].Outcome == supervisor.OutcomeSuccess
	})
	if got := markerLines(t, marker); len(got) != 2 || got[0] != "manual" || got[1] != "catch_up" {
		t.Errorf("spawns = %q, want [manual catch_up]: one catch-up, and only one", got)
	}
	if rs := mgr.Runs(quick.Name); rs[2].Trigger != supervisor.TriggerCatchUp {
		t.Errorf("run 3 = %+v, want trigger catch_up", rs[2])
	}

	// --- A run in flight at the close continues, bounded by its timeout. ---
	set(time.Date(2026, 9, 28, 17, 59, 59, 0, time.UTC))
	ev := fire("webhook.long")
	if d := sources.Fire(&ev); d[0].Kind != supervisor.DecisionStarted {
		t.Fatalf("17:59:59 firing = %+v, want started", d)
	}
	waitUntil(t, "the long run is up", func() bool {
		s, _ := mgr.Snapshot(long.Name)
		return s.State == core.StateRunning
	})
	// The close, exactly: 18:00:00 is out of hours (end-exclusive). A firing
	// at this instant is skipped outside_hours — in hours, with a run in
	// flight and on_overlap = queue, it would have been queued.
	set(time.Date(2026, 9, 28, 18, 0, 0, 0, time.UTC))
	clock.ticks <- clock.Now()
	ev = fire("webhook.long")
	if d := sources.Fire(&ev); d[0].Kind != supervisor.DecisionSkipped || d[0].Reason != supervisor.ReasonOutsideHours {
		t.Fatalf("18:00:00 firing = %+v, want skipped / outside_hours", d)
	}
	for i := 1; i <= 3; i++ {
		set(time.Date(2026, 9, 28, 18, 0, i, 0, time.UTC))
		clock.ticks <- clock.Now()
	}
	// Watched for a while rather than read once: a hold the pass dispatched
	// runs on its own goroutine and would land within the stop grace.
	for deadline := time.Now().Add(400 * time.Millisecond); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if s, _ := mgr.Snapshot(long.Name); s.State != core.StateRunning || s.Held {
			t.Fatalf("after the close the long run is %s (held=%v); the gate must leave a run in flight alone", s.State, s.Held)
		}
	}
	waitUntil(t, "the long run ends at its timeout", func() bool {
		rs := mgr.Runs(long.Name)
		return len(rs) > 0 && rs[0].Outcome != supervisor.OutcomeRunning
	})
	if rs := mgr.Runs(long.Name); rs[0].Outcome != supervisor.OutcomeTimedOut {
		t.Errorf("long run = %+v, want timed_out: its timeout bounds it, not the gate", rs[0])
	}
	// Read the record back off disk, LAST: this closes mgr to drain the
	// ledger, and the assertions above need it running. A coalesced skip's
	// count arrives on BUFFERED `updated` lines (CoalesceRun appends with
	// sync=false so a burst of firings does not cost one fsync each), so the
	// file lags the queue — this read saw 46 of 50 before the drain.
	runs := persistedRuns(t, statePath, quick.Name, mgr)
	if len(runs) < 1 {
		t.Fatalf("the ledger holds no records for the 50 out-of-hours firings: %+v", runs)
	}
	// Run 1 is the weekend's coalesced skip; the manual and catch_up runs the
	// rest of this test added follow it.
	if r := runs[0]; r.RunID != 1 || r.Outcome != supervisor.OutcomeSkipped || r.Reason != supervisor.ReasonOutsideHours || r.Coalesced != 50 || r.Trigger != supervisor.TriggerWebhook || r.Source != "webhook.gh" {
		t.Errorf("run 1 = %+v, want skipped / outside_hours / coalesced 50 / webhook.gh", r)
	}
}

// TestDaemonJudgesADoorbellOnTheSchedulerClock is the clock half of the
// wiring: a channel doorbell is stamped, and judged, on the clock the daemon
// hands startDaemonSources — the scheduler's seam — not on the wall clock.
//
// The hours are built around the REAL date (yesterday, today and tomorrow, all
// day) and the fake clock sits three days out, so the two clocks disagree
// about the gate whenever this runs: a daemon that left the source manager on
// the wall clock would start a run here instead of recording two skips.
func TestDaemonJudgesADoorbellOnTheSchedulerClock(t *testing.T) {
	srv := testserver.New(testserver.Options{})
	t.Cleanup(srv.Close)

	tmp := t.TempDir()
	reg := attach.NewRegistry(100)
	opts := daemonManagerOptions(reg)
	opts.StatePath = filepath.Join(tmp, "state.json")
	opts.LogDir = filepath.Join(tmp, "logs")
	opts.JobsDir = filepath.Join(tmp, "jobs")
	opts.Policy.StopGrace = 200 * time.Millisecond

	wall := time.Now().UTC()
	day := func(d int) string { return wall.AddDate(0, 0, d).Weekday().String()[:3] }
	expr := fmt.Sprintf("TZ=UTC %s,%s,%s 00:00-24:00", day(-1), day(0), day(1))
	e, err := hours.Parse(expr)
	if err != nil {
		t.Fatal(err)
	}
	if in, _, _ := e.In(wall); !in {
		t.Fatalf("test setup: %q should contain the real now", expr)
	}
	fake := wall.AddDate(0, 0, 3)
	if in, _, _ := e.In(fake); in {
		t.Fatalf("test setup: %q should exclude the fake clock", expr)
	}

	marker := filepath.Join(tmp, "marker")
	h := core.Harness{
		Name: "sweep", Adapter: "generic", Backend: core.BackendNative,
		Args:    []string{"-c", `echo ran >> '` + marker + `'`},
		Restart: core.RestartNo,

		Triggers: []string{"channel.sb"}, Timeout: core.DefaultRunTimeout,
		OnOverlap: core.OverlapQueue, KeepRuns: 500,
		OperatingHours: expr, HoursExpr: e, CatchUp: true,
	}
	cfg := &core.Config{
		Harnesses:    map[string]core.Harness{h.Name: h},
		HarnessOrder: []string{h.Name},
		Profiles:     map[string]core.Profile{},
		Channels:     map[string]core.ChannelSource{"sb": {Name: "sb", URL: srv.URL, Enabled: true}},
		ChannelOrder: []string{"sb"},
	}
	mgr := supervisor.NewManager(cfg, opts)
	t.Cleanup(mgr.Close)
	reg.SetController(mgr)
	if err := mgr.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	clock := &stubClock{now: fake, ticks: make(chan time.Time)}
	sched := startDaemonScheduler(mgr, cfg, clock)
	t.Cleanup(sched.Close)
	sources := startDaemonSources(mgr, clock)
	t.Cleanup(sources.Close)

	// The first connect owes a catch-up (REQ "Channel Catch-Up"); out of
	// hours it is recorded skipped instead of run.
	waitUntil(t, "the connect catch-up is recorded", func() bool { return len(mgr.Runs(h.Name)) == 1 })
	srv.PushNotification(`{"content":"ding","meta":{"todo_id":"t-1"}}`)
	waitUntil(t, "the doorbell is recorded", func() bool { return len(mgr.Runs(h.Name)) == 2 })

	for i, r := range mgr.Runs(h.Name) {
		if r.Outcome != supervisor.OutcomeSkipped || r.Reason != supervisor.ReasonOutsideHours {
			t.Errorf("record %d = %+v, want skipped / outside_hours on the fake clock", i, r)
		}
	}
	if rs := mgr.Runs(h.Name); rs[0].Trigger != supervisor.TriggerCatchUp || rs[1].Trigger != supervisor.TriggerChannel {
		t.Errorf("records = %+v, want the connect catch-up then the doorbell", rs)
	}
	if got := markerLines(t, marker); len(got) != 0 {
		t.Errorf("processes ran out of hours: %q", got)
	}
}
