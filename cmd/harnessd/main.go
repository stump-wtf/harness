// Command harnessd is the long-lived supervision daemon.
//
// Governing: ADR-0002 (one long-lived daemon owns all harness state; clients
// are thin) and ADR-0005 (init supervises only the daemon; the daemon
// supervises harnesses). This is the entry point systemd/launchd start
// (`ExecStart=harnessd`). The supervisor, PTY layer, and control-plane socket
// land in later stories; this foundation binary establishes the entry point
// and loads + validates the config the daemon will serve.
package main

import (
	"flag"
	"fmt"
	"os"

	"gitea.stump.rocks/stump.wtf/harness/internal/buildinfo"
	"gitea.stump.rocks/stump.wtf/harness/internal/config"
)

func main() {
	fs := flag.NewFlagSet("harnessd", flag.ExitOnError)
	configPath := fs.String("config", config.DefaultPath(), "path to harnessd.toml")
	showVersion := fs.Bool("version", false, "print version and exit")
	_ = fs.Parse(os.Args[1:])

	if *showVersion {
		fmt.Printf("harnessd %s\n", buildinfo.Version)
		return
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "harnessd: %v\n", err)
		os.Exit(1)
	}

	// Placeholder for the supervision loop + control-plane socket (ADR-0002,
	// ADR-0005). For now, report what the daemon would supervise so the entry
	// point is exercisable end-to-end.
	fmt.Printf("harnessd %s — loaded %s\n", buildinfo.Version, *configPath)
	fmt.Printf("  harnesses: %d\n", len(cfg.Harnesses))
	fmt.Printf("  profiles:  %d\n", len(cfg.Profiles))
	if autostart := cfg.AutostartHarnesses(); len(autostart) > 0 {
		fmt.Printf("  autostart: %v\n", autostart)
	}
}
