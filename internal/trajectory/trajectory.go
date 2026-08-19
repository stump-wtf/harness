// Package trajectory discovers and reads agent session transcripts through the
// adapter registry, enforcing the harvest opt-in (SPEC-0006 REQ "Harvest
// Opt-In") and read-only access (SPEC-0006 REQ "Trajectory Discovery").
//
// Governing: ADR-0011 (trajectory discovery is read-only), ADR-0007 (scrollback
// fallback), ADR-0008 (secrets — trajectory exposure is opt-in), SPEC-0006 REQ
// "Trajectory Discovery", SPEC-0006 REQ "Harvest Opt-In".
//
// The service never writes to, alters, or deletes a trajectory. Every code path
// is read-only by construction — there are no write methods on Service.
package trajectory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/adapter"
	"gitea.stump.rocks/stump.wtf/harness/internal/core"
	"github.com/stump-wtf/agent-trace/tail"
)

// Sentinel errors for domain-specific failure modes callers need to
// distinguish programmatically (SPEC-0006 REQ "Error Handling Standards").
var (
	// ErrHarvestDisabled is returned when a harness has not opted in to
	// trajectory harvesting. SPEC-0006 REQ "Harvest Opt-In": "a harness
	// that has not opted in MUST NOT have its trajectory listed or returned
	// by any facade operation."
	ErrHarvestDisabled = errors.New("trajectory harvesting is not enabled for this harness")

	// ErrUnknownHarness is returned when a harness name does not resolve to
	// a registered harness in the config.
	ErrUnknownHarness = errors.New("unknown harness")
)

// SessionSummary is the read-only metadata for a discovered trajectory session.
// It carries no transcript content — call Get to retrieve the full body.
type SessionSummary struct {
	// ID is the agent-level session identifier (display only).
	ID string `json:"id"`
	// Source is "native" for a tool's own transcript, or "scrollback" for
	// the ADR-0007 fallback.
	Source string `json:"source"`
	// Path is the filesystem path to the transcript file.
	Path string `json:"path"`
	// Cwd is the session's working directory, when known.
	Cwd string `json:"cwd,omitempty"`
	// Model is the model identifier, when known.
	Model string `json:"model,omitempty"`
	// Title is a human-readable session title, when known.
	Title string `json:"title,omitempty"`
	// StartedAt is the session start time (RFC 3339), when known.
	StartedAt string `json:"startedAt,omitempty"`
	// EndedAt is the session end time (RFC 3339), when known.
	EndedAt string `json:"endedAt,omitempty"`
}

// Trajectory is the full read-only body of a single trajectory session. The
// daemon never writes to, alters, or deletes the underlying file.
type Trajectory struct {
	Session SessionSummary `json:"session"`
	// Content is the raw transcript content (JSONL for native, ANSI/text for
	// scrollback). It is read once and returned as-is; no transformation is
	// applied.
	Content string `json:"content"`
}

// Service discovers and reads trajectories for harnesses. It is constructed
// with an adapter registry and is stateless between calls — every List/Get
// reads the current config and filesystem state, so a config reload that
// enables harvesting takes effect immediately without restarting the harness
// (SPEC-0006 REQ "Harvest Opt-In" scenario "Opt-in exposes the trajectory").
type Service struct {
	registry *adapter.Registry
	// resolveWorkdir returns the absolute working directory for a harness
	// name. In production this comes from the supervisor/manager; in tests it
	// can be stubbed. When nil, the harness's config workdir is used directly.
	resolveWorkdir func(name string) string
	// scrollbackDir is the directory where the daemon stores per-harness
	// scrollback logs. Wired by the daemon; tests set it via SetScrollbackDir.
	scrollbackDir string
}

// NewService creates a trajectory Service backed by the given adapter registry.
func NewService(registry *adapter.Registry) *Service {
	return &Service{registry: registry}
}

// SetWorkdirResolver wires the function that resolves a harness name to its
// runtime working directory. This is used to filter native transcript sessions
// by their Cwd — a harness running in /home/user/project only sees sessions
// whose Cwd matches.
func (s *Service) SetWorkdirResolver(fn func(name string) string) {
	s.resolveWorkdir = fn
}

// SetScrollbackDir sets the directory where scrollback log files are found,
// for daemon wiring and tests. The scrollback fallback (ADR-0007) reports a
// single trajectory at <dir>/<name>.log.
func (s *Service) SetScrollbackDir(dir string) {
	s.scrollbackDir = dir
}

// List returns trajectory sessions for the named harness, or
// ErrHarvestDisabled when the harness has not opted in. Per SPEC-0006 REQ
// "Harvest Opt-In": a harness that has not enabled harvesting is omitted
// entirely from list results.
//
// When the harness's adapter reports a native trajectory directory, sessions
// are enumerated from there (filtered by the harness's workdir when available).
// When the adapter reports no native trajectory (Generic), the scrollback log
// is reported as the single trajectory source (ADR-0007 fallback).
func (s *Service) List(cfg *core.Config, name string) ([]SessionSummary, error) {
	h, ok := cfg.Harnesses[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownHarness, name)
	}
	if !h.HarvestTrajectory {
		return nil, fmt.Errorf("%w: %s", ErrHarvestDisabled, name)
	}

	adp := s.registry.Resolve(h)

	// Native trajectory path.
	td := adp.TailAdapter()
	if td != nil {
		workdir := s.workdirFor(h, name)
		filter := tail.SessionFilter{}
		if workdir != "" {
			filter.Cwd = workdir
		}
		sessions, err := tail.ListSessionsFiltered(td, filter)
		if err != nil {
			return nil, fmt.Errorf("trajectory list %s: %w", name, err)
		}
		out := make([]SessionSummary, 0, len(sessions))
		for _, sm := range sessions {
			out = append(out, SessionSummary{
				ID:        sm.ID,
				Source:    "native",
				Path:      sm.Path,
				Cwd:       sm.Cwd,
				Model:     sm.Model,
				Title:     sm.Title,
				StartedAt: sm.StartedAt,
				EndedAt:   sm.EndedAt,
			})
		}
		sortByStartedDesc(out)
		return out, nil
	}

	// Scrollback fallback (ADR-0007).
	scrollbackPath := s.scrollbackPath(name)
	if scrollbackPath != "" {
		if _, err := os.Stat(scrollbackPath); err == nil {
			return []SessionSummary{{
				ID:     name,
				Source: "scrollback",
				Path:   scrollbackPath,
			}}, nil
		}
	}

	return nil, nil
}

// Get returns the full trajectory for the named harness at the given session
// path, or ErrHarvestDisabled when the harness has not opted in. The content
// is read once and returned as-is; no daemon code path writes to, alters, or
// deletes the underlying file (SPEC-0006 REQ "Trajectory Discovery").
func (s *Service) Get(cfg *core.Config, name, sessionPath string) (*Trajectory, error) {
	h, ok := cfg.Harnesses[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownHarness, name)
	}
	if !h.HarvestTrajectory {
		return nil, fmt.Errorf("%w: %s", ErrHarvestDisabled, name)
	}

	// Read the file content. This is the only filesystem read path, and it
	// is strictly os.Open → read → close. No write handle is ever taken.
	content, err := os.ReadFile(sessionPath)
	if err != nil {
		return nil, fmt.Errorf("trajectory read %s: %w", name, err)
	}

	adp := s.registry.Resolve(h)
	source := "scrollback"
	if adp != nil && adp.TailAdapter() != nil {
		source = "native"
	}

	return &Trajectory{
		Session: SessionSummary{
			ID:     name,
			Source: source,
			Path:   sessionPath,
		},
		Content: string(content),
	}, nil
}

// workdirFor resolves the runtime working directory for a harness, preferring
// the runtime resolver when wired, then the config workdir.
func (s *Service) workdirFor(h core.Harness, name string) string {
	if s.resolveWorkdir != nil {
		if wd := s.resolveWorkdir(name); wd != "" {
			return wd
		}
	}
	return h.Workdir
}

// scrollbackPath returns the path to the harness's scrollback log file:
// <scrollbackDir>/<name>.log.
func (s *Service) scrollbackPath(name string) string {
	if s.scrollbackDir == "" {
		return ""
	}
	return filepath.Join(s.scrollbackDir, name+".log")
}

// sortByStartedDesc sorts sessions newest-first by StartedAt. Sessions with
// missing or unparseable timestamps sort last.
func sortByStartedDesc(sessions []SessionSummary) {
	sort.SliceStable(sessions, func(i, j int) bool {
		ti, oki := parseRFC3339(sessions[i].StartedAt)
		tj, okj := parseRFC3339(sessions[j].StartedAt)
		if !oki && !okj {
			return false
		}
		if !oki {
			return false
		}
		if !okj {
			return true
		}
		return ti.After(tj)
	})
}

// parseRFC3339 parses an RFC 3339 timestamp, returning the time and true on
// success, or zero and false when the string is empty or unparseable.
func parseRFC3339(ts string) (time.Time, bool) {
	if ts == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		t, err = time.Parse(time.RFC3339, ts)
		if err != nil {
			return time.Time{}, false
		}
	}
	return t, true
}
