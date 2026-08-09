package tui

// Governing: SPEC-0001 REQ "Harness Form" — n/e open a Huh form over the harness
// schema (cmd/prompt/model/auto_accept/args/workdir/env_file/restart_delay/
// restart/backend/description/profile membership) that writes back to
// harness.toml (ADR-0006: file is truth); e
// pre-fills from the existing harness; then the daemon reloads and the harness
// appears on the dashboard. This file owns the schema<->TOML serialization; the
// Huh widget wiring lives in overlays.go.

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"

	"gitea.stump.rocks/stump.wtf/harness/internal/config"
	"gitea.stump.rocks/stump.wtf/harness/internal/core"
	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// HarnessForm is the editable harness schema behind the n/e Huh form. It is the
// TUI-facing projection of a core.Harness table; RestartDelay is seconds to
// match the TOML unit (config.rawHarness.RestartDelay).
type HarnessForm struct {
	Name string
	Cmd  string
	// Prompt is the agent one-shot alternative to Cmd (ADR-0011): exactly one
	// of the two is set, and Args belong to Cmd only (Validate enforces both).
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
	Quiet        bool
	Args         []string
	Workdir      string
	EnvFile      string
	RestartDelay int    // seconds
	Restart      string // core.RestartPolicy; empty = the parse default
	Backend      string
	Description  string
	Enabled      bool
}

// NewHarnessForm is a blank form for `n` with sane defaults (native backend).
func NewHarnessForm() HarnessForm {
	// A prompt one-shot is headless by default (issue #60); the form lets the
	// user opt back into output by clearing Quiet before saving.
	return HarnessForm{Backend: string(core.BackendNative), Quiet: true}
}

// Validate checks the minimum the daemon config parser requires (a name and
// exactly one of cmd/prompt, a known backend, non-negative delay) so the form
// catches errors before writing TOML the daemon would reject on reload.
func (f HarnessForm) Validate() error {
	if strings.TrimSpace(f.Name) == "" {
		return fmt.Errorf("name is required")
	}
	cmdSet := strings.TrimSpace(f.Cmd) != ""
	promptSet := strings.TrimSpace(f.Prompt) != ""
	switch {
	case !cmdSet && !promptSet:
		return fmt.Errorf("cmd is required (or prompt for an agent one-shot)")
	case cmdSet && promptSet:
		return fmt.Errorf("cmd and prompt are mutually exclusive")
	case promptSet && len(f.Args) > 0:
		return fmt.Errorf("prompt and args are mutually exclusive")
	}
	if model := strings.TrimSpace(f.Model); model != "" {
		if !promptSet {
			return fmt.Errorf("model requires prompt (for a cmd harness, pass --model in args)")
		}
		if strings.ContainsFunc(model, unicode.IsSpace) {
			return fmt.Errorf("model must be a single token (no whitespace)")
		}
	}
	if f.AutoAccept && !promptSet {
		return fmt.Errorf("auto_accept requires prompt (for a cmd harness, pass the tool's flag in args)")
	}
	if f.MaxTurns < 0 {
		return fmt.Errorf("max_turns must not be negative")
	}
	if f.MaxTurns > 0 && !promptSet {
		return fmt.Errorf("max_turns requires prompt (for a cmd harness, pass --max-turns in args)")
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
	return nil
}

// TOML renders the form as a `[harness.<name>]` table. Only set fields are
// emitted so the file stays clean. The output re-parses through config.Parse
// into an equivalent harness (round-trip guarantee, tested).
func (f HarnessForm) TOML() string {
	var b strings.Builder
	fmt.Fprintf(&b, "[harness.%s]\n", f.Name)
	prompt := strings.TrimSpace(f.Prompt)
	if prompt != "" {
		// Prompt harness: `prompt` replaces cmd/args entirely (Validate
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
	} else {
		fmt.Fprintf(&b, "cmd = %s\n", strconv.Quote(f.Cmd))
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
	if f.Description != "" {
		fmt.Fprintf(&b, "description = %s\n", strconv.Quote(f.Description))
	}
	if f.Enabled {
		b.WriteString("enabled = true\n")
	}
	return b.String()
}

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
		cmd:         sel.Cmd,
		prompt:      sel.Prompt,
		model:       sel.Model,
		autoAccept:  sel.AutoAccept,
		quiet:       sel.Quiet,
		maxTurns:    strconv.Itoa(sel.MaxTurns),
		backend:     orDefault(sel.Backend, string(core.BackendNative)),
		description: sel.Description,
		enabled:     sel.Enabled,
	}
	cfg, err := config.Load(path)
	if err != nil {
		return fi
	}
	h, ok := cfg.Harnesses[sel.Name]
	if !ok {
		return fi
	}
	fi.cmd = h.Cmd
	fi.prompt = h.Prompt
	fi.model = h.Model
	fi.autoAccept = h.AutoAccept
	fi.quiet = h.Quiet
	fi.maxTurns = strconv.Itoa(h.MaxTurns)
	fi.args = strings.Join(h.Args, " ")
	fi.workdir = h.Workdir
	fi.envFile = h.EnvFile
	if h.RestartDelay > 0 {
		fi.delay = strconv.Itoa(int(h.RestartDelay / time.Second))
	}
	fi.restart = orDefault(string(h.Restart), string(core.RestartAlways))
	fi.backend = orDefault(string(h.Backend), string(core.BackendNative))
	fi.description = h.Description
	fi.enabled = h.Enabled
	return fi
}

// toForm converts the Huh string-bound inputs into a typed HarnessForm, parsing
// space-separated args and the integer restart_delay.
func (fi formInputs) toForm() HarnessForm {
	f := HarnessForm{
		Name:        strings.TrimSpace(fi.name),
		Cmd:         strings.TrimSpace(fi.cmd),
		Prompt:      strings.TrimSpace(fi.prompt),
		Model:       strings.TrimSpace(fi.model),
		AutoAccept:  fi.autoAccept,
		Quiet:       fi.quiet,
		Workdir:     strings.TrimSpace(fi.workdir),
		EnvFile:     strings.TrimSpace(fi.envFile),
		Restart:     strings.TrimSpace(fi.restart),
		Backend:     strings.TrimSpace(fi.backend),
		Description: strings.TrimSpace(fi.description),
		Enabled:     fi.enabled,
	}
	if args := strings.Fields(fi.args); len(args) > 0 {
		f.Args = args
	}
	if d, err := strconv.Atoi(strings.TrimSpace(fi.delay)); err == nil {
		f.RestartDelay = d
	}
	if n, err := strconv.Atoi(strings.TrimSpace(fi.maxTurns)); err == nil {
		f.MaxTurns = n
	}
	return f
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
