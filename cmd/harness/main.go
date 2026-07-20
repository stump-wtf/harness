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
	"gitea.stump.rocks/stump.wtf/harness/internal/config"
	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

func main() {
	gfs := flag.NewFlagSet("harness", flag.ExitOnError)
	socket := gfs.String("socket", protocol.DefaultSocketPath(), "daemon socket path")
	configPath := gfs.String("config", config.DefaultPath(), "harnessd.toml path (TUI harness form writes here)")
	jsonOut := gfs.Bool("json", false, "machine-readable JSON output")
	showVersion := gfs.Bool("version", false, "print version and exit")
	gfs.Usage = usage
	_ = gfs.Parse(os.Args[1:])

	if *showVersion {
		fmt.Printf("harness %s\n", buildinfo.Version)
		return
	}

	// No verb → open the cockpit TUI (SPEC-0001: `harness` with no args opens
	// the keyboard-driven dashboard onto the daemon).
	verb := gfs.Arg(0)
	if verb == "" {
		if err := runTUI(*socket, *configPath); err != nil {
			fmt.Fprintf(os.Stderr, "harness: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// `harness daemon` runs the long-lived supervision daemon in-process. It is
	// the ADR-0005 systemd ExecStart (`harness daemon`) and replaces the
	// historical standalone `harnessd` binary. It owns its own flag set (its
	// flags do not overlap with the client verbs'), so we hand it the remaining
	// args and exit on its terms.
	if verb == "daemon" {
		runDaemon(gfs.Args()[1:])
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
	opts := verbOpts{socket: *socket, json: *vJSON, lines: *lines, follow: *follow, ro: *ro, name: name}

	if err := run(verb, opts); err != nil {
		fmt.Fprintf(os.Stderr, "harness: %v\n", err)
		os.Exit(1)
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
	socket string
	json   bool
	lines  int
	follow bool
	ro     bool
	name   string
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
		usage()
		return fmt.Errorf("unknown command %q", verb)
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
  harness daemon [daemon-flags]            run the supervision daemon

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
  attach NAME [--ro]   attach to a harness's terminal
  daemon               run the supervision daemon (ADR-0005 ExecStart)

flags:
  --socket PATH        daemon socket (default $XDG_RUNTIME_DIR/harnessd.sock)
  --json               machine-readable output

daemon flags (see "harness daemon -h"):
  --config PATH        path to harnessd.toml
  --socket PATH        control/data plane socket path
  --scrollback N       per-harness scrollback ring depth (lines)
  --ssh                enable the remote Wish SSH server
  --ssh-listen HOST:PORT   SSH bind address (overrides [server] listen)
`)
}
