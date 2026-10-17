// Package healthprobe runs one health_cmd hook for the daemon and reports
// its verdict.
//
// It lives outside internal/supervisor on purpose. The admission funnel
// (SPEC-0021 REQ-4, ADR-0027) gates harness process starts, and its
// source-walking check admits process spawns only at spawn's exec sites,
// with no exceptions — a health probe is not a harness start: it spawns no
// supervised process, takes no PTY, and must not consume the harness's run
// budget. Like the notify hook (ADR-0033) it is a daemon-side hook, and it
// execs outside the supervisor package for the same reason. The prober's
// scheduling, streak and action logic stays in internal/supervisor.
//
// Governing: ADR-0045 (proposed); stump.wtf/harness#960.
package healthprobe

import (
	"context"
	"errors"
	"os/exec"
)

// Result is one probe run's verdict.
type Result struct {
	OK       bool
	ExitCode int // -1 when the probe could not start or outran its context
	Output   string
}

// Run execs command directly — not through a shell: the config file is
// trusted for what it runs, but the config text is never a script — with
// environment env, and returns its combined output.
func Run(ctx context.Context, command string, env []string) Result {
	cmd := exec.CommandContext(ctx, command)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	res := Result{OK: err == nil, Output: string(out)}
	if err == nil {
		return res
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		res.ExitCode = ee.ExitCode()
		if ctx.Err() != nil {
			res.ExitCode = -1 // a probe that outran its timeout is a failure
		}
	} else {
		res.ExitCode = -1 // could not start: the probe itself is broken
	}
	return res
}
