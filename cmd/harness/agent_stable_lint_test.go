package main

// Stable Lint And Check
//
// Golden tests over the fixture stables in testdata/stables, run through the
// full command tree the binary uses: one clean stable, and one each with a
// schema violation, a high scan finding, a missing README, a name mismatch
// and a missing prompt file — exit codes and --json asserted against
// golden files. Two more stables pass lint and fail only check: an unknown
// adapter, and a prompt template requiring context a scheduled firing never
// supplies. TestAgentStableLintIsOffline proves neither verb dials the
// daemon socket, runs git, or touches the stable store.
//
// Regenerate the goldens with: go test ./cmd/harness -run TestAgentStableLintGolden -update-stable-lint
//
// Governing: ADR-0044, SPEC-0026 REQ-3, REQ-5, REQ-7, REQ-12.
//
// @joestump-agent 10/09/2026 - Added for harness#933.

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/agentpkg/stablelint"
	"github.com/stump-wtf/harness/internal/cliui"
)

var updateStableLint = flag.Bool("update-stable-lint", false, "rewrite testdata/stables/*/golden-lint.json")

// runStableVerb runs `harness agent stable <verb> <dir> [--json]` through
// the root command and returns stdout and the exit code main would use.
func runStableVerb(t *testing.T, verb, dir string, asJSON bool) (string, int) {
	t.Helper()
	cliui.SetJSON(false)
	t.Cleanup(func() { cliui.SetJSON(false) })
	root := newRootCmd()
	var out, errBuf bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errBuf)
	args := []string{"agent", "stable", verb, dir}
	if asJSON {
		args = append(args, "--json")
	}
	root.SetArgs(args)
	err := root.Execute()
	var ec exitCodeError
	switch {
	case err == nil:
		return out.String(), 0
	case errors.As(err, &ec):
		return out.String(), ec.code
	default:
		t.Fatalf("%s %s: unexpected error (stderr %q): %v", verb, dir, errBuf.String(), err)
		return "", -1
	}
}

func TestAgentStableLintGolden(t *testing.T) {
	cases := []struct {
		stable   string
		wantExit int
		// wantIDs are the error ids the package must carry, in order.
		wantIDs []string
	}{
		{"clean", 0, nil},
		{"schema", 1, []string{stablelint.IDManifest}},
		{"high", 1, []string{"scan.shell.pipe-to-shell"}},
		{"no-readme", 1, []string{stablelint.IDNoReadme}},
		{"name-mismatch", 1, []string{stablelint.IDNameMismatch}},
		{"missing-prompt", 1, []string{stablelint.IDMissingFile}},
	}
	for _, tc := range cases {
		t.Run(tc.stable, func(t *testing.T) {
			dir := filepath.Join("testdata", "stables", tc.stable)
			out, code := runStableVerb(t, "lint", dir, true)
			if code != tc.wantExit {
				t.Fatalf("exit %d, want %d; output:\n%s", code, tc.wantExit, out)
			}
			var reports []stablelint.Report
			if err := json.Unmarshal([]byte(out), &reports); err != nil {
				t.Fatalf("--json output is not a JSON array of reports: %v\n%s", err, out)
			}
			var gotIDs []string
			for _, r := range reports {
				if r.Errors == nil || r.Warnings == nil {
					t.Errorf("%s: errors/warnings must be [] not null: %s", r.Package, out)
				}
				for _, is := range r.Errors {
					gotIDs = append(gotIDs, is.ID)
				}
			}
			if strings.Join(gotIDs, ",") != strings.Join(tc.wantIDs, ",") {
				t.Errorf("error ids %v, want %v\n%s", gotIDs, tc.wantIDs, out)
			}

			golden := filepath.Join(dir, "golden-lint.json")
			if *updateStableLint {
				if err := os.WriteFile(golden, []byte(out), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("read golden (run with -update-stable-lint): %v", err)
			}
			if out != string(want) {
				t.Errorf("--json differs from %s:\n got:\n%s\nwant:\n%s", golden, out, want)
			}

			// The text form exits the same way; check is a superset of lint,
			// so a lint failure is a check failure with the same exit code.
			if _, code := runStableVerb(t, "lint", dir, false); code != tc.wantExit {
				t.Errorf("text lint exit %d, want %d", code, tc.wantExit)
			}
			if _, code := runStableVerb(t, "check", dir, false); code != tc.wantExit {
				t.Errorf("check exit %d, want %d", code, tc.wantExit)
			}
		})
	}
}

// TestAgentStableCheckCatchesLoadErrors pins what only check can see: both
// stables lint clean, and check fails each through the real config load.
func TestAgentStableCheckCatchesLoadErrors(t *testing.T) {
	for _, tc := range []struct{ stable, want string }{
		{"check-adapter", `unknown harness kind "no-such-agent"`},
		{"check-template", "{{run.source}} is absent on every scheduled firing"},
	} {
		t.Run(tc.stable, func(t *testing.T) {
			dir := filepath.Join("testdata", "stables", tc.stable)
			if out, code := runStableVerb(t, "lint", dir, false); code != 0 {
				t.Fatalf("lint must pass a load-only failure; exit %d:\n%s", code, out)
			}
			out, code := runStableVerb(t, "check", dir, true)
			if code != 1 {
				t.Fatalf("check exit %d, want 1:\n%s", code, out)
			}
			var reports []stablelint.Report
			if err := json.Unmarshal([]byte(out), &reports); err != nil {
				t.Fatalf("--json: %v\n%s", err, out)
			}
			if len(reports) != 1 || len(reports[0].Errors) != 1 {
				t.Fatalf("want one package with one error:\n%s", out)
			}
			got := reports[0].Errors[0]
			if got.ID != stablelint.IDLoad || !strings.Contains(got.Message, tc.want) {
				t.Errorf("error %+v, want id %s containing %q", got, stablelint.IDLoad, tc.want)
			}
		})
	}
}

// TestAgentStableCheckLoadsClean proves the load test is not vacuous on the
// clean stable: the text output names both packages as ok after check, so a
// check that silently skipped the load would still have to agree with lint
// — and TestAgentStableCheckCatchesLoadErrors shows the same path fails.
func TestAgentStableCheckLoadsClean(t *testing.T) {
	out, code := runStableVerb(t, "check", filepath.Join("testdata", "stables", "clean"), false)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for _, want := range []string{"agent: pr-reviewer: ok", "agent: watcher: ok", "2 package(s), 0 error(s), 0 warning(s)"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// TestAgentStableLintNotAStable fails a directory with no packages/ rather
// than reporting a clean run over nothing.
func TestAgentStableLintNotAStable(t *testing.T) {
	root := newRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"agent", "stable", "lint", t.TempDir()})
	err := root.Execute()
	if !errors.Is(err, stablelint.ErrNotStable) {
		t.Fatalf("err = %v, want ErrNotStable", err)
	}
}

// TestAgentStableLintIsOffline runs both verbs with a listening daemon
// socket, a git on PATH that records any invocation, and an empty state
// home, and asserts none of them was touched. The socket proves the
// listener could fire: a control dial is accepted before the verbs run.
func TestAgentStableLintIsOffline(t *testing.T) {
	// A short base: a unix socket path must fit in sun_path (104 bytes on
	// macOS), which t.TempDir() under /var/folders does not.
	base, err := os.MkdirTemp("", "hsl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })

	sock := filepath.Join(base, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var dials atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			dials.Add(1)
			c.Close()
		}
	}()
	t.Setenv("HARNESS_SOCKET", sock)

	bin := filepath.Join(base, "bin")
	marker := filepath.Join(base, "git-ran")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\necho \"$@\" >> "+marker+"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	state := filepath.Join(base, "state")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", state)

	// The controls: the listener counts a dial, and the sentinel records a
	// git run — so silence afterwards means "never happened", not "could
	// not have been seen".
	if c, err := net.Dial("unix", sock); err == nil {
		c.Close()
	} else {
		t.Fatal(err)
	}
	for i := 0; i < 200 && dials.Load() == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if dials.Load() != 1 {
		t.Fatalf("control dial not observed (got %d)", dials.Load())
	}
	_ = exec.Command("git", "control").Run()
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("git sentinel did not record the control run: %v", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}

	for _, verb := range []string{"lint", "check"} {
		for _, stable := range []string{"clean", "high", "check-adapter"} {
			runStableVerb(t, verb, filepath.Join("testdata", "stables", stable), true)
		}
	}

	if n := dials.Load(); n != 1 {
		t.Errorf("the verbs dialed the daemon socket %d time(s)", n-1)
	}
	if b, err := os.ReadFile(marker); err == nil {
		t.Errorf("the verbs ran git: %q", b)
	}
	entries, err := os.ReadDir(state)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the verbs wrote to the state home: %v", entries)
	}
}
