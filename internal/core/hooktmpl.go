package core

// Run Lifecycle Hook And Workdir Templates
//
// TL;DR: `pre_run` and `post_run` are argv lists the daemon execs around a
// one-shot run, and `workdir` may now itself be a template, so a run can be
// given a directory of its own instead of sharing one with every earlier run
// of the same harness. This file is the ONE place that decides which context
// paths each of those three may name, so the config parser and the supervisor
// cannot disagree about what a hook is allowed to see.
//
// The allow set deliberately differs from command argv's. A hook is operator
// text exec'd by the daemon, not a supervised process, so it may name the
// run's verdict (`run.outcome`, `run.exit_code`), which is the whole point of
// `post_run` and which argv may never see because a run has no verdict while it
// is being spawned. What no hook may name is an event path or untrusted text:
// a hook's argv must not be steerable by anything an outside party wrote into
// a webhook payload, and a hook that needs the payload gets `{{event.file}}`
// the way a command harness does. A templated `workdir` is the narrowest of
// the three: it decides where a process is born and what the daemon may later
// prune, so only identifiers and dates are allowed, and the substitution must
// sit in the LAST path element so retention can tell a run directory from a
// directory that was already there.
//
// Governing: ADR-0051 (proposed); stump.wtf/harness#980.

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/stump-wtf/harness/internal/tmpl"
)

// Context paths the lifecycle hooks and a templated workdir may name beyond
// the argv set. run.datadir is the per-run agent data directory the daemon
// creates (ADR-0051); run.outcome and run.exit_code exist only once the run
// has ended, so only post_run may reference them.
const (
	PathRunDataDir  = "run.datadir"
	PathRunOutcome  = "run.outcome"
	PathRunExitCode = "run.exit_code"
)

// RunHook names one of the two lifecycle hooks. Its string form is the config
// key, so an error can name the key without a second table.
type RunHook string

const (
	// HookPreRun runs after the run's record is open and before the harness
	// process is spawned.
	HookPreRun RunHook = "pre_run"
	// HookPostRun runs after the process is reaped and before the run's record
	// is closed, so its verdict can still change the outcome.
	HookPostRun RunHook = "post_run"
)

// hookPaths is the allow set shared by both hooks: the operator and daemon
// tiers of the run context, plus the per-run data directory.
var hookPaths = map[string]bool{
	PathHarnessName: true, PathHarnessWorkdir: true, PathModel: true, PathPackageDir: true,
	PathRunID: true, PathRunTrigger: true, PathRunSource: true,
	PathRunStartedAt: true, PathRunDate: true, PathRunDataDir: true,
}

// postRunPaths are the two paths that exist only after a run has ended.
var postRunPaths = map[string]bool{PathRunOutcome: true, PathRunExitCode: true}

// workdirPaths is a templated workdir's allow set: identifiers and dates that
// name a directory and nothing else. harness.workdir is absent by construction
// (a workdir cannot contain itself), and no event or untrusted path is here:
// where a process is born is the operator's decision, never a payload's.
var workdirPaths = map[string]bool{
	PathHarnessName: true, PathModel: true, PathPackageDir: true,
	PathRunID: true, PathRunTrigger: true, PathRunSource: true,
	PathRunStartedAt: true, PathRunDate: true,
}

// hookElement names a hook argv element the way every hook error does.
func hookElement(hook RunHook, i int) string {
	return fmt.Sprintf("%q", fmt.Sprintf("%s[%d]", string(hook), i))
}

// CheckRunHook validates one hook's whole argv: no empty element, no
// placeholder the hook can never be given, no path outside the hook's allow
// set. It is a load-time check, so a hook that could only ever fail at run
// time is a config error instead.
//
// A hook on a harness with no run records (no `schedule` and no `triggers`)
// still runs (an on-demand one-shot has a record) so unlike command argv,
// which is refused a required {{run.id}} on such a harness, a hook may name
// the run's own fields.
// Governing: ADR-0051 (proposed); stump.wtf/harness#980.
func CheckRunHook(hook RunHook, argv []string) error {
	for i, element := range argv {
		if strings.TrimSpace(element) == "" {
			return fmt.Errorf("%s: must not be blank", hookElement(hook, i))
		}
		t, err := tmpl.Parse(element)
		if err != nil {
			return fmt.Errorf("%s: %w", hookElement(hook, i), err)
		}
		for _, r := range t.Refs() {
			at := fmt.Sprintf("%s line %d, column %d", hookElement(hook, i), r.Line, r.Col)
			switch {
			case r.Kind == tmpl.Untrusted || untrustedPaths[r.Path]:
				return fmt.Errorf("%s: untrusted text is never permitted in %s ({{untrusted %s}} renders only inside a prompt template; give the program {{event.file}} to read instead)", at, hook, r.Path)
			case hookPaths[r.Path]:
			case hook == HookPostRun && postRunPaths[r.Path]:
			case eventPath(r.Path):
				return fmt.Errorf("%s: an event path is not available in %s (give the program {{event.file}} and let it read the envelope itself)", at, hook)
			default:
				return fmt.Errorf("%s: unknown template path %q (%s may reference: harness.name, harness.workdir, model, package.dir, run.id, run.trigger, run.source, run.started_at, run.date, run.datadir%s)",
					at, r.Path, hook, postRunHint(hook))
			}
		}
	}
	return nil
}

// postRunHint lists the two extra paths only post_run answers for.
func postRunHint(hook RunHook) string {
	if hook == HookPostRun {
		return ", run.outcome, run.exit_code"
	}
	return ""
}

// RenderRunHook renders a hook's argv against ctx, one element to one
// argument, exactly as RenderCommandArgs does (SPEC-0017 REQ-11): nothing is
// split, trimmed, globbed or re-quoted, and an element that renders empty
// stays an empty argument. The argv must already have passed CheckRunHook.
func RenderRunHook(argv []string, ctx tmpl.Context) ([]string, error) {
	out := make([]string, len(argv))
	for i, a := range argv {
		t, err := tmpl.Parse(a)
		if err != nil {
			return nil, fmt.Errorf("hook argv[%d]: %w", i, err)
		}
		v, err := t.Render(ctx)
		if err != nil {
			return nil, fmt.Errorf("hook argv[%d]: %w", i, err)
		}
		out[i] = v
	}
	return out, nil
}

// CheckWorkdirTemplate validates a templated `workdir`. Beyond the path allow
// set it holds one structural rule that makes retention safe: with
// `workdir_retain` set, every placeholder must sit in the LAST path element,
// so the directory retention prunes is a directory whose only varying part is
// the run directories themselves. Without that rule, `~/src/{{harness.name}}`
// plus a retain would mean pruning the operator's other checkouts.
// Governing: ADR-0051 (proposed); stump.wtf/harness#980.
func CheckWorkdirTemplate(workdir string, retained bool) error {
	t, err := tmpl.Parse(workdir)
	if err != nil {
		return fmt.Errorf("\"workdir\": %w", err)
	}
	for _, r := range t.Refs() {
		at := fmt.Sprintf("\"workdir\" line %d, column %d", r.Line, r.Col)
		switch {
		case r.Kind == tmpl.Untrusted || untrustedPaths[r.Path]:
			return fmt.Errorf("%s: untrusted text can never name a directory the daemon creates", at)
		case eventPath(r.Path):
			return fmt.Errorf("%s: an event path can never name a directory the daemon creates or prunes", at)
		case r.Path == PathHarnessWorkdir, r.Path == PathRunDataDir:
			return fmt.Errorf("%s: {{%s}} is the directory this template is deciding", at, r.Path)
		case postRunPaths[r.Path]:
			return fmt.Errorf("%s: {{%s}} does not exist until the run has ended", at, r.Path)
		case !workdirPaths[r.Path]:
			return fmt.Errorf("%s: unknown template path %q (workdir may reference: harness.name, model, package.dir, run.id, run.trigger, run.source, run.started_at, run.date)", at, r.Path)
		}
	}
	if retained {
		last, _, found := lastPathElement(t.Source())
		if !found {
			return fmt.Errorf("\"workdir\": with workdir_retain the template must sit below a fixed directory, so retention has somewhere to look (got %q)", t.Source())
		}
		if !strings.Contains(last, "{{") || strings.Contains(strings.TrimSuffix(t.Source(), last), "{{") {
			return fmt.Errorf("\"workdir\": with workdir_retain only the last path element may vary, so what retention removes can only ever be a run directory (got %q)", t.Source())
		}
	}
	return nil
}

// lastPathElement splits a rendered-or-templated path into its final element
// and everything before it. found is false for a path with no separator.
func lastPathElement(p string) (last, parent string, found bool) {
	i := strings.LastIndexAny(p, `/\`)
	if i < 0 {
		return p, "", false
	}
	return p[i+1:], p[:i], true
}

// hookPlaceholderRe matches one placeholder in template source text; it is the
// same shape tmpl.Parse accepts, and here it only ever builds a glob.
var hookPlaceholderRe = regexp.MustCompile(`\{\{[^{}]*\}\}`)

// RunDirGlob turns a per-run workdir template into (parent, glob): the
// directory retention scans, and the pattern its run directories match. The
// parent is the template with its last element removed and its placeholders
// already rendered away (CheckWorkdirTemplate guarantees none remain there);
// the glob is the last element with each placeholder replaced by `*`.
//
// A caller must only delete a match that is a real directory and not a
// symlink, and never the directory of the run in flight.
// Governing: ADR-0051 (proposed); stump.wtf/harness#980.
func RunDirGlob(workdir string, ctx tmpl.Context) (parent, glob string, ok bool) {
	t, err := tmpl.Parse(workdir)
	if err != nil {
		return "", "", false
	}
	src := t.Source()
	last, before, found := lastPathElement(src)
	if !found {
		return "", "", false
	}
	rendered, err := tmpl.Parse(before)
	if err != nil {
		return "", "", false
	}
	parent, err = rendered.Render(ctx)
	if err != nil {
		return "", "", false
	}
	return parent, hookPlaceholderRe.ReplaceAllString(last, "*"), true
}

// WorkdirHasTemplate reports whether a configured workdir carries any
// placeholder, which is what makes a run's directory per-run rather than
// shared.
func WorkdirHasTemplate(workdir string) bool {
	return strings.Contains(workdir, "{{")
}
