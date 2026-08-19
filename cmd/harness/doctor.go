package main

// Governing: SPEC-0001 REQ "Zero And Error States" (the cockpit and CLI
// share one calm-ops voice; `harness doctor` is the user-facing health check
// that surfaces every common breakage — missing config, daemon not running,
// version skew, harnesses failed/flapping — as a single tabular
// pass/warn/fail report). ADR-0002 (clients are thin: doctor dials the
// daemon like any other verb, one HELLO + two control calls then close);
// ADR-0004 (the local Unix socket is the transport, so a missing socket is
// the single most common failure mode and earns a specific row); ADR-0006
// (config is TOML parsed via config.Load, so a parse failure is the other
// common one); SPEC-0003 (the state glyphs distinguish healthy from
// degraded harnesses — we map per-harness state into the doctor's
// pass/warn/fail levels).

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"charm.land/lipgloss/v2"

	"gitea.stump.rocks/stump.wtf/harness/internal/buildinfo"
	"gitea.stump.rocks/stump.wtf/harness/internal/client"
	"gitea.stump.rocks/stump.wtf/harness/internal/cliui"
	"gitea.stump.rocks/stump.wtf/harness/internal/config"
	"gitea.stump.rocks/stump.wtf/harness/internal/core"
	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// check is one row in the doctor table.
type check struct {
	name   string      // "config", "daemon", …
	level  cliui.Level // pass/warn/fail
	detail string      // human-readable status ("2 harnesses", "v1.2.3", …)
	hint   string      // actionable fix when level != Success; "" otherwise
}

// doctorResult is the JSON shape emitted when --json is set. One object per
// check plus an aggregate summary. Scripts can consume this with jq.
type doctorResult struct {
	Config  checkResult  `json:"config"`
	Daemon  *checkResult `json:"daemon,omitempty"`
	Version *checkResult `json:"version,omitempty"`
	Harness *checkResult `json:"harness,omitempty"`
	// Profile (#99) and Autostart are conditional rows: present only when the
	// condition fires. Both already counted toward the summary tally, but had no
	// object here, so `--json` reported `warned: 1` with nothing to point at.
	Profile   *checkResult  `json:"profile,omitempty"`
	Autostart *checkResult  `json:"autostart,omitempty"`
	Ssh       *checkResult  `json:"ssh,omitempty"`
	Summary   summaryResult `json:"summary"`
}

type checkResult struct {
	Status string `json:"status"` // "ok" | "warn" | "error"
	Name   string `json:"name"`
	Detail string `json:"detail"`
	Hint   string `json:"hint,omitempty"`
}

type summaryResult struct {
	Passed int `json:"passed"`
	Warned int `json:"warned"`
	Failed int `json:"failed"`
}

// runDoctor runs the health-check battery and renders a single tabular
// report to stderr (one row per check + a summary row). Returns the exit
// code the process should use (0 if all passed, 1 if any failed). Doctor
// owns its entire output surface — the caller must NOT route the returned
// code through cliui.Fatal, since the table already conveys everything.
//
// The checks are ordered cheapest-first; a daemon that can't be reached
// still lets you see the config + summary rows. When --json is set, a
// machine-readable doctorResult object is emitted on stdout instead.
func runDoctor(o verbOpts) int {
	var rows []check

	// --- Check 1: config file exists and parses ----------------------------
	// Governing: ADR-0006 (TOML config is the source of truth).
	cfgPath := o.configPath
	if cfgPath == "" {
		cfgPath = config.DefaultPath()
	}
	cfg, cfgErr := config.Load(cfgPath)
	switch {
	case cfgErr == nil:
		rows = append(rows, check{
			name:   "config",
			level:  cliui.LevelSuccess,
			detail: fmt.Sprintf("%s — %d harnesses", cfgPath, len(cfg.Harnesses)),
		})
	case cliui.IsMissingConfig(cfgErr):
		rows = append(rows, check{
			name:   "config",
			level:  cliui.LevelError,
			detail: fmt.Sprintf("not found at %s", cfgPath),
			hint:   "create one (see `harness daemon -h`) or pass --config PATH",
		})
	default:
		rows = append(rows, check{
			name:   "config",
			level:  cliui.LevelError,
			detail: fmt.Sprintf("parse failed: %v", cfgErr),
			hint:   "fix the TOML syntax and re-run `harness doctor`",
		})
	}

	// --- Check 2: daemon reachable -----------------------------------------
	// Governing: ADR-0002 (thin client dials the Unix socket); ADR-0004.
	c, daemonErr := client.Dial(o.socket, buildinfo.Version, nil)
	if daemonErr != nil {
		// Distinguish "nothing at the socket" from "a daemon is there but
		// rejected our handshake (proto version mismatch)". Both fail Dial,
		// but the hint differs: the first wants `harness daemon`, the second
		// wants a daemon restart (PR #23 nit).
		row := check{
			name:  "daemon",
			level: cliui.LevelError,
		}
		if strings.Contains(daemonErr.Error(), "incompatible") {
			row.detail = fmt.Sprintf("proto mismatch at %s (%v)", o.socket, daemonErr)
			row.hint = "restart the daemon to pick up the new binary"
		} else {
			row.detail = fmt.Sprintf("unreachable at %s", o.socket)
			row.hint = "start it with: harness daemon"
		}
		rows = append(rows, row)
		// No point continuing: every later check needs the daemon.
		emitDoctor(os.Stdout, os.Stderr, rows)
		return 1
	}
	defer c.Close()
	rows = append(rows, check{
		name:   "daemon",
		level:  cliui.LevelSuccess,
		detail: fmt.Sprintf("listening at %s", o.socket),
	})

	// --- Check 3: version match (client vs daemon) -------------------------
	// Governing: SPEC-0002 REQ "Handshake And Versioning" (proto major must
	// match; build version is informational but worth surfacing on skew,
	// #181). The shared SkewNotice decides what counts as skew: a dev build
	// next to any daemon is the normal dev workflow and stays silent.
	di, err := c.DaemonInfo()
	switch {
	case err != nil:
		rows = append(rows, check{
			name:   "version",
			level:  cliui.LevelWarn,
			detail: fmt.Sprintf("couldn't fetch daemon info: %v", err),
		})
	default:
		if notice := buildinfo.SkewNotice(di.Version, buildinfo.Version); notice != "" {
			rows = append(rows, check{
				name:   "version",
				level:  cliui.LevelWarn,
				detail: notice,
				hint:   "restart the daemon to pick up the new binary",
			})
		} else {
			rows = append(rows, check{
				name:   "version",
				level:  cliui.LevelSuccess,
				detail: fmt.Sprintf("client %s · daemon %s", buildinfo.Version, di.Version),
			})
		}
	}

	// --- Check 4: remote SSH server -----------------------------------------
	// Governing: ADR-0004 (the optional Wish cockpit) + ADR-0008 (public-key
	// only, refuses an empty allowlist). The row cross-checks config intent
	// against the daemon's live listener, so "enabled but refused to start"
	// (empty allowlist, bind failure) is visible without reading daemon logs.
	if cfg != nil {
		rows = append(rows, sshCheck(cfg.Server, &di))
	}

	// --- Check 5: harnesses in healthy state -------------------------------
	// Governing: SPEC-0003 (the state model and its healthy/degraded/failed
	// tiers drive the per-row level here).
	//
	// Issue #99: the active profile may be unresolved (persisted in state.json
	// but no longer in config). This is a warning, not a hard failure — the
	// daemon falls back to autostart — but it must be surfaced because the
	// daemon started a different set than the operator might expect.
	if di.ActiveProfile != "" && di.ProfileResolved != nil && !*di.ProfileResolved {
		rows = append(rows, check{
			name:   "profile",
			level:  cliui.LevelWarn,
			detail: fmt.Sprintf("active profile %q not in config (fell back to autostart)", di.ActiveProfile),
			hint:   "run `harness use-profile <name>` to select a valid profile",
		})
	}

	// An autostart profile member that state.json restored disabled is never
	// started, and every other signal reads healthy: `harness list` shows it
	// stopped like any deliberate stop, the harnesses row below counts it
	// healthy, and the config still says `autostart = true`. Warn, don't fail —
	// persisted intent winning is correct, it just has to be visible.
	if len(di.DormantAutostart) > 0 {
		rows = append(rows, check{
			name:   "autostart",
			level:  cliui.LevelWarn,
			detail: fmt.Sprintf("%d autostart member(s) left down by persisted intent: %s", len(di.DormantAutostart), strings.Join(di.DormantAutostart, ", ")),
			hint:   "run `harness start <name>` to re-enable (persists across restarts)",
		})
	}

	hs, err := c.List()
	switch {
	case err != nil:
		rows = append(rows, check{
			name:   "harnesses",
			level:  cliui.LevelWarn,
			detail: fmt.Sprintf("couldn't list: %v", err),
		})
	case len(hs) == 0:
		rows = append(rows, check{
			name:   "harnesses",
			level:  cliui.LevelWarn,
			detail: "none configured",
			hint:   "add a [harness.*] table to your config",
		})
	default:
		var failedStates, degraded []string
		for _, h := range hs {
			switch core.State(h.State) {
			case core.StateFailed:
				failedStates = append(failedStates, h.Name)
			case core.StateDegraded:
				degraded = append(degraded, h.Name)
			}
		}
		switch {
		case len(failedStates) > 0:
			rows = append(rows, check{
				name:   "harnesses",
				level:  cliui.LevelError,
				detail: fmt.Sprintf("%d/%d failed: %s", len(failedStates), len(hs), strings.Join(failedStates, ", ")),
				hint:   "restart with: harness restart <name>",
			})
		case len(degraded) > 0:
			rows = append(rows, check{
				name:   "harnesses",
				level:  cliui.LevelWarn,
				detail: fmt.Sprintf("%d/%d degraded: %s", len(degraded), len(hs), strings.Join(degraded, ", ")),
				hint:   "check `harness logs <name>` for the failure",
			})
		default:
			rows = append(rows, check{
				name:   "harnesses",
				level:  cliui.LevelSuccess,
				detail: fmt.Sprintf("all %d healthy", len(hs)),
			})
		}
	}

	emitDoctor(os.Stdout, os.Stderr, rows)

	// Exit non-zero if any row failed.
	for _, r := range rows {
		if r.level == cliui.LevelError {
			return 1
		}
	}
	return 0
}

// sshCheck builds the remote-SSH row by cross-checking the config's [server]
// intent against what the daemon reports as actually listening. The daemon
// only sets SshAddr when the Wish server truly started, so the four outcomes
// are: disabled-and-off (pass), forced-on via --ssh despite config (pass,
// annotated), enabled-but-not-listening (fail — the daemon refused to start
// it, most often an empty allowlist per ADR-0008), and listening (pass, with
// the allowlist size).
func sshCheck(sc core.ServerConfig, di *protocol.DaemonInfo) check {
	listening := ""
	keys := 0
	if di != nil {
		listening = di.SshAddr
		keys = di.SshKeys
	}
	switch {
	case !sc.Enabled && listening == "":
		return check{name: "ssh", level: cliui.LevelSuccess, detail: "off (not enabled in config)"}
	case !sc.Enabled && listening != "":
		// Only reachable when the daemon was started with --ssh, which
		// overrides config; annotate so the row does not look contradictory.
		return check{
			name:   "ssh",
			level:  cliui.LevelSuccess,
			detail: fmt.Sprintf("listening on %s · %d key(s) (forced on by --ssh flag)", listening, keys),
		}
	case sc.Enabled && listening == "":
		return check{
			name:   "ssh",
			level:  cliui.LevelError,
			detail: "enabled in config but not listening",
			hint:   "check the daemon log — an empty authorized-keys allowlist refuses to start (ADR-0008)",
		}
	default:
		return check{
			name:   "ssh",
			level:  cliui.LevelSuccess,
			detail: fmt.Sprintf("listening on %s · %d key(s)", listening, keys),
		}
	}
}

// emitDoctor renders the rows either as JSON on stdout (when --json) or as
// a human tabular report on stderr. Split out so it can be unit-tested with
// an injected writer.
func emitDoctor(stdout, stderr io.Writer, rows []check) {
	if cliui.JSON() {
		emitDoctorJSON(stdout, rows)
		return
	}
	printDoctorTable(stderr, rows)
}

// emitDoctorJSON serializes the check rows as a doctorResult object.
func emitDoctorJSON(w io.Writer, rows []check) {
	var (
		pass, warn, fail int
		res              doctorResult
	)
	for _, r := range rows {
		cr := checkResult{
			Status: r.level.String(),
			Name:   r.name,
			Detail: r.detail,
			Hint:   r.hint,
		}
		switch r.level {
		case cliui.LevelSuccess:
			pass++
		case cliui.LevelWarn:
			warn++
		case cliui.LevelError:
			fail++
		}
		switch r.name {
		case "config":
			res.Config = cr
		case "daemon":
			c := cr
			res.Daemon = &c
		case "version":
			c := cr
			res.Version = &c
		case "harnesses":
			c := cr
			res.Harness = &c
		case "profile":
			c := cr
			res.Profile = &c
		case "autostart":
			c := cr
			res.Autostart = &c
		case "ssh":
			c := cr
			res.Ssh = &c
		}
	}
	res.Summary = summaryResult{Passed: pass, Warned: warn, Failed: fail}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(res)
}

// printDoctorTable writes the rows plus a summary tally as a single table
// using the shared Table renderer. In a TTY the status column is colored
// (paired with the glyph so a mono terminal still reads it); when not a TTY
// or --json is set, it degrades to plain "ok/warn/error" tokens so the table
// stays script-parseable.
func printDoctorTable(w io.Writer, rows []check) {
	var pass, warn, fail int
	for _, r := range rows {
		switch r.level {
		case cliui.LevelSuccess:
			pass++
		case cliui.LevelWarn:
			warn++
		case cliui.LevelError:
			fail++
		}
	}

	t := NewTable(w, "CHECK", "STATUS", "DETAIL")
	for _, r := range rows {
		status := r.level.String()
		if t.colored {
			status = lipgloss.NewStyle().
				Foreground(r.level.Color(t.pal)).
				Bold(true).
				Render(fmt.Sprintf("%s %s", r.level.Glyph(), status))
		}
		t.Row(r.name, status, r.detail)
		if r.hint != "" {
			// Hint is a continuation of the DETAIL column: leave CHECK and
			// STATUS empty so the arrow lands exactly under DETAIL (and
			// wraps under DETAIL if it overflows), not at column 0.
			t.Row("", "", t.dimItalic("→ "+r.hint))
		}
	}

	// Separator above the summary row, then the colored tally.
	t.Separator()
	summaryLevel := cliui.LevelSuccess
	switch {
	case fail > 0:
		summaryLevel = cliui.LevelError
	case warn > 0:
		summaryLevel = cliui.LevelWarn
	}
	tally := fmt.Sprintf("%d passed · %d warning(s) · %d failed", pass, warn, fail)
	if t.colored {
		tally = lipgloss.NewStyle().
			Foreground(summaryLevel.Color(t.pal)).
			Bold(true).
			Render(tally)
	}
	t.RowFull("summary", tally)
	_ = t.Flush()
}
