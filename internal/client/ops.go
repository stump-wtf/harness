package client

// Governing: SPEC-0002 REQ "Control Operations" (the typed client half of each
// verb) and REQ "Attach Session" (AttachOpen/…); ADR-0002 (the CLI is the
// supported programmatic surface, so these are the load-bearing calls scripts
// use). Each helper is a one-shot request → typed response.

import (
	"encoding/json"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// List returns every harness in config order.
func (c *Client) List() ([]protocol.HarnessInfo, error) {
	resp, err := c.call(protocol.ControlReq{Op: protocol.OpList})
	if err != nil {
		return nil, err
	}
	var out []protocol.HarnessInfo
	return out, json.Unmarshal(resp.Data, &out)
}

// Describe returns one harness.
func (c *Client) Describe(name string) (protocol.HarnessInfo, error) {
	return c.harnessOp(protocol.OpDescribe, name)
}

// Start/Stop/Restart mutate one harness and return its fresh state (idempotent
// server-side per SPEC-0002).
func (c *Client) Start(name string) (protocol.HarnessInfo, error) {
	return c.harnessOp(protocol.OpStart, name)
}
func (c *Client) Stop(name string) (protocol.HarnessInfo, error) {
	return c.harnessOp(protocol.OpStop, name)
}
func (c *Client) Restart(name string) (protocol.HarnessInfo, error) {
	return c.harnessOp(protocol.OpRestart, name)
}

// Enable sets a harness's enabled intent to true and starts it if stopped.
func (c *Client) Enable(name string) (protocol.HarnessInfo, error) {
	return c.harnessOp(protocol.OpEnable, name)
}

// Disable clears a harness's enabled intent and stops it if running.
func (c *Client) Disable(name string) (protocol.HarnessInfo, error) {
	return c.harnessOp(protocol.OpDisable, name)
}

// harnessOp runs a single-harness op returning a HarnessInfo.
func (c *Client) harnessOp(op protocol.Op, name string) (protocol.HarnessInfo, error) {
	resp, err := c.call(protocol.ControlReq{Op: op, Name: name})
	if err != nil {
		return protocol.HarnessInfo{}, err
	}
	var out protocol.HarnessInfo
	return out, json.Unmarshal(resp.Data, &out)
}

// Logs returns a tail of the harness's on-disk log.
func (c *Client) Logs(name string, lines int) (protocol.LogsData, error) {
	resp, err := c.call(protocol.ControlReq{Op: protocol.OpLogs, Name: name, Lines: lines})
	if err != nil {
		return protocol.LogsData{}, err
	}
	var out protocol.LogsData
	return out, json.Unmarshal(resp.Data, &out)
}

// Profiles returns every profile with the active one flagged.
func (c *Client) Profiles() ([]protocol.ProfileInfo, error) {
	resp, err := c.call(protocol.ControlReq{Op: protocol.OpProfiles})
	if err != nil {
		return nil, err
	}
	var out []protocol.ProfileInfo
	return out, json.Unmarshal(resp.Data, &out)
}

// UseProfile activates a profile and returns the resulting profile list.
func (c *Client) UseProfile(name string) ([]protocol.ProfileInfo, error) {
	resp, err := c.call(protocol.ControlReq{Op: protocol.OpUseProfile, Profile: name})
	if err != nil {
		return nil, err
	}
	var out []protocol.ProfileInfo
	return out, json.Unmarshal(resp.Data, &out)
}

// Reload re-parses the daemon's config file and returns the refreshed list; a
// parse error comes back as a *protocol.ErrorMsg with code reload_failed.
func (c *Client) Reload() ([]protocol.HarnessInfo, error) {
	resp, err := c.call(protocol.ControlReq{Op: protocol.OpReload})
	if err != nil {
		return nil, err
	}
	var out []protocol.HarnessInfo
	return out, json.Unmarshal(resp.Data, &out)
}

// ProjectUp registers (or reconciles) a project's harnesses under the
// <project>/<name> namespace and starts newly-added ones, returning their
// fresh states. Governing: ADR-0009 (project-scoped compose), SPEC-0004 REQ
// "Project Control Operations". Failures come back as *protocol.ErrorMsg with
// code project_collision / invalid_project.
func (c *Client) ProjectUp(project string, harnesses []protocol.ProjectHarness) (protocol.ProjectUpData, error) {
	resp, err := c.call(protocol.ControlReq{Op: protocol.OpProjectUp, Name: project, Harnesses: harnesses})
	if err != nil {
		return protocol.ProjectUpData{}, err
	}
	var out protocol.ProjectUpData
	return out, json.Unmarshal(resp.Data, &out)
}

// ProjectDown stops and deregisters every harness of the named project
// (SPEC-0004 REQ "Tear Down"); an unknown project comes back as a
// *protocol.ErrorMsg with code unknown_project.
func (c *Client) ProjectDown(project string) (protocol.ProjectDownData, error) {
	resp, err := c.call(protocol.ControlReq{Op: protocol.OpProjectDown, Name: project})
	if err != nil {
		return protocol.ProjectDownData{}, err
	}
	var out protocol.ProjectDownData
	return out, json.Unmarshal(resp.Data, &out)
}

// DaemonInfo returns daemon metadata.
func (c *Client) DaemonInfo() (protocol.DaemonInfo, error) {
	resp, err := c.call(protocol.ControlReq{Op: protocol.OpDaemonInfo})
	if err != nil {
		return protocol.DaemonInfo{}, err
	}
	var out protocol.DaemonInfo
	return out, json.Unmarshal(resp.Data, &out)
}

// --- attach data plane (SPEC-0002 REQ "Attach Session") -------------------

// AttachOpen sends an ATTACH_OPEN for a session id the caller chose. The daemon
// replies with a screen snapshot, scrollback tail, then live ATTACH_DATA frames
// tagged with the same id (read those via Conn().ReadFrame).
func (c *Client) AttachOpen(sessionID uint32, name string, cols, rows int, mode protocol.AttachMode) error {
	body, err := json.Marshal(protocol.AttachOpen{Name: name, Cols: cols, Rows: rows, Mode: mode})
	if err != nil {
		return err
	}
	return c.pc.WriteFrame(protocol.TypeAttachOpen, protocol.EncodeAttach(sessionID, body))
}

// AttachInput sends client keystrokes for a session (ignored server-side for a
// read-only session).
func (c *Client) AttachInput(sessionID uint32, data []byte) error {
	return c.pc.WriteFrame(protocol.TypeAttachData, protocol.EncodeAttach(sessionID, data))
}

// AttachResize sends a new viewport for a session (smallest-attached-wins
// applies server-side).
func (c *Client) AttachResize(sessionID uint32, cols, rows int) error {
	body, err := json.Marshal(protocol.AttachResize{Cols: cols, Rows: rows})
	if err != nil {
		return err
	}
	return c.pc.WriteFrame(protocol.TypeAttachResize, protocol.EncodeAttach(sessionID, body))
}

// AttachClose tears down one attach session.
func (c *Client) AttachClose(sessionID uint32) error {
	return c.pc.WriteFrame(protocol.TypeAttachClose, protocol.EncodeAttach(sessionID, nil))
}
