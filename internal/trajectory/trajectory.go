// Package trajectory discovers and reads agent session transcripts through the
// adapter registry, enforcing the harvest opt-in (SPEC-0006 REQ "Harvest
// Opt-In") and read-only access (SPEC-0006 REQ "Trajectory Discovery").
//
// Governing: ADR-0011 (trajectory discovery is read-only), ADR-0007 (scrollback
// fallback), ADR-0008 (secrets — trajectory exposure is opt-in), SPEC-0006 REQ
// "Trajectory Discovery", SPEC-0006 REQ "Harvest Opt-In", SPEC-0006 REQ
// "Run Correlation" (which of a shared store's sessions a harness may be
// shown).
//
// The service never writes to, alters, or deletes a trajectory. Every code path
// is read-only by construction — there are no write methods on Service.
package trajectory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"charm.land/log/v2"
	"github.com/stump-wtf/harness/internal/adapter"
	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/runtrace"
)

// attributionTimeout bounds one List's correlation. The reads are local
// SQLite and JSONL, one store pass per run window, and a store locked by a
// wedged writer must not hold a caller open indefinitely — the same bound the
// structured logs view puts on the same work.
const attributionTimeout = 20 * time.Second

// RunRecord is what the host knows about one harness's runs: the windows it
// ran in, oldest first, and how far back that record reaches.
type RunRecord struct {
	// Runs are the harness's run windows. A native session is listed only
	// when it started inside one of them (SPEC-0006 REQ "Run Correlation"),
	// so no windows means nothing to list.
	Runs []runtrace.Window
	// KnownSince, when set, is how far back Runs is complete: everything
	// older could have been written by this harness with no record here. The
	// daemon's own record is complete, so it leaves this zero; a consumer
	// holding only each harness's latest run sets it to that run's start.
	// Peers use it to stay a possible author of their own history — see
	// runtrace.Scope.
	KnownSince time.Time
}

// RunWindows reports the run record for a harness name. The daemon wires it
// from the records `harness logs` already attributes with: the supervisor's
// snapshot plus each run the durable lifecycle log records. An error is not
// an empty record — for the queried harness it fails the call, and for a peer
// it makes that peer a possible author of every session, which is the
// conservative answer, since a peer can only ever hide a session.
type RunWindows func(name string) (RunRecord, error)

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

	// ErrUnknownSession is returned when Get is asked for a path that is not
	// one of the sessions List reports for that harness. The path may name
	// another harness's transcript, an opted-out harness's scrollback, or any
	// other file on the host; Get refuses all of them the same way.
	ErrUnknownSession = errors.New("not a trajectory session of this harness")
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
	// runWindows supplies each harness's run windows — the time half of the
	// correlation rule, which is what makes a machine-global store listable
	// per harness. When nil, no native session is attributable and List
	// reports none (SPEC-0006 REQ "Run Correlation").
	runWindows RunWindows
	// resolveEnv returns the discovery environment for a harness name, as the
	// harness's own process sees it (runtrace.DiscoveryEnvKeys). It is what
	// lets discovery follow an env_file that relocates a tool's store. When
	// nil, the defaults each tool uses apply.
	resolveEnv func(name string) map[string]string
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

// SetRunWindows wires the source of run-window knowledge for every harness.
// List attributes native sessions to these windows before returning them, so
// a harness sees only the sessions its own runs wrote, and Get — which serves
// only what List reports — inherits the same scope. Without it, nothing says
// which of a machine-global store's sessions belong to a harness, and the
// native listing is empty rather than machine-global (SPEC-0006 REQ "Run
// Correlation").
func (s *Service) SetRunWindows(fn RunWindows) {
	s.runWindows = fn
}

// SetDiscoveryEnv wires the function that returns a harness's discovery
// environment as its own process sees it, for the store-location half of the
// correlation rule. In production this comes from supervisor.DiscoveryEnv, so
// an env_file that relocates a tool's data directory is followed instead of
// the daemon's own environment being assumed.
func (s *Service) SetDiscoveryEnv(fn func(name string) map[string]string) {
	s.resolveEnv = fn
}

// List returns trajectory sessions for the named harness, or
// ErrHarvestDisabled when the harness has not opted in. Per SPEC-0006 REQ
// "Harvest Opt-In": a harness that has not enabled harvesting is omitted
// entirely from list results.
//
// When the harness's adapter reports a native trajectory directory, its
// sessions are attributed to the harness's own runs first (SPEC-0006 REQ "Run
// Correlation"): a session in the store that no run of this harness covers, or
// that another harness sharing the working directory could equally have
// written, is not reported. A command harness with `transcripts` lists the
// bound adapter's sessions (SPEC-0017 REQ-4). When the adapter reports no
// native trajectory (Generic, an unbound command harness, OMP), the scrollback
// log is reported as the single trajectory source (ADR-0007 fallback).
func (s *Service) List(cfg *core.Config, name string) ([]SessionSummary, error) {
	h, ok := cfg.Harnesses[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownHarness, name)
	}
	if !h.HarvestTrajectory {
		return nil, fmt.Errorf("%w: %s", ErrHarvestDisabled, name)
	}

	adp := s.registry.TrajectoryAdapter(h)

	// Native trajectory path.
	if adp.TailAdapter() != nil {
		sessions, err := s.attributeRuns(cfg, name, h)
		if err == nil {
			return sessions, nil
		}
		if !errors.Is(err, runtrace.ErrNoTrajectory) {
			return nil, err
		}
		// runtrace has no store for this kind, so the scrollback record below
		// is this harness's trajectory (ADR-0007) — the same answer the
		// structured logs view gives for a scope with no native transcript.
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

// attributeRuns returns the native sessions the correlation rule attributes to
// a run of this harness: every run window it has, correlated in turn, with the
// ambiguous ones dropped.
//
// The rule itself lives in runtrace and is not restated here; what this adds
// is which records supply the windows. Run windows are the harness's own — the
// supervisor's snapshot and the durable lifecycle log, wired by the daemon —
// so a session predating every recorded run is not reported: nothing ties it
// to this harness, and a store that has been on the machine longer than the
// config has is exactly where another user's work sits. A harness with no
// working directory is correlated in the daemon's own directory, the directory
// it was spawned in (the same answer `harness logs` gives).
func (s *Service) attributeRuns(cfg *core.Config, name string, h core.Harness) ([]SessionSummary, error) {
	target := runtrace.Scope{
		Name:    name,
		Adapter: h.TrajectoryKind(),
		Workdir: s.workdirFor(h, name),
		Args:    h.Args,
		Env:     s.discoveryEnv(name),
	}
	if target.Workdir == "" {
		target.Workdir, _ = os.Getwd()
	}
	if s.runWindows == nil {
		log.Info("trajectory listing has no run records, reporting no native sessions",
			"harness", name)
		return nil, nil
	}
	rec, err := s.runWindows(name)
	if err != nil {
		return nil, fmt.Errorf("trajectory runs %s: %w", name, err)
	}
	windows := coalesceWindows(rec.Runs)
	if len(windows) == 0 {
		return nil, nil
	}
	peers, err := s.peerScopes(cfg, target)
	if err != nil {
		return nil, err
	}

	// context.Background(), not a caller's context: nothing in this call chain
	// has one. List is reached from HandleListTrajectories, a facade tool
	// handler, and none of its callers thread a context either. The bounded
	// one below is what keeps a store locked by a wedged writer from holding
	// the call open; threading a caller's context properly means changing
	// List, the handler and every caller of both, which is its own change.
	//
	// @joestump-agent 08/23/2026 - Added for the agent-trace bump.
	ctx, cancel := context.WithTimeout(context.Background(), attributionTimeout)
	defer cancel()
	now := time.Now()

	out := make([]SessionSummary, 0, len(windows))
	seen := map[string]bool{}
	for _, w := range windows {
		att, err := runtrace.Attribute(ctx, target, w, peers, now)
		if err != nil {
			return nil, fmt.Errorf("trajectory attribute %s: %w", name, err)
		}
		// Ambiguity is dropped and written down, never scored: nothing in a
		// transcript identifies the process that wrote it, so a session two
		// harnesses could each have written belongs to neither.
		for _, x := range att.Excluded {
			log.Info("trajectory session excluded as ambiguous",
				"harness", name,
				"session", x.Session.Meta.ID,
				"started_at", x.Session.Started.Format(time.RFC3339Nano),
				"claimants", strings.Join(x.Claimants, ","))
		}
		for _, sess := range att.Sessions {
			// Two windows can reach one session through two sources under
			// different spellings of the same store; a session id is a UUID,
			// so identity is the tool plus the id.
			key := string(sess.Meta.Harness) + "/" + sess.Meta.ID
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, SessionSummary{
				ID:        sess.Meta.ID,
				Source:    "native",
				Path:      sess.Meta.Path,
				Cwd:       sess.Meta.Cwd,
				Model:     sess.Meta.Model,
				Title:     sess.Meta.Title,
				StartedAt: sess.Meta.StartedAt,
				EndedAt:   sess.Meta.EndedAt,
			})
		}
	}
	sortByStartedDesc(out)
	return out, nil
}

// peerScopes builds every other declared harness that could have written a
// session in target's workdir, with its run windows: the input that makes a
// session ambiguous. A peer can only ever hide a session, never claim one for
// the harness being queried, so its run record being unreadable is answered
// the conservative way — as running throughout.
func (s *Service) peerScopes(cfg *core.Config, target runtrace.Scope) ([]runtrace.Scope, error) {
	daemonDir, _ := os.Getwd()
	var peers []runtrace.Scope
	for _, pname := range harnessNames(cfg) {
		if pname == target.Name {
			continue
		}
		p, ok := cfg.Harnesses[pname]
		if !ok {
			continue
		}
		scope := runtrace.Scope{
			Name:    pname,
			Adapter: p.TrajectoryKind(),
			Workdir: s.workdirFor(p, pname),
			Args:    p.Args,
		}
		// A harness with no workdir is spawned in the daemon's own, and writes
		// its stores there like any other peer.
		if scope.Workdir == "" {
			scope.Workdir = daemonDir
		}
		if !runtrace.CouldWrite(scope.Adapter, target.Adapter) || !runtrace.SameDir(scope.Workdir, target.Workdir) {
			continue
		}
		if s.runWindows != nil {
			rec, err := s.runWindows(pname)
			if err != nil {
				log.Warn("trajectory peer run history unreadable, treating it as running throughout",
					"harness", pname, "err", err)
				scope.Runs = []runtrace.Window{{Start: time.Unix(1, 0)}}
				peers = append(peers, scope)
				continue
			}
			scope.Runs = rec.Runs
			scope.KnownSince = rec.KnownSince
		}
		peers = append(peers, scope)
	}
	return peers, nil
}

// harnessNames returns the config's harness names in a stable order, so one
// host's listing does not depend on Go's map iteration.
func harnessNames(cfg *core.Config) []string {
	if len(cfg.HarnessOrder) == len(cfg.Harnesses) {
		return cfg.HarnessOrder
	}
	names := make([]string, 0, len(cfg.Harnesses))
	for n := range cfg.Harnesses {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// discoveryEnv returns the harness's discovery environment, or nil when no
// resolver is wired, which leaves runtrace on each tool's defaults.
func (s *Service) discoveryEnv(name string) map[string]string {
	if s.resolveEnv == nil {
		return nil
	}
	return s.resolveEnv(name)
}

// coalesceWindows merges run windows whose slack-expanded bounds touch, so
// frequent runs cost one store pass instead of one each. It is exactly the
// coverage of the windows it merges: two windows that touch after
// runtrace.Slack is applied on both sides leave no instant between them that
// one of them did not cover. Windows with no start are dropped — an unknown
// run cannot vouch for a session.
func coalesceWindows(runs []runtrace.Window) []runtrace.Window {
	known := make([]runtrace.Window, 0, len(runs))
	for _, w := range runs {
		if !w.Start.IsZero() {
			known = append(known, w)
		}
	}
	sort.SliceStable(known, func(i, j int) bool { return known[i].Start.Before(known[j].Start) })
	var out []runtrace.Window
	for _, w := range known {
		if n := len(out); n > 0 {
			last := &out[n-1]
			if last.End.IsZero() {
				continue // open: it already covers everything after its start
			}
			if !w.Start.After(last.End.Add(2 * runtrace.Slack)) {
				if w.End.After(last.End) {
					last.End = w.End
				}
				continue
			}
		}
		out = append(out, w)
	}
	return out
}

// Get returns the full trajectory for the named harness at the given session
// path, or ErrHarvestDisabled when the harness has not opted in. The content
// is read once and returned as-is; no daemon code path writes to, alters, or
// deletes the underlying file (SPEC-0006 REQ "Trajectory Discovery").
//
// Get reads only a session that List reports for the same harness. The
// caller's path is a lookup key into that list, never a path to open, so Get
// inherits List's scoping: the harvest opt-in, which run windows attribute
// the session to this harness, and the scrollback fallback. A path List does
// not report, whether it is another harness's transcript, a session from a run
// this harness was not in, an opted-out harness's scrollback, or /etc/passwd,
// fails with ErrUnknownSession before any file is opened. Paths are compared
// after filepath.Clean, so a spelling of a listed path with "./" or a doubled
// separator still matches, while a "../" escape or a relative path does not.
//
// Governing: SPEC-0006 REQ "Harvest Opt-In", SPEC-0006 REQ "Trajectory
// Discovery", SPEC-0006 REQ "Run Correlation", ADR-0008 (trajectory exposure
// is opt-in).
//
// @joestump-agent 09/29/2026 - Scoped the read to List's sessions. Get used to
// os.ReadFile any caller-supplied path, so an opted-in harness's get_trajectory
// could read any file the daemon can (#89).
func (s *Service) Get(cfg *core.Config, name, sessionPath string) (*Trajectory, error) {
	sessions, err := s.List(cfg, name)
	if err != nil {
		return nil, err
	}
	want := filepath.Clean(sessionPath)
	var match *SessionSummary
	for i := range sessions {
		if filepath.Clean(sessions[i].Path) == want {
			match = &sessions[i]
			break
		}
	}
	if match == nil {
		return nil, fmt.Errorf("%w: %s: %q", ErrUnknownSession, name, sessionPath)
	}

	// Read the file content. This is the only filesystem read path, and it
	// is strictly os.Open → read → close. No write handle is ever taken.
	content, err := os.ReadFile(match.Path)
	if err != nil {
		return nil, fmt.Errorf("trajectory read %s: %w", name, err)
	}

	return &Trajectory{Session: *match, Content: string(content)}, nil
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
