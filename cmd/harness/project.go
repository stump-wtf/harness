package main

// Governing: ADR-0009 (project-scoped config and compose commands) and
// SPEC-0004 REQ "Bring Up" (`harness up` discovers + parses the repo-root
// harness.toml, sends project_up, prints a one-shot status table, detached),
// REQ "Tear Down" (`harness down` sends project_down; stop + deregister, the
// global config is never touched), and REQ "Project-Scoped Verbs" (`ps` and a
// bare NAME to logs/start/stop/restart resolve against the enclosing
// project's namespace first). REQ "Error Handling Standards": every error is
// wrapped with verb context, config sentinels pass through errors.Is, and the
// daemon's structured ERROR message is surfaced verbatim.

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"gitea.stump.rocks/stump.wtf/harness/internal/client"
	"gitea.stump.rocks/stump.wtf/harness/internal/config"
	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// cmdUp implements `harness up` (SPEC-0004 REQ "Bring Up"): discover the
// enclosing project (up-walk from cwd, #29), send project_up with the full
// desired set (reconcile semantics live in the daemon), print a one-shot
// status table of the project's harnesses, and return to the shell.
//
// Discovery runs before dialing so a missing harness.toml fails fast with the
// ErrNoProjectFound sentinel and nothing is ever sent to the daemon; a missing
// daemon then fails with the exact same dial error every other client verb
// produces (withClient returns it unwrapped for cliui to classify).
func cmdUp(o verbOpts) error {
	proj, err := config.DiscoverProject()
	if err != nil {
		return fmt.Errorf("harness up: %w", err)
	}
	return withClient(o, nil, func(c *client.Client, o verbOpts) error {
		data, err := c.ProjectUp(proj.Name, wireHarnesses(proj))
		if err != nil {
			// The daemon's structured ERROR message rides through verbatim
			// inside the wrap (protocol.ErrorMsg implements error).
			return fmt.Errorf("harness up: %w", err)
		}
		if o.json {
			return printJSON(data)
		}
		return printHarnessTable(os.Stdout, data.Harnesses)
	})
}

// cmdDown implements `harness down [PROJECT]` (SPEC-0004 REQ "Tear Down"):
// stop and deregister every harness of the project so the daemon retains no
// record. The global config file is never touched — down only speaks to the
// daemon. An explicit PROJECT positional covers the "I deleted the project
// file first" case (SPEC-0004 design open question, resolved here): with an
// argument, no discovery runs at all.
func cmdDown(o verbOpts) error {
	name := o.name
	if name == "" {
		proj, err := config.DiscoverProject()
		if err != nil {
			if errors.Is(err, config.ErrNoProjectFound) {
				return fmt.Errorf("harness down: %w — pass the project explicitly: harness down NAME", err)
			}
			return fmt.Errorf("harness down: %w", err)
		}
		name = proj.Name
	}
	return withClient(o, nil, func(c *client.Client, o verbOpts) error {
		data, err := c.ProjectDown(name)
		if err != nil {
			return fmt.Errorf("harness down: %w", err)
		}
		if o.json {
			return printJSON(data)
		}
		fmt.Printf("project %q down — %d harness(es) stopped and deregistered\n",
			data.Project, len(data.Removed))
		return nil
	})
}

// cmdPs implements `harness ps` (SPEC-0004 REQ "Project-Scoped Verbs"):
// inside a project it lists only that project's harnesses; outside a project
// it is a plain alias for `list` (global scope, unchanged behavior).
func cmdPs(c *client.Client, o verbOpts) error {
	proj, err := projectScope()
	if err != nil {
		return fmt.Errorf("harness ps: %w", err)
	}
	hs, err := c.List()
	if err != nil {
		return err
	}
	if proj != nil {
		hs = filterProjectHarnesses(hs, proj.Name)
	}
	if o.json {
		return printJSON(hs)
	}
	return printHarnessTable(os.Stdout, hs)
}

// projectScope reports the enclosing project, or nil when cwd is not inside
// one (ErrNoProjectFound is the expected "no scope" answer, not a failure). A
// discoverable-but-malformed project file is returned as an error so it is
// surfaced, never silently ignored (SPEC-0004 REQ "Error Handling Standards").
func projectScope() (*config.Project, error) {
	proj, err := config.DiscoverProject()
	if err != nil {
		if errors.Is(err, config.ErrNoProjectFound) {
			return nil, nil
		}
		return nil, err
	}
	return proj, nil
}

// projectScoped wraps a name-taking verb handler (logs/start/stop/restart)
// with SPEC-0004 bare-name resolution: inside a project, a bare NAME resolves
// project-local first (<project>/<name>), falling back to an exact global
// match. A NAME containing "/" is already fully qualified and passes through
// untouched, as does everything when cwd is not inside a project (global
// behavior unchanged). --all keeps its existing daemon-wide meaning.
func projectScoped(fn func(*client.Client, verbOpts) error) func(*client.Client, verbOpts) error {
	return func(c *client.Client, o verbOpts) error {
		if o.name == "" || o.all || strings.Contains(o.name, "/") {
			return fn(c, o)
		}
		proj, err := projectScope()
		if err != nil {
			return err
		}
		if proj == nil {
			return fn(c, o)
		}
		resolved, err := resolveScopedName(c, proj.Name, o.name)
		if err != nil {
			return err
		}
		o.name = resolved
		return fn(c, o)
	}
}

// resolveScopedName maps a bare project-local name onto the daemon's
// registry: <project>/<name> wins when registered; otherwise an exact global
// match is used; otherwise the error names both candidates so the user sees
// exactly what was tried. Ambiguity is impossible by construction — the
// qualified and bare names can never collide (SPEC-0004 REQ "Project Naming
// And Namespacing").
func resolveScopedName(c *client.Client, project, name string) (string, error) {
	qualified := project + "/" + name
	hs, err := c.List()
	if err != nil {
		return "", fmt.Errorf("resolve %q in project %q: %w", name, project, err)
	}
	var hasLocal, hasGlobal bool
	for _, h := range hs {
		switch h.Name {
		case qualified:
			hasLocal = true
		case name:
			hasGlobal = true
		}
	}
	switch {
	case hasLocal:
		return qualified, nil
	case hasGlobal:
		return name, nil
	default:
		return "", fmt.Errorf("no harness %q in project %q (looked for %q, then a global %q)",
			name, project, qualified, name)
	}
}

// filterProjectHarnesses keeps only the harnesses whose provenance is the
// named project (the daemon stamps HarnessInfo.Project at registration, so
// this filters on ownership, not on a string prefix of the display name).
func filterProjectHarnesses(hs []protocol.HarnessInfo, project string) []protocol.HarnessInfo {
	out := make([]protocol.HarnessInfo, 0, len(hs))
	for _, h := range hs {
		if h.Project == project {
			out = append(out, h)
		}
	}
	return out
}

// wireHarnesses converts the parsed project definitions (in file order) into
// the protocol's project_up payload. Enabled is carried through faithfully:
// the wire default is false and the daemon only autostarts enabled harnesses,
// so dropping it would register everything permanently stopped (SPEC-0004 REQ
// "Project File Schema": identical field meanings).
func wireHarnesses(proj *config.Project) []protocol.ProjectHarness {
	out := make([]protocol.ProjectHarness, 0, len(proj.Config.HarnessOrder))
	for _, name := range proj.Config.HarnessOrder {
		h := proj.Config.Harnesses[name]
		out = append(out, protocol.ProjectHarness{
			Name:           name,
			Cmd:            h.Cmd,
			Args:           h.Args,
			Workdir:        h.Workdir,
			EnvFile:        h.EnvFile,
			RestartDelayMs: h.RestartDelay.Milliseconds(),
			Backend:        string(h.Backend),
			Description:    h.Description,
			TmuxSocket:     h.TmuxSocket,
			Enabled:        h.Enabled,
		})
	}
	return out
}
