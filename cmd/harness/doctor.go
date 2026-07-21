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
	"text/tabwriter"

	"github.com/charmbracelet/lipgloss"

	"gitea.stump.rocks/stump.wtf/harness/internal/buildinfo"
	"gitea.stump.rocks/stump.wtf/harness/internal/client"
	"gitea.stump.rocks/stump.wtf/harness/internal/cliui"
	"gitea.stump.rocks/stump.wtf/harness/internal/config"
	"gitea.stump.rocks/stump.wtf/harness/internal/core"
	"gitea.stump.rocks/stump.wtf/harness/internal/tui/theme"
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
	Config   checkResult   `json:"config"`
	Daemon   *checkResult  `json:"daemon,omitempty"`
	Version  *checkResult  `json:"version,omitempty"`
	Harness  *checkResult  `json:"harness,omitempty"`
	Summary  summaryResult `json:"summary"`
	ExitCode int           `json:"-"`
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
		rows = append(rows, check{
			name:   "daemon",
			level:  cliui.LevelError,
			detail: fmt.Sprintf("unreachable at %s", o.socket),
			hint:   "start it with: harness daemon",
		})
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
	// match; build version is informational but worth surfacing on skew).
	di, err := c.DaemonInfo()
	switch {
	case err != nil:
		rows = append(rows, check{
			name:   "version",
			level:  cliui.LevelWarn,
			detail: fmt.Sprintf("couldn't fetch daemon info: %v", err),
		})
	case di.Version != buildinfo.Version:
		rows = append(rows, check{
			name:   "version",
			level:  cliui.LevelWarn,
			detail: fmt.Sprintf("client %s vs daemon %s", buildinfo.Version, di.Version),
			hint:   "restart the daemon to pick up the new binary",
		})
	default:
		rows = append(rows, check{
			name:   "version",
			level:  cliui.LevelSuccess,
			detail: fmt.Sprintf("client and daemon both %s", buildinfo.Version),
		})
	}

	// --- Check 4: harnesses in healthy state -------------------------------
	// Governing: SPEC-0003 (the state model and its healthy/degraded/failed
	// tiers drive the per-row level here).
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
		}
	}
	res.Summary = summaryResult{Passed: pass, Warned: warn, Failed: fail}
	res.ExitCode = 0
	if fail > 0 {
		res.ExitCode = 1
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(res)
}

// printDoctorTable writes the rows plus a summary tally as a single table.
// In a TTY the status column is colored (paired with the glyph so a mono
// terminal still reads it); when not a TTY or --json is set, it degrades to
// plain "PASS/WARN/FAIL" tokens so the table stays script-parseable.
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

	useColor := !cliui.JSON() && cliui.IsTTY(os.Stderr)
	th := theme.Default()
	pal := th.Palette

	// Bold header + separator line under it. The separator width matches the
	// tabwriter's natural table width so it reads as a single horizontal rule.
	separator := strings.Repeat("─", maxBlockWidth())
	header := fmt.Sprintf("  %s\t%s\t%s", bold(useColor, pal, "CHECK"), bold(useColor, pal, "STATUS"), bold(useColor, pal, "DETAIL"))

	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, header)
	fmt.Fprintln(tw, separator)

	for _, r := range rows {
		status := r.level.String()
		if useColor {
			status = lipgloss.NewStyle().
				Foreground(r.level.Color(pal)).
				Bold(true).
				Render(fmt.Sprintf("%s %s", r.level.Glyph(), status))
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", r.name, status, r.detail)
		if r.hint != "" {
			hint := "→ " + r.hint
			if useColor {
				hint = lipgloss.NewStyle().
					Foreground(pal.Dim).
					Italic(true).
					Render(hint)
			}
			fmt.Fprintf(tw, "  \t\t%s\n", hint)
		}
	}

	// Separator above the summary row, then the colored tally.
	fmt.Fprintln(tw, separator)
	summaryLevel := cliui.LevelSuccess
	switch {
	case fail > 0:
		summaryLevel = cliui.LevelError
	case warn > 0:
		summaryLevel = cliui.LevelWarn
	}
	tally := fmt.Sprintf("%d passed · %d warning(s) · %d failed", pass, warn, fail)
	if useColor {
		tally = lipgloss.NewStyle().
			Foreground(summaryLevel.Color(pal)).
			Bold(true).
			Render(tally)
	}
	fmt.Fprintf(tw, "  %s\t\t%s\n", "summary", tally)
	_ = tw.Flush()
}

// bold returns s rendered bold when colorize is set, else plain s.
func bold(colorize bool, p theme.Palette, s string) string {
	if !colorize {
		return s
	}
	return lipgloss.NewStyle().Foreground(p.Fg).Bold(true).Render(s)
}

// maxBlockWidth mirrors cliui.maxBlockWidth (unexported) so the separator
// rule matches the styled-box width budget. Kept here because doctor owns
// the table layout, not cliui.
func maxBlockWidth() int { return 80 }
