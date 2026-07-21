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
	"os"

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
		var sub string
		var daemonArgs []string
		if len(rest) > 0 {
			sub = rest[0]
			if len(rest) > 1 {
				daemonArgs = rest[1:]
			}
		}
		switch sub {
		case "", "run", "start":
			runDaemon(daemonArgs)
		case "stop":
			opts := verbOpts{socket: *socket, configPath: *configPath, json: *jsonOut}
			os.Exit(cliui.Fatal(cmdStopDaemon(opts)))
		case "status":
			opts := verbOpts{socket: *socket, configPath: *configPath, json: *jsonOut}
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
	rest := gfs.Args()
	if len(rest) > 0 {
		rest = rest[1:]
	}
	// Parse flags and positionals in any order. Go's flag package stops at the
	// first non-flag, so we loop: parse, take one positional, parse the rest —
	// this makes `harness logs ticker --lines 3` behave like the flags-first form.
	name := parseInterleaved(vfs, rest)
	opts := verbOpts{socket: *socket, configPath: *configPath, json: *vJSON, lines: *lines, follow: *follow, ro: *ro, name: name}

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

// verbOpts carries the resolved flags/positionals for a verb.
type verbOpts struct {
	socket     string
	configPath string
	json       bool
	lines      int
	follow     bool
	ro         bool
	name       string
}

// run dispatches one verb. Every verb dials the daemon fresh (thin client,
// ADR-0002).
func run(verb string, o verbOpts) error {
	switch verb {
	case "list":
		return withClient(o, nil, cmdList)
	case "describe":
		return withClient(o, requireName(o), cmdDescribe)
	case "start", "stop", "restart":
		return withClient(o, requireName(o), lifecycle(verb))
	case "logs":
		return withClient(o, requireName(o), cmdLogs)
	case "profiles":
		return withClient(o, nil, cmdProfiles)
	case "use-profile":
		return withClient(o, requireName(o), cmdUseProfile)
	case "reload":
		return withClient(o, nil, cmdReload)
	case "daemon-info":
		return withClient(o, nil, cmdDaemonInfo)
	case "attach":
		return withClient(o, requireName(o), cmdAttach)
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

// lifecycle wraps start/stop/restart into one handler.
func lifecycle(verb string) func(*client.Client, verbOpts) error {
	return func(c *client.Client, o verbOpts) error {
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

func usage() {
	fmt.Fprint(os.Stderr, `harness — systemctl for your agents

usage:
  harness [--socket PATH] [--json] <command> [args]
  harness daemon <subcommand> [daemon-flags]

commands:
  list                 list configured harnesses and their state (default)
  describe NAME        show one harness in detail
  start NAME           start (enable) a harness
  stop NAME            stop (disable) a harness
  restart NAME         restart a harness (clears a failed latch)
  logs NAME [--lines N] [--follow]   show a harness's log tail
  profiles             list profiles (active one flagged)
  use-profile NAME     activate a profile
  reload               re-read the daemon config
  daemon-info          show daemon status
  doctor               run health checks (config, daemon, harnesses)
  attach NAME [--ro]   attach to a harness's terminal

daemon subcommands (see "harness daemon --help"):
  daemon start         run the supervision daemon (ADR-0005 ExecStart; alias: run)
  daemon stop          gracefully stop the running daemon
  daemon status        alias for daemon-info

flags:
  --socket PATH        daemon socket (default $XDG_RUNTIME_DIR/harness.sock)
  --json               machine-readable output
`)
}

// daemonUsage prints the help for the `harness daemon` subcommand group.
// Routed when the user runs `harness daemon --help|-h|help` or an unknown
// subcommand.
func daemonUsage() {
	fmt.Fprint(os.Stderr, `harness daemon — supervise long-running harnesses

usage:
  harness daemon <subcommand> [flags]

subcommands:
  start                run the supervision daemon in the foreground
                       (alias: run; bare "harness daemon" defaults to start)
  stop                 gracefully stop the running daemon (SIGTERM)
  status               show daemon status (alias: daemon-info)

start/run flags:
  --config PATH        path to harness.toml
  --socket PATH        control/data plane socket path
  --scrollback N       per-harness scrollback ring depth (lines)
  --log-level LEVEL    debug, info (default), warn, error
  --log-file PATH      append logs to this file instead of stderr
  --detach             fork into the background; redirect stdio to --log-file
                       (default: $XDG_STATE_HOME/harness/harness-daemon.log)
  --ssh                enable the remote Wish SSH server
  --ssh-listen H:P     SSH bind address (overrides [server] listen)
  --version            print version and exit

examples:
  harness daemon start --detach       run in the background
  harness daemon stop                 stop the running daemon
  harness daemon status               check if the daemon is up
`)
}
