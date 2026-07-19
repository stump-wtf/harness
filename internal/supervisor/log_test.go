package supervisor

// Governing tests: ADR-0007 (tee raw PTY output to a rotating per-harness log
// under $XDG_STATE_HOME/harnessd/logs/<name>.log, size/age rotation).

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
