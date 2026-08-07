package main

// Governing: ADR-0009 (project-scoped config and compose commands) and
// SPEC-0004 REQ "Bring Up" (`harness up` discovers + parses the repo-root
// harness.toml, sends project_up, prints a one-shot status table, detached),
// REQ "Tear Down" (`harness down` sends project_down; stop + deregister, the
// global config is never touched), and REQ "Project-Scoped Verbs" (`ps` and a
// bare NAME to describe/logs/start/stop/restart/attach resolve to
// <project>/<name> inside a project — purely lexically, no global fallback).
// REQ "Error Handling Standards": every error is wrapped with verb context,
// config sentinels pass through errors.Is, and the daemon's structured ERROR
// message is surfaced verbatim.

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"gitea.stump.rocks/stump.wtf/harness/internal/client"
	"gitea.stump.rocks/stump.wtf/harness/internal/config"
	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// activeConfigPath returns the global config path in effect for this
// invocation (--config override, else the conventional default). Project
// discovery excludes it so the active global config is never adopted as a
// project file, wherever it lives (SPEC-0004 REQ "Project File Discovery").
func activeConfigPath(o verbOpts) string {
	if o.configPath != "" {
		return o.configPath
	}
	return config.DefaultPath()
}

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
	proj, err := config.DiscoverProjectExcluding(activeConfigPath(o))
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
// argument, no discovery runs at all. The actionable "pass the project
// explicitly" hint renders via cliui's classify path (internal/cliui), like
// every other actionable error.
func cmdDown(o verbOpts) error {
	name := strings.TrimSpace(o.name)
	explicit := name != ""
	if !explicit {
		proj, err := config.DiscoverProjectExcluding(activeConfigPath(o))
		if err != nil {
			return fmt.Errorf("harness down: %w", err)
		}
		name = proj.Name
	}
	return withClient(o, nil, func(c *client.Client, o verbOpts) error {
		data, err := c.ProjectDown(name)
		// An explicit positional is often typed as the directory basename
		// ("My-Cool-Project") while discovery registered the sanitized form
		// ("my-cool-project"). Retry with the same normalization discovery
		// applies before giving up, so the escape hatch works for derived
		// names too — without breaking projects whose explicit [project].name
		// was registered verbatim (SPEC-0004 REQ "Project Naming And
		// Namespacing").
		if err != nil && explicit && isUnknownProject(err) {
			if sanitized := config.SanitizeProjectName(name); sanitized != name {
				if data2, err2 := c.ProjectDown(sanitized); err2 == nil {
					data, err = data2, nil
				}
			}
		}
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

// isUnknownProject reports whether err is the daemon's structured
// unknown_project ERROR (SPEC-0004 scenario "project_down on unknown
// project").
func isUnknownProject(err error) bool {
	var em *protocol.ErrorMsg
	return errors.As(err, &em) && em.Code == protocol.ErrUnknownProject
}

// cmdPs implements `harness ps` (SPEC-0004 REQ "Project-Scoped Verbs"):
// inside a project it lists only that project's harnesses; outside a project
// it is a plain alias for `list` (global scope, unchanged behavior — the
// fetch/render tail is shared with cmdList via renderHarnessList). Discovery
// runs before dialing, mirroring cmdUp, so a malformed project file never
// wastes a daemon connection.
func cmdPs(o verbOpts) error {
	proj, err := projectScope(o)
	if err != nil {
		return fmt.Errorf("harness ps: %w", err)
	}
	var filter string
	if proj != nil {
		filter = proj.Name
	}
	return withClient(o, nil, func(c *client.Client, o verbOpts) error {
		if err := renderHarnessList(c, o, filter); err != nil {
			return fmt.Errorf("harness ps: %w", err)
		}
		return nil
	})
}

// projectScope reports the enclosing project, or nil when cwd is not inside
// one (ErrNoProjectFound is the expected "no scope" answer, not a failure). A
// discoverable-but-malformed project file is returned as an error so it is
// surfaced, never silently ignored (SPEC-0004 REQ "Error Handling Standards").
func projectScope(o verbOpts) (*config.Project, error) {
	proj, err := config.DiscoverProjectExcluding(activeConfigPath(o))
	if err != nil {
		if errors.Is(err, config.ErrNoProjectFound) {
			return nil, nil
		}
		return nil, err
	}
	return proj, nil
}

// projectScoped wraps a name-taking verb handler (describe/logs/start/stop/
// restart) with SPEC-0004 bare-name resolution (scopeVerbName) and the
// verb-context error wrap REQ "Error Handling Standards" mandates at each
// layer boundary. honorAll gates the --all short-circuit to the lifecycle
// verbs — the only verbs --all means anything to — so `logs NAME --all`
// resolves exactly like `logs NAME`.
func projectScoped(verb string, honorAll bool, fn func(*client.Client, verbOpts) error) func(*client.Client, verbOpts) error {
	return func(c *client.Client, o verbOpts) error {
		if err := fn(c, scopeVerbName(o, honorAll)); err != nil {
			// %w keeps errors.Is/As classification intact (cliui.classify
			// unwraps dial/protocol errors through the verb-context wrap).
			return fmt.Errorf("harness %s: %w", verb, err)
		}
		return nil
	}
}

// scopeVerbName resolves o.name purely lexically (SPEC-0004 REQ
// "Project-Scoped Verbs": a bare name inside a project SHALL resolve to
// <project>/<name> — no fallback to a global harness, no registry lookup). A
// NAME containing "/" is already fully qualified and passes through
// untouched, as does everything when cwd is not inside a project (global
// behavior unchanged). When honorAll is set, --all keeps its daemon-wide
// meaning and skips resolution.
//
// This verb reuses global behavior, so a malformed ancestor project file must
// not break it: the parse error is surfaced as a one-line stderr warning
// (never silently swallowed — SPEC-0004 REQ "Error Handling Standards") and
// the verb proceeds exactly as if no project file existed. The project verbs
// (up/down/ps) keep the error fatal.
func scopeVerbName(o verbOpts, honorAll bool) verbOpts {
	if o.name == "" || strings.Contains(o.name, "/") || (honorAll && o.all) {
		return o
	}
	proj, err := projectScope(o)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: ignoring project file: %v — operating on global scope\n", err)
		return o
	}
	if proj == nil {
		return o
	}
	o.name = proj.Name + "/" + o.name
	return o
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
// so dropping it would register everything permanently stopped. Parsing
// already defaulted an absent `enabled` key to true (SPEC-0004 REQ "Bring
// Up": register and start each one), so the wire value here is the user's
// effective intent.
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
			Restart:        string(h.Restart),
			Backend:        string(h.Backend),
			Description:    h.Description,
			TmuxSocket:     h.TmuxSocket,
			Enabled:        h.Enabled,
		})
	}
	return out
}
