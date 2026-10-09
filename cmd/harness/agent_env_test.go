package main

// Agent package declared environment, end to end (issue #930): a package
// declaring [[env]] installs through the real command tree, its harness
// tables load through the real config loader, a real in-process daemon
// reports what each child would see, and doctor and describe render it.
// Every env file and the daemon's environment carry distinctive sentinel
// values, and no output anywhere may contain one.
//
// Governing: ADR-0044, ADR-0038; SPEC-0026 REQ-3, REQ-12.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stump-wtf/harness/internal/buildinfo"
	"github.com/stump-wtf/harness/internal/client"
	"github.com/stump-wtf/harness/internal/cliui"
	"github.com/stump-wtf/harness/internal/protocol"
)

const envPkg = `[package]
name = "issue-triager"
description = "triages issues"

[harness]
harness = "claude-code"

[[env]]
name        = "HT930_TOKEN"
required    = true
secret      = true
description = "forge token"

[[env]]
name        = "HT930_REPOS"
required    = true
description = "repo list"

[[env]]
name        = "HT930_OPTIONAL"
description = "extra flags for the triager"
`

// Sentinel values: each is unique, so finding one in output names the leak.
var envSentinels = []string{
	"sentinel-token-in-file-7f3a",
	"sentinel-repos-in-file-2b9c",
	"sentinel-optional-in-file-4d1e",
	"sentinel-repos-second-file-9a0f",
	"sentinel-token-daemon-env-6c2b",
	"sentinel-repos-overridden-5e8d",
}

func assertNoEnvValue(t *testing.T, where, out string) {
	t.Helper()
	for _, s := range envSentinels {
		if strings.Contains(out, s) {
			t.Errorf("%s leaked a value (%s):\n%s", where, s, out)
		}
	}
}

func writeEnvFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPackageEnvInstallDoctorDescribe(t *testing.T) {
	e := newAgentEnv(t)
	remote, _ := agentRemote(t, map[string]string{
		"packages/issue-triager/package.toml": envPkg,
	})
	if _, _, err := e.run("agent", "stable", "add", "stump-wtf", remote); err != nil {
		t.Fatal(err)
	}

	// agent info lists the declared variables, names only.
	info, _, err := e.run("agent", "info", "stump-wtf/issue-triager")
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	for _, want := range []string{
		"environment:",
		"HT930_TOKEN (required, secret): forge token",
		"HT930_REPOS (required): repo list",
		"HT930_OPTIONAL (optional): extra flags for the triager",
	} {
		if !strings.Contains(info, want) {
			t.Errorf("info is missing %q:\n%s", want, info)
		}
	}

	// install's confirmation lists them, and its output ENDS with a
	// ready-to-copy env_file skeleton, names only.
	out, _, err := e.run("agent", "install", "stump-wtf/issue-triager", "--yes")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if !strings.Contains(out, "environment:\n  HT930_TOKEN (required, secret): forge token\n") {
		t.Errorf("install confirmation does not list the declared environment:\n%s", out)
	}
	wantTail := strings.Join([]string{
		"# required, secret: forge token",
		"HT930_TOKEN=",
		"# required: repo list",
		"HT930_REPOS=",
		"# optional: extra flags for the triager",
		"# HT930_OPTIONAL=",
	}, "\n") + "\n"
	if !strings.HasSuffix(out, wantTail) {
		t.Errorf("install output must end with the env_file skeleton %q:\n%s", wantTail, out)
	}
	src := installedSource(t, e.cfgPath, "issue-triager")

	// Harness tables over the same pin, one per doctor outcome. The daemon's
	// own environment carries one sentinel; every file carries others.
	dir := e.stateDir
	full := writeEnvFile(t, dir, "full.env", "HT930_TOKEN=sentinel-token-in-file-7f3a\nexport HT930_REPOS=\"sentinel-repos-in-file-2b9c\"\nHT930_OPTIONAL=sentinel-optional-in-file-4d1e\n")
	noOpt := writeEnvFile(t, dir, "noopt.env", "HT930_TOKEN=sentinel-token-in-file-7f3a\nHT930_REPOS=sentinel-repos-in-file-2b9c\n")
	first := writeEnvFile(t, dir, "first.env", "HT930_TOKEN=sentinel-token-in-file-7f3a\nHT930_REPOS=sentinel-repos-overridden-5e8d\n")
	second := writeEnvFile(t, dir, "second.env", "HT930_REPOS=sentinel-repos-second-file-9a0f\nHT930_OPTIONAL=sentinel-optional-in-file-4d1e\n")
	blanked := writeEnvFile(t, dir, "blank.env", "HT930_TOKEN=\nHT930_REPOS=sentinel-repos-in-file-2b9c\nHT930_OPTIONAL=sentinel-optional-in-file-4d1e\n")
	t.Setenv("HT930_TOKEN", "sentinel-token-daemon-env-6c2b")
	t.Setenv("HT930_REPOS", "")
	t.Setenv("HT930_OPTIONAL", "")

	cfg := fmt.Sprintf(`[harness.nothing]
source = %[1]q

[harness.single-file]
source = %[1]q
env_file = %[2]q

[harness.no-optional]
source = %[1]q
env_file = %[3]q

[harness.file-list]
source = %[1]q
env_file = [%[4]q, %[5]q]

[harness.blanked]
source = %[1]q
env_file = %[6]q

[harness.hand-written]
harness = "generic"
args = ["true"]
`, src.String(), full, noOpt, first, second, blanked)
	if err := os.WriteFile(e.cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	socket := bootTestDaemonAt(t, e.cfgPath)
	o := verbOpts{socket: socket, configPath: e.cfgPath}

	// doctor --json: one agent_env row per package-sourced harness.
	cliui.SetJSON(true)
	jsonOut, jsonErr := captureStd(t, func() {
		if code := runDoctor(o); code != 1 {
			t.Errorf("runDoctor = %d, want 1 (a required variable is missing)", code)
		}
	})
	cliui.SetJSON(false)
	assertNoEnvValue(t, "doctor --json stdout", jsonOut)
	assertNoEnvValue(t, "doctor --json stderr", jsonErr)
	var res doctorResult
	if err := json.Unmarshal([]byte(jsonOut), &res); err != nil {
		t.Fatalf("doctor JSON: %v\n%s", err, jsonOut)
	}
	rows := map[string]checkResult{}
	for _, r := range res.AgentEnv {
		if r.Name != "agent_env" {
			t.Errorf("agent_env row named %q", r.Name)
		}
		harness, _, _ := strings.Cut(r.Detail, ": ")
		rows[harness] = r
	}
	expect := []struct {
		harness, status string
		contains        []string
		absent          []string
	}{
		// No env_file: the daemon's environment supplies the token alone.
		{"nothing", "error", []string{"required unset: HT930_REPOS", "optional unset: HT930_OPTIONAL (extra flags for the triager)"}, []string{"HT930_TOKEN"}},
		{"single-file", "ok", []string{"all 3 declared variable(s) set"}, nil},
		{"no-optional", "warn", []string{"optional unset: HT930_OPTIONAL (extra flags for the triager)"}, []string{"required unset"}},
		// A list: the second file supplies what the first lacks.
		{"file-list", "ok", []string{"all 3"}, nil},
		// An env_file line that sets a name to "" blanks the daemon's value
		// in the child, so it is unset — the composition the spawn uses.
		{"blanked", "error", []string{"required unset: HT930_TOKEN"}, nil},
	}
	for _, x := range expect {
		r, ok := rows[x.harness]
		if !ok {
			t.Errorf("no agent_env row for %s; rows: %+v", x.harness, res.AgentEnv)
			continue
		}
		if r.Status != x.status {
			t.Errorf("%s: status %q, want %q (%s)", x.harness, r.Status, x.status, r.Detail)
		}
		for _, c := range x.contains {
			if !strings.Contains(r.Detail, c) {
				t.Errorf("%s: detail %q does not contain %q", x.harness, r.Detail, c)
			}
		}
		for _, a := range x.absent {
			if strings.Contains(r.Detail, a) {
				t.Errorf("%s: detail %q must not contain %q", x.harness, r.Detail, a)
			}
		}
	}
	if _, ok := rows["hand-written"]; ok {
		t.Error("a hand-written harness declares nothing and gets no row")
	}
	if len(res.AgentEnv) != len(expect) {
		t.Errorf("got %d agent_env rows, want %d: %+v", len(res.AgentEnv), len(expect), res.AgentEnv)
	}

	// The human table carries the same rows and no value either.
	tableOut, tableErr := captureStd(t, func() { runDoctor(o) })
	assertNoEnvValue(t, "doctor stdout", tableOut)
	assertNoEnvValue(t, "doctor stderr", tableErr)
	if !strings.Contains(tableErr, "nothing: required unset: HT930_REPOS") || !strings.Contains(tableErr, "HT930_REPOS") {
		t.Errorf("doctor table is missing the agent_env row:\n%s", tableErr)
	}

	// describe shows each declared variable and whether it is satisfied,
	// with where the value comes from — never the value.
	c, err := client.Dial(socket, buildinfo.Version, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, x := range []struct {
		harness string
		want    []string
	}{
		{"nothing", []string{"HT930_TOKEN (required, secret) — set (environment)", "HT930_REPOS (required) — unset", "HT930_OPTIONAL (optional) — unset"}},
		{"file-list", []string{"HT930_TOKEN (required, secret) — set (env_file)", "HT930_REPOS (required) — set (env_file)"}},
	} {
		var derr error
		desc, descErr := captureStd(t, func() { derr = cmdDescribe(c, verbOpts{name: x.harness}) })
		if derr != nil {
			t.Fatalf("describe %s: %v", x.harness, derr)
		}
		assertNoEnvValue(t, "describe "+x.harness, desc+descErr)
		for _, w := range x.want {
			if !strings.Contains(desc, w) {
				t.Errorf("describe %s is missing %q:\n%s", x.harness, w, desc)
			}
		}
	}

	// describe --json carries names and presence, never a value.
	var derr error
	descJSON, _ := captureStd(t, func() { derr = cmdDescribe(c, verbOpts{name: "single-file", json: true}) })
	if derr != nil {
		t.Fatal(derr)
	}
	assertNoEnvValue(t, "describe --json", descJSON)
	var hi protocol.HarnessInfo
	if err := json.Unmarshal([]byte(descJSON), &hi); err != nil {
		t.Fatalf("describe JSON: %v\n%s", err, descJSON)
	}
	if len(hi.PackageEnv) != 3 || !hi.PackageEnv[0].Set || hi.PackageEnv[0].From != "env_file" {
		t.Errorf("describe --json package_env = %+v", hi.PackageEnv)
	}

	// A daemon too old to report (simulated: unsupported) gets a warn row
	// for a package whose installed manifest declares variables — never a
	// silent pass — and no row for a hand-written harness.
	old := &stubAgentsClient{list: []protocol.HarnessInfo{
		{Name: "nothing", Source: src.String()},
		{Name: "hand-written"},
	}}
	oldRows := packageEnvChecks(old, false)
	if len(oldRows) != 1 || oldRows[0].level != cliui.LevelWarn || !strings.Contains(oldRows[0].detail, "not checked") {
		t.Errorf("an old daemon must warn that the check did not run, got %+v", oldRows)
	}
}
