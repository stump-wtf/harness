package tui

// Governing: SPEC-0001 REQ "Harness Form" — n/e open a Huh form over the harness
// schema (harness/prompt/model/auto_accept/max_turns/quiet/schedule/args/workdir/
// env_file/restart_delay/restart/backend/tmux_socket/description/enabled/
// harvest_trajectory/mcp_allow/profile membership) that writes back to
// harness.toml (ADR-0006: file is truth); e
// pre-fills from the existing harness; then the daemon reloads and the harness
// appears on the dashboard. This file owns the schema<->TOML serialization; the
// Huh widget wiring lives in overlays.go.
//
// Also governing: SPEC-0001 REQ "Lossless Edit Round-Trip" — the e save path
// rewrites the whole [harness.<name>] table, so the form must carry EVERY
// core.Harness config key or the omitted ones are deleted from harness.toml on
// the next unrelated edit (issue #161). form_test.go's
// TestHarnessFormCoversEveryHarnessField pins the census.

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/BurntSushi/toml"
	"github.com/anmitsu/go-shlex"
	"github.com/robfig/cron/v3"

	"github.com/stump-wtf/harness/internal/config"
	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/hours"
	"github.com/stump-wtf/harness/internal/protocol"
)

// HarnessForm is the editable harness schema behind the n/e Huh form. It is the
// TUI-facing projection of a core.Harness table; RestartDelay is seconds to
// match the TOML unit (config.rawHarness.RestartDelay).
type HarnessForm struct {
	Name string
	// Harness is the harness-kind enum (crush/claude-code/codex/generic/
	// command). It selects the adapter, which supplies the executable for a
	// long-running harness and the argv synthesis for a prompt one-shot
	// (ADR-0011); a command harness's Argv is its whole process. It is
	// REQUIRED — there is no default, so Validate rejects a blank one rather
	// than picking an agent on the user's behalf.
	Harness string
	// Prompt is the agent one-shot instruction; Args belong to a long-running
	// harness only (Validate enforces the split).
	Prompt string
	// Model is the agent model selection (issue #57): requires Prompt — a cmd
	// harness passes --model through Args itself — and is a single token
	// (Validate mirrors the parser on both).
	Model string
	// AutoAccept is the agent unattended/yolo mode (issue #58): requires
	// Prompt — a cmd harness passes its tool's flag through Args itself
	// (Validate mirrors the parser).
	AutoAccept bool
	// MaxTurns is the agent turn budget for a prompt harness (issue #59):
	// requires Prompt — a cmd harness passes --max-turns through Args itself
	// (Validate mirrors the parser). 0 means unset/unlimited.
	MaxTurns int
	// Quiet is the headless single-flag for a prompt harness (issue #60): a
	// one-shot runs quietly by default, and setting this false opts back into
	// streaming output to an attach. False without Prompt is rejected
	// (Validate mirrors the parser).
	Quiet bool
	// PromptFile is the path to a file holding the instruction — the
	// alternative to an inline Prompt (ADR-0018), and mutually exclusive with
	// it. A round-trip field like Schedule and TmuxSocket below: the save path
	// rewrites the whole table, so a form that dropped it would delete a
	// scheduled harness's prompt source on the next unrelated edit. The form
	// carries the PATH only — the file's contents are read at spawn and must
	// never be written back into harness.toml.
	PromptFile string
	// Schedule is the daemon-owned cron expression for a scheduled one-shot
	// (issue #66): requires Prompt, and is mutually exclusive with Enabled and
	// with a respawning restart policy (Validate mirrors the parser on all
	// three). Carried through the form so an edit round-trips it — the save
	// path rewrites the whole table, so a field the form drops is a schedule
	// silently deleted from harness.toml.
	Schedule string
	// CatchUp is the schedule's missed-window policy (issue #117): requires
	// Schedule (Validate mirrors the parser). Round-trip field for the same
	// reason as Schedule.
	CatchUp bool
	// Timeout, OnOverlap and KeepRuns shape a schedule's runs (issue #119).
	// Each holds only a value that differs from its parser default — "" / 0
	// means "the default" — so an untouched edit round-trips without growing
	// keys. Validate mirrors the parser: a non-default value requires Schedule.
	Timeout   string
	OnOverlap string
	KeepRuns  int
	Args      []string
	// Argv is a command harness's whole process (SPEC-0017 REQ-2), argv[0]
	// first. It replaces Args on that kind and is refused on every other
	// (Validate mirrors the parser). A round-trip field like Schedule: the
	// save path rewrites the whole table, so a form that dropped `argv`
	// would leave a command harness that no longer parses after an edit to
	// its description. Written verbatim, element for element, never joined
	// into a command string (SPEC-0017 REQ-15).
	Argv []string
	// PromptDelivery is a command harness's prompt_delivery (SPEC-0017
	// REQ-12): how its prompt reaches the program. A round-trip field like
	// Argv, and validated by the same matrix the parser uses
	// (core.CheckCommandPrompt).
	PromptDelivery string
	// argvErr is why the argv input did not parse (toForm), reported by
	// Validate. Unlike args, a malformed argv is not silently dropped: saving
	// would then write a command harness with no argv at all.
	argvErr      error
	Workdir      string
	EnvFile      string
	RestartDelay int    // seconds
	Restart      string // core.RestartPolicy; empty = the parse default
	Backend      string
	// TmuxSocket names the tmux server socket; inert unless Backend == tmux
	// (ADR-0006 keeps it for backward compatibility). Carried through the form
	// for the same reason as Schedule: the save path rewrites the whole table,
	// so a field the form drops detaches the harness from the operator's tmux
	// server on the next unrelated edit (issue #161).
	TmuxSocket  string
	Description string
	Enabled     bool
	// HarvestTrajectory opts the harness into read-only trajectory exposure
	// through the MCP facade (ADR-0008: opt-in, a trajectory may contain
	// secrets). Round-trip field (issue #161).
	HarvestTrajectory bool
	// MCPAllow is the per-harness MCP capability scope (SPEC-0005), defaulting
	// to ["read"] in the parser. Round-trip field (issue #161): dropping it
	// silently revokes a harness's write authority — or, worse on the way back,
	// would re-grant it.
	MCPAllow []string
	// OperatingHours gates a resident harness to weekly windows (ADR-0019):
	// requires the harness NOT be scheduled (Validate mirrors the parser's
	// mutual exclusion with Schedule). Carried through the form for the same
	// round-trip reason as Schedule.
	OperatingHours string
	// HoursShutdown/HoursShutdownTimeout are operating hours' close-mode keys
	// (SPEC-0012 REQ "Shutdown Mode"), same "blank means the parser default"
	// convention as Timeout/OnOverlap above: each holds only a value that
	// differs from its default ("graceful" / "15m"). Both require
	// OperatingHours (Validate mirrors the parser).
	HoursShutdown        string
	HoursShutdownTimeout string
	// ExportTelemetry is the tri-state telemetry opt-in (SPEC-0015 REQ-1):
	// "" leaves the key out (follow [telemetry] export_all), "true"/"false"
	// write it. A string rather than a bool because "unset" and "false" mean
	// different things once export_all is on. Round-trip field (issue #161):
	// dropping an explicit false would start publishing that harness.
	ExportTelemetry string
	// Triggers binds the harness to [channel.*]/[webhook.*] sources
	// (SPEC-0014). A round-trip field like Schedule, and a strict one: the
	// save path rewrites the whole table, so a form that dropped `triggers`
	// would silently unbind a harness from its webhook the next time someone
	// edited its description — and the harness would keep looking healthy
	// while never firing again.
	//
	// The form does NOT carry the source tables themselves. They are separate
	// top-level tables, and SPEC-0014 REQ "Triggers Round-Trip Through Config
	// Writers" requires a harness rewrite to leave them byte-identical, so
	// the only safe thing to do with them here is nothing.
	Triggers []string
}

// NewHarnessForm is a blank form for `n` with sane defaults (native backend).
func NewHarnessForm() HarnessForm {
	// A prompt one-shot is headless by default (issue #60); the form lets the
	// user opt back into output by clearing Quiet before saving.
	return HarnessForm{Backend: string(core.BackendNative), Quiet: true}
}

// Validate checks the minimum the daemon config parser requires (a name and
// a known harness kind, a known backend, non-negative delay) so the form
// catches errors before writing TOML the daemon would reject on reload.
func (f HarnessForm) Validate() error {
	if strings.TrimSpace(f.Name) == "" {
		return fmt.Errorf("name is required")
	}
	switch strings.TrimSpace(f.ExportTelemetry) {
	case "", "true", "false":
	default:
		return fmt.Errorf("export_telemetry must be blank, true or false")
	}
	// Either prompt source makes this an agent one-shot; the parser's rules
	// below all key off that, not off an inline prompt alone.
	promptSet := strings.TrimSpace(f.Prompt) != "" || strings.TrimSpace(f.PromptFile) != ""
	if strings.TrimSpace(f.Prompt) != "" && strings.TrimSpace(f.PromptFile) != "" {
		return fmt.Errorf("prompt and prompt_file are mutually exclusive")
	}
	switch f.Harness {
	case "crush", "claude-code", "codex", "generic", core.AdapterCommand:
	case "":
		return fmt.Errorf("harness is required (one of: crush, claude-code, codex, generic, command)")
	default:
		return fmt.Errorf("harness must be one of: crush, claude-code, codex, generic, command")
	}
	// Mirror the parser's refusal: saving this would leave harness.toml
	// unparseable. Governing: SPEC-0017 REQ "Generic Kind Rejects Prompts".
	if f.Harness == "generic" && promptSet {
		return fmt.Errorf("generic runs sh and has no prompt synthesis; use harness crush, claude-code or codex for a prompt one-shot, or command with argv to run another program without a shell")
	}
	if err := f.validateCommand(promptSet); err != nil {
		return err
	}
	if promptSet && len(f.Args) > 0 {
		return fmt.Errorf("prompt and args are mutually exclusive")
	}
	isCommand := f.Harness == core.AdapterCommand
	if model := strings.TrimSpace(f.Model); model != "" {
		// A command harness passes model through {{model}} in its argv;
		// validateCommand has already refused one that does not.
		if !promptSet && !isCommand {
			return fmt.Errorf("model requires prompt (for a long-running harness, pass --model in args)")
		}
		if strings.ContainsFunc(model, unicode.IsSpace) {
			return fmt.Errorf("model must be a single token (no whitespace)")
		}
	}
	if f.AutoAccept && !promptSet {
		return fmt.Errorf("auto_accept requires prompt (for a long-running harness, pass the tool's flag in args)")
	}
	if f.MaxTurns < 0 {
		return fmt.Errorf("max_turns must not be negative")
	}
	if f.MaxTurns > 0 && !promptSet {
		return fmt.Errorf("max_turns requires prompt (for a long-running harness, pass --max-turns in args)")
	}
	if f.Backend != "" && !core.Backend(f.Backend).Valid() {
		return fmt.Errorf("backend must be native or tmux")
	}
	if f.RestartDelay < 0 {
		return fmt.Errorf("restart_delay must not be negative")
	}
	if !core.RestartPolicy(f.Restart).Valid() {
		return fmt.Errorf("restart must be no, always, unless-stopped, or on-failure")
	}
	// Mirror the parser's `schedule` rules (issue #66, config.registerHarness).
	// The save path writes harness.toml before the daemon ever sees it, so a
	// combination the parser rejects would leave the file unparseable on disk —
	// every later reload fails until it is hand-edited.
	if schedule := strings.TrimSpace(f.Schedule); schedule != "" {
		if !promptSet && !isCommand {
			return fmt.Errorf("schedule requires prompt, or harness command (a scheduled harness is a one-shot run)")
		}
		if f.Enabled {
			return fmt.Errorf("schedule and enabled are mutually exclusive")
		}
		if r := core.RestartPolicy(f.Restart); r == core.RestartAlways || r == core.RestartUnlessStopped {
			return fmt.Errorf("schedule requires restart no or on-failure (%s respawns the one-shot after a clean exit)", r)
		}
		if _, err := cron.ParseStandard(schedule); err != nil {
			return fmt.Errorf("invalid schedule %q: %v", schedule, err)
		}
	}
	// Mirror the parser's `triggers` rules (SPEC-0014 REQ "Triggers Key",
	// REQ "Triggered Harness Exclusions"), for the same reason as schedule
	// above. Only the SHAPE of each reference is checkable here: whether the
	// named source exists depends on the rest of the file plus every
	// harness_d drop-in, which the form does not read.
	triggers := normalizeTriggers(f.Triggers)
	if len(triggers) > 0 {
		if !promptSet && !isCommand {
			return fmt.Errorf("triggers requires prompt, or harness command (a triggered harness is a one-shot run)")
		}
		if f.Enabled {
			return fmt.Errorf("triggers and enabled are mutually exclusive")
		}
		if r := core.RestartPolicy(f.Restart); r == core.RestartAlways || r == core.RestartUnlessStopped {
			return fmt.Errorf("triggers requires restart no or on-failure (%s respawns the one-shot with no event)", r)
		}
		seen := map[string]bool{}
		for _, t := range triggers {
			ref, err := core.ParseTriggerRef(t)
			if err != nil {
				return fmt.Errorf("invalid triggers entry %q: %v", t, err)
			}
			if seen[ref.String()] {
				return fmt.Errorf("triggers lists %q twice", ref.String())
			}
			seen[ref.String()] = true
		}
	}
	hasChannelTrigger := false
	for _, t := range triggers {
		if strings.HasPrefix(t, core.SourceKindChannel+".") {
			hasChannelTrigger = true
			break
		}
	}
	// operating_hours counts only alongside triggers, as in the parser: a
	// resident hours-gated harness has no firings to miss. It is also what
	// keeps TOML() honest, since that writes catch_up only for a harness
	// with a schedule or triggers.
	hoursGatedFirings := strings.TrimSpace(f.OperatingHours) != "" && len(triggers) > 0
	if f.CatchUp && strings.TrimSpace(f.Schedule) == "" && !hasChannelTrigger && !hoursGatedFirings {
		return fmt.Errorf("catch_up requires schedule, a channel trigger, or operating_hours with triggers")
	}
	// Mirror the parser's run keys (issue #119), widened by SPEC-0014 from
	// "scheduled" to "triggered": an event source produces runs the same way
	// a clock does.
	//
	// This predicate is deliberately NOT reused by the operating_hours check
	// below. `operating_hours` excludes a *schedule* (a cron one-shot is
	// already time-gated by its own expression) but is explicitly ALLOWED on
	// a harness whose only firing source is an event — SPEC-0014 REQ
	// "Operating Hours On Triggered Harnesses" exists precisely to gate those
	// firings. Folding triggers into the exclusion would forbid the
	// combination the spec is about.
	triggered := strings.TrimSpace(f.Schedule) != "" || len(triggers) > 0
	if t := strings.TrimSpace(f.Timeout); t != "" {
		if !triggered {
			return fmt.Errorf("timeout requires schedule or triggers")
		}
		if d, err := time.ParseDuration(t); err != nil || d < 0 {
			return fmt.Errorf("invalid timeout %q (want a duration such as 45m or 2h, or 0 for no limit)", t)
		}
	}
	if o := strings.TrimSpace(f.OnOverlap); o != "" && o != string(core.OverlapSkip) {
		if !triggered {
			return fmt.Errorf("on_overlap requires schedule or triggers")
		}
		if !core.OverlapPolicy(o).Valid() {
			return fmt.Errorf("on_overlap must be skip, queue, or replace")
		}
	}
	if f.KeepRuns < 0 {
		return fmt.Errorf("keep_runs must be at least 1")
	}
	if f.KeepRuns > 0 && f.KeepRuns != core.DefaultKeepRuns && !triggered {
		return fmt.Errorf("keep_runs requires schedule or triggers")
	}
	// Mirror the parser's operating_hours rules (ADR-0019, config.registerHarness):
	// same reasoning as schedule above — a combination the parser rejects would
	// leave the file unparseable on disk until hand-edited.
	operatingHours := strings.TrimSpace(f.OperatingHours)
	if operatingHours != "" {
		if strings.TrimSpace(f.Schedule) != "" {
			return fmt.Errorf("operating_hours and schedule are mutually exclusive")
		}
		if _, err := hours.Parse(operatingHours); err != nil {
			return fmt.Errorf("invalid operating_hours %q: %v", operatingHours, err)
		}
	}
	if hs := strings.TrimSpace(f.HoursShutdown); hs != "" {
		if operatingHours == "" {
			return fmt.Errorf("hours_shutdown requires operating_hours")
		}
		if len(triggers) > 0 {
			return fmt.Errorf("hours_shutdown is not accepted on a triggered harness")
		}
		if !core.HoursShutdownMode(hs).Valid() {
			return fmt.Errorf("hours_shutdown must be graceful or immediate")
		}
	}
	if hst := strings.TrimSpace(f.HoursShutdownTimeout); hst != "" {
		if operatingHours == "" {
			return fmt.Errorf("hours_shutdown_timeout requires operating_hours")
		}
		if len(triggers) > 0 {
			return fmt.Errorf("hours_shutdown_timeout is not accepted on a triggered harness")
		}
		if d, err := time.ParseDuration(hst); err != nil || d <= 0 {
			return fmt.Errorf("invalid hours_shutdown_timeout %q (want a positive duration such as 15m)", hst)
		}
	}
	return nil
}

// validateCommand mirrors the parser's `command` key rules
// (config.checkCommandKeys), for the reason every rule in Validate exists: a
// combination the parser rejects would leave harness.toml unparseable on
// disk. The argv shape itself is core.CheckCommandArgv, the same function the
// parser and the wire call, so the three cannot disagree on it.
// Governing: ADR-0023, SPEC-0017 REQ-2 "Command Harness Kind", REQ-3.
func (f HarnessForm) validateCommand(promptSet bool) error {
	if f.argvErr != nil {
		return f.argvErr
	}
	delivery := strings.TrimSpace(f.PromptDelivery)
	if f.Harness != core.AdapterCommand {
		if len(f.Argv) > 0 {
			return fmt.Errorf("argv is only accepted on harness command (use args for %s)", f.Harness)
		}
		return core.CheckCommandPrompt(f.Harness, nil, delivery, promptSet)
	}
	switch {
	case len(f.Args) > 0:
		return fmt.Errorf("args is not accepted on a command harness: put the whole command line in argv")
	case f.AutoAccept || f.MaxTurns != 0:
		return fmt.Errorf("auto_accept and max_turns are not accepted on a command harness: it owns its argv")
	}
	if err := core.CheckCommandArgv(f.Argv); err != nil {
		return err
	}
	// The parser's prompt delivery matrix (SPEC-0017 REQ-12): a prompt the
	// argv never passes on, with no stdin or file delivery, would be dropped.
	if err := core.CheckCommandPrompt(f.Harness, f.Argv, delivery, promptSet); err != nil {
		return err
	}
	scheduled := strings.TrimSpace(f.Schedule) != ""
	triggered := scheduled || len(normalizeTriggers(f.Triggers)) > 0
	return core.CheckCommandTemplateContext(f.Argv, strings.TrimSpace(f.Model), scheduled, triggered)
}

// formatRunTimeout renders a timeout the way an operator would type it:
// "30m", "1h30m", "0s" — not Duration.String's "30m0s".
func formatRunTimeout(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// TOML renders the form as a `[harness.<name>]` table. Only set fields are
// emitted so the file stays clean. The output re-parses through config.Parse
// into an equivalent harness (round-trip guarantee, tested).
func (f HarnessForm) TOML() string {
	var b strings.Builder
	fmt.Fprintf(&b, "[harness.%s]\n", f.Name)
	// `harness` is required and has no default, so it is always written —
	// unlike every optional field below, which is emitted only when set.
	fmt.Fprintf(&b, "harness = %s\n", strconv.Quote(f.Harness))
	prompt := strings.TrimSpace(f.Prompt)
	promptFile := strings.TrimSpace(f.PromptFile)
	isCommand := f.Harness == core.AdapterCommand
	oneShotKind := prompt != "" || promptFile != "" || isCommand
	if isCommand {
		// A command harness owns its argv; a prompt rides beside it and
		// reaches the program by prompt_delivery (SPEC-0017 REQ-12), never
		// through synthesized flags, so none of the agent keys are written.
		// Placeholders are written as the operator typed them (REQ-15).
		if prompt != "" {
			fmt.Fprintf(&b, "prompt = %s\n", strconv.Quote(prompt))
		} else if promptFile != "" {
			fmt.Fprintf(&b, "prompt_file = %s\n", strconv.Quote(promptFile))
		}
		if len(f.Argv) > 0 {
			// One TOML string per element exactly as the operator wrote it:
			// quoting each element keeps "a b" one argument, and a
			// placeholder stays a placeholder, never a rendering.
			parts := make([]string, len(f.Argv))
			for i, a := range f.Argv {
				parts[i] = strconv.Quote(a)
			}
			fmt.Fprintf(&b, "argv = [%s]\n", strings.Join(parts, ", "))
		}
		if d := strings.TrimSpace(f.PromptDelivery); d != "" {
			fmt.Fprintf(&b, "prompt_delivery = %s\n", strconv.Quote(d))
		}
		if model := strings.TrimSpace(f.Model); model != "" {
			// A command harness's model feeds {{model}} in its argv
			// (SPEC-0017 REQ-3); Validate refuses it without one.
			fmt.Fprintf(&b, "model = %s\n", strconv.Quote(model))
		}
	} else if prompt != "" || promptFile != "" {
		// Prompt harness: `prompt` replaces args entirely (Validate
		// enforces the exclusivity; the daemon synthesizes the argv at spawn,
		// ADR-0011). `model`, `auto_accept`, and `max_turns` ride beside it as
		// config truth — never as synthesized args (issues #57, #58, #59).
		if prompt != "" {
			fmt.Fprintf(&b, "prompt = %s\n", strconv.Quote(prompt))
		} else {
			// The PATH, never the file's contents — inlining the document
			// here is the round-trip corruption ADR-0018 exists to avoid.
			fmt.Fprintf(&b, "prompt_file = %s\n", strconv.Quote(promptFile))
		}
		if model := strings.TrimSpace(f.Model); model != "" {
			fmt.Fprintf(&b, "model = %s\n", strconv.Quote(model))
		}
		if f.AutoAccept {
			b.WriteString("auto_accept = true\n")
		}
		if f.MaxTurns > 0 {
			fmt.Fprintf(&b, "max_turns = %d\n", f.MaxTurns)
		}
		if !f.Quiet {
			// Opt out of the headless one-shot default so the agent streams
			// output to whoever attaches (issue #60).
			b.WriteString("quiet = false\n")
		}
	} else {
		if len(f.Args) > 0 {
			parts := make([]string, len(f.Args))
			for i, a := range f.Args {
				parts[i] = strconv.Quote(a)
			}
			fmt.Fprintf(&b, "args = [%s]\n", strings.Join(parts, ", "))
		}
	}
	// A prompt harness or a command harness may be a one-shot (SPEC-0017
	// REQ-3: a command harness needs no prompt for schedule or triggers).
	// Validate rejects a schedule or triggers on anything else, so this
	// block is the only place they can appear.
	if oneShotKind {
		schedule := strings.TrimSpace(f.Schedule)
		triggers := normalizeTriggers(f.Triggers)
		if schedule != "" {
			// The daemon fires this one-shot on a cron cadence (issue #66).
			fmt.Fprintf(&b, "schedule = %s\n", strconv.Quote(schedule))
		}
		if len(triggers) > 0 {
			// Event sources (SPEC-0014). Written beside `schedule` rather
			// than instead of it: the two combine, and a harness may fire on
			// both a clock and a doorbell.
			parts := make([]string, len(triggers))
			for i, t := range triggers {
				parts[i] = strconv.Quote(t)
			}
			fmt.Fprintf(&b, "triggers = [%s]\n", strings.Join(parts, ", "))
		}
		if schedule != "" || len(triggers) > 0 {
			if f.CatchUp {
				b.WriteString("catch_up = true\n")
			}
			// Run keys (issue #119), emitted only when they differ from the
			// parser default. The on_overlap default is not a constant: it
			// is `queue` for a harness with triggers and `skip` otherwise
			// (SPEC-0014 REQ "Overlap Default For Triggered Harnesses"), so
			// the value that can be omitted differs with the firing source.
			// Comparing against the wrong one would either drop an explicit
			// policy or grow a key nobody wrote.
			if t := strings.TrimSpace(f.Timeout); t != "" {
				fmt.Fprintf(&b, "timeout = %s\n", strconv.Quote(t))
			}
			defaultOverlap := string(core.OverlapSkip)
			if len(triggers) > 0 {
				defaultOverlap = string(core.OverlapQueue)
			}
			if o := strings.TrimSpace(f.OnOverlap); o != "" && o != defaultOverlap {
				fmt.Fprintf(&b, "on_overlap = %s\n", strconv.Quote(o))
			}
			if f.KeepRuns > 0 && f.KeepRuns != core.DefaultKeepRuns {
				fmt.Fprintf(&b, "keep_runs = %d\n", f.KeepRuns)
			}
		}
	}
	if f.Workdir != "" {
		fmt.Fprintf(&b, "workdir = %s\n", strconv.Quote(f.Workdir))
	}
	if f.EnvFile != "" {
		fmt.Fprintf(&b, "env_file = %s\n", strconv.Quote(f.EnvFile))
	}
	if f.RestartDelay > 0 {
		fmt.Fprintf(&b, "restart_delay = %d\n", f.RestartDelay)
	}
	// Omit restart when it equals the parse default for this harness kind —
	// "no" for prompt one-shots and scheduled or triggered command harnesses
	// (SPEC-0017 REQ-3), "always" otherwise — so an untouched edit
	// round-trips without growing keys.
	defaultRestart := string(core.RestartAlways)
	commandOneShot := isCommand && (strings.TrimSpace(f.Schedule) != "" || len(normalizeTriggers(f.Triggers)) > 0)
	if prompt != "" || promptFile != "" || commandOneShot {
		defaultRestart = string(core.RestartNo)
	}
	if f.Restart != "" && f.Restart != defaultRestart {
		fmt.Fprintf(&b, "restart = %s\n", strconv.Quote(f.Restart))
	}
	if f.Backend != "" && f.Backend != string(core.BackendNative) {
		fmt.Fprintf(&b, "backend = %s\n", strconv.Quote(f.Backend))
	}
	// Emitted whenever set, not only under backend = "tmux": the key is inert
	// on a native harness (ADR-0006) and the parser accepts it there, so
	// gating the write on the current backend would turn "switch to native,
	// switch back" into the very data loss this round-trip exists to prevent
	// (issue #161).
	if socket := strings.TrimSpace(f.TmuxSocket); socket != "" {
		fmt.Fprintf(&b, "tmux_socket = %s\n", strconv.Quote(socket))
	}
	if f.Description != "" {
		fmt.Fprintf(&b, "description = %s\n", strconv.Quote(f.Description))
	}
	if f.Enabled {
		b.WriteString("enabled = true\n")
	}
	if f.HarvestTrajectory {
		b.WriteString("harvest_trajectory = true\n")
	}
	// Omit mcp_allow when it equals the parser's default scope so an untouched
	// edit round-trips without growing keys (same rule as restart above). An
	// EMPTY-but-non-nil scope is the deny-all a user wrote as `mcp_allow = []`
	// and must be emitted verbatim: omitting it hands the harness back the
	// ["read"] default, which is a silent capability GRANT rather than the
	// silent loss the rest of issue #161 is about. Only a nil scope — no form
	// opinion at all — falls through to the parser default.
	if allow := f.MCPAllow; allow != nil && !isDefaultMCPAllow(allow) {
		parts := make([]string, len(allow))
		for i, a := range allow {
			parts[i] = strconv.Quote(a)
		}
		fmt.Fprintf(&b, "mcp_allow = [%s]\n", strings.Join(parts, ", "))
	}
	if operatingHours := strings.TrimSpace(f.OperatingHours); operatingHours != "" {
		// A resident harness's weekly time gate (ADR-0019). Unlike schedule's
		// run keys, not confined to a prompt/cmd branch — operating_hours
		// applies to either kind of harness.
		fmt.Fprintf(&b, "operating_hours = %s\n", strconv.Quote(operatingHours))
		if hs := strings.TrimSpace(f.HoursShutdown); hs != "" && hs != string(core.HoursShutdownGraceful) {
			fmt.Fprintf(&b, "hours_shutdown = %s\n", strconv.Quote(hs))
		}
		if hst := strings.TrimSpace(f.HoursShutdownTimeout); hst != "" {
			fmt.Fprintf(&b, "hours_shutdown_timeout = %s\n", strconv.Quote(hst))
		}
	}
	if et := strings.TrimSpace(f.ExportTelemetry); et == "true" || et == "false" {
		fmt.Fprintf(&b, "export_telemetry = %s\n", et)
	}
	return b.String()
}

// normalizeTriggers trims and drops blank entries from a triggers list, so
// Validate and TOML agree on what "no triggers" means. A list of nothing but
// whitespace is a cleared field, not a binding to an unnamed source.
func normalizeTriggers(in []string) []string {
	var out []string
	for _, t := range in {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// isDefaultMCPAllow reports whether scope is exactly the parser's default,
// ["read"] (SPEC-0005 REQ "Capability Scoping"). config.Parse materializes that
// default for a table with no mcp_allow key, so the edit pre-fill always sees
// it and re-emitting it verbatim would add a key the user never wrote.
func isDefaultMCPAllow(scope []string) bool {
	return len(scope) == 1 && scope[0] == "read"
}

// defaultMCPAllowInput is the parser's default scope in the form's
// space-separated input encoding. Both pre-fills seed the mcp_allow widget with
// it so the field always shows the effective scope and a blank field can carry
// its own meaning (deny-all).
const defaultMCPAllowInput = "read"

// AppendHarness appends a new harness table to an existing harness.toml body,
// separated by a blank line. The daemon then reloads (ADR-0006). This is the
// write path for the `n` form.
func AppendHarness(existing []byte, f HarnessForm) []byte {
	out := strings.TrimRight(string(existing), "\n")
	if out != "" {
		out += "\n\n"
	}
	out += f.TOML()
	return []byte(out)
}

// editInputsFor builds the `e` (edit) form pre-fill for an existing harness.
//
// The daemon's HarnessInfo projection (protocol) carries only name/cmd/backend/
// description/enabled — it OMITS args/workdir/env_file/restart_delay. Pre-filling
// from HarnessInfo alone and then rewriting the harness's `[harness.<name>]`
// table on save (overlays.go saveHarnessCmd) silently dropped every omitted key,
// wiping config the user never touched. ADR-0006 makes the file the source of
// truth, so we load the harness's full table from the config file and pre-fill
// the whole schema, guaranteeing a lossless edit round-trip. The HarnessInfo
// subset is the fallback when the file can't be read or lacks the table (e.g. a
// harness the daemon knows but that isn't in the file yet).
func editInputsFor(path string, sel protocol.HarnessInfo) formInputs {
	fi := formInputs{
		name:           sel.Name,
		harness:        sel.Adapter,
		prompt:         sel.Prompt,
		promptFile:     sel.PromptFile,
		promptDelivery: sel.PromptDelivery,
		model:          sel.Model,
		autoAccept:     sel.AutoAccept,
		quiet:          sel.Quiet,
		maxTurns:       strconv.Itoa(sel.MaxTurns),
		backend:        orDefault(sel.Backend, string(core.BackendNative)),
		description:    sel.Description,
		enabled:        sel.Enabled,
		// The fallback (file unreadable, or the table isn't there yet) must
		// match the parser's default scope, not blank — blank now means the
		// deny-all `mcp_allow = []`.
		mcpAllow: defaultMCPAllowInput,
	}
	cfg, err := config.Load(path)
	if err != nil {
		return fi
	}
	h, ok := cfg.Harnesses[sel.Name]
	if !ok {
		return fi
	}
	fi.harness = h.Adapter
	fi.prompt = h.Prompt
	fi.promptFile = h.PromptFile
	fi.model = h.Model
	fi.autoAccept = h.AutoAccept
	fi.quiet = h.Quiet
	fi.maxTurns = strconv.Itoa(h.MaxTurns)
	fi.schedule = h.Schedule
	fi.catchUp = h.CatchUp
	fi.triggers = strings.Join(h.Triggers, " ")
	if h.Triggered() {
		// Pre-fill only what differs from the parser's defaults: a blank
		// field means the default, and a harness that never set the key must
		// not grow one on save (issue #119). The on_overlap default follows
		// the firing source (SPEC-0014 REQ "Overlap Default For Triggered
		// Harnesses"), so it is derived rather than a constant.
		if h.Timeout != core.DefaultRunTimeout {
			fi.timeout = formatRunTimeout(h.Timeout)
		}
		defaultOverlap := core.OverlapSkip
		if len(h.Triggers) > 0 {
			defaultOverlap = core.OverlapQueue
		}
		if h.OnOverlap != defaultOverlap {
			fi.onOverlap = string(h.OnOverlap)
		}
		if h.KeepRuns != core.DefaultKeepRuns {
			fi.keepRuns = strconv.Itoa(h.KeepRuns)
		}
	}
	fi.args = shellQuoteJoin(h.Args)
	fi.argv = formatArgvInput(h.Argv)
	fi.promptDelivery = h.PromptDelivery
	fi.workdir = h.Workdir
	fi.envFile = h.EnvFile
	if h.RestartDelay > 0 {
		fi.delay = strconv.Itoa(int(h.RestartDelay / time.Second))
	}
	fi.restart = orDefault(string(h.Restart), string(core.RestartAlways))
	fi.backend = orDefault(string(h.Backend), string(core.BackendNative))
	fi.tmuxSocket = h.TmuxSocket
	fi.description = h.Description
	fi.enabled = h.Enabled
	fi.harvestTrajectory = h.HarvestTrajectory
	if h.ExportTelemetry != nil {
		fi.exportTelemetry = strconv.FormatBool(*h.ExportTelemetry)
	}
	fi.mcpAllow = strings.Join(h.MCPAllow, " ")
	fi.operatingHours = h.OperatingHours
	if h.OperatingHours != "" {
		// Same "blank means the parser default" convention as the schedule
		// run keys above (issue #119).
		if h.HoursShutdown != core.HoursShutdownGraceful {
			fi.hoursShutdown = string(h.HoursShutdown)
		}
		if h.HoursShutdownTimeout != core.DefaultHoursShutdownTimeout {
			fi.hoursShutdownTimeout = formatRunTimeout(h.HoursShutdownTimeout)
		}
	}
	return fi
}

// toForm converts the Huh string-bound inputs into a typed HarnessForm, parsing
// space-separated args and the integer restart_delay.
func (fi formInputs) toForm() HarnessForm {
	f := HarnessForm{
		Name:        strings.TrimSpace(fi.name),
		Harness:     strings.TrimSpace(fi.harness),
		Prompt:      strings.TrimSpace(fi.prompt),
		PromptFile:  strings.TrimSpace(fi.promptFile),
		Model:       strings.TrimSpace(fi.model),
		AutoAccept:  fi.autoAccept,
		Quiet:       fi.quiet,
		Schedule:    strings.TrimSpace(fi.schedule),
		CatchUp:     fi.catchUp,
		Triggers:    strings.Fields(fi.triggers),
		Timeout:     strings.TrimSpace(fi.timeout),
		OnOverlap:   strings.TrimSpace(fi.onOverlap),
		Workdir:     strings.TrimSpace(fi.workdir),
		EnvFile:     strings.TrimSpace(fi.envFile),
		Restart:     strings.TrimSpace(fi.restart),
		Backend:     strings.TrimSpace(fi.backend),
		TmuxSocket:  strings.TrimSpace(fi.tmuxSocket),
		Description: strings.TrimSpace(fi.description),
		Enabled:     fi.enabled,

		HarvestTrajectory:    fi.harvestTrajectory,
		OperatingHours:       strings.TrimSpace(fi.operatingHours),
		HoursShutdown:        strings.TrimSpace(fi.hoursShutdown),
		HoursShutdownTimeout: strings.TrimSpace(fi.hoursShutdownTimeout),
		ExportTelemetry:      strings.TrimSpace(fi.exportTelemetry),
	}
	if args, err := shlex.Split(fi.args, true); err == nil && len(args) > 0 {
		f.Args = args
	}
	f.Argv, f.argvErr = parseArgvInput(fi.argv)
	f.PromptDelivery = strings.TrimSpace(fi.promptDelivery)
	// Unconditional, unlike args above: strings.Fields returns a non-nil empty
	// slice for a cleared input, which is how the form expresses the deny-all
	// `mcp_allow = []` (see TOML). Both the `n` and `e` pre-fills seed this
	// with the parser's ["read"] default, so a blank field is a deliberate
	// clear rather than an unset one.
	f.MCPAllow = strings.Fields(fi.mcpAllow)
	if d, err := strconv.Atoi(strings.TrimSpace(fi.delay)); err == nil {
		f.RestartDelay = d
	}
	if n, err := strconv.Atoi(strings.TrimSpace(fi.maxTurns)); err == nil {
		f.MaxTurns = n
	}
	if n, err := strconv.Atoi(strings.TrimSpace(fi.keepRuns)); err == nil {
		f.KeepRuns = n
	}
	return f
}

// shellQuoteJoin joins args into a single string with shell-style quoting so
// that an argument survives the round-trip through the single-line text input
// and back out through shlex.Split. Args needing no quoting are left bare;
// the rest are wrapped in double quotes with backslashes and embedded double
// quotes escaped.
//
// The backslash is the load-bearing character here and the easy one to miss.
// shlex.Split runs in POSIX mode, where a backslash escapes the character
// after it — so an arg this function leaves bare comes back with its
// backslashes eaten: `\d+` becomes `d+`, `C:\Users\joe` becomes
// `C:Usersjoe`. Whatever we quote, we must escape, and whatever contains a
// backslash must therefore be quoted.
//
// The empty string gets an explicit "" for the same reason: bare, it
// contributes nothing to the joined line and the argument disappears.
//
// @joestump 08/23/2026 - Added backslash and empty-arg handling. The
// whitespace-only rule this replaces round-tripped "print('hello world')"
// correctly but silently corrupted every arg carrying a backslash — regex
// patterns and Windows paths, in practice.
// needsShellQuote reports whether an arg must be quoted to survive
// shlex.Split. The two halves of this encoding have to agree on what counts as
// a separator, so this asks the same question the splitter does rather than
// listing characters by hand: go-shlex's DefaultTokenizer defines whitespace as
// unicode.IsSpace, not as " \t\n".
//
// The hand-written set missed \v and \r, which unicode.IsSpace accepts — an
// arg carrying either was left bare and came back split in two, the same shape
// of bug as the backslash above and found the same way. It also missed every
// non-ASCII space: NBSP, the U+2000 block, and ideographic space U+3000, which
// a value pasted from a document or a CJK keyboard can easily carry.
//
// @joestump-agent 08/23/2026 - Review fix; derive the separator set from the
// splitter instead of restating it.
func needsShellQuote(a string) bool {
	return strings.ContainsAny(a, "\"'\\") ||
		strings.IndexFunc(a, unicode.IsSpace) >= 0
}

func shellQuoteJoin(args []string) string {
	// Escape the backslash first so the escapes we add for quotes are not
	// themselves re-escaped; one Replacer pass does that without re-scanning.
	esc := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	parts := make([]string, len(args))
	for i, a := range args {
		if a == "" {
			parts[i] = `""`
			continue
		}
		if !needsShellQuote(a) {
			parts[i] = a
			continue
		}
		parts[i] = `"` + esc.Replace(a) + `"`
	}
	return strings.Join(parts, " ")
}

// formatArgvInput renders argv for the single-line argv input as the TOML
// array the file holds, one quoted string per element — the same text TOML()
// writes. It is deliberately not args' shell-style encoding: go-shlex drops
// an empty token wherever it appears, so `""` does not survive a round trip,
// and a command harness's argv must come back element for element, empty
// arguments included (SPEC-0017 REQ-15).
func formatArgvInput(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = strconv.Quote(a)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// parseArgvInput reads the argv input back: blank is no argv, anything else
// must be a TOML array of strings, parsed by the same decoder config.Parse
// uses, so the input means exactly what it would mean in harness.toml. Extra
// keys smuggled in after a newline are refused rather than ignored.
func parseArgvInput(in string) ([]string, error) {
	in = strings.TrimSpace(in)
	if in == "" {
		return nil, nil
	}
	var v struct {
		Argv []string `toml:"argv"`
	}
	md, err := toml.Decode("argv = "+in, &v)
	if err == nil && len(md.Undecoded()) > 0 {
		err = fmt.Errorf("unexpected %s", md.Undecoded()[0])
	}
	if err != nil {
		return nil, fmt.Errorf(`argv must be a TOML array of strings, e.g. ["/usr/local/bin/report", "--date", "a b"]: %v`, err)
	}
	if v.Argv == nil {
		v.Argv = []string{}
	}
	return v.Argv, nil
}

// readFileOrEmpty reads path, returning empty (not an error) when it's absent so
// a first-ever harness can be created against a not-yet-existing config.
func readFileOrEmpty(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	return b, err
}

// writeFile writes body to path with owner-only-ish perms.
func writeFile(path string, body []byte) error {
	return os.WriteFile(path, body, 0o644)
}
