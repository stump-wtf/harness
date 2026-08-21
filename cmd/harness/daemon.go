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
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"charm.land/log/v2"

	"gitea.stump.rocks/stump.wtf/harness/internal/attach"
	"gitea.stump.rocks/stump.wtf/harness/internal/buildinfo"
	"gitea.stump.rocks/stump.wtf/harness/internal/cliui"
	"gitea.stump.rocks/stump.wtf/harness/internal/config"
	"gitea.stump.rocks/stump.wtf/harness/internal/core"
	"gitea.stump.rocks/stump.wtf/harness/internal/daemon"
	"gitea.stump.rocks/stump.wtf/harness/internal/remote"
	"gitea.stump.rocks/stump.wtf/harness/internal/scheduler"
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
func runDaemon(o daemonOpts) {
	// Flag parsing, --version, and --detach are handled by the Cobra command
	// (daemon_cmd.go) and the settings ladder (settings_wire.go), so by the
	// time we get here every value is resolved and this function no longer
	// cares whether it came from a flag, HARNESS_*, the file, or a default.
	//
	// Governing: ADR-0016, SPEC-0010 REQ "Precedence Order".

	configureDaemonLogger(o.logLevel, o.logFile)

	// A missing config file is not an error: SPEC-0010 REQ "Fileless Operation"
	// requires a container configured entirely through HARNESS_* to come up and
	// serve, reporting zero harnesses. A file that EXISTS but does not parse
	// stays fatal — that is a broken deployment, not an absent one, and
	// silently starting empty would hide it.
	cfg, err := config.Load(o.configPath)
	if err != nil {
		if !cliui.IsMissingConfig(err) {
			os.Exit(cliui.Fatal(err))
		}
		log.Info("no config file; starting with no harnesses",
			"path", o.configPath,
			"hint", "define harnesses in a harness.toml, or point --config/HARNESS_CONFIG at one",
		)
		cfg = &core.Config{}
	}

	// The attach data plane: one Mux (x/vt emulator + scrollback ring) per
	// harness, lazily created. The Manager tees each harness's raw PTY output
	// into its Mux via the ExtraOut hook, alongside the durable log (ADR-0003/
	// ADR-0007). The Registry's controller (the Manager) applies the
	// smallest-attached-wins resize and delivers read-write keystrokes.
	reg := attach.NewRegistry(o.ringLines)
	mgr := supervisor.NewManager(cfg, supervisor.ManagerOptions{
		ExtraOutFor: reg.WriterFor,
		// Deregistered project harnesses release their Mux so removed projects do
		// projects never leak emulators/scrollback (SPEC-0004 REQ "Tear Down").
		DropExtraOut: reg.Remove,
		// A harness (re)started while a client is attached is spawned into a PTY
		// the size of that client's viewport, not 80×24 (ADR-0003).
		SizeFor: reg.SizeFor,
	})
	reg.SetController(mgr)

	// Mandated boot order (ADR-0005): restore intent from state.json, then
	// autostart the intended running set, then serve clients.
	if err := mgr.Restore(); err != nil {
		log.Warn("restore state failed (continuing with config defaults)", "err", err)
	}
	if !mgr.ProfileResolved() {
		log.Warn("active profile not found in config; fell back to autostart profile",
			"profile", mgr.ActiveProfile(),
			"hint", "run `harness use-profile <name>` to choose a valid profile",
		)
	}
	// Persisted intent beats autostart membership on purpose (an operator stop
	// must survive a restart), but without this the daemon comes up having
	// silently ignored `autostart = true` — config says one thing, `harness list`
	// shows another, and doctor calls the set healthy.
	if dormant := mgr.DormantAutostart(); len(dormant) > 0 {
		log.Warn("autostart profile members left down by persisted intent",
			"harnesses", strings.Join(dormant, ", "),
			"hint", "run `harness start <name>` to re-enable (it persists across restarts)",
		)
	}
	mgr.Autostart()

	// Scheduled harnesses: cron-fired one-shot agent runs owned by the daemon.
	// Governing: ADR-0013; SPEC-0008 REQ "Firing And Overlap"; issue #66.
	// At each firing the daemon starts the harness if
	// it is not already running (overlapping firings are skipped, not
	// stacked). "Running" here means any state with a live or in-transition
	// process: starting, running, degraded, and stopping all skip — a firing
	// must never spawn a second copy or flip enabled intent mid-stop.
	// Stopped, failed, and restarting fire (a fresh scheduled attempt clears
	// a failed latch via Start's normal path).
	sched := scheduler.New(func(name string) {
		if snap, ok := mgr.Snapshot(name); ok {
			switch snap.State {
			case core.StateStarting, core.StateRunning, core.StateDegraded, core.StateStopping:
				log.Debug("schedule fired but harness is active; skipping", "harness", name, "state", snap.State)
				return
			}
		}
		log.Info("schedule fired", "harness", name)
		if !mgr.StartTransient(name) {
			log.Warn("schedule fired for unknown harness", "harness", name)
		}
	})
	sched.Apply(cfg)
	sched.Start()
	// Re-apply schedules after every successful config reload, whichever
	// path triggered it: SIGHUP, the config watcher, or the daemon's reload
	// control op all funnel through Manager.Reload. Registered before any of
	// those sources is live.
	mgr.SetReloadHook(func() { sched.Apply(mgr.Config()) })

	srv := daemon.NewServer(daemon.Options{
		Manager:    mgr,
		Registry:   reg,
		Scheduler:  sched,
		SocketPath: o.socketPath,
		ConfigPath: o.configPath,
		Version:    buildinfo.Version,
	})
	if err := srv.Listen(); err != nil {
		log.Error("listen failed", "socket", o.socketPath, "err", err)
		signalDetached('e') // tell the waiting parent we failed
		mgr.Close()
		os.Exit(1)
	}

	log.Info("serving",
		"socket", srv.SocketPath(),
		"config", o.configPath,
		"harnesses", len(cfg.Harnesses),
		"version", buildinfo.Version,
	)
	signalDetached('o') // tell the waiting parent we're up

	// Issue #98: watch the config directory for changes and auto-reload.
	// Chezmoi (and czu on its timer) rewrite harness.toml via temp file +
	// rename, so the watcher monitors the directory, not the inode. The
	// opt-out is [daemon] watch_config = false.
	var cfgWatcher *supervisor.ConfigWatcher
	if cfg.Daemon.WatchConfigEnabled() {
		cw, err := supervisor.NewConfigWatcher(mgr, o.configPath)
		if err != nil {
			log.Warn("config watcher disabled (could not start)", "err", err)
		} else {
			cw.Start()
			cfgWatcher = cw
			log.Info("watching config for changes", "config", o.configPath)
		}
	}

	// Serve until a termination signal, then shut down cleanly: stop accepting,
	// tear down connections, stop harnesses, flush state. SIGHUP triggers a
	// graceful config reload (hot-reload harness.toml without stopping running
	// processes), matching systemd's ExecReload contract so `systemctl reload`
	// never kills the daemon or its children.
	go srv.Serve()

	// Optional remote access (ADR-0004/0008): the Wish SSH server hosts the same
	// TUI in-process as a local client of the socket above. Off unless enabled
	// via [server] or the -ssh flag. Secrets never touch this path — only public
	// keys and the persisted host key (ADR-0008).
	remoteSrv := startRemote(cfg.Server, o.sshEnable, o.sshListen, srv.SocketPath(), o.configPath)
	if remoteSrv != nil {
		srv.SetRemote(remoteSrv.Addr(), remoteSrv.Keys())
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	for {
		s := <-sig
		if s == syscall.SIGHUP {
			log.Info("received SIGHUP, reloading config")
			if err := mgr.ReloadFromFile(o.configPath); err != nil {
				log.Warn("config reload failed", "err", err)
			} else {
				log.Info("config reloaded")
			}
			continue
		}
		break
	}

	log.Info("shutting down")
	if cfgWatcher != nil {
		cfgWatcher.Close()
	}
	sched.Close()
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
	// Bind before claiming the server is up. Serve() binds inside the
	// goroutine, so `go rs.Serve()` cannot distinguish a live listener from
	// "address already in use" — and a non-nil return here is what tells
	// daemon_info (and therefore `harness doctor`) that SSH is listening.
	ln, err := rs.Listen()
	if err != nil {
		log.Error("remote SSH disabled: bind failed", "addr", rs.Addr(), "err", err)
		return nil
	}
	log.Info("remote SSH server listening", "addr", rs.Addr())
	go func() {
		if err := rs.ServeListener(ln); err != nil {
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
		f, err := openLogFile(logFile)
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
