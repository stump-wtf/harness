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

// daemonManagerOptions is the ManagerOptions the daemon actually runs with.
//
// It is a function rather than a literal at the call site so a test can assert
// on the policy THE DAEMON builds, and that distinction is the whole of #315.
// This construction supplied no Policy at all, so every production supervisor
// ran with the zero value — and Policy.normalize fills in every field except
// MaxRestarts, which the supervisor reads as "never give up". A harness that
// failed on every run retried forever instead of parking in `failed`. Every
// test that exercised give-up handed NewManager its own Policy, so nothing
// pointed at this one.
//
// Governing: SPEC-0003 REQ "Backoff Give-Up"; ADR-0005 (capped-exponential
// backoff); issue #315.
func daemonManagerOptions(reg *attach.Registry) supervisor.ManagerOptions {
	return supervisor.ManagerOptions{
		// Restart, backoff, give-up and stop-grace tunables. Without it the
		// give-up branch is unreachable, which is the protection harness.toml's
		// own comments promise: a harness once restarted 6,212 times and
		// exhausted a model provider's weekly quota.
		Policy:      supervisor.DefaultPolicy(),
		ExtraOutFor: reg.WriterFor,
		// Deregistered project harnesses release their Mux so removed projects
		// never leak emulators/scrollback (SPEC-0004 REQ "Tear Down").
		DropExtraOut: reg.Remove,
		// A harness (re)started while a client is attached is spawned into a PTY
		// the size of that client's viewport, not 80×24 (ADR-0003).
		SizeFor: reg.SizeFor,
	}
}

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
	mgr := supervisor.NewManager(cfg, daemonManagerOptions(reg))
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
	// Governing: ADR-0013; SPEC-0008 REQ "Firing And Overlap", REQ "Overlap
	// Policy", REQ "Missed Window Handling", REQ "Run History"; issues #66,
	// #117, #119.
	// Every firing — on time or catch-up — becomes a run request. The
	// harness's supervisor decides it on its actor loop: from idle it starts a
	// recorded run; with a run in flight it applies on_overlap (skip, queue, or
	// replace), and every one of those decisions leaves a run record. Deciding
	// on the loop is what keeps the check and the start from being split by
	// another start, which a snapshot-then-start guard here could not.
	sched := scheduler.New(scheduler.Options{
		Start: func(f scheduler.Firing) {
			name := f.Name
			trigger := supervisor.TriggerSchedule
			if f.Trigger == scheduler.TriggerCatchUp {
				trigger = supervisor.TriggerCatchUp
				log.Warn("catching up missed schedule: running once for windows that elapsed while the daemon was not evaluating",
					"harness", name, "window", f.Window.Format(time.RFC3339), "late", f.Late.Round(time.Second), "missed", f.Missed)
			} else {
				log.Info("schedule fired", "harness", name)
			}
			req := supervisor.RunRequest{Trigger: trigger, Window: f.Window, Windows: f.Missed}
			if _, ok := mgr.StartRun(name, req); !ok {
				log.Warn("schedule fired for unknown harness", "harness", name)
			}
		},
		// Marks ride in state.json (ADR-0007), so a daemon started after a
		// window it was down for knows it missed one, and a crash right after a
		// firing does not fire the same window again on restart.
		Store: scheduleStore{mgr},
		// A missed window becomes a `missed` run record, and stays loud in the
		// daemon log too.
		Recorder: runHistoryRecorder{mgr},
		// Every move of a next window reaches clients as job_schedule_changed
		// (SPEC-0008 REQ "Lifecycle Events"; #120).
		NextChanged: mgr.PublishScheduleChanged,
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

	// Issue #347: watch running crush harnesses for sessions wedged on
	// context-limit errors — the failure that reports healthy while the
	// harness answers nothing — and rotate the session (stop, archive the
	// store, start) when one stalls.
	sessionGuard := supervisor.NewSessionGuard(mgr, 0, 0)
	mgr.SetSessionGuard(sessionGuard)
	sessionGuard.Start()
	log.Info("session guard active", "interval", supervisor.DefaultSessionGuardInterval, "lookback", supervisor.DefaultSessionGuardLookback)

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
	sessionGuard.Close()
	mgr.SetSessionGuard(nil)
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

// scheduleStore adapts the Manager's state.json schedule marks to the
// scheduler's Store seam. The two mark types match field for field, so these
// conversions stop compiling the moment either one drifts.
type scheduleStore struct{ mgr *supervisor.Manager }

func (s scheduleStore) LoadMark(name string) (scheduler.Mark, bool) {
	m, ok := s.mgr.LoadScheduleMark(name)
	return scheduler.Mark(m), ok
}

func (s scheduleStore) UpdateMarks(put map[string]scheduler.Mark, forget []string) error {
	marks := make(map[string]supervisor.ScheduleMark, len(put))
	for name, m := range put {
		marks[name] = supervisor.ScheduleMark(m)
	}
	return s.mgr.UpdateScheduleMarks(marks, forget)
}

// runHistoryRecorder fills the scheduler's missed-window seam (#117) with a
// durable `missed` run record (#119). It keeps the warn-level log line as well:
// run history is not on the protocol yet, and a miss should be loud where
// operators already look.
type runHistoryRecorder struct{ mgr *supervisor.Manager }

func (r runHistoryRecorder) RecordMissed(m scheduler.MissedWindow) {
	scheduler.LogRecorder{}.RecordMissed(m)
	if err := r.mgr.RecordMissed(m.Name, m.First, m.Last, m.Count, m.DetectedAt); err != nil {
		log.Error("could not record missed schedule windows in run history", "harness", m.Name, "err", err)
	}
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
