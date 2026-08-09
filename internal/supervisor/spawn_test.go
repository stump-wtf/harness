package supervisor

// Governing tests: ADR-0005 (spawn under a PTY in workdir with env_file
// loaded, {workdir} arg expansion); ADR-0008 (env_file is where secrets stay).

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
)

func TestParseEnvFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "env")
	content := "" +
		"# a comment\n" +
		"\n" +
		"FOO=bar\n" +
		"export BAZ=qux\n" +
		"QUOTED=\"has spaces\"\n" +
		"SINGLE='single quoted'\n" +
		"NOEQUALS\n" +
		"EMPTY=\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := parseEnvFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"FOO=bar":              "",
		"BAZ=qux":              "",
		"QUOTED=has spaces":    "",
		"SINGLE=single quoted": "",
		"EMPTY=":               "",
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %d entries %v, want %d", len(got), got, len(want))
	}
	for _, kv := range got {
		if _, ok := want[kv]; !ok {
			t.Errorf("unexpected entry %q", kv)
		}
	}
}

func TestParseEnvFileMissingIsNotError(t *testing.T) {
	got, err := parseEnvFile(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("missing env_file should not error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
	if g, err := parseEnvFile(""); err != nil || g != nil {
		t.Fatalf("empty path: got %v err %v", g, err)
	}
}

func TestExpandArgsWorkdir(t *testing.T) {
	got := expandArgs([]string{"--data-dir", "{workdir}", "--flag"}, "/home/x")
	want := []string{"--data-dir", "/home/x", "--flag"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("arg %d = %q, want %q", i, got[i], want[i])
		}
	}
	if expandArgs(nil, "/x") != nil {
		t.Error("nil args should expand to nil")
	}
}

// TestExecArgvPromptSynthesis pins the ADR-0011 spawn-time synthesis: a
// prompt harness (empty Cmd) execs core.AgentCommand's argv with the whole
// multi-word prompt as ONE argv element, and the prompt text is exempt from
// the {workdir} placeholder expansion configured args go through.
func TestExecArgvPromptSynthesis(t *testing.T) {
	h := core.Harness{Name: "agent", Prompt: "check the deployments in {workdir} please"}
	name, args := execArgv(h, "/home/x")
	if name != "crush" {
		t.Errorf("cmd = %q, want crush", name)
	}
	want := []string{"run", "--quiet", "check the deployments in {workdir} please"}
	if !slices.Equal(args, want) {
		t.Errorf("args = %q, want %q (prompt must stay one argv element, unexpanded)", args, want)
	}
}

// TestExecArgvPromptModelSynthesis pins the model flag's spawn-time placement
// (issue #57): --model rides the synthesized agent argv BEFORE the prompt —
// the prompt stays the final argv element — and the model value, like the
// prompt, is exempt from the {workdir} placeholder expansion configured args
// go through.
func TestExecArgvPromptModelSynthesis(t *testing.T) {
	h := core.Harness{Name: "agent", Prompt: "check {workdir} please", Model: "claude-{workdir}"}
	name, args := execArgv(h, "/home/x")
	if name != "crush" {
		t.Errorf("cmd = %q, want crush", name)
	}
	want := []string{"run", "--quiet", "--model", "claude-{workdir}", "check {workdir} please"}
	if !slices.Equal(args, want) {
		t.Errorf("args = %q, want %q (model before prompt, both unexpanded)", args, want)
	}
}

// TestExecArgvCmdIgnoresModel: the cmd path never injects a model flag — there
// is no vendor-agnostic injection point in an arbitrary argv, so config
// validation forbids cmd+model, and a wire def carrying both spawns on its
// configured argv alone.
func TestExecArgvCmdIgnoresModel(t *testing.T) {
	h := core.Harness{Name: "svc", Cmd: "run", Args: []string{"--dir", "{workdir}"}, Model: "claude-opus-5"}
	name, args := execArgv(h, "/home/x")
	if name != "run" {
		t.Errorf("cmd = %q, want run", name)
	}
	if want := []string{"--dir", "/home/x"}; !slices.Equal(args, want) {
		t.Errorf("args = %q, want %q (no synthesized --model on the cmd path)", args, want)
	}
}

// TestExecArgvCmdUnchanged: a cmd harness keeps its configured argv, with
// {workdir} expansion — synthesis only engages when Cmd is empty.
func TestExecArgvCmdUnchanged(t *testing.T) {
	h := core.Harness{Name: "svc", Cmd: "run", Args: []string{"--dir", "{workdir}"}}
	name, args := execArgv(h, "/home/x")
	if name != "run" {
		t.Errorf("cmd = %q, want run", name)
	}
	if want := []string{"--dir", "/home/x"}; !slices.Equal(args, want) {
		t.Errorf("args = %q, want %q", args, want)
	}
}

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	if got := expandHome("~/foo"); got != filepath.Join(home, "foo") {
		t.Errorf("expandHome(~/foo) = %q", got)
	}
	if got := expandHome("~"); got != home {
		t.Errorf("expandHome(~) = %q", got)
	}
	if got := expandHome("/abs/path"); got != "/abs/path" {
		t.Errorf("absolute path changed: %q", got)
	}
}

// lookupEnv returns the value of the LAST KEY= entry (exec.Cmd.Env semantics:
// later duplicates shadow earlier ones), plus whether the key appeared at all.
func lookupEnv(env []string, key string) (string, bool) {
	val, found := "", false
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			val, found = v, true
		}
	}
	return val, found
}

// TestEnsureTermEnvGuaranteesColor pins the color fix: a harness spawned under
// a real PTY must advertise a color-capable terminal. A daemon launched
// detached (launchd/systemd, a closed login shell) can carry a missing or
// "dumb" TERM and no COLORTERM, which made harness apps render in black &
// white. ensureTermEnv repairs exactly that case.
func TestEnsureTermEnvGuaranteesColor(t *testing.T) {
	// Colorless daemon env: no TERM, no COLORTERM → both are supplied.
	got := ensureTermEnv([]string{"PATH=/usr/bin"})
	if v, ok := lookupEnv(got, "TERM"); !ok || v != "xterm-256color" {
		t.Errorf("TERM = %q (present %v), want xterm-256color", v, ok)
	}
	if v, ok := lookupEnv(got, "COLORTERM"); !ok || v != "truecolor" {
		t.Errorf("COLORTERM = %q (present %v), want truecolor", v, ok)
	}

	// A "dumb" TERM advertises no color; it is replaced, not kept.
	got = ensureTermEnv([]string{"TERM=dumb"})
	if v, _ := lookupEnv(got, "TERM"); v != "xterm-256color" {
		t.Errorf("dumb TERM should be repaired, got TERM=%q", v)
	}
}

// TestEnsureTermEnvPreservesExisting confirms existing terminal values win:
// a deliberate TERM/COLORTERM (from the daemon env or the env_file) is never
// clobbered by the generic defaults.
func TestEnsureTermEnvPreservesExisting(t *testing.T) {
	got := ensureTermEnv([]string{"TERM=xterm-kitty", "COLORTERM=yes"})
	if v, _ := lookupEnv(got, "TERM"); v != "xterm-kitty" {
		t.Errorf("existing TERM should win, got %q", v)
	}
	if v, _ := lookupEnv(got, "COLORTERM"); v != "yes" {
		t.Errorf("existing COLORTERM should win, got %q", v)
	}
}
