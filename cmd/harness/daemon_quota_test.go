package main

// Daemon Quota Parking
//
// SPEC-0021's park through the daemon's own wiring: the Manager
// daemonManagerOptions builds (its real restart Policy), the observer
// startDaemonObserver starts, the quota feed startDaemonQuota subscribes to
// it, and the gate pass startDaemonScheduler runs on a fake clock. The agent
// is a stand-in: this test binary, re-entered through TestMain, run as
// `crush` or `claude` from PATH, which writes its model outcomes to the
// session store the real tool would (a crush.db, a Claude Code transcript)
// and exits at once, the way crush does two seconds after a 402.
//
// The observer polls once an hour here. So an error mark reaches the park
// detector before the exit decides only if the exit path's own sync reads
// it: these tests are the proof that the race between a run's exit and the
// observer's poll is closed, deterministically, not narrowed.
//
// TestDaemonQuotaTheTarsShape is the 2026-10-04 incident as an acceptance
// test: the review one-shot on tars, out of credits, failed every ten minutes
// for ten hours. Now it parks after the first run, later firings are skipped
// without spawning crush, and it releases by itself.
//
// Governing: ADR-0027; SPEC-0021 REQ-11, REQ-12, REQ-13, REQ-14; issue #315
// (test the daemon's wiring, not a copy of it).
//
// @joestump 10/04/2026 - Added for stump.wtf/harness#477.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/config"
	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/observe"
	rt "github.com/stump-wtf/harness/internal/runtrace/runtracetest"
	"github.com/stump-wtf/harness/internal/supervisor"
)

// standInAgentEnv, set in a child's environment (through its harness's
// env_file, so no other test's child sees it), turns this test binary into a
// stand-in agent: it reads what to do from the plan file the variable names,
// writes that outcome to its session store, and exits.
const standInAgentEnv = "HARNESS_TEST_STANDIN_AGENT"

// The 2026-10-04 provider answer, split as crush stores it in a finish part.
const (
	tarsMessage = "Payment Required"
	tarsDetails = "You're out of credits. Add more at https://hyper.charm.land"
)

// runStandInAgent is the stand-in agent's whole life. The plan file holds one
// line: "crush-402" (out of credits), "crush-429" (rate limited, no reset),
// "crush-ok" (a tool call, exit 0), or "claude-limit <epoch>" (a Claude Code
// usage limit naming its reset). Each run appends a line to <plan>.runs.
func runStandInAgent(plan string) int {
	b, err := os.ReadFile(plan)
	if err != nil {
		fmt.Fprintln(os.Stderr, "stand-in agent:", err)
		return 2
	}
	f, err := os.OpenFile(plan+".runs", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 2
	}
	_, _ = fmt.Fprintln(f, os.Getpid())
	_ = f.Close()
	cwd, err := os.Getwd()
	if err != nil {
		return 2
	}
	now := time.Now()
	id := fmt.Sprintf("run-%d", os.Getpid())
	user := rt.CrushMessage{Role: "user", At: now, Parts: `[{"type":"text","data":{"text":"review the open PRs"}}]`}
	db := filepath.Join(cwd, ".crush", "crush.db")
	fields := strings.Fields(string(b))
	switch fields[0] {
	case "crush-402", "crush-429":
		finish := rt.FinishError(tarsMessage, tarsDetails)
		if fields[0] == "crush-429" {
			finish = rt.FinishError("Too Many Requests", "429: rate limit exceeded")
		}
		if err := rt.WriteCrushStore(db, rt.CrushSession{ID: id, Created: now, Updated: now, Messages: []rt.CrushMessage{
			user, {Role: "assistant", At: now, Parts: finish},
		}}); err != nil {
			fmt.Fprintln(os.Stderr, "stand-in agent:", err)
			return 2
		}
		fmt.Println("ERROR " + tarsMessage + ": " + tarsDetails)
		return 1 // at once: no sleep between the store write and the exit
	case "crush-ok":
		msgs := []rt.CrushMessage{user,
			{Role: "assistant", At: now, Parts: rt.ToolCall("c1", "view", map[string]any{"file_path": "README.md"})},
			{Role: "tool", At: now, Parts: rt.ToolResult("c1", "contents")},
		}
		if err := rt.WriteCrushStore(db, rt.CrushSession{ID: id, Created: now, Updated: now, Messages: msgs}); err != nil {
			return 2
		}
		return 0
	case "claude-limit":
		if err := writeClaudeLimit(cwd, id, now, fields[1]); err != nil {
			fmt.Fprintln(os.Stderr, "stand-in agent:", err)
			return 2
		}
		note := "Claude AI usage limit reached|" + fields[1]
		fmt.Printf(`{"type":"result","subtype":"success","is_error":true,"result":%q}`+"\n", note)
		return 1
	}
	return 2
}

// writeClaudeLimit writes a Claude Code transcript the way `claude -p` does
// when the account hits its usage limit: the prompt, then the API-error
// record agent-trace v0.7.1 turns into an error mark (agent-trace#104).
func writeClaudeLimit(cwd, id string, now time.Time, epoch string) error {
	dir := filepath.Join(os.Getenv("HOME"), ".claude", "projects", strings.ReplaceAll(cwd, string(filepath.Separator), "-"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	stamp := func(d time.Duration) string { return now.Add(d).UTC().Format("2006-01-02T15:04:05.000Z") }
	lines := []map[string]any{
		{"parentUuid": nil, "isSidechain": false, "userType": "external", "cwd": cwd, "sessionId": id, "version": "2.0.0",
			"type": "user", "uuid": "u1", "timestamp": stamp(0), "message": map[string]any{"role": "user", "content": "review the open PRs"}},
		{"parentUuid": "u1", "isSidechain": false, "userType": "external", "cwd": cwd, "sessionId": id, "version": "2.0.0",
			"type": "assistant", "uuid": "a1", "timestamp": stamp(time.Millisecond),
			"message": map[string]any{"id": "msg_a1", "type": "message", "role": "assistant", "model": "<synthetic>",
				"content":     []map[string]any{{"type": "text", "text": "Claude AI usage limit reached|" + epoch}},
				"stop_reason": "stop_sequence", "stop_sequence": "", "usage": map[string]any{"input_tokens": 0, "output_tokens": 0}},
			"error": "rate_limit", "apiErrorStatus": 429, "isApiErrorMessage": true},
	}
	var out []byte
	for _, l := range lines {
		b, err := json.Marshal(l)
		if err != nil {
			return err
		}
		out = append(append(out, b...), '\n')
	}
	return os.WriteFile(filepath.Join(dir, id+".jsonl"), out, 0o600)
}

// standIn puts this test binary first on PATH as tool ("crush", "claude"),
// with the plan its runs follow, and returns the plan's path and the env
// file that hands the plan to the child. TERM=dumb keeps a PTY child from
// waiting on terminal queries nobody answers.
func standIn(t *testing.T, tmp, tool, plan string) (planPath, envFile string) {
	t.Helper()
	bin := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(exe, filepath.Join(bin, tool)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	planPath = filepath.Join(tmp, "plan")
	setPlan(t, planPath, plan)
	envFile = filepath.Join(tmp, "agent.env")
	if err := os.WriteFile(envFile, []byte(standInAgentEnv+"="+planPath+"\nTERM=dumb\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return planPath, envFile
}

func setPlan(t *testing.T, path, plan string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(plan+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// agentRuns counts the stand-in's runs.
func agentRuns(t *testing.T, plan string) int {
	t.Helper()
	b, err := os.ReadFile(plan + ".runs")
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "\n")
}

// quotaDaemon is the daemon's wiring over cfg on a fake clock: its Manager
// (restored), observer (polling once an hour), quota feed and seam, and,
// when withScheduler, its scheduler and gate pass on the same clock.
type quotaDaemon struct {
	*budgetRig
	obs *observe.Observer
}

func newQuotaDaemon(t *testing.T, cfg *core.Config, at time.Time, withScheduler bool) *quotaDaemon {
	t.Helper()
	r := newBudgetRig(t, cfg, at)
	obsOpts := daemonObserverOptions()
	obsOpts.PollInterval = time.Hour // only an exit's own sync reads a store
	obs := startDaemonObserver(r.mgr, obsOpts)
	t.Cleanup(obs.Stop)
	qf := startDaemonQuota(obs, r.mgr)
	t.Cleanup(func() { stopDaemonQuota(qf, r.mgr) })
	if withScheduler {
		sched := startDaemonScheduler(r.mgr, cfg, r.clock)
		t.Cleanup(sched.Close)
	}
	return &quotaDaemon{budgetRig: r, obs: obs}
}

// hermeticHome points every store discovery reads at tmp.
func hermeticHome(t *testing.T, tmp string) {
	t.Helper()
	t.Setenv("HOME", tmp)
	t.Setenv("CRUSH_GLOBAL_DATA", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
}

// tarsConfig is the review harness on tars, as its harness.toml had it, in a
// workdir under tmp.
func tarsConfig(t *testing.T, tmp, envFile string) *core.Config {
	t.Helper()
	work := filepath.Join(tmp, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]byte(fmt.Sprintf(`[harness.review]
harness = "crush"
prompt = "review the open PRs"
model = "hyper/deepseek-v4.1-flash"
schedule = "CRON_TZ=UTC 5-59/10 * * * *"
restart = "no"
catch_up = true
keep_runs = 100
workdir = %q
env_file = %q
`, work, envFile)), filepath.Join(tmp, "harness.toml"))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func waitRunsWhere(t *testing.T, mgr *supervisor.Manager, name, desc string, pred func([]supervisor.RunRecord) bool) []supervisor.RunRecord {
	t.Helper()
	var rs []supervisor.RunRecord
	waitUntil(t, desc, func() bool {
		rs = mgr.Runs(name)
		return pred(rs)
	})
	return rs
}

func parked(s supervisor.Snapshot) bool {
	return s.State == core.StateStopped && s.Holds.Has(core.HoldQuota)
}

// TestDaemonQuotaTheTarsShape is the 2026-10-04 incident end to end: the
// scheduled crush review (`CRON_TZ=UTC 5-59/10 * * * *`, restart = "no",
// catch_up) whose provider account ran out of credits. Its first run writes
// crush's 402 finish part to the session store and exits 1 about as soon as it
// starts. The harness parks after that run (outcome quota_parked, state
// stopped, never failed) for quota_backoff's default 15m; the 00:15 firing and
// two channel firings are recorded skipped quota_parked, the two channel ones
// coalesced, and crush is never spawned for them; at 00:20 the gate releases
// the park by itself and catch_up gets exactly one run (credits topped up by
// then, so it succeeds), and the 00:25 firing runs as normal.
func TestDaemonQuotaTheTarsShape(t *testing.T) {
	tmp := t.TempDir()
	hermeticHome(t, tmp)
	plan, envFile := standIn(t, tmp, "crush", "crush-402")
	cfg := tarsConfig(t, tmp, envFile)
	d := newQuotaDaemon(t, cfg, time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC), true)
	const name = "review"

	// 00:05: the schedule fires; crush runs out of credits.
	d.tick(time.Date(2026, 10, 4, 0, 5, 0, 0, time.UTC))
	rs := waitRunsWhere(t, d.mgr, name, "the first run closed", func(rs []supervisor.RunRecord) bool {
		return len(rs) == 1 && rs[0].Outcome != supervisor.OutcomeRunning
	})
	if rs[0].Outcome != supervisor.OutcomeQuotaParked || rs[0].Trigger != supervisor.TriggerSchedule {
		t.Fatalf("first run = %s/%s, want a schedule run ending quota_parked", rs[0].Trigger, rs[0].Outcome)
	}
	snap := d.wait(t, name, "parked", parked)
	if snap.State == core.StateFailed {
		t.Fatal("the review harness reached failed")
	}
	p, ok := d.mgr.ParkOf(name)
	wantUntil := time.Date(2026, 10, 4, 0, 20, 0, 0, time.UTC)
	if !ok || !p.Until.Equal(wantUntil) || p.Rule != "common/payment required" {
		t.Fatalf("park = %+v ok=%v, want until 00:20 (quota_backoff 15m) by common/payment required", p, ok)
	}

	// 00:15: the next firing, and two channel-triggered ones: skipped
	// quota_parked, without crush.
	d.tick(time.Date(2026, 10, 4, 0, 15, 0, 0, time.UTC))
	waitRunsWhere(t, d.mgr, name, "the 00:15 firing recorded", func(rs []supervisor.RunRecord) bool { return len(rs) == 2 })
	for range 2 {
		d.mgr.StartRun(name, supervisor.RunRequest{Trigger: supervisor.TriggerChannel, Source: "channel.switchboard"})
	}
	rs = waitRunsWhere(t, d.mgr, name, "the firings recorded skipped", func(rs []supervisor.RunRecord) bool {
		return len(rs) == 3 && rs[2].Coalesced == 2
	})
	for _, r := range rs[1:] {
		if r.Outcome != supervisor.OutcomeSkipped || r.Reason != supervisor.ReasonQuotaParked {
			t.Fatalf("a firing during the park = %s/%s, want skipped/quota_parked", r.Outcome, r.Reason)
		}
	}
	if rs[1].Trigger != supervisor.TriggerSchedule || rs[2].Trigger != supervisor.TriggerChannel {
		t.Fatalf("skips = %s, %s; want the 00:15 schedule firing, then the channel ones coalesced", rs[1].Trigger, rs[2].Trigger)
	}
	if n := agentRuns(t, plan); n != 1 {
		t.Fatalf("crush ran %d times, want once: firings during a park spawn nothing", n)
	}

	// The operator tops up the credits. 00:20: the park expires and the gate
	// releases it; catch_up gets one run.
	setPlan(t, plan, "crush-ok")
	d.tickUntil(t, wantUntil, name, "released", func(s supervisor.Snapshot) bool { return s.Holds.Empty() })
	rs = waitRunsWhere(t, d.mgr, name, "one catch_up run", func(rs []supervisor.RunRecord) bool {
		return len(rs) == 4 && rs[3].Outcome != supervisor.OutcomeRunning
	})
	if rs[3].Trigger != supervisor.TriggerCatchUp || rs[3].Outcome != supervisor.OutcomeSuccess {
		t.Fatalf("after the release: %s/%s, want one catch_up run that succeeds", rs[3].Trigger, rs[3].Outcome)
	}
	if _, ok := d.mgr.ParkOf(name); ok {
		t.Fatal("still parked after the reset")
	}

	// 00:25: the schedule runs as normal.
	d.tick(time.Date(2026, 10, 4, 0, 25, 0, 0, time.UTC))
	rs = waitRunsWhere(t, d.mgr, name, "the 00:25 run", func(rs []supervisor.RunRecord) bool {
		return len(rs) == 5 && rs[4].Outcome != supervisor.OutcomeRunning
	})
	if rs[4].Trigger != supervisor.TriggerSchedule || rs[4].Outcome != supervisor.OutcomeSuccess {
		t.Fatalf("00:25: %s/%s, want a schedule run that succeeds", rs[4].Trigger, rs[4].Outcome)
	}
	if n := agentRuns(t, plan); n != 3 {
		t.Fatalf("crush ran %d times, want 3 (the 402, the catch_up, 00:25)", n)
	}
	if st := d.obs.Stats(); st.Dropped["budget"] != 0 {
		t.Errorf("the park detector's subscription dropped %d events", st.Dropped["budget"])
	}
}

// TestDaemonQuotaDecidesOnTheRunsOwnError is the race the park exists to win,
// alone: the agent writes its error to its store and exits immediately, the
// observer's next poll is an hour away, and the run must still read
// quota_parked, every time. Run with -race -count=20.
func TestDaemonQuotaDecidesOnTheRunsOwnError(t *testing.T) {
	tmp := t.TempDir()
	hermeticHome(t, tmp)
	_, envFile := standIn(t, tmp, "crush", "crush-402")
	d := newQuotaDaemon(t, tarsConfig(t, tmp, envFile), time.Date(2026, 10, 4, 0, 5, 0, 0, time.UTC), false)
	d.mgr.StartRun("review", supervisor.RunRequest{Trigger: supervisor.TriggerChannel})
	rs := waitRunsWhere(t, d.mgr, "review", "the run closed", func(rs []supervisor.RunRecord) bool {
		return len(rs) == 1 && rs[0].Outcome != supervisor.OutcomeRunning
	})
	if rs[0].Outcome != supervisor.OutcomeQuotaParked {
		t.Fatalf("outcome = %s, want quota_parked: the exit decided before the error mark arrived", rs[0].Outcome)
	}
	if snap := d.mustSnap(t, "review"); !parked(snap) {
		t.Fatalf("state=%s holds=%s, want stopped and held for quota", snap.State, snap.Holds)
	}
}

// A Claude Code one-shot on pipes (stream-json) that hits its usage limit
// parks until the reset its transcript names: the API-error record reaches the
// observer as an error mark through the exit's sync, so no run-log fallback
// is needed (SPEC-0021 REQ-11, as amended by stump.wtf/harness#477).
func TestDaemonQuotaClaudeCodeUsageLimitOnPipes(t *testing.T) {
	// Resolved: Claude Code records the cwd it was started in as the OS
	// reports it (/private/var on macOS, not the /var symlink), and the
	// transcript must name the workdir the harness is declared with.
	tmp, evalErr := filepath.EvalSymlinks(t.TempDir())
	if evalErr != nil {
		t.Fatal(evalErr)
	}
	hermeticHome(t, tmp)
	at := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	reset := at.Add(3 * time.Hour)
	_, envFile := standIn(t, tmp, "claude", fmt.Sprintf("claude-limit %d", reset.Unix()))
	work := filepath.Join(tmp, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]byte(fmt.Sprintf(`[harness.pr-review]
harness = "claude-code"
prompt = "review the open PRs"
schedule = "0 0 1 1 *"
restart = "no"
workdir = %q
env_file = %q
`, work, envFile)), filepath.Join(tmp, "harness.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !supervisor.RunsOnPipes(cfg.Harnesses["pr-review"]) {
		t.Fatal("a claude-code one-shot no longer runs on pipes; this test's premise is gone")
	}
	d := newQuotaDaemon(t, cfg, at, false)
	d.mgr.StartRun("pr-review", supervisor.RunRequest{Trigger: supervisor.TriggerManual})
	rs := waitRunsWhere(t, d.mgr, "pr-review", "the run closed", func(rs []supervisor.RunRecord) bool {
		return len(rs) == 1 && rs[0].Outcome != supervisor.OutcomeRunning
	})
	if rs[0].Outcome != supervisor.OutcomeQuotaParked {
		t.Fatalf("outcome = %s, want quota_parked; observer %+v", rs[0].Outcome, d.obs.Stats())
	}
	p, ok := d.mgr.ParkOf("pr-review")
	if !ok || !p.Until.Equal(reset) || p.Rule != "claude-code/usage limit reached" {
		t.Fatalf("park = %+v ok=%v, want until %v by claude-code/usage limit reached", p, ok, reset)
	}
}

// The 2026-09-19 shape through the daemon's wiring: a resident crush whose
// provider rate-limits every call, with no reset time, exits after each
// refusal. On the daemon's Policy (MaxRestarts = 5) it parks on its third
// exit, is released by the gate at the reset and starts by itself, parks
// again, and never reaches failed across nine exits.
func TestDaemonQuotaTheResidentShape(t *testing.T) {
	tmp := t.TempDir()
	hermeticHome(t, tmp)
	plan, envFile := standIn(t, tmp, "crush", "crush-429")
	work := filepath.Join(tmp, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]byte(fmt.Sprintf(`[harness.crush-sb]
harness = "crush"
restart = "always"
quota_backoff = "1m"
workdir = %q
env_file = %q
`, work, envFile)), filepath.Join(tmp, "harness.toml"))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 19, 9, 0, 0, 0, time.UTC)
	d := newQuotaDaemon(t, cfg, at, true)
	if d.policy.MaxRestarts != 5 {
		t.Fatalf("the daemon's MaxRestarts is %d; the scenario is written against 5", d.policy.MaxRestarts)
	}
	const name = "crush-sb"
	d.mgr.Start(name)
	parkedOrFailed := func(s supervisor.Snapshot) bool { return parked(s) || s.State == core.StateFailed }
	for cycle := range 3 {
		snap := d.wait(t, name, fmt.Sprintf("parked (cycle %d)", cycle), parkedOrFailed)
		if snap.State == core.StateFailed {
			t.Fatalf("cycle %d: gave up into failed after %d runs", cycle, agentRuns(t, plan))
		}
		if n := agentRuns(t, plan); n != 3*(cycle+1) {
			t.Fatalf("cycle %d: parked after %d runs, want %d (three refusals per park)", cycle, n, 3*(cycle+1))
		}
		p, ok := d.mgr.ParkOf(name)
		if !ok {
			t.Fatalf("cycle %d: held for quota with no park in force", cycle)
		}
		if cycle == 2 {
			break
		}
		started := snap.LastStarted
		d.tickUntil(t, p.Until, name, "started again at the reset", func(s supervisor.Snapshot) bool {
			return s.LastStarted.After(started)
		})
	}
	if snap := d.mustSnap(t, name); snap.State == core.StateFailed || !snap.Enabled {
		t.Fatalf("after nine refusals: state=%s enabled=%v, want parked and enabled", snap.State, snap.Enabled)
	}
}
