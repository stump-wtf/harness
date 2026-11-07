package core

// Run Lifecycle Hook And Workdir Templates
//
// The allow set is the security property here, so these tests are mostly about
// what a hook and a templated workdir may NOT name: an event path or untrusted
// text in a hook's argv, a verdict a pre_run cannot have earned yet, a
// directory name an outside party's payload chose.
//
// Governing: ADR-0051 (proposed); stump.wtf/harness#980.

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stump-wtf/harness/internal/tmpl"
)

func ctx(pairs ...string) tmpl.Map {
	v := map[string]string{}
	for i := 0; i+1 < len(pairs); i += 2 {
		v[pairs[i]] = pairs[i+1]
	}
	return tmpl.Map{Values: v}
}

func TestCheckRunHookAllowSet(t *testing.T) {
	ok := []struct {
		hook RunHook
		argv []string
	}{
		{HookPreRun, []string{"/bin/setup", "{{harness.name}}", "{{harness.workdir}}", "{{run.datadir}}", "{{run.id}}", "{{run.date}}"}},
		{HookPostRun, []string{"/bin/check", "{{run.outcome}}", "{{run.exit_code}}", "{{model?}}"}},
	}
	for _, tc := range ok {
		if err := CheckRunHook(tc.hook, tc.argv); err != nil {
			t.Errorf("%s %q rejected: %v", tc.hook, tc.argv, err)
		}
	}

	bad := []struct {
		hook, element, want string
	}{
		{string(HookPreRun), "{{run.outcome}}", `unknown template path "run.outcome"`},
		{string(HookPreRun), "{{run.exit_code}}", `unknown template path "run.exit_code"`},
		{string(HookPostRun), "{{event.url}}", "an event path is not available in post_run"},
		{string(HookPreRun), "{{event.title}}", "untrusted text is never permitted in pre_run"},
		{string(HookPostRun), "{{untrusted event.body}}", "untrusted text is never permitted in post_run"},
		{string(HookPostRun), "{{nope}}", `unknown template path "nope"`},
	}
	for _, tc := range bad {
		err := CheckRunHook(RunHook(tc.hook), []string{"/bin/x", tc.element})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s %q loaded; want %q (got %v)", tc.hook, tc.element, tc.want, err)
		}
	}
}

func TestCheckRunHookRejectsBlankAndBroken(t *testing.T) {
	if err := CheckRunHook(HookPreRun, []string{"/bin/x", "  "}); err == nil || !strings.Contains(err.Error(), `must not be blank`) {
		t.Errorf("blank element accepted: %v", err)
	}
	err := CheckRunHook(HookPreRun, []string{"/bin/x", "{{{{"})
	if !errors.Is(err, tmpl.ErrGrammar) {
		t.Errorf("grammar error not reported as one: %v", err)
	}
	if err := CheckRunHook(HookPreRun, nil); err != nil {
		t.Errorf("absent hook rejected: %v", err)
	}
}

func TestRenderRunHookIsOneArgumentPerElement(t *testing.T) {
	args, err := RenderRunHook([]string{"/bin/x", "a b", "{{run.id}}", "{{nope?}}"}, ctx(PathRunID, "42"))
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 4 || args[1] != "a b" || args[2] != "42" || args[3] != "" {
		t.Errorf("rendered %q", args)
	}
	if _, err := RenderRunHook([]string{"{{run.id}}"}, ctx()); !errors.Is(err, tmpl.ErrUnresolved) {
		t.Errorf("a missing required path rendered instead of failing: %v", err)
	}
}

func TestCheckWorkdirTemplate(t *testing.T) {
	for _, wd := range []string{
		"~/sweeps/handoff-m/runs/{{run.id}}",
		"~/sweeps/{{harness.name}}/runs/{{run.date}}",
		"~/src/{{harness.name}}",
	} {
		if err := CheckWorkdirTemplate(wd, false); err != nil {
			t.Errorf("%q rejected: %v", wd, err)
		}
	}
	bad := []struct{ wd, want string }{
		{"~/x/{{event.url}}", "an event path can never name a directory"},
		{"~/x/{{untrusted event.body}}", "untrusted text can never name a directory"},
		{"~/x/{{harness.workdir}}", "is the directory this template is deciding"},
		{"~/x/{{run.datadir}}", "is the directory this template is deciding"},
		{"~/x/{{run.outcome}}", "does not exist until the run has ended"},
		{"~/x/{{nope}}", `unknown template path "nope"`},
	}
	for _, tc := range bad {
		if err := CheckWorkdirTemplate(tc.wd, false); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q loaded; want %q (got %v)", tc.wd, tc.want, err)
		}
	}
}

func TestCheckWorkdirTemplateRetentionShape(t *testing.T) {
	if err := CheckWorkdirTemplate("~/sweeps/x/runs/{{run.id}}", true); err != nil {
		t.Errorf("the intended shape rejected: %v", err)
	}
	if err := CheckWorkdirTemplate("~/sweeps/x/runs/run-{{run.date}}-{{run.id}}", true); err != nil {
		t.Errorf("a last element with two placeholders rejected: %v", err)
	}
	for _, wd := range []string{"~/src/{{harness.name}}/runs/nightly", "{{run.id}}"} {
		if err := CheckWorkdirTemplate(wd, true); err == nil {
			t.Errorf("%q accepted with a retention cap", wd)
		}
	}
}

func TestRunDirGlob(t *testing.T) {
	parent, glob, ok := RunDirGlob("~/sweeps/x/runs/run-{{run.id}}", ctx())
	if !ok || filepath.Base(parent) != "runs" || glob != "run-*" {
		t.Errorf("RunDirGlob = %q, %q, %v; want .../runs, run-*, true", parent, glob, ok)
	}
	if _, _, ok := RunDirGlob("~/x/{{harness.name}}/runs/nightly", ctx()); ok {
		t.Error("a template that varies above its last element produced a glob")
	}
	if _, _, ok := RunDirGlob("{{run.id}}", ctx()); ok {
		t.Error("a bare placeholder produced a glob")
	}
}

func TestWorkdirHasTemplate(t *testing.T) {
	if WorkdirHasTemplate("~/src/app") || !WorkdirHasTemplate("~/src/{{harness.name}}") {
		t.Error("WorkdirHasTemplate disagrees with the template syntax")
	}
}
