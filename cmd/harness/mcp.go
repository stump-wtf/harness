package main

// harness mcp: the control plane as an MCP server over stdio.
//
// Governing: ADR-0002 (the tools delegate to the same client calls the verbs
// make, so there is one implementation of each operation), ADR-0035 and
// SPEC-0005 REQ "Facade Tool Surface" (the tool names and their read/write
// classes), and ADR-0054 (this slice: no broker and no per-harness token, so
// the server serves whoever can reach the socket, and everything that changes
// state is behind --allow-control).
//
// stdout is the JSON-RPC channel for the whole session, so everything this
// command has to say about itself goes to stderr.

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/spf13/cobra"

	"github.com/stump-wtf/harness/internal/buildinfo"
	"github.com/stump-wtf/harness/internal/client"
	"github.com/stump-wtf/harness/internal/mcp"
	"github.com/stump-wtf/harness/internal/protocol"
)

// newMcpCmd builds `harness mcp [--allow-control]`.
func newMcpCmd(g *globalOpts) *cobra.Command {
	var allowControl bool
	cmd := &cobra.Command{
		Use:           "mcp",
		Short:         "serve daemon status, runs, logs and lifecycle controls as an MCP server over stdio",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withClient(g.opts(), nil, func(c *client.Client, _ verbOpts) error {
				return serveMCP(&serialized{c: c}, allowControl, os.Stdin, os.Stdout, os.Stderr)
			})
		},
	}
	cmd.Flags().BoolVar(&allowControl, "allow-control", false, "also serve the tools that change state (trigger, start, stop, restart); without it the server is read-only")
	return cmd
}

// serveMCP serves the tool surface on the given streams until the client closes
// its side. It takes the streams so the stdio contract is testable: nothing but
// protocol frames may reach out.
func serveMCP(d mcp.Daemon, allowControl bool, in io.Reader, out, logw io.Writer) error {
	srv := mcp.New(d, allowControl, buildinfo.Version)
	srv.SetLogger(func(format string, args ...any) {
		fmt.Fprintf(logw, "harness mcp: "+format+"\n", args...)
	})
	mode := "read-only"
	if allowControl {
		mode = "read and control"
	}
	fmt.Fprintf(logw, "harness mcp: serving %d tools (%s) on stdio; close stdin to end the session\n",
		len(mcp.Tools(allowControl)), mode)
	return srv.Serve(context.Background(), in, out)
}

// serialized puts one control call on the wire at a time. The MCP server
// answers requests in parallel so a followed trigger cannot stall the session,
// but client.Client speaks a single request/response stream and holds no lock of
// its own: two calls sharing it interleave their frames, which the daemon reads
// as a corrupt length prefix. Locking per call, not per tool, keeps the
// follow's polling from blocking everything else.
type serialized struct {
	c  *client.Client
	mu sync.Mutex
}

func (s *serialized) List() ([]protocol.HarnessInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.c.List()
}

func (s *serialized) Describe(name string) (protocol.HarnessInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.c.Describe(name)
}

func (s *serialized) Runs(name string, limit int) (protocol.RunsData, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.c.Runs(name, limit)
}

func (s *serialized) QueryRuns(q client.RunsQuery) (protocol.RunsData, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.c.QueryRuns(q)
}

func (s *serialized) Logs(name string, lines int) (protocol.LogsData, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.c.Logs(name, lines)
}

func (s *serialized) RunLogs(name string, run, lines int) (protocol.LogsData, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.c.RunLogs(name, run, lines)
}

func (s *serialized) Jobs() ([]protocol.JobInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.c.Jobs()
}

func (s *serialized) Triggers() ([]protocol.TriggerSourceInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.c.Triggers()
}

func (s *serialized) DaemonInfo() (protocol.DaemonInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.c.DaemonInfo()
}

func (s *serialized) Trigger(name string) (protocol.TriggerData, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.c.Trigger(name)
}

func (s *serialized) Start(name string) (protocol.HarnessInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.c.Start(name)
}

func (s *serialized) Stop(name string) (protocol.HarnessInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.c.Stop(name)
}

func (s *serialized) Restart(name string) (protocol.HarnessInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.c.Restart(name)
}
