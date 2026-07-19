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
	"sync"
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
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
}

// Manager supervises every harness in a config.
type Manager struct {
	policy    Policy
	statePath string
	logCfg    LogConfig
	bus       *Bus

	mu            sync.Mutex
	cfg           *core.Config
	supervisors   map[string]*Supervisor
	order         []string
	activeProfile string

	dirty  chan struct{}
	closed chan struct{}
	wg     sync.WaitGroup
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

	m := &Manager{
		policy:      policy,
		statePath:   statePath,
		logCfg:      logCfg,
		bus:         NewBus(),
		cfg:         cfg,
		supervisors: make(map[string]*Supervisor),
		dirty:       make(chan struct{}, 1),
		closed:      make(chan struct{}),
	}
	for _, name := range cfg.HarnessOrder {
		m.addSupervisor(cfg.Harnesses[name])
	}
	m.wg.Add(1)
	go m.persistLoop()
	return m
}

// Events subscribes to the lifecycle event stream (SPEC-0003 REQ "Lifecycle
// Events"). The daemon later relays these over the control socket (SPEC-0002).
func (m *Manager) Events() (<-chan Event, func()) { return m.bus.Subscribe() }

// addSupervisor constructs and registers a supervisor for h. Caller holds no
// lock on first build; used under lock during reload.
func (m *Manager) addSupervisor(h core.Harness) {
	s := New(h, Options{
		Policy:   m.policy,
		Bus:      m.bus,
		LogCfg:   m.logCfg,
		OnChange: m.markDirty,
	})
	m.supervisors[h.Name] = s
	m.order = append(m.order, h.Name)
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
// autostart membership (ADR-0006) as their initial intent. Returns any
// state.json read/parse error (the caller may choose to proceed with defaults).
func (m *Manager) Restore() error {
	ps, err := loadState(m.statePath)
	if err != nil {
		return err
	}
	autostart := autostartSet(m.cfg)

	m.mu.Lock()
	m.activeProfile = ps.ActiveProfile
	sups := make(map[string]*Supervisor, len(m.supervisors))
	for k, v := range m.supervisors {
		sups[k] = v
	}
	m.mu.Unlock()

	for name, s := range sups {
		if pr, ok := ps.Harnesses[name]; ok {
			var last time.Time
			if pr.LastExitAt != nil {
				last = *pr.LastExitAt
			}
			s.Restore(pr.Enabled, pr.RestartCount, pr.LastExitCode, last)
		} else if autostart[name] {
			s.Restore(true, 0, 0, time.Time{})
		}
	}
	return nil
}

// Autostart starts every harness whose restored intent is enabled (SPEC-0003
// REQ "Autostart"). Safe to call once after Restore.
func (m *Manager) Autostart() {
	for _, s := range m.snapshotSupervisors() {
		if s.Snapshot().Enabled {
			s.Start()
		}
	}
}

// Start marks a single harness enabled and brings it up.
func (m *Manager) Start(name string) bool {
	if s := m.get(name); s != nil {
		s.Start()
		return true
	}
	return false
}

// Stop gracefully stops a single harness and clears its enabled intent.
func (m *Manager) Stop(name string) bool {
	if s := m.get(name); s != nil {
		s.Stop()
		return true
	}
	return false
}

// Restart restarts a single harness (clearing a failed latch).
func (m *Manager) Restart(name string) bool {
	if s := m.get(name); s != nil {
		s.Restart()
		return true
	}
	return false
}

// Snapshot returns one harness's runtime snapshot, ok=false if unknown.
func (m *Manager) Snapshot(name string) (Snapshot, bool) {
	if s := m.get(name); s != nil {
		return s.Snapshot(), true
	}
	return Snapshot{}, false
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
		out = append(out, s.Snapshot())
	}
	return out
}

// Reload applies a new parsed config (ADR-0006 hot reload). Definition changes
// to a running harness are staged (apply on next restart, SPEC-0003 REQ "Config
// Change Application"); new harnesses are added (stopped); removed harnesses are
// stopped and dropped.
func (m *Manager) Reload(newCfg *core.Config) {
	m.mu.Lock()
	old := m.supervisors
	m.cfg = newCfg
	// Stop + drop removed harnesses.
	var removed []*Supervisor
	newOrder := make([]string, 0, len(newCfg.HarnessOrder))
	for name := range old {
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
	m.order = newOrder
	m.mu.Unlock()

	for _, r := range removed {
		r.Shutdown()
	}
	for _, a := range toApply {
		a.s.ApplyConfig(a.h)
	}
	m.markDirty()
}

// addSupervisorLocked adds a supervisor without appending order (Reload rebuilds
// order). Caller holds m.mu.
func (m *Manager) addSupervisorLocked(h core.Harness) {
	s := New(h, Options{
		Policy:   m.policy,
		Bus:      m.bus,
		LogCfg:   m.logCfg,
		OnChange: m.markDirty,
	})
	m.supervisors[h.Name] = s
}

// Close stops every harness, flushes final state, and tears down the manager.
func (m *Manager) Close() {
	for _, s := range m.snapshotSupervisors() {
		s.Shutdown()
	}
	close(m.closed)
	m.wg.Wait()
	_ = m.Save() // final durable flush
}

// Save writes the current runtime state to state.json immediately (ADR-0007).
func (m *Manager) Save() error {
	ps := persistedState{
		Version:       stateSchemaVersion,
		ActiveProfile: m.activeProfile,
		Harnesses:     map[string]persistedHarness{},
	}
	for _, s := range m.snapshotSupervisors() {
		snap := s.Snapshot()
		ph := persistedHarness{
			Enabled:      snap.Enabled,
			State:        snap.State,
			RestartCount: snap.RestartCount,
			LastExitCode: snap.LastExitCode,
			Flapping:     snap.Flapping,
			Created:      snap.Created,
		}
		if !snap.LastExitAt.IsZero() {
			t := snap.LastExitAt
			ph.LastExitAt = &t
		}
		if !snap.LastStarted.IsZero() {
			t := snap.LastStarted
			ph.LastStarted = &t
		}
		ps.Harnesses[snap.Name] = ph
	}
	return saveState(m.statePath, ps)
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
