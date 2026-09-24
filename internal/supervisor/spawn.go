package supervisor

// Governing: ADR-0005 (each running harness has a supervisor goroutine that
// spawns `cmd args` under a PTY in `workdir`, with `env_file` loaded);
// ADR-0003 (native backend runs the process under the daemon's own PTY via
// x/xpty); ADR-0008 (secrets stay in env_file — loaded into the child's
// environment at spawn, never copied into state/logs we write).

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/charmbracelet/x/xpty"
	"github.com/robfig/cron/v3"

	"github.com/stump-wtf/harness/internal/adapter"
	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/tmpl"
)

// defaultPTYCols/Rows size a freshly spawned PTY when no client viewport is
// known (ADR-0003; a real attach resizes it later).
const (
	defaultPTYCols = 80
	defaultPTYRows = 24
)

// expandHome expands a leading ~ (or ~/) in p to the user's home directory.
func expandHome(p string) string {
	if p == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
		return p
	}
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// parseEnvFile reads a KEY=VALUE env file into a slice of "KEY=VALUE" strings
// suitable for exec.Cmd.Env. Blank lines and #-comments are skipped; a leading
// `export ` is tolerated; surrounding single or double quotes on the value are
// stripped. A missing path is not an error (the harness simply has no extra
// env); other read errors are surfaced. Secrets stay here (ADR-0008).
func parseEnvFile(path string) ([]string, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	path = expandHome(path)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("supervisor: open env_file: %w", err)
	}
	defer f.Close()

	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue // not a KEY=VALUE line
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		val = unquote(val)
		out = append(out, key+"="+val)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("supervisor: read env_file: %w", err)
	}
	return out, nil
}

// DiscoveryEnv returns the values h's process sees for keys — its env_file
// layered over the daemon's own environment, the same composition buildEnv
// gives the child — and nothing else. It exists so trajectory discovery can
// follow a tool an env_file relocated (CRUSH_GLOBAL_DATA, CLAUDE_CONFIG_DIR)
// without the caller ever holding the rest of that file, which is where a
// harness's secrets live (ADR-0008). An unreadable env_file still returns the
// daemon-environment values, alongside the error.
//
// Governing: ADR-0008, SPEC-0006 REQ "Run Correlation".
func DiscoveryEnv(h core.Harness, keys []string) (map[string]string, error) {
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		if v, ok := os.LookupEnv(k); ok {
			out[k] = v
		}
	}
	extra, err := parseEnvFile(h.EnvFile)
	for _, kv := range extra {
		k, v, _ := strings.Cut(kv, "=")
		for _, want := range keys {
			if k == want {
				out[k] = v
				break
			}
		}
	}
	return out, err
}

// EnvFileValues reads only keys from the env file at path, with the same
// parser a harness env_file gets, and returns nothing else — the rest of the
// file (a harness's own secrets, say) never leaves this function. A missing
// file yields an empty map; callers that require the file check for it
// themselves. Nothing is exported into the process environment.
//
// Governing: ADR-0008; SPEC-0015 REQ-3 ([telemetry] env_file shares this
// parser).
func EnvFileValues(path string, keys []string) (map[string]string, error) {
	kvs, err := parseEnvFile(path)
	if err != nil {
		return nil, err
	}
	want := make(map[string]bool, len(keys))
	for _, k := range keys {
		want[k] = true
	}
	out := make(map[string]string)
	for _, kv := range kvs {
		k, v, _ := strings.Cut(kv, "=")
		if want[k] {
			out[k] = v
		}
	}
	return out, nil
}

// unquote strips a single matching pair of surrounding single or double quotes.
func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// RunEnv is the run-context environment a recorded run's process is spawned
// with (SPEC-0014 REQ "Event Delivery To The Run"). Every field is an
// identifier or a path — never a payload byte, a header value or a credential.
//
// The names are RESERVED: they are appended after `env_file`, so they win a
// collision with one an operator set. A harness that set its own
// HARNESS_RUN_ID would otherwise make the agent's own run correlation lie,
// and the daemon's answer has to be the authoritative one.
type RunEnv struct {
	// RunID is the run's id, 0 for a start with no run record.
	RunID int
	// Trigger is what started the run: schedule, catch_up, manual, channel
	// or webhook.
	Trigger RunTrigger
	// Source is the trigger source reference, empty when no source caused
	// the run.
	Source string
	// EventFile is the absolute path of the run's 0600 event file, empty
	// when the run carries no event.
	EventFile string
	// StartedAt is when the run's record says it started, zero for a start
	// with no record. It is not an environment variable: it feeds the argv
	// template context's run.started_at and run.date (SPEC-0017 REQ-7), so a
	// respawn of the same run renders the same values.
	StartedAt time.Time
	// PromptPath is where a `command` harness with prompt_delivery = "file"
	// has its prompt written: <jobs dir>/<harness>/<run_id>.prompt for a run
	// with a record, $XDG_STATE_HOME/harness/prompts/<harness>.prompt
	// otherwise. It is not itself an environment variable: spawn exports
	// HARNESS_PROMPT_FILE only for a file delivery (SPEC-0017 REQ-12).
	PromptPath string
}

// vars renders r as KEY=VALUE pairs, omitting the two that are meaningful only
// for an event-driven run. Omission is the contract, not an empty string: REQ
// "Event Delivery To The Run" says HARNESS_RUN_SOURCE and HARNESS_EVENT_FILE
// are UNSET for a scheduled firing, and an agent testing `if [ -n "$X" ]`
// cannot tell an empty value from a missing one — but one testing for the
// variable's presence can.
func (r RunEnv) vars() []string {
	var out []string
	if r.RunID > 0 {
		out = append(out, "HARNESS_RUN_ID="+strconv.Itoa(r.RunID))
	}
	if r.Trigger != "" {
		out = append(out, "HARNESS_RUN_TRIGGER="+string(r.Trigger))
	}
	if r.Source != "" {
		out = append(out, "HARNESS_RUN_SOURCE="+r.Source)
	}
	if r.EventFile != "" {
		out = append(out, "HARNESS_EVENT_FILE="+r.EventFile)
	}
	return out
}

// buildEnv composes the child environment: the daemon's own environment plus
// the parsed env_file (env_file wins on key collisions, appended last), and
// then the run context, which wins over both.
//
// The child runs under a real, color-capable PTY (xpty), so it must advertise
// one: a daemon launched detached (launchd/systemd, a closed login shell) can
// carry a TERM that is missing or "dumb" and no COLORTERM, which makes
// full-screen harness apps render in black & white. We therefore guarantee
// sane terminal defaults — a color-capable TERM and COLORTERM=truecolor —
// for any key neither the daemon's environment nor the env_file already set.
func buildEnv(h core.Harness, run RunEnv) ([]string, error) {
	extra, err := parseEnvFile(h.EnvFile)
	if err != nil {
		return nil, err
	}
	// The daemon's own environment is never a source of run context: a
	// daemon started from inside a run (an agent that restarted it, say)
	// inherits that run's HARNESS_EVENT_FILE, and passing it on would hand
	// every later child another run's attacker-supplied event file.
	env := withoutRunVars(os.Environ())
	env = ensureTermEnv(env)
	if run.recorded() {
		// A recorded run's context is the daemon's to state, including
		// which of it is UNSET. Appending alone cannot unset anything, so
		// an env_file HARNESS_EVENT_FILE would otherwise reach a scheduled
		// run that has no event (REQ "Event Delivery To The Run").
		extra = withoutRunVars(extra)
	}
	env = append(env, extra...)
	// Last, so exec.Cmd.Env's later-wins rule makes these authoritative over
	// both the daemon's environment and env_file.
	env = append(env, run.vars()...)
	return env, nil
}

// runVarNames are the reserved run-context variables the daemon sets:
// RunEnv's four, and HARNESS_PROMPT_FILE, which spawn sets for a file prompt
// delivery (SPEC-0017 REQ-12) and which, like the others, is never inherited
// from the daemon's own environment.
var runVarNames = []string{"HARNESS_RUN_ID", "HARNESS_RUN_TRIGGER", "HARNESS_RUN_SOURCE", "HARNESS_EVENT_FILE", promptFileVar}

// promptFileVar names a file-delivery prompt's absolute path in the child's
// environment (SPEC-0017 REQ-12).
const promptFileVar = "HARNESS_PROMPT_FILE"

// recorded reports whether r describes a run with a record, i.e. whether the
// daemon has an authoritative value for every reserved name.
func (r RunEnv) recorded() bool { return r.RunID > 0 || r.Trigger != "" }

// withoutRunVars returns env minus every entry for a reserved run-context
// name.
func withoutRunVars(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if slices.Contains(runVarNames, k) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// ensureTermEnv returns env with a color-capable TERM and COLORTERM=truecolor
// appended for any terminal key it does not already set. Existing values win:
// a daemon that already exports TERM=xterm-kitty (or an env_file that pins
// COLORTERM) is left untouched, so this only repairs a colorless default.
//
// exec.Cmd.Env semantics: later duplicates shadow earlier ones, so appending
// only when absent is both sufficient and the conservative choice — we never
// clobber a deliberate value with our generic default.
func ensureTermEnv(env []string) []string {
	var hasTERM, hasCOLORTERM bool
	for _, kv := range env {
		if k, _, ok := strings.Cut(kv, "="); ok {
			switch k {
			case "TERM":
				hasTERM = true
			case "COLORTERM":
				hasCOLORTERM = true
			}
		}
	}
	// Treat an empty or "dumb" TERM as absent: it advertises no color and no
	// cursor addressing, which is exactly the broken default we are repairing.
	if !hasTERM || envValue(env, "TERM") == "" || envValue(env, "TERM") == "dumb" {
		env = append(env, "TERM=xterm-256color")
	}
	if !hasCOLORTERM {
		env = append(env, "COLORTERM=truecolor")
	}
	return env
}

// envValue returns the value of the last KEY= entry in env ("" if unset).
func envValue(env []string, key string) string {
	val := ""
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			val = v
		}
	}
	return val
}

// expandArgs substitutes the {workdir} placeholder in each arg with the
// expanded working directory (ADR-0006 documents {workdir} expansion happening
// at spawn time in the supervisor, not the config parser).
func expandArgs(args []string, workdir string) []string {
	if len(args) == 0 {
		return nil
	}
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = strings.ReplaceAll(a, "{workdir}", workdir)
	}
	return out
}

// resolvePrompt returns h with its prompt text in place: unchanged for an
// inline prompt or a cmd harness, and — for a prompt_file harness — a copy
// whose Prompt holds the file's contents and whose PromptFile is cleared.
//
// The read happens per spawn, so editing the referenced file changes the next
// run with no config reload; that is the whole point of naming a file instead
// of inlining the text (ADR-0018). The copy is local to this spawn and never
// written back to the registry, so config truth stays the PATH: the wire, the
// state file, and every config writer keep round-tripping prompt_file rather
// than inlining the document.
//
// Config load already validated this path with the same reader, but a file can
// be deleted between load and spawn, so the failure is handled here too — as a
// hard error, because an agent launched with an empty instruction is a silent
// no-op, the exact failure mode this feature removes.
// Governing: ADR-0018; SPEC-0006 REQ "Prompt Source".
func resolvePrompt(h core.Harness) (core.Harness, error) {
	if h.PromptFile == "" {
		return h, nil
	}
	prompt, err := core.ReadPromptFile(expandHome(h.PromptFile))
	if err != nil {
		return h, fmt.Errorf("supervisor: harness %q: prompt_file %w", h.Name, err)
	}
	h.Prompt = prompt
	h.PromptFile = ""
	return h, nil
}

// ErrGenericPrompt is returned when a harness reaches spawn carrying a prompt
// its adapter cannot synthesize an argv for — in practice `generic` (or an
// empty/unknown kind, which Resolve maps to Generic). Config validation and
// the wire reject the combination first; this is the backstop for any path
// that bypassed them, and it fails the start rather than running some agent
// in the operator's place.
// Governing: ADR-0023, SPEC-0017 REQ "Generic Kind Rejects Prompts".
var ErrGenericPrompt = errors.New(`"generic" runs sh and has no prompt synthesis`)

// ErrCommandPrompt is returned when a `command` harness reaches spawn carrying
// a prompt that nothing delivers, or a prompt_delivery that contradicts its
// argv (SPEC-0017 REQ-12's validation matrix, core.CheckCommandPrompt).
// Running the argv without the instruction the operator wrote would be a
// silent no-op. Config validation and the wire reject the combination first;
// this is the backstop.
// Governing: ADR-0023, SPEC-0017 REQ-2 "Command Harness Kind", REQ-12 "Prompt
// Delivery".
var ErrCommandPrompt = errors.New(`a "command" harness has a prompt it cannot deliver`)

// ErrPromptDelivery is returned when a `command` harness's prompt could not be
// handed to the program the way prompt_delivery says: the prompt file could
// not be written, the stdin pipe could not be made, or the rendered argv
// exceeds the platform's argument limits. The start fails and a run with a
// record is recorded `failed`: a one-shot launched without its instruction is
// the silent no-op this is here to prevent. It is never swallowed; beginStart
// logs it.
// Governing: ADR-0023, SPEC-0017 REQ-12 "Prompt Delivery", REQ "Error
// Handling Standards".
var ErrPromptDelivery = errors.New("prompt delivery failed")

// execArgv resolves the executable and argv spawn runs: the configured cmd
// with {workdir}-expanded args, or — for a prompt harness (empty Cmd, ADR-0011
// spawn-time synthesis) — the argv the adapter registry resolves from the
// harness's `harness` kind. Prompt and options are passed
// verbatim: only configured args go through expandArgs's {workdir}
// substitution, never those — a prompt legitimately containing "{workdir}" is
// instruction text, not a placeholder. The cmd path ignores Model, AutoAccept,
// and MaxTurns entirely (config validation forbids the combinations; a wire
// def carrying them spawns on its configured argv alone).
func execArgv(h core.Harness, workdir string, run RunEnv) (spawnPlan, error) {
	return execArgvWithRegistry(h, workdir, run, adapter.NewRegistryWithDefaults())
}

// spawnPlan is what spawn execs: the executable and its arguments, already
// rendered, and — for a `command` harness with a prompt — how the prompt
// reaches it. It is built once per spawn and never stored, so a rendered
// value lives only in the child's argv, its stdin pipe or its 0600 prompt
// file (SPEC-0017 REQ-11, REQ-12).
// Governing: SPEC-0017 design.md § "Where rendering happens in spawn".
type spawnPlan struct {
	name string
	args []string
	// delivery is the effective prompt_delivery of a command harness that
	// carries a prompt, and "" for everything else: a built-in adapter's
	// prompt is already inside args.
	delivery string
	// prompt is the text to deliver when delivery is "stdin" or "file".
	prompt string
	// promptFile is the absolute path a "file" delivery writes, 0600, before
	// exec, and exports as HARNESS_PROMPT_FILE.
	promptFile string
}

// renderContext is the template context a command harness's argv renders
// against: the operator tier (harness.name, harness.workdir, model) and the
// daemon tier (run.*), and nothing else (SPEC-0017 REQ-7). A path is absent,
// not empty, when the run does not have it — no record means no run.id or
// run.trigger, no source means no run.source — so a required reference fails
// the render instead of passing an empty argument.
//
// The prompt bindings are added by the caller, per prompt_delivery: `prompt`
// only for "argv", `prompt_file` only for "file" (REQ-12).
//
// It holds no untrusted values at all: argv may never reference free text
// (REQ-10), and an empty untrusted namespace makes that true at render time
// too, not only at load.
// Governing: ADR-0023, SPEC-0017 REQ-7 "Template Context".
func renderContext(h core.Harness, workdir string, run RunEnv, now time.Time) tmpl.Map {
	started := run.StartedAt
	if started.IsZero() {
		started = now
	}
	v := map[string]string{
		core.PathHarnessName:    h.Name,
		core.PathHarnessWorkdir: workdir,
		core.PathRunStartedAt:   started.UTC().Format(time.RFC3339),
		core.PathRunDate:        started.In(scheduleZone(h.Schedule)).Format(time.DateOnly),
	}
	if h.Model != "" {
		v[core.PathModel] = h.Model
	}
	if run.RunID > 0 {
		v[core.PathRunID] = strconv.Itoa(run.RunID)
	}
	if run.Trigger != "" {
		v[core.PathRunTrigger] = string(run.Trigger)
	}
	if run.Source != "" {
		v[core.PathRunSource] = run.Source
	}
	return tmpl.Map{Values: v}
}

// scheduleZone is the zone run.date is written in: the schedule's
// CRON_TZ=/TZ= prefix when it has one, else the daemon's local zone — the same
// zone the scheduler reads the expression in, so a run fired at 00:30 by
// `CRON_TZ=Asia/Tokyo 30 0 * * *` is dated the Tokyo day it was due.
// Governing: SPEC-0017 REQ-7 (`run.date`), SPEC-0008 REQ "Schedule Time Zone".
func scheduleZone(schedule string) *time.Location {
	if schedule == "" {
		return time.Local
	}
	sch, err := cron.ParseStandard(schedule)
	if err != nil {
		return time.Local
	}
	if spec, ok := sch.(*cron.SpecSchedule); ok && spec.Location != nil {
		return spec.Location
	}
	return time.Local
}

// execArgvWithRegistry is the adapter-aware version of execArgv. For a prompt
// harness it resolves the adapter via the registry and delegates to its
// PromptCommand, so each agent CLI gets its own flags instead of everything
// being hardcoded to crush. An adapter with no prompt mode answers with an
// empty executable, and that is an error here, never a fallback: the check
// keys on what PromptCommand returned rather than on the adapter's name, so a
// Generic that went back to borrowing another agent's argv would fail the
// spawn tests instead of passing them. For a long-running harness it returns
// the adapter's executable with the configured args, {workdir}-expanded.
//
// A `command` harness (an adapter.ArgvOwner) is answered first and from its
// own Argv alone: argv[0] exactly as configured, and each argv[1:] element
// rendered to exactly one argument against run's context — no shell, no
// {workdir} expansion, and neither Executable nor PromptCommand consulted.
// The argv is re-checked here with the same rule every front door applies, so
// a definition that bypassed them fails the start instead of exec'ing an
// empty or placeholder executable. A required value the run does not have
// returns an error wrapping *tmpl.UnresolvedError, and nothing is exec'd;
// beginStart turns that into a recorded skip or a failed start.
//
// A command harness's prompt (already resolved from prompt_file) is bound per
// prompt_delivery: as {{prompt}} for "argv", as {{prompt_file}} naming
// run.PromptPath for "file", and not at all for "stdin", whose bytes the plan
// carries for spawn to pipe in. The delivery matrix is re-checked too, so a
// prompt that nothing delivers fails the start with ErrCommandPrompt.
// Governing: issue #74 (adapter-aware prompt synthesis), SPEC-0017 REQ
// "Generic Kind Rejects Prompts", REQ-2 "Command Harness Kind", REQ-11
// "Rendering", REQ-12 "Prompt Delivery".
func execArgvWithRegistry(h core.Harness, workdir string, run RunEnv, reg *adapter.Registry) (spawnPlan, error) {
	if owner, ok := reg.Resolve(h).(adapter.ArgvOwner); ok {
		if err := core.CheckCommandArgv(h.Argv); err != nil {
			return spawnPlan{}, fmt.Errorf("supervisor: harness %q: %w", h.Name, err)
		}
		hasPrompt := h.Prompt != ""
		if err := core.CheckCommandPrompt(h.Adapter, h.Argv, h.PromptDelivery, hasPrompt); err != nil {
			return spawnPlan{}, fmt.Errorf("supervisor: harness %q: %w: %v", h.Name, ErrCommandPrompt, err)
		}
		plan := spawnPlan{}
		ctx := renderContext(h, workdir, run, time.Now())
		if hasPrompt {
			plan.delivery = core.EffectivePromptDelivery(h.Argv, h.PromptDelivery)
			switch plan.delivery {
			case core.PromptDeliveryArgv:
				ctx.Values[core.PathPrompt] = h.Prompt
			case core.PromptDeliveryStdin:
				plan.prompt = h.Prompt
			case core.PromptDeliveryFile:
				if run.PromptPath == "" {
					return spawnPlan{}, fmt.Errorf("supervisor: harness %q: %w: no prompt file path", h.Name, ErrPromptDelivery)
				}
				abs, err := filepath.Abs(run.PromptPath)
				if err != nil {
					return spawnPlan{}, fmt.Errorf("supervisor: harness %q: %w: prompt file path: %v", h.Name, ErrPromptDelivery, err)
				}
				plan.prompt, plan.promptFile = h.Prompt, abs
				ctx.Values[core.PathPromptFile] = abs
			}
		}
		name, args := owner.Argv(h, workdir)
		rendered, err := core.RenderCommandArgs(args, ctx)
		if err != nil {
			return spawnPlan{}, fmt.Errorf("supervisor: harness %q: %w", h.Name, err)
		}
		plan.name, plan.args = name, rendered
		return plan, nil
	}
	if h.Prompt != "" {
		opts := core.AgentOpts{
			Model:      h.Model,
			AutoAccept: h.AutoAccept,
			MaxTurns:   h.MaxTurns,
			Quiet:      h.Quiet,
		}
		cmd, args := reg.Resolve(h).PromptCommand(h.Prompt, opts)
		if cmd == "" {
			return spawnPlan{}, fmt.Errorf("supervisor: harness %q: %w", h.Name, ErrGenericPrompt)
		}
		return spawnPlan{name: cmd, args: args}, nil
	}
	// Long-running harness: the adapter owns the executable (the `harness`
	// enum key); configured args are appended after it.
	return spawnPlan{name: reg.Resolve(h).Executable(), args: expandArgs(h.Args, workdir)}, nil
}

// writePromptFile writes a file-delivery prompt to path, mode 0600, replacing
// whatever was there. It writes a temporary file beside it and renames it
// into place, so the program never reads a half-written prompt and a file an
// operator loosened by hand cannot keep its old mode: the rename puts a fresh
// 0600 inode at the path. The directory is made 0700, like the jobs directory
// the event file lives in.
//
// 0600 is the requirement, not a default: the prompt is the operator's
// instruction and may say more than the jobs directory's other readers
// should see. A variable so a test can drive the failure path.
// Governing: SPEC-0017 REQ-12 "Prompt Delivery"; ADR-0008.
var writePromptFile = func(path, prompt string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.WriteString(prompt); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	// CreateTemp already makes the file 0600; say so rather than rely on it.
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// process is a live spawned harness: its PTY, the command handle (for signals
// and reaping), and its OS pid.
type process struct {
	pty xpty.Pty
	cmd *exec.Cmd
	pid int
	// stdin is the prompt feed of a prompt_delivery = "stdin" run, nil for
	// every other spawn. Its presence is what "this run's stdin is its
	// prompt" means: attach input is refused while it is set (SPEC-0017
	// REQ-12).
	stdin *stdinFeed
}

// stdinIsPrompt reports whether the process's fd 0 is a prompt pipe rather
// than the PTY, so nothing typed at an attach can reach the program.
func (p *process) stdinIsPrompt() bool { return p != nil && p.stdin != nil }

// stdinFeed writes a stdin-delivery prompt into the write end of the child's
// fd 0 pipe on its own goroutine, then closes it so the child reads EOF. A
// pipe is byte-exact at any size, where the PTY's line discipline would echo
// the prompt, cap a line near 4 KiB (1 KiB on macOS) and turn ^C/^D/^Z bytes
// into signals and EOF.
//
// The goroutine cannot outlive the run: finish, which the supervisor's wait
// calls as soon as the process is reaped, closes the write end — waking a
// Write still blocked on a pipe nobody drains, say one a surviving descendant
// holds open — and waits for the goroutine to return.
// Governing: ADR-0023, SPEC-0017 REQ-12; design.md § "`stdin` delivery moves
// the controlling terminal to fd 1".
type stdinFeed struct {
	w    *os.File
	done chan struct{}
	// written and err are the goroutine's result, set before done closes and
	// read only after it has: a short write means the child exited, or
	// closed its stdin, before it read the whole prompt.
	written int
	err     error
}

// startStdinFeed starts writing prompt into w.
func startStdinFeed(w *os.File, prompt string) *stdinFeed {
	f := &stdinFeed{w: w, done: make(chan struct{})}
	go func() {
		defer close(f.done)
		f.written, f.err = w.WriteString(prompt)
		if cerr := w.Close(); f.err == nil && cerr != nil && !errors.Is(cerr, os.ErrClosed) {
			f.err = cerr
		}
		if hook := stdinFeedExited.Load(); hook != nil {
			(*hook)()
		}
	}()
	return f
}

// finish closes the pipe's write end and waits for the feed goroutine.
// Idempotent.
func (f *stdinFeed) finish() {
	if f == nil {
		return
	}
	_ = f.w.Close()
	<-f.done
}

// stdinFeedExited is a test seam, unset outside tests: it runs as a feed
// goroutine returns, so a test can show that one cannot outlive its run.
// Atomic because feeds read it from their own goroutines.
var stdinFeedExited atomic.Pointer[func()]

// Workdir is the process working directory the supervisor spawns h into: the
// configured value with a leading ~ expanded. Exported so the control plane can
// put the same string on the wire that spawn assigns to cmd.Dir — a client
// correlating an agent transcript's cwd back to a harness is comparing against
// this, and a second expansion elsewhere is a second thing to drift.
func Workdir(h core.Harness) string { return expandHome(h.Workdir) }

// spawn launches h under a fresh PTY of cols×rows in its workdir with env_file
// loaded. The child is placed in its own session (Setsid) so the whole process
// group can be signalled on graceful stop (SPEC-0003 REQ "Graceful Stop"). The
// returned process's PTY is the raw byte stream the caller tees to logs.
//
// Governing: ADR-0003 (the native backend owns PTY sizing; the attach layer's
// smallest-attached-wins viewport is authoritative). The size is passed in
// rather than fixed at 80×24 because a harness is routinely (re)started while a
// client is already attached — a restart, a crash-restart, or `^b s` from
// attached mode. Spawning at 80×24 in that case leaves the child permanently
// undersized: the mux's recorded size already equals the client viewport, so
// its resize policy sees no change and never pushes a TIOCSWINSZ, and the app
// inside renders into an 80×24 box in the corner of a full-size window.
//
// A `command` harness's prompt is delivered here, per its plan (SPEC-0017
// REQ-12): a "file" delivery writes the 0600 prompt file before exec and
// exports HARNESS_PROMPT_FILE last, so it wins over the daemon's environment
// and env_file; a "stdin" delivery makes fd 0 a pipe the prompt is written
// into, leaves fd 1 and fd 2 on the PTY, and makes the PTY the controlling
// terminal through fd 1. Any failure there is ErrPromptDelivery, and nothing
// is left running.
func spawn(h core.Harness, cols, rows int, run RunEnv) (*process, error) {
	// Resolve prompt_file to its text BEFORE allocating anything: a missing
	// instruction file must fail the start outright rather than leak a PTY and
	// launch an agent with nothing to do (ADR-0018).
	h, err := resolvePrompt(h)
	if err != nil {
		return nil, err
	}
	workdir := Workdir(h)
	// Resolve the argv before the environment and the PTY, for the same
	// reason: a harness spawn refuses (a `generic` carrying a prompt, SPEC-0017
	// REQ "Generic Kind Rejects Prompts"; a `command` harness with a malformed
	// argv or a prompt it cannot deliver, REQ-2, REQ-12; an argv template with
	// a required value this run lacks, REQ-11) fails here, having allocated
	// and exec'd nothing.
	plan, err := execArgv(h, workdir, run)
	if err != nil {
		return nil, err
	}
	name, args := plan.name, plan.args
	env, err := buildEnv(h, run)
	if err != nil {
		return nil, err
	}
	if plan.promptFile != "" {
		// Written after the render succeeded, so a skipped run leaves no
		// prompt file behind, and before exec, so the program can read it.
		if err := writePromptFile(plan.promptFile, plan.prompt); err != nil {
			return nil, fmt.Errorf("supervisor: harness %q: %w: write prompt file: %v", h.Name, ErrPromptDelivery, err)
		}
		// Last, so exec.Cmd.Env's later-wins rule makes it authoritative
		// over the daemon's environment and env_file alike.
		env = append(env, promptFileVar+"="+plan.promptFile)
	}

	if cols < 1 {
		cols = defaultPTYCols
	}
	if rows < 1 {
		rows = defaultPTYRows
	}
	pty, err := xpty.NewPty(cols, rows)
	if err != nil {
		return nil, fmt.Errorf("supervisor: allocate pty: %w", err)
	}

	cmd := exec.Command(name, args...)
	cmd.Dir = workdir
	cmd.Env = env
	// New session → child is a process-group leader (pgid == pid); a graceful
	// stop can signal the entire group (kill(-pid)) to reap child processes
	// like a shell's `sleep`.
	//
	// Setctty additionally makes the PTY the new session's *controlling*
	// terminal (Ctty 0 = the child's stdin, which xpty points at the slave).
	// Without it the child has no ctty, and the kernel has nobody to notify:
	// a TIOCSWINSZ changes the PTY's dimensions but raises no SIGWINCH, so a
	// full-screen app never learns it should re-lay-out and keeps painting its
	// old geometry into a correctly-sized window — the same symptom as spawning
	// at the wrong size, one layer down (ADR-0003). It is also what lets an
	// attached client's ^C reach the foreground process group as SIGINT.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}

	// A stdin delivery puts a pipe on fd 0 instead of the slave. xpty's Start
	// only fills the std streams left nil, so setting Stdin first keeps fd 1
	// and fd 2 on the slave, and Ctty names fd 1: Ctty is a descriptor in the
	// child, and fd 0 is no longer a terminal (SPEC-0017 REQ-12).
	var stdinR, stdinW *os.File
	if plan.delivery == core.PromptDeliveryStdin {
		stdinR, stdinW, err = os.Pipe()
		if err != nil {
			_ = pty.Close()
			return nil, fmt.Errorf("supervisor: harness %q: %w: stdin pipe: %v", h.Name, ErrPromptDelivery, err)
		}
		cmd.Stdin = stdinR
		cmd.SysProcAttr.Ctty = 1
	}

	if err := pty.Start(cmd); err != nil {
		_ = pty.Close()
		if stdinR != nil {
			_ = stdinR.Close()
			_ = stdinW.Close()
		}
		if errors.Is(err, syscall.E2BIG) {
			// The kernel refused the argv as too long — a prompt passed
			// through {{prompt}} is the usual way to get there. Name the
			// remedy; the error carries the executable, never the argv.
			return nil, fmt.Errorf("supervisor: harness %q: %w: the argv exceeds the platform's argument limits (%w); use prompt_delivery = \"file\" or \"stdin\" for a prompt this large", h.Name, ErrPromptDelivery, err)
		}
		return nil, fmt.Errorf("supervisor: start %q: %w", name, err)
	}
	// The child holds the slave now, so drop the daemon's copy. xpty keeps
	// one open for the PTY's lifetime, and while the daemon holds it a Linux
	// master never reads EOF: the reader could only be stopped by closing the
	// master under it, which throws away whatever the child wrote that the
	// reader had not read yet. On a loaded host that was a short run's whole
	// output. With the child's session the only holder, the master hands over
	// that tail and then EOF once the last holder exits, so the exit path can
	// wait for the reader instead of cutting it off (wait, supervisor.go).
	// Resize and input go through the master and are unaffected.
	if sl, ok := pty.(interface{ Slave() *os.File }); ok {
		_ = sl.Slave().Close()
	}
	proc := &process{pty: pty, cmd: cmd, pid: cmd.Process.Pid}
	if stdinR != nil {
		// Same reasoning as the slave: with the daemon's read end closed,
		// the child is the only reader, so a child that exits early turns
		// the feed's blocked write into EPIPE instead of a hang.
		_ = stdinR.Close()
		proc.stdin = startStdinFeed(stdinW, plan.prompt)
	}
	return proc, nil
}

// signalGroup sends sig to the child's process group, falling back to the
// single process if the group send fails.
func (p *process) signalGroup(sig syscall.Signal) {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-p.pid, sig); err != nil {
		_ = p.cmd.Process.Signal(sig)
	}
}
