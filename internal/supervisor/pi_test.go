package supervisor

// Pi And OMP Spawn Tests
//
// Governing tests: SPEC-0017 REQ-13 "Pi And OMP Adapters", scenarios "OMP
// one-shot argv" and "auto_accept is inert". The argv is read from inside the
// spawned child, not from execArgv's return value: a fake `omp`/`pi` first on
// PATH writes the arguments it actually received, so the test checks what ran.

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/config"
	"github.com/stump-wtf/harness/internal/core"
)

// fakeAgentOnPath puts an executable named exe first on PATH that writes each
// argument it receives, NUL-terminated, to the returned file.
func fakeAgentOnPath(t *testing.T, exe string) (argvFile string) {
	t.Helper()
	dir := t.TempDir()
	argvFile = filepath.Join(dir, exe+".argv")
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\0' \"$a\"; done > '" + argvFile + "'\n"
	if err := os.WriteFile(filepath.Join(dir, exe), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argvFile
}

// spawnedArgv parses toml, spawns its one harness and returns the argv the
// child received.
func spawnedArgv(t *testing.T, exe, toml string) []string {
	t.Helper()
	argvFile := fakeAgentOnPath(t, exe)
	cfg, err := config.Parse([]byte(toml), "harness.toml")
	if err != nil {
		t.Fatalf("config does not load: %v\n%s", err, toml)
	}
	h := cfg.Harnesses[cfg.HarnessOrder[0]]
	proc, err := spawn(h, 80, 24, RunEnv{})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	_ = proc.cmd.Wait()
	_ = proc.pty.Close()
	var raw []byte
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if raw, err = os.ReadFile(argvFile); err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("the fake %s never ran: %v", exe, err)
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
}

func TestOMPOneShotArgv(t *testing.T) {
	got := spawnedArgv(t, "omp", `
[harness.omp-fix]
harness = "omp"
model = "openrouter/z-ai/glm-5.3-flash"
prompt = "fix the build"
`)
	want := []string{"--print", "--model", "openrouter/z-ai/glm-5.3-flash", "fix the build"}
	if !slices.Equal(got, want) {
		t.Fatalf("omp received %q, want %q", got, want)
	}
}

// auto_accept (and max_turns, and quiet = false) load on a pi harness and put
// nothing extra on the argv.
func TestPiAutoAcceptIsInert(t *testing.T) {
	got := spawnedArgv(t, "pi", `
[harness.pi-fix]
harness = "pi"
prompt = "fix the build"
auto_accept = true
max_turns = 40
quiet = false
`)
	if want := []string{"--print", "fix the build"}; !slices.Equal(got, want) {
		t.Fatalf("pi received %q, want %q", got, want)
	}
}

// A resident pi harness runs `pi` with its args appended, like any adapter.
func TestPiResidentRunsExecutableWithArgs(t *testing.T) {
	got := spawnedArgv(t, "pi", `
[harness.pi-resident]
harness = "pi"
args = ["--continue", "a b"]
`)
	if want := []string{"--continue", "a b"}; !slices.Equal(got, want) {
		t.Fatalf("pi received %q, want %q", got, want)
	}
}

// The wire is the second front door (SPEC-0017 REQ-14): project_up and the
// scratchpad accept the pi and omp kinds and a command harness's transcripts,
// and refuse transcripts where the config parser does, registering nothing.
func TestProjectUpPiOMPAndTranscripts(t *testing.T) {
	m := newTestManager(t, managerCfg(shHarness("global", loopScript, 0)))
	defs := []core.Harness{
		{Name: "pi", Adapter: core.AdapterPi, Prompt: "fix the build", Backend: core.BackendNative, Quiet: true},
		{Name: "omp", Adapter: core.AdapterOMP, Args: []string{"--continue"}, Backend: core.BackendNative},
		{Name: "hand", Adapter: core.AdapterCommand, Argv: []string{"claude", "-p", "x"}, Transcripts: "claude-code", Backend: core.BackendNative},
	}
	if _, err := m.ProjectUp("agents", defs); err != nil {
		t.Fatalf("ProjectUp: %v", err)
	}
	got, _, ok := m.HarnessRecord("agents/hand")
	if !ok || got.Transcripts != "claude-code" || got.TrajectoryKind() != "claude-code" {
		t.Fatalf("agents/hand = %+v ok=%v, want the claude-code binding kept", got, ok)
	}

	for _, tc := range []struct {
		name, want string
		h          core.Harness
	}{
		{"unknown source", `unknown "transcripts" source "gemini"`,
			core.Harness{Name: "bad", Adapter: core.AdapterCommand, Argv: []string{"x"}, Transcripts: "gemini", Backend: core.BackendNative}},
		{"on an adapter kind", `only accepted on harness = "command"`,
			core.Harness{Name: "bad", Adapter: "crush", Prompt: "x", Transcripts: "crush", Backend: core.BackendNative}},
		{"unknown kind", `unknown harness kind "gemini"`,
			core.Harness{Name: "bad", Adapter: "gemini", Backend: core.BackendNative}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := snapshotNames(m)
			_, err := m.ProjectUp("rejects", []core.Harness{tc.h})
			if !errors.Is(err, ErrInvalidProjectDef) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ProjectUp err = %v, want ErrInvalidProjectDef containing %q", err, tc.want)
			}
			if after := snapshotNames(m); !slices.Equal(before, after) {
				t.Errorf("a refused definition registered something: %v -> %v", before, after)
			}
		})
	}
}

// The state file keeps the binding, and a changed binding is a changed
// definition (a re-up applies it) but not a run-affecting one: the process
// execs the same argv either way.
func TestTranscriptsRoundTripsAndCounts(t *testing.T) {
	h := core.Harness{Name: "agents/hand", Adapter: core.AdapterCommand, Argv: []string{"claude", "-p", "x"},
		Transcripts: "claude-code", Backend: core.BackendNative, Restart: core.RestartAlways, Quiet: true}
	back := toPersistedProjectHarness(h).toCore()
	if back.Transcripts != "claude-code" || !harnessDefEqual(h, back) {
		t.Errorf("persisted round-trip = %+v, want %+v", back, h)
	}
	changed := h
	changed.Transcripts = "pi"
	if harnessDefEqual(h, changed) {
		t.Error("harnessDefEqual ignores transcripts: a re-up changing the binding would be dropped")
	}
	if runAffecting(h, changed) {
		t.Error("runAffecting counts transcripts: rebinding would restart a process whose argv did not change")
	}
}
