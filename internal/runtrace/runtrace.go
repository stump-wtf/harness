// Run Correlation
//
// Attributes agent-trace sessions to one run of one harness, and flattens the
// attributed sessions into the time-ordered activity record `harness logs`
// renders. agent-trace's adapters are machine-global: a tool's store holds every
// session that tool ever wrote — from this harness, from sibling harnesses that
// share a working directory, and from the operator's own interactive runs. No
// transcript records the process that wrote it, so attribution is a heuristic
// over three signals (adapter identity, working directory, run time window) and
// it fails closed: a session more than one harness could have written belongs to
// none of them, and the exclusion is reported rather than hidden.
//
// Discovery is scoped to what the harness's own tool instance can see. A crush
// harness whose env_file sets CRUSH_GLOBAL_DATA keeps its registry under that
// directory, not under ~/.local/share/crush, and every crush instance keeps its
// sessions in <workdir>/.crush regardless of which registry it writes to — so
// both are consulted, and a corrupt or stale registry does not blind the lookup.
//
// Governing: ADR-0011 (agent adapters), SPEC-0006 REQ "Run Correlation",
// issue #89 (correlation rule), issue #302 (`harness logs` renders agent-trace).
//
// @joestump-agent 09/11/2026 - Added for harness#302 and harness#89.
//
// @joestump-agent 09/11/2026 - Review of #307: entry strings are redacted;
// claimant checks resolve symlinked workdirs and count a peer whose history is
// unknown at the session's start; a session reached through two sources is
// counted once.
package runtrace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/stump-wtf/agent-trace/classify"
	"github.com/stump-wtf/agent-trace/tail"

	"gitea.stump.rocks/stump.wtf/harness/internal/redact"
)

// Slack widens a run window at both ends. Crush stores whole seconds, so a
// session opened in the second its process spawned is recorded up to a second
// before the supervisor's nanosecond start time, and the daemon's lifecycle
// lines are second-truncated the same way. The tolerance covers that
// truncation; it is not a fudge factor for clock skew, because every timestamp
// involved comes from the same machine's clock.
const Slack = 2 * time.Second

var (
	// ErrNoTrajectory reports an adapter with no native transcript format
	// (generic, or a kind agent-trace does not parse). Callers fall back to the
	// durable log (ADR-0007).
	ErrNoTrajectory = errors.New("adapter records no native trajectory")

	// ErrNoWorkdir reports a harness with no configured workdir. It inherits
	// the daemon's working directory, which identifies no particular harness,
	// so nothing can be attributed to it.
	ErrNoWorkdir = errors.New("harness has no workdir to correlate sessions against")
)

// DiscoveryEnvKeys are the only environment keys correlation reads. Each one
// relocates a tool's store; none of them is a credential. The daemon resolves
// them from a harness's env_file layered over its own environment and never
// hands this package anything else from that file (ADR-0008).
var DiscoveryEnvKeys = []string{"HOME", "XDG_DATA_HOME", "CRUSH_GLOBAL_DATA", "CLAUDE_CONFIG_DIR", "CODEX_HOME"}

// Window is one run's time interval. End is zero while the run is in flight;
// an open window extends to the moment it is evaluated.
type Window struct {
	Start time.Time
	End   time.Time
}

// Open reports whether the run has no recorded end.
func (w Window) Open() bool { return w.End.IsZero() }

// Covers reports whether t falls inside the window, Slack included. A window
// with no start covers nothing: an unknown run cannot vouch for a session.
func (w Window) Covers(t, now time.Time) bool {
	if w.Start.IsZero() || t.IsZero() {
		return false
	}
	lo, hi := w.bounds(now)
	return !t.Before(lo) && !t.After(hi)
}

func (w Window) bounds(now time.Time) (time.Time, time.Time) {
	end := w.End
	if end.IsZero() {
		end = now
	}
	return w.Start.Add(-Slack), end.Add(Slack)
}

// Scope is everything correlation knows about one harness.
type Scope struct {
	// Name is the harness name, the identity attribution reports.
	Name string
	// Adapter is the harness kind ("crush", "claude-code", "codex",
	// "generic"). An empty or unrecognised value is treated as generic, the
	// same way adapter.Registry.Resolve treats it.
	Adapter string
	// Workdir is the resolved absolute working directory the process was
	// spawned in. Empty means none was configured.
	Workdir string
	// Args is the harness's configured argv, as the supervisor spawns it.
	// Correlation reads only the flags that relocate a tool's store — today
	// crush's --data-dir/-D — and nothing else from it.
	Args []string
	// Env holds DiscoveryEnvKeys as the harness's process sees them. Missing
	// keys fall back to the defaults each tool uses.
	Env map[string]string
	// Runs are the run windows known for this harness. For the harness being
	// queried they are unused — the window passed to Attribute is the run in
	// question; for peers they are what makes a session ambiguous.
	Runs []Window
	// KnownSince, when set, is how far back Runs is complete: the harness may
	// have run before it with no record here. A consumer holding only each
	// harness's latest run (the TUI, from `list`) sets it to that run's start,
	// and a session older than it counts this harness as a possible author.
	// Zero means Runs is all the history there is (the daemon's view, from the
	// durable log).
	KnownSince time.Time
}

// covers reports whether one of s's known runs covers t.
func (s Scope) covers(t, now time.Time) bool {
	for _, r := range s.Runs {
		if r.Covers(t, now) {
			return true
		}
	}
	return false
}

// mayHaveRun reports whether s could have been running at t: a known run
// covers t, or t predates KnownSince and so falls in history s does not hold.
func (s Scope) mayHaveRun(t, now time.Time) bool {
	return s.covers(t, now) || (!s.KnownSince.IsZero() && t.Before(s.KnownSince.Add(Slack)))
}

func (s Scope) home() string {
	if h := s.Env["HOME"]; h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return h
}

// Sources returns agent-trace adapters scoped to the stores this harness's tool
// instance can have written to, or ErrNoTrajectory for a kind with no native
// transcript.
//
// crush gets two: the project store at <workdir>/.crush/crush.db, which is
// where crush writes unless a config relocates data_directory, and the
// instance's own projects.json registry under CRUSH_GLOBAL_DATA (then
// XDG_DATA_HOME/crush, then ~/.local/share/crush — crush's own resolution
// order), which catches a relocated data_directory. The project store is
// consulted directly because the registry is not reliable on its own: crush
// rewrites it without a cross-process lock, and on a host running several
// instances it has been observed corrupt and frozen weeks stale, which made
// registry-only discovery report no sessions at all.
func Sources(s Scope) ([]tail.Adapter, error) {
	work := clean(s.Workdir)
	switch s.Adapter {
	case "crush":
		// A store named outright is the whole answer. It belongs to this
		// harness alone, so neither the shared project store nor the registry
		// may widen the search — both are reachable by every harness in the
		// workdir, which is exactly the ambiguity an explicit store resolves.
		if store := Store(s); store != "" {
			return []tail.Adapter{&tail.CrushAdapter{DBPath: filepath.Join(store, "crush.db"), Cwd: work}}, nil
		}
		var out []tail.Adapter
		if work != "" {
			db := filepath.Join(work, ".crush", "crush.db")
			if _, err := os.Stat(db); err == nil {
				out = append(out, &tail.CrushAdapter{DBPath: db, Cwd: work})
			}
		}
		out = append(out, &tail.CrushAdapter{ProjectsPath: filepath.Join(crushGlobalData(s), "projects.json")})
		return out, nil
	case "claude-code":
		dir := s.Env["CLAUDE_CONFIG_DIR"]
		if dir == "" {
			dir = filepath.Join(s.home(), ".claude")
		}
		return []tail.Adapter{&tail.ClaudeCodeAdapter{Dir: filepath.Join(dir, "projects")}}, nil
	case "codex":
		dir := s.Env["CODEX_HOME"]
		if dir == "" {
			dir = filepath.Join(s.home(), ".codex")
		}
		return []tail.Adapter{&tail.CodexAdapter{Dir: filepath.Join(dir, "sessions")}}, nil
	}
	return nil, fmt.Errorf("%w: %q", ErrNoTrajectory, s.Adapter)
}

// crushGlobalData mirrors crush's GlobalConfigData resolution, minus the
// Windows branch harness does not run on.
func crushGlobalData(s Scope) string {
	if d := s.Env["CRUSH_GLOBAL_DATA"]; d != "" {
		return d
	}
	if x := s.Env["XDG_DATA_HOME"]; x != "" {
		return filepath.Join(x, "crush")
	}
	return filepath.Join(s.home(), ".local", "share", "crush")
}

// Store is the session store a harness's tool instance was configured to
// write, or "" when nothing names one and discovery must infer it.
//
// This is what makes several harnesses sharing one working directory
// correlatable. Attribution's other signal is the working directory, and on a
// host where three crush harnesses all run in ~/src it rules nothing out: the
// sessions are genuinely indistinguishable, so every one of them is excluded.
// A store, when the config names one, identifies the harness by itself.
//
// crush resolves it as --data-dir/-D over options.data_directory over
// <workdir>/.crush, and the first two are the ones a harness can be configured
// with. The third is the inferred case this returns "" for.
//
// Governing: SPEC-0006 REQ "Run Correlation", issue #330.
func Store(s Scope) string {
	if s.Adapter != "crush" {
		return ""
	}
	work := clean(s.Workdir)
	if d := dataDirArg(s.Args); d != "" {
		return resolveStore(d, work)
	}
	if d := crushConfigDataDir(s, work); d != "" {
		return resolveStore(d, work)
	}
	return ""
}

// dataDirArg reads crush's --data-dir/-D out of an argv. crush parses flags
// with pflag, which takes a value as the following entry, joined with "=", or
// — for a shorthand — joined directly ("-D/path"), so all four spellings are
// read rather than only the one a config happens to use today.
func dataDirArg(args []string) string {
	for i, a := range args {
		switch {
		case a == "--data-dir", a == "-D":
			if i+1 < len(args) {
				return args[i+1]
			}
		case strings.HasPrefix(a, "--data-dir="):
			return strings.TrimPrefix(a, "--data-dir=")
		case strings.HasPrefix(a, "-D="):
			return strings.TrimPrefix(a, "-D=")
		case strings.HasPrefix(a, "-D") && len(a) > 2:
			return a[2:]
		}
	}
	return ""
}

// resolveStore applies crush's own rule for a configured data directory: an
// absolute path is used as-is, a relative one resolves against the working
// directory. {workdir} is expanded first, because the supervisor expands it in
// argv at spawn and the value correlation reads is the one crush was given.
func resolveStore(dir, work string) string {
	dir = strings.ReplaceAll(dir, "{workdir}", work)
	if filepath.IsAbs(dir) {
		return filepath.Clean(dir)
	}
	if work == "" {
		return ""
	}
	return filepath.Join(work, dir)
}

// crushConfigDataDir reads options.data_directory from the configs this crush
// instance loads: its global crush.json under CRUSH_GLOBAL_DATA, then the
// project-local file in the workdir, which overrides it. crush also walks up
// from the working directory looking for a project config; a harness spawns
// directly into its configured workdir, so the walk-up is not reproduced here
// — a store it would find is left to the inferred path.
func crushConfigDataDir(s Scope, work string) string {
	out := configDataDir(filepath.Join(crushGlobalData(s), "crush.json"))
	if work == "" {
		return out
	}
	// crush's own order, first match wins.
	for _, name := range []string{".crushrc", "crushrc", ".crush.json", "crush.json"} {
		if d := configDataDir(filepath.Join(work, name)); d != "" {
			return d
		}
	}
	return out
}

// configDataDir reads options.data_directory from one crush config file. A
// missing or malformed file is not an error: discovery falls through to the
// next source, the same way it survives a corrupt registry.
func configDataDir(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var cfg struct {
		Options struct {
			DataDirectory string `json:"data_directory"`
		} `json:"options"`
	}
	if json.Unmarshal(b, &cfg) != nil {
		return ""
	}
	return cfg.Options.DataDirectory
}

// storeOf recovers the store a discovered session came from. The crush adapter
// keys a session as "<database path>/<session id>", so the directory holding
// the database is the store — the same value Store reports for a harness
// configured with one.
func storeOf(meta tail.SessionMeta) string {
	if meta.Harness != tail.HarnessCrush || meta.Path == "" {
		return ""
	}
	return filepath.Dir(filepath.Dir(meta.Path))
}

// Session is one discovered session together with the adapter that can parse
// it.
type Session struct {
	Meta    tail.SessionMeta
	Started time.Time
	adapter tail.Adapter
}

// Exclusion is a session that fell inside the window but that more than one
// harness could have written.
type Exclusion struct {
	Session   Session
	Claimants []string
}

// Attribution is the outcome of correlating one run.
type Attribution struct {
	Harness  string
	Window   Window
	Sessions []Session
	Excluded []Exclusion
}

// Attribute finds the sessions that belong to target's run w.
//
// A session is attributed when all of these hold:
//
//  1. it came from one of target's Sources — the adapter identity matches and
//     the store is one this tool instance writes to;
//  2. its recorded working directory is exactly target's workdir (a
//     subdirectory is a different project to every agent tool here);
//  3. it started inside w, Slack included;
//  4. no other harness could have written it. A peer is a claimant when it
//     shares the workdir (symlinks resolved), runs the same kind (or runs
//     generic, which may be any tool), and one of its known runs covers the
//     session's start — or the start predates the peer's KnownSince, where
//     its history is unknown. Any claimant besides target moves the session
//     to Excluded, for everyone.
//
// A session with no parseable start time cannot satisfy (3) and is dropped.
// What (4) cannot catch is written down in SPEC-0006: a session started by hand
// in the same directory during the run is indistinguishable from the harness's
// own, because nothing in any transcript identifies the process.
func Attribute(ctx context.Context, target Scope, w Window, peers []Scope, now time.Time) (Attribution, error) {
	out := Attribution{Harness: target.Name, Window: w}
	sources, err := Sources(target)
	if err != nil {
		return out, err
	}
	work := clean(target.Workdir)
	if work == "" {
		return out, fmt.Errorf("%w: %s", ErrNoWorkdir, target.Name)
	}
	if w.Start.IsZero() {
		return out, nil
	}
	lo, hi := w.bounds(now)
	// Since is a pushdown, not the rule: it lets the SQLite stores skip rows
	// and the JSONL stores skip files by mtime. The exact checks run below.
	filter := tail.SessionFilter{Cwd: work, Since: lo}
	seen := map[string]bool{}
	// Resolved once: a store can cost a config read, and the claimant check
	// below runs for every peer of every session.
	targetStore := Store(target)
	peerStores := make([]string, len(peers))
	for i, p := range peers {
		peerStores[i] = Store(p)
	}
	for _, a := range sources {
		metas, err := tail.ListSessionsFiltered(ctx, a, filter)
		if err != nil {
			return out, fmt.Errorf("runtrace: list %s sessions for %s: %w", target.Adapter, target.Name, err)
		}
		for _, m := range metas {
			// Two sources can reach one store under different spellings — the
			// project store directly, and a registry entry through a symlink
			// — and the adapter keys a session by that spelling, which would
			// list it, and every event in it, twice. A session id is a UUID,
			// so identity is the tool plus the id.
			id := string(m.Harness) + "/" + m.ID
			if seen[id] || clean(m.Cwd) != work {
				continue
			}
			started, ok := m.Started()
			if !ok || started.Before(lo) || started.After(hi) {
				continue
			}
			seen[id] = true
			sess := Session{Meta: m, Started: started, adapter: a}
			if names := claimants(target, targetStore, peers, peerStores, work, started, now); len(names) > 1 {
				out.Excluded = append(out.Excluded, Exclusion{Session: sess, Claimants: names})
				continue
			}
			out.Sessions = append(out.Sessions, sess)
		}
	}
	sort.SliceStable(out.Sessions, func(i, j int) bool { return out.Sessions[i].Started.Before(out.Sessions[j].Started) })
	sort.SliceStable(out.Excluded, func(i, j int) bool {
		return out.Excluded[i].Session.Started.Before(out.Excluded[j].Session.Started)
	})
	return out, nil
}

// claimants returns every harness that could have written a session started
// at started in cwd — target first, then peers by name.
func claimants(target Scope, targetStore string, peers []Scope, peerStores []string, cwd string, started, now time.Time) []string {
	var names []string
	for i, p := range peers {
		if p.Name == target.Name || !CouldWrite(p.Adapter, target.Adapter) || !SameDir(p.Workdir, cwd) {
			continue
		}
		// Sharing a workdir is not sharing a store: two harnesses each told to
		// keep their sessions somewhere of their own cannot have written each
		// other's, so neither clouds the other's runs. Positive evidence only
		// — an inferred store is "" and stays a possible author, because a
		// registry can point anywhere.
		if targetStore != "" && peerStores[i] != "" && !SameDir(targetStore, peerStores[i]) {
			continue
		}
		if p.mayHaveRun(started, now) {
			names = append(names, p.Name)
		}
	}
	sort.Strings(names)
	return append([]string{target.Name}, names...)
}

// CouldWrite reports whether a harness of kind adapter could have produced a
// session of kind kind. A generic harness runs an arbitrary command — which
// may be that very tool — so it is always a possible author. Counting it can
// only hide a session, never misattribute one.
func CouldWrite(adapter, kind string) bool {
	switch adapter {
	case kind:
		return true
	case "crush", "claude-code", "codex":
		return false
	}
	return true
}

// Claimant returns the single harness a discovered session is attributable to,
// using the same rule as Attribute against each scope's Runs and KnownSince. It
// is the form a consumer holding many sessions and many harnesses needs — the
// TUI's chatroom labels every session its machine-wide watcher discovers — and
// it reports ok=false for a session with no claimant, with several, whose only
// claimant runs generic, or that a harness whose known history does not reach
// back to it could also have written.
//
// That last case is what KnownSince is for. From latest runs alone, a peer
// that restarted after the session began looks idle when it began, and the
// session is credited to whichever sibling was running then — one agent's
// work under another's name.
func Claimant(meta tail.SessionMeta, scopes []Scope, now time.Time) (string, bool) {
	started, ok := meta.Started()
	cwd := clean(meta.Cwd)
	if !ok || cwd == "" {
		return "", false
	}
	kind := string(meta.Harness)
	var found []Scope
	for _, s := range scopes {
		if CouldWrite(s.Adapter, kind) && SameDir(s.Workdir, cwd) && s.mayHaveRun(started, now) {
			found = append(found, s)
		}
	}
	// The session says which store it came from, and a configured store names
	// one harness. Drop only those whose own store is demonstrably a different
	// directory — a harness with an inferred store stays a candidate, because
	// its registry could point anywhere.
	if store := storeOf(meta); store != "" {
		var owners []Scope
		for _, s := range found {
			if st := Store(s); st == "" || SameDir(st, store) {
				owners = append(owners, s)
			}
		}
		if len(owners) > 0 {
			found = owners
		}
	}
	if len(found) != 1 || found[0].Adapter != kind || !found[0].covers(started, now) {
		return "", false
	}
	return found[0].Name, true
}

// EntryKind classifies one activity entry.
type EntryKind string

const (
	// KindSession marks the start of an attributed session.
	KindSession EntryKind = "session"
	// KindTool is a classified tool call (search, read, edit, exec, verify,
	// other).
	KindTool EntryKind = "tool"
	// KindMark is a non-tool annotation: a user message, a compaction, a
	// subagent launch, or an agent error.
	KindMark EntryKind = "mark"
)

// Entry is one line of a run's agent activity.
type Entry struct {
	// ID is stable across repeated reads of the same session, so a follower
	// can print only what it has not printed yet.
	ID      string
	Time    time.Time
	Session string
	Seq     int
	Kind    EntryKind
	// Action is the classify action for a tool, or the mark type for a mark
	// ("user-message", "compaction", "error", …).
	Action  string
	Tool    string
	Target  string
	Summary string
	Error   bool
	// Ambiguous marks an entry from an excluded session, present only when
	// the caller explicitly asked for them.
	Ambiguous bool
}

// Events parses every attributed session — and, when includeExcluded is set,
// every excluded one, flagged Ambiguous — and returns their activity inside the
// run window in time order. A session that fails to parse does not hide the
// others; its error is returned alongside what did parse.
func Events(ctx context.Context, a Attribution, includeExcluded bool, now time.Time) ([]Entry, []error) {
	var out []Entry
	var errs []error
	add := func(s Session, ambiguous bool) {
		entries, err := sessionEntries(ctx, s, a.Window, now)
		if err != nil {
			errs = append(errs, err)
			return
		}
		for i := range entries {
			entries[i].Ambiguous = ambiguous
		}
		out = append(out, entries...)
	}
	for _, s := range a.Sessions {
		add(s, false)
	}
	if includeExcluded {
		for _, x := range a.Excluded {
			add(x.Session, true)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Time.Equal(out[j].Time) {
			return out[i].Time.Before(out[j].Time)
		}
		if out[i].Session != out[j].Session {
			return out[i].Session < out[j].Session
		}
		if out[i].Seq != out[j].Seq {
			return out[i].Seq < out[j].Seq
		}
		// A mark carries the seq of the tool call that follows it, so at a tie
		// the mark reads first.
		return kindRank(out[i].Kind) < kindRank(out[j].Kind)
	})
	return out, errs
}

func kindRank(k EntryKind) int {
	switch k {
	case KindSession:
		return 0
	case KindMark:
		return 1
	}
	return 2
}

// sessionEntries flattens one session into entries. Every string an entry
// carries came from the transcript, which records commands verbatim — a
// `curl -H "Authorization: …"` or a token-bearing git remote included — so
// each passes through redact before it can reach `harness logs` or its --json
// (ADR-0008).
func sessionEntries(ctx context.Context, s Session, w Window, now time.Time) ([]Entry, error) {
	events, marks, meta, err := s.adapter.Parse(ctx, s.Meta.Path)
	if err != nil {
		return nil, fmt.Errorf("runtrace: parse %s session %s: %w", s.Meta.Harness, s.Meta.ID, err)
	}
	lo, hi := w.bounds(now)
	in := func(t time.Time) bool { return !t.Before(lo) && !t.After(hi) }
	id := s.Meta.ID
	out := []Entry{{
		ID:      id + "/session",
		Time:    s.Started,
		Session: id,
		Seq:     -1,
		Kind:    KindSession,
		Action:  string(KindSession),
		Summary: redact.String(sessionSummary(s.Meta, meta)),
	}}
	for _, ev := range events {
		t := stamp(ev.Timestamp, s.Started)
		if !in(t) {
			continue
		}
		out = append(out, Entry{
			ID:      fmt.Sprintf("%s/tool/%d", id, ev.Seq),
			Time:    t,
			Session: id,
			Seq:     ev.Seq,
			Kind:    KindTool,
			Action:  ev.Action,
			Tool:    ev.Tool,
			Target:  redact.String(primaryTarget(ev)),
			Summary: redact.String(tallySuffix.ReplaceAllString(ev.Summary, "")),
			Error:   ev.IsError,
		})
	}
	for i, mk := range marks {
		t := stamp(mk.Timestamp, s.Started)
		if !in(t) {
			continue
		}
		out = append(out, Entry{
			ID:      fmt.Sprintf("%s/mark/%d", id, i),
			Time:    t,
			Session: id,
			Seq:     mk.Seq,
			Kind:    KindMark,
			Action:  mk.Type,
			Summary: redact.String(mk.Note),
			Error:   mk.Type == "error",
		})
	}
	return out, nil
}

// sessionSummary names a session the way an operator scans for it: short id,
// then model and title when the transcript recorded them.
func sessionSummary(listed, parsed tail.SessionMeta) string {
	id := listed.ID
	if len(id) > 8 {
		id = id[:8]
	}
	parts := []string{id, string(listed.Harness)}
	if parsed.Model != "" {
		parts = append(parts, parsed.Model)
	}
	if listed.Title != "" {
		parts = append(parts, listed.Title)
	}
	s := parts[0]
	for _, p := range parts[1:] {
		s += " · " + p
	}
	return s
}

// tallySuffix is the classification tally classify.SummarizeTool appends to a
// summary ("view -> 0 targets, 1 outside", plus " error" for a failed call).
// It describes how the call was classified, not what the agent did; an operator
// reading a run needs the command or the path in that space, and failure is
// already carried by Entry.Error.
var tallySuffix = regexp.MustCompile(` -> \d+ targets, \d+ outside( error)?$`)

// primaryTarget is the most deeply touched in-repo target (edit > read > hit,
// first on a tie), else the first file the call touched outside the workdir.
// A sweep that runs in a scratch directory and reads its prompt from
// ~/.config does all of its reading "outside", and without the fallback every
// one of those reads rendered as a bare tool name.
func primaryTarget(ev classify.Event) string {
	best, rank := "", 0
	for _, t := range ev.Targets {
		if r := classify.RankTouch(t.Touch); r > rank {
			best, rank = t.Path, r
		}
	}
	if best == "" && len(ev.Outside) > 0 {
		best = ev.Outside[0].Path
	}
	return best
}

// stamp parses an RFC 3339 timestamp, falling back to when the session
// started. An adapter that does not date an event still dated its session.
func stamp(ts string, fallback time.Time) time.Time {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, ts); err == nil {
			return t
		}
	}
	return fallback
}

// SameDir reports whether a and b name one directory: equal once cleaned, or
// the same place once symlinks are resolved. Harnesses that spell a shared
// workdir differently still share its stores, and the project-store source
// stamps every session with the target's own spelling — so a claimant check
// comparing spellings would miss the peer and credit its sessions to the
// target.
func SameDir(a, b string) bool {
	a, b = clean(a), clean(b)
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

// clean normalizes a directory for comparison, keeping "" empty rather than
// letting filepath.Clean turn it into ".".
func clean(p string) string {
	if p == "" {
		return ""
	}
	return filepath.Clean(p)
}
