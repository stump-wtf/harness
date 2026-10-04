package supervisor

// Quota Park Tests
//
// SPEC-0021 REQ-11, REQ-13, REQ-14 and REQ-19 on the real Manager and real
// processes. The observer is stood in for by the quota sync seam: a stand-in
// agent appends its model outcomes to a marks file (its "transcript") and
// exits, and the seam feeds every new line to QuotaObserve when the exit path
// asks, as the daemon's feed does with the real observer. So a mark reaches
// the detector only through the exit path's own sync, exactly the race the
// real wiring closes; cmd/harness drives the same shapes through the real
// observer and a crush session store.
//
// The Manager's clock is a fake one, as the gate's is: parks, admission and
// the clearer read it. The gate pass is driven by hand (gate below): what the
// clearer reports cleared is released.
//
// Governing: ADR-0027, ADR-0008; SPEC-0021 REQ-11, REQ-12, REQ-13, REQ-14,
// REQ-19, REQ-20; SPEC-0003 REQ "Backoff Give-Up".
//
// @joestump 10/04/2026 - Added for stump.wtf/harness#477.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
)

// tarsNote is the 2026-10-04 incident's error mark, as agent-trace v0.7.1
// renders crush's finish part.
const tarsNote = "Payment Required: You're out of credits. Add more at https://hyper.charm.land"

// fakeNow is a settable clock for ManagerOptions.Now.
type fakeNow struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeNow) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeNow) Set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

// quotaRig is a Manager on a fake clock whose quota sync seam reads the
// marks file its stand-in agents append to.
type quotaRig struct {
	m      *Manager
	clock  *fakeNow
	env    runsEnv
	marks  string
	mu     sync.Mutex
	fed    int
	syncs  int
	closer func()
}

func newQuotaRig(t *testing.T, cfg *core.Config, p Policy, at time.Time) *quotaRig {
	t.Helper()
	r := &quotaRig{clock: &fakeNow{t: at}, env: newRunsEnv(t)}
	r.marks = filepath.Join(r.env.dir, "marks")
	r.boot(t, cfg, p)
	return r
}

// boot starts a Manager on the rig's state, as a daemon (re)start does.
func (r *quotaRig) boot(t *testing.T, cfg *core.Config, p Policy) {
	t.Helper()
	m := NewManager(cfg, ManagerOptions{Policy: p, StatePath: r.env.state, LogDir: r.env.logs, JobsDir: r.env.jobs, Now: r.clock.Now})
	r.closer = sync.OnceFunc(m.Close)
	t.Cleanup(r.closer)
	m.SetQuotaSync(r.sync)
	if err := m.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	r.m = m
}

// sync is the stand-in observer: every marks line not yet fed reaches
// QuotaObserve, dated now (after the run's start, as the real marks are).
func (r *quotaRig) sync(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.syncs++
	f, err := os.Open(r.marks)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		n++
		if n <= r.fed {
			continue
		}
		name, rest, _ := strings.Cut(sc.Text(), "|")
		if rest == "ok" {
			r.m.QuotaObserve(name, "crush", time.Now(), true, "")
		} else {
			r.m.QuotaObserve(name, "crush", time.Now(), false, rest)
		}
	}
	r.fed = n
	return sc.Err()
}

// syncCount is how many times the exit path asked the seam.
func (r *quotaRig) syncCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.syncs
}

// gate is one gate tick at the rig's clock: every hold the clearers report
// cleared is released, as the scheduler's pass does.
func (r *quotaRig) gate() {
	for name, rs := range r.m.HoldsCleared(r.clock.Now()) {
		for _, reason := range rs.Reasons() {
			r.m.Release(name, reason)
		}
	}
}

// agent is a stand-in agent's script: it writes outcome (a note, or "ok")
// to its transcript and exits with code.
func (r *quotaRig) agent(name, outcome string, code int) string {
	quoted := strings.ReplaceAll(name+"|"+outcome, "'", `'\''`)
	return fmt.Sprintf("printf '%%s\\n' '%s' >> '%s'; exit %d", quoted, r.marks, code)
}

func (r *quotaRig) snap(t *testing.T, name string) Snapshot {
	t.Helper()
	s, ok := r.m.Snapshot(name)
	if !ok {
		t.Fatalf("unknown harness %q", name)
	}
	return s
}

func parkedSnap(s Snapshot) bool {
	return s.State == core.StateStopped && s.Holds.Has(core.HoldQuota)
}

// the daemon's restart policy (MaxRestarts = 5, the give-up the 2026-09-19
// resident walked into), with only its durations shrunk.
func productionPolicy(t *testing.T) Policy {
	t.Helper()
	p := DefaultPolicy()
	if p.MaxRestarts != 5 {
		t.Fatalf("the daemon's MaxRestarts is %d; the scenario is written against 5", p.MaxRestarts)
	}
	p.CrashWindow = 50 * time.Millisecond
	p.BackoffBase = 2 * time.Millisecond
	p.BackoffCap = 10 * time.Millisecond
	p.HealthyRun = time.Hour
	p.StopGrace = 300 * time.Millisecond
	return p
}

// countExits counts the "exited" lines on name's durable log.
func countExits(t *testing.T, logs, name string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(logs, name+".log"))
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), " exited code=")
}

// TestThe20260919Shape is SPEC-0021 REQ-13 Scenario "The 2026-09-19 shape" on
// the daemon's restart policy (MaxRestarts = 5): a resident whose provider
// refuses every call, written as a stand-in agent that records a quota error
// with no reset time and exits non-zero on every start. It parks on the third
// exit, never reaches `failed` however many times it exits (well past seven,
// where give-up would have fired at the sixth), and starts again by itself
// each time a park expires, with each park twice as long as the last.
//
// Revert the restart-policy consult (onProcessGone's quotaExit) and this
// fails: the sixth consecutive failed exit gives up into `failed`.
func TestThe20260919Shape(t *testing.T) {
	t0 := time.Date(2026, 9, 19, 9, 0, 0, 0, time.UTC)
	env := newRunsEnv(t)
	r := &quotaRig{clock: &fakeNow{t: t0}, env: env, marks: filepath.Join(env.dir, "marks")}
	h := shHarness("crush-sb", r.agent("crush-sb", "429 Too Many Requests: rate limit exceeded", 1), time.Millisecond)
	h.Restart = core.RestartAlways
	h.Budget.QuotaBackoff, h.Budget.QuotaBackoffMax = time.Minute, time.Hour
	r.boot(t, managerCfg(h), productionPolicy(t))
	events, cancel := r.m.Events()
	defer cancel()
	failed := make(chan Event, 1)
	go func() {
		for ev := range events {
			if ev.Kind == EventStateChanged && ev.Name == h.Name && ev.To == core.StateFailed {
				select {
				case failed <- ev:
				default:
				}
			}
		}
	}()

	r.m.Start(h.Name)
	var parks []ParkInfo
	parkedOrFailed := func(s Snapshot) bool { return parkedSnap(s) || s.State == core.StateFailed }
	for cycle := range 3 {
		snap := waitSnapshot(t, r.m, h.Name, fmt.Sprintf("parked (cycle %d)", cycle), parkedOrFailed)
		if snap.State == core.StateFailed {
			t.Fatalf("cycle %d: gave up into failed after %d exits, consecutive failures %d", cycle, countExits(t, r.env.logs, h.Name), snap.ConsecutiveFailures)
		}
		p, ok := r.m.ParkOf(h.Name)
		if !ok {
			t.Fatalf("cycle %d: held for quota with no park in force", cycle)
		}
		parks = append(parks, p)
		if !snap.Enabled || snap.ConsecutiveFailures != 0 || snap.RestartCount == 0 {
			t.Fatalf("cycle %d: parked snapshot %+v, want enabled, give-up count reset", cycle, snap)
		}
		select {
		case ev := <-failed:
			t.Fatalf("reached failed: %+v", ev)
		default:
		}
		// The park expires; the gate tick releases it and the harness
		// starts by itself, through admission.
		started := snap.LastStarted
		r.clock.Set(p.Until)
		r.gate()
		waitSnapshot(t, r.m, h.Name, "started again at the reset", func(s Snapshot) bool { return s.LastStarted.After(started) })
	}
	if snap := waitSnapshot(t, r.m, h.Name, "parked a fourth time", parkedOrFailed); snap.State == core.StateFailed {
		t.Fatalf("gave up into failed after %d exits", countExits(t, r.env.logs, h.Name))
	}
	if n := countExits(t, r.env.logs, h.Name); n < 7 {
		t.Fatalf("%d exits, want at least seven", n)
	}
	select {
	case ev := <-failed:
		t.Fatalf("reached failed: %+v", ev)
	default:
	}
	// Each park began when the previous one ended, and lasts twice as long:
	// quota_backoff 1m, doubling (REQ-12).
	began := t0
	for i, want := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute} {
		if got := parks[i].Until.Sub(began); got != want {
			t.Errorf("park %d lasts %v, want %v", i, got, want)
		}
		if parks[i].Step != i || parks[i].Rule != "common/rate limit" {
			t.Errorf("park %d = %+v, want step %d by common/rate limit", i, parks[i], i)
		}
		began = parks[i].Until
	}
}

// SPEC-0021 REQ-19 Scenario "A park is logged", with REQ-13's resident
// effects: crush-sb, running, is parked by rule crush/litellm.ratelimiterror
// until 15:00. It stops at once, is not restarted and keeps `enabled`; the
// park is in state.json; its durable log names the rule, the reset and that
// no restart happens; and subscribers receive harness_hold_changed carrying
// the reset as its next.
func TestAParkIsLogged(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	until := time.Date(2026, 10, 4, 15, 0, 0, 0, time.UTC)
	h := shHarness("crush-sb", loopScript, 0)
	h.Restart = core.RestartAlways
	r := newQuotaRig(t, managerCfg(h), fastPolicy(), now)
	events, cancel := r.m.Events()
	defer cancel()
	r.m.Start(h.Name)
	up := waitSnapshot(t, r.m, h.Name, "running", func(s Snapshot) bool { return s.State == core.StateRunning })

	r.m.QuotaObserve(h.Name, "crush", time.Now(), false,
		"Too Many Requests: litellm.RateLimitError: RateLimitError: quota resets at 2026-10-04T15:00:00Z")
	snap := waitSnapshot(t, r.m, h.Name, "parked", parkedSnap)
	if !snap.Enabled || snap.RestartCount != up.RestartCount {
		t.Fatalf("parked: enabled=%v restarts %d→%d, want enabled and no restart", snap.Enabled, up.RestartCount, snap.RestartCount)
	}
	p, ok := r.m.ParkOf(h.Name)
	if !ok || !p.Until.Equal(until) || p.Rule != "crush/litellm.ratelimiterror" || p.By != h.Name {
		t.Fatalf("park = %+v ok=%v, want until 15:00 by crush/litellm.ratelimiterror", p, ok)
	}
	if rec := readParks(t, r.env.state)["harness:crush-sb"]; !rec.Until.Equal(until) || rec.Rule != p.Rule {
		t.Fatalf("state.json park = %+v, want until 15:00 by %s", rec, p.Rule)
	}
	logText := waitLogContains(t, r.env.logs, h.Name, " parked reason=quota")
	for _, want := range []string{"rule=crush/litellm.ratelimiterror", "until=2026-10-04T15:00:00Z", `restart="none before the reset"`} {
		if !strings.Contains(logText, want) {
			t.Errorf("the park's durable-log line lacks %q:\n%s", want, logText)
		}
	}
	waitFor(t, 3*time.Second, "harness_hold_changed with the reset", func() bool {
		for {
			select {
			case ev := <-events:
				if ev.Kind == EventHoldChanged && ev.Name == h.Name && ev.Holds == core.HoldSetOf(core.HoldQuota) && ev.HoldNext.Equal(until) {
					return true
				}
			default:
				return false
			}
		}
	})
	time.Sleep(100 * time.Millisecond) // ample for a respawn the restart policy would arm
	if after := r.snap(t, h.Name); !parkedSnap(after) || after.LastStarted != snap.LastStarted {
		t.Fatalf("after the park: state=%s started %v→%v, want still parked, never restarted", after.State, snap.LastStarted, after.LastStarted)
	}

	// 15:00: the gate tick clears the park, and it starts by itself; the
	// clear is a harness_hold_changed too, with no reason left.
	r.clock.Set(until)
	r.gate()
	waitSnapshot(t, r.m, h.Name, "running after the reset", func(s Snapshot) bool { return s.State == core.StateRunning && s.Holds.Empty() })
	if logText := waitLogContains(t, r.env.logs, h.Name, " released reason=quota"); !strings.Contains(logText, "reason=quota") {
		t.Errorf("no release line for quota:\n%s", logText)
	}
	waitFor(t, 3*time.Second, "harness_hold_changed for the clear", func() bool {
		for {
			select {
			case ev := <-events:
				if ev.Kind == EventHoldChanged && ev.Name == h.Name && ev.Holds.Empty() {
					return true
				}
			default:
				return false
			}
		}
	})
}

// SPEC-0021 REQ-13 Scenario "A group parks together": three harnesses with
// quota_group = "claude-max"; when one is parked until 15:00, all three are
// parked until 15:00, each naming the member that triggered it.
func TestAGroupParksTogether(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	until := time.Date(2026, 10, 4, 15, 0, 0, 0, time.UTC)
	var hs []core.Harness
	for _, name := range []string{"implement", "validate", "review"} {
		h := shHarness(name, loopScript, 0)
		h.Restart = core.RestartAlways
		h.Budget.QuotaGroup = "claude-max"
		hs = append(hs, h)
	}
	r := newQuotaRig(t, managerCfg(hs...), fastPolicy(), now)
	for _, h := range hs {
		r.m.Start(h.Name)
		waitSnapshot(t, r.m, h.Name, h.Name+" running", func(s Snapshot) bool { return s.State == core.StateRunning })
	}

	r.m.QuotaObserve("validate", "claude-code", time.Now(), false,
		"rate_limit (429): You've hit your session limit · resets 3pm (UTC)")
	for _, h := range hs {
		waitSnapshot(t, r.m, h.Name, h.Name+" parked", parkedSnap)
		p, ok := r.m.ParkOf(h.Name)
		if !ok || !p.Until.Equal(until) || p.Group != "claude-max" || p.By != "validate" {
			t.Fatalf("%s: park = %+v ok=%v, want until 15:00 on claude-max by validate", h.Name, p, ok)
		}
	}
	for _, name := range []string{"implement", "review"} {
		if logText := waitLogContains(t, r.env.logs, name, " parked reason=quota"); !strings.Contains(logText, "by=validate") || !strings.Contains(logText, "quota_group=claude-max") {
			t.Errorf("%s's park line does not name the trigger:\n%s", name, logText)
		}
	}
	rec, ok := readParks(t, r.env.state)["group:claude-max"]
	if !ok || !rec.Until.Equal(until) || rec.By != "validate" || len(rec.Members) != 3 {
		t.Fatalf("state.json group park = %+v ok=%v, want until 15:00 by validate over three members", rec, ok)
	}

	r.clock.Set(until)
	r.gate()
	for _, h := range hs {
		waitSnapshot(t, r.m, h.Name, h.Name+" running after the reset", func(s Snapshot) bool { return s.State == core.StateRunning })
	}
}

// SPEC-0021 REQ-13 Scenario "A restart during a park": parked until 15:00,
// the daemon restarts at 14:00; the harness boots parked until 15:00, starts
// no process before then, and starts at 15:00.
func TestARestartDuringAPark(t *testing.T) {
	at := time.Date(2026, 10, 4, 13, 50, 0, 0, time.UTC)
	until := time.Date(2026, 10, 4, 15, 0, 0, 0, time.UTC)
	marker := filepath.Join(t.TempDir(), "spawns")
	h := shHarness("crush-sb", "echo ran >> '"+marker+"'; "+loopScript, 0)
	h.Restart = core.RestartAlways
	cfg := managerCfg(h)
	r := newQuotaRig(t, cfg, fastPolicy(), at)
	r.m.Start(h.Name)
	waitSnapshot(t, r.m, h.Name, "running", func(s Snapshot) bool { return s.State == core.StateRunning })
	r.m.QuotaObserve(h.Name, "claude-code", time.Now(), false, "Claude AI usage limit reached|"+fmt.Sprint(until.Unix()))
	waitSnapshot(t, r.m, h.Name, "parked", parkedSnap)
	before := spawns(t, marker)

	// The daemon goes down and comes back at 14:00.
	r.closer()
	r.clock.Set(time.Date(2026, 10, 4, 14, 0, 0, 0, time.UTC))
	r.boot(t, cfg, fastPolicy())
	r.m.Autostart()
	snap := waitSnapshot(t, r.m, h.Name, "booted parked", parkedSnap)
	if !snap.Enabled {
		t.Fatal("booted parked but no longer enabled")
	}
	if p, ok := r.m.ParkOf(h.Name); !ok || !p.Until.Equal(until) {
		t.Fatalf("park after the restart = %+v ok=%v, want until 15:00", p, ok)
	}
	r.gate() // a tick before the reset releases nothing
	time.Sleep(100 * time.Millisecond)
	if n := spawns(t, marker); n != before {
		t.Fatalf("%d process starts during the park after the restart, want none", n-before)
	}

	r.clock.Set(until)
	r.gate()
	waitSnapshot(t, r.m, h.Name, "running at 15:00", func(s Snapshot) bool { return s.State == core.StateRunning })
}

// SPEC-0021 REQ-13 Scenario "A malformed park in state.json": one harness's
// park record does not decode; that harness boots unparked with the error
// logged, and every other harness's park and state load normally.
func TestAMalformedParkInStateJSON(t *testing.T) {
	at := time.Date(2026, 10, 4, 14, 0, 0, 0, time.UTC)
	a, b := shHarness("a", loopScript, 0), shHarness("b", loopScript, 0)
	a.Restart, b.Restart = core.RestartAlways, core.RestartAlways
	env := newRunsEnv(t)
	doc := map[string]any{
		"version": 1,
		"harnesses": map[string]any{
			"a": map[string]any{"enabled": true, "state": "running", "restart_count": 7, "last_exit_code": 3, "flapping": false},
			"b": map[string]any{"enabled": true, "state": "running", "restart_count": 2, "last_exit_code": 0, "flapping": false},
		},
		"parks": map[string]any{
			"harness:a": map[string]any{"until": "half past never", "rule": "common/rate limit"},
			"harness:b": map[string]any{"until": "2026-10-04T15:00:00Z", "rule": "common/rate limit", "step": 0, "by": "b"},
		},
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(env.state), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env.state, data, 0o600); err != nil {
		t.Fatal(err)
	}
	logs := captureLog(t) // admission_test.go
	r := &quotaRig{clock: &fakeNow{t: at}, env: env}
	r.boot(t, managerCfg(a, b), fastPolicy())
	r.m.Autostart()

	waitSnapshot(t, r.m, "a", "a boots unparked and starts", func(s Snapshot) bool { return s.State == core.StateRunning && s.Holds.Empty() })
	if _, ok := r.m.ParkOf("a"); ok {
		t.Fatal("a's malformed park was restored")
	}
	if snap := r.snap(t, "a"); snap.RestartCount != 7 {
		t.Errorf("a's other state did not load: restart count %d, want 7", snap.RestartCount)
	}
	waitSnapshot(t, r.m, "b", "b boots parked", parkedSnap)
	if p, ok := r.m.ParkOf("b"); !ok || p.Until != time.Date(2026, 10, 4, 15, 0, 0, 0, time.UTC) {
		t.Fatalf("b's park = %+v ok=%v, want until 15:00", p, ok)
	}
	if out := logs.String(); !strings.Contains(out, "quota park does not decode") || !strings.Contains(out, "harness:a") {
		t.Errorf("the malformed park was not logged:\n%s", out)
	}
}

// The park is stored with no error text (ADR-0008; SPEC-0021 REQ-13): the
// fixture's message appears nowhere in state.json, only the rule's name.
func TestNoErrorTextInStateJSON(t *testing.T) {
	at := time.Date(2026, 10, 4, 0, 5, 0, 0, time.UTC)
	env := newRunsEnv(t)
	r := &quotaRig{clock: &fakeNow{t: at}, env: env, marks: filepath.Join(env.dir, "marks")}
	h := oneShot(r.agent("review", tarsNote, 1))
	r.boot(t, managerCfg(h), fastPolicy())
	r.m.StartRun(h.Name, RunRequest{Trigger: TriggerSchedule})
	waitSnapshot(t, r.m, h.Name, "parked", parkedSnap)
	data, err := os.ReadFile(env.state)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{tarsNote, "Payment Required", "out of credits", "hyper.charm.land"} {
		if bytes.Contains(data, []byte(text)) {
			t.Errorf("state.json carries error text %q:\n%s", text, data)
		}
	}
	if !bytes.Contains(data, []byte(`"rule": "common/payment required"`)) {
		t.Errorf("state.json does not name the rule:\n%s", data)
	}
}

// oneShot is the tars review harness's shape: a scheduled one-shot with
// restart = "no" and catch_up, here running script.
func oneShot(script string) core.Harness {
	h := shHarness("review", script, 0)
	h.Schedule = "5-59/10 * * * *"
	h.Restart = core.RestartNo
	h.CatchUp = true
	h.KeepRuns = 100
	return h
}

// The one-shot half of REQ-13, on the 2026-10-04 shape: a run that exits 1 on
// crush's 402 reads quota_parked and leaves the harness stopped (never
// failed); later firings are skipped quota_parked, coalesced, with no
// process; and when the park clears, catch_up gets exactly one run.
func TestAOneShotParksAndCatchesUpOnce(t *testing.T) {
	at := time.Date(2026, 10, 4, 0, 5, 0, 0, time.UTC)
	env := newRunsEnv(t)
	r := &quotaRig{clock: &fakeNow{t: at}, env: env, marks: filepath.Join(env.dir, "marks")}
	marker := filepath.Join(env.dir, "spawns")
	h := oneShot("echo ran >> '" + marker + "'; " + r.agent("review", tarsNote, 1))
	r.boot(t, managerCfg(h), fastPolicy())

	r.m.StartRun(h.Name, RunRequest{Trigger: TriggerSchedule})
	snap := waitSnapshot(t, r.m, h.Name, "parked", parkedSnap)
	if snap.State == core.StateFailed {
		t.Fatal("the run's harness reached failed")
	}
	runs := waitRuns(t, r.m, h.Name, "the run closed", func(rs []RunRecord) bool {
		return len(rs) == 1 && rs[0].Outcome != OutcomeRunning
	})
	if runs[0].Outcome != OutcomeQuotaParked || runs[0].ExitCode == nil || *runs[0].ExitCode != 1 {
		t.Fatalf("run = %+v, want quota_parked with exit 1", runs[0])
	}
	p, ok := r.m.ParkOf(h.Name)
	if !ok || !p.Until.Equal(at.Add(15*time.Minute)) || p.Rule != "common/payment required" {
		t.Fatalf("park = %+v ok=%v, want quota_backoff's 15m by common/payment required", p, ok)
	}

	// Two firings during the park: skipped quota_parked, one coalesced
	// record, no process.
	r.clock.Set(at.Add(10 * time.Minute))
	for _, trig := range []RunTrigger{TriggerChannel, TriggerChannel} {
		if d, _ := r.m.StartRun(h.Name, RunRequest{Trigger: trig, Source: "channel.switchboard"}); d.Kind != DecisionSkipped {
			t.Fatalf("a firing during the park was %s, want skipped", d.Kind)
		}
	}
	runs = waitRuns(t, r.m, h.Name, "a coalesced skip", func(rs []RunRecord) bool {
		return len(rs) == 2 && rs[1].Coalesced == 2
	})
	if runs[1].Outcome != OutcomeSkipped || runs[1].Reason != ReasonQuotaParked {
		t.Fatalf("skip record = %+v, want skipped/quota_parked", runs[1])
	}
	if n := spawns(t, marker); n != 1 {
		t.Fatalf("%d processes, want only the first run's", n)
	}

	// 00:20: the park clears; catch_up runs once (and, still out of
	// credits, parks again for twice as long).
	r.clock.Set(p.Until)
	r.gate()
	waitRuns(t, r.m, h.Name, "one catch_up run", func(rs []RunRecord) bool {
		return len(rs) == 3 && rs[2].Trigger == TriggerCatchUp && rs[2].Outcome == OutcomeQuotaParked
	})
	if n := spawns(t, marker); n != 2 {
		t.Fatalf("%d processes after the release, want 2", n)
	}
	if p2, ok := r.m.ParkOf(h.Name); !ok || p2.Until.Sub(p.Until) != 30*time.Minute || p2.Step != 1 {
		t.Fatalf("second park = %+v ok=%v, want 30m at step 1", p2, ok)
	}
}

// SPEC-0021 REQ-11 Scenario "The phrase in a successful run": a run that
// exits 0 never parks, whatever its transcript says; the same transcript
// with a non-zero exit does.
func TestThePhraseInASuccessfulRun(t *testing.T) {
	for _, tc := range []struct {
		code int
		want RunOutcome
	}{{0, OutcomeSuccess}, {1, OutcomeQuotaParked}} {
		t.Run(fmt.Sprint("exit ", tc.code), func(t *testing.T) {
			at := time.Date(2026, 10, 4, 0, 5, 0, 0, time.UTC)
			env := newRunsEnv(t)
			r := &quotaRig{clock: &fakeNow{t: at}, env: env, marks: filepath.Join(env.dir, "marks")}
			h := oneShot(r.agent("review", "Claude AI usage limit reached|"+fmt.Sprint(at.Add(time.Hour).Unix()), tc.code))
			r.boot(t, managerCfg(h), fastPolicy())
			r.m.StartRun(h.Name, RunRequest{Trigger: TriggerSchedule})
			runs := waitRuns(t, r.m, h.Name, "the run closed", func(rs []RunRecord) bool {
				return len(rs) == 1 && rs[0].Outcome != OutcomeRunning
			})
			if runs[0].Outcome != tc.want {
				t.Fatalf("outcome = %s, want %s", runs[0].Outcome, tc.want)
			}
			_, parked := r.m.ParkOf(h.Name)
			if parked != (tc.code != 0) {
				t.Fatalf("parked = %v after exit %d", parked, tc.code)
			}
			want := 1
			if tc.code == 0 {
				want = 0 // a zero exit never asks
			}
			if got := r.syncCount(); got != want {
				t.Errorf("the exit path synced %d times, want %d", got, want)
			}
		})
	}
}

// A one-shot that fails for a reason that is not quota fails, as before: the
// detector parks on quota only (REQ-12).
func TestANonQuotaFailureStillFails(t *testing.T) {
	at := time.Date(2026, 10, 4, 0, 5, 0, 0, time.UTC)
	env := newRunsEnv(t)
	r := &quotaRig{clock: &fakeNow{t: at}, env: env, marks: filepath.Join(env.dir, "marks")}
	h := oneShot(r.agent("review", "401 Unauthorized: invalid api key", 1))
	r.boot(t, managerCfg(h), fastPolicy())
	r.m.StartRun(h.Name, RunRequest{Trigger: TriggerSchedule})
	waitSnapshot(t, r.m, h.Name, "failed", func(s Snapshot) bool { return s.State == core.StateFailed })
	if _, ok := r.m.ParkOf(h.Name); ok {
		t.Fatal("parked on an auth error")
	}
}

// readParks decodes state.json's parks.
func readParks(t *testing.T, path string) map[string]parkRecord {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var ps persistedState
	if err := json.Unmarshal(data, &ps); err != nil {
		t.Fatal(err)
	}
	out := map[string]parkRecord{}
	if len(ps.Parks) > 0 {
		if err := json.Unmarshal(ps.Parks, &out); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// A one-shot parked across a daemon restart: it boots parked, a firing
// skipped before the restart still earns its catch_up, and that one run
// starts when the park clears (SPEC-0021 REQ-13 Scenario "A restart during a
// park", one-shot half).
func TestARestartDuringAOneShotParkCatchesUpOnce(t *testing.T) {
	at := time.Date(2026, 10, 4, 0, 5, 0, 0, time.UTC)
	env := newRunsEnv(t)
	r := &quotaRig{clock: &fakeNow{t: at}, env: env, marks: filepath.Join(env.dir, "marks")}
	marker := filepath.Join(env.dir, "spawns")
	h := oneShot("echo ran >> '" + marker + "'; " + r.agent("review", tarsNote, 1))
	cfg := managerCfg(h)
	r.boot(t, cfg, fastPolicy())
	r.m.StartRun(h.Name, RunRequest{Trigger: TriggerSchedule})
	waitSnapshot(t, r.m, h.Name, "parked", parkedSnap)
	r.m.StartRun(h.Name, RunRequest{Trigger: TriggerSchedule})
	waitRuns(t, r.m, h.Name, "a skipped firing", func(rs []RunRecord) bool { return len(rs) == 2 && rs[1].Reason == ReasonQuotaParked })

	r.closer()
	r.boot(t, cfg, fastPolicy())
	waitSnapshot(t, r.m, h.Name, "booted parked", parkedSnap)
	p, ok := r.m.ParkOf(h.Name)
	if !ok {
		t.Fatal("no park after the restart")
	}
	r.clock.Set(p.Until)
	r.gate()
	runs := waitRuns(t, r.m, h.Name, "one catch_up run", func(rs []RunRecord) bool {
		return len(rs) == 3 && rs[2].Outcome != OutcomeRunning
	})
	if runs[2].Trigger != TriggerCatchUp {
		t.Fatalf("after the restart and the reset: %s run, want catch_up", runs[2].Trigger)
	}
	if n := spawns(t, marker); n != 2 {
		t.Fatalf("%d processes, want 2 (the first run and the catch_up)", n)
	}
}
