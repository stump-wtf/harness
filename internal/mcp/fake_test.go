package mcp

import (
	"fmt"

	"github.com/stump-wtf/harness/internal/client"
	"github.com/stump-wtf/harness/internal/protocol"
)

// fakeDaemon is the daemon the tool layer is tested against: canned replies and
// a record of which control calls were made, so a test can prove each tool
// delegates to the client call its verb makes instead of inventing one.
type fakeDaemon struct {
	calls []string

	harnesses    []protocol.HarnessInfo
	described    protocol.HarnessInfo
	describeErr  error
	runs         protocol.RunsData
	runsSeq      []protocol.RunsData // successive answers, for a followed run
	runsAnswered int
	queried      client.RunsQuery
	logs         protocol.LogsData
	logsLines    int
	runLogs      protocol.LogsData
	logsRun      int
	jobs         []protocol.JobInfo
	triggers     []protocol.TriggerSourceInfo
	info         protocol.DaemonInfo
	trigger      protocol.TriggerData
	started      protocol.HarnessInfo
	lifecycleErr error
}

func (f *fakeDaemon) record(s string) { f.calls = append(f.calls, s) }

func (f *fakeDaemon) List() ([]protocol.HarnessInfo, error) {
	f.record("list")
	return f.harnesses, nil
}

func (f *fakeDaemon) Describe(name string) (protocol.HarnessInfo, error) {
	f.record("describe " + name)
	if f.describeErr != nil {
		return protocol.HarnessInfo{}, f.describeErr
	}
	return f.described, nil
}

func (f *fakeDaemon) Runs(name string, limit int) (protocol.RunsData, error) {
	f.record(fmt.Sprintf("runs %s %d", name, limit))
	if f.runsSeq != nil {
		i := f.runsAnswered
		if i >= len(f.runsSeq) {
			i = len(f.runsSeq) - 1
		}
		f.runsAnswered++
		return f.runsSeq[i], nil
	}
	return f.runs, nil
}

func (f *fakeDaemon) QueryRuns(q client.RunsQuery) (protocol.RunsData, error) {
	f.record("query_runs")
	f.queried = q
	return f.runs, nil
}

func (f *fakeDaemon) Logs(name string, lines int) (protocol.LogsData, error) {
	f.record(fmt.Sprintf("logs %s %d", name, lines))
	f.logsLines = lines
	return f.logs, nil
}

func (f *fakeDaemon) RunLogs(name string, run, lines int) (protocol.LogsData, error) {
	f.record(fmt.Sprintf("run_logs %s %d %d", name, run, lines))
	f.logsRun = run
	return f.runLogs, nil
}

func (f *fakeDaemon) Jobs() ([]protocol.JobInfo, error) {
	f.record("jobs")
	return f.jobs, nil
}

func (f *fakeDaemon) Triggers() ([]protocol.TriggerSourceInfo, error) {
	f.record("triggers")
	return f.triggers, nil
}

func (f *fakeDaemon) DaemonInfo() (protocol.DaemonInfo, error) {
	f.record("daemon_info")
	return f.info, nil
}

func (f *fakeDaemon) Trigger(name string) (protocol.TriggerData, error) {
	f.record("trigger " + name)
	return f.trigger, nil
}

func (f *fakeDaemon) Start(name string) (protocol.HarnessInfo, error) {
	f.record("start " + name)
	return f.started, f.lifecycleErr
}

func (f *fakeDaemon) Stop(name string) (protocol.HarnessInfo, error) {
	f.record("stop " + name)
	return f.started, f.lifecycleErr
}

func (f *fakeDaemon) Restart(name string) (protocol.HarnessInfo, error) {
	f.record("restart " + name)
	return f.started, f.lifecycleErr
}

// unknownHarnessErr is what the daemon answers for a name it has no record of.
func unknownHarnessErr(name string) error {
	return &protocol.ErrorMsg{Code: protocol.ErrUnknownHarness, Message: fmt.Sprintf("no harness %q", name)}
}

// entries is a run's activity, oldest first, with the stable ids a cursor uses.
func entries(specs ...[3]string) []protocol.LogEntry {
	out := make([]protocol.LogEntry, 0, len(specs))
	for i, s := range specs {
		out = append(out, protocol.LogEntry{
			ID:      fmt.Sprintf("e%d", i+1),
			Time:    fmt.Sprintf("2026-10-26T09:0%d:00Z", i),
			Kind:    s[0],
			Action:  s[1],
			Summary: s[2],
		})
	}
	return out
}
