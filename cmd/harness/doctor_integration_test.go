package main

// Governing: integration coverage for `harness doctor` against a real,
// in-process daemon. The daemon-suite test in internal/daemon covers the
// protocol; this file verifies the doctor's end-to-end wiring (config ok,
// daemon reachable, version match, harnesses healthy) using the same
// in-process boot pattern.

import (
	"testing"

	"gitea.stump.rocks/stump.wtf/harness/internal/cliui"
)

// TestRunDoctorHappyPath boots an in-process daemon with one disabled
// harness (the shared bootTestDaemon helper, which also matches the daemon
// version so the version check passes), then runs doctor against it. All
// four checks should pass and the exit code should be 0. The harness is
// disabled so the test doesn't spawn any subprocesses — "healthy" here means
// "not failed/degraded".
func TestRunDoctorHappyPath(t *testing.T) {
	cliui.SetJSON(true) // plain outputs, deterministic
	t.Cleanup(func() { cliui.SetJSON(false) })

	socket, configPath := bootTestDaemon(t)

	o := verbOpts{socket: socket, configPath: configPath, json: true}
	if code := runDoctor(o); code != 0 {
		t.Errorf("runDoctor = %d, want 0 (all checks should pass)", code)
	}
}
