// Command harness is the thin, scriptable CLI/TUI client.
//
// Governing: ADR-0001 (Go + Charmbracelet; the TUI and scriptable verbs live
// here) and ADR-0002 (the client owns nothing durable — it dials the daemon,
// renders, and can die at any time; the CLI is the supported programmatic
// surface). SPEC-0002 (the verbs mirror the control plane 1:1) and SPEC-0003
// (list renders the state glyphs). Each verb is a one-shot: dial the socket,
// issue one request, print (human or --json), exit.
//
// The command tree itself lives in root.go (ADR-0016: Cobra owns dispatch);
// main is only the entry point, so the argv contract has exactly one home.
package main

import (
	"fmt"
	"os"
	"strings"

	"gitea.stump.rocks/stump.wtf/harness/internal/buildinfo"
	"gitea.stump.rocks/stump.wtf/harness/internal/client"
	"gitea.stump.rocks/stump.wtf/harness/internal/cliui"
	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

func main() {
	// Seed the JSON mode from a raw argv scan before Cobra runs. Help and usage
	// render during Execute, ahead of any PersistentPreRunE, so waiting for the
	// resolved value would emit a styled, ANSI-laden help screen to a caller who
	// asked for machine-readable output. PersistentPreRunE re-applies the
	// resolved value afterwards, which is what lets HARNESS_JSON work for
	// everything except help.
	cliui.SetJSON(hasJSONArg(os.Args))

	root := newRootCmd()
	if err := root.Execute(); err != nil {
		os.Exit(cliui.Fatal(err))
	}
}

// verbOpts carries the resolved flags/positionals for a verb.
type verbOpts struct {
	socket     string
	configPath string
	json       bool
	lines      int
	follow     bool
	ro         bool
	all        bool // start/restart --all: apply to every harness
	name       string
}

// run dispatches one verb. Every verb dials the daemon fresh (thin client,
// ADR-0002).
//
// Argument validation (name required / rejected / optional) now happens in the
// command tree via nameArity, so this is pure dispatch. Only project scoping —
// which is lexical, not argument-shaped — remains here.
func run(verb string, o verbOpts) error {
	switch verb {
	case "list":
		return withClient(o, nil, cmdList)
	case "ps":
		// Compose-style listing: project-scoped inside a project, an alias
		// for `list` outside one (SPEC-0004 REQ "Project-Scoped Verbs";
		// ADR-0009).
		return cmdPs(o)
	case "up":
		// Bring the enclosing project up (SPEC-0004 REQ "Bring Up"). cmdUp
		// discovers/parses the project file before dialing so a missing
		// harness.toml never touches the daemon.
		return cmdUp(o)
	case "down":
		// Tear the project down (SPEC-0004 REQ "Tear Down"). NAME is an
		// optional explicit project (covers a deleted project file).
		return cmdDown(o)
	case "rm":
		// Remove ONE registered harness (SPEC-0004 REQ "Remove") — the
		// single-member tear-down. Scoped like every other name-taking verb:
		// a bare NAME inside a project resolves to <project>/NAME. The
		// command tree's nameRequired arity already rejects a missing name.
		return withClient(o, nil, projectScoped(verb, false, cmdRm))
	case "describe":
		// Scoped like every other name-taking verb: a bare NAME inside a
		// project resolves to <project>/NAME (SPEC-0004).
		return withClient(o, nil, projectScoped(verb, false, cmdDescribe))
	case "start", "stop", "restart":
		// projectScoped resolves a bare NAME to <project>/NAME inside a
		// project (SPEC-0004 REQ "Project-Scoped Verbs") and is a no-op
		// outside one; --all keeps its daemon-wide meaning for these
		// lifecycle verbs only.
		return withClient(o, nil, projectScoped(verb, true, lifecycle(verb)))
	case "logs":
		return withClient(o, nil, projectScoped(verb, false, cmdLogs))
	case "profiles":
		return withClient(o, nil, cmdProfiles)
	case "use-profile":
		return withClient(o, nil, cmdUseProfile)
	case "reload":
		return withClient(o, nil, cmdReload)
	case "attach":
		// Attach launches its own Bubble Tea program (alt-screen) and manages
		// its own daemon connection, so it doesn't go through withClient. The
		// bare-name resolution is purely lexical (no daemon round-trip), so
		// applying it here is safe for the PTY path — only the name string
		// handed to the TUI changes (SPEC-0004 project scoping).
		return cmdAttach(scopeVerbName(o, false))
	default:
		// Unreachable via the command tree, which rejects unknown verbs before
		// dispatch. Kept as a guard so a future command added to the tree but
		// not to this switch fails loudly rather than silently doing nothing.
		return fmt.Errorf("unknown command %q (run `harness -h` for the list)", verb)
	}
}

// withClient dials, runs fn, and closes. preErr short-circuits an argument
// error before dialing.
func withClient(o verbOpts, preErr error, fn func(*client.Client, verbOpts) error) error {
	if preErr != nil {
		return preErr
	}
	// Attach subscribes to nothing special; one-shot verbs skip events too.
	c, err := client.Dial(o.socket, buildinfo.Version, nil)
	if err != nil {
		return err
	}
	defer c.Close()
	return fn(c, o)
}

// lifecycle wraps start/stop/restart into one handler. When o.all is set it
// resolves the verb against every harness the daemon knows (in the order the
// daemon returns them); otherwise it applies to the single named harness.
func lifecycle(verb string) func(*client.Client, verbOpts) error {
	return func(c *client.Client, o verbOpts) error {
		if o.all {
			return lifecycleAll(c, o, verb)
		}
		var (
			info protocol.HarnessInfo
			err  error
		)
		switch verb {
		case "start":
			info, err = c.Start(o.name)
		case "stop":
			info, err = c.Stop(o.name)
		case "restart":
			info, err = c.Restart(o.name)
		}
		if err != nil {
			return err
		}
		if o.json {
			return printJSON(info)
		}
		fmt.Printf("%s %s → %s\n", stateGlyph(info.State), info.Name, info.State)
		return nil
	}
}

// lifecycleAll applies verb to every harness. A failure on one harness does
// not abort the rest — we collect per-harness errors and surface them at the
// end so `start --all` brings up as many harnesses as possible rather than
// stopping at the first one that's wedged. JSON output is an array of the
// per-harness HarnessInfo results.
func lifecycleAll(c *client.Client, o verbOpts, verb string) error {
	hs, err := c.List()
	if err != nil {
		return fmt.Errorf("--all: list harnesses: %w", err)
	}
	if len(hs) == 0 {
		return fmt.Errorf("--all: no harnesses configured")
	}
	// Animated path: on a real terminal (and only outside --json) the run
	// renders through the Bubble Tea lifecycle view — spinner on the harness
	// being acted on, SPEC-0003 state per completed row, overall progress.
	// Pipes and scripts keep the plain line-per-harness contract.
	if !o.json && cliui.WriterIsTTY(os.Stdout) {
		return runLifecycleAnimated(verb, c, harnessNames(hs))
	}
	var (
		results []protocol.HarnessInfo
		errs    []string
	)
	for _, h := range hs {
		var (
			info protocol.HarnessInfo
			e    error
		)
		switch verb {
		case "start":
			info, e = c.Start(h.Name)
		case "stop":
			info, e = c.Stop(h.Name)
		case "restart":
			info, e = c.Restart(h.Name)
		}
		if e != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", h.Name, e))
			continue
		}
		results = append(results, info)
		if !o.json {
			fmt.Printf("%s %s → %s\n", stateGlyph(info.State), info.Name, info.State)
		}
	}
	if o.json {
		_ = printJSON(results)
	}
	if len(errs) > 0 {
		return fmt.Errorf("%d failed:\n  %s", len(errs), strings.Join(errs, "\n  "))
	}
	return nil
}

// harnessNames projects a list result onto the ordered name slice the
// animated lifecycle view walks.
func harnessNames(hs []protocol.HarnessInfo) []string {
	names := make([]string, len(hs))
	for i, h := range hs {
		names[i] = h.Name
	}
	return names
}

// usage and daemonUsage live in help.go (styled via theme palette,
// plain fallback for non-TTY/--json).
