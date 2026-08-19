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
	"io"
	"slices"
	"sync"
	"syscall"
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

	// projects tracks every registered (ephemeral) project by name, and
	// provenance maps a registered harness's full name to its owning project
	// ("" / absent = global config) so `down`/`ps` scope correctly and a global
	// reload never touches project harnesses. Governing: ADR-0009
	// (project-scoped compose), SPEC-0004 REQ "Project Naming And Namespacing".
	projects   map[string]*projectRecord
	provenance map[string]string

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
		policy:       policy,
		statePath:    statePath,
		logCfg:       logCfg,
		bus:          NewBus(),
		extraOutFor:  opts.ExtraOutFor,
		dropExtraOut: opts.DropExtraOut,
		sizeFor:      opts.SizeFor,
		cfg:          cfg,
		supervisors:  make(map[string]*Supervisor),
		projects:     make(map[string]*projectRecord),
		provenance:   make(map[string]string),
		dirty:        make(chan struct{}, 1),
		closed:       make(chan struct{}),
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

// addSupervisor constructs and registers a global-config supervisor for h,
// appending it to the render order. Only called single-threaded from
// NewManager.
func (m *Manager) addSupervisor(h core.Harness) {
	m.addSupervisorLocked(h, false)
	m.order = append(m.order, h.Name)
}

// extraOut resolves the per-harness tee writer (the attach emulator/ring) from
// the configured factory, or nil when none is set.
func (m *Manager) extraOut(name string) io.Writer {
	if m.extraOutFor == nil {
		return nil
	}
	return m.extraOutFor(name)
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
		return err
	}
	autostart := autostartSet(m.cfg)

	m.mu.Lock()
	m.activeProfile = ps.ActiveProfile

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
	for name, s := range sups {
		pr, inState := ps.Harnesses[name]
		var last time.Time
		if inState && pr.LastExitAt != nil {
			last = *pr.LastExitAt
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
		switch {
		case unresolved && hasAutostartProfile && autostart[name]:
			// The persisted profile is gone, so the persisted per-harness
			// intent it produced cannot be trusted either — a member recorded
			// as disabled was most likely disabled BY that profile, not by the
			// operator. Autostart membership wins, which is the whole point of
			// the fallback: the daemon must not come up having started nothing.
			// Counters are preserved so restart history is not lost (#99).
			s.Restore(true, pr.RestartCount, pr.LastExitCode, last)
		case inState:
			s.Restore(pr.Enabled, pr.RestartCount, pr.LastExitCode, last)
		case autostart[name]:
			s.Restore(true, 0, 0, time.Time{})
		}
	}

	// Stable order so the warning and doctor row don't reshuffle between boots
	// (sups is a map).
	slices.Sort(dormant)
	m.mu.Lock()
	m.dormantAutostart = dormant
	m.mu.Unlock()
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
		// Starting it IS the fix for a dormant autostart member, so stop
		// reporting it — otherwise boot's warning and doctor's row outlive the
		// condition they describe, until the next restore.
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

// Enable sets a harness's enabled intent and starts it if stopped.
func (m *Manager) Enable(name string) bool {
	if s := m.get(name); s != nil {
		s.Start()
		return true
	}
	return false
}

// Disable clears a harness's enabled intent and stops it if running.
func (m *Manager) Disable(name string) bool {
	if s := m.get(name); s != nil {
		s.Stop()
		return true
	}
	return false
}

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
			s.Start()
		}
	}
	m.markDirty()
	return true
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
		m.addSupervisorLocked(h, false)
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
		r.Shutdown()
	}
	for _, a := range toApply {
		a.s.ApplyConfig(a.h)
	}
	// Start newly-introduced autostart harnesses outside the lock (Start blocks).
	for _, s := range newAutostart {
		s.Start()
	}
	m.markDirty()
	// Invoked outside m.mu: the hook (scheduler re-apply) reads Config(),
	// which takes the lock.
	if m.reloadHook != nil {
		m.reloadHook()
	}
}

// addSupervisorLocked adds a supervisor without appending order (callers
// manage order). ephemeral marks a project-registered harness (ADR-0009): it
// gets no persistence OnChange hook because project harnesses are never
// written to state.json — every markDirty a crash-looping project harness
// fired would just rewrite a byte-identical file. Lifecycle events still flow
// through the shared Bus regardless. Caller holds m.mu (or is the
// single-threaded NewManager construction path).
func (m *Manager) addSupervisorLocked(h core.Harness, ephemeral bool) {
	onChange := m.markDirty
	if ephemeral {
		onChange = nil
	}
	s := New(h, Options{
		Policy:      m.policy,
		Bus:         m.bus,
		LogCfg:      m.logCfg,
		ExtraOut:    m.extraOut(h.Name),
		OnChange:    onChange,
		InitialSize: m.initialSizeFor(h.Name),
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
// Project-registered harnesses are excluded: they are ephemeral, live-and-die
// with project_up/project_down, and are not restored across a daemon restart
// (ADR-0009 non-goal; SPEC-0004).
func (m *Manager) Save() error {
	// Everything Save needs — the active profile and the provenance-filtered
	// supervisor set — is collected under ONE lock hold, so a concurrent
	// ProjectDown or UseProfile cannot interleave between the snapshot and the
	// filter and smuggle an ephemeral project harness (or a stale profile)
	// into state.json.
	m.mu.Lock()
	activeProfile := m.activeProfile
	sups := make([]*Supervisor, 0, len(m.supervisors))
	for _, name := range m.order {
		if m.provenance[name] != "" {
			continue // ephemeral project harness (ADR-0009)
		}
		if s, ok := m.supervisors[name]; ok {
			sups = append(sups, s)
		}
	}
	m.mu.Unlock()

	ps := persistedState{
		Version:       stateSchemaVersion,
		ActiveProfile: activeProfile,
		Harnesses:     map[string]persistedHarness{},
	}
	for _, s := range sups {
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
