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

	"github.com/anmitsu/go-shlex"
	"github.com/robfig/cron/v3"

	"gitea.stump.rocks/stump.wtf/harness/internal/config"
	"gitea.stump.rocks/stump.wtf/harness/internal/core"
	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// HarnessForm is the editable harness schema behind the n/e Huh form. It is the
// TUI-facing projection of a core.Harness table; RestartDelay is seconds to
// match the TOML unit (config.rawHarness.RestartDelay).
type HarnessForm struct {
	Name string
	// Harness is the harness-kind enum (crush/claude-code/codex/generic). It
	// selects the adapter, which supplies the executable for a long-running
	// harness and the argv synthesis for a prompt one-shot (ADR-0011). It is
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
	// Schedule is the daemon-owned cron expression for a scheduled one-shot
	// (issue #66): requires Prompt, and is mutually exclusive with Enabled and
	// with a respawning restart policy (Validate mirrors the parser on all
	// three). Carried through the form so an edit round-trips it — the save
	// path rewrites the whole table, so a field the form drops is a schedule
	// silently deleted from harness.toml.
	Schedule     string
	Args         []string
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
	promptSet := strings.TrimSpace(f.Prompt) != ""
	switch f.Harness {
	case "crush", "claude-code", "codex", "generic":
	case "":
		return fmt.Errorf("harness is required (one of: crush, claude-code, codex, generic)")
	default:
		return fmt.Errorf("harness must be one of: crush, claude-code, codex, generic")
	}
	if promptSet && len(f.Args) > 0 {
		return fmt.Errorf("prompt and args are mutually exclusive")
	}
	if model := strings.TrimSpace(f.Model); model != "" {
		if !promptSet {
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
		if !promptSet {
			return fmt.Errorf("schedule requires prompt (a scheduled harness is a one-shot agent run)")
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
	return nil
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
	if prompt != "" {
		// Prompt harness: `prompt` replaces args entirely (Validate
		// enforces the exclusivity; the daemon synthesizes the argv at spawn,
		// ADR-0011). `model`, `auto_accept`, and `max_turns` ride beside it as
		// config truth — never as synthesized args (issues #57, #58, #59).
		fmt.Fprintf(&b, "prompt = %s\n", strconv.Quote(prompt))
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
		if schedule := strings.TrimSpace(f.Schedule); schedule != "" {
			// The daemon fires this one-shot on a cron cadence (issue #66).
			// Prompt-only, like the knobs above: Validate rejects a schedule
			// without a prompt, so this branch is the only place it can appear.
			fmt.Fprintf(&b, "schedule = %s\n", strconv.Quote(schedule))
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
	// "no" for prompt one-shots, "always" otherwise — so an untouched edit
	// round-trips without growing keys.
	defaultRestart := string(core.RestartAlways)
	if prompt != "" {
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
	return b.String()
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
		name:        sel.Name,
		harness:     sel.Adapter,
		prompt:      sel.Prompt,
		model:       sel.Model,
		autoAccept:  sel.AutoAccept,
		quiet:       sel.Quiet,
		maxTurns:    strconv.Itoa(sel.MaxTurns),
		backend:     orDefault(sel.Backend, string(core.BackendNative)),
		description: sel.Description,
		enabled:     sel.Enabled,
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
	fi.model = h.Model
	fi.autoAccept = h.AutoAccept
	fi.quiet = h.Quiet
	fi.maxTurns = strconv.Itoa(h.MaxTurns)
	fi.schedule = h.Schedule
	fi.args = shellQuoteJoin(h.Args)
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
	fi.mcpAllow = strings.Join(h.MCPAllow, " ")
	return fi
}

// toForm converts the Huh string-bound inputs into a typed HarnessForm, parsing
// space-separated args and the integer restart_delay.
func (fi formInputs) toForm() HarnessForm {
	f := HarnessForm{
		Name:        strings.TrimSpace(fi.name),
		Harness:     strings.TrimSpace(fi.harness),
		Prompt:      strings.TrimSpace(fi.prompt),
		Model:       strings.TrimSpace(fi.model),
		AutoAccept:  fi.autoAccept,
		Quiet:       fi.quiet,
		Schedule:    strings.TrimSpace(fi.schedule),
		Workdir:     strings.TrimSpace(fi.workdir),
		EnvFile:     strings.TrimSpace(fi.envFile),
		Restart:     strings.TrimSpace(fi.restart),
		Backend:     strings.TrimSpace(fi.backend),
		TmuxSocket:  strings.TrimSpace(fi.tmuxSocket),
		Description: strings.TrimSpace(fi.description),
		Enabled:     fi.enabled,

		HarvestTrajectory: fi.harvestTrajectory,
	}
	if args, err := shlex.Split(fi.args, true); err == nil && len(args) > 0 {
		f.Args = args
	}
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
	return f
}

// shellQuoteJoin joins args into a single string with shell-style quoting so
// that an argument containing whitespace survives the round-trip through the
// single-line text input. Args without whitespace are left bare; args with
// whitespace are wrapped in double quotes with embedded double quotes escaped.
func shellQuoteJoin(args []string) string {
	parts := make([]string, len(args))
	for i, a := range args {
		if !strings.ContainsAny(a, " \t\n\"'") {
			parts[i] = a
			continue
		}
		parts[i] = "\"" + strings.ReplaceAll(a, "\"", "\\\"") + "\""
	}
	return strings.Join(parts, " ")
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
