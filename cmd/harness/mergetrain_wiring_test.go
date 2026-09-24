package main

// Merge Train Wiring Test
//
// Governing tests: SPEC-0025 REQ-1, REQ-12 — the daemon hands StartMergeTrain
// the loaded [mergetrain] table, the real process environment for the token
// variable, and its own state directory. Built from daemonMergeTrainOptions,
// the function runDaemon calls, not a copy of it (#315).

import (
	"context"
	"testing"

	"github.com/stump-wtf/harness/internal/config"
	"github.com/stump-wtf/harness/internal/daemon"
	"github.com/stump-wtf/harness/internal/supervisor"
)

func TestDaemonMergeTrainOptions(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("WIRING_MT_TOKEN", "from-the-environment")
	cfg, err := config.Parse([]byte(`
[mergetrain]
enabled = true
repos = ["stump.wtf/harness"]
forge_base_url = "https://gitea.example"
forge_token_env = "WIRING_MT_TOKEN"
`), "harness.toml")
	if err != nil {
		t.Fatal(err)
	}
	o := daemonMergeTrainOptions(cfg.MergeTrain)
	if !o.Config.Enabled || o.Config.Repos[0] != "stump.wtf/harness" || o.Config.Mode != "report" {
		t.Fatalf("Config = %+v, want the loaded table", o.Config)
	}
	if o.Getenv == nil || o.Getenv("WIRING_MT_TOKEN") != "from-the-environment" {
		t.Fatal("Getenv does not read the process environment")
	}
	if o.StateDir != supervisor.StateHome() {
		t.Fatalf("StateDir = %q, want %q", o.StateDir, supervisor.StateHome())
	}
	if o.Log == nil || o.NewForge != nil {
		t.Fatal("the daemon must log, and must use the real (Gitea) forge")
	}

	// No [mergetrain] table: the daemon's options start nothing.
	empty, err := config.Parse([]byte("[harness.a]\nharness = \"crush\"\n"), "harness.toml")
	if err != nil {
		t.Fatal(err)
	}
	m, err := daemon.StartMergeTrain(context.Background(), daemonMergeTrainOptions(empty.MergeTrain))
	if err != nil || m != nil {
		t.Fatalf("StartMergeTrain with no table = %v, %v; want nil, nil", m, err)
	}
}
