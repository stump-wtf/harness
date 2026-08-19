package supervisor

// Governing: ADR-0007 (State & scrollback ownership) — runtime state is a small
// JSON file at $XDG_STATE_HOME/harness/state.json holding enabled/disabled per
// harness, last exit code + restart count + flapping status, and timestamps;
// written on transitions (debounced), read on daemon start to restore the
// world (ADR-0005). Config is NOT duplicated here — the TOML is its source of
// truth (ADR-0006). Clean split: TOML = intent authoring, state.json = what
// actually happened. Secrets never land here (ADR-0008).

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
)

// stateSchemaVersion is bumped on incompatible state.json changes so a future
// daemon can migrate rather than misread.
const stateSchemaVersion = 1

// persistedState is the on-disk runtime state document.
type persistedState struct {
	Version       int                         `json:"version"`
	ActiveProfile string                      `json:"active_profile,omitempty"`
	Harnesses     map[string]persistedHarness `json:"harnesses"`
	// Projects holds the registered project definitions (SPEC-0004 REQ
	// "Registration Persistence"): a project brought up with project_up
	// stays registered across daemon restarts until project_down or remove
	// tears it down, Compose-style. Keyed by project name; the slice
	// preserves project-file order.
	Projects map[string]persistedProject `json:"projects,omitempty"`
}

// persistedProject is one registered project's definition set, in order.
type persistedProject struct {
	Harnesses []persistedProjectHarness `json:"harnesses"`
}

// persistedProjectHarness is the persisted form of one project harness
// definition — exactly the fields a project_up wire payload can carry, plus
// the tmux socket. Runtime state (enabled intent, counters) lives in the
// shared Harnesses map under the fully-qualified name, so project and
// global harnesses restore through the same path.
type persistedProjectHarness struct {
	Name           string   `json:"name"`
	Harness        string   `json:"harness,omitempty"`
	Args           []string `json:"args,omitempty"`
	Prompt         string   `json:"prompt,omitempty"`
	Model          string   `json:"model,omitempty"`
	AutoAccept     bool     `json:"auto_accept,omitempty"`
	MaxTurns       int      `json:"max_turns,omitempty"`
	Quiet          *bool    `json:"quiet,omitempty"`
	Workdir        string   `json:"workdir,omitempty"`
	EnvFile        string   `json:"env_file,omitempty"`
	RestartDelayMs int64    `json:"restart_delay_ms,omitempty"`
	Restart        string   `json:"restart,omitempty"`
	Backend        string   `json:"backend,omitempty"`
	Description    string   `json:"description,omitempty"`
	Enabled        bool     `json:"enabled,omitempty"`
	TmuxSocket     string   `json:"tmux_socket,omitempty"`
}

// toPersistedProject captures a project record's definitions in persisted
// form. h is the namespaced core.Harness exactly as registered.
func toPersistedProjectHarness(h core.Harness) persistedProjectHarness {
	var quiet *bool
	if h.Prompt != "" {
		// Only meaningful for a prompt harness; nil keeps a bare cmd harness's
		// state file free of the headless one-shot default.
		q := h.Quiet
		quiet = &q
	}
	return persistedProjectHarness{
		Name:           h.Name,
		Harness:        h.Adapter,
		Args:           h.Args,
		Prompt:         h.Prompt,
		Model:          h.Model,
		AutoAccept:     h.AutoAccept,
		MaxTurns:       h.MaxTurns,
		Quiet:          quiet,
		Workdir:        h.Workdir,
		EnvFile:        h.EnvFile,
		RestartDelayMs: h.RestartDelay.Milliseconds(),
		Restart:        string(h.Restart),
		Backend:        string(h.Backend),
		Description:    h.Description,
		Enabled:        h.Enabled,
		TmuxSocket:     h.TmuxSocket,
	}
}

// toCore reverses toPersistedProjectHarness. Defaults mirror the wire
// mapping in the daemon (harnessFromWire): absent backend/restart fall back
// to the config-parser defaults rather than the empty string.
func (p persistedProjectHarness) toCore() core.Harness {
	backend := core.Backend(p.Backend)
	if p.Backend == "" {
		backend = core.BackendNative
	}
	restart := core.RestartPolicy(p.Restart)
	if p.Restart == "" {
		restart = core.RestartAlways
		if p.Prompt != "" {
			restart = core.RestartNo
		}
	}
	quiet := true
	if p.Quiet != nil {
		quiet = *p.Quiet
	}
	return core.Harness{
		Name:         p.Name,
		Adapter:      p.Harness,
		Args:         p.Args,
		Prompt:       p.Prompt,
		Model:        p.Model,
		AutoAccept:   p.AutoAccept,
		MaxTurns:     p.MaxTurns,
		Quiet:        quiet,
		Workdir:      p.Workdir,
		EnvFile:      p.EnvFile,
		RestartDelay: time.Duration(p.RestartDelayMs) * time.Millisecond,
		Restart:      restart,
		Backend:      backend,
		Description:  p.Description,
		Enabled:      p.Enabled,
		TmuxSocket:   p.TmuxSocket,
	}
}

// persistedHarness is one harness's durable runtime state (ADR-0007). No config
// fields — those live in the TOML.
type persistedHarness struct {
	Enabled      bool       `json:"enabled"` // intent (SPEC-0003 REQ "State Model")
	State        core.State `json:"state"`   // last observed state (informational)
	RestartCount int        `json:"restart_count"`
	LastExitCode int        `json:"last_exit_code"`
	LastExitAt   *time.Time `json:"last_exit_at,omitempty"`
	Flapping     bool       `json:"flapping"`
	Created      time.Time  `json:"created,omitempty"`
	LastStarted  *time.Time `json:"last_started,omitempty"`
}

// StateHome returns $XDG_STATE_HOME/harness (falling back to ~/.local/state).
func StateHome() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return filepath.Join(".", "harness")
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "harness")
}

// DefaultStatePath is $XDG_STATE_HOME/harness/state.json.
func DefaultStatePath() string { return filepath.Join(StateHome(), "state.json") }

// DefaultLogDir is $XDG_STATE_HOME/harness/logs.
func DefaultLogDir() string { return filepath.Join(StateHome(), "logs") }

// loadState reads and parses state.json. A missing file yields an empty (not
// error) state so first boot works; a malformed file is reported.
func loadState(path string) (persistedState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return persistedState{Version: stateSchemaVersion, Harnesses: map[string]persistedHarness{}}, nil
		}
		return persistedState{}, fmt.Errorf("supervisor: read state.json: %w", err)
	}
	var ps persistedState
	if err := json.Unmarshal(data, &ps); err != nil {
		return persistedState{}, fmt.Errorf("supervisor: parse state.json: %w", err)
	}
	if ps.Harnesses == nil {
		ps.Harnesses = map[string]persistedHarness{}
	}
	return ps, nil
}

// saveState atomically writes ps to path (write tmp + rename, ADR-0006/0007
// atomic-write discipline).
func saveState(path string, ps persistedState) error {
	ps.Version = stateSchemaVersion
	data, err := json.MarshalIndent(ps, "", "  ")
	if err != nil {
		return fmt.Errorf("supervisor: marshal state.json: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("supervisor: create state dir: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".state-*.json.tmp")
	if err != nil {
		return fmt.Errorf("supervisor: temp state.json: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("supervisor: write state.json: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("supervisor: close state.json: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("supervisor: rename state.json: %w", err)
	}
	return nil
}
