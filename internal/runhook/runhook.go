// Package runhook runs one pre_run or post_run hook for the daemon and
// reports its verdict.
//
// It lives outside internal/supervisor on purpose, for the reason
// internal/healthprobe does: the admission funnel (SPEC-0021 REQ-4, ADR-0027)
// admits process spawns only at spawn's exec sites, and a lifecycle hook is
// not a harness start; it takes no admission, no PTY, no run budget, and no
// restart policy. Like `[notify]` and `health_cmd` it is a daemon-side hook
// (ADR-0033). The hook's argv is rendered and its outcome folded into the run
// record by the supervisor, which owns the run.
//
// argv is exec'd directly, never through a shell: the config file is trusted
// for what it runs, but the config text is never a script.
//
// Governing: ADR-0033, ADR-0051 (proposed); stump.wtf/harness#980.
package runhook

import (
	"context"
	"errors"
	"os/exec"
)

// Result is one hook's verdict. ExitCode is -1 when the hook could not be
// started at all (no such program) or outran its context, which the supervisor
// treats as a failed hook the same way as a nonzero exit.
type Result struct {
	OK       bool
	ExitCode int
	Output   string
}

// Run execs argv with working directory dir and environment env, and returns
// its combined output. An empty dir means inherit, which no caller in the
// daemon uses: a hook runs in the run's directory.
func Run(ctx context.Context, argv []string, dir string, env []string) Result {
	if len(argv) == 0 {
		return Result{ExitCode: -1, Output: "hook has no argv"}
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	res := Result{OK: err == nil, Output: string(out)}
	if res.OK {
		res.ExitCode = 0
		return res
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		res.ExitCode = ee.ExitCode()
		if ctx.Err() != nil {
			res.ExitCode = -1 // a hook cut off by its deadline failed
		}
	} else {
		res.ExitCode = -1 // could not start: the hook itself is broken
	}
	return res
}
