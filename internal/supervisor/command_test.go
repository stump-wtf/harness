package supervisor

// Command Harness Tests
//
// `harness = "command"` execs its argv directly: argv[0] is the executable and
// argv[1:] reach it byte for byte, with no shell anywhere. These tests run a
// real child — this test binary, re-entered through TestMain as an argv probe
// — so what they check is what the child actually received and who its parent
// was, not what execArgv returned.
//
// Governing: ADR-0023, SPEC-0017 REQ-2 "Command Harness Kind", REQ-14
// "Project And Wire Front Doors".

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
)

// argvProbeEnv, when set in a child's environment, turns this test binary into
// an argv probe: it writes what it was exec'd with to the named file and exits
// before any test runs.
const argvProbeEnv = "HARNESS_TEST_ARGV_PROBE"

// argvProbe is what the probe child records about itself.
type argvProbe struct {
	// Argv0 is os.Args[0], the path the child was exec'd as.
	Argv0 string `json:"argv0"`
	// Args is os.Args[1:], exactly as the kernel handed them over.
	Args []string `json:"args"`
	// PPID is the child's parent. It equals the test process's pid only when
	// the daemon exec'd the child itself; a shell in between would be the
	// parent instead (or would fork the probe as its grandchild).
	PPID int `json:"ppid"`
}

func TestMain(m *testing.M) {
	if out := os.Getenv(argvProbeEnv); out != "" {
		os.Exit(writeArgvProbe(out))
	}
	os.Exit(m.Run())
}

// writeArgvProbe records the probe and reports the exit code. It writes to a
// temporary name and renames, so a reader never sees a half-written record.
func writeArgvProbe(out string) int {
	b, err := json.Marshal(argvProbe{Argv0: os.Args[0], Args: os.Args[1:], PPID: os.Getppid()})
	if err != nil {
		return 2
	}
	if err := os.WriteFile(out+".tmp", b, 0o600); err != nil {
		return 2
	}
	if err := os.Rename(out+".tmp", out); err != nil {
		return 2
	}
	return 0
}

// probeHarness builds a command harness whose argv[0] is argv0 and which runs
// as an argv probe writing to the returned path. The probe variable reaches
// the child through env_file, not the test process's own environment, so no
// other test's child can pick it up.
//
// TERM=dumb is load-bearing for speed, not correctness: the probe is this whole
// test binary, whose terminal-styling dependencies query the terminal at
// startup when stdout is a TTY, and spawn gives every child a PTY that nobody
// answers. Each query then waits out its timeout, about ten seconds a spawn.
func probeHarness(t *testing.T, name, argv0 string, args ...string) (core.Harness, string) {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "probe.json")
	envFile := filepath.Join(dir, "probe.env")
	if err := os.WriteFile(envFile, []byte(argvProbeEnv+"="+out+"\nTERM=dumb\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return core.Harness{
		Name:     name,
		Adapter:  core.AdapterCommand,
		Argv:     append([]string{argv0}, args...),
		EnvFiles: []string{envFile},
		Backend:  core.BackendNative,
		Restart:  core.RestartNo,
	}, out
}

// testBinary is the running test binary, the probe's executable.
func testBinary(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

// readProbe waits for the probe record at out and decodes it.
func readProbe(t *testing.T, out string) argvProbe {
	t.Helper()
	var p argvProbe
	waitFor(t, 10*time.Second, "argv probe record", func() bool {
		b, err := os.ReadFile(out)
		return err == nil && json.Unmarshal(b, &p) == nil
	})
	return p
}

// spawnAndReap spawns h and waits for it to exit, closing its PTY.
func spawnAndReap(t *testing.T, h core.Harness) {
	t.Helper()
	proc, err := spawn(h, 80, 24, RunEnv{})
	if err != nil {
		t.Fatalf("spawn %q: %v", h.Name, err)
	}
	_ = proc.cmd.Wait()
	_ = proc.pty.Close()
}

// TestSpawnCommandExecsWithoutShell is the scenario "Exec without a shell".
// The four arguments are the spec's, except that the last one is a harmless
// `touch` of a marker instead of `rm -rf /`: were a shell ever involved, `;`
// would end the command, `$(id)` would expand, and the marker would appear.
// The child must receive exactly four arguments, byte-identical, and its
// parent must be this process — the daemon exec'd it, not some `sh`.
func TestSpawnCommandExecsWithoutShell(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "shell-ran")
	want := []string{"a b", "$(id)", ";", "touch " + marker}
	h, out := probeHarness(t, "noshell", testBinary(t), want...)

	spawnAndReap(t, h)
	p := readProbe(t, out)

	if !slices.Equal(p.Args, want) {
		t.Errorf("child received %d args %q, want exactly %d byte-identical args %q", len(p.Args), p.Args, len(want), want)
	}
	if p.PPID != os.Getpid() {
		t.Errorf("child's parent is pid %d, not the daemon (%d): something exec'd it on the daemon's behalf", p.PPID, os.Getpid())
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the marker a shell would create exists (stat: %v): the argv went through a shell", err)
	}
}

// TestSpawnCommandRelativeArgv0ResolvesAgainstWorkdir: a relative argv[0]
// with a path separator names a file under the harness's workdir, not under
// whatever directory the daemon runs in. The test's own cwd has no bin/probe,
// so the child running at all shows which directory was used.
func TestSpawnCommandRelativeArgv0ResolvesAgainstWorkdir(t *testing.T) {
	workdir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workdir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(testBinary(t), filepath.Join(workdir, "bin", "probe")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join("bin", "probe")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the test's cwd has a bin/probe (stat: %v), so this test cannot tell the two directories apart", err)
	}
	h, out := probeHarness(t, "relative", filepath.Join("bin", "probe"), "x")
	h.Workdir = workdir

	spawnAndReap(t, h)
	p := readProbe(t, out)

	if want := filepath.Join(workdir, "bin", "probe"); p.Argv0 != want {
		t.Errorf("child exec'd as %q, want %q", p.Argv0, want)
	}
	if !slices.Equal(p.Args, []string{"x"}) {
		t.Errorf("child args = %q, want [x]", p.Args)
	}
}

// TestExecArgvCommandIsVerbatim: the argv spawn gets is the configured one.
// A bare argv[0] is left for the PATH lookup, an absolute one is untouched,
// and argv[1:] get no {workdir} expansion — that is `args` behaviour, and a
// command harness's arguments are passed through exactly. The returned slice
// is a copy, so nothing downstream can edit the registered definition.
func TestExecArgvCommandIsVerbatim(t *testing.T) {
	for _, tc := range []struct {
		name     string
		argv     []string
		wantName string
	}{
		{"bare name", []string{"report", "{workdir}/out", ""}, "report"},
		{"absolute", []string{"/usr/local/bin/report", "--run", "a b"}, "/usr/local/bin/report"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := core.Harness{Name: "r", Adapter: core.AdapterCommand, Argv: slices.Clone(tc.argv)}
			name, args := mustExecArgv(t, h, "/home/x")
			if name != tc.wantName {
				t.Errorf("cmd = %q, want %q", name, tc.wantName)
			}
			if !slices.Equal(args, tc.argv[1:]) {
				t.Errorf("args = %q, want %q verbatim", args, tc.argv[1:])
			}
			if len(args) > 0 {
				args[0] = "mutated"
				if h.Argv[1] == "mutated" {
					t.Error("execArgv returned an alias of the harness's Argv")
				}
			}
		})
	}
}

// TestSpawnRefusesMalformedCommand is the spawn backstop: a command harness
// that bypassed validation with a prompt, an empty argv or a placeholder
// executable fails the start with an error naming it, and nothing is exec'd.
// The probe binary is argv[0] (or would be), so its record appearing is the
// evidence of an exec.
func TestSpawnRefusesMalformedCommand(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*core.Harness)
		want   error
	}{
		{"prompt", func(h *core.Harness) { h.Prompt = "triage the queue" }, ErrCommandPrompt},
		{"empty argv", func(h *core.Harness) { h.Argv = nil }, nil},
		{"placeholder argv0", func(h *core.Harness) { h.Argv[0] = "{{event.repo}}" }, nil},
		{"placeholder argv1", func(h *core.Harness) { h.Argv = append(h.Argv, "{{run.id}}") }, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, out := probeHarness(t, "bad-"+strings.ReplaceAll(tc.name, " ", "-"), testBinary(t))
			tc.mutate(&h)
			proc, err := spawn(h, 80, 24, RunEnv{})
			if proc != nil {
				_ = proc.cmd.Wait()
				_ = proc.pty.Close()
				t.Fatalf("spawn started %q; want a refusal", proc.cmd.Path)
			}
			if err == nil {
				t.Fatal("spawn returned no error and no process")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
			if !strings.Contains(err.Error(), `"`+h.Name+`"`) {
				t.Errorf("error %q does not name the harness %q", err, h.Name)
			}
			time.Sleep(50 * time.Millisecond)
			if _, statErr := os.Stat(out); !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("the probe ran (stat: %v)", statErr)
			}
		})
	}
}

// TestProjectUpCommandHarnessExecsWithoutShell is REQ-14's "Command harness in
// a project": the wire registers a resident command harness, the manager
// starts it, and the child it starts is the configured argv, exec'd directly.
func TestProjectUpCommandHarnessExecsWithoutShell(t *testing.T) {
	m := newTestManager(t, managerCfg(shHarness("global", loopScript, 0)))
	want := []string{"a b", "$(id)"}
	h, out := probeHarness(t, "report", testBinary(t), want...)
	h.Enabled = true

	res, err := m.ProjectUp("reduit", []core.Harness{h})
	if err != nil {
		t.Fatalf("ProjectUp: %v", err)
	}
	if len(res.Names) != 1 || res.Names[0] != "reduit/report" {
		t.Fatalf("registered %v, want [reduit/report]", res.Names)
	}
	got, _, ok := m.HarnessRecord("reduit/report")
	if !ok || got.Adapter != core.AdapterCommand || !slices.Equal(got.Argv, h.Argv) {
		t.Fatalf("HarnessRecord = %+v ok=%v, want the command harness with its argv", got, ok)
	}

	p := readProbe(t, out)
	if !slices.Equal(p.Args, want) {
		t.Errorf("child received %q, want %q", p.Args, want)
	}
	if p.PPID != os.Getpid() {
		t.Errorf("child's parent is pid %d, not the daemon (%d)", p.PPID, os.Getpid())
	}
}

// TestProjectUpRejectsInvalidCommandDefs: the wire re-validates a command
// definition by the config parser's rules, including the scenario "The wire
// re-validates templates" (a placeholder argv[0]), and registers nothing.
func TestProjectUpRejectsInvalidCommandDefs(t *testing.T) {
	cmd := func(mut func(*core.Harness)) []core.Harness {
		h := core.Harness{Name: "report", Adapter: core.AdapterCommand, Argv: []string{"/bin/echo", "hi"}, Backend: core.BackendNative}
		mut(&h)
		return []core.Harness{h}
	}
	for _, tc := range []struct {
		name, want string
		defs       []core.Harness
	}{
		{"placeholder argv0", `"argv[0]"`, cmd(func(h *core.Harness) { h.Argv[0] = "{{event.repo}}" })},
		{"placeholder argv1", `"argv[1]"`, cmd(func(h *core.Harness) { h.Argv[1] = "{{run.id}}" })},
		{"blank argv0", `"argv[0]"`, cmd(func(h *core.Harness) { h.Argv[0] = " " })},
		{"missing argv", `requires "argv"`, cmd(func(h *core.Harness) { h.Argv = nil })},
		{"args on command", "argv", cmd(func(h *core.Harness) { h.Args = []string{"-c", "true"} })},
		{"prompt on command", "prompt", cmd(func(h *core.Harness) { h.Prompt = "hi" })},
		{"prompt_file on command", "prompt", cmd(func(h *core.Harness) { h.PromptFile = "/tmp/p.md" })},
		{"model on command", "model", cmd(func(h *core.Harness) { h.Model = "x/y" })},
		{"auto_accept on command", "auto_accept", cmd(func(h *core.Harness) { h.AutoAccept = true })},
		{"max_turns on command", "max_turns", cmd(func(h *core.Harness) { h.MaxTurns = 3 })},
		{"argv on claude-code", "argv", []core.Harness{{
			Name: "report", Adapter: "claude-code", Argv: []string{"claude"}, Backend: core.BackendNative,
		}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestManager(t, managerCfg(shHarness("global", loopScript, 0)))
			_, err := m.ProjectUp("reduit", tc.defs)
			if !errors.Is(err, ErrInvalidProjectDef) {
				t.Fatalf("ProjectUp err = %v, want ErrInvalidProjectDef", err)
			}
			for _, sub := range []string{`harness "report"`, tc.want} {
				if !strings.Contains(err.Error(), sub) {
					t.Errorf("error %q does not contain %q", err, sub)
				}
			}
			if got := snapshotNames(m); len(got) != 1 || got[0] != "global" {
				t.Errorf("an invalid command def registered something: %v", got)
			}
		})
	}
}

// TestScratchRunRefusesPlaceholderArgv0: the scratchpad is the other wire
// door, and applies the same rule.
func TestScratchRunRefusesPlaceholderArgv0(t *testing.T) {
	m := newTestManager(t, managerCfg())
	_, err := m.ScratchRun(core.Harness{Adapter: core.AdapterCommand, Argv: []string{"{{event.repo}}", "x"}}, "cmd")
	if !errors.Is(err, ErrInvalidProjectDef) {
		t.Fatalf("ScratchRun err = %v, want ErrInvalidProjectDef", err)
	}
	if got := snapshotNames(m); len(got) != 0 {
		t.Errorf("a refused scratchpad registered something: %v", got)
	}
}

// TestCommandArgvRoundTripsAndCounts: the state file keeps a project command
// harness's argv, and argv is part of both the re-up equality and the
// "needs a restart" test — a changed argv that compared equal would leave the
// old process running the old command with no config-changed flag.
func TestCommandArgvRoundTripsAndCounts(t *testing.T) {
	h := core.Harness{Name: "reduit/report", Adapter: core.AdapterCommand, Argv: []string{"/bin/echo", "a b", "{{x"},
		Backend: core.BackendNative, Restart: core.RestartAlways, Quiet: true}
	back := toPersistedProjectHarness(h).toCore()
	if !slices.Equal(back.Argv, h.Argv) {
		t.Errorf("persisted argv = %q, want %q", back.Argv, h.Argv)
	}
	if !harnessDefEqual(h, back) {
		t.Errorf("round-tripped definition differs:\n got %+v\nwant %+v", back, h)
	}
	changed := h
	changed.Argv = []string{"/bin/echo", "a", "b", "{{x"}
	if harnessDefEqual(h, changed) {
		t.Error("harnessDefEqual ignores argv")
	}
	if !runAffecting(h, changed) {
		t.Error("runAffecting ignores argv")
	}
}
