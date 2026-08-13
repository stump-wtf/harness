package trajectory

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"gitea.stump.rocks/stump.wtf/agent-trace/tail"
	"gitea.stump.rocks/stump.wtf/harness/internal/adapter"
	"gitea.stump.rocks/stump.wtf/harness/internal/core"
)

// newTestService creates a trajectory service with a default adapter registry
// for testing. The scrollback directory is set to a temp dir.
func newTestService(t *testing.T) *Service {
	t.Helper()
	dir := t.TempDir()
	svc := NewService(adapter.NewRegistryWithDefaults())
	svc.SetScrollbackDir(dir)
	return svc
}

// configWith returns a minimal config with one harness.
func configWith(harnesses ...core.Harness) *core.Config {
	cfg := &core.Config{
		Harnesses: make(map[string]core.Harness),
	}
	for _, h := range harnesses {
		cfg.Harnesses[h.Name] = h
		cfg.HarnessOrder = append(cfg.HarnessOrder, h.Name)
	}
	return cfg
}

func cmdHarness(name, cmd string) core.Harness {
	return core.Harness{Name: name, Cmd: cmd}
}

// withFakeHome sets HOME to a temp dir for the test and returns the path.
func withFakeHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old := os.Getenv("HOME")
	os.Setenv("HOME", dir)
	t.Cleanup(func() { os.Setenv("HOME", old) })
	return dir
}

// writeClaudeCodeSession writes a minimal valid Claude Code JSONL transcript
// under the given HOME directory and returns its path.
func writeClaudeCodeSession(t *testing.T, home, cwd string) string {
	t.Helper()
	// Claude Code stores sessions under ~/.claude/projects/<encoded-cwd>/
	// The encoding replaces / and . with -, but agent-trace's adapter handles
	// the real layout. We use the adapter's own SessionDir to discover.
	projectsDir := filepath.Join(home, ".claude", "projects")
	if err := os.MkdirAll(projectsDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Write a session file that the ClaudeCodeAdapter can discover and parse.
	// The file must be named with a session ID and have .jsonl extension.
	sessionContent := `{"type":"user","timestamp":"2026-01-01T10:00:00Z","sessionId":"abc-123","cwd":"` + cwd + `","message":{"role":"user","content":"hello"}}
`
	sessionPath := filepath.Join(projectsDir, "session-abc123.jsonl")
	if err := os.WriteFile(sessionPath, []byte(sessionContent), 0644); err != nil {
		t.Fatal(err)
	}
	return sessionPath
}

// --- SPEC-0006 REQ "Harvest Opt-In" ---

func TestListOptOutIsDefault(t *testing.T) {
	// SPEC-0006 REQ "Harvest Opt-In" scenario "Opt-out is the default":
	// "WHEN a harness has not enabled trajectory harvesting THEN
	// list_trajectories omits it entirely and get_trajectory refuses with
	// a structured error"
	svc := newTestService(t)
	cfg := configWith(cmdHarness("agent", "claude"))

	sessions, err := svc.List(cfg, "agent")
	if !errors.Is(err, ErrHarvestDisabled) {
		t.Fatalf("List: got err %v, want ErrHarvestDisabled", err)
	}
	if sessions != nil {
		t.Fatalf("expected nil sessions, got %v", sessions)
	}
}

func TestGetOptOutIsDefault(t *testing.T) {
	svc := newTestService(t)
	cfg := configWith(cmdHarness("agent", "claude"))

	_, err := svc.Get(cfg, "agent", "/some/path")
	if !errors.Is(err, ErrHarvestDisabled) {
		t.Fatalf("Get: got err %v, want ErrHarvestDisabled", err)
	}
}

func TestListUnknownHarness(t *testing.T) {
	svc := newTestService(t)
	cfg := configWith()

	_, err := svc.List(cfg, "nonexistent")
	if !errors.Is(err, ErrUnknownHarness) {
		t.Fatalf("got err %v, want ErrUnknownHarness", err)
	}
}

func TestGetUnknownHarness(t *testing.T) {
	svc := newTestService(t)
	cfg := configWith()

	_, err := svc.Get(cfg, "nonexistent", "/some/path")
	if !errors.Is(err, ErrUnknownHarness) {
		t.Fatalf("got err %v, want ErrUnknownHarness", err)
	}
}

// --- SPEC-0006 REQ "Trajectory Discovery" scrollback fallback ---

func TestListOptInScrollbackFallback(t *testing.T) {
	// When a harness opts in and its adapter reports no native trajectory
	// (Generic), the scrollback record is reported as the trajectory source.
	// SPEC-0006 REQ "Trajectory Discovery" scenario "Fallback to scrollback".
	dir := t.TempDir()
	svc := NewService(adapter.NewRegistryWithDefaults())
	svc.SetScrollbackDir(dir)

	// Create the scrollback log file for "worker".
	scrollbackPath := filepath.Join(dir, "worker.log")
	if err := os.WriteFile(scrollbackPath, []byte("[scrollback content]"), 0644); err != nil {
		t.Fatal(err)
	}

	h := core.Harness{
		Name:              "worker",
		Cmd:               "my-custom-tool",
		HarvestTrajectory: true,
	}
	cfg := configWith(h)

	sessions, err := svc.List(cfg, "worker")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 scrollback session, got %d", len(sessions))
	}
	s := sessions[0]
	if s.Source != "scrollback" {
		t.Fatalf("source = %q, want scrollback", s.Source)
	}
	if s.Path != scrollbackPath {
		t.Fatalf("path = %q, want %q", s.Path, scrollbackPath)
	}
	if s.ID != "worker" {
		t.Fatalf("id = %q, want worker", s.ID)
	}
}

func TestListOptInNoScrollbackFile(t *testing.T) {
	// Opt-in harness with generic adapter but no scrollback file → empty
	// list, no error.
	svc := newTestService(t)
	h := core.Harness{
		Name:              "worker",
		Cmd:               "my-custom-tool",
		HarvestTrajectory: true,
	}
	cfg := configWith(h)

	sessions, err := svc.List(cfg, "worker")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("expected 0 sessions, got %d", len(sessions))
	}
}

func TestGetScrollbackFallback(t *testing.T) {
	dir := t.TempDir()
	svc := NewService(adapter.NewRegistryWithDefaults())
	svc.SetScrollbackDir(dir)

	content := "[scrollback content]"
	scrollbackPath := filepath.Join(dir, "worker.log")
	if err := os.WriteFile(scrollbackPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	h := core.Harness{
		Name:              "worker",
		Cmd:               "my-custom-tool",
		HarvestTrajectory: true,
	}
	cfg := configWith(h)

	traj, err := svc.Get(cfg, "worker", scrollbackPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if traj.Content != content {
		t.Fatalf("content = %q, want %q", traj.Content, content)
	}
	if traj.Session.Source != "scrollback" {
		t.Fatalf("source = %q, want scrollback", traj.Session.Source)
	}
}

// --- SPEC-0006 REQ "Trajectory Discovery" native transcript ---

func TestListNativeTrajectoryClaudeCode(t *testing.T) {
	// SPEC-0006 REQ "Trajectory Discovery" scenario "Native transcript is
	// located": "WHEN a claude-code harness has written a session transcript
	// THEN list_trajectories reports it for that harness."
	//
	// This test uses a fake HOME to create a Claude Code session transcript
	// and verifies the service discovers it via agent-trace's adapter.
	withFakeHome(t) // sets HOME so ClaudeCodeAdapter.SessionDir() resolves under the temp dir
	cwd := t.TempDir()

	// Write a session file using the ClaudeCodeAdapter's own directory layout.
	// The adapter's SessionDir() resolves to ~/.claude/projects/.
	ccAdapter := &tail.ClaudeCodeAdapter{}
	sessionDir := ccAdapter.SessionDir()
	if err := os.MkdirAll(sessionDir, 0755); err != nil {
		t.Fatal(err)
	}

	sessionContent := `{"type":"user","timestamp":"2026-01-01T10:00:00Z","sessionId":"abc-123","cwd":"` + cwd + `","message":{"role":"user","content":"hello"}}
`
	sessionPath := filepath.Join(sessionDir, "session-abc123.jsonl")
	if err := os.WriteFile(sessionPath, []byte(sessionContent), 0644); err != nil {
		t.Fatal(err)
	}

	svc := newTestService(t)
	h := core.Harness{
		Name:              "agent",
		Cmd:               "claude",
		Workdir:           cwd,
		HarvestTrajectory: true,
	}
	cfg := configWith(h)

	sessions, err := svc.List(cfg, "agent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(sessions) == 0 {
		t.Fatal("expected at least 1 native session, got 0")
	}

	// Find our session.
	var found *SessionSummary
	for i := range sessions {
		if sessions[i].ID == "abc-123" {
			found = &sessions[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("session abc-123 not found in %d sessions", len(sessions))
	}
	if found.Source != "native" {
		t.Fatalf("source = %q, want native", found.Source)
	}
}

func TestGetNativeTrajectory(t *testing.T) {
	// get_trajectory returns the raw file content for a native session.
	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "session.jsonl")
	content := `{"type":"user","timestamp":"2026-01-01T10:00:00Z"}`
	if err := os.WriteFile(sessionPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	svc := newTestService(t)
	h := core.Harness{
		Name:              "agent",
		Cmd:               "claude",
		Agent:             "claude-code",
		HarvestTrajectory: true,
	}
	cfg := configWith(h)

	traj, err := svc.Get(cfg, "agent", sessionPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if traj.Content != content {
		t.Fatalf("content mismatch")
	}
	if traj.Session.Source != "native" {
		t.Fatalf("source = %q, want native", traj.Session.Source)
	}
}

// --- SPEC-0006 REQ "Harvest Opt-In" reload sensitivity ---

func TestOptInAfterReload(t *testing.T) {
	// SPEC-0006 REQ "Harvest Opt-In" scenario "Opt-in exposes the trajectory":
	// "WHEN a harness enables trajectory harvesting and the config is reloaded
	// THEN its trajectory appears in list_trajectories without restarting the
	// harness."
	//
	// The service is stateless — it reads the config on every call, so
	// a reload that enables harvesting is immediately visible.
	dir := t.TempDir()
	svc := NewService(adapter.NewRegistryWithDefaults())
	svc.SetScrollbackDir(dir)

	scrollbackPath := filepath.Join(dir, "worker.log")
	if err := os.WriteFile(scrollbackPath, []byte("[content]"), 0644); err != nil {
		t.Fatal(err)
	}

	// Config v1: harvest disabled.
	cfgV1 := configWith(core.Harness{
		Name:              "worker",
		Cmd:               "my-tool",
		HarvestTrajectory: false,
	})

	_, err := svc.List(cfgV1, "worker")
	if !errors.Is(err, ErrHarvestDisabled) {
		t.Fatalf("before reload: got err %v, want ErrHarvestDisabled", err)
	}

	// Config v2: harvest enabled (simulating a reload). Same service, no
	// restart.
	cfgV2 := configWith(core.Harness{
		Name:              "worker",
		Cmd:               "my-tool",
		HarvestTrajectory: true,
	})

	sessions, err := svc.List(cfgV2, "worker")
	if err != nil {
		t.Fatalf("after reload: unexpected error: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("after reload: expected 1 session, got %d", len(sessions))
	}
	if sessions[0].Source != "scrollback" {
		t.Fatalf("source = %q, want scrollback", sessions[0].Source)
	}
}

// --- Read-only enforcement ---

func TestGetReadOnly(t *testing.T) {
	// Verify that Get reads but does not modify the file.
	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "session.jsonl")
	original := `{"test":true}`
	if err := os.WriteFile(sessionPath, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}

	svc := newTestService(t)
	h := core.Harness{
		Name:              "agent",
		Cmd:               "claude",
		HarvestTrajectory: true,
	}
	cfg := configWith(h)

	_, err := svc.Get(cfg, "agent", sessionPath)
	if err != nil {
		t.Fatal(err)
	}

	// File should be unchanged.
	after, err := os.ReadFile(sessionPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != original {
		t.Fatalf("file was modified: got %q, want %q", string(after), original)
	}

	// File mode should be unchanged (no write occurred).
	info, err := os.Stat(sessionPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0644 {
		t.Fatalf("file mode changed: got %v, want 0644", info.Mode().Perm())
	}
}

// --- Sorting ---

func TestSortByStartedDesc(t *testing.T) {
	sessions := []SessionSummary{
		{ID: "old", StartedAt: "2026-01-01T00:00:00Z"},
		{ID: "newest", StartedAt: "2026-06-01T00:00:00Z"},
		{ID: "mid", StartedAt: "2026-03-01T00:00:00Z"},
		{ID: "notime"},
	}
	sortByStartedDesc(sessions)

	// Newest first, no-time last.
	if sessions[0].ID != "newest" {
		t.Fatalf("sessions[0] = %q, want newest", sessions[0].ID)
	}
	if sessions[1].ID != "mid" {
		t.Fatalf("sessions[1] = %q, want mid", sessions[1].ID)
	}
	if sessions[2].ID != "old" {
		t.Fatalf("sessions[2] = %q, want old", sessions[2].ID)
	}
	// The no-time session should be last.
	if sessions[3].ID != "notime" {
		t.Fatalf("sessions[3] = %q, want notime", sessions[3].ID)
	}
}
