package supervisor

// Run Lifecycle Hooks And Per-Run Work Directories
//
// These tests drive a real Manager, so every assertion is about what the run
// record says, what actually landed on disk, and whether the harness process
// was exec'd at all; never about what an internal helper returned. The hook
// exec itself is a seam (runHookExec): the suite must not depend on which
// programs happen to be on the machine's PATH, and what is under test is where
// a hook sits in the run's life, not exec.
//
// Governing: ADR-0051 (proposed); stump.wtf/harness#980.

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/runhook"
)

type hookCall struct {
	hook core.RunHook
	argv []string
	dir  string
	env  []string
}

// stubHook replaces the hook exec for one test, recording every call and
// answering with resultFor. It restores the real exec on cleanup, so a test
// that runs after this one spawns programs again.
func stubHook(t *testing.T, resultFor func(hook core.RunHook) runhook.Result) *[]hookCall {
	t.Helper()
	var calls []hookCall
	real := runHookExec
	runHookExec = func(hook core.RunHook, ctx context.Context, argv []string, dir string, env []string) runhook.Result {
		calls = append(calls, hookCall{hook: hook, argv: argv, dir: dir, env: env})
		res := resultFor(hook)
		res.OK = res.ExitCode == 0
		return res
	}
	t.Cleanup(func() { runHookExec = real })
	return &calls
}

func alwaysOK(core.RunHook) runhook.Result { return runhook.Result{} }
func alwaysFails(h core.RunHook) runhook.Result {
	return runhook.Result{ExitCode: 3}
}

func TestPreRunFailureSpawnsNothing(t *testing.T) {
	e := newRunsEnv(t)
	h, out := commandSweep(t, "report", "--run", "{{run.id}}")
	h.PreRun = []string{"/bin/false"}
	calls := stubHook(t, alwaysFails)
	m, _ := e.manager(t, sweepCfg(h), fastPolicy())

	m.StartRun("report", RunRequest{Trigger: TriggerSchedule, Window: time.Now()})
	rec := waitRuns(t, m, "report", "the run closes failed by its hook",
		func(rs []RunRecord) bool { return len(rs) == 1 && rs[0].Outcome == OutcomeFailed })[0]

	if rec.Reason != ReasonPreRun {
		t.Errorf("reason = %q, want %q", rec.Reason, ReasonPreRun)
	}
	if _, err := os.Stat(out); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the harness process ran anyway (probe record %s exists: %v)", out, err)
	}
	if len(*calls) != 1 || (*calls)[0].hook != core.HookPreRun {
		t.Fatalf("hook calls = %+v, want one pre_run", *calls)
	}
}

func TestPostRunDemotesASuccessAndNamesTheVerdict(t *testing.T) {
	e := newRunsEnv(t)
	dir := t.TempDir()
	h, _ := commandSweep(t, "report", "--run", "{{run.id}}")
	h.Workdir = filepath.Join(dir, "runs/{{run.id}}")
	h.PostRun = []string{"/bin/check", "{{run.outcome}}", "{{run.datadir}}"}
	calls := stubHook(t, alwaysFails)
	m, _ := e.manager(t, sweepCfg(h), fastPolicy())

	m.StartRun("report", RunRequest{Trigger: TriggerSchedule, Window: time.Now()})
	rec := waitRuns(t, m, "report", "the verdict check demotes the run",
		func(rs []RunRecord) bool { return len(rs) == 1 && rs[0].Outcome == OutcomeFailed })[0]

	if rec.Reason != ReasonPostRun {
		t.Errorf("reason = %q, want %q", rec.Reason, ReasonPostRun)
	}
	if rec.ExitCode == nil || *rec.ExitCode != 0 {
		t.Errorf("exit code = %v, want the process's own 0", rec.ExitCode)
	}
	var post *hookCall
	for i := range *calls {
		if (*calls)[i].hook == core.HookPostRun {
			post = &(*calls)[i]
		}
	}
	if post == nil {
		t.Fatalf("no post_run call in %+v", *calls)
	}
	want := []string{"/bin/check", "success", filepath.Join(dir, "runs", strconv.Itoa(rec.RunID), ".harness-run")}
	if !slices.Equal(post.argv, want) {
		t.Errorf("post_run argv = %q, want %q", post.argv, want)
	}
	// The hook is shown the verdict the process earned; what it thinks of that
	// verdict is what may then change the record.
	if !strings.Contains(strings.Join(post.env, "\n"), "HARNESS_RUN_OUTCOME=success") {
		t.Errorf("post_run env = %q, want the run's own outcome", post.env)
	}
	if !strings.Contains(strings.Join(post.env, "\n"), "HARNESS_NAME=report") {
		t.Errorf("post_run env = %q, want the harness named", post.env)
	}
}

func TestSuccessfulRunKeepsItsOutcomeAndGetsBothDirs(t *testing.T) {
	e := newRunsEnv(t)
	dir := t.TempDir()
	h, _ := commandSweep(t, "report", "--run", "{{run.id}}")
	h.Workdir = filepath.Join(dir, "runs/{{run.id}}")
	h.PreRun = []string{"/bin/true"}
	h.PostRun = []string{"/bin/true"}
	calls := stubHook(t, alwaysOK)
	m, _ := e.manager(t, sweepCfg(h), fastPolicy())

	m.StartRun("report", RunRequest{Trigger: TriggerSchedule, Window: time.Now()})
	waitRuns(t, m, "report", "the run succeeds", outcomesAre(OutcomeSuccess))

	runs := filepath.Join(dir, "runs")
	entries, err := os.ReadDir(runs)
	if err != nil || len(entries) != 1 {
		t.Fatalf("run directories under %s = %v (err %v), want one", runs, entries, err)
	}
	if !strings.HasPrefix(entries[0].Name(), "1") {
		t.Errorf("run directory = %q, want it named for the run", entries[0].Name())
	}
	data := filepath.Join(runs, entries[0].Name(), ".harness-run")
	if info, err := os.Stat(data); err != nil || !info.IsDir() {
		t.Errorf("agent data dir %s missing: %v", data, err)
	}
	if len(*calls) != 2 || (*calls)[0].hook != core.HookPreRun || (*calls)[1].hook != core.HookPostRun {
		t.Errorf("hook calls = %+v, want pre_run then post_run", *calls)
	}
	for _, c := range *calls {
		if c.dir != filepath.Join(runs, entries[0].Name()) {
			t.Errorf("hook ran in %q, want the run's own directory", c.dir)
		}
	}
}

func TestWorkdirRetainPrunesOlderRunDirs(t *testing.T) {
	e := newRunsEnv(t)
	dir := t.TempDir()
	h, _ := commandSweep(t, "report", "--run", "{{run.id}}")
	h.Workdir = filepath.Join(dir, "runs/{{run.id}}")
	h.WorkdirRetain = 2
	m, _ := e.manager(t, sweepCfg(h), fastPolicy())

	for i := 0; i < 3; i++ {
		m.StartRun("report", RunRequest{Trigger: TriggerSchedule, Window: time.Now()})
		waitRuns(t, m, "report", "the run finished", func(rs []RunRecord) bool { return len(rs) == i+1 })
		// The next start waits for this run to be over, not merely recorded: a
		// run still on the books would overlap-skip it, and then the cap would
		// be measured against directories a skipped firing never created.
		waitFor(t, 5*time.Second, "the harness is back to stopped", func() bool {
			snap, ok := m.Snapshot("report")
			return ok && snap.State == core.StateStopped
		})
	}

	entries, err := os.ReadDir(filepath.Join(dir, "runs"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		names := make([]string, 0, len(entries))
		for _, en := range entries {
			names = append(names, en.Name())
		}
		t.Fatalf("run directories = %v, want the newest 2 of 3", names)
	}
	if _, err := os.Stat(filepath.Join(dir, "runs", "1")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the oldest run directory survived the cap: %v", err)
	}
}

func TestRetentionNeverTouchesWhatIsNotARunDir(t *testing.T) {
	e := newRunsEnv(t)
	dir := t.TempDir()
	runs := filepath.Join(dir, "runs")
	if err := os.MkdirAll(runs, 0o700); err != nil {
		t.Fatal(err)
	}
	// A file, a symlink, and a directory the template's shape does not match:
	// none is a run directory, so none may be removed by a cap of 1.
	for name, make := range map[string]func(string) error{
		"notes.txt": func(p string) error { return os.WriteFile(p, []byte("keep"), 0o600) },
		"checkout":  func(p string) error { return os.Symlink(dir, p) },
		"nightly":   func(p string) error { return os.MkdirAll(p, 0o700) },
	} {
		if err := make(filepath.Join(runs, name)); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-24 * time.Hour)
	if err := os.MkdirAll(filepath.Join(runs, "7"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := markRunDir(filepath.Join(runs, "7"), "report"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(runs, "7"), old, old); err != nil {
		t.Fatal(err)
	}

	h, _ := commandSweep(t, "report", "--run", "{{run.id}}")
	h.Workdir = filepath.Join(runs, "{{run.id}}")
	h.WorkdirRetain = 1
	m, _ := e.manager(t, sweepCfg(h), fastPolicy())
	m.StartRun("report", RunRequest{Trigger: TriggerSchedule, Window: time.Now()})
	waitRuns(t, m, "report", "the run finished", outcomesAre(OutcomeSuccess))

	for _, name := range []string{"notes.txt", "checkout", "nightly"} {
		if _, err := os.Lstat(filepath.Join(runs, name)); err != nil {
			t.Errorf("%s was removed by retention: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(runs, "7")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the marked stale run directory survived the cap: %v", err)
	}
}

// TestRetentionSkipsAnotherHarnessRunDir pins the mark's content, not just its
// existence: two harnesses that share a workdir parent prune only their own
// directories, so a sibling's runs and an unmarked stranger both survive a
// cap that spends this harness's stale one.
func TestRetentionSkipsAnotherHarnessRunDir(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-24 * time.Hour)
	for name, marked := range map[string]string{
		"stale":   "report",
		"foreign": "nightly",
		"plain":   "",
		"current": "report",
	} {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if marked != "" {
			if err := markRunDir(p, marked); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if got := pruneRunDirs(dir, "*", "report", 1, "current"); got != 1 {
		t.Fatalf("pruneRunDirs removed %d directories, want only report's own stale one", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "stale")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("report's stale run directory survived its own cap: %v", err)
	}
	for _, name := range []string{"foreign", "plain", "current"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s was removed by report's retention: %v", name, err)
		}
	}
}

func TestHooksOnASharedWorkdirGetTheirOwnDataDir(t *testing.T) {
	e := newRunsEnv(t)
	dir := t.TempDir()
	h, _ := commandSweep(t, "report", "--run", "{{run.id}}")
	h.Workdir = dir
	h.PostRun = []string{"/bin/true", "{{run.datadir}}"}
	calls := stubHook(t, alwaysOK)
	m, _ := e.manager(t, sweepCfg(h), fastPolicy())

	m.StartRun("report", RunRequest{Trigger: TriggerSchedule, Window: time.Now()})
	waitRuns(t, m, "report", "the run finished", outcomesAre(OutcomeSuccess))

	if len(*calls) != 1 {
		t.Fatalf("hook calls = %+v, want one post_run", *calls)
	}
	want := filepath.Join(dir, runDataDirName, "1")
	if (*calls)[0].argv[1] != want {
		t.Errorf("{{run.datadir}} = %q, want %q", (*calls)[0].argv[1], want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Errorf("the shared workdir's per-run data directory is missing: %v", err)
	}
}
