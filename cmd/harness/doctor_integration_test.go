package main

// Governing: integration coverage for `harness doctor` against a real,
// in-process daemon. The daemon-suite test in internal/daemon covers the
// protocol; this file verifies the doctor's end-to-end wiring (config ok,
// daemon reachable, version match, harnesses healthy) using the same
// in-process boot pattern.

import (
	"os"
	"path/filepath"
	"testing"

	"gitea.stump.rocks/stump.wtf/harness/internal/attach"
	"gitea.stump.rocks/stump.wtf/harness/internal/buildinfo"
	"gitea.stump.rocks/stump.wtf/harness/internal/cliui"
	"gitea.stump.rocks/stump.wtf/harness/internal/config"
	"gitea.stump.rocks/stump.wtf/harness/internal/daemon"
	"gitea.stump.rocks/stump.wtf/harness/internal/supervisor"
)

// TestRunDoctorHappyPath boots an in-process daemon with one disabled
// harness, then runs doctor against it. All four checks should pass and the
// exit code should be 0. The harness is disabled so the test doesn't spawn
// any subprocesses — "healthy" here means "not failed/degraded".
func TestRunDoctorHappyPath(t *testing.T) {
	cliui.SetJSON(true) // plain outputs, deterministic
	t.Cleanup(func() { cliui.SetJSON(false) })

	tmp := t.TempDir()
	configPath := filepath.Join(tmp, "harness.toml")
	if err := writeMinimalConfig(configPath); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	// Unix socket paths are length-limited (~108 bytes); the test tempdir can
	// be long, so put the socket under a short /tmp dir.
	sockDir, err := os.MkdirTemp("/tmp", "hnd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	socket := filepath.Join(sockDir, "d.sock")

	reg := attach.NewRegistry(1000)
	mgr := supervisor.NewManager(cfg, supervisor.ManagerOptions{
		StatePath:   filepath.Join(tmp, "state.json"),
		LogDir:      filepath.Join(tmp, "logs"),
		ExtraOutFor: reg.WriterFor,
	})
	reg.SetController(mgr)

	srv := daemon.NewServer(daemon.Options{
		Manager:    mgr,
		Registry:   reg,
		SocketPath: socket,
		ConfigPath: configPath,
		Version:    buildinfo.Version, // match so the version check passes
	})
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	t.Cleanup(func() {
		srv.Close()
		mgr.Close()
	})

	o := verbOpts{socket: socket, configPath: configPath, json: true}
	if code := runDoctor(o); code != 0 {
		t.Errorf("runDoctor = %d, want 0 (all checks should pass)", code)
	}
}
