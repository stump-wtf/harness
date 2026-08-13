// Package facade defines the MCP facade tool surface (SPEC-0005 REQ "Facade
// Tool Surface") and the trajectory tools required by SPEC-0006 REQ "Trajectory
// Discovery".
//
// Governing: ADR-0010 (local MCP surface), SPEC-0005 REQ "Facade Tool Surface",
// SPEC-0005 REQ "Capability Scoping", SPEC-0006 REQ "Trajectory Discovery",
// SPEC-0006 REQ "Harvest Opt-In".
//
// The facade mirrors control-plane verbs as MCP tools. Issue #76 implements
// only the trajectory tools (read-class); the full control-plane mirror and
// MCP broker belong to the SPEC-0005 implementation story.
package facade

import (
	"errors"
	"fmt"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
	"gitea.stump.rocks/stump.wtf/harness/internal/trajectory"
)

// Class classifies a facade tool as read or write for mcp_allow gating.
// SPEC-0005 REQ "Capability Scoping": read tools are available to every
// harness; write tools require mcp_allow to include "write".
type Class string

const (
	// ClassRead is a read-only facade tool (harness_list, harness_describe,
	// list_trajectories, get_trajectory). Available to every harness.
	ClassRead Class = "read"
	// ClassWrite is a write facade tool (harness_start, harness_stop,
	// harness_restart). Requires mcp_allow to include "write".
	ClassWrite Class = "write"
)

// Tool is one facade tool — its name, class for mcp_allow gating, and the
// description shown in the MCP tool manifest.
type Tool struct {
	// Name is the MCP tool name (e.g. "list_trajectories").
	Name string
	// Class is read or write — determines mcp_allow gating.
	Class Class
	// Description is the human-readable tool purpose shown in the MCP
	// manifest.
	Description string
}

// Sentinel errors for facade operations (SPEC-0005 REQ "Error Handling
// Standards").
var (
	// ErrNotPermitted is returned when a harness's mcp_allow does not include
	// the required class. The refusal carries a structured error
	// distinguishing "not permitted" from "operation failed" (SPEC-0005 REQ
	// "Capability Scoping").
	ErrNotPermitted = errors.New("operation not permitted for this harness (mcp_allow)")

	// ErrUnknownTool is returned when a tool name does not resolve to a
	// registered facade tool.
	ErrUnknownTool = errors.New("unknown facade tool")
)

// TrajectoryTools are the two read-class trajectory tools required by SPEC-0006
// REQ "Trajectory Discovery". They are registered under the reserved facade
// namespace and classified as read operations under mcp_allow.
var TrajectoryTools = []Tool{
	{
		Name:        "list_trajectories",
		Class:       ClassRead,
		Description: "List trajectory sessions for a harness (read-only, requires harvest opt-in)",
	},
	{
		Name:        "get_trajectory",
		Class:       ClassRead,
		Description: "Get the full trajectory content for a harness session (read-only, requires harvest opt-in)",
	},
}

// AllTools returns every facade tool currently registered. For issue #76 this
// is only the trajectory pair; the SPEC-0005 story adds the control-plane
// mirror (harness_list, harness_describe, etc.).
func AllTools() []Tool {
	return append([]Tool{}, TrajectoryTools...)
}

// Allowed reports whether the given harness's mcp_allow permits a tool of the
// given class. Read-class tools (including trajectory tools) are available to
// every harness regardless of mcp_allow; write-class tools require "write" in
// the list. SPEC-0005 REQ "Capability Scoping": "a tool classified as a write
// operation SHALL be refused for a harness whose mcp_allow does not include
// 'write'".
func Allowed(h core.Harness, class Class) bool {
	if class == ClassRead {
		return true
	}
	for _, a := range h.MCPAllow {
		if a == "write" {
			return true
		}
	}
	return false
}

// CheckTool verifies that the calling harness is permitted to invoke a tool
// of the given name. Returns ErrUnknownTool for unrecognized tool names and
// ErrNotPermitted when the harness's mcp_allow does not include the required
// class.
func CheckTool(h core.Harness, toolName string) error {
	for _, t := range AllTools() {
		if t.Name == toolName {
			if !Allowed(h, t.Class) {
				return fmt.Errorf("%w: %s requires %s", ErrNotPermitted, toolName, t.Class)
			}
			return nil
		}
	}
	return fmt.Errorf("%w: %s", ErrUnknownTool, toolName)
}

// ListTrajectoriesResult is the result of the list_trajectories facade tool.
// It carries the session summaries or a structured error.
type ListTrajectoriesResult struct {
	Sessions []trajectory.SessionSummary `json:"sessions,omitempty"`
	Error    string                      `json:"error,omitempty"`
	Code     string                      `json:"errorCode,omitempty"`
}

// GetTrajectoryResult is the result of the get_trajectory facade tool.
type GetTrajectoryResult struct {
	Trajectory *trajectory.Trajectory `json:"trajectory,omitempty"`
	Error      string                 `json:"error,omitempty"`
	Code       string                 `json:"errorCode,omitempty"`
}

// HandleListTrajectories executes the list_trajectories facade tool for the
// calling harness. It delegates to the trajectory service, which enforces the
// harvest opt-in (SPEC-0006 REQ "Harvest Opt-In") and read-only access.
func HandleListTrajectories(svc *trajectory.Service, cfg *core.Config, harnessName string) ListTrajectoriesResult {
	sessions, err := svc.List(cfg, harnessName)
	if err != nil {
		return ListTrajectoriesResult{
			Error: err.Error(),
			Code:  errorCode(err),
		}
	}
	return ListTrajectoriesResult{Sessions: sessions}
}

// HandleGetTrajectory executes the get_trajectory facade tool for the calling
// harness. It delegates to the trajectory service, which enforces the harvest
// opt-in and reads the file read-only.
func HandleGetTrajectory(svc *trajectory.Service, cfg *core.Config, harnessName, sessionPath string) GetTrajectoryResult {
	traj, err := svc.Get(cfg, harnessName, sessionPath)
	if err != nil {
		return GetTrajectoryResult{
			Error: err.Error(),
			Code:  errorCode(err),
		}
	}
	return GetTrajectoryResult{Trajectory: traj}
}

// errorCode maps a sentinel error to a stable string code for structured error
// responses (SPEC-0005 REQ "Error Handling Standards").
func errorCode(err error) string {
	switch {
	case errors.Is(err, trajectory.ErrHarvestDisabled):
		return "harvest_disabled"
	case errors.Is(err, trajectory.ErrUnknownHarness):
		return "unknown_harness"
	case errors.Is(err, ErrNotPermitted):
		return "not_permitted"
	default:
		return "internal"
	}
}
