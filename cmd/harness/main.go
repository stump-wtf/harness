// Command harness is the thin TUI/CLI client.
//
// Governing: ADR-0001 (Go + Charmbracelet; the TUI and scriptable verbs live
// here) and ADR-0002 (the client owns nothing durable — it connects to the
// daemon, renders, and can die at any time). The Bubble Tea TUI and the RPC
// client land in later stories; this foundation binary establishes the entry
// point and the scriptable `list` verb reading the config directly.
package main

import (
	"flag"
	"fmt"
	"os"
	"text/tabwriter"

	"gitea.stump.rocks/stump.wtf/harness/internal/buildinfo"
	"gitea.stump.rocks/stump.wtf/harness/internal/config"
	"gitea.stump.rocks/stump.wtf/harness/internal/core"
)

func main() {
	fs := flag.NewFlagSet("harness", flag.ExitOnError)
	configPath := fs.String("config", config.DefaultPath(), "path to harnessd.toml")
	showVersion := fs.Bool("version", false, "print version and exit")
	fs.Usage = usage
	_ = fs.Parse(os.Args[1:])

	if *showVersion {
		fmt.Printf("harness %s\n", buildinfo.Version)
		return
	}

	switch fs.Arg(0) {
	case "", "list":
		if err := list(*configPath); err != nil {
			fmt.Fprintf(os.Stderr, "harness: %v\n", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "harness: unknown command %q\n", fs.Arg(0))
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `harness — systemctl for your agents

usage:
  harness [list]     list configured harnesses (default)
  harness --version  print version

flags:
  --config PATH      path to harnessd.toml
`)
}

// list renders the configured harnesses with their lifecycle glyph. Until the
// daemon RPC lands (later story), every harness reads as stopped (○) — the
// config alone has no runtime state (ADR-0002).
func list(path string) error {
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "  NAME\tCMD\tBACKEND\tDESCRIPTION")
	// Until the daemon RPC lands (later story), every harness reads as stopped
	// (○) — the config alone carries no runtime state (ADR-0002).
	glyph := core.StateStopped.Glyph()
	for _, h := range cfg.OrderedHarnesses() {
		fmt.Fprintf(w, "%s %s\t%s\t%s\t%s\n",
			glyph, h.Name, h.Cmd, h.Backend, h.Description)
	}
	return w.Flush()
}
