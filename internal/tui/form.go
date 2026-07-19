package tui

// Governing: SPEC-0001 REQ "Harness Form" — n/e open a Huh form over the harness
// schema (cmd/args/workdir/env_file/restart_delay/backend/description/profile
// membership) that writes back to harnessd.toml (ADR-0006: file is truth); e
// pre-fills from the existing harness; then the daemon reloads and the harness
// appears on the dashboard. This file owns the schema<->TOML serialization; the
// Huh widget wiring lives in overlays.go.

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
)

// HarnessForm is the editable harness schema behind the n/e Huh form. It is the
// TUI-facing projection of a core.Harness table; RestartDelay is seconds to
// match the TOML unit (config.rawHarness.RestartDelay).
type HarnessForm struct {
	Name         string
	Cmd          string
	Args         []string
	Workdir      string
	EnvFile      string
	RestartDelay int // seconds
	Backend      string
	Description  string
	Enabled      bool
}

// NewHarnessForm is a blank form for `n` with sane defaults (native backend).
func NewHarnessForm() HarnessForm {
	return HarnessForm{Backend: string(core.BackendNative)}
}

// Validate checks the minimum the daemon config parser requires (a name and a
// cmd, a known backend, non-negative delay) so the form catches errors before
// writing TOML the daemon would reject on reload.
func (f HarnessForm) Validate() error {
	if strings.TrimSpace(f.Name) == "" {
		return fmt.Errorf("name is required")
	}
	if strings.TrimSpace(f.Cmd) == "" {
		return fmt.Errorf("cmd is required")
	}
	if f.Backend != "" && !core.Backend(f.Backend).Valid() {
		return fmt.Errorf("backend must be native or tmux")
	}
	if f.RestartDelay < 0 {
		return fmt.Errorf("restart_delay must not be negative")
	}
	return nil
}

// TOML renders the form as a `[harness.<name>]` table. Only set fields are
// emitted so the file stays clean. The output re-parses through config.Parse
// into an equivalent harness (round-trip guarantee, tested).
func (f HarnessForm) TOML() string {
	var b strings.Builder
	fmt.Fprintf(&b, "[harness.%s]\n", f.Name)
	fmt.Fprintf(&b, "cmd = %s\n", strconv.Quote(f.Cmd))
	if len(f.Args) > 0 {
		parts := make([]string, len(f.Args))
		for i, a := range f.Args {
			parts[i] = strconv.Quote(a)
		}
		fmt.Fprintf(&b, "args = [%s]\n", strings.Join(parts, ", "))
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

// AppendHarness appends a new harness table to an existing harnessd.toml body,
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

// toForm converts the Huh string-bound inputs into a typed HarnessForm, parsing
// space-separated args and the integer restart_delay.
func (fi formInputs) toForm() HarnessForm {
	f := HarnessForm{
		Name:        strings.TrimSpace(fi.name),
		Cmd:         strings.TrimSpace(fi.cmd),
		Workdir:     strings.TrimSpace(fi.workdir),
		EnvFile:     strings.TrimSpace(fi.envFile),
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
