package main

// Daemon Metrics Wiring
//
// Builds the Prometheus surface (internal/metrics) over the daemon's own
// Manager, observer and scheduler, and binds its listener. Two decisions live
// here rather than in the package, because they are about the daemon's
// lifecycle:
//
//   - The listener configuration is resolved before any harness starts, and a
//     non-loopback bind without a token stops the daemon there (SPEC-0013
//     REQ-1). Refusing after Autostart would leave agents running under a
//     daemon that is about to exit.
//   - A bind failure is logged and the daemon carries on. The scraper sees the
//     target down, which it already alerts on; taking every harness down
//     because a port is taken would be the larger outage.
//   - The collector subscribes to the Manager's lifecycle bus before
//     Autostart, so the transitions boot causes — starting, running, an early
//     crash loop — are counted (SPEC-0013 REQ-2). The observer and the
//     scheduler do not exist yet at that point; they are attached, and the
//     listener bound, once they do.
//
// These are functions, like daemonManagerOptions, so the tests drive what the
// daemon itself builds (#315).
//
// Governing: ADR-0020, SPEC-0013 REQ-1, REQ-2; design.md "Listener".
//
// @joestump-agent 09/21/2026 - Added for harness#356.
//
// @joestump-agent 09/21/2026 - Review: split into beginDaemonMetrics (before
// Autostart) and serve (after the observer and scheduler exist), so boot
// transitions are no longer missed.

import (
	"context"
	"time"

	"charm.land/log/v2"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/metrics"
	"github.com/stump-wtf/harness/internal/observe"
	"github.com/stump-wtf/harness/internal/supervisor"
)

// daemonMetricsListener resolves [server] metrics_listen/metrics_token_file.
// An error means the daemon must not start.
func daemonMetricsListener(sc core.ServerConfig) (metrics.Listener, error) {
	return metrics.ResolveListener(sc.MetricsListen, sc.MetricsTokenFile)
}

// daemonMetrics is the running metrics surface; nil fields mean off.
type daemonMetrics struct {
	m   *metrics.Metrics
	srv *metrics.Server
	l   metrics.Listener
}

// beginDaemonMetrics builds the collector over the daemon's Manager and
// subscribes it to the lifecycle bus. runDaemon calls it before Autostart. It
// returns a daemonMetrics with a nil m when the listener is off.
func beginDaemonMetrics(mgr *supervisor.Manager, l metrics.Listener) *daemonMetrics {
	if l.Addr == "" {
		log.Info("metrics listener off", "hint", "[server] metrics_listen = \"off\"")
		return &daemonMetrics{}
	}
	// Run counts come from the run ledger's feed, never the lifecycle bus
	// (SPEC-0022 REQ-11).
	m := metrics.New(mgr, metrics.Options{Runs: mgr.Ledger()})
	m.Start()
	return &daemonMetrics{m: m, l: l}
}

// serve attaches the observer and the schedule reader and binds the
// listener. srv stays nil when the listener is off or could not bind.
func (d *daemonMetrics) serve(obs *observe.Observer, nextRun func(string) (time.Time, bool)) {
	if d == nil || d.m == nil {
		return
	}
	var src metrics.EventSource
	if obs != nil {
		src = obs
	}
	d.m.Attach(src, nextRun)
	l := d.l
	if l.TokenFileLoose {
		log.Warn("metrics token file is readable by group or others", "hint", "chmod 600 the metrics_token_file")
	}
	srv, err := metrics.Listen(l, d.m.Handler())
	if err != nil {
		log.Error("metrics listener disabled: bind failed", "addr", l.Addr, "err", err,
			"hint", "set [server] metrics_listen to a free address and restart the daemon")
		return
	}
	log.Info("metrics listening", "addr", srv.Addr(), "path", "/metrics", "auth", l.Token != "")
	d.srv = srv
}

// Stop shuts the listener down and unsubscribes the collector.
func (d *daemonMetrics) Stop() {
	if d == nil {
		return
	}
	if d.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = d.srv.Shutdown(ctx)
		cancel()
	}
	if d.m != nil {
		d.m.Close()
	}
}
