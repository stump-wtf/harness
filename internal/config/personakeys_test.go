package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Governing: SPEC-0018 REQ-11. system_prompt_file, mcp_config and
// allowed_tools are claude-code one-shot keys: claude-code + a prompt source
// only, paths resolved against the declaring file, missing files a located
// load error, and no tool name may smuggle a flag.

func writePersonaFixture(t *testing.T, harness, extra string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "system.md"), []byte("You are a reviewer.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mcp.json"), []byte(`{"mcpServers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	src := "[harness.reviewer]\nharness = \"" + harness + "\"\n" + extra + "\n"
	cfgPath := filepath.Join(dir, "harness.toml")
	if err := os.WriteFile(cfgPath, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, dir
}

func TestPersonaKeysParseAndResolve(t *testing.T) {
	cfgPath, dir := writePersonaFixture(t, "claude-code",
		"prompt = \"review\"\nsystem_prompt_file = \"system.md\"\nmcp_config = \"mcp.json\"\nallowed_tools = [\"Read\", \"Bash(git log:*)\"]\n")
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	h := cfg.Harnesses["reviewer"]
	if h.SystemPromptFile != filepath.Join(dir, "system.md") {
		t.Errorf("SystemPromptFile = %q, want resolved against the declaring file", h.SystemPromptFile)
	}
	if h.MCPConfig != filepath.Join(dir, "mcp.json") {
		t.Errorf("MCPConfig = %q, want resolved", h.MCPConfig)
	}
	if len(h.AllowedTools) != 2 || h.AllowedTools[1] != "Bash(git log:*)" {
		t.Errorf("AllowedTools = %q, want the two tools kept whole", h.AllowedTools)
	}
}

func TestPersonaKeysValidation(t *testing.T) {
	tests := []struct {
		name    string
		harness string
		body    string
		wantSub string
	}{
		{
			name:    "on a resident harness",
			harness: "claude-code",
			body:    "system_prompt_file = \"system.md\"\n",
			wantSub: `require "prompt" or "prompt_file"`,
		},
		{
			name:    "on another adapter",
			harness: "crush",
			body:    "prompt = \"review\"\nallowed_tools = [\"Read\"]\n",
			wantSub: "claude-code one-shot keys",
		},
		{
			name:    "missing file",
			harness: "claude-code",
			body:    "prompt = \"review\"\nsystem_prompt_file = \"gone.md\"\n",
			wantSub: "system_prompt_file",
		},
		{
			name:    "a flag smuggled in as a tool",
			harness: "claude-code",
			body:    "prompt = \"review\"\nallowed_tools = [\"--dangerously-skip-permissions\"]\n",
			wantSub: "must not start with",
		},
		{
			name:    "a blank tool",
			harness: "claude-code",
			body:    "prompt = \"review\"\nallowed_tools = [\" \"]\n",
			wantSub: "must not be blank",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfgPath, _ := writePersonaFixture(t, tc.harness, tc.body)
			_, err := Load(cfgPath)
			if err == nil {
				t.Fatal("expected a load error")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not mention %q", err, tc.wantSub)
			}
		})
	}
}

// TestAllowedToolsStoredTrimmed: validation trims each entry, so storage
// must too — " Read" would otherwise reach claude as an argv element naming
// no tool (review of #718).
func TestAllowedToolsStoredTrimmed(t *testing.T) {
	cfgPath, _ := writePersonaFixture(t, "claude-code",
		"prompt = \"review\"\nallowed_tools = [\" Read \", \"Bash(git log:*)\"]\n")
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Harnesses["reviewer"].AllowedTools
	if len(got) != 2 || got[0] != "Read" || got[1] != "Bash(git log:*)" {
		t.Errorf("allowed_tools = %q, want [Read Bash(git log:*)]", got)
	}
}
