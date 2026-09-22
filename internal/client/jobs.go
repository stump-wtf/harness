package client

// Scheduled Run Ops
//
// The typed client half of jobs, trigger and runs, and of the run selector on
// logs.
//
// Governing: SPEC-0002 REQ "Control Operations"; SPEC-0008 REQ "Protocol
// Operations", REQ "Manual Trigger"; issue #120.
//
// @joestump-agent 09/11/2026 - Added for issue #120.

import (
	"encoding/json"

	"github.com/stump-wtf/harness/internal/protocol"
)

// Jobs lists every scheduled harness with its next window and latest run.
func (c *Client) Jobs() ([]protocol.JobInfo, error) {
	resp, err := c.call(protocol.ControlReq{Op: protocol.OpJobs})
	if err != nil {
		return nil, err
	}
	var out []protocol.JobInfo
	return out, json.Unmarshal(resp.Data, &out)
}

// Trigger starts a manual run of a scheduled harness. The daemon applies the
// harness's on_overlap exactly as it would for a schedule firing.
func (c *Client) Trigger(name string) (protocol.TriggerData, error) {
	return c.TriggerWithEvent(name, nil)
}

// TriggerWithEvent starts a manual run and, when event is non-empty, replays
// that event envelope into it (SPEC-0014 REQ "Manual Trigger With Event").
//
// The bytes go over the wire verbatim, exactly as the operator's file held
// them. Decoding and re-encoding here would normalize a field the daemon is
// about to judge — key order, an omitted optional, a number's spelling — so
// the client would be validating a document the daemon never sees.
func (c *Client) TriggerWithEvent(name string, event []byte) (protocol.TriggerData, error) {
	req := protocol.ControlReq{Op: protocol.OpTrigger, Name: name}
	if len(event) > 0 {
		req.Event = json.RawMessage(event)
	}
	resp, err := c.call(req)
	if err != nil {
		return protocol.TriggerData{}, err
	}
	var out protocol.TriggerData
	return out, json.Unmarshal(resp.Data, &out)
}

// Runs returns a harness's run history, newest first; limit 0 means the
// daemon's default.
func (c *Client) Runs(name string, limit int) (protocol.RunsData, error) {
	resp, err := c.call(protocol.ControlReq{Op: protocol.OpRuns, Name: name, Limit: limit})
	if err != nil {
		return protocol.RunsData{}, err
	}
	var out protocol.RunsData
	return out, json.Unmarshal(resp.Data, &out)
}

// RunLogs returns the tail of one run's own log.
func (c *Client) RunLogs(name string, run, lines int) (protocol.LogsData, error) {
	resp, err := c.call(protocol.ControlReq{Op: protocol.OpLogs, Name: name, Run: run, Lines: lines})
	if err != nil {
		return protocol.LogsData{}, err
	}
	var out protocol.LogsData
	return out, json.Unmarshal(resp.Data, &out)
}
