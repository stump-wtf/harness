// Package mcp serves the harness control plane as an MCP server over stdio
// (stump.wtf/harness#950).
//
// Governing: ADR-0002 (the CLI is the supported programmatic surface, so every
// tool here delegates to the same typed client call its verb makes), ADR-0035
// and SPEC-0005 REQ "Facade Tool Surface" (the tool names and their read/write
// classes), and ADR-0054 (this slice: the stdio bridge without the broker or
// per-harness tokens, control behind an explicit startup flag).
//
// The package deliberately holds no daemon state and dials nothing itself: the
// caller supplies a Daemon (in practice a *client.Client), which keeps the tool
// layer testable against a fake and lets the CLI own socket resolution.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/stump-wtf/harness/internal/client"
	"github.com/stump-wtf/harness/internal/facade"
	"github.com/stump-wtf/harness/internal/protocol"
)

// Daemon is the control-plane surface the tools need. *client.Client satisfies
// it; tests supply a fake.
type Daemon interface {
	List() ([]protocol.HarnessInfo, error)
	Describe(name string) (protocol.HarnessInfo, error)
	Runs(name string, limit int) (protocol.RunsData, error)
	QueryRuns(q client.RunsQuery) (protocol.RunsData, error)
	Logs(name string, lines int) (protocol.LogsData, error)
	RunLogs(name string, run, lines int) (protocol.LogsData, error)
	Jobs() ([]protocol.JobInfo, error)
	Triggers() ([]protocol.TriggerSourceInfo, error)
	DaemonInfo() (protocol.DaemonInfo, error)
	Trigger(name string) (protocol.TriggerData, error)
	Start(name string) (protocol.HarnessInfo, error)
	Stop(name string) (protocol.HarnessInfo, error)
	Restart(name string) (protocol.HarnessInfo, error)
}

// Tool is one MCP tool: its name, its read/write class for gating, the
// description a client shows the model, and the JSON schema for its arguments.
type Tool struct {
	Name        string
	Class       facade.Class
	Description string
	InputSchema map[string]any
}

// ErrControlDisabled is returned when a write-class tool is called on a server
// started without control. SPEC-0005 REQ "Capability Scoping" refuses a write
// to a caller whose grant lacks "write"; in this slice the grant is the startup
// flag, so the refusal says which flag to set (ADR-0054).
var ErrControlDisabled = errors.New("control tools are disabled: start with --allow-control")

// MaxLogBytes caps one log payload so a chatty run cannot dump megabytes into
// an agent's context; the payload carries a truncation marker when cut.
const MaxLogBytes = 256 * 1024

// DefaultLogLines is the tail length a call gets when it asks for no count.
const DefaultLogLines = 200

// tools returns the registry for a server's control setting. Read-class tools
// are always present; write-class tools appear only when control is allowed,
// so a monitoring agent's server cannot even be told about them.
func tools(allowControl bool) []Tool {
	all := []Tool{
		{Name: "harness_list", Class: facade.ClassRead, Description: "List every harness with its state, enabled/held flags and last exit.", InputSchema: object(nil, nil)},
		{Name: "harness_describe", Class: facade.ClassRead, Description: "Show one harness in detail (config, state, children, sessions, last run).", InputSchema: object(map[string]any{
			"name": str("Harness name"),
		}, []string{"name"})},
		{Name: "harness_runs", Class: facade.ClassRead, Description: "Run history from the ledger: outcome, exit code, duration, trigger and skip reason. With no name, across all harnesses.", InputSchema: object(map[string]any{
			"name":       str("Harness name; omit to query every harness"),
			"limit":      integer("Records to return, newest first (default 20 for one harness, 50 across harnesses, max 1000)"),
			"since":      str("RFC 3339 lower bound"),
			"until":      str("RFC 3339 upper bound"),
			"outcomes":   stringArray("Filter by outcome (ok, failed, timed_out, skipped, …)"),
			"triggers":   stringArray("Filter by trigger (schedule, manual, channel, webhook, …)"),
			"before_seq": integer("Page: return records older than this ledger sequence"),
		}, nil)},
		{Name: "harness_logs", Class: facade.ClassRead, Description: "Read a run's output: format \"summary\" gives ordered activity entries with a cursor for polling, format \"raw\" gives the log tail. Both are size-capped and say when truncated.", InputSchema: object(map[string]any{
			"name":      str("Harness name"),
			"run":       integer("Run id to read; omit for the latest run"),
			"lines":     integer("Tail length (default 200)"),
			"format":    enum([]string{"summary", "raw"}, "How to shape the output (default summary)"),
			"cursor":    str("Entry id from a previous summary call: return only what is newer"),
			"offset":    integer("Byte offset into a raw payload, for paging past a truncation"),
			"max_bytes": integer("Hard size cap (default 262144)"),
		}, []string{"name"})},
		{Name: "harness_jobs", Class: facade.ClassRead, Description: "List triggered harnesses with their sources, next run and recent results.", InputSchema: object(nil, nil)},
		{Name: "harness_triggers", Class: facade.ClassRead, Description: "List trigger sources: state, last event and error, counters, harnesses.", InputSchema: object(nil, nil)},
		{Name: "harness_daemon_info", Class: facade.ClassRead, Description: "Daemon status: version, uptime, protocol, socket, counts.", InputSchema: object(nil, nil)},
		{Name: "harness_trigger", Class: facade.ClassWrite, Description: "Run a triggered harness now (on_overlap applies). With wait, follow the run to its outcome.", InputSchema: object(map[string]any{
			"name":         str("Harness name"),
			"wait":         boolean("Follow the run and return its record"),
			"wait_seconds": integer("Cap on wait when wait is true (default 600)"),
		}, []string{"name"})},
		{Name: "harness_start", Class: facade.ClassWrite, Description: "Start (enable) a harness and return its new state.", InputSchema: object(map[string]any{"name": str("Harness name")}, []string{"name"})},
		{Name: "harness_stop", Class: facade.ClassWrite, Description: "Stop (disable) a harness; a running run is killed unless the daemon is mid graceful stop. Returns the new state.", InputSchema: object(map[string]any{"name": str("Harness name")}, []string{"name"})},
		{Name: "harness_restart", Class: facade.ClassWrite, Description: "Restart a harness (clears a failed latch) and return its new state.", InputSchema: object(map[string]any{"name": str("Harness name")}, []string{"name"})},
	}
	if allowControl {
		return all
	}
	read := make([]Tool, 0, len(all))
	for _, t := range all {
		if t.Class == facade.ClassRead {
			read = append(read, t)
		}
	}
	return read
}

// Tools returns the registry a client may see, as JSON-schema-bearing tools.
func Tools(allowControl bool) []Tool { return tools(allowControl) }

// lookup finds one tool by name under a server's control setting.
func lookup(name string, allowControl bool) (Tool, bool) {
	for _, t := range tools(allowControl) {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

// CallResult is one tool call's outcome, shaped the way MCP wants it: content
// blocks the model reads, plus structuredContent for a client that parses.
type CallResult struct {
	Content           []ContentBlock `json:"content"`
	StructuredContent any            `json:"structuredContent,omitempty"`
	IsError           bool           `json:"isError,omitempty"`
}

// ContentBlock is one MCP content item.
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Call executes one tool against d. A daemon or argument error comes back as a
// tool result carrying isError, not as a transport error: SPEC-0005 REQ "Error
// Handling Standards" wants the caller to distinguish "not permitted" and
// "unknown_harness" from a connection failure.
func Call(ctx context.Context, d Daemon, name string, raw json.RawMessage, allowControl bool) (*CallResult, error) {
	// The lookup is against the whole registry on purpose: a gated write tool
	// exists, this server just will not run it, and "not permitted" is the
	// answer that tells the caller so (SPEC-0005 REQ "Capability Scoping").
	tool, ok := lookup(name, true)
	if !ok {
		return toolError(fmt.Errorf("%w: %s", facade.ErrUnknownTool, name)), nil
	}
	if tool.Class == facade.ClassWrite && !allowControl {
		return toolError(ErrControlDisabled), nil
	}
	switch name {
	case "harness_list":
		if err := noArgs(raw); err != nil {
			return toolError(err), nil
		}
		info, err := d.List()
		if err != nil {
			return toolError(err), nil
		}
		return result(map[string]any{"harnesses": info}), nil
	case "harness_describe":
		var a struct {
			Name string `json:"name"`
		}
		if err := decode(raw, &a); err != nil {
			return toolError(err), nil
		}
		if err := requireName(a.Name); err != nil {
			return toolError(err), nil
		}
		info, err := d.Describe(a.Name)
		if err != nil {
			return toolError(err), nil
		}
		return result(info), nil
	case "harness_runs":
		var a struct {
			Name      string   `json:"name"`
			Limit     int      `json:"limit"`
			Since     string   `json:"since"`
			Until     string   `json:"until"`
			Outcomes  []string `json:"outcomes"`
			Triggers  []string `json:"triggers"`
			BeforeSeq uint64   `json:"before_seq"`
		}
		if err := decode(raw, &a); err != nil {
			return toolError(err), nil
		}
		if a.Name != "" {
			data, err := d.Runs(a.Name, a.Limit)
			if err != nil {
				return toolError(err), nil
			}
			return result(data), nil
		}
		data, err := d.QueryRuns(client.RunsQuery{
			Since: a.Since, Until: a.Until, Outcomes: a.Outcomes,
			Triggers: a.Triggers, Limit: a.Limit, BeforeSeq: a.BeforeSeq,
		})
		if err != nil {
			return toolError(err), nil
		}
		return result(data), nil
	case "harness_logs":
		var a LogsArgs
		if err := decode(raw, &a); err != nil {
			return toolError(err), nil
		}
		res, err := ReadLogs(ctx, d, a)
		if err != nil {
			return toolError(err), nil
		}
		return result(res), nil
	case "harness_jobs":
		if err := noArgs(raw); err != nil {
			return toolError(err), nil
		}
		jobs, err := d.Jobs()
		if err != nil {
			return toolError(err), nil
		}
		return result(map[string]any{"jobs": jobs}), nil
	case "harness_triggers":
		if err := noArgs(raw); err != nil {
			return toolError(err), nil
		}
		srcs, err := d.Triggers()
		if err != nil {
			return toolError(err), nil
		}
		return result(map[string]any{"sources": srcs}), nil
	case "harness_daemon_info":
		if err := noArgs(raw); err != nil {
			return toolError(err), nil
		}
		info, err := d.DaemonInfo()
		if err != nil {
			return toolError(err), nil
		}
		return result(info), nil
	case "harness_trigger":
		var a struct {
			Name        string `json:"name"`
			Wait        bool   `json:"wait"`
			WaitSeconds int    `json:"wait_seconds"`
		}
		if err := decode(raw, &a); err != nil {
			return toolError(err), nil
		}
		// A queued trigger has no run id until it starts, so note the newest id
		// now to recognise the run when it does, exactly as `trigger --wait`
		// does.
		if a.WaitSeconds < 0 {
			return toolError(fmt.Errorf("wait_seconds must not be negative")), nil
		}
		if err := requireName(a.Name); err != nil {
			return toolError(err), nil
		}
		before := 0
		if a.Wait {
			if rd, err := d.Runs(a.Name, 1); err == nil && len(rd.Runs) > 0 {
				before = rd.Runs[0].RunID
			}
		}
		td, err := d.Trigger(a.Name)
		if err != nil {
			return toolError(err), nil
		}
		if !a.Wait {
			return result(td), nil
		}
		rec, err := waitForRun(ctx, d, a.Name, td, before, a.WaitSeconds)
		if err != nil {
			return result(map[string]any{"trigger": td, "wait_error": err.Error()}), nil
		}
		return result(map[string]any{"trigger": td, "run": rec}), nil
	case "harness_start", "harness_stop", "harness_restart":
		var a struct {
			Name string `json:"name"`
		}
		if err := decode(raw, &a); err != nil {
			return toolError(err), nil
		}
		if err := requireName(a.Name); err != nil {
			return toolError(err), nil
		}
		var (
			info protocol.HarnessInfo
			err  error
		)
		switch name {
		case "harness_start":
			info, err = d.Start(a.Name)
		case "harness_stop":
			info, err = d.Stop(a.Name)
		default:
			info, err = d.Restart(a.Name)
		}
		if err != nil {
			return toolError(err), nil
		}
		return result(info), nil
	}
	return toolError(fmt.Errorf("%w: %s", facade.ErrUnknownTool, name)), nil
}

// result wraps a payload as a successful call result: the JSON text a model
// reads, plus the same value structured for a client that parses it.
func result(v any) *CallResult {
	text, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		text = []byte(fmt.Sprintf("%v", err))
	}
	return &CallResult{
		Content:           []ContentBlock{{Type: "text", Text: string(text)}},
		StructuredContent: v,
	}
}

// toolError reports a failed tool call inside the result, so the model sees the
// reason and the transport stays up.
func toolError(err error) *CallResult {
	payload := map[string]any{
		"error":   err.Error(),
		"code":    errorCode(err),
		"message": errorMessage(err),
	}
	text, _ := json.MarshalIndent(payload, "", " ")
	return &CallResult{
		Content:           []ContentBlock{{Type: "text", Text: string(text)}},
		StructuredContent: payload,
		IsError:           true,
	}
}

// errorCode maps an error to the stable code SPEC-0005 wants in a refusal, so
// a caller can branch on "unknown_harness" rather than matching prose.
func errorCode(err error) string {
	var e *protocol.ErrorMsg
	if errors.As(err, &e) {
		return string(e.Code)
	}
	switch {
	case errors.Is(err, facade.ErrUnknownTool):
		return "unknown_tool"
	case errors.Is(err, ErrControlDisabled):
		return "not_permitted"
	default:
		return "internal"
	}
}

// errorMessage is the daemon's own sentence when there is one, since the code
// alone is too terse for a model to act on.
func errorMessage(err error) string {
	var e *protocol.ErrorMsg
	if errors.As(err, &e) {
		return e.Message
	}
	return err.Error()
}

// decode requires an object argument (or none) and fills dst.
func decode(raw json.RawMessage, dst any) error {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		raw = []byte("{}")
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

// requireName rejects a blank target: the daemon would answer the confusing
// unknown_harness "", so the argument error says what is actually wrong.
func requireName(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("this tool needs a harness name")
	}
	return nil
}

// noArgs accepts an absent or empty object and rejects anything with keys, so a
// typo'd argument on a zero-argument tool is a visible error rather than a
// silently ignored one.
func noArgs(raw json.RawMessage) error {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	if len(m) > 0 {
		return fmt.Errorf("this tool takes no arguments (got %d)", len(m))
	}
	return nil
}

// ---- schema helpers --------------------------------------------------------

func object(props map[string]any, required []string) map[string]any {
	o := map[string]any{"type": "object"}
	if props != nil {
		o["properties"] = props
		o["additionalProperties"] = false
	}
	if len(required) > 0 {
		o["required"] = required
	}
	return o
}

func str(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func integer(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

func boolean(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

func stringArray(desc string) map[string]any {
	return map[string]any{
		"type":        "array",
		"items":       map[string]any{"type": "string"},
		"description": desc,
	}
}

func enum(values []string, desc string) map[string]any {
	return map[string]any{"type": "string", "enum": values, "description": desc}
}
