// Command harnessd is the long-lived supervision daemon.
//
// Governing: ADR-0002 (one long-lived daemon owns all harness state; clients
// are thin) and ADR-0005 (init supervises only the daemon; the daemon
// supervises harnesses). SPEC-0002 (it serves the framed control+attach
// protocol over the local Unix socket). This wires the pieces together in the
// mandated order — NewManager → Restore → Autostart → serve the socket —
// exposing Start/Stop/Restart/Snapshots + Events over the control plane and one
// x/vt emulator + scrollback ring per harness (fed via the Manager ExtraOut
// hook) over the attach data plane.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"gitea.stump.rocks/stump.wtf/harness/internal/attach"
	"gitea.stump.rocks/stump.wtf/harness/internal/buildinfo"
	"gitea.stump.rocks/stump.wtf/harness/internal/config"
	"gitea.stump.rocks/stump.wtf/harness/internal/daemon"
	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
	"gitea.stump.rocks/stump.wtf/harness/internal/supervisor"
)

func main() {
	fs := flag.NewFlagSet("harnessd", flag.ExitOnError)
	configPath := fs.String("config", config.DefaultPath(), "path to harnessd.toml")
	socketPath := fs.String("socket", protocol.DefaultSocketPath(), "control/data plane socket path")
	ringLines := fs.Int("scrollback", attach.DefaultRingLines, "per-harness scrollback ring depth (lines)")
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

	// The attach data plane: one Mux (x/vt emulator + scrollback ring) per
	// harness, lazily created. The Manager tees each harness's raw PTY output
	// into its Mux via the ExtraOut hook, alongside the durable log (ADR-0003/
	// ADR-0007). The Registry's controller (the Manager) applies the
	// smallest-attached-wins resize and delivers read-write keystrokes.
	reg := attach.NewRegistry(*ringLines)
	mgr := supervisor.NewManager(cfg, supervisor.ManagerOptions{
		ExtraOutFor: reg.WriterFor,
	})
	reg.SetController(mgr)

	// Mandated boot order (ADR-0005): restore intent from state.json, then
	// autostart the intended running set, then serve clients.
	if err := mgr.Restore(); err != nil {
		fmt.Fprintf(os.Stderr, "harnessd: restore state: %v (continuing with config defaults)\n", err)
	}
	mgr.Autostart()

	srv := daemon.NewServer(daemon.Options{
		Manager:    mgr,
		Registry:   reg,
		SocketPath: *socketPath,
		ConfigPath: *configPath,
		Version:    buildinfo.Version,
	})
	if err := srv.Listen(); err != nil {
		fmt.Fprintf(os.Stderr, "harnessd: listen %s: %v\n", *socketPath, err)
		mgr.Close()
		os.Exit(1)
	}

	fmt.Printf("harnessd %s — serving %s (config %s, %d harnesses)\n",
		buildinfo.Version, srv.SocketPath(), *configPath, len(cfg.Harnesses))

	// Serve until a termination signal, then shut down cleanly: stop accepting,
	// tear down connections, stop harnesses, flush state.
	go srv.Serve()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	fmt.Fprintln(os.Stderr, "harnessd: shutting down")
	srv.Close()
	mgr.Close()
}
