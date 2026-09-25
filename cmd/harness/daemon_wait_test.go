package main

// Startup waits for the daemons the cmd/harness tests exec as real binaries.
//
// A fixed 10s poll was too short under `make test`, which runs every package
// in parallel: on 2026-09-24 TestCLIInterleavedFlagsAndPositional failed twice
// on a loaded machine with "never came up" (after 13.6s and 27s), then passed
// on a rerun. Only lengthening the wait would turn a daemon that died at boot
// into a minute of silence, so a wait here ends on whichever comes first: the
// daemon answering on its socket, the process exiting (reported at once, with
// its captured output), or a ceiling derived from the test's own -timeout.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// daemonStartupMax bounds how long a test waits for an exec'd daemon to
// answer. It is generous on purpose: a daemon that dies is caught by its exit,
// not by this, so the ceiling only has to separate "slow" from "hung".
const daemonStartupMax = 60 * time.Second

// daemonStartupCeiling is daemonStartupMax, shortened when the test binary's
// -timeout would fire first. The headroom lets a hang fail with the daemon's
// own output rather than the -timeout panic's goroutine dump.
func daemonStartupCeiling(t *testing.T) time.Duration {
	t.Helper()
	ceiling := daemonStartupMax
	if dl, ok := t.Deadline(); ok {
		if left := time.Until(dl) - 10*time.Second; left < ceiling {
			ceiling = max(left, time.Second)
		}
	}
	return ceiling
}

// daemonProc is a process exec'd by a test, its stdout and stderr captured to
// a file so a startup failure can say why.
type daemonProc struct {
	cmd     *exec.Cmd
	logPath string
	exited  chan struct{} // closed once cmd.Wait returns
}

// startDaemonProc execs bin with args and env, reaping it in the background
// so its exit is observable while a caller waits for its socket. The process
// is killed at cleanup.
func startDaemonProc(t *testing.T, bin string, env []string, args ...string) *daemonProc {
	t.Helper()
	// A file rather than a buffer: the child writes it directly, so reading it
	// mid-run is not a data race and cmd.Wait never waits on a copy goroutine.
	logPath := filepath.Join(shortSockDir(t), "daemon.out")
	f, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = f, f
	err = cmd.Start()
	_ = f.Close() // the child holds its own descriptor
	if err != nil {
		t.Fatalf("start %v: %v", args, err)
	}
	d := &daemonProc{cmd: cmd, logPath: logPath, exited: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(d.exited)
	}()
	t.Cleanup(d.stop)
	return d
}

// stop kills the process and waits for it to be reaped. Safe to call twice.
func (d *daemonProc) stop() {
	_ = d.cmd.Process.Kill()
	<-d.exited
}

// output is everything the process has written so far.
func (d *daemonProc) output() string {
	b, _ := os.ReadFile(d.logPath)
	return string(b)
}

// waitReady fails the test unless the daemon answers on socket before it
// exits or the startup ceiling passes.
func (d *daemonProc) waitReady(t *testing.T, bin string, env []string, socket string) {
	t.Helper()
	if err := d.awaitReady(bin, env, socket, daemonStartupCeiling(t)); err != nil {
		t.Fatal(err)
	}
}

// awaitReady is waitReady's error-returning core, so the failure paths can be
// tested directly. The error carries the process's exit status (or "still
// running") and its captured output.
func (d *daemonProc) awaitReady(bin string, env []string, socket string, ceiling time.Duration) error {
	err := awaitDaemon(bin, env, socket, ceiling, d.exited)
	if err == nil {
		return nil
	}
	status := "still running"
	select {
	case <-d.exited:
		status = d.cmd.ProcessState.String()
	default:
	}
	return fmt.Errorf("daemon %q (%s): %w\n--- daemon output ---\n%s",
		strings.Join(d.cmd.Args, " "), status, err, d.output())
}

// errDaemonExited is awaitDaemon's error when the process went away first.
var errDaemonExited = errors.New("exited before answering")

// awaitDaemon polls until a daemon answers `list` on socket, exited closes, or
// ceiling passes. A nil exited never fires, for a detached daemon that is not
// the test's child. Each probe dials the socket first and execs the client only
// once something is listening, so a loaded machine is not also paying for a
// fresh process every tick while the daemon is still booting.
func awaitDaemon(bin string, env []string, socket string, ceiling time.Duration, exited <-chan struct{}) error {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), ceiling)
	defer cancel()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		lastProbe := probeDaemon(ctx, bin, env, socket)
		if lastProbe == nil {
			return nil
		}
		select {
		case <-exited:
			return fmt.Errorf("%w on %s after %s (last probe: %v)",
				errDaemonExited, socket, time.Since(start).Round(time.Millisecond), lastProbe)
		case <-ctx.Done():
			return fmt.Errorf("not answering on %s after %s (last probe: %v)",
				socket, time.Since(start).Round(time.Millisecond), lastProbe)
		case <-tick.C:
		}
	}
}

// probeDaemon reports why the daemon on socket is not answering, or nil.
func probeDaemon(ctx context.Context, bin string, env []string, socket string) error {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		return err
	}
	_ = conn.Close()
	cmd := exec.CommandContext(ctx, bin, "--socket", socket, "list")
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("list: %v: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

// A daemon that dies at startup must fail the wait at once, with its own
// output, rather than after the whole ceiling. An unknown flag makes the real
// binary exit before it ever binds.
func TestAwaitDaemonReportsEarlyExit(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary; skipped under -short")
	}
	bin := buildHarnessBinary(t)
	env, _ := isolatedEnv(t)
	socket := filepath.Join(shortSockDir(t), "h.sock")

	d := startDaemonProc(t, bin, env, "daemon", "start", "--socket", socket, "--no-such-flag")
	ceiling := daemonStartupCeiling(t)
	start := time.Now()
	err := d.awaitReady(bin, env, socket, ceiling)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a daemon started with an unknown flag was reported ready")
	}
	if !errors.Is(err, errDaemonExited) {
		t.Errorf("want the exit reported, got: %v", err)
	}
	// Look past the header: it echoes the argv, which names the flag too.
	_, captured, _ := strings.Cut(err.Error(), "--- daemon output ---")
	if !strings.Contains(captured, "no-such-flag") {
		t.Errorf("the error does not carry the daemon's own complaint:\n%v", err)
	}
	if elapsed >= ceiling/2 {
		t.Errorf("took %s to notice the exit (ceiling %s); the wait is not watching the process", elapsed, ceiling)
	}
}

// A process that stays up but never binds must still fail once the ceiling
// passes, carrying whatever it printed — the check this helper exists to keep.
func TestAwaitDaemonTimesOutOnSilentProcess(t *testing.T) {
	env, _ := isolatedEnv(t)
	socket := filepath.Join(shortSockDir(t), "h.sock")

	d := startDaemonProc(t, "/bin/sh", env, "-c", "echo booting; exec sleep 30")
	// Let it print first, so the assertion on its output is not a race.
	for printed := time.Now(); !strings.Contains(d.output(), "booting"); time.Sleep(10 * time.Millisecond) {
		if time.Since(printed) > 10*time.Second {
			t.Fatal("the stand-in process never printed")
		}
	}
	err := d.awaitReady("/nonexistent", env, socket, 300*time.Millisecond)

	if err == nil {
		t.Fatal("a process that never bound was reported ready")
	}
	if errors.Is(err, errDaemonExited) {
		t.Errorf("a live process was reported as exited: %v", err)
	}
	for _, want := range []string{"not answering", "still running", "booting"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}
}
