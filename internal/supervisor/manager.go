package supervisor

// Governing: ADR-0005 (the daemon supervises harnesses in-process; on start it
// restores the intended running set); ADR-0006 (config is the source of truth,
// hot-reloaded; a parse error keeps the last-good config; changes to a running
// harness apply on next restart); ADR-0007 (persisted runtime state + rotating
// logs); SPEC-0003 (autostart, config-change application, lifecycle events).
//
// Manager is the daemon-facing façade over a set of per-harness Supervisors: it
// owns the event Bus, the state.json persistence, and the config-reload path.

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"time"

	"charm.land/log/v2"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/ledger"
	"github.com/stump-wtf/harness/internal/runtrace"
	"github.com/stump-wtf/harness/internal/sealedlog"
)

// persistDebounce is how long the manager coalesces state-change writes before
// flushing state.json (ADR-0007: "written on transitions (debounced)").
const persistDebounce = 50 * time.Millisecond

// ManagerOptions configure a Manager.
type ManagerOptions struct {
	// Policy governs restart/backoff/stop for every harness.
	Policy Policy
	// StatePath is the state.json location (default DefaultStatePath()).
	StatePath string
	// LogDir is the per-harness log directory (default DefaultLogDir()).
	LogDir string
	// LogCfg tunes rotation (Dir is overridden by LogDir).
	LogCfg LogConfig
	// ExtraOutFor, if set, returns an additional io.Writer that each harness's
	// raw PTY output is teed to alongside its durable log (ADR-0003/ADR-0007).
	// The daemon uses this to feed the per-harness x/vt emulator + scrollback
	// ring that backs live attach (SPEC-0002 REQ "Attach Session"). Returning
	// nil for a name disables the tee for that harness. The reviewer flagged the
	// absence of this Manager-level ExtraOut wiring in the prior package; this
	// closes it.
	ExtraOutFor func(name string) io.Writer
	// DropExtraOut, if set, releases whatever ExtraOutFor allocated for a
	// harness name once that harness is deregistered (project_down, or a re-up
	// that removed it). The daemon wires this to the attach Registry so an
	// up→down→up cycle frees the vt emulator + scrollback ring instead of
	// leaking it and resurfacing the dead incarnation's screen (SPEC-0004 REQ
	// "Tear Down"; ADR-0009).
	DropExtraOut func(name string)
	// SizeFor, if set, reports the viewport a harness's freshly spawned PTY
	// should be born at — the attach layer's authoritative
	// smallest-attached-wins size for that name. The daemon wires this to the
	// attach Registry so a harness (re)started while a client is attached comes
	// up at the client's size instead of 80×24 (ADR-0003).
	SizeFor func(name string) (cols, rows int)
	// JobsDir is the root of per-run logs, <JobsDir>/<name>/<run_id>.log
	// (SPEC-0008 REQ "Per-Run Logs"). Defaults to a "jobs" directory beside
	// the log directory — $XDG_STATE_HOME/harness/jobs in production.
	JobsDir string
	// CompressLogs compresses sealed logs in the background: each rotated
	// backup of a durable log, and each closed run's log and raw stream
	// (ADR-0007 as amended; SPEC-0003 REQ "Durable Log Rotation And
	// Compression"; SPEC-0008 REQ "Per-Run Logs"). The daemon passes the
	// resolved `[daemon] compress_logs`, whose default is TRUE
	// (daemonManagerOptions, pinned by its wiring test). The zero value here
	// is off so a test Manager's logs stay where the test reads them.
	CompressLogs bool
	// LedgerDir is the run ledger's directory (SPEC-0022 REQ-1). Defaults to
	// a "ledger" directory beside state.json — $XDG_STATE_HOME/harness/ledger
	// in production.
	LedgerDir string
	// Watch, if set, is the turn-state bridge graceful closes sample.
	// Defaults to a real internal/runtrace watcher; tests inject stubs so
	// the close machinery can be driven without trace stores.
	Watch TurnBridge
	// HoldClearers are the clearing hooks for hold reasons other than hours
	// (SPEC-0021 REQ-14), keyed by reason: each answers whether a harness's
	// hold for that reason has cleared at the gate tick's clock. The park
	// story feeds quota's (a park's reset instant). Budget's (a rollover, or
	// a cap a reload raised) is the Manager's own, budgetHoldCleared, and an
	// entry here replaces it only in a test (manager_holds.go,
	// manager_admit.go).
	HoldClearers map[core.HoldReason]HoldClearer
	// Now is the clock admission decides on when a start path brings no tick
	// of its own: an operator start, a restart, a firing (SPEC-0021 REQ-4;
	// manager_admit.go). It defaults to the wall clock with its monotonic
	// reading stripped, as the scheduler's tick is; tests set it to the same
	// fake clock they drive the scheduler with, so the budget day and the
	// gate agree on what time it is.
	Now func() time.Time
}

// Manager supervises every harness in a config.
type Manager struct {
	policy       Policy
	statePath    string
	logCfg       LogConfig
	bus          *Bus
	extraOutFor  func(name string) io.Writer
	dropExtraOut func(name string)
	sizeFor      func(name string) (int, int)
	// reloadHook, when set, runs after every successful Reload regardless of
	// which path triggered it (SIGHUP, watcher, reload control op). Set once
	// at daemon boot via SetReloadHook (issue #66).
	reloadHook func()

	// guard, when set (SetSessionGuard at daemon boot), samples running
	// crush stores for context-limit wedges and rotates stalled sessions
	// (issue #347). Its per-harness findings overlay onto every Snapshot.
	guard *SessionGuard

	mu                sync.Mutex
	cfg               *core.Config
	supervisors       map[string]*Supervisor
	order             []string
	activeProfile     string
	profileUnresolved bool // persisted active profile is missing from config (#99)

	// dormantAutostart names harnesses that an autostart profile asks for but
	// that state.json restored as disabled, so the daemon deliberately did NOT
	// start them. Persisted intent wins on purpose — an operator `harness stop`
	// must survive a daemon restart — but the result is a config that says
	// "autostart" next to a harness that never comes up, with nothing said about
	// it. Recorded here so boot can log it and doctor can show it.
	dormantAutostart []string

	// scheduleMarks is the scheduler's durable per-harness position
	// (ADR-0013; SPEC-0008 REQ "Missed Window Handling"), restored from and
	// saved to state.json with everything else here.
	scheduleMarks map[string]ScheduleMark

	// leases holds each gated harness's after-hours lease end (SPEC-0012 REQ
	// "After-Hours Lease"), restored from and saved to state.json. An entry
	// is created only by StartFor — the start control op on a gated,
	// out-of-hours harness — and removed when it ends, when its hours open,
	// or by a stop. The gate pass consults it through Lease before holding.
	leases map[string]time.Time

	// expiredLease remembers the end of the lease whose expiry the gate pass
	// is about to enforce: the instant the harness went out of hours, which
	// anchors a graceful close's deadline (SPEC-0012 REQ "Graceful
	// Shutdown"). Written by leaseState when it drops a spent lease,
	// consumed by Hold, cleared by StartFor and by an in-hours discard.
	expiredLease map[string]time.Time

	// watch is the turn-state bridge graceful closes sample (SPEC-0012 REQ
	// "Turn State Signal"); armedCloseAt dedupes the pass's per-tick re-arms
	// inside the minute before a close. Governing: manager_hours.go.
	watch        TurnBridge
	armedCloseAt map[string]time.Time

	// holdClearers are ManagerOptions.HoldClearers, read-only after
	// NewManager (manager_holds.go). The budget reason's is the Manager's
	// own (budgetHoldCleared) unless a test supplies one.
	holdClearers map[core.HoldReason]HoldClearer

	// nowFn is ManagerOptions.Now (now()). budgetMu guards bud, the budget
	// day and its counters, which admission decides on (manager_admit.go;
	// SPEC-0021 REQ-4, REQ-7). Lock order: budgetMu before journalMu before
	// mu.
	nowFn    func() time.Time
	budgetMu sync.Mutex
	bud      budgetBook

	// runs is each harness's run id allocator, and jobsDir the root of the
	// per-run logs (manager_runs.go; SPEC-0008 REQ "Run History"). The records
	// themselves are in ledger, whose only writer is this Manager's
	// RunJournal (SPEC-0022 REQ-6). legacyRuns holds a pre-ledger state.json's
	// record lists until the first-boot import has copied them in (REQ-13).
	runs       map[string]*runHistory
	legacyRuns map[string][]RunRecord
	jobsDir    string
	ledger     *ledger.Ledger
	// sealer compresses sealed logs in the background; nil when
	// compress_logs is off (ManagerOptions.CompressLogs).
	sealer *sealedlog.Compressor
	// journalMu makes a run id's allocation and its ledger line's enqueue
	// one step (appendNew), so ids and seqs agree on order.
	journalMu sync.Mutex

	// saveMu serializes Save. The debounced persist loop is not the only
	// writer — the scheduler flushes its marks synchronously before a run
	// starts — and two unserialized Saves can rename an older snapshot over a
	// newer one.
	saveMu sync.Mutex

	// projects tracks every registered project by name, and
	// provenance maps a registered harness's full name to its owning project
	// ("" / absent = global config) so `down`/`ps` scope correctly and a global
	// reload never touches project harnesses. Governing: ADR-0009
	// (project-scoped compose), SPEC-0004 REQ "Project Naming And Namespacing".
	projects   map[string]*projectRecord
	provenance map[string]string
	// scratchDefs holds the registered scratchpad definitions by minted name
	// (ADR-0017) so HarnessRecord/HarnessDef resolve them like any other
	// harness — provenance "scratch" has no projectRecord to read from.
	scratchDefs map[string]core.Harness

	dirty  chan struct{}
	closed chan struct{}
	// closeOnce guards closed, so Close is idempotent. It has to be: Close
	// drains the ledger, and a caller that wants a drained ledger (a test
	// reading the files, a shutdown path that also runs a t.Cleanup) would
	// otherwise panic on the second call with "close of closed channel".
	closeOnce sync.Once
	wg        sync.WaitGroup
}

// NewManager builds a Manager for cfg. Supervisors are created (stopped) but not
// started; call Restore then Autostart (or Start) to bring up the intended set.
func NewManager(cfg *core.Config, opts ManagerOptions) *Manager {
	policy := opts.Policy.normalize()
	statePath := opts.StatePath
	if statePath == "" {
		statePath = DefaultStatePath()
	}
	logCfg := opts.LogCfg
	if opts.LogDir != "" {
		logCfg.Dir = opts.LogDir
	} else if logCfg.Dir == "" {
		logCfg.Dir = DefaultLogDir()
	}
	jobsDir := opts.JobsDir
	if jobsDir == "" {
		jobsDir = filepath.Join(filepath.Dir(logCfg.Dir), "jobs")
	}
	// One compressor for every harness: a single background goroutine and a
	// single reused encoder, however many harnesses seal files. Nil (off)
	// makes every Seal a no-op.
	var sealer *sealedlog.Compressor
	if opts.CompressLogs {
		sealer = sealedlog.NewCompressor()
	}
	logCfg.Sealer = sealer

	ledgerDir := opts.LedgerDir
	if ledgerDir == "" {
		ledgerDir = filepath.Join(filepath.Dir(statePath), "ledger")
	}
	// Open does not fail: a ledger it cannot write yet queues and retries
	// (SPEC-0022 REQ-6). What it could not read at boot is logged here.
	lg, err := ledger.Open(ledgerDir, ledger.Options{})
	if err != nil {
		log.Error("run ledger boot incomplete", "dir", ledgerDir, "err", err)
	}

	m := &Manager{
		runs:          make(map[string]*runHistory),
		legacyRuns:    make(map[string][]RunRecord),
		ledger:        lg,
		jobsDir:       jobsDir,
		sealer:        sealer,
		policy:        policy,
		statePath:     statePath,
		logCfg:        logCfg,
		bus:           NewBus(),
		extraOutFor:   opts.ExtraOutFor,
		dropExtraOut:  opts.DropExtraOut,
		sizeFor:       opts.SizeFor,
		cfg:           cfg,
		supervisors:   make(map[string]*Supervisor),
		projects:      make(map[string]*projectRecord),
		scratchDefs:   make(map[string]core.Harness),
		provenance:    make(map[string]string),
		scheduleMarks: make(map[string]ScheduleMark),
		leases:        make(map[string]time.Time),
		expiredLease:  make(map[string]time.Time),
		armedCloseAt:  make(map[string]time.Time),
		watch:         opts.Watch,
		holdClearers:  maps.Clone(opts.HoldClearers),
		nowFn:         opts.Now,
		bud:           budgetBook{runs: make(map[string]int), skips: make(map[string]time.Time)},
		dirty:         make(chan struct{}, 1),
		closed:        make(chan struct{}),
	}
	if m.watch == nil {
		m.watch = runtrace.NewWatcher(runtrace.DefaultWatchPoll)
	}
	// A budget hold clears at the gate tick once the budget is no longer
	// spent (SPEC-0021 REQ-14, REQ-20); the Manager is what knows.
	if _, ok := m.holdClearers[core.HoldBudget]; !ok {
		if m.holdClearers == nil {
			m.holdClearers = make(map[core.HoldReason]HoldClearer)
		}
		m.holdClearers[core.HoldBudget] = m.budgetHoldCleared
	}
	for _, name := range cfg.HarnessOrder {
		m.addSupervisor(cfg.Harnesses[name])
	}
	m.wg.Add(1)
	go m.persistLoop()
	return m
}

// now is the Manager's clock (ManagerOptions.Now): the wall clock with its
// monotonic reading stripped, so an instant compared with a day boundary is
// judged by the wall clock even across a suspend.
func (m *Manager) now() time.Time {
	if m.nowFn != nil {
		return m.nowFn()
	}
	return time.Now().Round(0)
}

// Events subscribes to the lifecycle event stream (SPEC-0003 REQ "Lifecycle
// Events"). The daemon later relays these over the control socket (SPEC-0002).
func (m *Manager) Events() (<-chan Event, func()) { return m.bus.Subscribe() }

// EventsCounted is Events plus this subscriber's running count of events lost
// to a full buffer (Bus.SubscribeCounted), for a consumer that must report
// its own undercount — the metrics collector (SPEC-0013 REQ-6).
func (m *Manager) EventsCounted() (<-chan Event, func(), func() uint64) {
	return m.bus.SubscribeCounted()
}

// addSupervisor constructs and registers a global-config supervisor for h,
// appending it to the render order. Only called single-threaded from
// NewManager.
func (m *Manager) addSupervisor(h core.Harness) {
	m.addSupervisorLocked(h)
	m.order = append(m.order, h.Name)
}

// extraOut resolves the per-harness tee writer (the attach emulator/ring) from
// the configured factory, or nil when none is set. A writer that can prepare
// for h's kind of run is told which it is (Primer, pipes.go).
func (m *Manager) extraOut(h core.Harness) io.Writer {
	if m.extraOutFor == nil {
		return nil
	}
	w := m.extraOutFor(h.Name)
	if p, ok := w.(Primer); ok {
		p.Prime(RunsOnPipes(h))
	}
	return w
}

// initialSizeFor binds the configured SizeFor hook to one harness name, or nil
// when none is set (the supervisor then falls back to 80×24). It is resolved
// lazily on every spawn, so the size reflects whoever is attached *now* rather
// than whoever was attached when the supervisor was constructed.
func (m *Manager) initialSizeFor(name string) func() (int, int) {
	if m.sizeFor == nil {
		return nil
	}
	return func() (int, int) { return m.sizeFor(name) }
}

// markDirty signals the persist loop that state changed (non-blocking).
func (m *Manager) markDirty() {
	select {
	case m.dirty <- struct{}{}:
	default:
	}
}

// Restore loads state.json and seeds each supervisor's persisted intent and
// counters (ADR-0007). Harnesses absent from state.json fall back to config
// autostart membership (ADR-0006) as their initial intent.
//
// If the persisted active profile no longer exists in config (issue #99) — a
// normal chezmoi rename — the daemon resolves it to the autostart=true profile,
// and tracks the unresolved state so doctor can surface it. The caller should
// check ProfileResolved() and log a warning.
func (m *Manager) Restore() error {
	ps, err := loadState(m.statePath)
	if err != nil {
		if errors.Is(err, errMalformedState) {
			// The daemon carries on from config defaults and its next Save
			// replaces this file. Keep a copy first, so what it held — intent,
			// projects, run history — is recoverable by hand rather than gone.
			if kept, cerr := preserveMalformedState(m.statePath); cerr == nil {
				err = fmt.Errorf("%w (kept a copy at %s)", err, kept)
			}
		}
		return err
	}
	autostart := autostartSet(m.cfg)

	m.mu.Lock()
	m.activeProfile = ps.ActiveProfile
	m.restoreRunsLocked(ps.Runs)
	for name, sched := range ps.Schedules {
		mark := ScheduleMark{Spec: sched.Spec, DecidedThrough: sched.DecidedThrough}
		if sched.LastRunAt != nil {
			mark.LastRunAt = *sched.LastRunAt
		}
		m.scheduleMarks[name] = mark
	}

	// Re-register persisted projects (SPEC-0004 REQ "Registration
	// Persistence") so their supervisors exist before the intent loop below
	// seeds them — same Compose-style contract as the globals: up stays up
	// across daemon restarts until down/rm. Definitions are registered but
	// NOT started here; the persisted per-harness intent (written under the
	// fully-qualified name) plus Autostart brings up the running set. Sorted
	// by project name for a deterministic registration order.
	projects := make([]string, 0, len(ps.Projects))
	for name := range ps.Projects {
		projects = append(projects, name)
	}
	slices.Sort(projects)
	for _, project := range projects {
		pp := ps.Projects[project]
		defs := make([]core.Harness, 0, len(pp.Harnesses))
		for _, ph := range pp.Harnesses {
			defs = append(defs, ph.toCore())
		}
		m.restoreProjectLocked(project, defs)
	}

	// Issue #99: detect a persisted profile name that no longer resolves.
	// Only the flag is set here — the fallback itself happens in the restore
	// loop below, because that loop is what actually decides each harness's
	// intent and would otherwise overwrite anything seeded at this point.
	if ps.ActiveProfile != "" {
		if _, ok := m.cfg.Profiles[ps.ActiveProfile]; !ok {
			m.profileUnresolved = true
		}
	}
	unresolved := m.profileUnresolved
	hasAutostartProfile := m.autostartProfileName() != ""

	sups := make(map[string]*Supervisor, len(m.supervisors))
	for k, v := range m.supervisors {
		sups[k] = v
	}
	m.mu.Unlock()

	var dormant []string
	// Lease restores are collected in the loop and committed under m.mu at
	// the end (the loop itself runs unlocked — s.Restore blocks on the actor
	// loop), so the persist loop never sees an unsynchronized map write.
	restoredLeases := make(map[string]time.Time)
	for name, s := range sups {
		pr, inState := ps.Harnesses[name]
		var last time.Time
		if inState && pr.LastExitAt != nil {
			last = *pr.LastExitAt
		}
		// After-hours leases (SPEC-0012 REQ "After-Hours Lease") restore only
		// for a harness that is still gated: the lease borrows its meaning
		// from the operating hours, and one left behind by a config that
		// since dropped them (or by a harness that changed shape) is dead
		// weight — Lease would discard it lazily anyway, but dropping it here
		// keeps state.json honest from the first save.
		if inState && pr.LeaseUntil != nil {
			if s.Snapshot().Gated {
				restoredLeases[name] = *pr.LeaseUntil
			} else {
				log.Info("dropping lease for a harness that is no longer gated", "harness", name)
			}
		}
		// An autostart member restored as disabled will not be started by
		// Autostart(), and no existing signal says so: `harness list` shows it
		// stopped, doctor calls the set healthy, and the config still reads
		// `autostart = true`. Collect it so both can speak up. This is only the
		// plain inState case — the #99 fallback above overrides intent to true,
		// so it is never dormant.
		if inState && !pr.Enabled && autostart[name] && !(unresolved && hasAutostartProfile) {
			dormant = append(dormant, name)
		}
		// A scheduled harness is a cron one-shot that only the scheduler may
		// start (ADR-0013) — which is why the config parser rejects
		// `enabled = true` alongside `schedule`. Persisted intent can still
		// say true: written by a daemon predating StartTransient, or by an
		// operator's `harness start <sweep>`, which goes through Start.
		// Restore would seed that, Autostart would fire the one-shot
		// off-schedule, and Autostart's Start re-persists true — so the stale
		// intent never heals and every daemon restart fires the run again.
		// Clamp it here: for a scheduled harness the schedule IS the intent.
		// Residual of issue #159.
		// Triggered, not Scheduled: a webhook-only harness is the same
		// one-shot, and a stale enabled=true would fire it on every boot
		// with no event (SPEC-0014).
		scheduled := s.Snapshot().Triggered
		var started time.Time
		if inState && pr.LastStarted != nil {
			started = *pr.LastStarted
		}
		// The last intent change (issue #835) is history, not intent: it
		// restores for every harness the file names, so a daemon restarted
		// after a flip still answers who flipped it and when.
		var lastIntent IntentChange
		if inState && pr.LastIntentAt != nil {
			lastIntent = IntentChange{At: *pr.LastIntentAt, Source: pr.LastIntentSource, Peer: pr.LastIntentPeer}
		}

		switch {
		case scheduled:
			// Counters are observability, not intent — preserve them, the
			// same contract the #99 fallback below keeps. The operator-stop
			// suppression (stump.wtf/harness#786) IS intent, so it restores:
			// a schedule paused by `harness stop` stays paused across a
			// daemon restart.
			s.Restore(false, pr.OperatorStopped, pr.RestartCount, pr.LastExitCode, last, started, lastIntent)
			// A weekend's outside_hours skips are still owed their
			// catch-up after a restart (SPEC-0014 REQ "Operating Hours
			// On Triggered Harnesses"; firing_hours.go).
			m.seedHoursSkipped(name, s)
		case unresolved && hasAutostartProfile && autostart[name]:
			// The persisted profile is gone, so the persisted per-harness
			// intent it produced cannot be trusted either — a member recorded
			// as disabled was most likely disabled BY that profile, not by the
			// operator. Autostart membership wins, which is the whole point of
			// the fallback: the daemon must not come up having started nothing.
			// Counters are preserved so restart history is not lost (#99).
			s.Restore(true, false, pr.RestartCount, pr.LastExitCode, last, started, lastIntent)
		case inState:
			s.Restore(pr.Enabled, pr.OperatorStopped, pr.RestartCount, pr.LastExitCode, last, started, lastIntent)
		case autostart[name]:
			s.Restore(true, false, 0, 0, time.Time{}, time.Time{}, IntentChange{})
		}
	}

	// Stable order so the warning and doctor row don't reshuffle between boots
	// (sups is a map).
	slices.Sort(dormant)
	m.mu.Lock()
	m.dormantAutostart = dormant
	for name, until := range restoredLeases {
		m.leases[name] = until
	}
	m.mu.Unlock()

	// Before Autostart admits anything: import, backfill and reconcile the
	// run ledger (SPEC-0022 REQ-7, REQ-13), then rebuild today's budget
	// counters from it (SPEC-0021 REQ-7).
	m.bootLedger()
	m.bootBudget()
	return nil
}

// preserveMalformedState copies an unparseable state file aside and returns the
// copy's path.
func preserveMalformedState(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	kept := path + ".malformed-" + time.Now().Format("20060102T150405")
	return kept, os.WriteFile(kept, data, 0o600)
}

// Autostart starts every harness whose restored intent is enabled (SPEC-0003
// REQ "Autostart"). Safe to call once after Restore.
//
// A gated harness (operating_hours set) is not started here: it begins held,
// and the scheduler's gate pass — whose first evaluation runs as soon as the
// scheduler starts — releases it if it is in hours. That keeps the in-hours
// decision on the scheduler's clock seam rather than a wall-clock read here,
// and means a daemon booting at 20:00 never starts a 09:00-13:00 harness just
// to shut it a second later. Governing: SPEC-0012 REQ "Gate Enforcement".
//
// Scheduled harnesses are skipped unconditionally: a cron one-shot is started
// by the scheduler and by nothing else (ADR-0013). The same holds for any
// triggered harness (SPEC-0014): its firing source is its only starter. Restore already clamps
// their persisted intent, so this is defense in depth — but it is the check
// that actually holds the invariant, because Start re-persists enabled=true
// and would make any leak permanent (issue #159).
func (m *Manager) Autostart() {
	now := time.Now()
	for _, s := range m.snapshotSupervisors() {
		snap := s.Snapshot()
		if snap.Triggered {
			continue
		}
		if !snap.Enabled {
			continue
		}
		if snap.Gated {
			// SPEC-0012 REQ "After-Hours Lease": a gated harness covered by a
			// lease that has not ended starts on boot and is held by the
			// gate pass when the lease ends — not later. The lease record
			// was restored from state.json by Restore; without one, out of
			// hours begins held as usual.
			m.mu.Lock()
			until, leased := m.leases[s.Name()]
			m.mu.Unlock()
			if leased && now.Before(until) {
				s.StartWith(TriggerLease)
				continue
			}
			s.Hold(core.HoldHours, core.HoursShutdownImmediate, time.Time{})
			continue
		}
		s.StartWith(TriggerAutostart)
	}
}

// startOrHold is how intent-setting paths (UseProfile, a reload introducing an
// autostart harness) bring a harness up: an ungated one starts, and a gated
// one that is down records enabled intent and begins held, for the gate pass
// to release if it is in hours. A gated harness already up just gains the
// intent (it keeps running and closes at its window's end), and a failed one
// is started as before — Release never starts a failed harness, so holding it
// would strand it. Governing: SPEC-0012 REQ "Operating Hours Reload", ADR-0014.
//
// trigger names the start path for the resident run it opens (SPEC-0022
// REQ-3): manual for an operator's profile switch, autostart for a reload.
func startOrHold(s *Supervisor, trigger RunTrigger) {
	snap := s.Snapshot()
	if !snap.Gated || snap.State == core.StateFailed || snapUp(snap.State) {
		s.StartWith(trigger)
		return
	}
	s.EnableHeld(trigger)
}

// snapUp reports the states the operating-hours gate holds: a harness that is
// up, or on its way back up (SPEC-0012 REQ "Gate Enforcement").
func snapUp(st core.State) bool {
	switch st {
	case core.StateStarting, core.StateRunning, core.StateDegraded, core.StateRestarting:
		return true
	}
	return false
}

// Lease reports name's after-hours lease end, ok=false when it has none. It
// is the seam the gate pass consults before holding a harness: a harness
// covered by a lease that has not ended is never held, and once the lease has
// ended (or its hours opened first, which discards it) the answer reverts to
// "none" and the pass enforces the gate as usual. Governing: SPEC-0012 REQ
// "After-Hours Lease".
//
// Lease reads the wall clock (monotonic reading stripped, as the scheduler's
// tick is). The gate pass never uses it: it asks LeaseAt with its own tick's
// time, so lease validity and hours are judged at one instant.
func (m *Manager) Lease(name string) (until time.Time, ok bool) {
	return m.leaseState(name, time.Now().Round(0))
}

// LeaseAt is Lease judged at now — the scheduler tick's clock, which is the
// only time source the operating-hours gate may decide on (design.md § "A
// pure internal/hours package": the clock seam stays the only time source).
// The gate pass calls it for every gated harness on every tick, in hours or
// not, and the call is what retires a lease: one that has ended at now is
// dropped with its end remembered as the close's anchor, and one whose hours
// have opened is discarded. Governing: SPEC-0012 REQ "After-Hours Lease", REQ
// "Gate Evaluation".
func (m *Manager) LeaseAt(name string, now time.Time) (until time.Time, ok bool) {
	return m.leaseState(name, now)
}

// leaseState is Lease against an injectable clock, so the discard rules —
// expired, hours opened, hours removed — are testable at minute boundaries
// without waiting on the wall clock. It mutates the lease map (a discard is
// durable bookkeeping, not a read). A lease ends AT its end instant: the
// pass holds when !now.Before(until), and so the record is spent then too.
func (m *Manager) leaseState(name string, now time.Time) (time.Time, bool) {
	m.mu.Lock()
	until, ok := m.leases[name]
	if !ok {
		m.mu.Unlock()
		return time.Time{}, false
	}
	h := m.cfg.Harnesses[name]
	if !now.Before(until) {
		// Expired: the pass will hold on this very tick. Drop the record so
		// state.json stops carrying a dead lease, and remember the end — the
		// instant the harness went out of hours, which anchors the close's
		// deadline (SPEC-0012 REQ "Graceful Shutdown") — for Hold to consume.
		delete(m.leases, name)
		m.expiredLease[name] = until
		m.mu.Unlock()
		log.Info("after-hours lease ended", "harness", name, "until", until.Format(time.RFC3339))
		m.markDirty()
		return time.Time{}, false
	}
	if h.OperatingHours != "" {
		if in, _, _ := h.HoursExpr.In(now); in {
			// Hours opened before the lease ran out: discard it, and the
			// harness simply continues as an in-hours harness (SPEC-0012 REQ
			// "After-Hours Lease"). A close whose deadline anchored to an
			// earlier boundary is not coming — hours are open — so drop any
			// remembered end with it.
			delete(m.leases, name)
			delete(m.expiredLease, name)
			m.mu.Unlock()
			log.Info("after-hours lease discarded; hours opened", "harness", name)
			m.markDirty()
			return time.Time{}, false
		}
	} else {
		// The harness lost its operating_hours under a live lease: nothing
		// gates it anymore, so the lease is meaningless.
		delete(m.leases, name)
		delete(m.expiredLease, name)
		m.mu.Unlock()
		m.markDirty()
		return time.Time{}, false
	}
	m.mu.Unlock()
	return until, true
}

// StartFor starts name under an after-hours lease ending forDur from now
// (SPEC-0012 REQ "After-Hours Lease"). The lease is persisted to state.json
// SYNCHRONOUSLY before the start, so a crash between the two leaves a bounded
// lease on disk — boot resolves it by starting the harness, never an
// unbounded run.
//
// forDur must be positive. A harness that is ungated, or gated and already in
// hours, fails with ErrNoLease: no lease applies to it. An already-leased
// harness gets its end replaced with the new one. Enabled intent is persisted
// as any manual start does.
func (m *Manager) StartFor(name string, forDur time.Duration) error {
	if forDur <= 0 {
		return fmt.Errorf("%w: lease length must be positive", ErrNoLease)
	}
	m.mu.Lock()
	s := m.supervisors[name]
	h := m.cfg.Harnesses[name]
	if s == nil {
		m.mu.Unlock()
		return ErrUnknownHarness
	}
	if h.OperatingHours == "" {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s has no operating_hours", ErrNoLease, name)
	}
	if in, _, _ := h.HoursExpr.In(time.Now()); in {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s is in hours now", ErrNoLease, name)
	}
	// Wall clock only: a monotonic reading on until would make a comparison
	// with another monotonic time ignore a suspend (the monotonic clock stops
	// while the host sleeps), so the lease would outlast its wall-clock end.
	until := time.Now().Round(0).Add(forDur)
	m.leases[name] = until
	// The lease governs the gate from here: an expired-lease anchor (if one
	// was remembered) is superseded, and a close waiting on turn state is
	// cancelled by the start below — no watch needed for it anymore.
	delete(m.expiredLease, name)
	m.mu.Unlock()

	// The lease is durable BEFORE the start: the whole point of the ordering
	// (design.md § "Leases in state.json"). A failed save still starts the
	// harness — running matters more than the record — but it is logged here
	// at the moment the guarantee breaks rather than at the crash that would
	// have needed it.
	if err := m.Save(); err != nil {
		log.Error("could not persist after-hours lease before starting",
			"harness", name, "until", until.Format(time.RFC3339), "err", err)
	}
	log.Info("after-hours lease", "harness", name, "until", until.Format(time.RFC3339))
	m.unfollowWatch(name) // a lease starting cancels the close (SPEC-0012)
	// A lease's start passes admission like any other (SPEC-0021 REQ-4); a
	// refusal is the caller's error, and the lease stands for the window it
	// was asked for.
	_, err := m.startWith(name, TriggerLease, "")
	return err
}

// ErrNoLease reports that a requested after-hours lease does not apply: the
// harness is unknown, ungated, or already in its operating hours. Errors.Is-able.
var ErrNoLease = errors.New("no lease applies")

// ErrUnknownHarness reports that a lease was requested for a harness the
// manager does not supervise. Errors.Is-able.
var ErrUnknownHarness = errors.New("unknown harness")

// DefaultLease is the lease length a start control op applies when a gated,
// out-of-hours harness is started without an explicit --for (SPEC-0012 REQ
// "After-Hours Lease": the optional for defaults to 1h).
const DefaultLease = time.Hour

// LeaseApplies reports whether an after-hours lease applies to name right
// now: the harness is known, gated, and outside its operating hours — the
// condition under which a start needs a lease, and under which the daemon's
// start control op routes a plain start through StartFor with the default
// length. Governing: SPEC-0012 REQ "After-Hours Lease".
func (m *Manager) LeaseApplies(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, known := m.supervisors[name]; !known {
		return false
	}
	h := m.cfg.Harnesses[name]
	if h.OperatingHours == "" {
		return false
	}
	in, _, _ := h.HoursExpr.In(time.Now())
	return !in
}

// Start marks a single harness enabled and brings it up.
func (m *Manager) Start(name string) bool {
	ok, _ := m.startWith(name, TriggerManual, "")
	return ok
}

// StartPeer is Start naming the socket peer the platform reported for the
// verb (issue #835): the intent-change line carries it.
func (m *Manager) StartPeer(name, peer string) bool {
	ok, _ := m.startWith(name, TriggerManual, peer)
	return ok
}

// StartChecked is StartPeer that also reports a start admission refused
// (SPEC-0021 REQ-4, REQ-21): the error wraps budget.ErrOverBudget,
// budget.ErrParked or budget.ErrLedgerUnavailable where one applies. The
// intent it records stands either way; a refused resident is held. ok is
// false for an unknown harness.
func (m *Manager) StartChecked(name, peer string) (ok bool, refused error) {
	return m.startWith(name, TriggerManual, peer)
}

// startWith is Start naming the start path its resident run records, and
// reporting a refusal.
func (m *Manager) startWith(name string, trigger RunTrigger, peer string) (bool, error) {
	if s := m.get(name); s != nil {
		d := s.startChecked(trigger, peer)
		// Starting it IS the fix for a dormant autostart member, so stop
		// reporting it — otherwise boot's warning and doctor's row outlive the
		// condition they describe, until the next restore.
		m.clearDormant(name)
		return true, d.Refused
	}
	return false, nil
}

// StartTransient brings a harness up without persisting enabled intent to
// state.json. Used by the scheduler for one-shot firings (issue #159).
func (m *Manager) StartTransient(name string) bool {
	if s := m.get(name); s != nil {
		s.StartTransient()
		m.clearDormant(name)
		return true
	}
	return false
}

// clearDormant drops name from the dormant-autostart list.
func (m *Manager) clearDormant(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dormantAutostart = slices.DeleteFunc(m.dormantAutostart, func(n string) bool {
		return n == name
	})
}

// Stop gracefully stops a single harness and clears its enabled intent.
func (m *Manager) Stop(name string) bool { return m.stopBy(name, "verb:stop", "") }

// StopPeer is Stop naming the socket peer the platform reported for the verb
// (issue #835).
func (m *Manager) StopPeer(name, peer string) bool { return m.stopBy(name, "verb:stop", peer) }

// StopBy is Stop naming the path that clears the intent (issue #835): the
// loop guard passes "guard", so a guard stop does not masquerade as a verb.
func (m *Manager) StopBy(name, source string) bool { return m.stopBy(name, source, "") }

func (m *Manager) stopBy(name, source, peer string) bool {
	if s := m.get(name); s != nil {
		// A stop discards any after-hours lease in every gate state — leased,
		// held, closing, whatever (SPEC-0012 REQ "After-Hours Lease"): the
		// operator's stop is the final word, and no later window may start
		// the harness with a lease the operator already killed.
		m.mu.Lock()
		_, had := m.leases[name]
		delete(m.leases, name)
		delete(m.expiredLease, name)
		m.mu.Unlock()
		if had {
			m.markDirty()
		}
		m.unfollowWatch(name) // a stop never waits on turn state (SPEC-0012)
		s.StopBy(source, peer)
		return true
	}
	return false
}

// Restart restarts a single harness (clearing a failed latch).
func (m *Manager) Restart(name string) bool { return m.restartBy(name, "verb:restart", "") }

// RestartPeer is Restart naming the socket peer of a socket verb (issue #835).
func (m *Manager) RestartPeer(name, peer string) bool { return m.restartBy(name, "verb:restart", peer) }

func (m *Manager) restartBy(name, source, peer string) bool {
	if s := m.get(name); s != nil {
		s.RestartBy(source, peer)
		return true
	}
	return false
}

// RestartChecked is RestartPeer that also reports a start admission refused
// (StartChecked).
func (m *Manager) RestartChecked(name, peer string) (ok bool, refused error) {
	if s := m.get(name); s != nil {
		return true, s.restartChecked("verb:restart", peer).Refused
	}
	return false, nil
}

// Enable sets a harness's enabled intent and starts it if stopped. It is
// Start under another name — cmdStart is what carries the intent — and
// delegates rather than repeating the body, which is how it keeps Start's
// dormant-autostart clearing instead of quietly diverging from it.
func (m *Manager) Enable(name string) bool { return m.Start(name) }

// EnablePeer is Enable naming the socket peer of a socket verb (issue #835).
func (m *Manager) EnablePeer(name, peer string) bool { return m.StartPeer(name, peer) }

// Disable clears a harness's enabled intent and stops it if running (cmdStop
// carries the intent; see Enable).
func (m *Manager) Disable(name string) bool { return m.Stop(name) }

// DisablePeer is Disable naming the socket peer of a socket verb (issue #835).
func (m *Manager) DisablePeer(name, peer string) bool { return m.StopPeer(name, peer) }

// Resize resizes a single harness's live PTY (ADR-0003), ok=false if unknown.
func (m *Manager) Resize(name string, cols, rows int) bool {
	if s := m.get(name); s != nil {
		s.Resize(cols, rows)
		return true
	}
	return false
}

// WriteInput delivers attach keystrokes to a single harness's PTY (SPEC-0002
// REQ "Attach Session"), ok=false if unknown.
func (m *Manager) WriteInput(name string, p []byte) bool {
	if s := m.get(name); s != nil {
		s.WriteInput(p)
		return true
	}
	return false
}

// SignalGroup delivers a signal to a single harness's live process group
// (stump.wtf/harness#182: the attach layer re-delivers SIGWINCH after a resize
// that may have landed during the guest's boot), ok=false if unknown.
func (m *Manager) SignalGroup(name string, sig syscall.Signal) bool {
	if s := m.get(name); s != nil {
		s.SignalGroup(sig)
		return true
	}
	return false
}

// Config returns the manager's current parsed config (ADR-0006 source of
// truth). The daemon uses it to answer describe/profiles and to project
// Cmd/Backend/Description into control responses.
func (m *Manager) Config() *core.Config {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg
}

// LogDir returns the per-harness log directory (ADR-0007). The daemon reads
// <dir>/<name>.log to service the logs control op.
func (m *Manager) LogDir() string { return m.logCfg.Dir }

// WaitSealed blocks until every sealed log queued for compression so far has
// been handled; it returns at once when compression is off. Nothing in the
// daemon waits on compression — readers take either form — so this is for a
// test that must see the compressed form on disk, without polling for it.
func (m *Manager) WaitSealed() { m.sealer.Wait() }

// ProfileResolved reports whether the active profile name resolves to a real
// profile in the current config (issue #99). False means the persisted profile
// was renamed or removed upstream (e.g. by a chezmoi config delivery) and the
// daemon fell back to the autostart profile.
func (m *Manager) ProfileResolved() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return !m.profileUnresolved
}

// DormantAutostart returns the harnesses an autostart profile asks for that
// state.json restored as disabled, so Autostart() left them down. Empty is the
// healthy case. Persisted intent beating config is deliberate (an operator stop
// must survive a restart), but it is silent — this is what lets boot log it and
// doctor surface it, instead of the operator reading `autostart = true` next to
// a harness that never starts.
func (m *Manager) DormantAutostart() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.dormantAutostart...)
}

// ActiveProfile returns the currently active profile name, if any (ADR-0006).
func (m *Manager) ActiveProfile() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.activeProfile
}

// UseProfile activates a profile: it records the active profile and starts
// (enables) every member harness, the "hop into a configuration" gesture
// (ADR-0006). Returns false if the profile is unknown. Non-members are left
// untouched — switching does not stop harnesses out from under other work.
// Choosing a profile clears any prior unresolved state (#99).
func (m *Manager) UseProfile(name string) bool {
	m.mu.Lock()
	p, ok := m.cfg.Profiles[name]
	if !ok {
		m.mu.Unlock()
		return false
	}
	members := append([]string(nil), p.Harnesses...)
	m.activeProfile = name
	m.profileUnresolved = false
	// Every member is about to be enabled below, so no member can still be a
	// dormant autostart entry.
	m.dormantAutostart = slices.DeleteFunc(m.dormantAutostart, func(n string) bool {
		return slices.Contains(members, n)
	})
	m.mu.Unlock()

	for _, hn := range members {
		if s := m.get(hn); s != nil {
			// A gated member that is down records intent and stays held; the
			// gate decides whether its process exists (SPEC-0012 REQ
			// "Operating Hours Reload").
			startOrHold(s, TriggerManual)
		}
	}
	m.markDirty()
	return true
}

// Snapshot returns one harness's runtime snapshot, ok=false if unknown.
func (m *Manager) Snapshot(name string) (Snapshot, bool) {
	if s := m.get(name); s != nil {
		return m.overlayGuard(s.Snapshot()), true
	}
	return Snapshot{}, false
}

// SetSessionGuard attaches the session guard whose findings overlay onto
// every Snapshot (issue #347). Nil detaches (daemon shutdown).
func (m *Manager) SetSessionGuard(g *SessionGuard) {
	m.mu.Lock()
	m.guard = g
	m.mu.Unlock()
}

// overlayGuard stamps the session guard's observation onto snap so callers
// see a wedged session even while process state still reads healthy. Zero
// when no guard is attached or it never flagged the harness.
func (m *Manager) overlayGuard(snap Snapshot) Snapshot {
	m.mu.Lock()
	g := m.guard
	m.mu.Unlock()
	if g == nil {
		return snap
	}
	snap.SessionStalled, snap.SessionRotations = g.Flag(snap.Name)
	return snap
}

// Snapshots returns every harness's snapshot in config order.
func (m *Manager) Snapshots() []Snapshot {
	m.mu.Lock()
	order := append([]string(nil), m.order...)
	sups := m.supervisors
	list := make([]*Supervisor, 0, len(order))
	for _, name := range order {
		if s, ok := sups[name]; ok {
			list = append(list, s)
		}
	}
	m.mu.Unlock()
	out := make([]Snapshot, 0, len(list))
	for _, s := range list {
		out = append(out, m.overlayGuard(s.Snapshot()))
	}
	return out
}

// Reload applies a new parsed config (ADR-0006 hot reload). Definition changes
// to a running harness are staged (apply on next restart, SPEC-0003 REQ "Config
// Change Application"); new harnesses are added (stopped); removed harnesses are
// stopped and dropped. Project-registered harnesses are untouched: the global
// config is not their definition source, so a global reload never removes or
// re-defines them (ADR-0009; SPEC-0004 REQ "Project Naming And Namespacing").
func (m *Manager) Reload(newCfg *core.Config) {
	m.mu.Lock()
	old := m.supervisors
	m.cfg = newCfg
	// Re-evaluate the active profile against the new config (#99): a reload
	// may have introduced the profile back (resolving the flag) or removed it
	// (setting the flag). Either way, keep profileUnresolved in sync.
	if m.activeProfile != "" {
		_, ok := m.cfg.Profiles[m.activeProfile]
		m.profileUnresolved = !ok
	}
	// Stop + drop removed harnesses (global provenance only).
	var removed []*Supervisor
	newOrder := make([]string, 0, len(newCfg.HarnessOrder))
	for name := range old {
		if m.provenance[name] != "" {
			continue // project-owned; global reload never touches it
		}
		if _, ok := newCfg.Harnesses[name]; !ok {
			removed = append(removed, old[name])
			delete(old, name)
		}
	}
	// Apply changes / add new, preserving new config order.
	var toApply []struct {
		s *Supervisor
		h core.Harness
	}
	var toAdd []core.Harness
	for _, name := range newCfg.HarnessOrder {
		// Same provenance guard as the removal loop (defense-in-depth: the
		// config parser rejects "/" in global names, so this is only reachable
		// from a hand-built Config): a global definition must never clobber a
		// project-owned supervisor, nor duplicate its name in the order — the
		// project-preserve loop below already re-appends it (ADR-0009;
		// SPEC-0004 REQ "Project Naming And Namespacing").
		if m.provenance[name] != "" {
			continue
		}
		h := newCfg.Harnesses[name]
		newOrder = append(newOrder, name)
		if s, ok := old[name]; ok {
			toApply = append(toApply, struct {
				s *Supervisor
				h core.Harness
			}{s, h})
		} else {
			toAdd = append(toAdd, h)
		}
	}
	for _, h := range toAdd {
		m.addSupervisorLocked(h)
	}
	// Option A (issue #150): a harness newly introduced by this reload that
	// has config autostart membership gets runtime intent set and is started,
	// exactly as if the daemon had booted with it present. Pre-existing
	// harnesses keep their persisted intent — an explicit `harness stop` is
	// never undone by an unrelated reload (265e42a / 2dfa8fc invariant).
	autostart := autostartSet(newCfg)
	var newAutostart []*Supervisor
	for _, h := range toAdd {
		if autostart[h.Name] {
			if s := m.supervisors[h.Name]; s != nil {
				newAutostart = append(newAutostart, s)
			}
		}
	}
	// Keep project harnesses in the render order, after the globals, preserving
	// their existing relative order (ADR-0009: registration order).
	for _, name := range m.order {
		if m.provenance[name] != "" {
			newOrder = append(newOrder, name)
		}
	}
	m.order = newOrder
	m.mu.Unlock()

	for _, r := range removed {
		r.ShutdownFor(ReasonReload)
	}
	for _, a := range toApply {
		a.s.ApplyConfig(a.h)
	}
	// Start newly-introduced autostart harnesses outside the lock (Start blocks).
	// A gated one records intent true and begins held instead (ADR-0014,
	// SPEC-0012 REQ "Operating Hours Reload").
	for _, s := range newAutostart {
		startOrHold(s, TriggerAutostart)
	}
	m.markDirty()
	// Invoked outside m.mu: the hook (scheduler re-apply) reads Config(),
	// which takes the lock.
	if m.reloadHook != nil {
		m.reloadHook()
	}
}

// addSupervisorLocked adds a supervisor without appending order (callers
// manage order). Project harnesses registered here get the same persistence
// OnChange hook as global ones: their runtime intent is written to state.json
// (SPEC-0004 REQ "Registration Persistence"), so a restart restores the
// running set, not just the definitions.
func (m *Manager) addSupervisorLocked(h core.Harness) {
	s := New(h, Options{
		Policy:      m.policy,
		Bus:         m.bus,
		LogCfg:      m.logCfg,
		ExtraOut:    m.extraOut(h),
		OnChange:    m.markDirty,
		InitialSize: m.initialSizeFor(h.Name),
		Runs:        m,
		Admit:       m.admitRelease,
		Admission:   m,
	})
	m.supervisors[h.Name] = s
}

// addEphemeralSupervisorLocked is addSupervisorLocked for the scratch class
// (ADR-0017): identical supervision, but no persistence OnChange hook — a
// scratchpad never writes state.json, so there is nothing to mark dirty and
// every markDirty an exiting scratchpad fired would just rewrite a
// byte-identical file. Caller holds m.mu.
func (m *Manager) addEphemeralSupervisorLocked(h core.Harness) {
	s := New(h, Options{
		Policy:      m.policy,
		Bus:         m.bus,
		LogCfg:      m.logCfg,
		ExtraOut:    m.extraOut(h),
		InitialSize: m.initialSizeFor(h.Name),
		Admit:       m.admitRelease,
	})
	m.supervisors[h.Name] = s
}

// Close stops every harness, flushes final state, and tears down the manager.
//
// It is idempotent. The first call drains the run ledger (Ledger.Close), which
// is what puts a coalesced skip's buffered count on disk; a second call is a
// no-op rather than a panic, so a caller may close to read a drained ledger
// and still have a t.Cleanup close it again.
func (m *Manager) Close() {
	m.closeOnce.Do(func() {
		for _, s := range m.snapshotSupervisors() {
			s.Shutdown()
		}
		close(m.closed)
		m.wg.Wait()
		_ = m.Save() // final durable flush
		m.closeOpenRuns()
		if err := m.ledger.Close(ledgerCloseTimeout); err != nil {
			log.Error("run ledger did not drain at shutdown", "err", err)
		}
		// Last, after every supervisor has closed its runs and sealed their
		// logs: whatever is still queued stays plain, and the next boot's
		// sweep compresses it (sealLeftovers).
		m.sealer.Close()
	})
}

// ledgerCloseTimeout bounds the run ledger's drain at shutdown (SPEC-0022
// REQ-20): long enough for a queue of checkpoints and a sync, short enough
// that a dead disk cannot hold the daemon's exit hostage.
const ledgerCloseTimeout = 5 * time.Second

// Save writes the current runtime state to state.json immediately (ADR-0007).
// Project-registered harnesses are INCLUDED: a project registration is durable
// Compose-style state (SPEC-0004 REQ "Registration Persistence") — it survives
// daemon restarts until project_down or remove tears it down — so both their
// definitions (Projects) and their runtime intent (Harnesses, keyed by the
// fully-qualified name) are persisted.
func (m *Manager) Save() error {
	m.saveMu.Lock()
	defer m.saveMu.Unlock()
	// Everything Save needs — the active profile, the supervisor set, the
	// project definitions, and the schedule marks — is collected under ONE
	// lock hold, so a concurrent ProjectDown or UseProfile cannot interleave
	// between the snapshot pieces and smuggle a torn-down project (or a stale
	// profile) into state.json.
	m.mu.Lock()
	schedules := make(map[string]persistedSchedule, len(m.scheduleMarks))
	for name, mark := range m.scheduleMarks {
		// The scheduler forgets a disarmed entry's mark itself, but a harness
		// deleted from harness.toml while the daemon was down never had an
		// entry to forget; drop its mark here instead of carrying it forever.
		if _, ok := m.supervisors[name]; !ok {
			continue
		}
		ps := persistedSchedule{Spec: mark.Spec, DecidedThrough: mark.DecidedThrough}
		if !mark.LastRunAt.IsZero() {
			t := mark.LastRunAt
			ps.LastRunAt = &t
		}
		schedules[name] = ps
	}
	runs := m.persistedRunsLocked()
	activeProfile := m.activeProfile
	sups := make([]*Supervisor, 0, len(m.supervisors))
	for _, name := range m.order {
		if m.provenance[name] == ProvenanceScratch {
			continue // scratchpads are never persisted (ADR-0017, SPEC-0011)
		}
		if s, ok := m.supervisors[name]; ok {
			sups = append(sups, s)
		}
	}
	projects := make(map[string]persistedProject, len(m.projects))
	for name, rec := range m.projects {
		pp := persistedProject{Harnesses: make([]persistedProjectHarness, 0, len(rec.order))}
		for _, full := range rec.order {
			if h, ok := rec.harnesses[full]; ok {
				pp.Harnesses = append(pp.Harnesses, toPersistedProjectHarness(h))
			}
		}
		projects[name] = pp
	}
	// Leases snapshot under the same hold: the persist loop calls Save on its
	// own goroutine, and Stop/StartFor mutate the map from theirs.
	leases := make(map[string]time.Time, len(m.leases))
	for name, until := range m.leases {
		leases[name] = until
	}
	m.mu.Unlock()

	ps := persistedState{
		Version:       stateSchemaVersion,
		ActiveProfile: activeProfile,
		Harnesses:     map[string]persistedHarness{},
		Projects:      projects,
		Schedules:     schedules,
		Runs:          runs,
	}
	for _, s := range sups {
		snap := s.Snapshot()
		ph := persistedHarness{
			Enabled:         snap.Enabled,
			OperatorStopped: snap.OperatorStopped,
			State:           snap.State,
			RestartCount:    snap.RestartCount,
			LastExitCode:    snap.LastExitCode,
			Flapping:        snap.Flapping,
			Created:         snap.Created,
		}
		if !snap.LastExitAt.IsZero() {
			t := snap.LastExitAt
			ph.LastExitAt = &t
		}
		if !snap.LastStarted.IsZero() {
			t := snap.LastStarted
			ph.LastStarted = &t
		}
		// The last intent change (issue #835) persists with the rest of the
		// durable state, so describe keeps answering who flipped the intent
		// across a daemon restart.
		if !snap.LastIntent.At.IsZero() {
			t := snap.LastIntent.At
			ph.LastIntentAt = &t
			ph.LastIntentSource = snap.LastIntent.Source
			ph.LastIntentPeer = snap.LastIntent.Peer
		}
		// The after-hours lease rides with the rest of the durable state
		// (SPEC-0012 REQ "After-Hours Lease"): it is what lets a daemon
		// restarted mid-lease start the harness on boot and hold it at the
		// lease's end, not at some recomputed later moment.
		if until, ok := leases[snap.Name]; ok {
			t := until
			ph.LeaseUntil = &t
		}
		ps.Harnesses[snap.Name] = ph
	}
	return saveState(m.statePath, ps)
}

// ScheduleMark is a scheduled harness's durable scheduler position: every
// window at or before DecidedThrough has been decided (fired, caught up, or
// recorded missed). It mirrors scheduler.Mark field for field — the daemon
// converts between the two — so this package need not import the scheduler.
// Governing: ADR-0007, ADR-0013; SPEC-0008 REQ "Missed Window Handling".
type ScheduleMark struct {
	Spec           string
	DecidedThrough time.Time
	LastRunAt      time.Time
}

// LoadScheduleMark returns the persisted scheduler mark for name, if any.
func (m *Manager) LoadScheduleMark(name string) (ScheduleMark, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mark, ok := m.scheduleMarks[name]
	return mark, ok
}

// UpdateScheduleMarks drops forget, writes put, and flushes state.json before
// returning. Synchronous rather than debounced on purpose: the scheduler calls
// it before starting the run a mark accounts for, so a daemon that crashes
// right after a firing does not fire the same window again on restart.
func (m *Manager) UpdateScheduleMarks(put map[string]ScheduleMark, forget []string) error {
	m.mu.Lock()
	for _, name := range forget {
		delete(m.scheduleMarks, name)
	}
	for name, mark := range put {
		m.scheduleMarks[name] = mark
	}
	m.mu.Unlock()
	return m.Save()
}

// persistLoop debounces dirty signals into atomic state.json writes.
func (m *Manager) persistLoop() {
	defer m.wg.Done()
	var timer *time.Timer
	var timerC <-chan time.Time
	for {
		select {
		case <-m.dirty:
			if timer == nil {
				timer = time.NewTimer(persistDebounce)
				timerC = timer.C
			} else {
				timer.Reset(persistDebounce)
			}
		case <-timerC:
			_ = m.Save()
			timer = nil
			timerC = nil
		case <-m.closed:
			if timer != nil {
				timer.Stop()
			}
			return
		}
	}
}

// get returns the supervisor for name, or nil.
func (m *Manager) get(name string) *Supervisor {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.supervisors[name]
}

// HarnessCount returns how many harnesses are actually registered — globals
// plus live project harnesses, the same set Snapshots/list report (SPEC-0004;
// ADR-0009). The daemon projects it into daemon_info so the count matches
// what list shows.
func (m *Manager) HarnessCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.supervisors)
}

// dropFromOrderLocked rebuilds m.order once without the given names: a single
// linear filter instead of one scan-and-splice per name. Caller holds m.mu.
func (m *Manager) dropFromOrderLocked(names []string) {
	if len(names) == 0 {
		return
	}
	gone := make(map[string]bool, len(names))
	for _, n := range names {
		gone[n] = true
	}
	m.order = slices.DeleteFunc(m.order, func(n string) bool { return gone[n] })
}

// snapshotSupervisors returns a stable slice of the current supervisors.
func (m *Manager) snapshotSupervisors() []*Supervisor {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Supervisor, 0, len(m.supervisors))
	for _, name := range m.order {
		if s, ok := m.supervisors[name]; ok {
			out = append(out, s)
		}
	}
	return out
}

// autostartSet returns the set of harness names the config wants running on
// boot (ADR-0006 autostart profiles + per-harness enabled).
func autostartSet(cfg *core.Config) map[string]bool {
	set := make(map[string]bool)
	for _, name := range cfg.AutostartHarnesses() {
		set[name] = true
	}
	return set
}

// autostartProfileName returns the name of the first profile with
// autostart = true, or "" if none exists. Used as the fallback when the
// persisted active profile is missing from config (issue #99).
func (m *Manager) autostartProfileName() string {
	for _, name := range m.cfg.ProfileOrder {
		if m.cfg.Profiles[name].Autostart {
			return name
		}
	}
	return ""
}
