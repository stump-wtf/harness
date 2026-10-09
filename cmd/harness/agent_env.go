package main

// Agent package declared environment (issue #930): the env_file skeleton
// install ends with, and the doctor row that checks each package-sourced
// harness's declared variables against what the daemon reports its child
// would see. Names only, everywhere — a value is never read into output.
//
// Governing: ADR-0044 (agent package stables), ADR-0038 (secret
// references), SPEC-0026 REQ-3, REQ-12.
//
// @joestump-agent 10/09/2026 - Added for harness#930.

import (
	"fmt"
	"io"
	"strings"

	"github.com/stump-wtf/harness/internal/agentpkg"
	"github.com/stump-wtf/harness/internal/cliui"
	"github.com/stump-wtf/harness/internal/protocol"
)

// printEnvSkeleton ends install's output with a ready-to-copy env_file
// skeleton for the package's [[env]] declarations: names and description
// comments, never a value. Nothing prints when the package declares none.
func printEnvSkeleton(w io.Writer, harness string, man *agentpkg.Manifest) {
	lines := agentpkg.EnvFileSkeleton(man)
	if len(lines) == 0 {
		return
	}
	fmt.Fprintf(w, "agent: the package expects these environment variables; add them to [harness.%s] env_file (or the daemon's environment) and fill in the values:\n", harness)
	for _, l := range lines {
		fmt.Fprintln(w, l)
	}
}

// packageEnvChecks builds one doctor row per package-sourced harness that
// declares environment variables (SPEC-0026 REQ-12), from the running
// daemon's own report of which declared names its child would see — its
// env_file list layered over the daemon's environment, the composition the
// spawn uses. A missing required variable fails the row; a missing optional
// one warns with its description; every name set passes. The rows carry
// names and descriptions only: the daemon never sends a value, and nothing
// here could print one.
//
// A daemon older than ProtoMinor 26 sends no declarations, so supported is
// false; a package-sourced harness whose installed manifest declares
// variables then gets a warn row saying the check could not run, rather
// than a silent pass.
func packageEnvChecks(c agentsListClient, supported bool) []check {
	hs, err := c.List()
	if err != nil {
		// The daemon and harnesses rows already report the failure.
		return nil
	}
	var rows []check
	for _, h := range hs {
		if h.Source == "" {
			continue
		}
		const name = "agent_env"
		who := h.Name + ": "
		if !supported {
			if !manifestDeclaresEnv(h.Source) {
				continue
			}
			rows = append(rows, check{
				name:   name,
				level:  cliui.LevelWarn,
				detail: who + "the daemon predates package environment reporting, so the declared variables were not checked",
				hint:   "restart the daemon to pick up the new binary",
			})
			continue
		}
		if len(h.PackageEnv) == 0 {
			continue
		}
		var missingReq, missingOpt []string
		for _, ev := range h.PackageEnv {
			if ev.Set {
				continue
			}
			if ev.Required {
				missingReq = append(missingReq, ev.Name)
				continue
			}
			opt := ev.Name
			if ev.Description != "" {
				opt += " (" + ev.Description + ")"
			}
			missingOpt = append(missingOpt, opt)
		}
		var parts []string
		if len(missingReq) > 0 {
			parts = append(parts, "required unset: "+strings.Join(missingReq, ", "))
		}
		if len(missingOpt) > 0 {
			parts = append(parts, "optional unset: "+strings.Join(missingOpt, ", "))
		}
		switch {
		case len(missingReq) > 0:
			rows = append(rows, check{
				name:   name,
				level:  cliui.LevelError,
				detail: who + strings.Join(parts, "; "),
				hint:   fmt.Sprintf("set each name in [harness.%s] env_file (or the daemon's environment) — `harness agent info` lists what each is for", h.Name),
			})
		case len(missingOpt) > 0:
			rows = append(rows, check{
				name:   name,
				level:  cliui.LevelWarn,
				detail: who + strings.Join(parts, "; "),
				hint:   fmt.Sprintf("optional: set it in [harness.%s] env_file if the harness needs it", h.Name),
			})
		default:
			rows = append(rows, check{
				name:   name,
				level:  cliui.LevelSuccess,
				detail: who + fmt.Sprintf("all %d declared variable(s) set", len(h.PackageEnv)),
			})
		}
	}
	return rows
}

// manifestDeclaresEnv reports whether the installed pin source names
// declares any [[env]] variable, read from local disk. A pin that cannot be
// read declares nothing here; the agent_pins row reports a missing one.
func manifestDeclaresEnv(source string) bool {
	src, err := agentpkg.ParseSource(source)
	if err != nil {
		return false
	}
	man, err := agentpkg.LoadManifest(agentpkg.ManifestPath(src))
	return err == nil && len(man.Env) > 0
}

// packageEnvCell renders one declared variable for describe: its name and
// flags, then whether it is set and from where. An unset required variable
// is amber, the one cell an operator must act on.
func packageEnvCell(t *Table, ev protocol.PackageEnvStatus) string {
	flags := "optional"
	if ev.Required {
		flags = "required"
	}
	if ev.Secret {
		flags += ", secret"
	}
	cell := fmt.Sprintf("%s (%s) — ", ev.Name, flags)
	switch {
	case ev.Set:
		return t.faintPlain(cell + "set (" + ev.From + ")")
	case ev.Required:
		return t.amberBold(cell + "unset")
	default:
		return t.faintPlain(cell + "unset")
	}
}
