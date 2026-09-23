package main

// Second-Daemon Guard (issue #578)
//
// On tars the control socket disappeared while the daemon was alive and
// supervising, and every client said "daemon not running" for twenty minutes.
// The prime suspect is a second `harness daemon` that unlinks a "stale" socket
// before binding: it never touches the live daemon, it just deletes the only
// way to reach it.
//
// This exercises the real binary, because the guard has to hold on the path a
// person or a systemd unit actually takes — `harness daemon run` — and because
// an in-process test cannot show that the FIRST daemon is still reachable
// afterwards.
//
// @joestump 09/22/2026 - Added with the fix.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSecondDaemonRefusesALiveSocket(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary; skipped under -short")
	}
	bin := buildHarnessBinary(t)
	env, stateDir := isolatedEnv(t)
	socket, _ := startDaemonOnSocket(t, bin, env)

	// The second daemon gets its own config, with a harness only it knows
	// about, and it MUST be passed explicitly: isolatedEnv does not move
	// $XDG_CONFIG_HOME, so a bare `daemon run` loads the real user's
	// harness.toml and autostarts their harnesses on whatever machine runs
	// the suite.
	secondCfg := filepath.Join(stateDir, "second.toml")
	if err := os.WriteFile(secondCfg, []byte("[harness.secondonly]\nharness = \"generic\"\nargs = [\"-c\", \"sleep 600\"]\nenabled = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A regression here does not fail — it SERVES, forever, on a socket it
	// took from the daemon that was already there. Bound so CI cannot hang.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "daemon", "run", "--socket", socket, "--config", secondCfg)
	cmd.Env = env
	out, err := cmd.CombinedOutput()

	if err == nil {
		t.Fatalf("a second daemon started on a live socket; output:\n%s", out)
	}
	if ctx.Err() != nil {
		t.Fatalf("the second daemon kept running instead of refusing; output:\n%s", out)
	}
	if !strings.Contains(string(out), "already listening") {
		t.Errorf("second daemon output = %q, want it to say another daemon is already listening", out)
	}

	// The property the incident turned on: the FIRST daemon is still reachable.
	if !daemonAnswers(bin, env, socket) {
		t.Fatalf("the live daemon is no longer reachable at %s after a second daemon ran", socket)
	}

	// And the refusal came before anything with side effects. A daemon that
	// restores, autostarts and only then finds the socket taken has already
	// started its own copies of the harnesses, and its shutdown flushes its
	// state.json over the live daemon's; its private harness landing in
	// state.json is the deterministic trace of that.
	statePath := filepath.Join(stateDir, "harness", "state.json")
	state, readErr := os.ReadFile(statePath)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		t.Fatalf("read %s: %v", statePath, readErr)
	}
	if strings.Contains(string(state), "secondonly") {
		t.Errorf("the refused daemon restored, autostarted and saved state before refusing; state.json:\n%s", state)
	}
}
