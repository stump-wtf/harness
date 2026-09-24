package supervisor

// Command Prompt Delivery Tests
//
// A `command` harness's prompt reaches the program by prompt_delivery: as one
// argv element ({{prompt}}), through a pipe on fd 0 while fd 1 and fd 2 stay
// the PTY and fd 1 is the controlling terminal, or as a 0600 file named by
// HARNESS_PROMPT_FILE. Every assertion here is made by a real child — the
// argv probe of command_test.go, extended below to report on its own
// descriptors, its stdin and its environment — or by what landed on disk. None
// of them reads a log line as evidence that something was delivered.
//
// Governing: ADR-0023, SPEC-0017 REQ-12 "Prompt Delivery", REQ-11 "Rendering"
// (scenario "Nothing rendered is persisted"), REQ "Error Handling Standards";
// design.md § "`stdin` delivery moves the controlling terminal to fd 1".

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/term"

	"github.com/stump-wtf/harness/internal/attach"
	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/protocol"
)

// The probe's prompt delivery knobs. Like argvProbeEnv they reach the child
// through its env_file (withProbeEnv), never the test process's environment.
const (
	// probeStdinOutEnv: copy stdin, to EOF, into the named file before the
	// record is written.
	probeStdinOutEnv = "HARNESS_TEST_PROBE_STDIN_OUT"
	// probeSayEnv: print this line to stdout.
	probeSayEnv = "HARNESS_TEST_PROBE_SAY"
	// probeTTYWaitEnv: after the record, read the terminal on fd 1 until the
	// named file exists, then write whatever arrived to <record>.tty.
	probeTTYWaitEnv = "HARNESS_TEST_PROBE_TTY_WAIT"
	// probeHoldEnv: start a grandchild, in its own session, that holds this
	// process's stdin open for 30s, and write its pid to the named file.
	probeHoldEnv = "HARNESS_TEST_PROBE_HOLD"
	// stdinHolderEnv turns the test binary into that grandchild.
	stdinHolderEnv = "HARNESS_TEST_STDIN_HOLDER"
)

// probeDelivery is what the probe records about how it was handed its
// prompt, each field observed by the child itself.
type probeDelivery struct {
	// Stdin0TTY and Stdout1TTY are `test -t 0` and `test -t 1`.
	Stdin0TTY  bool `json:"stdin0_tty"`
	Stdout1TTY bool `json:"stdout1_tty"`
	// Fd1Ctty: fd 1 is this process's controlling terminal (tcgetpgrp on it
	// succeeds only then).
	Fd1Ctty bool `json:"fd1_ctty"`
	// StdinBytes counts what the probe read from stdin, when asked to.
	StdinBytes int `json:"stdin_bytes"`
	// PromptFileSet/Env are HARNESS_PROMPT_FILE as the child saw it; the
	// mode and body are of that file, read by the child at startup, so they
	// prove the file was there, private and complete before exec.
	PromptFileSet  bool   `json:"prompt_file_set"`
	PromptFileEnv  string `json:"prompt_file_env"`
	PromptFileMode uint32 `json:"prompt_file_mode"`
	PromptFileBody string `json:"prompt_file_body"`
}

// observeDelivery fills the probe's delivery observations and performs the
// actions its knobs ask for. A non-zero return is the probe's exit code.
func (p *argvProbe) observeDelivery() int {
	p.Stdin0TTY = term.IsTerminal(0)
	p.Stdout1TTY = term.IsTerminal(1)
	p.Fd1Ctty = fdIsCtty(1)
	p.PromptFileEnv, p.PromptFileSet = os.LookupEnv(promptFileVar)
	if p.PromptFileSet {
		if fi, err := os.Stat(p.PromptFileEnv); err == nil {
			p.PromptFileMode = uint32(fi.Mode().Perm())
		}
		if b, err := os.ReadFile(p.PromptFileEnv); err == nil {
			p.PromptFileBody = string(b)
		}
	}
	if dst := os.Getenv(probeStdinOutEnv); dst != "" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return 3
		}
		p.StdinBytes = len(b)
		if err := os.WriteFile(dst+".tmp", b, 0o600); err != nil {
			return 3
		}
		if err := os.Rename(dst+".tmp", dst); err != nil {
			return 3
		}
	}
	if say := os.Getenv(probeSayEnv); say != "" {
		fmt.Println(say)
	}
	if pidfile := os.Getenv(probeHoldEnv); pidfile != "" {
		holder := exec.Command(os.Args[0])
		holder.Env = []string{stdinHolderEnv + "=1"}
		holder.Stdin = os.Stdin
		holder.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := holder.Start(); err != nil {
			return 4
		}
		if err := os.WriteFile(pidfile, []byte(strconv.Itoa(holder.Process.Pid)), 0o600); err != nil {
			return 4
		}
	}
	return 0
}

// afterProbeRecord runs once the record is on disk: the TTY watch, which
// reports whether anything typed at an attach reached the child's terminal.
func afterProbeRecord(out string) int {
	stop := os.Getenv(probeTTYWaitEnv)
	if stop == "" {
		return 0
	}
	got := make(chan []byte, 1)
	go func() {
		// fd 1 is the PTY slave, open read-write: a byte an attach session
		// managed to write into the master arrives here (a line at a time,
		// the terminal being in canonical mode).
		buf := make([]byte, 4096)
		n, _ := os.Stdout.Read(buf)
		got <- buf[:n]
	}()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(stop); err == nil {
			break
		}
	}
	var b []byte
	select {
	case b = <-got:
	default:
	}
	if err := os.WriteFile(out+".tty", b, 0o600); err != nil {
		return 5
	}
	return 0
}

// holdStdin is the grandchild probeHoldEnv starts: it keeps its stdin (the
// prompt pipe) open and reads none of it.
func holdStdin() int {
	time.Sleep(30 * time.Second)
	return 0
}

// fdIsCtty reports whether fd is the calling process's controlling terminal:
// TIOCGPGRP fails with ENOTTY for any other terminal on Linux and macOS.
func fdIsCtty(fd int) bool {
	var pgrp int32
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(syscall.TIOCGPGRP), uintptr(unsafe.Pointer(&pgrp)))
	return errno == 0
}

// withProbeEnv appends KEY=VALUE lines to h's probe env_file.
func withProbeEnv(t *testing.T, h core.Harness, kv ...string) {
	t.Helper()
	f, err := os.OpenFile(h.EnvFile, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, line := range kv {
		if _, err := f.WriteString(line + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}

// spawnPromptAndReap spawns h directly (no run record), waits for it to exit,
// and finishes its stdin feed and PTY the way the supervisor's wait does.
func spawnPromptAndReap(t *testing.T, h core.Harness, run RunEnv) {
	t.Helper()
	proc, err := spawn(h, 80, 24, run)
	if err != nil {
		t.Fatalf("spawn %q: %v", h.Name, err)
	}
	_ = proc.cmd.Wait()
	proc.stdin.finish()
	_ = proc.pty.Close()
}

// largePrompt is n bytes that would not survive a terminal: every byte value,
// NULs and the ^C/^D/^Z a line discipline turns into signals and EOF, and
// lines far longer than MAX_CANON, with a marker the persistence checks look
// for.
func largePrompt(n int) string {
	var b bytes.Buffer
	b.WriteString("PROMPT-MARKER-7f3a ")
	r := rand.New(rand.NewPCG(505, 12))
	for b.Len() < n {
		switch b.Len() % 5000 {
		case 0:
			b.WriteString("\x03\x04\x1a\x00\n")
		default:
			b.WriteByte(byte(r.UintN(256)))
		}
	}
	return b.String()[:n]
}

// TestSpawnPromptArgvDelivery is REQ-12's "argv delivery": the prompt fills
// {{prompt}} as exactly one argument, whatever it contains — spaces, quotes,
// a newline and a template of its own, which stays literal because `prompt`
// is never template-expanded (REQ-5).
func TestSpawnPromptArgvDelivery(t *testing.T) {
	for _, prompt := range []string{"hello world", "print {{run.id}} literally \"q\" 'q'\n$(id); rm -rf /"} {
		h, out := probeHarness(t, "omp", testBinary(t), "--print", "{{prompt}}")
		h.Prompt = prompt
		spawnPromptAndReap(t, h, RunEnv{})
		p := readProbe(t, out)
		if want := []string{"--print", prompt}; !slices.Equal(p.Args, want) {
			t.Errorf("child received %d args %q, want exactly %q", len(p.Args), p.Args, want)
		}
		if !p.Stdin0TTY {
			t.Error("an argv delivery moved fd 0 off the PTY")
		}
		if p.PromptFileSet {
			t.Errorf("HARNESS_PROMPT_FILE = %q on an argv delivery", p.PromptFileEnv)
		}
	}
}

// TestStdinDeliveryDescriptorsSeenByShell is the acceptance check that `test
// -t 0` is false and `test -t 1` is true inside a stdin-delivery child, run
// by a real /bin/sh rather than inferred. The control is the same program
// with no prompt, whose fd 0 is the PTY, so the check is shown able to see a
// terminal on fd 0 at all.
func TestStdinDeliveryDescriptorsSeenByShell(t *testing.T) {
	script := `test -t 0; a=$?; test -t 1; b=$?; printf '%s %s' "$a" "$b" > "$0"`
	for _, tc := range []struct {
		name, prompt, want string
	}{
		{"stdin delivery", "hello", "1 0"},
		{"control: no prompt", "", "0 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "tty")
			h := core.Harness{
				Name: "sh-" + strings.ReplaceAll(tc.name, " ", "-"), Adapter: core.AdapterCommand,
				Argv:    []string{"/bin/sh", "-c", script, out},
				Backend: core.BackendNative, Restart: core.RestartNo,
			}
			if tc.prompt != "" {
				h.Prompt, h.PromptDelivery = tc.prompt, core.PromptDeliveryStdin
			}
			spawnPromptAndReap(t, h, RunEnv{})
			var got string
			waitFor(t, 10*time.Second, "the shell's report", func() bool {
				b, err := os.ReadFile(out)
				got = string(b)
				return err == nil && len(b) > 0
			})
			if got != tc.want {
				t.Errorf("`test -t 0; test -t 1` exit statuses = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestStdinDeliveryOfALargePrompt is REQ-12's "stdin delivery of a large
// prompt": a 1 MiB prompt holding every byte value, NULs, ^C/^D/^Z and lines
// far past MAX_CANON reaches a real child byte-identical through its stdin;
// the child sees fd 0 as a pipe and fd 1 as its controlling terminal; its
// output appears in the run log; and the prompt appears in none of the
// daemon's own records (REQ-11 "Nothing rendered is persisted").
func TestStdinDeliveryOfALargePrompt(t *testing.T) {
	e := newRunsEnv(t)
	h, out := commandSweep(t, "bulk")
	h.Prompt = largePrompt(1 << 20)
	h.PromptDelivery = core.PromptDeliveryStdin
	copied := filepath.Join(t.TempDir(), "stdin.bin")
	withProbeEnv(t, h, probeStdinOutEnv+"="+copied, probeSayEnv+"=probe-read-its-stdin")
	m, closeM := e.manager(t, sweepCfg(h), fastPolicy())

	m.StartRun("bulk", RunRequest{Trigger: TriggerSchedule})
	rec := waitRuns(t, m, "bulk", "the stdin run finishes", outcomesAre(OutcomeSuccess))[0]

	p := readProbe(t, out)
	got, err := os.ReadFile(copied)
	if err != nil {
		t.Fatalf("the child's copy of its stdin: %v", err)
	}
	if !bytes.Equal(got, []byte(h.Prompt)) {
		t.Errorf("child read %d bytes from stdin, want the %d-byte prompt byte-identical", len(got), len(h.Prompt))
	}
	if p.Stdin0TTY {
		t.Error("fd 0 is a terminal; want the prompt pipe")
	}
	if !p.Stdout1TTY {
		t.Error("fd 1 is not a terminal; want the PTY")
	}
	if !p.Fd1Ctty {
		t.Error("fd 1 is not the child's controlling terminal")
	}
	if log := readText(t, m.RunLogPath("bulk", rec.RunID)); !strings.Contains(log, "probe-read-its-stdin") {
		t.Errorf("the child's output is not in the run log:\n%s", log)
	}

	closeM()
	for _, path := range []string{e.state, filepath.Join(e.logs, "bulk.log")} {
		if strings.Contains(readText(t, path), "PROMPT-MARKER-7f3a") {
			t.Errorf("%s contains the prompt", path)
		}
	}
}

// TestStdinFeedCannotOutliveTheRun: a child that exits without reading its
// stdin, leaving a grandchild holding the pipe open, still has its feed
// goroutine finished by the time the run is recorded. The 1 MiB prompt is far
// larger than a pipe buffer, so the feed is blocked in Write when the child
// exits, and only the supervisor closing the write end can release it — the
// grandchild would hold it for 30 seconds.
func TestStdinFeedCannotOutliveTheRun(t *testing.T) {
	exited := make(chan struct{})
	var once sync.Once
	hook := func() { once.Do(func() { close(exited) }) }
	stdinFeedExited.Store(&hook)
	t.Cleanup(func() { stdinFeedExited.Store(nil) })

	e := newRunsEnv(t)
	h, _ := commandSweep(t, "holder")
	h.Prompt = largePrompt(1 << 20)
	h.PromptDelivery = core.PromptDeliveryStdin
	pidfile := filepath.Join(t.TempDir(), "holder.pid")
	withProbeEnv(t, h, probeHoldEnv+"="+pidfile)
	m, _ := e.manager(t, sweepCfg(h), fastPolicy())

	m.StartRun("holder", RunRequest{Trigger: TriggerSchedule})
	waitRuns(t, m, "holder", "the run finishes", outcomesAre(OutcomeSuccess))
	b, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatalf("the probe started no stdin holder: %v", err)
	}
	pid, _ := strconv.Atoi(string(b))
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("the stdin holder (pid %d) is already gone, so this test would pass without the fix: %v", pid, err)
	}

	select {
	case <-exited:
	default:
		t.Fatal("the run is recorded but its stdin feed goroutine is still running")
	}
}

// TestFileDelivery is REQ-12's "file delivery": run 5 of a file-delivery
// harness is handed <jobs dir>/<harness>/5.prompt through HARNESS_PROMPT_FILE
// and {{prompt_file}}; the file is 0600 and holds the prompt when the child
// starts; HARNESS_PROMPT_FILE beats the same name in the daemon's environment
// and in env_file; and pruning run 5 removes its prompt file, while the kept
// runs' files stay.
func TestFileDelivery(t *testing.T) {
	t.Setenv(promptFileVar, "/daemon/env/prompt")
	e := newRunsEnv(t)
	h, out := commandSweep(t, "review", "--instructions", "{{prompt_file}}")
	h.Prompt = "review the queue, then say so"
	h.PromptDelivery = core.PromptDeliveryFile
	h.KeepRuns = 2
	withProbeEnv(t, h, promptFileVar+"=/env_file/prompt")
	m, _ := e.manager(t, sweepCfg(h), fastPolicy())

	var p argvProbe
	for i := 1; i <= 7; i++ {
		_ = os.Remove(out)
		m.StartRun("review", RunRequest{Trigger: TriggerSchedule})
		waitRuns(t, m, "review", "run "+strconv.Itoa(i)+" finishes", func(rs []RunRecord) bool {
			return len(rs) > 0 && rs[len(rs)-1].RunID == i && rs[len(rs)-1].Outcome == OutcomeSuccess
		})
		if i == 5 {
			p = readProbe(t, out)
		}
	}

	want := filepath.Join(e.jobs, "review", "5.prompt")
	if p.PromptFileEnv != want {
		t.Errorf("run 5's HARNESS_PROMPT_FILE = %q (set %v), want %q over the daemon env and env_file values", p.PromptFileEnv, p.PromptFileSet, want)
	}
	if !slices.Equal(p.Args, []string{"--instructions", want}) {
		t.Errorf("run 5's args = %q, want {{prompt_file}} rendered to %q", p.Args, want)
	}
	if p.PromptFileMode != 0o600 {
		t.Errorf("the prompt file was mode %#o when the child started, want 0600", p.PromptFileMode)
	}
	if p.PromptFileBody != h.Prompt {
		t.Errorf("the prompt file held %q when the child started, want the prompt", p.PromptFileBody)
	}

	waitFor(t, 5*time.Second, "runs 1-5's prompt files are pruned with their records", func() bool {
		for id := 1; id <= 5; id++ {
			if _, err := os.Stat(filepath.Join(e.jobs, "review", strconv.Itoa(id)+".prompt")); err == nil {
				return false
			}
		}
		return true
	})
	// The control: the kept runs' prompt files are on disk, 0600, so the
	// absences above are pruning and not files that were never written.
	for _, id := range []int{6, 7} {
		fi, err := os.Stat(filepath.Join(e.jobs, "review", strconv.Itoa(id)+".prompt"))
		if err != nil {
			t.Errorf("kept run %d's prompt file: %v", id, err)
			continue
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("kept run %d's prompt file is mode %#o, want 0600", id, fi.Mode().Perm())
		}
	}
}

// TestFileDeliveryWithoutARunRecord: a start with no run record writes its
// prompt to <state>/prompts/<harness>.prompt, beside the logs directory.
func TestFileDeliveryWithoutARunRecord(t *testing.T) {
	e := newRunsEnv(t)
	h, out := probeHarness(t, "resident", testBinary(t))
	h.Prompt = "a resident's instructions"
	h.PromptDelivery = core.PromptDeliveryFile
	m, _ := e.manager(t, sweepCfg(h), fastPolicy())

	m.Start("resident")
	p := readProbe(t, out)
	want := filepath.Join(e.dir, "prompts", "resident.prompt")
	if p.PromptFileEnv != want || p.PromptFileBody != h.Prompt || p.PromptFileMode != 0o600 {
		t.Errorf("HARNESS_PROMPT_FILE = %q holding %q mode %#o; want %q holding the prompt, 0600", p.PromptFileEnv, p.PromptFileBody, p.PromptFileMode, want)
	}
}

// TestArgvOverThePlatformLimitFailsTheRun: a prompt too large for argv fails
// the start with ErrPromptDelivery, naming the harness and recommending file
// or stdin, runs nothing, and the run is recorded failed — never swallowed:
// the harness log carries the cause, and not the prompt.
func TestArgvOverThePlatformLimitFailsTheRun(t *testing.T) {
	prompt := "PROMPT-MARKER-7f3a " + strings.Repeat("x", 4<<20)

	h, out := probeHarness(t, "huge", testBinary(t), "{{prompt}}")
	h.Prompt = prompt
	proc, err := spawn(h, 80, 24, RunEnv{})
	if proc != nil {
		_ = proc.cmd.Wait()
		_ = proc.pty.Close()
		t.Fatal("a 4 MiB argv was exec'd")
	}
	if !errors.Is(err, ErrPromptDelivery) || !errors.Is(err, syscall.E2BIG) {
		t.Fatalf("err = %v, want ErrPromptDelivery wrapping E2BIG", err)
	}
	for _, want := range []string{`"huge"`, `prompt_delivery = "file" or "stdin"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "PROMPT-MARKER") {
		t.Error("the error carries the prompt")
	}
	if _, statErr := os.Stat(out); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("the probe ran (stat: %v)", statErr)
	}

	e := newRunsEnv(t)
	s, _ := commandSweep(t, "huge-run", "{{prompt}}")
	s.Prompt = prompt
	m, closeM := e.manager(t, sweepCfg(s), fastPolicy())
	m.StartRun("huge-run", RunRequest{Trigger: TriggerSchedule})
	waitRuns(t, m, "huge-run", "the run is recorded failed", outcomesAre(OutcomeFailed))
	closeM()
	log := readText(t, filepath.Join(e.logs, "huge-run.log"))
	if !strings.Contains(log, "prompt_delivery") {
		t.Errorf("the harness log does not record the delivery failure:\n%s", log)
	}
	if strings.Contains(log, "PROMPT-MARKER") {
		t.Error("the harness log contains the prompt")
	}
}

// TestPromptFileWriteFailureFailsTheStart: a file delivery whose prompt file
// cannot be written fails with ErrPromptDelivery and execs nothing.
func TestPromptFileWriteFailureFailsTheStart(t *testing.T) {
	h, out := probeHarness(t, "nofile", testBinary(t))
	h.Prompt = "x"
	h.PromptDelivery = core.PromptDeliveryFile
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	proc, err := spawn(h, 80, 24, RunEnv{PromptPath: filepath.Join(blocker, "nofile.prompt")})
	if proc != nil {
		_ = proc.cmd.Wait()
		_ = proc.pty.Close()
		t.Fatal("spawn started the program without its prompt file")
	}
	if !errors.Is(err, ErrPromptDelivery) {
		t.Fatalf("err = %v, want ErrPromptDelivery", err)
	}
	time.Sleep(50 * time.Millisecond)
	if _, statErr := os.Stat(out); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("the probe ran (stat: %v)", statErr)
	}
}

// syncBuffer collects an attach session's output across goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) write(p []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b.Write(p)
	return nil
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestAttachDuringStdinRun is REQ-12's "Attach during a stdin run": an
// operator attached read-write to a running stdin-delivery harness sees its
// output, what they type is not written to the child, and they are told why.
// The child itself reports what reached its terminal. The control runs the
// same probe without a prompt, where fd 0 is the PTY, and the typed line
// must arrive — so the empty report above it is a refusal, not a probe that
// cannot hear.
func TestAttachDuringStdinRun(t *testing.T) {
	for _, tc := range []struct {
		name    string
		stdin   bool
		arrives bool
	}{
		{"stdin run refuses input", true, false},
		{"control: a plain run receives it", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newRunsEnv(t)
			name := "watch"
			h, out := commandSweep(t, name)
			stop := filepath.Join(t.TempDir(), "stop")
			withProbeEnv(t, h, probeSayEnv+"=probe-visible-output", probeTTYWaitEnv+"="+stop)
			if tc.stdin {
				h.Prompt, h.PromptDelivery = "the prompt\n", core.PromptDeliveryStdin
				withProbeEnv(t, h, probeStdinOutEnv+"="+filepath.Join(t.TempDir(), "stdin"))
			}
			reg := attach.NewRegistry(0)
			m := NewManager(sweepCfg(h), ManagerOptions{
				Policy: fastPolicy(), StatePath: e.state, LogDir: e.logs,
				ExtraOutFor: reg.WriterFor, SizeFor: reg.SizeFor,
			})
			t.Cleanup(m.Close)
			reg.SetController(m)
			if err := m.Restore(); err != nil {
				t.Fatal(err)
			}

			m.StartRun(name, RunRequest{Trigger: TriggerSchedule})
			readProbe(t, out) // the child is up, past its stdin, watching fd 1

			var seen syncBuffer
			sess := reg.Mux(name).Attach(1, protocol.AttachRW, 80, 24, seen.write)
			defer sess.Detach()
			sess.Input([]byte("typed-by-operator\r"))
			sess.Input([]byte("more\r"))
			time.Sleep(300 * time.Millisecond)
			if err := os.WriteFile(stop, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			waitRuns(t, m, name, "the run finishes", outcomesAre(OutcomeSuccess))

			reached := readText(t, out+".tty")
			if got := strings.Contains(reached, "typed-by-operator"); got != tc.arrives {
				t.Errorf("typed input reached the child = %v (terminal read %q), want %v", got, reached, tc.arrives)
			}
			waitFor(t, 5*time.Second, "the attach shows the child's output", func() bool {
				return strings.Contains(seen.String(), "probe-visible-output")
			})
			notice := strings.Count(seen.String(), StdinPromptNotice)
			switch {
			case tc.stdin && notice != 1:
				t.Errorf("the client was told why %d times, want once for the burst:\n%q", notice, seen.String())
			case !tc.stdin && notice != 0:
				t.Errorf("a plain run's client was told its input is refused")
			}
		})
	}
}

// TestWireRefusesAPromptNothingDelivers is REQ-14 for prompt delivery: the
// project/scratchpad wire applies REQ-12's matrix, so a definition the file
// parser would refuse cannot be registered by hand.
func TestWireRefusesAPromptNothingDelivers(t *testing.T) {
	base := core.Harness{Name: "c", Adapter: core.AdapterCommand, Argv: []string{"tool"}, Prompt: "hi",
		Backend: core.BackendNative, Restart: core.RestartNo}
	for _, tc := range []struct {
		name   string
		mutate func(*core.Harness)
		want   string
	}{
		{"nothing delivers", func(*core.Harness) {}, "the prompt would be dropped"},
		{"stdin", func(h *core.Harness) { h.PromptDelivery = core.PromptDeliveryStdin }, ""},
		{"{{prompt}} under file", func(h *core.Harness) {
			h.PromptDelivery = core.PromptDeliveryFile
			h.Argv = []string{"tool", "{{prompt}}"}
		}, "only available with prompt_delivery"},
		{"delivery on crush", func(h *core.Harness) { h.Adapter, h.Argv, h.PromptDelivery = "crush", nil, core.PromptDeliveryStdin }, "only accepted on harness"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := base
			tc.mutate(&h)
			err := validateHarnessDef("scratchpad", h)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrInvalidProjectDef) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want ErrInvalidProjectDef containing %q", err, tc.want)
			}
		})
	}
	// Persisted project definitions keep the delivery, so a restored
	// harness is not reloaded into one that drops its prompt.
	h := base
	h.PromptDelivery = core.PromptDeliveryStdin
	if got := toPersistedProjectHarness(h).toCore().PromptDelivery; got != core.PromptDeliveryStdin {
		t.Errorf("persisted round trip PromptDelivery = %q", got)
	}
}
