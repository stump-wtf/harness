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
	"errors"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"charm.land/log/v2"

	"github.com/stump-wtf/harness/internal/attach"
	"github.com/stump-wtf/harness/internal/buildinfo"
	"github.com/stump-wtf/harness/internal/cliui"
	"github.com/stump-wtf/harness/internal/config"
	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/daemon"
	"github.com/stump-wtf/harness/internal/observe"
	"github.com/stump-wtf/harness/internal/remote"
	"github.com/stump-wtf/harness/internal/scheduler"
	"github.com/stump-wtf/harness/internal/supervisor"
	"github.com/stump-wtf/harness/internal/telemetry"
	"github.com/stump-wtf/harness/internal/trigger/source"
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

// daemonObserverOptions is the agent event observer configuration the daemon
// runs with: production defaults throughout. It is a function, like
// daemonManagerOptions, so the wiring test drives the observer the daemon
// builds and shrinks only its poll interval.
//
// Governing: issue #390.
func daemonObserverOptions() observe.Options {
	return observe.Options{}
}

// startDaemonObserver builds the agent event observer over the daemon's own
// Manager and starts it. The daemon stops it on shutdown, before the Manager
// closes.
func startDaemonObserver(mgr *supervisor.Manager, opts observe.Options) *observe.Observer {
	obs := observe.New(mgr, opts)
	obs.Start()
	return obs
}

// startDaemonLoopGuard subscribes the runaway tool-loop guard to the daemon's
// observer, killing a run through the daemon's own Manager when one of its
// sessions repeats one identical tool call past the threshold. It is a
// function, like startDaemonObserver, so the wiring test drives the guard the
// daemon builds.
//
// Governing: stumpcloud/stumpcloud#469.
func startDaemonLoopGuard(obs *observe.Observer, mgr *supervisor.Manager) *observe.LoopGuard {
	g := observe.StartLoopGuard(obs, mgr, 0, nil)
	log.Info("runaway loop guard active", "threshold", observe.DefaultLoopThreshold)
	return g
}

// resolveDaemonTelemetry resolves the [telemetry] table against the daemon's
// environment and [telemetry] env_file, logging warnings and notes. It returns
// nil when no destination is configured: the daemon then builds no pipeline,
// takes no observer subscription, opens no file and makes no connection. An
// error — an enabled OTLP signal with no endpoint, a missing env_file — must
// refuse the start, so the daemon calls this before Autostart.
//
// Governing: ADR-0022; SPEC-0015 REQ-1, REQ-3, REQ-14.
func resolveDaemonTelemetry(tc core.TelemetryConfig, env telemetry.Env) (*telemetry.Resolved, error) {
	if note := telemetry.IgnoredEnvNote(tc, env.Getenv); note != "" {
		log.Info(note)
	}
	res, err := telemetry.Resolve(tc, env)
	if err != nil || res == nil {
		return nil, err
	}
	for _, w := range res.Warnings {
		log.Warn("telemetry: " + w)
	}
	for _, n := range res.Notes {
		log.Info("telemetry: " + n)
	}
	return res, nil
}

// startDaemonTelemetry starts the export pipeline over the daemon's observer
// and Manager, logging one line per signal with what was resolved and where
// each setting came from — never a header value. nil when nothing is enabled.
// It is a function, like daemonManagerOptions, so the wiring test drives the
// pipeline the daemon builds.
//
// Governing: ADR-0022; SPEC-0015 REQ-9, REQ-14.
func startDaemonTelemetry(res *telemetry.Resolved, obs telemetry.Subscriber, src telemetry.Source, opts telemetry.Options) *telemetry.Pipeline {
	if !res.Enabled() {
		return nil
	}
	for _, sig := range []*telemetry.Signal{res.Logs, res.Traces} {
		if sig != nil {
			log.Info("telemetry export active", sig.LogKeyvals()...)
		}
	}
	if path := res.EventsFile(); path != "" {
		log.Info("telemetry export active", "signal", telemetry.SignalEventsFile, "path", path, "path_source", telemetry.SourceFile)
	}
	return telemetry.New(res, obs, src, opts)
}

// shutdownDaemonTelemetry starts the pipeline's shutdown flush in the
// background, bounded by [telemetry] shutdown_timeout, and returns a channel
// closed when it is done — at once when there is no pipeline. runDaemon waits
// on it only after the Manager has closed, so the flush never delays the
// harnesses' own stop, and Pipeline.Shutdown keeps the wait itself within
// the budget even when an events-file write hangs (SPEC-0015 REQ-11). A
// function, like startDaemonTelemetry, so a test drives the daemon's own
// timeout wiring.
//
// Governing: ADR-0022; SPEC-0015 REQ-11.
func shutdownDaemonTelemetry(p *telemetry.Pipeline, res *telemetry.Resolved) <-chan struct{} {
	done := make(chan struct{})
	if p == nil {
		close(done)
		return done
	}
	go func() {
		defer close(done)
		ctx, cancel := context.WithTimeout(context.Background(), res.Config.ShutdownTimeout)
		defer cancel()
		p.Shutdown(ctx)
	}()
	return done
}

// warnTelemetryReload logs, once per reload, that a changed [telemetry] table
// waits for a restart: the exporters hold queues, connections and an open
// file (SPEC-0015 REQ-2). Per-harness export_telemetry needs no warning; the
// gate reads the current definition for every item.
func warnTelemetryReload(running, reloaded core.TelemetryConfig) bool {
	if running == reloaded {
		return false
	}
	log.Warn("[telemetry] changed in harness.toml; the change takes effect after a daemon restart",
		"hint", "restart the daemon (per-harness export_telemetry changes apply on reload)")
	return true
}

// telemetryReloadWarning is the reload reaction runDaemon hands
// startDaemonScheduler: after each reload it compares the reloaded
// [telemetry] table with running, the table the pipeline was built from, and
// warns when they differ (SPEC-0015 REQ-2). It rides the scheduler's hook
// because the Manager holds exactly one; a function so a test drives the
// same pairing the daemon registers.
func telemetryReloadWarning(mgr *supervisor.Manager, running core.TelemetryConfig) func() {
	return func() { warnTelemetryReload(running, mgr.Config().Telemetry) }
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

	// Refuse a live socket BEFORE anything with side effects runs. Listen
	// probes again below, but by then Restore and Autostart have started this
	// daemon's copies of the live daemon's harnesses, and the mgr.Close on
	// the refusal path flushes its state.json over the live one's (#578).
	if err := daemon.CheckSocketFree(o.socketPath); err != nil {
		logListenFailure(o.socketPath, err)
		signalDetached('e') // tell the waiting parent we failed
		os.Exit(1)
	}

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

	// Telemetry export (ADR-0022): resolved before any harness starts, so a
	// signal that is consented to but cannot be delivered refuses the start.
	telemetryRes, err := resolveDaemonTelemetry(cfg.Telemetry, telemetry.ProcessEnv(buildinfo.Version))
	if err != nil {
		os.Exit(cliui.Fatal(err))
	}

	// SPEC-0013 REQ-1: a metrics listener off loopback without a bearer
	// token is refused here, before any harness is started.
	metricsListener, err := daemonMetricsListener(cfg.Server)
	if err != nil {
		log.Error("refusing to start", "err", err)
		signalDetached('e')
		os.Exit(1)
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
	// Issue #356: the metrics collector subscribes to lifecycle events before
	// Autostart, so the transitions boot causes are counted (SPEC-0013 REQ-2).
	// Its observer, schedule and listener arrive below.
	daemonMet := beginDaemonMetrics(mgr, metricsListener)
	mgr.Autostart()

	// Scheduled harnesses and the operating-hours gate share one wall-clock
	// tick (ADR-0013, ADR-0019). nil is the real clock. A changed [telemetry]
	// table waits for a restart, so every reload that changes it says so
	// (SPEC-0015 REQ-2); it rides the scheduler's reload hook because the
	// Manager holds exactly one.
	sched := startDaemonScheduler(mgr, cfg, nil, telemetryReloadWarning(mgr, cfg.Telemetry))

	// The trigger source manager (ADR-0021 / SPEC-0014). It is built here,
	// on the daemon's own path, even though nothing produces events yet: the
	// listener and the channel session are later stories, and a manager that
	// only ever appeared in their wiring would mean the fan-out was never
	// exercised against the Manager the daemon actually runs. It reads the
	// live config per firing, so a reload's change to a harness's `triggers`
	// applies from the next event (REQ "Source Reconciliation On Reload").
	sources := startDaemonSources(mgr)
	// After startDaemonScheduler, which registered its own hook: this
	// composes onto it rather than replacing it (see wireSourceReload).
	wireSourceReload(mgr, sources)
	// The webhook listener's reload rides the same hook, composed after
	// source reconciliation, and has to be registered here — before the
	// config watcher or SIGHUP can reload — even though the listener binds
	// beside startRemote below.
	webhooks := beginDaemonWebhooks(mgr, sources, o.webhookListen)
	wireWebhookReload(mgr, webhooks)

	srv := daemon.NewServer(daemon.Options{
		Manager:    mgr,
		Registry:   reg,
		Scheduler:  sched,
		SocketPath: o.socketPath,
		ConfigPath: o.configPath,
		Version:    buildinfo.Version,
	})
	if err := srv.Listen(); err != nil {
		logListenFailure(o.socketPath, err)
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

	// Issue #390: read what the supervised agents write — tool calls, and the
	// provider errors a running process never surfaces — for the metrics and
	// telemetry consumers that subscribe to it. Built after Autostart, so its
	// history floor is this daemon's start and nothing from before it is
	// reported as live.
	observer := startDaemonObserver(mgr, daemonObserverOptions())
	log.Info("agent event observer active", "interval", observe.DefaultPollInterval)

	// stumpcloud/stumpcloud#469: kill a run whose session repeats one
	// identical tool call past the threshold — the shape behind the 608
	// "." comments on harness#383/#384. The daemon keeps supervising; only
	// the runaway run dies.
	loopGuard := startDaemonLoopGuard(observer, mgr)

	// Issue #391: export that stream, only when [telemetry] names a
	// destination and only for opted-in harnesses (SPEC-0015 REQ-1).
	telemetryPipeline := startDaemonTelemetry(telemetryRes, observer, mgr, telemetry.Options{})
	// Issue #356: GET /metrics (SPEC-0013), fed by the observer and the
	// Manager, on its own listener.
	daemonMet.serve(observer, sched.NextFire)

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

	// The webhook listener (ADR-0021 / SPEC-0014): the second network front
	// door, for machines. Off unless [server] webhook_listen,
	// --webhook-listen or HARNESS_WEBHOOK_LISTEN names an address, and every
	// route on it is authenticated.
	webhooks.serve()

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
	// Telemetry first: it stops taking items, then flushes within
	// shutdown_timeout (SPEC-0015 REQ-11). The flush runs alongside the rest
	// of shutdown rather than ahead of it, so it never delays the harnesses'
	// own stop; the daemon waits for it only at the very end.
	telemetryDone := shutdownDaemonTelemetry(telemetryPipeline, telemetryRes)
	// Before the observer and the Manager: metrics reads both.
	daemonMet.Stop()
	// Before the observer stops: the guard consumes from it, and its Stop
	// unregisters the subscription so observer.Stop never closes a channel
	// the guard is still reading.
	loopGuard.Stop()
	// Before the Manager closes: the observer reads its snapshots.
	observer.Stop()
	sessionGuard.Close()
	mgr.SetSessionGuard(nil)
	if cfgWatcher != nil {
		cfgWatcher.Close()
	}
	sched.Close()
	// Before the source manager closes: stop taking deliveries, and give the
	// ones in flight webhook.ShutdownGrace to reach it and answer, rather
	// than have them fire into a closed manager.
	webhooks.shutdown()
	// Before the Manager closes: a firing in progress is still holding a
	// reference to it, and Close waits for those to reach StartRun.
	sources.Close()
	if remoteSrv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = remoteSrv.Shutdown(ctx)
		cancel()
	}
	srv.Close()
	mgr.Close()
	<-telemetryDone
}

// startDaemonSources builds and starts the trigger source manager the daemon
// runs. It is a function rather than inline in runDaemon for the reason
// startDaemonScheduler is: a wiring test must be able to drive THE DAEMON'S
// manager, not one it assembled itself (#315).
//
// Governing: ADR-0021; SPEC-0014 REQ "Firing", REQ "Concurrency Safety".
func startDaemonSources(mgr *supervisor.Manager) *source.Manager {
	sm := source.New(source.Options{
		Runner: mgr,
		// A function, not a snapshot: REQ "Source Reconciliation On Reload"
		// says a change to the set of harnesses bound to a source applies
		// from the next event, which is only true if each firing re-reads it.
		Config: mgr.Config,
		Log:    log.Default(),
	})
	sm.Start(context.Background())
	return sm
}

// wireSourceReload composes source reconciliation onto the Manager's reload
// hook, preserving whatever was already registered.
//
// SetReloadHook takes one callback and is documented as set-once during boot,
// so composing here is how two consumers share it without either having to
// know about the other. Getting this wrong is silent in the worst way: a
// replacement would stop the scheduler being re-applied on reload, and
// schedules would simply stop tracking the config with nothing in the log to
// say so.
//
// Reconciliation runs AFTER the scheduler's re-apply, matching the order they
// appear in runDaemon. Nothing depends on the order; a stable one is just one
// less thing to wonder about when reading a log.
//
// Governing: ADR-0021; SPEC-0014 REQ "Source Reconciliation On Reload".
func wireSourceReload(mgr *supervisor.Manager, sources *source.Manager) {
	prev := mgr.ReloadHook()
	mgr.SetReloadHook(func() {
		if prev != nil {
			prev()
		}
		sources.Reconcile(mgr.Config())
	})
}

// startDaemonScheduler builds, applies and starts the scheduler the daemon
// runs, and registers it to re-apply after every config reload. It is a
// function rather than inline in runDaemon so a test can drive THE DAEMON'S
// wiring with a fake clock — the #315 lesson: a test that builds its own
// scheduler would pass even if the daemon stopped passing the gate. A nil
// clock is the real wall clock.
//
// Governing: ADR-0013; SPEC-0008; ADR-0019, SPEC-0012 REQ "Gate Evaluation",
// REQ "Operating Hours Reload".
func startDaemonScheduler(mgr *supervisor.Manager, cfg *core.Config, clock scheduler.Clock, onReload ...func()) *scheduler.Scheduler {
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
		Clock: clock,
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
		// The operating-hours gate pass rides the same tick: it holds a gated
		// harness out of hours and releases it when they open, both on the
		// harness's actor loop (SPEC-0012 REQ "Gate Enforcement").
		Gate: hoursGate{mgr},
		// Every in_hours flip reaches clients as harness_hours_changed
		// (SPEC-0012 REQ "Operating Hours Visibility").
		HoursChanged: mgr.PublishHoursChanged,
		// A lease ending gets its own durable-log line, alongside close
		// start/hold/open (SPEC-0012 REQ "Operating Hours Visibility").
		LeaseEnded: func(name string) {
			mgr.LogLifecycle(name, "lease end", "reason", "operating_hours")
		},
	})
	sched.Apply(cfg)
	sched.Start()
	// Re-apply schedules after every successful config reload, whichever
	// path triggered it: SIGHUP, the config watcher, or the daemon's reload
	// control op all funnel through Manager.Reload. Registered before any of
	// those sources is live.
	// onReload carries the daemon's other reload reactions: the Manager
	// holds one hook, so a second SetReloadHook would silently replace this
	// one and stop schedules re-applying.
	//
	// runDaemon then composes trigger-source reconciliation onto this same
	// hook (wireSourceReload), so there is one definition of "the config
	// changed" rather than two that can disagree about which reload counted.
	mgr.SetReloadHook(func() {
		sched.Apply(mgr.Config())
		for _, fn := range onReload {
			fn()
		}
	})

	return sched
}

// hoursGate adapts the Manager to the scheduler's operating-hours Gate seam.
// A graceful close is marked by Hold, stepped by CloseStep from the Manager's
// turn-state watch, and bounded by a deadline anchored to CloseAt — the
// instant the harness went out of hours (SPEC-0012 REQ "Graceful Shutdown").
// Governing: ADR-0019, SPEC-0012 REQ "Gate Enforcement", REQ "Graceful
// Shutdown", REQ "Turn State Signal".
type hoursGate struct{ mgr *supervisor.Manager }

func (g hoursGate) Status(name string) (up, held, closing, ok bool) { return g.mgr.GateStatus(name) }

func (g hoursGate) Lease(name string, now time.Time) (time.Time, bool) {
	return g.mgr.LeaseAt(name, now)
}

func (g hoursGate) CloseAt(name string, now time.Time) (time.Time, bool) {
	return g.mgr.CloseAt(name, now)
}

func (g hoursGate) Hold(name string, mode core.HoursShutdownMode, closeAt time.Time) {
	g.mgr.Hold(name, mode, closeAt)
}

func (g hoursGate) CloseStep(name string, now time.Time) { g.mgr.CloseStep(name, now) }

func (g hoursGate) Arm(name string, closeAt time.Time) { g.mgr.Arm(name, closeAt) }

func (g hoursGate) Release(name string) { g.mgr.Release(name) }

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

// logListenFailure reports why the daemon could not take its socket.
//
// A socket another daemon answers on is a refusal, not a bind failure, and it
// needs its own line: the old behaviour removed that socket and bound over it,
// which is how tars ended up with a live daemon nobody could reach (#578).
func logListenFailure(socketPath string, err error) {
	if errors.Is(err, daemon.ErrSocketInUse) {
		log.Error("refusing to start: another harness daemon is already listening",
			"socket", socketPath,
			"err", err,
			"hint", "talk to it (`harness daemon status`), or stop it first (`harness daemon stop`)",
		)
		return
	}
	log.Error("listen failed", "socket", socketPath, "err", err)
}
