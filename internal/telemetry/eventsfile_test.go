package telemetry

// Events File Faults
//
// Governing tests: SPEC-0015 REQ-8 (each line written whole, a failed write
// does not stop the sink), REQ-11 (the shutdown flush is bounded even when
// the file write is not).
//
// A real disk will not run out of space or hang on cue, so these drive the
// pipeline through Options.OpenEventsFile with a handle that fails a write
// halfway through, or blocks in it, and assert on the bytes left in the file
// and on how long Shutdown took.
//
// @joestump-agent 09/21/2026 - Added for the ENOSPC partial-line and hung
// write findings on harness#391.

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/stump-wtf/harness/internal/core"
)

// faultyFile writes through to a real file, but while short is set it writes
// only the first half of the data and reports ENOSPC — what a disk filling up
// mid-batch does.
type faultyFile struct {
	*os.File
	short *atomic.Bool
}

func (f faultyFile) Write(p []byte) (int, error) {
	if f.short.Load() {
		n, _ := f.File.Write(p[:len(p)/2])
		return n, syscall.ENOSPC
	}
	return f.File.Write(p)
}

func TestEventsFileFailedWriteLeavesNoPartialLine(t *testing.T) {
	file := filepath.Join(t.TempDir(), "events.jsonl")
	var short atomic.Bool
	open := func(path string) (EventsFileHandle, error) {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			return nil, err
		}
		return faultyFile{f, &short}, nil
	}
	res := testResolved("", func(c *core.TelemetryConfig) { c.Logs, c.Traces = false, false; c.EventsFile = file })
	p, obs := startPipeline(t, res, newFakeSource(optIn("w")), Options{OpenEventsFile: open})

	obs.publish(toolEv("w", "k", 0, "first", false, t0))
	waitFor(t, "the first line", func() bool { return p.Stats()[SignalEventsFile].Exported == 1 })
	before, _ := os.ReadFile(file)

	short.Store(true)
	obs.publish(toolEv("w", "k", 1, "second, cut short by a full disk", false, t0))
	waitFor(t, "the failed batch", func() bool { return p.Stats()[SignalEventsFile].Failed == 1 })
	if after, _ := os.ReadFile(file); string(after) != string(before) {
		t.Fatalf("a failed write left %d bytes behind:\n%q", len(after)-len(before), after[len(before):])
	}

	short.Store(false)
	obs.publish(toolEv("w", "k", 2, "third", false, t0))
	waitFor(t, "the third line", func() bool { return p.Stats()[SignalEventsFile].Exported == 2 })

	f, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var bodies []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("line is not one JSON object (a partial write survived): %q", sc.Text())
		}
		bodies = append(bodies, m["body"].(string))
	}
	if len(bodies) != 2 || bodies[0] != "first" || bodies[1] != "third" {
		t.Fatalf("bodies = %q, want [first third]", bodies)
	}
}
