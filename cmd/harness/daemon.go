package main

// Governing: ADR-0002 (one long-lived daemon owns all harness state; clients
// are thin) and ADR-0005 (init supervises only the daemon; the daemon
// supervises harnesses). SPEC-0002 (it serves the framed control+attach
// protocol over the local Unix socket). This wires the pieces together in the
// mandated order — NewManager → Restore → Autostart → serve the socket —
// exposing Start/Stop/Restart/Snapshots + Events over the control plane and one
// x/vt emulator + scrollback ring per harness (fed via the Manager ExtraOut
// hook) over the attach data plane.
//
// The daemon is exposed as `harness daemon` (see main.go dispatch). ADR-0005
// specifies the systemd ExecStart as `harness daemon`; the historical
// standalone `harnessd` binary is retired in favour of the single-binary form.

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/charmbracelet/log"

	"gitea.stump.rocks/stump.wtf/harness/internal/attach"
	"gitea.stump.rocks/stump.wtf/harness/internal/buildinfo"
	"gitea.stump.rocks/stump.wtf/harness/internal/cliui"
	"gitea.stump.rocks/stump.wtf/harness/internal/config"
	"gitea.stump.rocks/stump.wtf/harness/internal/core"
	"gitea.stump.rocks/stump.wtf/harness/internal/daemon"
	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
	"gitea.stump.rocks/stump.wtf/harness/internal/remote"
	"gitea.stump.rocks/stump.wtf/harness/internal/supervisor"
)

// runDaemon is the entry point for `harness daemon`. It owns its own flag set
// (the daemon's flags don't overlap with the client verbs') and parses args
// after the `daemon` subcommand token.
//
// Governing: ADR-0001 (the daemon uses charmbracelet/log for structured,
// colorized output); ADR-0005 (the daemon is normally supervised by init;
// `--detach` is a dev convenience that forks into the background and redirects
// stdio to a logfile).
func runDaemon(args []string) {
	fs := flag.NewFlagSet("harness daemon", flag.ExitOnError)
	configPath := fs.String("config", config.DefaultPath(), "path to harness.toml")
	socketPath := fs.String("socket", protocol.DefaultSocketPath(), "control/data plane socket path")
	ringLines := fs.Int("scrollback", attach.DefaultRingLines, "per-harness scrollback ring depth (lines)")
	sshEnable := fs.Bool("ssh", false, "enable the remote Wish SSH server (ADR-0004; overrides [server] enabled)")
	sshListen := fs.String("ssh-listen", "", "SSH bind address host:port (overrides [server] listen)")
	logLevel := fs.String("log-level", "info", "log level: debug, info, warn, error")
	logFile := fs.String("log-file", "", "append logs to this file instead of stderr")
	detach := fs.Bool("detach", false, "fork into the background; redirect stdio to --log-file (dev convenience; prefer systemd in production)")
	showVersion := fs.Bool("version", false, "print version and exit")
	_ = fs.Parse(args)

	if *showVersion {
		fmt.Printf("harness daemon %s\n", buildinfo.Version)
		return
	}

	// --detach: fork into the background. We re-exec ourselves without
	// --detach, with stdio redirected to the logfile, and the parent exits
	// once the child signals readiness (binds the socket). This is a dev
	// convenience; in production ADR-0005 wants systemd (or launchd) owning
	// the daemon.
	if *detach {
		if err := detachDaemon(args); err != nil {
			os.Exit(cliui.Fatal(err))
		}
		return
	}

	// Configure the daemon-wide logger (ADR-0001: charmbracelet/log). It
	// writes to stderr by default (so systemd captures it) or to --log-file;
	// level is configurable. The TTY-aware styling mirrors cliui's contract —
	// structured logs read like the rest of the product.
	configureDaemonLogger(*logLevel, *logFile)

	cfg, err := config.Load(*configPath)
	if err != nil {
		os.Exit(cliui.Fatal(err))
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
		log.Warn("restore state failed (continuing with config defaults)", "err", err)
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
		log.Error("listen failed", "socket", *socketPath, "err", err)
		signalDetached('e') // tell the waiting parent we failed
		mgr.Close()
		os.Exit(1)
	}

	log.Info("serving",
		"socket", srv.SocketPath(),
		"config", *configPath,
		"harnesses", len(cfg.Harnesses),
		"version", buildinfo.Version,
	)
	signalDetached('o') // tell the waiting parent we're up

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

	log.Info("shutting down")
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
		log.Warn("remote SSH disabled", "err", err)
		return nil
	}
	log.Info("remote SSH server listening", "addr", rs.Addr())
	go func() {
		if err := rs.Serve(); err != nil {
			log.Error("remote SSH server", "err", err)
		}
	}()
	return rs
}

// configureDaemonLogger sets up the package-level charmbracelet/log default
// logger per ADR-0001. level may be debug/info/warn/error; logFile may be ""
// (stderr) or a path to append to. Time format is ISO 8601 (sortable, matches
// the log-file-per-harness format in the supervisor).
func configureDaemonLogger(level, logFile string) {
	var w io.Writer = os.Stderr
	if logFile != "" {
		f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			// Fall back to stderr — we can't do much else this early.
			log.Warn("could not open log file, falling back to stderr", "path", logFile, "err", err)
		} else {
			w = f
		}
	}
	log.SetOutput(w)
	log.SetTimeFormat("2006-01-02T15:04:05.000Z07:00")
	log.SetReportTimestamp(true)
	switch level {
	case "debug":
		log.SetLevel(log.DebugLevel)
	case "info":
		log.SetLevel(log.InfoLevel)
	case "warn":
		log.SetLevel(log.WarnLevel)
	case "error":
		log.SetLevel(log.ErrorLevel)
	default:
		log.SetLevel(log.InfoLevel)
	}
}
