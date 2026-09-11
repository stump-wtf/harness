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

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
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
	resp, err := c.call(protocol.ControlReq{Op: protocol.OpTrigger, Name: name})
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
