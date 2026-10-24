package config

// Run Lifecycle Hook And Workdir Keys
//
// TL;DR: the load-time rules for `pre_run`, `post_run`, a templated `workdir`
// and `workdir_retain`. Which template paths each of them may name is
// core.CheckRunHook's and core.CheckWorkdirTemplate's decision (one place, so
// the supervisor and the parser cannot disagree); what lives here is the part
// that needs the rest of the harness table: which kinds of harness have runs
// discrete enough for a hook to describe, and whether a retention cap has a
// run directory to prune.
//
// Governing: ADR-0051 (proposed); stump.wtf/harness#980.

import (
	"github.com/stump-wtf/harness/internal/core"
)

// checkRunHook validates one hook key. An absent or empty list configures no
// hook and is accepted; anything else has to be argv on a harness whose runs
// are discrete enough for the hook to mean something.
//
// A resident harness is refused rather than tolerated: its process lifetime
// spans every run, so `post_run` would fire on a crash restart of a process
// that was never a run the operator asked for, and `pre_run` would run inside
// the crash-loop backoff. A one-shot is a harness whose every start is one run
// the operator (or the clock, or an event) asked for; a prompt harness, or a
// `command` harness with a schedule or triggers.
func checkRunHook(filename, name string, line int, hook core.RunHook, argv []string, oneShot bool) ([]string, error) {
	if len(argv) == 0 {
		return nil, nil
	}
	if !oneShot {
		return nil, newError(filename, line,
			"harness %q: %q requires a one-shot harness (a harness with a prompt source, or harness = \"command\" with a schedule or triggers); a resident harness's process spans every run, so a hook around it has no run to describe",
			name, string(hook))
	}
	if err := core.CheckRunHook(hook, argv); err != nil {
		return nil, newError(filename, line, "harness %q: %v", name, err)
	}
	return argv, nil
}

// checkRunWorkdir validates `workdir` and `workdir_retain` together, and
// reports the retention cap to carry downstream. A workdir with no
// placeholder means exactly what it always meant; a shared directory the
// daemon never creates and never prunes; so the only rules for that case are
// the old ones.
//
// Retention is checked before the template's paths are, because the structural
// rule (only the last element may vary) is what makes it safe to delete
// anything at all: pruning is derived from the template, and a template whose
// varying element is not the last would point it at directories that were
// there before harness was.
func checkRunWorkdir(filename, name string, line int, workdir string, retain *int, oneShot bool) (int, error) {
	templated := core.WorkdirHasTemplate(workdir)
	if retain == nil {
		if templated {
			if !oneShot {
				return 0, newError(filename, line,
					"harness %q: a templated \"workdir\" requires a one-shot harness; a resident harness is one process, not one directory per run", name)
			}
			if err := core.CheckWorkdirTemplate(workdir, false); err != nil {
				return 0, newError(filename, line, "harness %q: %v", name, err)
			}
		}
		return 0, nil
	}
	if *retain < 1 {
		return 0, newError(filename, line,
			"harness %q: \"workdir_retain\" must be at least 1 (got %d); 0 would mean keeping none, which is not a thing the daemon does", name, *retain)
	}
	if !templated {
		return 0, newError(filename, line,
			"harness %q: \"workdir_retain\" requires a templated \"workdir\" (for example \"~/sweeps/%s/runs/{{run.id}}\"); there is nothing to prune in a directory every run shares", name, name)
	}
	if !oneShot {
		return 0, newError(filename, line,
			"harness %q: \"workdir_retain\" requires a one-shot harness, because only a one-shot's directory is finished and disposable", name)
	}
	if err := core.CheckWorkdirTemplate(workdir, true); err != nil {
		return 0, newError(filename, line, "harness %q: %v", name, err)
	}
	return *retain, nil
}
