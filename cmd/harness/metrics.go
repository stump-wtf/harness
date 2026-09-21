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
//
// These are functions, like daemonManagerOptions, so the tests drive what the
// daemon itself builds (#315).
//
// Governing: ADR-0020, SPEC-0013 REQ-1; design.md "Listener".
//
// @joestump-agent 09/21/2026 - Added for harness#356.

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
}

// startDaemonMetrics builds the collector over the daemon's Manager, observer
// and schedule, and binds l. It returns a daemonMetrics whose srv is nil when
// the listener is off or could not bind.
func startDaemonMetrics(mgr *supervisor.Manager, obs *observe.Observer, nextRun func(string) (time.Time, bool), l metrics.Listener) *daemonMetrics {
	if l.Addr == "" {
		log.Info("metrics listener off", "hint", "[server] metrics_listen = \"off\"")
		return &daemonMetrics{}
	}
	var src metrics.EventSource
	if obs != nil {
		src = obs
	}
	m := metrics.New(mgr, metrics.Options{Observer: src, NextRun: nextRun})
	m.Start()
	if l.TokenFileLoose {
		log.Warn("metrics token file is readable by group or others", "hint", "chmod 600 the metrics_token_file")
	}
	srv, err := metrics.Listen(l, m.Handler())
	if err != nil {
		log.Error("metrics listener disabled: bind failed", "addr", l.Addr, "err", err,
			"hint", "set [server] metrics_listen to a free address and restart the daemon")
		return &daemonMetrics{m: m}
	}
	log.Info("metrics listening", "addr", srv.Addr(), "path", "/metrics", "auth", l.Token != "")
	return &daemonMetrics{m: m, srv: srv}
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
