package supervisor

// Governing: SPEC-0003 (harness-lifecycle) — the full seven-state machine with
// `enabled` orthogonal to `state`, autostart, restart-on-exit (incl. clean
// exit), crash-loop detection with capped-exponential backoff → degraded →
// failed, manual restart clearing failed, and graceful stop; ADR-0005 (the
// daemon supervises each harness with one in-process goroutine that owns its
// state). All mutation happens on a single goroutine (the actor loop); external
// callers interact only through commands and a mutex-guarded Snapshot, so the
// concurrency is race-free by construction.

import (
	"io"
	"sync"
	"syscall"
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
)

// exitResult carries a finished process's outcome from the waiter goroutine to
// the loop.
type exitResult struct {
	gen  uint64 // run generation, so a stale waiter can be ignored
	code int
}

// cmdKind enumerates the operator/daemon requests the loop serializes.
type cmdKind int

const (
	cmdStart cmdKind = iota
	cmdStop
	cmdRestart
	cmdApplyConfig
	cmdRestore
	cmdShutdown
)

// restoreData seeds persisted intent + counters on daemon start (ADR-0007).
type restoreData struct {
	enabled      bool
	restartCount int
	lastExitCode int
	lastExitAt   time.Time
}

// command is one request to the loop, with a done channel the caller waits on
// so start/stop/restart are synchronous from the caller's perspective.
type command struct {
	kind    cmdKind
	cfg     *core.Harness
	restore *restoreData
	done    chan struct{}
}

// Snapshot is a race-free copy of a harness's observable runtime state, for the
// TUI/daemon to render. `Enabled` is intent; `State` is reality (SPEC-0003 REQ
// "State Model").
type Snapshot struct {
	Name          string
	State         core.State
	Enabled       bool
	RestartCount  int // ↻ total restarts
	LastExitCode  int
	LastExitAt    time.Time
	Flapping      bool
	NextRetryIn   time.Duration
	ConfigChanged bool // "config changed — restart to apply" (SPEC-0003)
	Created       time.Time
	LastStarted   time.Time
	PID           int
}

// Supervisor owns the lifecycle of exactly one harness. It runs a single actor
// goroutine (loop) that is the sole mutator of all state.
type Supervisor struct {
	policy   Policy
	bus      *Bus
	logCfg   LogConfig
	extraOut io.Writer // optional tee target (e.g. the future emulator ring)
	onChange func()    // called after any observable change (persist hook)

	cmds      chan command
	exitCh    chan exitResult
	timerCh   chan struct{} // restart timer fired
	surviveCh chan uint64   // survival timer fired (carries run gen)
	done      chan struct{}

	// ---- loop-owned state (touch only from loop) ----
	harness core.Harness
	pending *core.Harness // config change awaiting next (re)start
	state   core.State
	enabled bool

	proc *process
	gen  uint64 // increments each spawn; guards stale waiter/survive events

	restartCount int
	crashTimes   []time.Time // exit timestamps within the crash window
	flapAttempts int         // consecutive flapping restarts (drives backoff/give-up)
	flapping     bool
	startedAt    time.Time
	lastExitCode int
	lastExitAt   time.Time
	created      time.Time
	lastStarted  time.Time
	nextRetryIn  time.Duration

	restartTimer  *time.Timer
	log           *rotatingLog
	configChanged bool // staged config awaiting restart (SPEC-0003)

	// ---- snapshot (guarded) ----
	mu   sync.Mutex
	snap Snapshot
}

// Options configure a Supervisor.
type Options struct {
	Policy   Policy
	Bus      *Bus
	LogCfg   LogConfig
	ExtraOut io.Writer // optional additional tee target for PTY output
	OnChange func()    // invoked after observable state changes (for persistence)
}

// New creates a Supervisor for h and starts its actor loop. The harness begins
// `stopped` and disabled; call Start (or Manager autostart) to bring it up.
func New(h core.Harness, opts Options) *Supervisor {
	now := time.Now()
	s := &Supervisor{
		policy:       opts.Policy.normalize(),
		bus:          opts.Bus,
		logCfg:       opts.LogCfg,
		extraOut:     opts.ExtraOut,
		onChange:     opts.OnChange,
		cmds:         make(chan command),
		exitCh:       make(chan exitResult, 1),
		timerCh:      make(chan struct{}, 1),
		surviveCh:    make(chan uint64, 1),
		done:         make(chan struct{}),
		harness:      h,
		state:        core.StateStopped,
		created:      now,
		lastExitCode: 0,
	}
	s.snap = Snapshot{Name: h.Name, State: core.StateStopped, Created: now}
	go s.loop()
	return s
}

// Name returns the harness name.
func (s *Supervisor) Name() string { return s.harness.Name }

// Snapshot returns a race-free copy of the current observable state.
func (s *Supervisor) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snap
}

// Start marks the harness enabled and brings it up (SPEC-0003 REQ "Autostart"
// path and manual start). Blocks until the request is processed.
func (s *Supervisor) Start() { s.send(command{kind: cmdStart}) }

// Stop performs a graceful stop and sets enabled=false (SPEC-0003 REQ
// "Graceful Stop"). Blocks until the harness is stopped.
func (s *Supervisor) Stop() { s.send(command{kind: cmdStop}) }

// Restart clears any failed latch, resets crash-loop state, and begins a fresh
// start cycle (SPEC-0003 REQ "Backoff Give-Up" manual recovery). Blocks until
// processed.
func (s *Supervisor) Restart() { s.send(command{kind: cmdRestart}) }

// ApplyConfig stages a new definition. If the process is running it is left
// untouched and the change is flagged to apply on next (re)start (SPEC-0003 REQ
// "Config Change Application"); if not running it takes effect immediately.
func (s *Supervisor) ApplyConfig(h core.Harness) { s.send(command{kind: cmdApplyConfig, cfg: &h}) }

// Shutdown stops the harness if running and terminates the actor loop. After
// Shutdown the Supervisor must not be used.
func (s *Supervisor) Shutdown() {
	s.send(command{kind: cmdShutdown})
	<-s.done
}

// send delivers a command and waits for the loop to finish handling it.
func (s *Supervisor) send(c command) {
	c.done = make(chan struct{})
	select {
	case s.cmds <- c:
		<-c.done
	case <-s.done:
	}
}

// loop is the actor: the single goroutine that owns and mutates all harness
// state. Every other method just feeds it channels.
func (s *Supervisor) loop() {
	defer close(s.done)
	for {
		select {
		case c := <-s.cmds:
			if s.handleCommand(c) {
				close(c.done)
				return // shutdown
			}
			close(c.done)
		case ex := <-s.exitCh:
			s.handleExit(ex)
		case <-s.timerCh:
			s.handleRestartTimer()
		case gen := <-s.surviveCh:
			s.handleSurvival(gen)
		}
	}
}

// handleCommand dispatches an operator/daemon request. Returns true on
// shutdown.
func (s *Supervisor) handleCommand(c command) (shutdown bool) {
	switch c.kind {
	case cmdStart:
		s.enabled = true
		s.publishChangeUnchanged() // persist intent even if already up
		if !s.hasProcess() && s.state != core.StateStopping {
			s.clearFailLatch()
			s.beginStart()
		}
	case cmdStop:
		s.enabled = false
		s.cancelRestartTimer()
		if s.hasProcess() {
			s.gracefulStop()
		} else if s.state != core.StateFailed {
			s.transition(core.StateStopped)
		} else {
			// Failed with no process: honor intent, keep the failed marker.
			s.publishChangeUnchanged()
		}
	case cmdRestart:
		s.enabled = true
		s.cancelRestartTimer()
		s.clearFailLatch()
		s.resetCrashState()
		if s.hasProcess() {
			s.gracefulStopKeepEnabled()
		}
		s.beginStart()
	case cmdApplyConfig:
		s.applyConfig(*c.cfg)
	case cmdRestore:
		r := c.restore
		s.enabled = r.enabled
		s.restartCount = r.restartCount
		s.lastExitCode = r.lastExitCode
		s.lastExitAt = r.lastExitAt
		s.publishSnapshot()
	case cmdShutdown:
		s.cancelRestartTimer()
		if s.hasProcess() {
			s.enabled = false
			s.gracefulStop()
		}
		s.closeLog()
		return true
	}
	return false
}

// ---- state transitions ---------------------------------------------------

// transition moves to next (a legal SPEC-0003 edge), emits
// harness_state_changed, and republishes the snapshot.
func (s *Supervisor) transition(next core.State) {
	from := s.state
	if from == next {
		s.publishSnapshot()
		return
	}
	if !from.CanTransitionTo(next) {
		// Should never happen given the flows below; guard defensively rather
		// than corrupt the machine.
		return
	}
	s.state = next
	if s.bus != nil {
		s.bus.Publish(Event{Kind: EventStateChanged, Name: s.harness.Name, Time: time.Now(), From: from, To: next})
	}
	s.publishSnapshot()
}

// beginStart spawns the process and moves stopped/failed/restarting/degraded →
// starting → running. On spawn failure it routes into the restart/give-up path.
func (s *Supervisor) beginStart() {
	// Adopt any staged config change now (SPEC-0003 REQ "Config Change
	// Application": applies on next (re)start).
	if s.pending != nil {
		s.harness = *s.pending
		s.pending = nil
		s.configChanged = false
	}
	s.transition(core.StateStarting)

	proc, err := spawn(s.harness)
	if err != nil {
		// Treat a spawn failure like an immediate crash.
		s.onProcessGone(-1, err != nil)
		return
	}
	s.proc = proc
	s.gen++
	gen := s.gen
	s.startedAt = time.Now()
	s.lastStarted = s.startedAt

	// Ensure a log sink exists and tee raw PTY output to it (ADR-0007).
	s.ensureLog()
	var sink io.Writer = s.log
	if s.extraOut != nil {
		sink = io.MultiWriter(s.log, s.extraOut)
	}
	go s.readOutput(proc, sink)
	go s.wait(proc, gen)

	s.transition(core.StateRunning)

	// Arm the survival timer: a run that outlives the crash window clears the
	// flap history (SPEC-0003 REQ "Crash-Loop Detection" recovery).
	s.armSurvival(gen)
}

// readOutput copies the raw PTY stream to the log/tee until the PTY closes.
func (s *Supervisor) readOutput(proc *process, sink io.Writer) {
	_, _ = io.Copy(sink, proc.pty)
}

// wait reaps the process and reports its exit to the loop.
func (s *Supervisor) wait(proc *process, gen uint64) {
	_ = proc.cmd.Wait()
	code := -1
	if proc.cmd.ProcessState != nil {
		code = proc.cmd.ProcessState.ExitCode()
	}
	select {
	case s.exitCh <- exitResult{gen: gen, code: code}:
	case <-s.done:
	}
}

// handleExit processes a natural (unsolicited) process exit.
func (s *Supervisor) handleExit(ex exitResult) {
	if ex.gen != s.gen || s.proc == nil {
		return // stale exit from a run we already tore down (e.g. during stop)
	}
	s.reapProcess()
	s.onProcessGone(ex.code, false)
}

// onProcessGone is the shared exit handler for both a real exit and a spawn
// failure. code is the exit status (-1 if signalled/failed); spawnFailed marks
// a process that never came up.
func (s *Supervisor) onProcessGone(code int, spawnFailed bool) {
	now := time.Now()
	s.lastExitCode = code
	s.lastExitAt = now
	if s.bus != nil {
		s.bus.Publish(Event{Kind: EventExited, Name: s.harness.Name, Time: now, Code: code})
	}

	// Exit while disabled → stopped, no respawn (SPEC-0003 REQ "Restart On
	// Exit" / "Intent vs. reality").
	if !s.enabled {
		s.transition(core.StateStopped)
		return
	}

	// Determine whether the just-ended run survived the crash window; if so the
	// counter resets before we count this exit.
	if !spawnFailed && !s.startedAt.IsZero() && now.Sub(s.startedAt) > s.policy.CrashWindow {
		s.resetCrashState()
	}

	// Record this exit and evaluate crash-loop status.
	s.crashTimes = append(s.crashTimes, now)
	s.trimCrashTimes(now)
	s.restartCount++ // ↻

	loop := len(s.crashTimes) >= s.policy.CrashThreshold
	if loop {
		s.flapping = true
		s.flapAttempts++
	}

	// Give-up: exhausted flapping restart attempts → failed (SPEC-0003 REQ
	// "Backoff Give-Up").
	if s.flapping && s.policy.MaxRestarts > 0 && s.flapAttempts > s.policy.MaxRestarts {
		if s.state == core.StateRunning {
			s.transition(core.StateDegraded)
		}
		s.transition(core.StateFailed)
		s.nextRetryIn = 0
		s.publishSnapshot()
		return
	}

	// Compute the restart delay: capped-exponential backoff while flapping,
	// otherwise the harness's base restart_delay.
	var delay time.Duration
	if s.flapping {
		delay = s.policy.backoff(s.flapAttempts)
		// Enter/stay degraded during the flapping wait.
		if s.state == core.StateRunning || s.state == core.StateStarting {
			// starting→degraded is not a legal edge; route spawn-failure
			// flapping through restarting instead.
			if s.state == core.StateStarting {
				s.transition(core.StateRestarting)
			} else {
				s.transition(core.StateDegraded)
			}
		}
		s.nextRetryIn = delay
		if s.bus != nil {
			s.bus.Publish(Event{Kind: EventFlapping, Name: s.harness.Name, Time: now, Restarts: s.restartCount, NextRetryIn: delay})
		}
	} else {
		delay = s.harness.RestartDelay
		s.transition(core.StateRestarting)
		s.nextRetryIn = delay
	}
	s.publishSnapshot()
	s.armRestart(delay)
}

// handleRestartTimer fires a pending restart after the delay elapsed.
func (s *Supervisor) handleRestartTimer() {
	if !s.enabled || s.hasProcess() {
		return
	}
	if s.state != core.StateRestarting && s.state != core.StateDegraded {
		return
	}
	// degraded/restarting → (starting → running) via beginStart; degraded is
	// routed degraded→restarting→starting per the SPEC-0003 transition table.
	if s.state == core.StateDegraded {
		s.transition(core.StateRestarting)
	}
	s.beginStart()
}

// handleSurvival resets crash-loop state once a run outlives the crash window.
func (s *Supervisor) handleSurvival(gen uint64) {
	if gen != s.gen || !s.hasProcess() {
		return // stale timer from an earlier run
	}
	s.resetCrashState()
	if s.state == core.StateDegraded {
		s.transition(core.StateRunning) // recovery (SPEC-0003 degraded→running)
	}
	s.publishSnapshot()
}

// ---- graceful stop -------------------------------------------------------

// gracefulStop runs the SPEC-0003 REQ "Graceful Stop" sequence: SIGTERM → grace
// → SIGKILL if needed → PTY teardown → stopped. enabled is left as the caller
// set it (Stop sets false; internal callers may keep it).
func (s *Supervisor) gracefulStop() {
	if !s.hasProcess() {
		if s.state != core.StateStopped && s.state.CanTransitionTo(core.StateStopped) {
			s.transition(core.StateStopped)
		}
		return
	}
	s.transition(core.StateStopping)
	proc := s.proc
	gen := s.gen

	proc.signalGroup(syscall.SIGTERM)
	select {
	case ex := <-s.exitCh:
		if ex.gen == gen {
			s.lastExitCode = ex.code
			s.lastExitAt = time.Now()
		}
	case <-time.After(s.policy.StopGrace):
		// Still alive after the grace period → SIGKILL, then reap.
		proc.signalGroup(syscall.SIGKILL)
		ex := <-s.exitCh
		if ex.gen == gen {
			s.lastExitCode = ex.code
			s.lastExitAt = time.Now()
		}
	}
	s.reapProcess()
	s.transition(core.StateStopped)
}

// gracefulStopKeepEnabled stops the running process for a manual restart
// without changing intent (enabled stays true).
func (s *Supervisor) gracefulStopKeepEnabled() {
	keep := s.enabled
	s.enabled = false // suppress restart-on-exit during the intentional stop
	s.gracefulStop()
	s.enabled = keep
}

// ---- helpers -------------------------------------------------------------

func (s *Supervisor) hasProcess() bool { return s.proc != nil }

// reapProcess closes the PTY (unblocking the reader) and drops the process.
func (s *Supervisor) reapProcess() {
	if s.proc != nil {
		_ = s.proc.pty.Close()
		s.proc = nil
	}
}

// trimCrashTimes drops exit timestamps older than the crash window.
func (s *Supervisor) trimCrashTimes(now time.Time) {
	cutoff := now.Add(-s.policy.CrashWindow)
	i := 0
	for i < len(s.crashTimes) && s.crashTimes[i].Before(cutoff) {
		i++
	}
	if i > 0 {
		s.crashTimes = append(s.crashTimes[:0], s.crashTimes[i:]...)
	}
}

// resetCrashState clears flap counters (recovery / manual restart).
func (s *Supervisor) resetCrashState() {
	s.crashTimes = nil
	s.flapAttempts = 0
	s.flapping = false
	s.nextRetryIn = 0
}

// clearFailLatch releases a failed harness so it can start again.
func (s *Supervisor) clearFailLatch() {
	if s.state == core.StateFailed {
		// failed→starting is the only legal edge out; beginStart makes that
		// move. Nothing to do here but reset crash bookkeeping.
		s.resetCrashState()
	}
}

// armRestart schedules a restart after delay, delivering to timerCh.
func (s *Supervisor) armRestart(delay time.Duration) {
	s.cancelRestartTimer()
	if delay <= 0 {
		// Fire immediately but still via the channel so the loop stays the sole
		// mutator.
		select {
		case s.timerCh <- struct{}{}:
		default:
		}
		return
	}
	s.restartTimer = time.AfterFunc(delay, func() {
		select {
		case s.timerCh <- struct{}{}:
		case <-s.done:
		}
	})
}

// cancelRestartTimer stops a pending restart timer and drains a stale tick.
func (s *Supervisor) cancelRestartTimer() {
	if s.restartTimer != nil {
		s.restartTimer.Stop()
		s.restartTimer = nil
	}
	select {
	case <-s.timerCh:
	default:
	}
}

// armSurvival schedules the crash-window survival reset for run gen.
func (s *Supervisor) armSurvival(gen uint64) {
	time.AfterFunc(s.policy.CrashWindow, func() {
		select {
		case s.surviveCh <- gen:
		case <-s.done:
		}
	})
}

// applyConfig implements SPEC-0003 REQ "Config Change Application".
func (s *Supervisor) applyConfig(h core.Harness) {
	if s.hasProcess() && runAffecting(s.harness, h) {
		// Running: leave the process untouched, flag "restart to apply".
		s.pending = &h
		s.configChanged = true
		s.publishSnapshot()
		return
	}
	// Not running (or a no-op change): apply immediately.
	s.harness = h
	s.pending = nil
	s.configChanged = false
	s.publishSnapshot()
}

// runAffecting reports whether a config change would alter how the process
// runs (and thus needs a restart to take effect). Cosmetic fields
// (description, enabled intent) do not count.
func runAffecting(a, b core.Harness) bool {
	if a.Cmd != b.Cmd || a.Workdir != b.Workdir || a.EnvFile != b.EnvFile ||
		a.RestartDelay != b.RestartDelay || a.Backend != b.Backend || a.TmuxSocket != b.TmuxSocket {
		return true
	}
	if len(a.Args) != len(b.Args) {
		return true
	}
	for i := range a.Args {
		if a.Args[i] != b.Args[i] {
			return true
		}
	}
	return false
}

// ensureLog lazily opens the rotating log for this harness.
func (s *Supervisor) ensureLog() {
	if s.log != nil {
		return
	}
	if s.logCfg.Dir == "" {
		return
	}
	if rl, err := newRotatingLog(s.harness.Name, s.logCfg); err == nil {
		s.log = rl
	}
}

func (s *Supervisor) closeLog() {
	if s.log != nil {
		_ = s.log.Close()
		s.log = nil
	}
}

// publishSnapshot refreshes the guarded snapshot from loop-owned state and
// notifies the persist hook.
func (s *Supervisor) publishSnapshot() {
	pid := 0
	if s.proc != nil {
		pid = s.proc.pid
	}
	s.mu.Lock()
	s.snap = Snapshot{
		Name:          s.harness.Name,
		State:         s.state,
		Enabled:       s.enabled,
		RestartCount:  s.restartCount,
		LastExitCode:  s.lastExitCode,
		LastExitAt:    s.lastExitAt,
		Flapping:      s.flapping,
		NextRetryIn:   s.nextRetryIn,
		ConfigChanged: s.configChanged,
		Created:       s.created,
		LastStarted:   s.lastStarted,
		PID:           pid,
	}
	s.mu.Unlock()
	if s.onChange != nil {
		s.onChange()
	}
}

// publishChangeUnchanged refreshes the snapshot after an intent-only change
// (e.g. enabling an already-running harness) without a state transition.
func (s *Supervisor) publishChangeUnchanged() { s.publishSnapshot() }

// Restore seeds persisted intent + counters (ADR-0007) before the harness is
// started. Call immediately after New, before Start/Autostart.
func (s *Supervisor) Restore(enabled bool, restartCount, lastExitCode int, lastExitAt time.Time) {
	s.send(command{kind: cmdRestore, restore: &restoreData{
		enabled:      enabled,
		restartCount: restartCount,
		lastExitCode: lastExitCode,
		lastExitAt:   lastExitAt,
	}})
}
