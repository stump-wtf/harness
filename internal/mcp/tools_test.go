package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/client"
	"github.com/stump-wtf/harness/internal/protocol"
)

// callTool runs one tool call and fails the test on a transport error: a tool
// that refuses or that reports a daemon problem answers inside its result.
func callTool(t *testing.T, d Daemon, name, args string, allowControl bool) *CallResult {
	t.Helper()
	res, err := Call(context.Background(), d, name, json.RawMessage(args), allowControl)
	if err != nil {
		t.Fatalf("Call(%s): %v", name, err)
	}
	return res
}

// errorCodeOf pulls the code out of a tool result's structured payload.
func errorCodeOf(t *testing.T, res *CallResult) string {
	t.Helper()
	m, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structured content is %T, want an object", res.StructuredContent)
	}
	code, _ := m["code"].(string)
	return code
}

// writeToolNames are the tools that change state; they must stay behind the
// startup flag (ADR-0054).
var writeToolNames = []string{"harness_trigger", "harness_start", "harness_stop", "harness_restart"}

func TestControlToolsAreAbsentWithoutTheFlag(t *testing.T) {
	hidden := map[string]bool{}
	for _, name := range Tools(false) {
		for _, w := range writeToolNames {
			if name.Name == w {
				hidden[w] = true
			}
		}
	}
	if len(hidden) > 0 {
		t.Fatalf("a read-only server lists write tools: %v", hidden)
	}
	open := map[string]bool{}
	for _, tool := range Tools(true) {
		open[tool.Name] = true
	}
	for _, w := range writeToolNames {
		if !open[w] {
			t.Errorf("Tools(true) is missing %s", w)
		}
	}
}

func TestWriteToolRefusedWithoutTheFlag(t *testing.T) {
	d := &fakeDaemon{started: protocol.HarnessInfo{Name: "web", State: "running"}}
	res := callTool(t, d, "harness_start", `{"name":"web"}`, false)
	if !res.IsError {
		t.Fatalf("start on a read-only server succeeded: %+v", res)
	}
	if code := errorCodeOf(t, res); code != "not_permitted" {
		t.Errorf("code = %q, want not_permitted", code)
	}
	for _, c := range d.calls {
		if strings.HasPrefix(c, "start") {
			t.Fatalf("the refusal still reached the daemon: %v", d.calls)
		}
	}
	if len(Tools(false)) == 0 {
		t.Fatal("read tools disappeared along with the write ones")
	}
}

func TestWriteToolAllowedWithTheFlag(t *testing.T) {
	d := &fakeDaemon{started: protocol.HarnessInfo{Name: "web", State: "running"}}
	res := callTool(t, d, "harness_restart", `{"name":"web"}`, true)
	if res.IsError {
		t.Fatalf("restart failed: %s", res.Content[0].Text)
	}
	if len(d.calls) != 1 || d.calls[0] != "restart web" {
		t.Fatalf("calls = %v, want one restart of web", d.calls)
	}
}

func TestReadToolsDelegateToTheirVerbCall(t *testing.T) {
	d := &fakeDaemon{
		harnesses: []protocol.HarnessInfo{{Name: "web", State: "running"}},
		described: protocol.HarnessInfo{Name: "web", State: "running"},
		runs:      protocol.RunsData{Name: "web", Runs: []protocol.RunInfo{{RunID: 3, Outcome: "success"}}},
		logs:      protocol.LogsData{Name: "web", Source: protocol.LogSourceAgentTrace, Entries: entries([3]string{protocol.LogEntryTool, "edit", "main.go"})},
		runLogs:   protocol.LogsData{Name: "web", Text: "run 9 output"},
		jobs:      []protocol.JobInfo{{Name: "web"}},
		triggers:  []protocol.TriggerSourceInfo{{Source: "channel.sb", Kind: "channel"}},
		info:      protocol.DaemonInfo{Version: "v0.11.0"},
	}
	cases := []struct {
		tool string
		args string
		want string
	}{
		{"harness_list", `{}`, "list"},
		{"harness_describe", `{"name":"web"}`, "describe web"},
		{"harness_runs", `{"name":"web","limit":5}`, "runs web 5"},
		{"harness_logs", `{"name":"web","lines":50}`, "logs web 50"},
		{"harness_logs", `{"name":"web","run":9,"format":"raw"}`, "run_logs web 9 200"},
		{"harness_jobs", `{}`, "jobs"},
		{"harness_triggers", `{}`, "triggers"},
		{"harness_daemon_info", `{}`, "daemon_info"},
	}
	for _, c := range cases {
		d.calls = nil
		res := callTool(t, d, c.tool, c.args, false)
		if res.IsError {
			t.Fatalf("%s %s: %s", c.tool, c.args, res.Content[0].Text)
		}
		if len(d.calls) == 0 || d.calls[0] != c.want {
			t.Fatalf("%s %s made calls %v, want %q", c.tool, c.args, d.calls, c.want)
		}
		if len(res.Content) != 1 || res.Content[0].Type != "text" || res.Content[0].Text == "" {
			t.Fatalf("%s returned no text content: %+v", c.tool, res.Content)
		}
	}
}

func TestDaemonErrorKeepsItsCode(t *testing.T) {
	d := &fakeDaemon{describeErr: unknownHarnessErr("ghost")}
	res := callTool(t, d, "harness_describe", `{"name":"ghost"}`, false)
	if !res.IsError {
		t.Fatal("a daemon error answered as a success")
	}
	if code := errorCodeOf(t, res); code != "unknown_harness" {
		t.Errorf("code = %q, want unknown_harness", code)
	}
}

func TestUnknownToolIsRefused(t *testing.T) {
	res := callTool(t, &fakeDaemon{}, "harness_merge_everything", `{}`, true)
	if !res.IsError {
		t.Fatal("an unregistered tool ran")
	}
	if code := errorCodeOf(t, res); code != "unknown_tool" {
		t.Errorf("code = %q, want unknown_tool", code)
	}
}

func TestZeroArgToolsRejectArguments(t *testing.T) {
	d := &fakeDaemon{}
	res := callTool(t, d, "harness_list", `{"nmae":"web"}`, false)
	if !res.IsError {
		t.Fatal("a typo'd argument was ignored")
	}
	if len(d.calls) > 0 {
		t.Fatalf("the call still reached the daemon: %v", d.calls)
	}
}

func TestMissingNameIsAnArgumentError(t *testing.T) {
	for _, tool := range []string{"harness_describe", "harness_logs"} {
		res := callTool(t, &fakeDaemon{}, tool, `{}`, tool == "harness_logs")
		if !res.IsError {
			t.Errorf("%s accepted an empty object", tool)
		}
	}
}

func TestRunsWithoutNameQueriesEveryHarness(t *testing.T) {
	d := &fakeDaemon{runs: protocol.RunsData{Runs: []protocol.RunInfo{{Harness: "web", RunID: 1, Outcome: "ok"}}}}
	res := callTool(t, d, "harness_runs", `{"since":"2026-10-25T00:00:00Z","outcomes":["failed"],"limit":10}`, false)
	if res.IsError {
		t.Fatalf("runs query failed: %s", res.Content[0].Text)
	}
	if d.queried.Since != "2026-10-25T00:00:00Z" || len(d.queried.Outcomes) != 1 || d.queried.Limit != 10 {
		t.Fatalf("query = %+v, want the filters passed through", d.queried)
	}
	if len(d.calls) != 1 || d.calls[0] != "query_runs" {
		t.Fatalf("calls = %v, want one ledger query", d.calls)
	}
}

func TestTriggerWaitFollowsTheRun(t *testing.T) {
	defer func(old time.Duration) { waitPoll = old }(waitPoll)
	waitPoll = time.Millisecond

	d := &fakeDaemon{
		trigger: protocol.TriggerData{
			Name:     "web",
			Decision: protocol.TriggerStarted,
			Run:      &protocol.RunInfo{RunID: 8, Outcome: "running", Trigger: "manual"},
		},
		runsSeq: []protocol.RunsData{
			{Name: "web", Runs: []protocol.RunInfo{{RunID: 8, Outcome: "running", Trigger: "manual"}}},
			{Name: "web", Runs: []protocol.RunInfo{{RunID: 8, Outcome: "success", Trigger: "manual"}}},
		},
	}
	res := callTool(t, d, "harness_trigger", `{"name":"web","wait":true,"wait_seconds":5}`, true)
	if res.IsError {
		t.Fatalf("trigger failed: %s", res.Content[0].Text)
	}
	m := res.StructuredContent.(map[string]any)
	if _, ok := m["wait_error"]; ok {
		t.Fatalf("the follow failed: %v", m["wait_error"])
	}
	rec, ok := m["run"].(protocol.RunInfo)
	if !ok || rec.RunID != 8 || rec.Outcome != "success" {
		t.Fatalf("followed run = %+v, want run 8 ending success", m["run"])
	}
}

func TestTriggerWaitReportsTheTriggerWhenFollowingFails(t *testing.T) {
	defer func(old time.Duration) { waitPoll = old }(waitPoll)
	waitPoll = time.Millisecond

	d := &fakeDaemon{
		trigger: protocol.TriggerData{Name: "web", Decision: protocol.TriggerStarted,
			Run: &protocol.RunInfo{RunID: 8, Outcome: "running"}},
		runsSeq: []protocol.RunsData{{Name: "web"}},
	}
	res := callTool(t, d, "harness_trigger", `{"name":"web","wait":true,"wait_seconds":1}`, true)
	m := res.StructuredContent.(map[string]any)
	if m["wait_error"] == nil {
		t.Fatalf("a follow that never saw the run reported success: %v", m)
	}
	if m["trigger"] == nil {
		t.Fatal("the trigger record was dropped along with the follow")
	}
}

func TestRegistryNamesAreUniqueAndSchemaBearing(t *testing.T) {
	seen := map[string]bool{}
	for _, tool := range Tools(true) {
		if seen[tool.Name] {
			t.Fatalf("%s is registered twice", tool.Name)
		}
		seen[tool.Name] = true
		if tool.Description == "" {
			t.Errorf("%s has no description", tool.Name)
		}
		if tool.InputSchema["type"] != "object" {
			t.Errorf("%s has no object input schema", tool.Name)
		}
		class, ok := ToolClass(tool.Name)
		if !ok || (class != "read" && class != "write") {
			t.Errorf("%s class = %q", tool.Name, class)
		}
	}
}

// The registry must satisfy the interface the CLI hands it, or the build would
// have to keep client and tool names in step by hand.
var _ Daemon = (*client.Client)(nil)
