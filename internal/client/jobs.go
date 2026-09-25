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
	"errors"
	"fmt"

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
//
// A daemon older than ProtoMinor 11 does not know the Event field and would
// silently drop it, starting a run with no event file — for a harness with a
// schedule, a successful-looking replay that delivered nothing. So an event
// sent to one is refused here, before anything runs.
func (c *Client) TriggerWithEvent(name string, event []byte) (protocol.TriggerData, error) {
	req := protocol.ControlReq{Op: protocol.OpTrigger, Name: name}
	if len(event) > 0 {
		if minor, ok := protoMinor(c.daemon.ProtoVersion); !ok || minor < eventTriggerMinor {
			return protocol.TriggerData{}, fmt.Errorf(
				"client: daemon proto %q predates trigger events (needs 1.%d); restart the daemon on this version to use --event",
				c.daemon.ProtoVersion, eventTriggerMinor)
		}
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

// Triggers lists every declared trigger source with its state, its last event
// and error, its counters and the harnesses it fires (SPEC-0014 REQ "Trigger
// Visibility").
//
// A daemon older than ProtoMinor 13 does not know the op. It answers
// unknown_op, which on its own reads like a typo in the client; the error
// says what it actually means.
func (c *Client) Triggers() ([]protocol.TriggerSourceInfo, error) {
	resp, err := c.call(protocol.ControlReq{Op: protocol.OpTriggers})
	if err != nil {
		var em *protocol.ErrorMsg
		if errors.As(err, &em) && em.Code == protocol.ErrUnknownOp {
			return nil, fmt.Errorf("client: daemon proto %q predates `harness triggers` (needs 1.%d); restart the daemon on this version",
				c.daemon.ProtoVersion, triggersMinor)
		}
		return nil, err
	}
	var out []protocol.TriggerSourceInfo
	return out, json.Unmarshal(resp.Data, &out)
}

// triggersMinor is the ProtoMinor that added the triggers op.
const triggersMinor = 13

// eventTriggerMinor is the ProtoMinor that added ControlReq.Event.
const eventTriggerMinor = 11

// protoMinor parses the minor half of a "major.minor" proto version. A bare
// major has no minor to speak of, which reports false.
func protoMinor(version string) (int, bool) {
	var maj, minor int
	if _, err := fmt.Sscanf(version, "%d.%d", &maj, &minor); err != nil {
		return 0, false
	}
	return minor, true
}
