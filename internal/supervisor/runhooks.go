package supervisor

// Run Lifecycle Hooks
//
// TL;DR: `pre_run` runs after a run's record is open and before its process is
// spawned; `post_run` runs after the process is reaped and before the record
// closes, so the run's own verdict is already known and a failing post_run can
// still change what the record says. Both are daemon-side hooks, like
// `[notify]` and `health_cmd`: no PTY, no admission, no restart policy, no run
// of their own (ADR-0033).
//
// Where they sit matters more than what they run. A pre_run that failed must
// stop the harness from spawning at all; the point of the hook is to put the
// run in a state the agent can be trusted with, and an agent that starts
// anyway is the failure the hook was written to prevent; so a failed pre_run
// closes the run `failed` with reason `pre_run_failed` and never execs the
// harness. A post_run sits inside the record's lifetime for the opposite
// reason: it is the mechanical check the thread asked for ("did this run
// actually do the thing"), and a check that could only run after the record
// was sealed could annotate nothing. It demotes a `success` to `failed` with
// reason `post_run_failed` and leaves every other outcome exactly as the
// process earned it; a failed run whose post_run also fails is still the
// failed run it was.
//
// A hook never runs through a shell, and never sees an event payload in its
// argv: it gets {{event.file}} the way a command harness does, so text an
// outside party wrote stays data.
//
// Governing: ADR-0033, ADR-0051 (proposed); stump.wtf/harness#980.

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/runhook"
	"github.com/stump-wtf/harness/internal/tmpl"
)

// DefaultRunHookTimeout bounds one hook: like a health probe, a hook that
// never answers would otherwise hold the run open forever.
const DefaultRunHookTimeout = 10 * time.Minute

// runHookExec is the exec seam, a variable so a test can observe what a hook
// was asked to run, and fail one, without the suite depending on what is on
// the machine's PATH.
var runHookExec = func(hook core.RunHook, ctx context.Context, argv []string, dir string, env []string) runhook.Result {
	return runhook.Run(ctx, argv, dir, env)
}

// hookTimeout overrides DefaultRunHookTimeout for tests; zero means default.
var hookTimeout time.Duration

func hookDeadline() time.Duration {
	if hookTimeout > 0 {
		return hookTimeout
	}
	return DefaultRunHookTimeout
}

// hookContext is the template context one hook's argv renders against: the
// workdir tier the run was given, the per-run data directory, and; for
// post_run only, because they do not exist before the run ends; the verdict.
func (s *Supervisor) hookContext(run *activeRun, hook core.RunHook, outcome RunOutcome, code *int, now time.Time) tmpl.Map {
	v := workdirValues(s.harness, runEnvOf(run), now)
	v[core.PathHarnessWorkdir] = run.workdir
	v[core.PathRunDataDir] = run.dataDir
	if hook == core.HookPostRun {
		v[core.PathRunOutcome] = string(outcome)
		if code != nil {
			v[core.PathRunExitCode] = strconv.Itoa(*code)
		} else {
			v[core.PathRunExitCode] = ""
		}
	}
	return tmpl.Map{Values: v}
}

// hookEnv is a hook's environment: the daemon's, minus any earlier run's
// reserved variables, plus this run's own context. env_file is deliberately
// not sourced; it is the harness process's environment, and a hook is not
// that process (ADR-0051), exactly as health_cmd inherits the daemon instead.
func (s *Supervisor) hookEnv(run *activeRun, env RunEnv, outcome RunOutcome, code *int) []string {
	e := withoutRunVars(os.Environ())
	e = append(e, env.vars()...)
	e = append(e,
		"HARNESS_NAME="+s.harness.Name,
		"HARNESS_WORKDIR="+run.workdir,
		"HARNESS_RUN_DATADIR="+run.dataDir,
	)
	if run.rec.RunID > 0 {
		e = append(e, "HARNESS_RUN_ID="+strconv.Itoa(run.rec.RunID))
	}
	if outcome != "" {
		e = append(e, "HARNESS_RUN_OUTCOME="+string(outcome))
	}
	if code != nil {
		e = append(e, "HARNESS_RUN_EXIT_CODE="+strconv.Itoa(*code))
	}
	return e
}

// runHook renders and execs one hook. A render failure is returned as an error
// that unwraps to tmpl.ErrUnresolved or tmpl.ErrGrammar; an exec failure is a
// Result the caller judges. Output is capped: a hook that prints a wall gets
// its tail, like every other daemon-run program here.
func (s *Supervisor) runHook(hook core.RunHook, argv []string, run *activeRun, outcome RunOutcome, code *int) (runhook.Result, error) {
	ctx := s.hookContext(run, hook, outcome, code, time.Now())
	args, err := core.RenderRunHook(argv, ctx)
	if err != nil {
		return runhook.Result{}, err
	}
	ctxRun, cancel := context.WithTimeout(context.Background(), hookDeadline())
	defer cancel()
	res := runHookExec(hook, ctxRun, args, run.workdir, s.hookEnv(run, runEnvOf(run), outcome, code))
	if run.file != nil {
		// The hook's output belongs with the run it wraps, so an operator
		// reading the run log sees the setup and the verdict check beside the
		// agent's own session.
		_, _ = fmt.Fprintf(run.file, "\n[harness] %s: exit=%d\n%s\n", hook, res.ExitCode, tailBytes(res.Output, hookOutputBytes))
	}
	s.logEvent("run hook", "run_id", run.rec.RunID, "hook", string(hook), "exit_code", res.ExitCode)
	return res, nil
}

// prepareRun gives the run in flight its directories and runs pre_run, and
// reports whether the harness process may be spawned. It is called from
// beginStart, the one path every start reaches, so a hook cannot be skipped by
// whichever edge started the run.
//
// A harness with no templated workdir and no hooks is left exactly as it was:
// no directory created, nothing pruned, no spawn-time work added to a harness
// whose operator never asked for any of it.
func (s *Supervisor) prepareRun(run *activeRun) bool {
	if run == nil {
		return true
	}
	templated := core.WorkdirHasTemplate(s.harness.Workdir)
	hasHooks := len(s.harness.PreRun) > 0 || len(s.harness.PostRun) > 0
	if !templated && !hasHooks {
		return true
	}
	workdir, dataDir, pruned, err := prepareRunDirs(s.harness, runEnvOf(run), time.Now())
	if err != nil {
		s.logEvent("run workdir failed", "run_id", run.rec.RunID, "err", err.Error())
		s.transition(core.StateStopped)
		s.finishRunWith(OutcomeFailed, nil, ReasonWorkdir)
		return false
	}
	run.workdir, run.dataDir = workdir, dataDir
	if pruned > 0 {
		s.logEvent("run workdirs pruned", "run_id", run.rec.RunID, "removed", pruned)
	}
	if ok, code := s.runPreRun(run); !ok {
		s.transition(core.StateStopped)
		s.finishRunWith(OutcomeFailed, &code, ReasonPreRun)
		return false
	}
	run.preRan = true
	return true
}

// hookOutputBytes caps what a hook contributes to the run log.
const hookOutputBytes = 4096

// tailBytes keeps the last n bytes of s, dropping a partial rune at the cut.
func tailBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := len(s) - n
	for cut < len(s) && s[cut]&0xc0 == 0x80 {
		cut++
	}
	return s[cut:]
}

// runPreRun executes pre_run before the harness spawns. ok is false when the
// hook failed or could not render, in which case the caller must not spawn.
func (s *Supervisor) runPreRun(run *activeRun) (ok bool, exitCode int) {
	if len(s.harness.PreRun) == 0 {
		return true, 0
	}
	res, err := s.runHook(core.HookPreRun, s.harness.PreRun, run, "", nil)
	if err != nil {
		s.logEvent("run hook failed", "run_id", run.rec.RunID, "hook", string(core.HookPreRun), "err", err.Error())
		return false, -1
	}
	return res.OK, res.ExitCode
}

// runPostRun executes post_run and reports the outcome the record should
// carry. Only a success is demoted: nothing a hook dislikes can turn an
// already-failed, timed-out or cancelled run into a different kind of failure,
// and inventing one would hide what the process actually did.
func (s *Supervisor) runPostRun(run *activeRun, outcome RunOutcome, code *int) (RunOutcome, RunReason) {
	if len(s.harness.PostRun) == 0 {
		return outcome, ""
	}
	res, err := s.runHook(core.HookPostRun, s.harness.PostRun, run, outcome, code)
	if err != nil {
		s.logEvent("run hook failed", "run_id", run.rec.RunID, "hook", string(core.HookPostRun), "err", err.Error())
		if outcome == OutcomeSuccess {
			return OutcomeFailed, ReasonPostRun
		}
		return outcome, ""
	}
	if !res.OK && outcome == OutcomeSuccess {
		return OutcomeFailed, ReasonPostRun
	}
	return outcome, ""
}
