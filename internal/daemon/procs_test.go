package daemon

// The children sample (issue #958; ADR-0048) is a goroutine the server owns,
// so its two failure modes are the ones worth pinning: a Close that waits on a
// loop that never started would hang every test (and every shutdown) that
// builds a server without serving, and a sampler that never fills means the
// surfaces silently say nothing about children.
//
// Governing: ADR-0048 (proposed); issue #958.

import (
	"os/exec"
	"testing"
	"time"
)

func TestProcSamplerCloseWithoutStart(t *testing.T) {
	done := make(chan struct{})
	go func() {
		newProcSampler(time.Millisecond).Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close waited on a loop that never ran")
	}
}

func TestProcSamplerFillsItsSample(t *testing.T) {
	if testing.Short() {
		t.Skip("ps(1) needed")
	}
	if _, err := exec.LookPath("ps"); err != nil {
		t.Skip("no ps(1) on this box")
	}
	p := newProcSampler(time.Hour)
	if p.current() != nil {
		t.Fatal("an unstarted sampler already has a sample")
	}
	p.Start()
	t.Cleanup(p.Close)
	deadline := time.Now().Add(10 * time.Second)
	for p.current() == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if p.current() == nil {
		t.Fatal("the sampler never read the process table: children would be omitted forever")
	}
	if len(p.current().Children(1, 1)) == 0 && len(p.current().Children(0, 1)) == 0 {
		t.Fatal("the sample names no process at all")
	}
}
