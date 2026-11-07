package facade

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stump-wtf/harness/internal/adapter"
	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/trajectory"
)

func TestTrajectoryToolsClass(t *testing.T) {
	// SPEC-0006 acceptance criterion: "list_trajectories/get_trajectory
	// registered as read-class facade tools under mcp_allow."
	for _, tool := range TrajectoryTools {
		if tool.Class != ClassRead {
			t.Errorf("tool %q has class %q, want read", tool.Name, tool.Class)
		}
	}
}

func TestTrajectoryToolsNames(t *testing.T) {
	names := make(map[string]bool)
	for _, tool := range TrajectoryTools {
		names[tool.Name] = true
	}
	if !names["list_trajectories"] {
		t.Error("missing list_trajectories tool")
	}
	if !names["get_trajectory"] {
		t.Error("missing get_trajectory tool")
	}
}

func TestAllToolsIncludesTrajectory(t *testing.T) {
	all := AllTools(true)
	if len(all) < 2 {
		t.Fatalf("expected at least 2 tools, got %d", len(all))
	}
}

func TestAllowedReadForEveryHarness(t *testing.T) {
	// SPEC-0005 REQ "Capability Scoping": read tools available to every
	// harness regardless of mcp_allow.
	tests := []struct {
		name     string
		mcpAllow []string
	}{
		{"default nil", nil},
		{"default read", []string{"read"}},
		{"read+write", []string{"read", "write"}},
		{"empty", []string{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := core.Harness{Name: "test", MCPAllow: tt.mcpAllow}
			if !Allowed(h, ClassRead) {
				t.Fatal("read class should be allowed for every harness")
			}
		})
	}
}

func TestAllowedWriteGatedByMCPAllow(t *testing.T) {
	// SPEC-0005 REQ "Capability Scoping": write tools require "write" in
	// mcp_allow.
	tests := []struct {
		name     string
		mcpAllow []string
		want     bool
	}{
		{"nil → denied", nil, false},
		{"read only → denied", []string{"read"}, false},
		{"read+write → allowed", []string{"read", "write"}, true},
		{"write only → allowed", []string{"write"}, true},
		{"empty → denied", []string{}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := core.Harness{Name: "test", MCPAllow: tt.mcpAllow}
			if got := Allowed(h, ClassWrite); got != tt.want {
				t.Fatalf("Allowed(write) = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCheckToolReadAlwaysOK(t *testing.T) {
	h := core.Harness{Name: "test", MCPAllow: []string{"read"}}
	if err := CheckTool(h, "list_trajectories", true); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := CheckTool(h, "get_trajectory", true); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCheckToolUnknown(t *testing.T) {
	h := core.Harness{Name: "test"}
	if err := CheckTool(h, "nonexistent_tool", true); !errors.Is(err, ErrUnknownTool) {
		t.Fatalf("got err %v, want ErrUnknownTool", err)
	}
}

// --- HandleListTrajectories structured errors ---

func TestHandleListTrajectoriesHarvestDisabled(t *testing.T) {
	// SPEC-0006 REQ "Harvest Opt-In": non-opted-in harness gets a structured
	// error with the harvest_disabled code.
	svc := trajectory.NewService(nil) // registry not needed — fails before
	cfg := &core.Config{
		Harnesses: map[string]core.Harness{
			"agent": {Name: "agent", Adapter: "claude-code", HarvestTrajectory: false},
		},
	}

	result := HandleListTrajectories(svc, cfg, "agent")
	if result.Code != "harvest_disabled" {
		t.Fatalf("code = %q, want harvest_disabled", result.Code)
	}
	if result.Error == "" {
		t.Fatal("expected non-empty error message")
	}
}

func TestHandleListTrajectoriesUnknownHarness(t *testing.T) {
	svc := trajectory.NewService(nil)
	cfg := &core.Config{Harnesses: map[string]core.Harness{}}

	result := HandleListTrajectories(svc, cfg, "nonexistent")
	if result.Code != "unknown_harness" {
		t.Fatalf("code = %q, want unknown_harness", result.Code)
	}
}

func TestHandleGetTrajectoryHarvestDisabled(t *testing.T) {
	svc := trajectory.NewService(nil)
	cfg := &core.Config{
		Harnesses: map[string]core.Harness{
			"agent": {Name: "agent", Adapter: "claude-code", HarvestTrajectory: false},
		},
	}

	result := HandleGetTrajectory(svc, cfg, "agent", "/some/path")
	if result.Code != "harvest_disabled" {
		t.Fatalf("code = %q, want harvest_disabled", result.Code)
	}
}

func TestHandleGetTrajectoryRefusesAnUnlistedPath(t *testing.T) {
	// #89: get_trajectory on an opted-in harness must not read a path its
	// list_trajectories does not report. The file exists, so a refusal here
	// comes from scoping, and none of its content may reach the result.
	dir := t.TempDir()
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("TOPSECRET"), 0600); err != nil {
		t.Fatal(err)
	}
	svc := trajectory.NewService(adapter.NewRegistryWithDefaults())
	svc.SetScrollbackDir(dir)
	cfg := &core.Config{
		Harnesses: map[string]core.Harness{
			"agent": {Name: "agent", Adapter: "generic", HarvestTrajectory: true},
		},
	}

	result := HandleGetTrajectory(svc, cfg, "agent", secret)
	if result.Code != "unknown_session" {
		t.Fatalf("code = %q, want unknown_session (error %q)", result.Code, result.Error)
	}
	if result.Trajectory != nil {
		t.Fatalf("returned a trajectory for an unlisted path: %+v", result.Trajectory.Session)
	}
	if strings.Contains(result.Error, "TOPSECRET") {
		t.Fatalf("error leaks file content: %q", result.Error)
	}
}

func TestErrorCode(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{trajectory.ErrHarvestDisabled, "harvest_disabled"},
		{trajectory.ErrUnknownHarness, "unknown_harness"},
		{trajectory.ErrUnknownSession, "unknown_session"},
		{ErrNotPermitted, "not_permitted"},
		{errors.New("something else"), "internal"},
	}

	for _, tt := range tests {
		got := errorCode(tt.err)
		if got != tt.want {
			t.Errorf("errorCode(%v) = %q, want %q", tt.err, got, tt.want)
		}
	}
}
