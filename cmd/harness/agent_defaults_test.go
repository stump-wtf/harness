package main

// Install-time Defaults, End To End
//
// SPEC-0026 REQ-6 and REQ-8 through the real command tree on a local bare
// repository: --yes accepts no suggestion and says so, --accept-defaults
// accepts them all and they land as the operator's own keys, the interactive
// flow keeps / overwrites / skips per key, a trigger naming an undeclared
// source is skipped rather than invented, and a suggestion the daemon's own
// loader would refuse is skipped naming that error. Upgrade reports added,
// changed and dropped suggestions, never moves a key the table already
// sets, and does not ask about a suggestion the new pin left unchanged.
//
// Governing: ADR-0044 (amendment of 2026-10-09), SPEC-0026 REQ-6, REQ-8,
// Error Handling Standards.
//
// @joestump-agent 10/10/2026 - Added for harness#931.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/agentpkg"
	"github.com/stump-wtf/harness/internal/config"
	"github.com/stump-wtf/harness/internal/core"
)

// defaultsPkg is the fixture manifest carrying a prompt source — so a
// suggested schedule or trigger has something to fire — plus a [defaults]
// table holding body.
func defaultsPkg(body string) string {
	src := strings.Replace(agentPkg, `harness = "claude-code"`, `harness = "claude-code"
prompt = "review the pull request"`, 1)
	if body == "" {
		return src
	}
	return src + "\n[defaults]\n" + body
}

func rawConfig(t *testing.T, e *agentEnv) string {
	t.Helper()
	data, err := os.ReadFile(e.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func loadedHarness(t *testing.T, e *agentEnv, name string) core.Harness {
	t.Helper()
	cfg, err := config.Load(e.cfgPath)
	if err != nil {
		t.Fatalf("the table the write produced must load: %v", err)
	}
	h, ok := cfg.Harnesses[name]
	if !ok {
		t.Fatalf("no [harness.%s] in:\n%s", name, rawConfig(t, e))
	}
	return h
}

// --yes consents to the trust review and to nothing else: every suggestion
// stays unset, each one is listed as skipped with the flag that would have
// taken it, and the loaded config proves [defaults] is inert (REQ-6).
func TestAgentInstallYesAcceptsNoDefaults(t *testing.T) {
	e := newAgentEnv(t)
	e.writeFile("harness.toml", "[server]\nenabled = false\n")
	remote, _ := agentRemote(t, map[string]string{
		"packages/pr-reviewer/package.toml": defaultsPkg("timeout = \"45m\"\non_overlap = \"queue\"\n"),
	})
	if _, _, err := e.run("stable", "add", "stump-wtf", remote); err != nil {
		t.Fatal(err)
	}

	out, _, err := e.run("agent", "install", "stump-wtf/pr-reviewer", "--yes")
	if err != nil {
		t.Fatalf("install failed: %v (out: %s)", err, out)
	}
	data := rawConfig(t, e)
	for _, key := range []string{"timeout", "on_overlap"} {
		if strings.Contains(data, key+" =") {
			t.Fatalf("--yes must leave %q unset:\n%s", key, data)
		}
	}
	for _, want := range []string{"timeout = 45m: skipped", "on_overlap = queue: skipped", "--accept-defaults"} {
		if !strings.Contains(out, want) {
			t.Fatalf("the output must list each skipped suggestion with the reason (want %q):\n%s", want, out)
		}
	}
	h := loadedHarness(t, e, "pr-reviewer")
	if h.Timeout != 0 || string(h.OnOverlap) != "" {
		t.Fatalf("config load must apply no suggestion: timeout=%v on_overlap=%q", h.Timeout, h.OnOverlap)
	}
}

// --accept-defaults counts as the operator's choice even unattended, so the
// chosen keys land beside source in the same write, in the table, and the
// output says written.
func TestAgentInstallAcceptDefaultsWritesThem(t *testing.T) {
	e := newAgentEnv(t)
	e.writeFile("harness.toml", "[server]\nenabled = false\n")
	remote, _ := agentRemote(t, map[string]string{
		"packages/pr-reviewer/package.toml": defaultsPkg("schedule = \"0 9 * * 1-5\"\ntimeout = \"45m\"\non_overlap = \"queue\"\nrestart_delay = 30\n"),
	})
	if _, _, err := e.run("stable", "add", "stump-wtf", remote); err != nil {
		t.Fatal(err)
	}

	out, _, err := e.run("agent", "install", "stump-wtf/pr-reviewer", "--yes", "--accept-defaults")
	if err != nil {
		t.Fatalf("install failed: %v (out: %s)", err, out)
	}
	data := rawConfig(t, e)
	if i := strings.Index(data, "[harness.pr-reviewer]"); i < 0 {
		t.Fatalf("no table:\n%s", data)
	} else if table := data[i:]; !strings.Contains(table, `timeout = "45m"`) ||
		!strings.Contains(table, `on_overlap = "queue"`) || !strings.Contains(table, "restart_delay = 30") {
		t.Fatalf("the chosen defaults must be written into the table:\n%s", table)
	}
	for _, want := range []string{"timeout = 45m: written", "on_overlap = queue: written", "restart_delay = 30: written"} {
		if !strings.Contains(out, want) {
			t.Fatalf("the output must list each written default (want %q):\n%s", want, out)
		}
	}
	h := loadedHarness(t, e, "pr-reviewer")
	if h.Timeout != 45*time.Minute || string(h.OnOverlap) != "queue" || h.RestartDelay != 30*time.Second {
		t.Fatalf("the loaded table must carry the chosen values: %+v", h)
	}
}

// The interactive flow asks per key, in REQ-3 order, three ways: keep it,
// overwrite it with a value the parser accepts, or skip it. An overwrite
// the parser rejects is reported and asked for again, never written.
func TestAgentInstallDefaultsInteractive(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		want  string
		deny  string
		needs []string
	}{
		{
			name:  "keep, overwrite, skip",
			lines: []string{"y\n", "a\n", "a\n", "o\n", "replace\n", "s\n"},
			want:  "timeout = 45m: written",
			needs: []string{`timeout = "45m"`, `on_overlap = "replace"`, `schedule = "0 9 * * 1-5"`},
			deny:  "catch_up",
		},
		{
			name:  "an invalid overwrite is asked for again",
			lines: []string{"y\n", "a\n", "o\n", "45 minutes\n", "a\n", "s\n", "s\n"},
			want:  "invalid timeout",
			needs: []string{`timeout = "45m"`, `schedule = "0 9 * * 1-5"`},
			deny:  "catch_up",
		},
		{
			name:  "skip everything",
			lines: []string{"y\n", "s\n", "s\n", "s\n", "s\n"},
			want:  "timeout = 45m: skipped (you declined it)",
			deny:  "timeout =",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newAgentEnv(t)
			e.writeFile("harness.toml", "[server]\nenabled = false\n")
			remote, _ := agentRemote(t, map[string]string{
				"packages/pr-reviewer/package.toml": defaultsPkg("schedule = \"0 9 * * 1-5\"\ntimeout = \"45m\"\non_overlap = \"queue\"\ncatch_up = true\n"),
			})
			if _, _, err := e.run("stable", "add", "stump-wtf", remote); err != nil {
				t.Fatal(err)
			}
			ttyStdin(t, tc.lines...)
			out, _, err := e.run("agent", "install", "stump-wtf/pr-reviewer")
			if err != nil {
				t.Fatalf("install failed: %v (out: %s)", err, out)
			}
			if !strings.Contains(out, tc.want) {
				t.Fatalf("output must report %q:\n%s", tc.want, out)
			}
			data := rawConfig(t, e)
			for _, want := range tc.needs {
				if !strings.Contains(data, want) {
					t.Fatalf("the table must carry %s:\n%s", want, data)
				}
			}
			if table := data[strings.Index(data, "[harness.pr-reviewer]"):]; strings.Contains(table, tc.deny+" =") {
				t.Fatalf("the table must not carry %q:\n%s", tc.deny, table)
			}
		})
	}
}

// A triggers suggestion is only as good as the sources it names: a source
// the config does not declare means the whole suggestion is skipped naming
// it — install never creates a trigger source — and the install still
// succeeds without it.
func TestAgentInstallDefaultsTriggersNeedDeclaredSources(t *testing.T) {
	cases := []struct {
		name    string
		global  string
		written bool
	}{
		{name: "undeclared source is skipped", global: "[server]\nenabled = false\n"},
		{
			name:    "declared sources are accepted",
			global:  "[channel.sb]\nurl = \"https://sb.example.com/mcp/x\"\n[webhook.gh]\nverify = \"bearer\"\nenv_file = \"gh.env\"\nsecret = \"${GH_HOOK_SECRET}\"\n",
			written: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newAgentEnv(t)
			e.writeFile("harness.toml", tc.global)
			// A webhook source is not a unit until its secret travels with it,
			// so the declared fixture keeps an env file beside the config.
			e.writeFile("gh.env", "GH_HOOK_SECRET=testhooksecret\n")
			remote, _ := agentRemote(t, map[string]string{
				"packages/pr-reviewer/package.toml": defaultsPkg("triggers = [\"channel.sb\", \"webhook.gh\"]\n"),
			})
			if _, _, err := e.run("stable", "add", "stump-wtf", remote); err != nil {
				t.Fatal(err)
			}
			out, _, err := e.run("agent", "install", "stump-wtf/pr-reviewer", "--yes", "--accept-defaults")
			if err != nil {
				t.Fatalf("install failed: %v (out: %s)", err, out)
			}
			data := rawConfig(t, e)
			table := data[strings.Index(data, "[harness.pr-reviewer]"):]
			if tc.written != strings.Contains(table, "triggers") {
				t.Fatalf("triggers written = %v, want %v:\n%s", !tc.written == false, tc.written, table)
			}
			if !tc.written {
				for _, want := range []string{"webhook.gh", "install never creates one"} {
					if !strings.Contains(out, want) {
						t.Fatalf("the skip must name the missing source %q:\n%s", want, out)
					}
				}
			}
			if _, err := config.Load(e.cfgPath); err != nil {
				t.Fatalf("load: %v", err)
			}
		})
	}
}

// The candidate table is loaded with the daemon's own loader before the
// write, so a suggestion that would break the file is skipped naming the
// loader's error rather than leaving the operator a config that will not
// load: timeout with nothing to time out.
func TestAgentInstallDefaultsSkipsWhatTheLoaderRefuses(t *testing.T) {
	e := newAgentEnv(t)
	e.writeFile("harness.toml", "[server]\nenabled = false\n")
	remote, _ := agentRemote(t, map[string]string{
		"packages/pr-reviewer/package.toml": defaultsPkg("timeout = \"45m\"\nworkdir = \"srv/triage\"\n"),
	})
	if _, _, err := e.run("stable", "add", "stump-wtf", remote); err != nil {
		t.Fatal(err)
	}
	out, _, err := e.run("agent", "install", "stump-wtf/pr-reviewer", "--yes", "--accept-defaults")
	if err != nil {
		t.Fatalf("install failed: %v (out: %s)", err, out)
	}
	if !strings.Contains(out, `timeout = 45m: skipped (the config loader refused it`) {
		t.Fatalf("the refused suggestion must name the loader's error:\n%s", out)
	}
	if !strings.Contains(out, `requires "schedule" or "triggers"`) {
		t.Fatalf("the loader's own wording must reach the operator:\n%s", out)
	}
	if !strings.Contains(out, "workdir = srv/triage: written") {
		t.Fatalf("the suggestion the loader accepts must still be written:\n%s", out)
	}
	data := rawConfig(t, e)
	table := data[strings.Index(data, "[harness.pr-reviewer]"):]
	if strings.Contains(table, "timeout =") {
		t.Fatalf("a suggestion the loader refuses must not be written:\n%s", table)
	}
	if !strings.Contains(table, `workdir = "srv/triage"`) {
		t.Fatalf("workdir must be written:\n%s", table)
	}
	// Install never creates a suggested workdir.
	if _, err := os.Stat(filepath.Join(e.stateDir, "srv")); err == nil {
		t.Fatalf("install must not create the suggested workdir")
	}
}

// A suggestion the operator already accepted is theirs: an upgrade that
// changes it reports the change and leaves the written value
// byte-identical, even under --accept-defaults (REQ-8).
func TestAgentUpgradeNeverMovesAnAcceptedDefault(t *testing.T) {
	e := newAgentEnv(t)
	e.writeFile("harness.toml", "[server]\nenabled = false\n")
	remote, work := agentRemote(t, map[string]string{
		"packages/pr-reviewer/package.toml": defaultsPkg("timeout = \"45m\"\nschedule = \"0 9 * * 1-5\"\n"),
	})
	if _, _, err := e.run("stable", "add", "stump-wtf", remote); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.run("agent", "install", "stump-wtf/pr-reviewer", "--yes", "--accept-defaults"); err != nil {
		t.Fatal(err)
	}
	before := rawConfig(t, e)
	table := before[strings.Index(before, "[harness.pr-reviewer]"):]
	if !strings.Contains(table, `timeout = "45m"`) || !strings.Contains(table, `schedule = "0 9 * * 1-5"`) {
		t.Fatalf("install must have written both suggestions:\n%s", table)
	}

	pushManifest(t, remote, work, defaultsPkg("timeout = \"2h\"\nschedule = \"0 9 * * 1-5\"\n"))
	if _, _, err := e.run("stable", "update", "stump-wtf"); err != nil {
		t.Fatal(err)
	}
	out, _, err := e.run("agent", "upgrade", "stump-wtf/pr-reviewer", "--yes", "--accept-defaults")
	if err != nil {
		t.Fatalf("upgrade failed: %v (out: %s)", err, out)
	}
	if !strings.Contains(out, "changed suggestion timeout: 45m -> 2h") {
		t.Fatalf("the report must name the changed suggestion with both values:\n%s", out)
	}
	if strings.Contains(out, "timeout = 45m: written") || strings.Contains(out, "timeout = 2h: written") {
		t.Fatalf("an accepted key is never offered, let alone written:\n%s", out)
	}
	if !strings.Contains(out, "timeout = 45m: kept") {
		t.Fatalf("the output must say the table already sets it:\n%s", out)
	}
	after := rawConfig(t, e)
	if table := after[strings.Index(after, "[harness.pr-reviewer]"):]; !strings.Contains(table, `timeout = "45m"`) {
		t.Fatalf("the operator's value must be byte-identical:\n%s", table)
	}
	if _, err := config.Load(e.cfgPath); err != nil {
		t.Fatalf("load: %v", err)
	}
}

// Upgrade reports what the new pin added and what it dropped, offers only
// the added or changed suggestions, and does not ask twice about one the
// package left alone: a suggestion skipped at install comes back only when
// the package changes it.
func TestAgentUpgradeOffersOnlyChangedDefaults(t *testing.T) {
	e := newAgentEnv(t)
	e.writeFile("harness.toml", "[server]\nenabled = false\n")
	remote, work := agentRemote(t, map[string]string{
		"packages/pr-reviewer/package.toml": defaultsPkg("timeout = \"45m\"\nworkdir = \"srv/old\"\n"),
	})
	if _, _, err := e.run("stable", "add", "stump-wtf", remote); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.run("agent", "install", "stump-wtf/pr-reviewer", "--yes"); err != nil {
		t.Fatal(err)
	}

	pushManifest(t, remote, work, defaultsPkg("timeout = \"45m\"\non_overlap = \"queue\"\n"))
	if _, _, err := e.run("stable", "update", "stump-wtf"); err != nil {
		t.Fatal(err)
	}
	out, _, err := e.run("agent", "upgrade", "stump-wtf/pr-reviewer", "--yes")
	if err != nil {
		t.Fatalf("upgrade failed: %v (out: %s)", err, out)
	}
	for _, want := range []string{
		"new suggestion on_overlap: queue",
		"dropped suggestion workdir: srv/old (your table keeps what it holds)",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("the report must state %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "default timeout") {
		t.Fatalf("an unchanged suggestion must not be offered again:\n%s", out)
	}
	if !strings.Contains(out, "on_overlap = queue: skipped") {
		t.Fatalf("--yes must list the new suggestion as skipped:\n%s", out)
	}
	data := rawConfig(t, e)
	table := data[strings.Index(data, "[harness.pr-reviewer]"):]
	if strings.Contains(table, "on_overlap") {
		t.Fatalf("--yes writes no suggestion:\n%s", table)
	}
	src := installedSource(t, e.cfgPath, "pr-reviewer")
	if src.SHA == "" {
		t.Fatalf("the upgrade must still have advanced the pin: %+v", src)
	}
}

// The report and info mark the table's suggestions as suggestions: nothing
// on a pin's manifest reaches a running harness by config load.
func TestAgentInfoShowsDefaultsAsSuggestions(t *testing.T) {
	e := newAgentEnv(t)
	remote, _ := agentRemote(t, map[string]string{
		"packages/pr-reviewer/package.toml": defaultsPkg("timeout = \"45m\"\n"),
	})
	if _, _, err := e.run("stable", "add", "stump-wtf", remote); err != nil {
		t.Fatal(err)
	}
	out, _, err := e.run("agent", "info", "stump-wtf/pr-reviewer")
	if err != nil {
		t.Fatalf("info failed: %v (out: %s)", err, out)
	}
	if !strings.Contains(out, "suggested defaults:") || !strings.Contains(out, "suggests timeout = 45m") {
		t.Fatalf("info must show the suggestion as a suggestion:\n%s", out)
	}
	if _, _, err := e.run("agent", "install", "stump-wtf/pr-reviewer", "--yes"); err != nil {
		t.Fatal(err)
	}
	out, _, err = e.run("agent", "info", "stump-wtf/pr-reviewer")
	if err != nil {
		t.Fatalf("info failed: %v (out: %s)", err, out)
	}
	if !strings.Contains(out, "suggested defaults:") {
		t.Fatalf("the section is always shown:\n%s", out)
	}
}

// A manifest with no [defaults] at all asks nothing, says nothing about
// defaults, and writes nothing but source.
func TestAgentInstallWithoutDefaultsWritesOnlySource(t *testing.T) {
	e := newAgentEnv(t)
	e.writeFile("harness.toml", "[server]\nenabled = false\n")
	remote, _ := agentRemote(t, map[string]string{"packages/pr-reviewer/package.toml": agentPkg})
	if _, _, err := e.run("stable", "add", "stump-wtf", remote); err != nil {
		t.Fatal(err)
	}
	out, _, err := e.run("agent", "install", "stump-wtf/pr-reviewer", "--yes", "--accept-defaults")
	if err != nil {
		t.Fatalf("install failed: %v (out: %s)", err, out)
	}
	if strings.Contains(out, "default ") {
		t.Fatalf("a package that suggests nothing must say nothing about defaults:\n%s", out)
	}
	head := agentGit(t, agentpkg.StableDir("stump-wtf"), "rev-parse", "HEAD")
	src := installedSource(t, e.cfgPath, "pr-reviewer")
	if src.SHA != strings.TrimSpace(head) {
		t.Fatalf("source = %v, want the tip %s", src, head)
	}
}

// pushManifest commits one manifest to the fixture remote, the way a stable
// author ships a new version.
func pushManifest(t *testing.T, remote, work, manifest string) {
	t.Helper()
	path := filepath.Join(work, "packages", "pr-reviewer", "package.toml")
	if err := os.WriteFile(path, []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	agentGit(t, work, "add", "-A")
	agentGit(t, work, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qm", "next")
	agentGit(t, work, "push", "-q", remote, "main")
}
