package daemon

// Governing: SPEC-0002 REQ "Control Operations" — the daemon mirrors the CLI/TUI
// verbs 1:1 (list/describe/start/stop/restart/logs/profiles/use_profile/reload/
// daemon_info), idempotent where sensible (double-start is a no-op), with
// structured ERROR frames carrying a machine code + human message. ADR-0002
// (control is the same set of verbs the CLI and TUI expose). ADR-0006 (reload
// keeps last-good config on a parse error).

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/config"
	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
	"gitea.stump.rocks/stump.wtf/harness/internal/supervisor"
)

// handleControl decodes and services one CONTROL_REQ, replying with a
// CONTROL_RESP or a structured ERROR.
func (c *conn) handleControl(payload []byte) {
	var req protocol.ControlReq
	if err := json.Unmarshal(payload, &req); err != nil {
		_ = c.pc.WriteError(0, protocol.ErrBadRequest, "malformed control request: %v", err)
		return
	}
	switch req.Op {
	case protocol.OpList:
		c.respond(req, c.opList())
	case protocol.OpDescribe:
		c.opDescribe(req)
	case protocol.OpStart, protocol.OpStop, protocol.OpRestart:
		c.opLifecycle(req)
	case protocol.OpEnable, protocol.OpDisable:
		c.opEnableDisable(req)
	case protocol.OpLogs:
		c.opLogs(req)
	case protocol.OpProfiles:
		c.respond(req, c.opProfiles())
	case protocol.OpUseProfile:
		c.opUseProfile(req)
	case protocol.OpReload:
		c.opReload(req)
	case protocol.OpDaemonInfo:
		c.respond(req, c.opDaemonInfo())
	case protocol.OpProjectUp:
		c.opProjectUp(req)
	case protocol.OpProjectDown:
		c.opProjectDown(req)
	case protocol.OpRemove:
		c.opRemove(req)
	case protocol.OpScratchRun:
		c.opScratchRun(req)
	default:
		_ = c.pc.WriteError(req.ID, protocol.ErrUnknownOp, "unknown op %q", req.Op)
	}
}

// respond marshals data and writes a CONTROL_RESP; a marshal failure becomes an
// internal ERROR.
func (c *conn) respond(req protocol.ControlReq, data any) {
	raw, err := json.Marshal(data)
	if err != nil {
		_ = c.pc.WriteError(req.ID, protocol.ErrInternal, "encode response: %v", err)
		return
	}
	_ = c.pc.WriteJSON(protocol.TypeControlResp, &protocol.ControlResp{ID: req.ID, Op: req.Op, Data: raw})
}

// infoFor projects a snapshot + config record onto the wire HarnessInfo.
func (c *conn) infoFor(snap supervisor.Snapshot) protocol.HarnessInfo {
	info := protocol.HarnessInfo{
		Name:          snap.Name,
		State:         string(snap.State),
		Enabled:       snap.Enabled,
		RestartCount:  snap.RestartCount,
		LastExitCode:  snap.LastExitCode,
		Flapping:      snap.Flapping,
		NextRetryInMs: snap.NextRetryIn.Milliseconds(),
		ConfigChanged: snap.ConfigChanged,
		PID:           snap.PID,
	}
	// HarnessRecord resolves the definition and provenance together under one
	// manager lock hold — so a list of N harnesses costs N+1 lock round-trips
	// instead of 2N+1, and Cmd/Backend and Project can never come from two
	// different registry states mid-project_down (SPEC-0004 REQ "Project
	// Naming And Namespacing"; ADR-0009).
	h, project, ok := c.srv.mgr.HarnessRecord(snap.Name)
	if ok {
		info.Adapter = h.Adapter
		info.Prompt = h.Prompt
		info.Model = h.Model
		info.AutoAccept = h.AutoAccept
		info.MaxTurns = h.MaxTurns
		info.Quiet = h.Quiet
		info.Backend = string(h.Backend)
		info.Description = h.Description
		info.Schedule = h.Schedule
	}
	info.Project = project
	// Next-run comes from the live cron, not the config snapshot: the spec
	// alone can't answer "when", and the daemon is the only party that knows
	// the resolved phase (ADR-0013).
	if info.Schedule != "" && c.srv.sched != nil {
		if next, ok := c.srv.sched.NextFire(snap.Name); ok {
			info.NextRun = next.Format(time.RFC3339)
		}
	}
	return info
}

// opList returns every harness in config order (SPEC-0002 "list"; SPEC-0003
// glyphs are derived client-side from State).
func (c *conn) opList() []protocol.HarnessInfo {
	snaps := c.srv.mgr.Snapshots()
	out := make([]protocol.HarnessInfo, 0, len(snaps))
	for _, s := range snaps {
		out = append(out, c.infoFor(s))
	}
	return out
}

// opDescribe returns one harness, or an unknown-harness ERROR.
func (c *conn) opDescribe(req protocol.ControlReq) {
	snap, ok := c.srv.mgr.Snapshot(req.Name)
	if !ok {
		_ = c.pc.WriteError(req.ID, protocol.ErrUnknownHarness, "unknown harness %q", req.Name)
		return
	}
	info := c.infoFor(snap)
	c.attachInfoFor(&info)
	c.respond(req, info)
}

// attachInfoFor decorates a describe payload with the harness's attach plane:
// the authoritative viewport and every live session, with the session(s)
// setting the smallest-attached-wins minimum flagged (#183). Describe only —
// list would pay a registry round-trip per harness for data nobody asked for.
// A harness with no Mux (never attached, no output teed) gets neither field.
func (c *conn) attachInfoFor(info *protocol.HarnessInfo) {
	ms, ok := c.srv.reg.SnapshotFor(info.Name)
	if !ok {
		return
	}
	info.AttachViewport = fmt.Sprintf("%dx%d", ms.Cols, ms.Rows)
	sessions := make([]protocol.AttachSessionInfo, 0, len(ms.Sessions))
	for _, s := range ms.Sessions {
		sessions = append(sessions, protocol.AttachSessionInfo{
			ID:        s.ID,
			Mode:      string(s.Mode),
			Cols:      s.Cols,
			Rows:      s.Rows,
			CreatedAt: s.CreatedAt.Format(time.RFC3339),
			SetsMin:   s.SetsMin,
		})
	}
	info.AttachSessions = sessions
}

// opLifecycle handles start/stop/restart. Each is idempotent (SPEC-0002:
// double-start is a no-op success); an unknown harness is a structured ERROR.
func (c *conn) opLifecycle(req protocol.ControlReq) {
	var ok bool
	switch req.Op {
	case protocol.OpStart:
		ok = c.srv.mgr.Start(req.Name)
	case protocol.OpStop:
		ok = c.srv.mgr.Stop(req.Name)
	case protocol.OpRestart:
		ok = c.srv.mgr.Restart(req.Name)
	}
	if !ok {
		_ = c.pc.WriteError(req.ID, protocol.ErrUnknownHarness, "unknown harness %q", req.Name)
		return
	}
	// Reply with the fresh snapshot so the client can render the new state.
	snap, _ := c.srv.mgr.Snapshot(req.Name)
	c.respond(req, c.infoFor(snap))
}

// opEnableDisable handles enable/disable. Enable sets intent + starts; disable
// clears intent + stops. Both are idempotent; an unknown harness is a structured
// ERROR.
func (c *conn) opEnableDisable(req protocol.ControlReq) {
	var ok bool
	switch req.Op {
	case protocol.OpEnable:
		ok = c.srv.mgr.Enable(req.Name)
	case protocol.OpDisable:
		ok = c.srv.mgr.Disable(req.Name)
	}
	if !ok {
		_ = c.pc.WriteError(req.ID, protocol.ErrUnknownHarness, "unknown harness %q", req.Name)
		return
	}
	snap, _ := c.srv.mgr.Snapshot(req.Name)
	c.respond(req, c.infoFor(snap))
}

// opLogs returns a tail of the harness's on-disk log (ADR-0007). Works for a
// live or crashed harness alike.
//
// The reply also carries the harness's authoritative viewport when one exists,
// because the log tail is raw PTY output: it only reconstructs into a screen at
// the geometry it was drawn at. Sourced from the same Mux the attach plane
// resizes, so the peek pane and an attach session agree on the guest's size
// instead of each guessing (ADR-0003 smallest-attached-wins).
func (c *conn) opLogs(req protocol.ControlReq) {
	if _, ok := c.srv.mgr.Snapshot(req.Name); !ok {
		_ = c.pc.WriteError(req.ID, protocol.ErrUnknownHarness, "unknown harness %q", req.Name)
		return
	}
	lines := req.Lines
	if lines <= 0 {
		lines = 200
	}
	text := readLogTail(c.srv.mgr.LogDir(), req.Name, lines)
	data := protocol.LogsData{Name: req.Name, Text: text}
	// SnapshotFor never materializes a Mux (#183), so a harness nobody has
	// attached to and that has teed no output simply reports no viewport.
	if ms, ok := c.srv.reg.SnapshotFor(req.Name); ok {
		data.Cols, data.Rows = ms.Cols, ms.Rows
	}
	c.respond(req, data)
}

// opProfiles returns every profile, flagging the active one.
func (c *conn) opProfiles() []protocol.ProfileInfo {
	cfg := c.srv.mgr.Config()
	active := c.srv.mgr.ActiveProfile()
	out := make([]protocol.ProfileInfo, 0, len(cfg.ProfileOrder))
	for _, p := range cfg.OrderedProfiles() {
		out = append(out, protocol.ProfileInfo{
			Name:        p.Name,
			Description: p.Description,
			Harnesses:   p.Harnesses,
			Autostart:   p.Autostart,
			Active:      p.Name == active,
		})
	}
	return out
}

// opUseProfile activates a profile and broadcasts profile_changed.
func (c *conn) opUseProfile(req protocol.ControlReq) {
	if !c.srv.mgr.UseProfile(req.Profile) {
		_ = c.pc.WriteError(req.ID, protocol.ErrUnknownProfile, "unknown profile %q", req.Profile)
		return
	}
	c.srv.broadcast(protocol.EventMsg{Kind: protocol.EvProfileChange, Profile: req.Profile})
	c.respond(req, c.opProfiles())
}

// opReload re-parses the config file and applies it, keeping the last-good
// config on a parse error (ADR-0006). On success it broadcasts config_reloaded.
func (c *conn) opReload(req protocol.ControlReq) {
	if err := c.srv.mgr.ReloadFromFile(c.srv.configPath); err != nil {
		// Surface the location-carrying config error verbatim (SPEC-0001 reload
		// banner uses it).
		msg := err.Error()
		var cerr *config.Error
		if errors.As(err, &cerr) {
			msg = cerr.Error()
		}
		_ = c.pc.WriteError(req.ID, protocol.ErrReload, "%s", msg)
		return
	}
	c.srv.broadcast(protocol.EventMsg{Kind: protocol.EvConfigReload})
	c.respond(req, c.opList())
}

// opDaemonInfo returns daemon metadata.
func (c *conn) opDaemonInfo() protocol.DaemonInfo {
	resolved := c.srv.mgr.ProfileResolved()
	res := protocol.DaemonInfo{
		Version:       c.srv.version,
		ProtoVersion:  protocol.ProtoVersion,
		PID:           os.Getpid(),
		UptimeSeconds: timeSince(c.srv.started),
		Socket:        c.srv.socketPath,
		// The registered count — globals plus project harnesses — so the
		// number agrees with what list returns (SPEC-0004; ADR-0009).
		Harnesses:        c.srv.mgr.HarnessCount(),
		ActiveProfile:    c.srv.mgr.ActiveProfile(),
		ProfileResolved:  &resolved,
		DormantAutostart: c.srv.mgr.DormantAutostart(),
	}
	// Remote SSH (ADR-0004): report the live listener only when it actually
	// started, so clients can distinguish "off" from "enabled but refused".
	if addr, keys := c.srv.Remote(); addr != "" {
		res.SshAddr = addr
		res.SshKeys = keys
	}
	return res
}

// timeSince returns whole seconds elapsed since t.
func timeSince(t time.Time) int64 { return int64(time.Since(t).Seconds()) }
