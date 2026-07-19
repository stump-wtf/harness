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
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/attach"
	"gitea.stump.rocks/stump.wtf/harness/internal/buildinfo"
	"gitea.stump.rocks/stump.wtf/harness/internal/config"
	"gitea.stump.rocks/stump.wtf/harness/internal/core"
	"gitea.stump.rocks/stump.wtf/harness/internal/daemon"
	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
	"gitea.stump.rocks/stump.wtf/harness/internal/remote"
	"gitea.stump.rocks/stump.wtf/harness/internal/supervisor"
)

func main() {
	fs := flag.NewFlagSet("harnessd", flag.ExitOnError)
	configPath := fs.String("config", config.DefaultPath(), "path to harnessd.toml")
	socketPath := fs.String("socket", protocol.DefaultSocketPath(), "control/data plane socket path")
	ringLines := fs.Int("scrollback", attach.DefaultRingLines, "per-harness scrollback ring depth (lines)")
	sshEnable := fs.Bool("ssh", false, "enable the remote Wish SSH server (ADR-0004; overrides [server] enabled)")
	sshListen := fs.String("ssh-listen", "", "SSH bind address host:port (overrides [server] listen)")
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

	// Optional remote access (ADR-0004/0008): the Wish SSH server hosts the same
	// TUI in-process as a local client of the socket above. Off unless enabled
	// via [server] or the -ssh flag. Secrets never touch this path — only public
	// keys and the persisted host key (ADR-0008).
	remoteSrv := startRemote(cfg.Server, *sshEnable, *sshListen, srv.SocketPath(), *configPath)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	fmt.Fprintln(os.Stderr, "harnessd: shutting down")
	if remoteSrv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = remoteSrv.Shutdown(ctx)
		cancel()
	}
	srv.Close()
	mgr.Close()
}

// startRemote brings up the optional Wish SSH server when it is enabled by
// config ([server] enabled = true) or the -ssh flag. The flag forces it on and
// -ssh-listen overrides the bind address. It returns nil (and logs) rather than
// aborting the daemon when remote setup fails: the local socket is the critical
// path; remote is a bonus (ADR-0004). Governing: SPEC-0002, ADR-0004, ADR-0008.
func startRemote(sc core.ServerConfig, forceOn bool, listenOverride, socket, configPath string) *remote.Server {
	if !sc.Enabled && !forceOn {
		return nil
	}
	listen := sc.Listen
	if listenOverride != "" {
		listen = listenOverride
	}
	rs, err := remote.New(remote.Options{
		Listen:      listen,
		Socket:      socket,
		ConfigPath:  configPath,
		Version:     buildinfo.Version,
		HostKeyPath: sc.HostKeyPath,
		Keys:        sc.AuthorizedKeys,
		KeysFile:    sc.AuthorizedKeysFile,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "harnessd: remote SSH disabled: %v\n", err)
		return nil
	}
	fmt.Printf("harnessd: remote SSH server listening on %s\n", rs.Addr())
	go func() {
		if err := rs.Serve(); err != nil {
			fmt.Fprintf(os.Stderr, "harnessd: remote SSH server: %v\n", err)
		}
	}()
	return rs
}
