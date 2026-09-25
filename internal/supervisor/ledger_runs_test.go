package supervisor

// SPEC-0022 acceptance for the Manager's RunJournal (#444). Where a claim is
// about the ledger on disk these tests read the day files, or have the run
// itself read them, never the Manager's memory: an index that remembered a line
// the writer never wrote would pass every in-memory check.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/ledger"
)

func (e runsEnv) ledgerDir() string { return filepath.Join(e.dir, "ledger") }

// ledgerText is every day file's contents, concatenated.
func ledgerText(t *testing.T, e runsEnv) string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(e.ledgerDir(), "*.jsonl"))
	var b strings.Builder
	for _, f := range files {
		b.WriteString(readText(t, f))
	}
	return b.String()
}

// ledgerLines parses every line of every day file.
func ledgerLines(t *testing.T, e runsEnv) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, ln := range strings.Split(ledgerText(t, e), "\n") {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			t.Fatalf("unparseable ledger line %q: %v", ln, err)
		}
		out = append(out, m)
	}
	return out
}

// REQ-6 "The opened line is on disk before the spawn". The run's own process
// reads the day file the moment it starts: if the opened line were still in a
// queue, or only in memory, it would find nothing.
func TestOpenedLineIsOnDiskBeforeTheSpawn(t *testing.T) {
	e := newRunsEnv(t)
	seen := filepath.Join(e.dir, "seen")
	script := fmt.Sprintf(`grep -h '"type":"opened"' %q/*.jsonl | grep -c '"harness":"probe","run_id":1,' > %q`, e.ledgerDir(), seen)
	m, _ := e.manager(t, sweepCfg(sweep("probe", script)), fastPolicy())

	m.StartRun("probe", RunRequest{Trigger: TriggerSchedule})
	waitRuns(t, m, "probe", "run finishes", outcomesAre(OutcomeSuccess))
	if got := strings.TrimSpace(readText(t, seen)); got != "1" {
		t.Errorf("the process found %q opened lines for its own run at spawn, want 1", got)
	}
}

// REQ-1 layout and REQ-2 fold through the journal: one run is an opened and a
// closed line on disk, in seq order, folding to the record Runs reports.
func TestARunIsAnOpenedAndAClosedLine(t *testing.T) {
	e := newRunsEnv(t)
	m, _ := e.manager(t, sweepCfg(sweep("nightly", "exit 3")), fastPolicy())
	m.StartRun("nightly", RunRequest{Trigger: TriggerSchedule})
	waitRuns(t, m, "nightly", "run fails", outcomesAre(OutcomeFailed))

	st, err := os.Stat(e.ledgerDir())
	if err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("ledger dir: %v, mode %v; want 0700", err, st.Mode().Perm())
	}
	lines := ledgerLines(t, e)
	if len(lines) != 2 {
		t.Fatalf("ledger holds %d lines, want opened + closed: %v", len(lines), lines)
	}
	o, c := lines[0], lines[1]
	if o["type"] != "opened" || o["kind"] != "oneshot" || o["trigger"] != "schedule" || o["outcome"] != "running" || o["log"] != m.RunLogPath("nightly", 1) {
		t.Errorf("opened line = %v", o)
	}
	if c["type"] != "closed" || c["outcome"] != "failed" || c["exit_code"].(float64) != 3 || c["ended_at"] == nil || c["seq"].(float64) != o["seq"].(float64)+1 {
		t.Errorf("closed line = %v", c)
	}
}

// REQ-7 "A daemon killed mid-run": the ledger a SIGKILLed daemon leaves is an
// opened line and no closed one (internal/ledger's crash test kills a real
// process to show that is what survives). The next boot closes it interrupted,
// daemon_crash, with no end; writes that to the run's log; and the next run is
// +1, with no state.json and no log left to floor it: only the ledger.
func TestBootReconcilesARunADeadDaemonLeftOpen(t *testing.T) {
	e := newRunsEnv(t)
	logPath := filepath.Join(e.jobs, "nightly", "4.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("partial output\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	started := time.Now().Add(-time.Minute).UTC()
	l, err := ledger.Open(e.ledgerDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Append(ledger.Line{Type: ledger.TypeOpened, At: started, Harness: "nightly", RunID: 4, Record: ledger.Record{
		Kind: ledger.KindOneshot, Trigger: "schedule", Outcome: "running", StartedAt: &started, Log: logPath,
	}}, true); err != nil {
		t.Fatal(err)
	}
	if err := l.Import(nil); err != nil { // an upgraded daemon: the import has run
		t.Fatal(err)
	}
	if err := l.Close(time.Second); err != nil {
		t.Fatal(err)
	}

	m, _ := e.manager(t, sweepCfg(sweep("nightly", "exit 0")), fastPolicy())
	r, ok := m.Run("nightly", 4)
	if !ok || r.Outcome != OutcomeInterrupted || r.Reason != ReasonDaemonCrash || r.EndedAt != nil {
		t.Fatalf("run 4 = %+v, want interrupted, daemon_crash, no end", r)
	}
	lines := ledgerLines(t, e)
	last := lines[len(lines)-1]
	if last["type"] != "closed" || last["reason"] != "daemon_crash" || last["ended_at"] != nil {
		t.Errorf("reconciliation line on disk = %v", last)
	}
	if log := readText(t, logPath); !strings.Contains(log, "partial output") || !strings.Contains(log, "run interrupted") {
		t.Errorf("run 4's log was not told:\n%s", log)
	}
	// The allocator floors itself lazily, at the first run. With the log gone
	// and no state.json, only the ledger can know run 4 existed.
	_ = os.Remove(logPath)
	m.StartRun("nightly", RunRequest{Trigger: TriggerSchedule})
	rs := waitRuns(t, m, "nightly", "next run", func(rs []RunRecord) bool {
		return len(rs) == 2 && rs[1].Outcome == OutcomeSuccess
	})
	if rs[1].RunID != 5 {
		t.Errorf("next run id = %d, want 5", rs[1].RunID)
	}
}

// REQ-7: a clean shutdown closes the run in flight interrupted, reason
// shutdown, with an end.
func TestShutdownClosesTheRunWithAReason(t *testing.T) {
	e := newRunsEnv(t)
	m, closeM := e.manager(t, sweepCfg(sweep("long", "sleep 30")), fastPolicy())
	m.StartRun("long", RunRequest{Trigger: TriggerSchedule})
	waitRuns(t, m, "long", "run in flight", outcomesAre(OutcomeRunning))
	closeM()

	lines := ledgerLines(t, e)
	last := lines[len(lines)-1]
	if last["type"] != "closed" || last["outcome"] != "interrupted" || last["reason"] != "shutdown" || last["ended_at"] == nil {
		t.Errorf("last line after shutdown = %v", last)
	}
}

// REQ-13 "Upgrading a daemon with history", "jobs reads the ledger" (the
// Runs view jobs is built from) and "Run ids continue".
func TestUpgradeImportsStateHistoryOnce(t *testing.T) {
	e := newRunsEnv(t)
	base := time.Now().Add(-30 * time.Hour).UTC().Truncate(time.Second)
	var runs []string
	for i := 1; i <= 20; i++ {
		start := base.Add(time.Duration(i) * time.Hour)
		end := start.Add(time.Minute)
		outcome, code := "success", 0
		if i == 20 {
			outcome, code = "failed", 1
		}
		runs = append(runs, fmt.Sprintf(`{"run_id":%d,"trigger":"schedule","outcome":%q,"started_at":%q,"ended_at":%q,"exit_code":%d}`,
			i, outcome, start.Format(time.RFC3339), end.Format(time.RFC3339), code))
	}
	state := `{"version":1,"harnesses":{},"runs":{"nightly":{"last_run_id":20,"runs":[` + strings.Join(runs, ",") + `]}}}`
	if err := os.WriteFile(e.state, []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := sweepCfg(sweep("nightly", "exit 0"))

	m, closeM := e.manager(t, cfg, fastPolicy())
	rs := m.Runs("nightly")
	if len(rs) != 20 {
		t.Fatalf("after the import: %d runs, want 20", len(rs))
	}
	if last := rs[19]; last.RunID != 20 || last.Outcome != OutcomeFailed || ConsecutiveFailures(rs) != 1 {
		t.Errorf("newest imported run = %+v; jobs would misreport it", last)
	}
	imported := 0
	for _, ln := range ledgerLines(t, e) {
		if ln["imported"] == true {
			imported++
		}
	}
	if imported != 40 {
		t.Errorf("%d lines marked imported, want 40 (an opened and a closed per run)", imported)
	}
	// state.json: the file, not the struct. No record list, and the id kept.
	waitFor(t, 5*time.Second, "state.json drops the record list", func() bool {
		return !strings.Contains(readText(t, e.state), `"runs": [`) && !strings.Contains(readText(t, e.state), `"runs":[`)
	})
	var ps struct {
		Runs map[string]map[string]json.RawMessage `json:"runs"`
	}
	if err := json.Unmarshal([]byte(readText(t, e.state)), &ps); err != nil {
		t.Fatal(err)
	}
	if got := string(ps.Runs["nightly"]["last_run_id"]); got != "20" {
		t.Errorf("state.json last_run_id = %s, want 20", got)
	}
	if _, has := ps.Runs["nightly"]["runs"]; has {
		t.Errorf("state.json still carries run records: %s", readText(t, e.state))
	}
	closeM()

	before := len(ledgerLines(t, e))
	m2, _ := e.manager(t, cfg, fastPolicy())
	if after := len(ledgerLines(t, e)); after != before {
		t.Errorf("the second boot wrote %d lines; the import must run once", after-before)
	}
	m2.StartRun("nightly", RunRequest{Trigger: TriggerSchedule})
	rs = waitRuns(t, m2, "nightly", "run 21", func(rs []RunRecord) bool {
		return len(rs) == 21 && rs[20].Outcome == OutcomeSuccess
	})
	if rs[20].RunID != 21 {
		t.Errorf("first run after the upgrade = %d, want 21", rs[20].RunID)
	}
}

// REQ-17 "A harness with secrets in its env file": neither the env_file's
// value nor a token in the prompt reaches any ledger line. Grepped in the files.
func TestLedgerCarriesNoSecrets(t *testing.T) {
	e := newRunsEnv(t)
	envFile := filepath.Join(e.dir, "secrets.env")
	const secret, token = "hunter2-must-not-persist", "prompt-token-must-not-persist"
	if err := os.WriteFile(envFile, []byte("HARNESS_TEST_SECRET="+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A real prompt harness: the claude-code adapter builds the argv from the
	// prompt, and a stand-in `claude` on PATH checks it saw the env_file.
	bin := filepath.Join(e.dir, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	stub := "#!/bin/sh\ntest -n \"$HARNESS_TEST_SECRET\"\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	h := sweep("sweep", "")
	h.Adapter, h.Args = "claude-code", nil
	h.EnvFile = envFile
	h.Prompt = "use " + token + " to log in"
	m, closeM := e.manager(t, sweepCfg(h), fastPolicy())
	m.StartRun("sweep", RunRequest{Trigger: TriggerSchedule})
	rs := waitRuns(t, m, "sweep", "run finishes", func(rs []RunRecord) bool {
		return len(rs) == 1 && rs[0].Outcome != OutcomeRunning
	})
	if rs[0].Outcome != OutcomeSuccess {
		t.Fatalf("the run did not see its env_file: %+v", rs[0])
	}
	closeM()

	text := ledgerText(t, e)
	if text == "" {
		t.Fatal("the ledger is empty; the check below would pass vacuously")
	}
	for _, s := range []string{secret, token} {
		if strings.Contains(text, s) {
			t.Errorf("a ledger line carries %q", s)
		}
	}
}

// REQ-17: an event's payload never reaches the ledger, only its identifiers.
func TestLedgerCarriesNoEventPayload(t *testing.T) {
	e := newRunsEnv(t)
	m, closeM := e.manager(t, sweepCfg(triggeredSweep("pr-review", "echo ran", "webhook.gitea-pr")), fastPolicy())
	env := webhookEvent("webhook.gitea-pr")
	m.StartRun("pr-review", RunRequest{Trigger: TriggerWebhook, Source: env.Source, Event: env})
	waitRuns(t, m, "pr-review", "run finishes", outcomesAre(OutcomeSuccess))
	closeM()

	text := ledgerText(t, e)
	if !strings.Contains(text, `"event_id":"delivery-1"`) {
		t.Fatalf("the ledger lacks the event id, so the payload check proves nothing:\n%s", text)
	}
	if strings.Contains(text, sentinelPayload) {
		t.Error("a ledger line carries the event payload")
	}
}
