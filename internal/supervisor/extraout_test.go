package supervisor

// Governing: ADR-0003 / ADR-0007 (raw PTY output tees to BOTH the durable log
// and the vt ring) and the reviewer's note that the Manager lacked ExtraOut
// wiring. This verifies ManagerOptions.ExtraOutFor tees a running harness's PTY
// output to the supplied writer alongside its on-disk log, and that the
// per-harness Resize/WriteInput controls report unknown harnesses.

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
)

// safeBuffer is a mutex-guarded bytes.Buffer (the PTY reader writes it from its
// own goroutine while the test reads it).
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestManagerExtraOutTee(t *testing.T) {
	tmp := t.TempDir()
	logDir := filepath.Join(tmp, "logs")
	h := core.Harness{
		Name:    "echoer",
		Cmd:     "sh",
		Args:    []string{"-c", "printf HELLO-EXTRAOUT; sleep 0.1"},
		Backend: core.BackendNative,
	}
	cfg := &core.Config{
		Harnesses:    map[string]core.Harness{"echoer": h},
		HarnessOrder: []string{"echoer"},
	}

	var tee safeBuffer
	mgr := NewManager(cfg, ManagerOptions{
		StatePath:   filepath.Join(tmp, "state.json"),
		LogDir:      logDir,
		ExtraOutFor: func(string) io.Writer { return &tee },
	})
	defer mgr.Close()

	if !mgr.Start("echoer") {
		t.Fatal("Start(echoer) = false, want true")
	}

	// The ExtraOut writer should receive the PTY output.
	waitFor(t, 3*time.Second, "ExtraOut writer never received PTY output",
		func() bool { return bytes.Contains([]byte(tee.String()), []byte("HELLO-EXTRAOUT")) })

	// The durable log should independently contain the same output (ADR-0007).
	waitFor(t, 3*time.Second, "log file never received PTY output", func() bool {
		data, err := os.ReadFile(filepath.Join(logDir, "echoer.log"))
		return err == nil && bytes.Contains(data, []byte("HELLO-EXTRAOUT"))
	})
}

func TestManagerResizeAndInputUnknown(t *testing.T) {
	tmp := t.TempDir()
	cfg := &core.Config{
		Harnesses:    map[string]core.Harness{},
		HarnessOrder: nil,
	}
	mgr := NewManager(cfg, ManagerOptions{StatePath: filepath.Join(tmp, "state.json"), LogDir: filepath.Join(tmp, "logs")})
	defer mgr.Close()

	if mgr.Resize("ghost", 80, 24) {
		t.Error("Resize(unknown) = true, want false")
	}
	if mgr.WriteInput("ghost", []byte("x")) {
		t.Error("WriteInput(unknown) = true, want false")
	}
}
