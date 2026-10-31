package supervisor

// OpenCode Spawn And Observation Tests
//
// Governing tests: issue #919's acceptance criteria — `harness = "opencode"`
// loads, spawns and is observed, with the spawn driven for real: the fake
// `opencode` first on PATH records the argv it received and writes a session
// database where the CLI would write one, so what is checked is what ran and
// what the session reader then finds. A `command` harness bound with
// `transcripts = "opencode"` is observed as opencode while execing its own
// argv.

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/stump-wtf/harness/internal/config"
	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/runtrace"
	"github.com/stump-wtf/harness/internal/testwait"
)

// fakeOpencodeFixtureEnv names the recorded session database the fake CLI
// copies into place; fakeOpencodeArgvEnv names where it records its argv.
const (
	fakeOpencodeFixtureEnv = "HARNESS_TEST_OPENCODE_FIXTURE"
	fakeOpencodeArgvEnv    = "HARNESS_TEST_OPENCODE_ARGV"
)

// fakeOpencodeOnPath puts a fake `opencode` first on PATH. It records the
// argv it was exec'd with, NUL-terminated, and then writes the fixture
// session database to the path the real CLI uses, under HOME.
func fakeOpencodeOnPath(t *testing.T) (argvFile string) {
	t.Helper()
	dir := t.TempDir()
	argvFile = filepath.Join(dir, "opencode.argv")
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do printf '%s\\0' \"$a\"; done > \"" + argvFile + "\"\n" +
		"mkdir -p \"$HOME/.opencode\"\n" +
		"cp \"$" + fakeOpencodeFixtureEnv + "\" \"$HOME/.opencode/opencode.db\"\n"
	if err := os.WriteFile(filepath.Join(dir, "opencode"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argvFile
}

// waitOpencodeArgv returns the argv the fake CLI received, failing once the
// budget runs out.
func waitOpencodeArgv(t *testing.T, argvFile string) []string {
	t.Helper()
	deadline := time.Now().Add(testwait.Budget(t, 5*time.Second))
	for {
		raw, err := os.ReadFile(argvFile)
		if err == nil && len(raw) > 0 {
			return strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
		}
		if time.Now().After(deadline) {
			t.Fatalf("the fake opencode never ran (err %v)", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestOpenCodeOneShotSpawnsRunArgv(t *testing.T) {
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	argvFile := fakeOpencodeOnPath(t)
	t.Setenv(fakeOpencodeFixtureEnv, filepath.Join(dir, "unused.db"))
	cfg, err := config.Parse([]byte(`
[harness.oc-fix]
harness = "opencode"
model = "anthropic/claude-opus-5"
prompt = "fix the build"
workdir = "`+work+`"
`), "harness.toml")
	if err != nil {
		t.Fatalf("config does not load: %v", err)
	}
	h := cfg.Harnesses[cfg.HarnessOrder[0]]
	proc, err := spawn(&admission{}, h, 80, 24, RunEnv{})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	_ = proc.cmd.Wait()
	_ = proc.pty.Close()
	want := []string{"run", "--model", "anthropic/claude-opus-5", "fix the build"}
	if got := waitOpencodeArgv(t, argvFile); !slices.Equal(got, want) {
		t.Fatalf("opencode received %q, want %q", got, want)
	}
}

// A resident opencode harness runs `opencode` with its args appended, like any
// other adapter kind.
func TestOpenCodeResidentRunsExecutableWithArgs(t *testing.T) {
	got := spawnedArgv(t, "opencode", `
[harness.oc-resident]
harness = "opencode"
args = ["serve", "--port 4096"]
`)
	if want := []string{"serve", "--port 4096"}; !slices.Equal(got, want) {
		t.Fatalf("opencode received %q, want %q", got, want)
	}
}

// The whole path: the config loads, the process runs, and the session the
// fake CLI recorded is then discovered and attributed to this harness.
func TestOpenCodeOneShotIsObserved(t *testing.T) {
	home := t.TempDir()
	work := filepath.Join(home, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join(t.TempDir(), "recorded.db")
	start := time.Now().Add(-100 * time.Millisecond)
	writeOpencodeSessionDB(t, fixture, work, "ses_run1", start)

	t.Setenv("HOME", home)
	t.Setenv(fakeOpencodeFixtureEnv, fixture)
	argvFile := fakeOpencodeOnPath(t)

	cfg, err := config.Parse([]byte(`
[harness.oc-observed]
harness = "opencode"
prompt = "fix the build"
workdir = "`+work+`"
`), "harness.toml")
	if err != nil {
		t.Fatalf("config does not load: %v", err)
	}
	h := cfg.Harnesses[cfg.HarnessOrder[0]]
	started := time.Now()
	proc, err := spawn(&admission{}, h, 80, 24, RunEnv{})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	_ = proc.cmd.Wait()
	_ = proc.pty.Close()
	waitOpencodeArgv(t, argvFile)

	env, err := DiscoveryEnv(h, runtrace.DiscoveryEnvKeys)
	if err != nil {
		t.Fatalf("DiscoveryEnv: %v", err)
	}
	scope := runtrace.Scope{Name: h.Name, Adapter: h.TrajectoryKind(), Workdir: work, Env: env,
		Runs: []runtrace.Window{{Start: started}}}
	attr, err := runtrace.Attribute(context.Background(), scope, runtrace.Window{Start: started}, nil, time.Now())
	if err != nil {
		t.Fatalf("Attribute: %v", err)
	}
	if len(attr.Sessions) != 1 || attr.Sessions[0].Meta.ID != "ses_run1" {
		t.Fatalf("sessions = %+v, want the session the run recorded", attr.Sessions)
	}
	if attr.Sessions[0].Meta.Harness != "opencode" {
		t.Errorf("session harness = %q, want opencode", attr.Sessions[0].Meta.Harness)
	}
}

// A command harness bound with transcripts = "opencode" is observed as
// opencode while its own argv is what runs.
func TestProjectUpOpenCodeTranscriptsBinding(t *testing.T) {
	m := newTestManager(t, managerCfg(shHarness("global", loopScript, 0)))
	defs := []core.Harness{
		{Name: "oc", Adapter: core.AdapterOpenCode, Prompt: "fix the build", Backend: core.BackendNative, Quiet: true},
		{Name: "oc-by-hand", Adapter: core.AdapterCommand, Argv: []string{"opencode", "--pure"},
			Transcripts: core.AdapterOpenCode, Backend: core.BackendNative},
	}
	if _, err := m.ProjectUp("agents", defs); err != nil {
		t.Fatalf("ProjectUp: %v", err)
	}
	got, _, ok := m.HarnessRecord("agents/oc-by-hand")
	if !ok || got.TrajectoryKind() != core.AdapterOpenCode {
		t.Fatalf("agents/oc-by-hand = %+v ok=%v, want the opencode binding kept", got, ok)
	}
}

// writeOpencodeSessionDB records one root session in the schema agent-trace's
// OpenCode reader queries, with the working directory the CLI would record.
// Timestamps are Unix milliseconds.
func writeOpencodeSessionDB(t *testing.T, path, cwd, id string, at time.Time) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	for _, stmt := range []string{
		`CREATE TABLE session (id TEXT PRIMARY KEY, title TEXT, directory TEXT, parent_id TEXT,
		                     model TEXT, time_created INTEGER, time_updated INTEGER)`,
		`CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT, data TEXT, time_created INTEGER)`,
		`CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT, session_id TEXT, data TEXT, time_created INTEGER)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	ms := at.UnixMilli()
	if _, err := db.Exec(`INSERT INTO session VALUES (?, 'work', ?, NULL, ?, ?, ?)`,
		id, cwd, `{"id":"anthropic/claude-opus-5","providerID":"anthropic"}`, ms, ms+1000); err != nil {
		t.Fatalf("session row: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO message VALUES ('msg_1', ?, '{"role":"user"}', ?)`, id, ms); err != nil {
		t.Fatalf("user message: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO part VALUES ('part_1', 'msg_1', ?, ?, ?)`, id,
		`{"type":"tool","tool":"bash","callID":"call_1","state":{"status":"completed","input":{"command":"ls"}}}`, ms+200); err != nil {
		t.Fatalf("tool part: %v", err)
	}
}
