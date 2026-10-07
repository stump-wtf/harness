package supervisor

// Structured One-Shots On Pipes
//
// A prompt one-shot whose adapter declares a structured stream (Claude Code's
// stream-json, internal/adapter) has no screen. Under a PTY it paid for one
// anyway: stderr merged into the JSON, line endings translated, and every
// line rendered by two x/vt emulators with 10,000-row scrollbacks nobody
// reads, one per spawn in the log sanitizer and one per harness in the attach
// mux. A three-stage pipeline of such runs drove the daemon to 12 GB
// (https://github.com/stump-wtf/harness/issues/18).
//
// So such a run gets no PTY. Its stdout and stderr are pipes, its stdin is
// /dev/null, and it is a session leader with no controlling terminal:
//
//   - stdout is read a line at a time, masked (redact.Lines.JSONLine), and
//     written to jobs/<name>/<id>.stream.jsonl beside the run's log. A run with
//     no per-run log (a prompt harness with no triggers, or a run whose log
//     could not be opened) writes those lines to the harness's durable log
//     instead.
//   - stderr is read a line at a time, masked, stripped of escape sequences,
//     and written to the run's log and the harness's durable log: what the
//     PTY path wrote there, minus the emulator.
//   - both go on to the attach mux (ExtraOut) as CRLF-terminated lines, the
//     bytes a terminal would have shown.
//
// Nothing here parses the stream. Normalizing it is agent-trace's job
// (ADR-0033 "Normalization belongs to agent-trace"); this file only moves
// lines, bounded, from a pipe to a file.
//
// Lines are unbounded in principle and reach 4 MiB in practice (a tool result
// carrying a whole file), so bufio.Scanner's 64 KiB token cap is not an
// option. readLines holds one line at a time and drops, with a marker, any
// line longer than maxPipeLine. Nothing buffers a whole run.
//
// The stream file is sealed on the actor loop before the run's record is
// closed (sealStream, from finishRunWith): Manager.CloseRun queues a sealed
// run's artifacts for compression, so a line written after that is lost.
//
// Governing: ADR-0033 "Structured one-shots run on pipes" (no PTY, no
// emulator; the redacted stream kept as <id>.stream.jsonl beside the per-run
// log and pruned with it by keep_runs); ADR-0008 (nothing persisted
// unredacted); ADR-0005 (process-group supervision); SPEC-0017 REQ-18;
// SPEC-0008 REQ "Per-Run Logs".
//
// @joestump-agent 09/28/2026 - Added for
// https://github.com/stump-wtf/harness/issues/18.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"

	"github.com/stump-wtf/agent-trace/redact"
	"github.com/stump-wtf/harness/internal/adapter"
	"github.com/stump-wtf/harness/internal/core"
)

// maxPipeLine is the longest stdout or stderr line a pipe run keeps. A longer
// one is read to its end and dropped, and a marker line takes its place.
// Real stream-json lines reach 4 MiB; the cap exists so a process that writes
// without newlines cannot pin unbounded memory in the daemon. A variable so a
// test can exercise the drop without writing 16 MiB.
var maxPipeLine = 16 << 20

// pipeReadBuffer is the reader's buffer: a line up to this size is handed on
// without a copy.
const pipeReadBuffer = 64 << 10

// newEmulator builds every x/vt emulator this package runs: the log
// sanitizer's (sanitize.go). A pipe run builds none. A variable so a test can
// count constructions: "no emulator for a pipe run" is checked where an
// emulator comes into being, not inferred from which branch ran.
var newEmulator = vt.NewEmulator

// RunsOnPipes reports whether h's process runs on pipes: a prompt one-shot
// whose adapter declares a structured stream. A resident harness, a `command`
// harness (whatever its argv) and every adapter that declares nothing keep
// their PTY. Decided from the definition being spawned, so a reload that
// changes the adapter takes effect at the next spawn, like every other
// run-affecting field. Exported for the daemon, which reads a pipe run's
// stream file where it would read a PTY run's log.
func RunsOnPipes(h core.Harness) bool {
	if h.Prompt == "" && h.PromptFile == "" {
		return false
	}
	a := adapter.NewRegistryWithDefaults().Resolve(h)
	if _, ok := a.(adapter.ArgvOwner); ok {
		return false
	}
	return adapter.PromptStreamOf(a) != ""
}

// startOnPipes execs name with stdout and stderr on fresh pipes and stdin on
// /dev/null. The child is a session leader (Setsid), so pgid == pid and stop,
// kill and SignalGroup reach its whole group exactly as they do a PTY
// child's; it has no controlling terminal, because it has no terminal.
//
// The write ends are the child's own descriptors (exec hands an *os.File over
// without a copying goroutine), and the daemon's copies are closed once it has
// started, so a reader sees EOF when the child and every descendant that
// inherited them have exited. cmd.StdoutPipe is not used: Wait closes those
// pipes, and wait() reaps before it drains.
func startOnPipes(name string, args []string, dir string, env []string) (*process, error) {
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("supervisor: stdout pipe: %w", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		_, _ = outR.Close(), outW.Close()
		return nil, fmt.Errorf("supervisor: stderr pipe: %w", err)
	}
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = outW
	cmd.Stderr = errW
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		_, _, _, _ = outR.Close(), outW.Close(), errR.Close(), errW.Close()
		return nil, fmt.Errorf("supervisor: start %q: %w", name, err)
	}
	_, _ = outW.Close(), errW.Close()
	return &process{cmd: cmd, pid: cmd.Process.Pid, stdout: outR, stderr: errR, progress: &pipeProgress{}}, nil
}

// pipeDrainMax caps how long a pipe run's exit waits on readers that are
// still making progress.
const pipeDrainMax = time.Minute

// pipeProgress lets the exit drain tell a slow pipe reader from a stuck one.
//
// A PTY run's drain gives its reader exitDrainBound and then closes the
// master, because a reader still going after that is waiting on a terminal a
// descendant holds. A pipe reader can be behind for a different reason: it
// has already read a multi-megabyte line and is masking it, and what it has
// not written yet is the end of the run. Cutting it off there lost the tail of
// the stream (the result line) whenever masking outran the bound, which a
// loaded CI runner under -race does. So a pipe run's drain keeps waiting while
// any reader is busy or a read has returned since the last look, and gives up
// only when every reader has sat in an empty read for a whole bound: the
// descendant case. pipeDrainMax caps it all.
type pipeProgress struct {
	reads   atomic.Int64 // reads returned, ever
	waiting atomic.Int32 // readers inside a read right now
	live    atomic.Int32 // readers not yet finished
}

// track wraps a pipe so its reads count as progress.
func (p *pipeProgress) track(r io.Reader) io.Reader {
	p.live.Add(1)
	return trackedPipe{r: r, p: p}
}

type trackedPipe struct {
	r io.Reader
	p *pipeProgress
}

func (t trackedPipe) Read(b []byte) (int, error) {
	t.p.waiting.Add(1)
	n, err := t.r.Read(b)
	t.p.waiting.Add(-1)
	t.p.reads.Add(1)
	return n, err
}

// drain waits for done while the readers are making progress.
func (p *pipeProgress) drain(done <-chan struct{}) {
	if onReaderDrain != nil {
		onReaderDrain()
	}
	deadline := time.Now().Add(pipeDrainMax)
	last := p.reads.Load()
	for {
		select {
		case <-done:
			return
		case <-time.After(exitDrainBound):
		}
		cur := p.reads.Load()
		stuck := cur == last && p.waiting.Load() >= p.live.Load()
		if stuck || time.Now().After(deadline) {
			return
		}
		last = cur
	}
}

// drainOutput waits, bounded, for a spawn's readers: drainReader's fixed bound
// for a PTY spawn (p nil), the progress-aware wait for a pipe run's.
func drainOutput(p *pipeProgress, done <-chan struct{}) {
	if p == nil {
		drainReader(done)
		return
	}
	p.drain(done)
}

// LineOuter is implemented by an ExtraOut that takes a pipe run's output lines
// somewhere other than its PTY sink: the attach registry's per-harness Output,
// whose LineOut is a mux with no terminal emulator. readPipes asks for it once
// per pipe run; an ExtraOut without it gets the lines through Write.
type LineOuter interface {
	LineOut() io.Writer
}

// Primer is implemented by an ExtraOut that prepares for a harness's runs when
// its supervisor is built, told whether they run on pipes. The attach registry
// builds a terminal harness's mux then, as it always has, and nothing for a
// structured one-shot, whose runs never need an emulator.
type Primer interface {
	Prime(pipes bool)
}

// resize applies an attach viewport to the PTY. A pipe run has no terminal to
// size, so it is a no-op.
func (p *process) resize(cols, rows int) {
	if p.pty != nil {
		_ = p.pty.Resize(cols, rows)
	}
}

// writeInput delivers attach keystrokes to the PTY. A pipe run's stdin is
// /dev/null, so they are dropped.
func (p *process) writeInput(b []byte) {
	if p.pty != nil {
		_, _ = p.pty.Write(b)
	}
}

// closeIO closes the daemon's end of the process's output: the PTY master, or
// the pipes' read ends. Closing unblocks a reader the exit drain gave up on.
func (p *process) closeIO() {
	if p.pty != nil {
		_ = p.pty.Close()
	}
	if p.stdout != nil {
		_ = p.stdout.Close()
	}
	if p.stderr != nil {
		_ = p.stderr.Close()
	}
}

// hangup delivers to a pipe run's process group the SIGHUP a terminal gives a
// PTY child's group when its session leader exits, so a descendant that would
// have died with the terminal (and would hold the pipes open) does here too.
// A PTY child's kernel already does this. The leader has been reaped; a group
// id is not reused while any member lives, so -pid reaches only what is left
// of this run.
func (p *process) hangup() {
	if p.pty != nil || p.pid <= 1 {
		return
	}
	_ = syscall.Kill(-p.pid, syscall.SIGHUP)
}

// StreamPathFor turns a run's log path into its stream file path, as
// eventPathFor does for the event file; pruning (runArtifactID) and sealing
// (Manager.CloseRun) name the same file. Exported for the daemon's readers.
func StreamPathFor(logPath string) string {
	if logPath == "" {
		return ""
	}
	return strings.TrimSuffix(logPath, ".log") + ".stream.jsonl"
}

// streamSink is a pipe run's .stream.jsonl. The stdout reader writes it and
// closes it at EOF; the actor loop closes it when the run is sealed. Whichever
// comes first wins, and a line written after the close is dropped: by then the
// file belongs to a closed run.
type streamSink struct {
	mu sync.Mutex
	f  *os.File
}

// openStreamSink creates a run's stream file, private to the daemon's user
// (ADR-0008): it holds what the agent printed, masked.
func openStreamSink(path string) (*streamSink, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &streamSink{f: f}, nil
}

// writeLine appends one line and its newline.
func (s *streamSink) writeLine(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f != nil {
		_, _ = io.WriteString(s.f, line+"\n")
	}
}

// Close closes the file once; later calls and writes are no-ops.
func (s *streamSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	return err
}

// readPipes starts the readers of a pipe run's stdout and stderr, and closes
// done once both have reached EOF and the stream file is closed. It opens the
// stream file first, on the loop, so the path comes from the run in flight.
func (s *Supervisor) readPipes(proc *process, done chan struct{}) {
	if s.stream != nil {
		// Every run end seals it (finishRunWith); one still open here is a
		// path that forgot to, and a second writer must not share it.
		_ = s.stream.Close()
		s.stream = nil
	}
	var stream *streamSink
	if s.run != nil {
		if path := StreamPathFor(s.run.rec.Log); path != "" {
			var err error
			if stream, err = openStreamSink(path); err != nil {
				s.logEvent("run stream unavailable", "run_id", s.run.rec.RunID, "err", err.Error())
				stream = nil
			}
		}
	}
	s.stream = stream
	logOut, tee := s.historyOut(), s.extraOut
	if lo, ok := tee.(LineOuter); ok {
		// The attach registry serves a pipe run from a mux with no
		// emulator (internal/attach, lines.go).
		tee = lo.LineOut()
	}

	prog := proc.progress
	stdout, stderr := prog.track(proc.stdout), prog.track(proc.stderr)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer prog.live.Add(-1)
		pipeStdout(stdout, stream, logOut, tee)
		if stream != nil {
			_ = stream.Close()
		}
	}()
	go func() {
		defer wg.Done()
		defer prog.live.Add(-1)
		pipeStderr(stderr, logOut, tee)
	}()
	go func() {
		wg.Wait()
		close(done)
	}()
}

// sealStream closes the run's stream file before its record is closed:
// Manager.CloseRun seals a run's artifacts, and a line written after that is
// lost. The reader normally closed it at EOF already; the wait is the same
// bounded drain the run's log gets.
func (s *Supervisor) sealStream() {
	if s.stream == nil {
		return
	}
	s.awaitReader()
	_ = s.stream.Close()
	s.stream = nil
}

// pipeStdout moves stdout lines, masked, to the stream file (or, for a run
// with none, the durable log) and on to the attach mux. A dropped line leaves
// a JSON marker, so the file still parses line by line; its key is not
// "type", so it cannot pass for one of the agent's own events.
func pipeStdout(r io.Reader, stream *streamSink, logOut, tee io.Writer) {
	var mask redact.Lines
	readLines(r, maxPipeLine, func(line []byte, dropped int) {
		out := fmt.Sprintf(`{"harness":"line_dropped","stream":"stdout","bytes":%d,"limit":%d}`, dropped, maxPipeLine)
		if dropped == 0 {
			out = mask.JSONLine(string(line))
		}
		switch {
		case stream != nil:
			stream.writeLine(out)
		case logOut != nil:
			_, _ = io.WriteString(logOut, out+"\n")
		}
		if tee != nil {
			_, _ = io.WriteString(tee, out+"\r\n")
		}
	})
}

// pipeStderr moves stderr lines to the run's log and the durable log, and on
// to the attach mux: stripped of escape sequences first, so a colored warning
// is plain text in a log file and a secret cannot hide behind one, then
// masked.
func pipeStderr(r io.Reader, logOut, tee io.Writer) {
	var mask redact.Lines
	readLines(r, maxPipeLine, func(line []byte, dropped int) {
		out := fmt.Sprintf("[harness] dropped a %d-byte stderr line (longer than the %d-byte limit)", dropped, maxPipeLine)
		if dropped == 0 {
			out = mask.String(ansi.Strip(strings.TrimSuffix(string(line), "\r")))
		}
		if logOut != nil {
			_, _ = io.WriteString(logOut, out+"\n")
		}
		if tee != nil {
			_, _ = io.WriteString(tee, out+"\r\n")
		}
	})
}

// readLines calls fn with each line of r, without its newline, until r ends
// or fails. The final line is delivered even without a newline. A line longer
// than max is read to its end without being kept, and fn gets (nil, its
// length) instead. line is valid only for the call: it may point into the
// reader's buffer.
//
// One line is held at a time, and a long line's buffer is dropped after it,
// so a run that once printed 4 MiB does not keep 4 MiB for the rest of its
// life.
func readLines(r io.Reader, max int, fn func(line []byte, dropped int)) {
	br := bufio.NewReaderSize(r, pipeReadBuffer)
	var long []byte // a line longer than the buffer, so far
	dropped := 0    // bytes of an overlong line read and discarded, so far
	for {
		frag, err := br.ReadSlice('\n')
		full := errors.Is(err, bufio.ErrBufferFull)
		end := err == nil // frag ends in the newline
		if end {
			frag = frag[:len(frag)-1]
		}
		switch {
		case dropped > 0 || len(long)+len(frag) > max:
			dropped += len(long) + len(frag)
			long = nil
		case long != nil || full:
			long = append(long, frag...)
		}
		// A line ends at its newline, or where the stream does.
		if end || (err != nil && !full) {
			switch {
			case dropped > 0:
				fn(nil, dropped)
			case long != nil:
				fn(long, 0)
			case end || len(frag) > 0:
				fn(frag, 0)
			}
			long, dropped = nil, 0
		}
		if err != nil && !full {
			return
		}
	}
}
