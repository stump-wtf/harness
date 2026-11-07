package main

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/stump-wtf/harness/internal/client"
	"github.com/stump-wtf/harness/internal/mcp"
	"github.com/stump-wtf/harness/internal/protocol"
)

// stubDaemon stands in for a live daemon: it answers the control calls the tool
// layer makes and records nothing, which is enough to exercise the CLI's half of
// the stdio contract.
type stubDaemon struct {
	allowed []string
}

func (s *stubDaemon) record(name string) { s.allowed = append(s.allowed, name) }

func (s *stubDaemon) List() ([]protocol.HarnessInfo, error) {
	s.record("list")
	return []protocol.HarnessInfo{{Name: "web", State: "running"}}, nil
}

func (s *stubDaemon) Describe(string) (protocol.HarnessInfo, error) {
	return protocol.HarnessInfo{Name: "web", State: "running"}, nil
}

func (s *stubDaemon) Runs(string, int) (protocol.RunsData, error) {
	return protocol.RunsData{}, nil
}

func (s *stubDaemon) QueryRuns(client.RunsQuery) (protocol.RunsData, error) {
	return protocol.RunsData{}, nil
}

func (s *stubDaemon) Logs(string, int) (protocol.LogsData, error) {
	return protocol.LogsData{Name: "web", Text: "hello"}, nil
}

func (s *stubDaemon) RunLogs(string, int, int) (protocol.LogsData, error) {
	return protocol.LogsData{Name: "web", Text: "hello"}, nil
}

func (s *stubDaemon) Jobs() ([]protocol.JobInfo, error)               { return nil, nil }
func (s *stubDaemon) Triggers() ([]protocol.TriggerSourceInfo, error) { return nil, nil }
func (s *stubDaemon) DaemonInfo() (protocol.DaemonInfo, error) {
	return protocol.DaemonInfo{Version: "v0.11.0-test"}, nil
}
func (s *stubDaemon) Trigger(string) (protocol.TriggerData, error) {
	return protocol.TriggerData{}, nil
}
func (s *stubDaemon) Start(string) (protocol.HarnessInfo, error) { return protocol.HarnessInfo{}, nil }
func (s *stubDaemon) Stop(string) (protocol.HarnessInfo, error)  { return protocol.HarnessInfo{}, nil }
func (s *stubDaemon) Restart(string) (protocol.HarnessInfo, error) {
	return protocol.HarnessInfo{}, nil
}

func TestMcpVerbIsRegistered(t *testing.T) {
	root := newRootCmd()
	var mcpCmd *cobra.Command
	for _, c := range root.Commands() {
		if c.Name() == "mcp" {
			mcpCmd = c
		}
	}
	if mcpCmd == nil {
		t.Fatal("harness mcp is not in the command tree")
	}
	if mcpCmd.Short == "" {
		t.Error("harness mcp has no short description for help")
	}
	if mcpCmd.Flags().Lookup("allow-control") == nil {
		t.Error("--allow-control is missing")
	}
}

func TestMcpRejectsPositionals(t *testing.T) {
	root := newRootCmd()
	root.SetArgs([]string{"mcp", "web"})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	if err := root.Execute(); err == nil {
		t.Fatal("harness mcp accepted a positional argument")
	}
}

func TestMcpNeedsADaemon(t *testing.T) {
	root := newRootCmd()
	root.SetArgs([]string{"--socket", "/nonexistent/hnd-mcp-test.sock", "mcp"})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	if err := root.Execute(); err == nil {
		t.Fatal("harness mcp started without a daemon to talk to")
	}
}

// drive serves one session over buffers and returns the protocol stream and the
// diagnostics.
func drive(t *testing.T, d mcp.Daemon, allowControl bool, msgs ...string) (string, string) {
	t.Helper()
	var in strings.Builder
	for _, m := range msgs {
		in.WriteString(m)
		in.WriteString("\n")
	}
	var out, logw bytes.Buffer
	if err := serveMCP(d, allowControl, strings.NewReader(in.String()), &out, &logw); err != nil {
		t.Fatalf("serveMCP: %v", err)
	}
	return out.String(), logw.String()
}

func TestServeMCPKeepsStdoutToProtocolFrames(t *testing.T) {
	out, logs := drive(t, &stubDaemon{}, false,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"harness_list"}}`)

	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("stdout carried %d frames, want one answer per request:\n%s", len(lines), out)
	}
	answers := map[string]string{}
	for _, line := range lines {
		var msg map[string]any
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatalf("stdout is not JSON-RPC: %q", line)
		}
		if msg["jsonrpc"] != "2.0" {
			t.Fatalf("frame is not a JSON-RPC message: %q", line)
		}
		id, _ := msg["id"].(float64)
		answers[itoa(int(id))] = line
	}
	if len(answers) != 3 {
		t.Fatalf("the frames carry %d distinct ids, want 3", len(answers))
	}
	if !strings.Contains(logs, "read-only") {
		t.Errorf("the startup note does not say the server is read-only: %q", logs)
	}
	if !strings.Contains(answers["3"], "web") {
		t.Errorf("the tool answer is missing from the stream: %q", answers["3"])
	}
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

func TestServeMCPControlModeIsStated(t *testing.T) {
	_, logs := drive(t, &stubDaemon{}, true, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if !strings.Contains(logs, "read and control") {
		t.Errorf("the startup note does not say control is on: %q", logs)
	}
}
