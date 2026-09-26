package supervisor

// Session Guard Wiring Tests
//
// These drive the guard the way cmd/harness/daemon.go does — a real Manager,
// NewSessionGuard, Start — against harnesses configured the way production
// crush harnesses are: with `--data-dir` in args, so the store is NOT under
// the working directory. The guard's detector and archiver have their own unit
// tests in internal/sessionguard; what those cannot see is which store the
// daemon hands them, and that is where the guard went blind: every production
// crush harness on tars passes --data-dir, and the guard only ever looked at
// <workdir>/.crush/crush.db, so it skipped all of them while crush-qwen and
// crush-qwen-2 failed 1,070 turns on context-limit errors over 13 hours.
//
// Governing: issue #347 (session guard).

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/stump-wtf/harness/internal/core"
)

// guardCtxErr is a crush assistant turn that failed on the provider's context
// limit, in the litellm shape observed on tars.
const guardCtxErr = `["{\"type\":\"finish\",\"data\":{\"reason\":\"error\",\"message\":\"litellm.ContextWindowExceededError: maximum context length is 196608 tokens\"}}"]`

// guardOKTurn is a healthy assistant turn.
const guardOKTurn = `["{\"type\":\"text\",\"text\":\"done\"}"]`

// crushStore builds a crush-shaped store at dir/crush.db holding one
// assistant turn per entry of parts, all inside the guard's lookback, and
// returns the database path.
func crushStore(t *testing.T, dir string, parts ...string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "crush.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE messages (id INTEGER PRIMARY KEY, session_id TEXT, role TEXT, parts TEXT NOT NULL DEFAULT '[]', model TEXT, created_at INTEGER, updated_at INTEGER)`); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-time.Minute).Unix()
	for _, p := range parts {
		if _, err := db.Exec(`INSERT INTO messages (session_id, role, parts, created_at, updated_at) VALUES ('s1', 'assistant', ?, ?, ?)`, p, at, at); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

// idleCrushOnPath puts a long-running executable named crush first on PATH, so
// a harness of kind crush spawns and stays RUNNING without a real crush.
func idleCrushOnPath(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nwhile true; do sleep 0.05; done\n"
	if err := os.WriteFile(filepath.Join(dir, "crush"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	isolateCrushGlobal(t)
}

// isolateCrushGlobal points crush's global data directory at an empty temp
// dir, so a crush.json on the machine running the tests cannot name a store
// the fixture did not build.
func isolateCrushGlobal(t *testing.T) {
	t.Helper()
	t.Setenv("CRUSH_GLOBAL_DATA", t.TempDir())
}

// archivesOf lists the .oversized- archives of the store at db.
func archivesOf(t *testing.T, db string) []string {
	t.Helper()
	got, err := filepath.Glob(db + ".oversized-*")
	if err != nil {
		t.Fatal(err)
	}
	var primary []string
	for _, g := range got {
		if !strings.HasSuffix(g, "-wal") && !strings.HasSuffix(g, "-shm") {
			primary = append(primary, g)
		}
	}
	return primary
}

// countTurns reads how many assistant turns an archived store holds, so the
// test checks the archive is the wedged store's content, not just a file with
// the right name.
func countTurns(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE role = 'assistant'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// startGuarded brings every harness in cfg up under a Manager with a fast
// session guard attached, exactly as the daemon wires it, and waits for them
// to run.
func startGuarded(t *testing.T, cfg *core.Config) (*Manager, *SessionGuard) {
	t.Helper()
	m := newTestManager(t, cfg)
	if err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	m.Autostart()
	for _, name := range cfg.HarnessOrder {
		waitFor(t, 3*time.Second, name+" reaches running", func() bool {
			snap, _ := m.Snapshot(name)
			return snap.State == core.StateRunning
		})
	}
	g := NewSessionGuard(m, 20*time.Millisecond, DefaultSessionGuardLookback)
	m.SetSessionGuard(g)
	g.Start()
	t.Cleanup(func() {
		g.Close()
		m.SetSessionGuard(nil)
	})
	return m, g
}

// TestSessionGuardRotatesDataDirStore is the production shape: a crush harness
// whose args carry --data-dir, working in a directory whose own .crush store
// is healthy. The guard must read the data-dir store, find it wedged, and
// archive THAT store — leaving the workdir's decoy untouched — then restart
// the harness.
//
// Against a guard that only looks at <workdir>/.crush/crush.db this fails: it
// reads the healthy decoy, never flags, never archives.
func TestSessionGuardRotatesDataDirStore(t *testing.T) {
	idleCrushOnPath(t)
	work := t.TempDir()
	decoy := crushStore(t, filepath.Join(work, ".crush"), guardOKTurn, guardOKTurn)
	dataDir := filepath.Join(t.TempDir(), "crush-qwen", "data")
	wedged := crushStore(t, dataDir, guardCtxErr, guardCtxErr, guardCtxErr)

	cfg := managerCfg(core.Harness{
		Name:    "crush-qwen",
		Adapter: "crush",
		Workdir: work,
		Args:    []string{"--yolo", "--channels", "switchboard", "--data-dir", dataDir},
		Backend: core.BackendNative,
	})
	m, g := startGuarded(t, cfg)
	before, _ := m.Snapshot("crush-qwen")

	waitFor(t, 5*time.Second, "guard archives the --data-dir store", func() bool {
		return len(archivesOf(t, wedged)) == 1
	})
	archive := archivesOf(t, wedged)[0]
	if n := countTurns(t, archive); n != 3 {
		t.Fatalf("archive %s holds %d assistant turns, want the wedged store's 3", archive, n)
	}
	if _, err := os.Stat(wedged); !os.IsNotExist(err) {
		t.Fatalf("wedged store %s still in place after rotation (err=%v)", wedged, err)
	}
	if _, err := os.Stat(decoy); err != nil {
		t.Fatalf("workdir store must be untouched: %v", err)
	}
	if got := archivesOf(t, decoy); len(got) != 0 {
		t.Fatalf("workdir store archived: %v", got)
	}

	stalled, rotations := g.Flag("crush-qwen")
	if !stalled || rotations != 1 {
		t.Fatalf("Flag = (stalled=%v, rotations=%d), want (true, 1)", stalled, rotations)
	}
	waitFor(t, 3*time.Second, "harness restarted after rotation", func() bool {
		snap, _ := m.Snapshot("crush-qwen")
		return snap.State == core.StateRunning && snap.LastStarted.After(before.LastStarted)
	})
	if snap, _ := m.Snapshot("crush-qwen"); !snap.SessionStalled || snap.SessionRotations != 1 {
		t.Fatalf("snapshot overlay = (stalled=%v, rotations=%d), want (true, 1)", snap.SessionStalled, snap.SessionRotations)
	}
}

// TestGuardStoreResolution pins which database the guard reads for each way a
// crush harness can name its store: every pflag spelling of --data-dir/-D, a
// {workdir}-relative data dir, the workdir fallback, and a non-crush harness
// in the same directory, which owns no crush store and is never checked.
func TestGuardStoreResolution(t *testing.T) {
	isolateCrushGlobal(t)
	work := t.TempDir()
	workStore := crushStore(t, filepath.Join(work, ".crush"))
	dataDir := filepath.Join(t.TempDir(), "data")
	dataStore := crushStore(t, dataDir)
	relStore := crushStore(t, filepath.Join(work, "rel"))

	cases := []struct {
		name    string
		adapter string
		args    []string
		want    string
	}{
		{"flag then value", "crush", []string{"--yolo", "--data-dir", dataDir}, dataStore},
		{"flag=value", "crush", []string{"--data-dir=" + dataDir, "--yolo"}, dataStore},
		{"short then value", "crush", []string{"-D", dataDir}, dataStore},
		{"short=value", "crush", []string{"-D=" + dataDir}, dataStore},
		{"short joined", "crush", []string{"-D" + dataDir}, dataStore},
		{"workdir placeholder", "crush", []string{"--data-dir", "{workdir}/rel"}, relStore},
		{"relative to workdir", "crush", []string{"--data-dir", "rel"}, relStore},
		{"no data dir falls back to workdir", "crush", []string{"--yolo"}, workStore},
		{"named but not created yet", "crush", []string{"--data-dir", t.TempDir()}, ""},
		{"not crush", "claude-code", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := core.Harness{Name: "h", Adapter: tc.adapter, Workdir: work, Args: tc.args}
			if got := guardStore(h); got != tc.want {
				t.Fatalf("guardStore = %q, want %q", got, tc.want)
			}
		})
	}

	// options.data_directory in the project's crush.json names the store
	// too, and wins over <workdir>/.crush exactly as it does for crush.
	cfgWork := t.TempDir()
	crushStore(t, filepath.Join(cfgWork, ".crush"))
	cfgStore := crushStore(t, filepath.Join(cfgWork, "cfgdata"))
	if err := os.WriteFile(filepath.Join(cfgWork, "crush.json"), []byte(`{"options":{"data_directory":"cfgdata"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := guardStore(core.Harness{Name: "h", Adapter: "crush", Workdir: cfgWork}); got != cfgStore {
		t.Fatalf("config data_directory: guardStore = %q, want %q", got, cfgStore)
	}
}

// TestSessionGuardRotatesSharedStoreOnce covers two harnesses configured onto
// one data directory — crush-qwen and crush-qwen-2 on tars both wrote
// /home/joestump-agent/.local/share/crush-qwen/data/crush.db. Both are wedged
// by the same store, so both are stopped before it moves and both restart
// after, and the store is archived exactly once. Rotating one alone would
// leave the other writing into the archive it still holds open, wedged, and
// invisible to every later check.
func TestSessionGuardRotatesSharedStoreOnce(t *testing.T) {
	idleCrushOnPath(t)
	work := t.TempDir()
	dataDir := filepath.Join(t.TempDir(), "shared")
	wedged := crushStore(t, dataDir, guardCtxErr, guardCtxErr)

	mk := func(name string, args ...string) core.Harness {
		return core.Harness{Name: name, Adapter: "crush", Workdir: work, Args: args, Backend: core.BackendNative}
	}
	cfg := managerCfg(
		mk("crush-qwen", "--yolo", "--data-dir", dataDir),
		mk("crush-qwen-2", "--yolo", "--data-dir="+dataDir),
	)
	m, g := startGuarded(t, cfg)
	before := map[string]time.Time{}
	for _, n := range cfg.HarnessOrder {
		s, _ := m.Snapshot(n)
		before[n] = s.LastStarted
	}

	waitFor(t, 5*time.Second, "guard archives the shared store", func() bool {
		return len(archivesOf(t, wedged)) >= 1
	})
	for _, n := range cfg.HarnessOrder {
		waitFor(t, 3*time.Second, n+" restarted after rotation", func() bool {
			snap, _ := m.Snapshot(n)
			return snap.State == core.StateRunning && snap.LastStarted.After(before[n])
		})
		if stalled, rotations := g.Flag(n); !stalled || rotations != 1 {
			t.Fatalf("%s Flag = (stalled=%v, rotations=%d), want (true, 1)", n, stalled, rotations)
		}
	}
	if got := archivesOf(t, wedged); len(got) != 1 {
		t.Fatalf("shared store archived %d times, want once: %v", len(got), got)
	}
}
