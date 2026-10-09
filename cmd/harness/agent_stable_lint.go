package main

// Stable Lint And Check
//
// `harness agent stable lint [PATH]` and `harness agent stable check [PATH]`
// validate a stable checkout — a directory with packages/ — with Harness's
// own manifest parser and content scanner, so a stable's CI and an
// operator's install can never disagree. Both are offline: they read only
// PATH, never the daemon socket, the stable store, git or the network. The
// engine is internal/agentpkg/stablelint; this file is the CLI rendering.
//
// Governing: ADR-0044 (agent package stables), SPEC-0026 REQ-3, REQ-5,
// REQ-7, REQ-12 (CLI visibility).
//
// @joestump-agent 10/09/2026 - Added for harness#933.

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/stump-wtf/harness/internal/agentpkg/stablelint"
	"github.com/stump-wtf/harness/internal/cliui"
)

func newStableLintCmd(g *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:           "lint [PATH]",
		Short:         "validate a stable checkout offline: schema, scan, conventions",
		Args:          cobra.MaximumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAgentStableLint(cmd, stablePathArg(args), stablelint.Lint)
		},
	}
}

func newStableCheckCmd(g *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:           "check [PATH]",
		Short:         "lint a stable checkout, then load-test each package as config load would",
		Args:          cobra.MaximumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAgentStableLint(cmd, stablePathArg(args), stablelint.Check)
		},
	}
}

func stablePathArg(args []string) string {
	if len(args) > 0 {
		return args[0]
	}
	return "."
}

// runAgentStableLint renders the reports — one JSON object per package under
// --json, otherwise one line per finding — and exits 1 when any package has
// an error. Warnings never fail the run.
func runAgentStableLint(cmd *cobra.Command, root string, run func(string) ([]stablelint.Report, error)) error {
	reports, err := run(root)
	if err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	if cliui.JSON() {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		if err := enc.Encode(reports); err != nil {
			return err
		}
	} else {
		renderStableLint(cmd.OutOrStdout(), reports)
	}
	if stablelint.Failed(reports) {
		return exitCodeError{code: 1}
	}
	return nil
}

func renderStableLint(w io.Writer, reports []stablelint.Report) {
	var nerr, nwarn int
	for _, r := range reports {
		nerr += len(r.Errors)
		nwarn += len(r.Warnings)
		if len(r.Errors) == 0 && len(r.Warnings) == 0 {
			fmt.Fprintf(w, "agent: %s: ok\n", r.Package)
			continue
		}
		for _, is := range r.Errors {
			fmt.Fprintf(w, "agent: %s: error   %s%s: %s\n", r.Package, is.ID, stableLintLoc(is), is.Message)
		}
		for _, is := range r.Warnings {
			fmt.Fprintf(w, "agent: %s: warning %s%s: %s\n", r.Package, is.ID, stableLintLoc(is), is.Message)
		}
	}
	fmt.Fprintf(w, "agent: %d package(s), %d error(s), %d warning(s)\n", len(reports), nerr, nwarn)
}

func stableLintLoc(is stablelint.Issue) string {
	switch {
	case is.File != "" && is.Line > 0:
		return fmt.Sprintf(" %s:%d", is.File, is.Line)
	case is.File != "":
		return " " + is.File
	}
	return ""
}
