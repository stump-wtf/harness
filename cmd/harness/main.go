// Command harness is the thin, scriptable CLI/TUI client.
//
// Governing: ADR-0001 (Go + Charmbracelet; the TUI and scriptable verbs live
// here) and ADR-0002 (the client owns nothing durable — it dials the daemon,
// renders, and can die at any time; the CLI is the supported programmatic
// surface). SPEC-0002 (the verbs mirror the control plane 1:1) and SPEC-0003
// (list renders the state glyphs). Each verb is a one-shot: dial the socket,
// issue one request, print (human or --json), exit.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"gitea.stump.rocks/stump.wtf/harness/internal/buildinfo"
	"gitea.stump.rocks/stump.wtf/harness/internal/client"
	"gitea.stump.rocks/stump.wtf/harness/internal/cliui"
	"gitea.stump.rocks/stump.wtf/harness/internal/config"
	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

func main() {
	gfs := flag.NewFlagSet("harness", flag.ExitOnError)
	socket := gfs.String("socket", protocol.DefaultSocketPath(), "daemon socket path")
	configPath := gfs.String("config", config.DefaultPath(), "harness.toml path (TUI harness form writes here)")
	jsonOut := gfs.Bool("json", false, "machine-readable JSON output")
	showVersion := gfs.Bool("version", false, "print version and exit")
	gfs.Usage = usage
	_ = gfs.Parse(os.Args[1:])
	cliui.SetJSON(*jsonOut)

	if *showVersion {
		fmt.Printf("harness %s\n", buildinfo.Version)
		return
	}

	// No verb → open the cockpit TUI (SPEC-0001: `harness` with no args opens
	// the keyboard-driven dashboard onto the daemon).
	verb := gfs.Arg(0)
	if verb == "" {
		if err := runTUI(*socket, *configPath); err != nil {
			os.Exit(cliui.Fatal(err))
		}
		return
	}

	// `harness daemon` is a subcommand group (mirrors systemctl: daemon run,
	// daemon stop, daemon status). Bare `harness daemon` with no sub is
	// equivalent to `harness daemon run` — the ADR-0005 systemd ExecStart
	// form, kept for backward compatibility.
	if verb == "daemon" {
		// Parse the daemon subcommand (start/stop/status/help). Args after
		// the verb are either the subcommand token + its args, or flags the
		// caller passed before the subcommand. Guard the slice — bare
		// `harness daemon` (no sub) is allowed and means `start`.
		rest := gfs.Args()[1:] // drop the "daemon" token
		sub, daemonArgs := parseDaemonArgs(rest)
		switch sub {
		case "", "run", "start":
			runDaemon(daemonArgs)
		case "stop":
			opts := resolveDaemonOpts(daemonArgs, *socket, *configPath, *jsonOut)
			os.Exit(cliui.Fatal(cmdStopDaemon(opts)))
		case "status":
			opts := resolveDaemonOpts(daemonArgs, *socket, *configPath, *jsonOut)
			if err := withClient(opts, nil, cmdDaemonInfo); err != nil {
				os.Exit(cliui.Fatal(err))
			}
		case "-h", "--help", "help":
			daemonUsage()
		default:
			os.Exit(cliui.FatalMsg("unknown command",
				fmt.Sprintf("unknown daemon subcommand %q (start, stop, status)", sub),
				"try `harness daemon --help`"))
		}
		return
	}

	// Per-verb flags (also re-declares --json so it may follow the verb).
	vfs := flag.NewFlagSet(verb, flag.ExitOnError)
	vJSON := vfs.Bool("json", *jsonOut, "machine-readable JSON output")
	lines := vfs.Int("lines", 200, "logs: number of trailing lines")
	follow := vfs.Bool("follow", false, "logs: stream new output")
	ro := vfs.Bool("ro", false, "attach: read-only (ignore keystrokes)")
	all := vfs.Bool("all", false, "start/stop/restart: apply to every harness (name is ignored)")
	rest := gfs.Args()
	if len(rest) > 0 {
		rest = rest[1:]
	}
	// Parse flags and positionals in any order. Go's flag package stops at the
	// first non-flag, so we loop: parse, take one positional, parse the rest —
	// this makes `harness logs ticker --lines 3` behave like the flags-first form.
	name := parseInterleaved(vfs, rest)
	opts := verbOpts{socket: *socket, configPath: *configPath, json: *vJSON, lines: *lines, follow: *follow, ro: *ro, all: *all, name: name}

	// `doctor` owns its own reporting (tabular, no styled error box) and its
	// own exit code; bypass the generic run() → cliui.Fatal path.
	if verb == "doctor" {
		opts := verbOpts{socket: *socket, configPath: *configPath, json: *jsonOut}
		os.Exit(runDoctor(opts))
	}

	if err := run(verb, opts); err != nil {
		os.Exit(cliui.Fatal(err))
	}
}

// parseInterleaved parses fs against args where flags and a single positional
// (the harness/profile name) may appear in any order, returning the first
// positional. Go's flag package halts at the first non-flag token, so we parse
// in a loop, peeling off one positional per pass and re-parsing the remainder.
func parseInterleaved(fs *flag.FlagSet, args []string) string {
	var name string
	for len(args) > 0 {
		_ = fs.Parse(args)
		if fs.NArg() == 0 {
			break
		}
		if name == "" {
			name = fs.Arg(0)
		}
		args = fs.Args()[1:]
	}
	return name
}

// daemonSubcommands lists the tokens recognized as daemon subcommands. Used
// by parseDaemonArgs to distinguish `daemon --socket X stop` (flag + sub)
// from `daemon --socket X` (flag only, implicit start).
var daemonSubcommands = map[string]bool{
	"start": true, "run": true, "stop": true, "status": true,
	"help": true, "-h": true, "--help": true,
}

// resolveDaemonOpts parses the daemon-level flags (--socket, --config, --json)
// from daemonArgs — the flags parseDaemonArgs extracted from before/after the
// subcommand token — falling back to the global flag set's values for any flag
// not present. This ensures `harness daemon --socket X stop` connects to X,
// not the default socket the global flag set resolved.
func resolveDaemonOpts(daemonArgs []string, fallbackSocket, fallbackConfig string, fallbackJSON bool) verbOpts {
	fs := flag.NewFlagSet("daemon-opts", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	socket := fs.String("socket", fallbackSocket, "")
	configPath := fs.String("config", fallbackConfig, "")
	jsonOut := fs.Bool("json", fallbackJSON, "")
	_ = fs.Parse(daemonArgs)
	return verbOpts{socket: *socket, configPath: *configPath, json: *jsonOut}
}

// daemonFlagValues lists daemon flags that consume a following argument (i.e.
// not boolean flags). Used by parseDaemonArgs to skip flag values when scanning
// for the subcommand token, so `--log-file stop` isn't misread as the verb.
var daemonFlagValues = map[string]bool{
	"-config": true, "--config": true,
	"-socket": true, "--socket": true,
	"-scrollback": true, "--scrollback": true,
	"-ssh-listen": true, "--ssh-listen": true,
	"-log-level": true, "--log-level": true,
	"-log-file": true, "--log-file": true,
}

// parseDaemonArgs splits the args after `daemon` into a subcommand and the
// remaining daemon flags. It scans for a known subcommand token, skipping flag
// values so `--log-file stop` isn't misread as the verb; if none is found but
// the first arg starts with `-`, the implicit subcommand is "" (meaning start)
// and all args are forwarded as daemon flags. A bare `daemon` (no args) also
// means "start".
func parseDaemonArgs(rest []string) (sub string, daemonArgs []string) {
	if len(rest) == 0 {
		return "", nil
	}
	skipNext := false
	for i, arg := range rest {
		if skipNext {
			skipNext = false
			continue
		}
		if daemonSubcommands[arg] {
			sub = arg
			daemonArgs = make([]string, 0, len(rest)-1)
			daemonArgs = append(daemonArgs, rest[:i]...)
			daemonArgs = append(daemonArgs, rest[i+1:]...)
			return sub, daemonArgs
		}
		if daemonFlagValues[arg] {
			skipNext = true
		}
	}
	if strings.HasPrefix(rest[0], "-") {
		return "", rest
	}
	return rest[0], rest[1:]
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
func run(verb string, o verbOpts) error {
	switch verb {
	case "list":
		return withClient(o, nil, cmdList)
	case "ps":
		// Compose-style listing: project-scoped inside a project, an alias
		// for `list` outside one (SPEC-0004 REQ "Project-Scoped Verbs";
		// ADR-0009). ps operates on the discovered scope only — a stray
		// positional would be silently ignored, so reject it.
		if err := rejectName(verb, o); err != nil {
			return err
		}
		return cmdPs(o)
	case "up":
		// Bring the enclosing project up (SPEC-0004 REQ "Bring Up"). cmdUp
		// discovers/parses the project file before dialing so a missing
		// harness.toml never touches the daemon. Unlike `down [PROJECT]`, up
		// takes no positional — reject one rather than silently bringing up
		// the discovered project.
		if err := rejectName(verb, o); err != nil {
			return err
		}
		return cmdUp(o)
	case "down":
		// Tear the project down (SPEC-0004 REQ "Tear Down"). NAME is an
		// optional explicit project (covers a deleted project file).
		return cmdDown(o)
	case "describe":
		// Scoped like every other name-taking verb: a bare NAME inside a
		// project resolves to <project>/NAME (SPEC-0004).
		return withClient(o, requireName(o), projectScoped(verb, false, cmdDescribe))
	case "start", "stop", "restart":
		// `--all` replaces the required name with "every harness the daemon
		// knows about"; without it, a name is still required. projectScoped
		// resolves a bare NAME to <project>/NAME inside a project (SPEC-0004
		// REQ "Project-Scoped Verbs") and is a no-op outside one; --all keeps
		// its daemon-wide meaning for these lifecycle verbs only.
		pre := requireName(o)
		if o.all {
			pre = nil
		}
		return withClient(o, pre, projectScoped(verb, true, lifecycle(verb)))
	case "logs":
		return withClient(o, requireName(o), projectScoped(verb, false, cmdLogs))
	case "profiles":
		return withClient(o, nil, cmdProfiles)
	case "use-profile":
		return withClient(o, requireName(o), cmdUseProfile)
	case "reload":
		return withClient(o, nil, cmdReload)
	case "daemon-info":
		return withClient(o, nil, cmdDaemonInfo)
	case "attach":
		// Attach launches its own Bubble Tea program (alt-screen) and manages
		// its own daemon connection, so it doesn't go through withClient. The
		// bare-name resolution is purely lexical (no daemon round-trip), so
		// applying it here is safe for the PTY path — only the name string
		// handed to the TUI changes (SPEC-0004 project scoping).
		if err := requireName(o); err != nil {
			return err
		}
		return cmdAttach(scopeVerbName(o, false))
	default:
		// Don't dump full usage() here — the styled error from cliui.Fatal
		// is the single calm message; the hint points at the help flag.
		return fmt.Errorf("unknown command %q (run `harness -h` for the list)", verb)
	}
}

// requireName returns a pre-flight error func when the verb needs a NAME arg.
func requireName(o verbOpts) error {
	if o.name == "" {
		return fmt.Errorf("this command requires a harness/profile name")
	}
	return nil
}

// rejectName errors when a no-argument verb (up, ps) was given a positional —
// silently ignoring it would act on the discovered project while the user
// believes they targeted something else.
func rejectName(verb string, o verbOpts) error {
	if o.name != "" {
		return fmt.Errorf("harness %s takes no arguments (got %q)", verb, o.name)
	}
	return nil
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

// usage and daemonUsage live in help.go (styled via theme palette,
// plain fallback for non-TTY/--json).
