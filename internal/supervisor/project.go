package supervisor

// Governing: ADR-0009 (project-scoped config and compose commands), SPEC-0004
// REQ "Project Naming And Namespacing", REQ "Project Control Operations",
// and REQ "Registration Persistence".
//
// A project is a daemon-managed set of harnesses registered under the
// `<project>/<harness>` namespace by project_up and torn down by
// project_down (whole project) or remove (single member). Registrations are
// Compose-style durable state: they persist in state.json and are re-registered
// on daemon boot, staying up until an explicit tear-down — they are NOT
// ad-hoc. The Manager tracks per-harness provenance (global config vs.
// project:NAME) so tear-down and scoping never touch global harnesses, and a
// global reload never touches project ones. ProjectUp validates everything up
// front and mutates the registry atomically under the manager lock, so a
// failure registers nothing (SPEC-0004 REQ "Error Handling Standards": no
// partially-registered project).

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"gitea.stump.rocks/stump.wtf/harness/internal/config"
	"gitea.stump.rocks/stump.wtf/harness/internal/core"
)

// ErrInvalidProjectDef is the sentinel for a project_up carrying an invalid
// project name or harness definition (empty/namespaced names, missing cmd,
// duplicate locals, bad backend). Callers branch with errors.Is; the daemon
// maps it to the invalid_project wire code (SPEC-0004 REQ "Error Handling
// Standards").
var ErrInvalidProjectDef = errors.New("invalid project definition")

// projectRecord is the Manager's book-keeping for one registered project: the
// definitions it registered (keyed and named by full `<project>/<local>` name)
// in project-file order. Provenance for scoping lives in Manager.provenance.
type projectRecord struct {
	name string
	// harnesses maps full (namespaced) harness name → its registered definition.
	harnesses map[string]core.Harness
	// order is the full names in project-file order.
	order []string
}

// applyPair stages a definition change onto an already-registered supervisor.
type applyPair struct {
	s *Supervisor
	h core.Harness
}

// ProjectUpResult reports what a ProjectUp reconcile did. Names holds the
// project's fully-qualified harness names in project-file order — computed
// under the same lock hold as the registration itself, so the daemon can build
// its project_up reply from state a concurrent project_down cannot tear.
// Changed reports whether the reconcile actually altered the registry (added,
// removed, or re-defined anything); a verbatim no-op re-up leaves it false so
// callers can skip change broadcasts (SPEC-0004 REQ "Bring Up" idempotency).
type ProjectUpResult struct {
	Names   []string
	Changed bool
}

// ProjectUp registers project's harnesses daemon-wide as `<project>/<local>`
// and starts the newly-added enabled ones (SPEC-0004 REQ "Project Control
// Operations"). It is idempotent in the reconcile sense (SPEC-0004 REQ "Bring
// Up"): a re-up adds new harnesses, stops + deregisters removed ones, and
// stages changed definitions to apply on next restart per SPEC-0003 — a
// running process is never silently bounced, and continuing harnesses are
// otherwise untouched. A definition with Enabled=false registers without
// starting, mirroring how the global config's `enabled` gates autostart
// (SPEC-0004 REQ "Project File Schema": identical field meanings).
//
// Validation happens entirely up front and registry mutation is atomic under
// the manager lock: on any error (ErrInvalidProjectDef, or a project name that
// would shadow a bare global harness → config.ErrProjectNameCollision) nothing
// is registered.
func (m *Manager) ProjectUp(project string, defs []core.Harness) (ProjectUpResult, error) {
	// ---- validate up front: no partial state on failure (SPEC-0004) ----
	if err := validateProjectDefs(project, defs); err != nil {
		return ProjectUpResult{}, err
	}

	m.mu.Lock()
	rec, registered := m.projects[project]
	if !registered {
		// A NEW project's name must not shadow an existing bare global harness
		// name (SPEC-0004 REQ "Project Naming And Namespacing" scenario "Name
		// collides with a global harness"). Checked only at first registration:
		// a global harness of that name added later (via reload) must never
		// wedge reconcile of an already-registered project.
		if _, exists := m.cfg.Harnesses[project]; exists {
			m.mu.Unlock()
			return ProjectUpResult{}, fmt.Errorf("project up %q: %w: a global harness named %q exists",
				project, config.ErrProjectNameCollision, project)
		}
		rec = &projectRecord{name: project, harnesses: map[string]core.Harness{}}
	}

	// Build the desired set under fully-qualified names.
	desired := make(map[string]core.Harness, len(defs))
	desiredOrder := make([]string, 0, len(defs))
	for _, h := range defs {
		full := core.QualifiedName(project, h.Name)
		h.Name = full
		desired[full] = h
		desiredOrder = append(desiredOrder, full)
	}

	// A fully-qualified name may never clobber a supervisor this project does
	// not own (defensive: only reachable if a global name contains "/").
	for full := range desired {
		if _, taken := m.supervisors[full]; taken {
			if _, ours := rec.harnesses[full]; !ours {
				m.mu.Unlock()
				return ProjectUpResult{}, fmt.Errorf("project up %q: harness %q: %w: name already registered",
					project, full, config.ErrProjectNameCollision)
			}
		}
	}

	// ---- reconcile (all registry mutation under one lock hold) ----
	// Removed: registered before, absent now → stop + deregister.
	var removed []*Supervisor
	var removedNames []string
	for full := range rec.harnesses {
		if _, keep := desired[full]; keep {
			continue
		}
		if s := m.supervisors[full]; s != nil {
			removed = append(removed, s)
		}
		delete(m.supervisors, full)
		delete(m.provenance, full)
		removedNames = append(removedNames, full)
	}
	m.dropFromOrderLocked(removedNames)
	// Continuing: stage definition changes (SPEC-0003 REQ "Config Change
	// Application" — applied on next restart), skipping verbatim no-ops so an
	// identical re-up changes nothing.
	// New: register, and start only the enabled ones.
	changed := len(removedNames) > 0
	var toApply []applyPair
	var toStart []*Supervisor
	for _, full := range desiredOrder {
		h := desired[full]
		if s, ok := m.supervisors[full]; ok {
			if !harnessDefEqual(rec.harnesses[full], h) {
				toApply = append(toApply, applyPair{s, h})
				changed = true
			}
			continue
		}
		m.addSupervisorLocked(h)
		m.order = append(m.order, full)
		m.provenance[full] = project
		if h.Enabled {
			toStart = append(toStart, m.supervisors[full])
		}
		changed = true
	}
	rec.harnesses = desired
	rec.order = desiredOrder
	m.projects[project] = rec
	names := append([]string(nil), desiredOrder...)
	m.mu.Unlock()

	// Lifecycle work happens outside the lock (supervisor calls block until
	// their actor loop processes them). Removed harnesses stop in parallel —
	// each Shutdown can block up to Policy.StopGrace on a SIGTERM-deaf
	// process, so a sequential loop would cost N*grace.
	shutdownAll(removed)
	for _, full := range removedNames {
		m.releaseHarnessResources(full)
	}
	for _, p := range toApply {
		p.s.ApplyConfig(p.h)
	}
	for _, s := range toStart {
		s.Start()
	}
	// The registered set changed on disk (SPEC-0004 REQ "Registration
	// Persistence") — schedule a state.json flush.
	m.markDirty()
	return ProjectUpResult{Names: names, Changed: changed}, nil
}

// ProjectDown stops and deregisters every harness registered under project,
// returning their fully-qualified names; the daemon retains no record of the
// project afterward (SPEC-0004 REQ "Tear Down") — including each harness's
// attach Mux and on-disk logs. An unknown project returns an error wrapping
// config.ErrUnknownProject and changes no state.
func (m *Manager) ProjectDown(project string) ([]string, error) {
	m.mu.Lock()
	rec, ok := m.projects[project]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("project down %q: %w", project, config.ErrUnknownProject)
	}
	removedNames := append([]string(nil), rec.order...)
	var sups []*Supervisor
	for _, full := range rec.order {
		if s := m.supervisors[full]; s != nil {
			sups = append(sups, s)
		}
		delete(m.supervisors, full)
		delete(m.provenance, full)
	}
	m.dropFromOrderLocked(removedNames)
	delete(m.projects, project)
	m.mu.Unlock()

	// Graceful stops run outside the lock, in parallel (each can block up to
	// StopGrace); afterwards every per-harness resource is released so nothing
	// of the project outlives the down (SPEC-0004 REQ "Tear Down").
	shutdownAll(sups)
	for _, full := range removedNames {
		m.releaseHarnessResources(full)
	}
	// The project left the registry — flush the registration out of
	// state.json too (SPEC-0004 REQ "Registration Persistence").
	m.markDirty()
	return removedNames, nil
}

// restoreProjectLocked re-registers a persisted project at boot without
// starting anything (SPEC-0004 REQ "Registration Persistence"). defs carry
// their FULL namespaced names exactly as persisted — no re-namespacing, no
// validation round-trip (they were validated by the ProjectUp that registered
// them; state.json is daemon-owned). Caller holds m.mu. Best-effort per
// harness: a name that somehow collides with an existing supervisor is
// skipped rather than clobbering it.
func (m *Manager) restoreProjectLocked(project string, defs []core.Harness) {
	rec, registered := m.projects[project]
	if !registered {
		rec = &projectRecord{name: project, harnesses: map[string]core.Harness{}}
		m.projects[project] = rec
	}
	for _, h := range defs {
		if _, taken := m.supervisors[h.Name]; taken {
			continue
		}
		m.addSupervisorLocked(h)
		m.order = append(m.order, h.Name)
		m.provenance[h.Name] = project
		rec.harnesses[h.Name] = h
		rec.order = append(rec.order, h.Name)
	}
}

// ErrNotRemovable is the sentinel for remove on a harness the daemon does not
// own outright: global-config harnesses are authored in harness.toml
// (ADR-0006) and must be removed there (then reload), not torn down by a
// runtime op. Callers branch with errors.Is.
var ErrNotRemovable = errors.New("harness is not removable: global-config harnesses are owned by harness.toml")

// RemoveHarness stops and deregisters one project-registered harness
// (SPEC-0004 REQ "Remove") — the single-member counterpart to ProjectDown.
// Removing the last member drops the now-empty project record too. A global
// harness name returns ErrNotRemovable; an unknown name returns an error
// wrapping ErrUnknownProject semantics per provenance lookup.
func (m *Manager) RemoveHarness(name string) error {
	m.mu.Lock()
	project := m.provenance[name]
	if project == "" {
		m.mu.Unlock()
		if _, ok := m.cfg.Harnesses[name]; ok {
			return fmt.Errorf("remove %q: %w", name, ErrNotRemovable)
		}
		return fmt.Errorf("remove %q: unknown harness", name)
	}
	rec := m.projects[project]
	s := m.supervisors[name]
	delete(m.supervisors, name)
	delete(m.provenance, name)
	delete(m.scratchDefs, name)
	m.dropFromOrderLocked([]string{name})
	if rec != nil {
		delete(rec.harnesses, name)
		rec.order = slices.DeleteFunc(rec.order, func(n string) bool { return n == name })
		if len(rec.order) == 0 {
			delete(m.projects, project)
		}
	}
	m.mu.Unlock()

	if s != nil {
		s.Shutdown()
	}
	m.releaseHarnessResources(name)
	m.markDirty()
	return nil
}

// shutdownAll stops supervisors in parallel and waits for all of them. Each
// Shutdown blocks up to Policy.StopGrace waiting SIGTERM→grace→SIGKILL, so the
// fan-out caps tear-down at ~max(grace) instead of N*grace. Callers must not
// hold m.mu (Shutdown blocks on each supervisor's actor loop).
func shutdownAll(sups []*Supervisor) {
	var wg sync.WaitGroup
	for _, s := range sups {
		wg.Add(1)
		go func(s *Supervisor) {
			defer wg.Done()
			s.Shutdown()
		}(s)
	}
	wg.Wait()
}

// releaseHarnessResources frees the per-name daemon resources of a
// deregistered project harness: its attach Mux (via the DropExtraOut hook) and
// its on-disk log artifacts. Governing: SPEC-0004 REQ "Tear Down" — "the
// daemon retains no record of the project afterward"; ADR-0009.
func (m *Manager) releaseHarnessResources(name string) {
	if m.dropExtraOut != nil {
		m.dropExtraOut(name)
	}
	removeLogArtifacts(m.logCfg.Dir, name)
}

// ProjectOf returns the owning project's name for a registered harness, or ""
// for a global-config harness (or an unknown name). This is the provenance the
// daemon projects into HarnessInfo so clients can scope ps/down (SPEC-0004).
func (m *Manager) ProjectOf(name string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.provenance[name]
}

// ProjectHarnesses returns the fully-qualified names of project's registered
// harnesses in project-file order, ok=false if the project is unknown.
func (m *Manager) ProjectHarnesses(project string) ([]string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.projects[project]
	if !ok {
		return nil, false
	}
	return append([]string(nil), rec.order...), true
}

// HarnessRecord resolves a registered harness name to its definition and
// owning project ("" for a global) under a single lock hold, so callers that
// project both into one response (the daemon's HarnessInfo) cannot observe two
// different registry states mid-mutation — and pay one mutex round-trip
// instead of two (SPEC-0004 REQ "Project Naming And Namespacing"; ADR-0009).
func (m *Manager) HarnessRecord(name string) (core.Harness, string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Provenance first: a project-owned name's definition source is its
	// project record, never the global config (ADR-0009) — even if a
	// same-named global key were injected into the config.
	if project := m.provenance[name]; project != "" {
		if project == ProvenanceScratch {
			if h, ok := m.scratchDefs[name]; ok {
				return h, project, true
			}
			return core.Harness{}, project, false
		}
		if rec := m.projects[project]; rec != nil {
			if h, ok := rec.harnesses[name]; ok {
				return h, project, true
			}
		}
		return core.Harness{}, project, false
	}
	if h, ok := m.cfg.Harnesses[name]; ok {
		return h, "", true
	}
	return core.Harness{}, "", false
}

// HarnessDef returns the registered definition for a fully-qualified or bare
// harness name. The daemon uses it to project Cmd/Backend/Description into
// control responses for project harnesses exactly like global ones.
func (m *Manager) HarnessDef(name string) (core.Harness, bool) {
	h, _, ok := m.HarnessRecord(name)
	return h, ok
}

// validateProjectDefs checks a project_up payload before any registry mutation
// (SPEC-0004 REQ "Error Handling Standards": validate up front, apply
// atomically). Every failure wraps ErrInvalidProjectDef. "." and ".." are
// rejected alongside "/" because registered names derive filesystem paths
// (the per-harness log tree): ".." would escape the log directory and "."
// would collide with a bare global harness's log file.
func validateProjectDefs(project string, defs []core.Harness) error {
	if strings.TrimSpace(project) == "" {
		return fmt.Errorf("project up: %w: empty project name", ErrInvalidProjectDef)
	}
	if strings.Contains(project, "/") {
		return fmt.Errorf("project up %q: %w: project name must not contain %q",
			project, ErrInvalidProjectDef, "/")
	}
	if project == "." || project == ".." {
		return fmt.Errorf("project up %q: %w: project name %q is reserved",
			project, ErrInvalidProjectDef, project)
	}
	// "scratch" is reserved too: it is the provenance sentinel marking a
	// scratchpad (scratch.go ProvenanceScratch), and a project of the same
	// name would write the same value into the provenance map — Save would
	// silently drop its harnesses and ProjectOf would misbadge them
	// (ADR-0017; the collision flagged in the #236 review).
	if project == ProvenanceScratch {
		return fmt.Errorf("project up %q: %w: project name %q is reserved",
			project, ErrInvalidProjectDef, project)
	}
	if len(defs) == 0 {
		return fmt.Errorf("project up %q: %w: no harnesses defined", project, ErrInvalidProjectDef)
	}
	seen := make(map[string]bool, len(defs))
	for _, h := range defs {
		if seen[h.Name] {
			return fmt.Errorf("project up %q: %w: duplicate harness %q", project, ErrInvalidProjectDef, h.Name)
		}
		if err := validateHarnessDef(project, h); err != nil {
			return err
		}
		seen[h.Name] = true
	}
	return nil
}

// validateHarnessDef applies the per-definition rules shared by project_up and
// scratch_run (the wire is a second front door into the registry, so the
// `harness` enum + prompt invariants the config parsers enforce are re-checked
// here — ADR-0011). src labels the error ("project up %q" / "scratchpad").
func validateHarnessDef(src string, h core.Harness) error {
	switch {
	case strings.TrimSpace(h.Name) == "":
		return fmt.Errorf("%s: %w: harness with empty name", src, ErrInvalidProjectDef)
	case strings.Contains(h.Name, "/"):
		return fmt.Errorf("%s: harness %q: %w: name must not contain %q",
			src, h.Name, ErrInvalidProjectDef, "/")
	case h.Name == "." || h.Name == "..":
		return fmt.Errorf("%s: harness %q: %w: name is reserved",
			src, h.Name, ErrInvalidProjectDef)
	// `harness` is required and has no default, on the wire as much as in
	// the file: an empty kind here would otherwise reach Resolve and pick
	// an agent for a caller that never named one.
	case h.Adapter == "":
		return fmt.Errorf("%s: harness %q: %w: missing harness kind (want one of: crush, claude-code, codex, generic)",
			src, h.Name, ErrInvalidProjectDef)
	case h.Adapter != "crush" && h.Adapter != "claude-code" &&
		h.Adapter != "codex" && h.Adapter != "generic":
		return fmt.Errorf("%s: harness %q: %w: unknown harness kind %q",
			src, h.Name, ErrInvalidProjectDef, h.Adapter)
	case strings.TrimSpace(h.Prompt) != "" && len(h.Args) > 0:
		return fmt.Errorf("%s: harness %q: %w: prompt and args are mutually exclusive",
			src, h.Name, ErrInvalidProjectDef)
	case !h.Backend.Valid():
		return fmt.Errorf("%s: harness %q: %w: invalid backend %q",
			src, h.Name, ErrInvalidProjectDef, h.Backend)
	case !h.Restart.Valid():
		return fmt.Errorf("%s: harness %q: %w: invalid restart policy %q",
			src, h.Name, ErrInvalidProjectDef, h.Restart)
	case h.RestartDelay < 0:
		return fmt.Errorf("%s: harness %q: %w: negative restart delay",
			src, h.Name, ErrInvalidProjectDef)
	}
	return nil
}

// harnessDefEqual reports whether two definitions are identical field-for-
// field, so an unchanged re-up definition stages nothing and flags no change.
func harnessDefEqual(a, b core.Harness) bool {
	return a.Name == b.Name &&
		a.Adapter == b.Adapter &&
		slices.Equal(a.Args, b.Args) &&
		a.Prompt == b.Prompt &&
		a.Model == b.Model &&
		a.AutoAccept == b.AutoAccept &&
		a.MaxTurns == b.MaxTurns &&
		a.Quiet == b.Quiet &&
		a.Workdir == b.Workdir &&
		a.EnvFile == b.EnvFile &&
		a.RestartDelay == b.RestartDelay &&
		a.Restart == b.Restart &&
		a.Backend == b.Backend &&
		a.Description == b.Description &&
		a.Enabled == b.Enabled &&
		a.TmuxSocket == b.TmuxSocket &&
		a.Schedule == b.Schedule
}
