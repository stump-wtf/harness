package daemon

// Governing: SPEC-0004 REQ "Project Control Operations" (project_up registers
// + starts a project's harnesses under the <project>/<name> namespace and is
// reconcile-idempotent; project_down stops + deregisters them; both fail with
// structured ERROR frames carrying a machine code + human message) and REQ
// "Error Handling Standards" (errors wrapped at the layer boundary, sentinel
// mapping, structured key-value logging, no silent swallowing). ADR-0009
// (project-scoped compose commands); ADR-0002 (ops mirror the CLI verbs).

import (
	"errors"
	"time"

	"charm.land/log/v2"

	"gitea.stump.rocks/stump.wtf/harness/internal/config"
	"gitea.stump.rocks/stump.wtf/harness/internal/core"
	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
	"gitea.stump.rocks/stump.wtf/harness/internal/supervisor"
)

// opProjectUp registers (or reconciles) req.Name's harnesses under the project
// namespace and starts the newly-added ones, replying with the project's fresh
// states so the CLI can print its status table (SPEC-0004 REQ "Bring Up"). Any
// failure — name collision, invalid definition — is validated up front by the
// Manager and leaves no partially-registered project behind.
func (c *conn) opProjectUp(req protocol.ControlReq) {
	defs := make([]core.Harness, 0, len(req.Harnesses))
	for _, ph := range req.Harnesses {
		defs = append(defs, harnessFromWire(ph))
	}
	res, err := c.srv.mgr.ProjectUp(req.Name, defs)
	if err != nil {
		log.Warn("project up failed", "project", req.Name, "harnesses", len(defs), "err", err)
		c.writeProjectError(req, err)
		return
	}
	log.Info("project up", "project", req.Name, "harnesses", len(res.Names), "changed", res.Changed)
	if res.Changed {
		// The registered set changed; nudge subscribed TUIs to refresh their
		// list (same signal a global config reload uses). A verbatim no-op
		// re-up broadcasts nothing — a cron-style `harness up` loop must not
		// trigger refetch storms across every connected client.
		c.srv.broadcast(protocol.EventMsg{Kind: protocol.EvConfigReload})
	}

	// Build the reply from the names ProjectUp itself registered (computed
	// under its lock), never from a post-hoc registry query a concurrent
	// project_down could hollow out into a bogus empty success.
	infos := make([]protocol.HarnessInfo, 0, len(res.Names))
	for _, n := range res.Names {
		if snap, ok := c.srv.mgr.Snapshot(n); ok {
			infos = append(infos, c.infoFor(snap))
		}
	}
	c.respond(req, protocol.ProjectUpData{Project: req.Name, Harnesses: infos})
}

// opProjectDown stops and deregisters every harness of req.Name's project; the
// daemon retains no record afterward (SPEC-0004 REQ "Tear Down"). An unknown
// project is a structured ERROR with no state change.
func (c *conn) opProjectDown(req protocol.ControlReq) {
	removed, err := c.srv.mgr.ProjectDown(req.Name)
	if err != nil {
		log.Warn("project down failed", "project", req.Name, "err", err)
		c.writeProjectError(req, err)
		return
	}
	log.Info("project down", "project", req.Name, "removed", len(removed))
	c.srv.broadcast(protocol.EventMsg{Kind: protocol.EvConfigReload})
	c.respond(req, protocol.ProjectDownData{Project: req.Name, Removed: removed})
}

// opRemove stops and deregisters ONE registered harness (SPEC-0004 REQ
// "Remove") — the single-member counterpart to project_down that `harness rm`
// maps to. A global-config harness is refused with not_removable: those are
// authored in harness.toml (ADR-0006) and leave via edit + reload, not a
// runtime op.
func (c *conn) opRemove(req protocol.ControlReq) {
	project := c.srv.mgr.ProjectOf(req.Name)
	if project == "" {
		log.Warn("remove refused", "harness", req.Name)
		_ = c.pc.WriteError(req.ID, protocol.ErrNotRemovable,
			"remove %q: not a registered harness (global-config harnesses are owned by harness.toml)", req.Name)
		return
	}
	if err := c.srv.mgr.RemoveHarness(req.Name); err != nil {
		log.Warn("remove failed", "harness", req.Name, "err", err)
		code := protocol.ErrInternal
		if errors.Is(err, supervisor.ErrNotRemovable) {
			code = protocol.ErrNotRemovable
		}
		_ = c.pc.WriteError(req.ID, code, "%s", err.Error())
		return
	}
	log.Info("remove", "harness", req.Name, "project", project)
	c.srv.broadcast(protocol.EventMsg{Kind: protocol.EvConfigReload})
	c.respond(req, protocol.RemoveData{Name: req.Name, Project: project})
}

// opScratchRun registers and starts ONE ad-hoc scratchpad under a
// daemon-minted random name (SPEC-0011 REQ "Control Operation"; ADR-0017).
// The definition rides Harnesses[0] (the same wire shape project_up uses);
// req.Name is the optional slug override. Never persisted; torn down by
// remove.
func (c *conn) opScratchRun(req protocol.ControlReq) {
	if len(req.Harnesses) != 1 {
		_ = c.pc.WriteError(req.ID, protocol.ErrInvalidProject,
			"scratch_run: exactly one harness definition required (got %d)", len(req.Harnesses))
		return
	}
	ph := req.Harnesses[0]
	h := harnessFromWire(ph)
	// Session semantics (ADR-0017): unless the wire explicitly chose a
	// restart policy, a scratchpad defaults to "no" — an exited session stays
	// inspectable until rm, never respawned. harnessFromWire already applied
	// the config-parser default, so undo it when the wire field was empty.
	if ph.Restart == "" {
		h.Restart = core.RestartNo
	}
	name, err := c.srv.mgr.ScratchRun(h, req.Name)
	if err != nil {
		log.Warn("scratch run failed", "err", err)
		c.writeProjectError(req, err)
		return
	}
	snap, ok := c.srv.mgr.Snapshot(name)
	if !ok {
		_ = c.pc.WriteError(req.ID, protocol.ErrInternal, "scratch_run: %q vanished after registration", name)
		return
	}
	log.Info("scratch run", "harness", name)
	c.srv.broadcast(protocol.EventMsg{Kind: protocol.EvConfigReload})
	c.respond(req, protocol.ScratchRunData{Name: name, Info: c.infoFor(snap)})
}

// writeProjectError maps the supervisor/config sentinels onto the SPEC-0004
// wire codes and writes the structured ERROR frame (machine code + human
// message the CLI can surface verbatim).
func (c *conn) writeProjectError(req protocol.ControlReq, err error) {
	code := protocol.ErrInternal
	switch {
	case errors.Is(err, config.ErrProjectNameCollision):
		code = protocol.ErrProjectCollision
	case errors.Is(err, config.ErrUnknownProject):
		code = protocol.ErrUnknownProject
	case errors.Is(err, supervisor.ErrInvalidProjectDef):
		code = protocol.ErrInvalidProject
	}
	_ = c.pc.WriteError(req.ID, code, "%s", err.Error())
}

// harnessFromWire converts a wire ProjectHarness (project-local name) into the
// core domain type; the Manager namespaces the name at registration. An empty
// backend defaults to native (ADR-0003), matching the config parser; anything
// else is validated by Manager.ProjectUp.
func harnessFromWire(ph protocol.ProjectHarness) core.Harness {
	backend := core.Backend(ph.Backend)
	if ph.Backend == "" {
		backend = core.BackendNative
	}
	restart := core.RestartPolicy(ph.Restart)
	if ph.Restart == "" {
		// Absent on the wire (or an older client) = the config parsers'
		// default: always-restart, except a prompt one-shot, which must not
		// respawn after a successful run.
		restart = core.RestartAlways
		if ph.Prompt != "" || ph.PromptFile != "" {
			restart = core.RestartNo
		}
	}

	// Absent on the wire (or an older client) = the headless one-shot default;
	// an explicit false opts into streaming output to an attach.
	quiet := true
	if ph.Quiet != nil {
		quiet = *ph.Quiet
	}
	return core.Harness{
		Name:         ph.Name,
		Adapter:      ph.Harness,
		Args:         ph.Args,
		Prompt:       ph.Prompt,
		PromptFile:   ph.PromptFile,
		Model:        ph.Model,
		AutoAccept:   ph.AutoAccept,
		MaxTurns:     ph.MaxTurns,
		Quiet:        quiet,
		Workdir:      ph.Workdir,
		EnvFile:      ph.EnvFile,
		RestartDelay: time.Duration(ph.RestartDelayMs) * time.Millisecond,
		Restart:      restart,
		Backend:      backend,
		Description:  ph.Description,
		Enabled:      ph.Enabled,
		TmuxSocket:   ph.TmuxSocket,
	}
}
