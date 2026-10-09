package main

// Agent List
//
// `harness agent list` shows every harness whose table carries a source:
// its stable, package, pinned SHA (short; full with --json), the declaring
// file, and whether the stable's local clone — as of its last stable
// update — holds a newer default-branch commit than the pin. It is
// informational only: pure local reads, never a fetch, never an upgrade
// (SPEC-0026 REQ-12). Each row also carries the pinned manifest's SPDX
// license (REQ-3).
//
// Governing: ADR-0044 (agent package stables), SPEC-0026 REQ-12, REQ-3.
//
// @joestump-agent 10/02/2026 - Added for harness#815.

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/stump-wtf/harness/internal/agentpkg"
	"github.com/stump-wtf/harness/internal/cliui"
	"github.com/stump-wtf/harness/internal/config"
	"github.com/stump-wtf/harness/internal/core"
)

// agentListRow is one package-sourced harness, the JSON shape of
// `harness agent list --json` (the full SHA lives here, per REQ-12).
type agentListRow struct {
	Name           string `json:"name"`
	File           string `json:"file"`
	Stable         string `json:"stable"`
	Package        string `json:"package"`
	Pin            string `json:"pin"`
	NewerAvailable bool   `json:"newer_available"`
	// License is the pinned manifest's SPDX expression, "" when the package
	// declares none or its pin is not on disk (SPEC-0026 REQ-3).
	License string `json:"license"`
}

func newAgentListCmd(g *globalOpts) *cobra.Command {
	return &cobra.Command{
		Use:           "list",
		Short:         "list package-sourced harnesses and their pins (no fetch, no upgrade)",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAgentList(cmd, g.opts())
		},
	}
}

// runAgentList walks the global config plus any discovered project file,
// both read-only. Staleness comes from the clone as the last
// `harness agent stable update` left it (REQ-12).
func runAgentList(cmd *cobra.Command, o verbOpts) error {
	var rows []agentListRow

	cfg, err := config.Load(o.configPath)
	if err != nil {
		return fmt.Errorf("agent: load config: %w", err)
	}
	rows = append(rows, agentRows(o.configPath, cfg)...)

	if proj, perr := config.DiscoverProjectExcluding(o.configPath); perr == nil && proj != nil && proj.Config != nil {
		rows = append(rows, agentRows(proj.ConfigPath, proj.Config)...)
	}

	if cliui.JSON() {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}
	if len(rows) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "agent: no package-sourced harnesses")
		return nil
	}
	t := NewTable(cmd.OutOrStdout(), "NAME", "FILE", "PACKAGE", "PIN", "NEWER", "LICENSE")
	for _, r := range rows {
		newer := ""
		if r.NewerAvailable {
			newer = "yes"
		}
		license := r.License
		if license == "" {
			license = "none"
		}
		t.Row(r.Name, r.File, r.Stable+"/"+r.Package, shortSHA(r.Pin), newer, license)
	}
	t.Flush()
	fmt.Fprintln(cmd.OutOrStdout(), "agent: staleness is read from the clone as the last `harness agent stable update` left it; nothing fetched")
	return nil
}

// agentRows builds the rows for one declaring file's harnesses.
func agentRows(file string, cfg *core.Config) []agentListRow {
	var rows []agentListRow
	for _, name := range cfg.HarnessOrder {
		h := cfg.Harnesses[name]
		if h.PackageSource == "" {
			continue
		}
		src, err := agentpkg.ParseSource(h.PackageSource)
		if err != nil {
			continue
		}
		rows = append(rows, agentListRow{
			Name:           name,
			File:           file,
			Stable:         src.Stable,
			Package:        src.Package,
			Pin:            src.SHA,
			NewerAvailable: agentpkg.NewerThanPin(src.Stable, src.SHA),
			License:        pinLicense(src),
		})
	}
	return rows
}

// pinLicense reads the license from the installed pin's manifest — the terms
// the harness actually runs under, not the clone's newer ones. A pin missing
// from disk reads as "" here; the doctor row reports it.
func pinLicense(src agentpkg.Source) string {
	man, err := agentpkg.LoadManifest(agentpkg.ManifestPath(src))
	if err != nil {
		return ""
	}
	return man.Package.License
}
