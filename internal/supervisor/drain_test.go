package supervisor

// PTY Drain Tests
//
// A run's log must hold everything its process wrote before exiting. Linux
// does not make a session leader's exit wait until its terminal output has
// been read, so the PTY reader can still be behind when the exit arrives. The
// exit path therefore has to wait for the reader rather than close the PTY
// under it, and the reader has to be able to finish without that close.
//
// Governing: SPEC-0008 REQ "Per-Run Logs"; ADR-0007.
//
// @joestump 09/21/2026 - Added after TestLogsRunSelector lost a run's only
// output line on a loaded CI runner (actions run 12420, on PR #392).

import (
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestSpawnPTYReachesEOFWhenProcessExits: once a spawned process exits, its
// PTY reads to EOF with all of the process's output, and nothing has to close
// the PTY first. While the daemon kept its own copy of the slave, a Linux
// master never reached EOF, so the only way to stop the reader was to close
// the PTY under it and drop whatever it had not read yet.
func TestSpawnPTYReachesEOFWhenProcessExits(t *testing.T) {
	proc, err := spawn(shHarness("eof", "echo eof-probe", 0), 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proc.pty.Close() })
	go func() { _ = proc.cmd.Wait() }()

	read := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(proc.pty)
		read <- b
	}()
	select {
	case b := <-read:
		if !strings.Contains(string(b), "eof-probe") {
			t.Errorf("PTY reached EOF without the process's output: %q", b)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("PTY never reached EOF after its process exited; the reader can only be stopped by closing it, which drops unread output")
	}
}

// TestRunLogKeepsOutputOfLaggingReader: a run's output reaches its log even
// when the PTY reader has not read a byte by the time the process exits. The
// reader is held back until the supervisor first waits on it, the furthest a
// reader can lag and still be owed the output. That turns the CI race into a
// certainty: an exit path that closes the PTY before it waits loses the line
// on every run, not one in a hundred.
func TestRunLogKeepsOutputOfLaggingReader(t *testing.T) {
	if runtime.GOOS != "linux" {
		// macOS holds a session leader's exit until its terminal output has
		// been read, so a reader cannot lag the exit there, and holding one
		// back past the exit, as this test does, would never let it finish.
		t.Skip("only Linux lets a PTY reader fall behind its process's exit")
	}
	waited := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { lagPTYReader, onReaderDrain = nil, nil })
	onReaderDrain = func() { once.Do(func() { close(waited) }) }
	lagPTYReader = func(r io.Reader) io.Reader { return heldReader{r: r, until: waited} }

	e := newRunsEnv(t)
	m, _ := e.manager(t, sweepCfg(sweep("lagged", "echo output-of-lagged-run")), fastPolicy())
	m.StartRun("lagged", RunRequest{Trigger: TriggerSchedule})
	waitRuns(t, m, "lagged", "the run finishes", outcomesAre(OutcomeSuccess))

	log := readText(t, m.RunLogPath("lagged", 1))
	if !strings.Contains(log, "output-of-lagged-run") {
		t.Fatalf("run log lost the output a lagging reader had not read at exit:\n%s", log)
	}
	if strings.Index(log, "output-of-lagged-run") > strings.LastIndex(log, "run finished") {
		t.Errorf("run output landed after the finish line:\n%s", log)
	}
}

// heldReader blocks every read until its channel closes.
type heldReader struct {
	r     io.Reader
	until <-chan struct{}
}

func (h heldReader) Read(p []byte) (int, error) {
	<-h.until
	return h.r.Read(p)
}
