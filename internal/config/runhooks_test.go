package config

// Run Lifecycle Hook And Workdir Keys
//
// Load-time rules for #980: a hook needs a harness whose every start is one
// run someone asked for, a templated workdir needs the same, and
// workdir_retain needs a template whose LAST element is the run, so what
// retention deletes can only ever be a run directory. Which template paths a
// hook may name is core's check (hooktmpl_test.go); these tests are about the
// keys' existence, their kinds, and their combination.
//
// Governing: ADR-0051 (proposed); stump.wtf/harness#980.

import (
	"strings"
	"testing"
)

// sweepTOML is a scheduled crush one-shot: the config shape the hooks are
// written for. `extra` is appended to its table.
func sweepTOML(extra string) string {
	return "[harness.sweep]\nharness = \"crush\"\nprompt = \"sweep\"\nschedule = \"0 6 * * *\"\n" + extra + "\n"
}

func TestRunHookKeysParse(t *testing.T) {
	cfg, err := Parse([]byte(sweepTOML(
		"workdir = \"~/sweeps/sweep/runs/{{run.id}}\"\nworkdir_retain = 5\n"+
			"pre_run = [\"/usr/bin/git\", \"clone\", \"{{harness.name}}\"]\n"+
			"post_run = [\"/usr/local/bin/check.sh\", \"{{run.outcome}}\", \"{{run.exit_code}}\"]")), "test.toml")
	if err != nil {
		t.Fatal(err)
	}
	h := cfg.Harnesses["sweep"]
	if len(h.PreRun) != 3 || h.PreRun[0] != "/usr/bin/git" {
		t.Errorf("pre_run = %q", h.PreRun)
	}
	if len(h.PostRun) != 3 || h.PostRun[1] != "{{run.outcome}}" {
		t.Errorf("post_run = %q", h.PostRun)
	}
	if h.WorkdirRetain != 5 {
		t.Errorf("workdir_retain = %d, want 5", h.WorkdirRetain)
	}
	if !strings.HasPrefix(h.Workdir, "~/sweeps/sweep/runs/") {
		t.Errorf("workdir = %q", h.Workdir)
	}
}

func TestRunHooksOnResidentRejected(t *testing.T) {
	resident := "[harness.worker]\nharness = \"generic\"\nargs = [\"-c\", \"sleep 1000\"]\n"
	for _, tc := range []struct{ key, want string }{
		{"pre_run", "requires a one-shot harness"},
		{"post_run", "requires a one-shot harness"},
	} {
		_, err := Parse([]byte(resident+tc.key+" = [\"/bin/true\"]\n"), "test.toml")
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s on a resident loaded; want %q (got %v)", tc.key, tc.want, err)
		}
	}
}

func TestRunHookValuesRejected(t *testing.T) {
	cases := []struct{ extra, want string }{
		{`pre_run = [""]`, `must not be blank`},
		{`pre_run = ["/bin/true", "{{run.outcome}}"]`, `unknown template path "run.outcome"`},
		{`pre_run = ["/bin/true", "{{run.exit_code}}"]`, `unknown template path "run.exit_code"`},
		{`post_run = ["/bin/true", "{{event.url}}"]`, "an event path is not available in post_run"},
		{`post_run = ["/bin/true", "{{event.title}}"]`, "untrusted text is never permitted in post_run"},
		{`pre_run = ["/bin/true", "{{nope}}"]`, `unknown template path "nope"`},
		{`pre_run = ["/bin/true", "{{{{"]`, "template"},
	}
	for _, tc := range cases {
		_, err := Parse([]byte(sweepTOML(tc.extra)), "test.toml")
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s loaded; want %q (got %v)", tc.extra, tc.want, err)
		}
	}
}

func TestPostRunMayNameTheVerdict(t *testing.T) {
	if _, err := Parse([]byte(sweepTOML(`post_run = ["/bin/check", "{{run.outcome}}", "{{run.exit_code?}}"]`)), "test.toml"); err != nil {
		t.Errorf("post_run verdict paths rejected: %v", err)
	}
}

func TestWorkdirRetainRules(t *testing.T) {
	cases := []struct{ extra, want string }{
		// A cap with nothing to prune: a plain workdir is shared, and pruning
		// it would mean deleting a directory every run still needs.
		{"workdir = \"~/src/sweep\"\nworkdir_retain = 4", "requires a templated \"workdir\""},
		{`workdir = "~/sweeps/sweep/runs/{{run.id}}"` + "\nworkdir_retain = 0", "must be at least 1"},
		// The varying element must be the last, or retention would point at
		// directories that predate the harness.
		{`workdir = "~/sweeps/{{harness.name}}/runs/nightly"` + "\nworkdir_retain = 4", "only the last path element may vary"},
		{`workdir = "{{run.id}}"` + "\nworkdir_retain = 4", "must sit below a fixed directory"},
		{`workdir = "~/sweeps/sweep/runs/{{event.url}}"` + "\nworkdir_retain = 4", "an event path can never name a directory"},
		{`workdir = "~/sweeps/sweep/runs/{{run.outcome}}"`, "does not exist until the run has ended"},
		{`workdir = "~/sweeps/sweep/runs/{{nope}}"`, `unknown template path "nope"`},
	}
	for _, tc := range cases {
		_, err := Parse([]byte(sweepTOML(tc.extra)), "test.toml")
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s loaded; want %q (got %v)", tc.extra, tc.want, err)
		}
	}
}

func TestTemplatedWorkdirOnResidentRejected(t *testing.T) {
	_, err := Parse([]byte("[harness.worker]\nharness = \"generic\"\nargs = [\"-c\", \"sleep 1000\"]\nworkdir = \"~/sweeps/worker/runs/{{run.id}}\"\n"), "test.toml")
	if err == nil || !strings.Contains(err.Error(), "requires a one-shot harness") {
		t.Errorf("templated workdir on a resident loaded; want a one-shot error (got %v)", err)
	}
}

func TestPlainWorkdirStillUnchanged(t *testing.T) {
	cfg, err := Parse([]byte(sweepTOML(`workdir = "~/src/sweep"`)), "test.toml")
	if err != nil {
		t.Fatal(err)
	}
	if h := cfg.Harnesses["sweep"]; h.Workdir != "~/src/sweep" || h.WorkdirRetain != 0 || h.PreRun != nil || h.PostRun != nil {
		t.Errorf("plain workdir harness changed shape: %q retain=%d hooks=%q/%q", h.Workdir, h.WorkdirRetain, h.PreRun, h.PostRun)
	}
}
