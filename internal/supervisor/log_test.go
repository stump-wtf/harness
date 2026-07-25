package supervisor

// Governing tests: ADR-0007 (tee raw PTY output to a rotating per-harness log
// under $XDG_STATE_HOME/harness/logs/<name>.log, size/age rotation).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRotatingLogWritesAndRotatesBySize(t *testing.T) {
	dir := t.TempDir()
	rl, err := newRotatingLog("demo", LogConfig{Dir: dir, MaxBytes: 16, MaxBackups: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer rl.Close()

	// Each write is 10 bytes; the second should force a rotation (16-byte cap).
	for i := 0; i < 5; i++ {
		if _, err := rl.Write([]byte("0123456789")); err != nil {
			t.Fatal(err)
		}
	}
	// Active file must exist.
	if _, err := os.Stat(filepath.Join(dir, "demo.log")); err != nil {
		t.Fatalf("active log missing: %v", err)
	}
	// At least one rotated backup must exist.
	backups, _ := filepath.Glob(filepath.Join(dir, "demo-*.log"))
	if len(backups) == 0 {
		t.Fatal("expected rotated backups, found none")
	}
	if len(backups) > 3 {
		t.Fatalf("MaxBackups=3 not enforced: %d backups", len(backups))
	}
}

func TestRotatingLogRotatesByAge(t *testing.T) {
	dir := t.TempDir()
	rl, err := newRotatingLog("aged", LogConfig{Dir: dir, MaxBytes: 1 << 20, MaxAge: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer rl.Close()
	if _, err := rl.Write([]byte("first\n")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(25 * time.Millisecond)
	if _, err := rl.Write([]byte("second\n")); err != nil {
		t.Fatal(err)
	}
	backups, _ := filepath.Glob(filepath.Join(dir, "aged-*.log"))
	if len(backups) == 0 {
		t.Fatal("expected an age-based rotation")
	}
}

func TestRotatingLogContentPreserved(t *testing.T) {
	dir := t.TempDir()
	rl, err := newRotatingLog("keep", LogConfig{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rl.Write([]byte("hello world\n")); err != nil {
		t.Fatal(err)
	}
	if err := rl.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "keep.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "hello world") {
		t.Fatalf("log content not preserved: %q", data)
	}
}

// A harness pruning its own rotated logs must never delete a sibling harness
// whose name merely shares its prefix (kebab-case names like "web" / "web-api").
func TestRotatingLogPruneLeavesSiblingHarnessAlone(t *testing.T) {
	dir := t.TempDir()
	// Sibling harness "web-api" files that a naive `web-*.log` glob would match.
	sibActive := filepath.Join(dir, "web-api.log")
	sibBackup := filepath.Join(dir, "web-api-20260101T000000.000.log")
	if err := os.WriteFile(sibActive, []byte("sibling active\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sibBackup, []byte("sibling backup\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rl, err := newRotatingLog("web", LogConfig{Dir: dir, MaxBytes: 16, MaxBackups: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer rl.Close()
	// Force several rotations of "web" so its prune path runs repeatedly.
	for i := 0; i < 8; i++ {
		if _, err := rl.Write([]byte("0123456789")); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := os.Stat(sibActive); err != nil {
		t.Errorf("sibling active log web-api.log deleted by web's prune: %v", err)
	}
	if _, err := os.Stat(sibBackup); err != nil {
		t.Errorf("sibling backup web-api-*.log deleted by web's prune: %v", err)
	}
	// "web" must still enforce its own MaxBackups on its own rotated files.
	own, _ := filepath.Glob(filepath.Join(dir, "web-2*.log"))
	if len(own) > 1 {
		t.Errorf("web MaxBackups=1 not enforced on own backups: %d", len(own))
	}
}

// A namespaced project harness ("reduit/agent") rotates into its project
// subdirectory; prune must recognize those backups as its own (the name's
// directory part is stripped by filepath.Base) or they accumulate forever
// (ADR-0009; SPEC-0004 REQ "Project Naming And Namespacing").
func TestRotatingLogPrunesNamespacedBackups(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "reduit")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	// Pre-seed old rotated backups with valid stamps, as past rotations left.
	for _, stamp := range []string{
		"20260101T000000.000", "20260102T000000.000", "20260103T000000.000",
	} {
		if err := os.WriteFile(filepath.Join(sub, "agent-"+stamp+".log"), []byte("old\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	rl, err := newRotatingLog("reduit/agent", LogConfig{Dir: dir, MaxBytes: 16, MaxBackups: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer rl.Close()
	// Force one rotation so pruneBackups runs.
	for i := 0; i < 2; i++ {
		if _, err := rl.Write([]byte("0123456789")); err != nil {
			t.Fatal(err)
		}
	}

	backups, _ := filepath.Glob(filepath.Join(sub, "agent-*.log"))
	if len(backups) == 0 {
		t.Fatal("expected a rotated backup for the namespaced harness")
	}
	if len(backups) > 1 {
		t.Fatalf("MaxBackups=1 not enforced for namespaced harness: %d backups %v", len(backups), backups)
	}
}

// removeLogArtifacts is the tear-down path for deregistered project harnesses
// (SPEC-0004 REQ "Tear Down"): active log + backups go, the project directory
// goes only once empty, and sibling harnesses are untouched.
func TestRemoveLogArtifacts(t *testing.T) {
	dir := t.TempDir()
	rl, err := newRotatingLog("reduit/agent", LogConfig{Dir: dir, MaxBytes: 16, MaxBackups: 3})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ { // force at least one rotation
		if _, err := rl.Write([]byte("0123456789")); err != nil {
			t.Fatal(err)
		}
	}
	if err := rl.Close(); err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(dir, "reduit", "other.log")
	if err := os.WriteFile(sibling, []byte("sibling\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	removeLogArtifacts(dir, "reduit/agent")

	if leftovers, _ := filepath.Glob(filepath.Join(dir, "reduit", "agent*")); len(leftovers) != 0 {
		t.Errorf("agent log artifacts survive removal: %v", leftovers)
	}
	if _, err := os.Stat(sibling); err != nil {
		t.Errorf("sibling harness log removed: %v", err)
	}
	// Directory kept while the sibling lives in it, dropped once empty.
	if _, err := os.Stat(filepath.Join(dir, "reduit")); err != nil {
		t.Fatalf("non-empty project log dir removed: %v", err)
	}
	removeLogArtifacts(dir, "reduit/other")
	if _, err := os.Stat(filepath.Join(dir, "reduit")); !os.IsNotExist(err) {
		t.Errorf("empty project log dir not removed: %v", err)
	}
}

func TestRotatingLogWriteAfterClose(t *testing.T) {
	dir := t.TempDir()
	rl, err := newRotatingLog("closed", LogConfig{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	rl.Close()
	if _, err := rl.Write([]byte("x")); err == nil {
		t.Fatal("write after close should error")
	}
}
