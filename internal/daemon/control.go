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
	if h, ok := c.srv.mgr.Config().Harnesses[snap.Name]; ok {
		info.Cmd = h.Cmd
		info.Backend = string(h.Backend)
		info.Description = h.Description
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
	c.respond(req, c.infoFor(snap))
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

// opLogs returns a tail of the harness's on-disk log (ADR-0007). Works for a
// live or crashed harness alike.
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
	c.respond(req, protocol.LogsData{Name: req.Name, Text: text})
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
	cfg := c.srv.mgr.Config()
	return protocol.DaemonInfo{
		Version:       c.srv.version,
		ProtoVersion:  protocol.ProtoVersion,
		PID:           os.Getpid(),
		UptimeSeconds: timeSince(c.srv.started),
		Socket:        c.srv.socketPath,
		Harnesses:     len(cfg.Harnesses),
		ActiveProfile: c.srv.mgr.ActiveProfile(),
	}
}

// timeSince returns whole seconds elapsed since t.
func timeSince(t time.Time) int64 { return int64(time.Since(t).Seconds()) }
