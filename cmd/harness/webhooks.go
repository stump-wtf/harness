package main

// Daemon Webhook Listener Wiring
//
// Builds the SPEC-0014 webhook listener over the daemon's own source manager,
// binds it, reconciles it on reload, and shuts it down. Decisions that are
// about the daemon's lifecycle rather than the listener live here:
//
//   - Off unless an address names it: `[server] webhook_listen`,
//     `--webhook-listen` or HARNESS_WEBHOOK_LISTEN, a flag or the variable
//     winning over the file (SPEC-0010 REQ "Precedence Order"). With none set
//     no server is built at all, so there is no port to find.
//   - A bind or TLS failure is logged and the daemon carries on, as the
//     metrics and SSH listeners do: the local socket is the critical path, and
//     a sender whose deliveries fail notices. The bound sources report
//     `no_listener` rather than `listening`, which is the truth.
//   - The reload hook is registered at boot, before any reload source is
//     live (SetReloadHook is not synchronized), while the bind happens beside
//     startRemote. So the reload side has to tolerate a listener that is not
//     up yet — or never will be.
//
// These are functions, like startDaemonSources, so a wiring test drives what
// the daemon itself builds (#315).
//
// Governing: ADR-0021, ADR-0004; SPEC-0014 REQ "Webhook Listener", REQ
// "Source Reconciliation On Reload"; SPEC-0010 REQ "Precedence Order".
//
// @joestump 09/23/2026 - Introduced with the SPEC-0014 webhook listener (#458).
// @joestump 09/24/2026 - The listener counts into the source manager's
// counters (#480).

import (
	"context"
	"net"
	"sync"

	"charm.land/log/v2"

	"github.com/stump-wtf/harness/internal/metrics"
	"github.com/stump-wtf/harness/internal/supervisor"
	"github.com/stump-wtf/harness/internal/trigger/source"
	"github.com/stump-wtf/harness/internal/trigger/webhook"
)

// daemonWebhooks is the daemon's webhook listener, or the absence of one.
type daemonWebhooks struct {
	mgr     *supervisor.Manager
	sources *source.Manager
	// override is the address from the flag or the environment, "" for none.
	override string
	// opts carries test-only knobs (short timeouts); production leaves it
	// zero, which is every REQ "Webhook Listener" default.
	opts webhook.Options

	mu sync.Mutex
	// srv is nil until serve binds, and stays nil when the listener is off
	// or failed to bind.
	srv  *webhook.Server
	done chan struct{}
}

// beginDaemonWebhooks prepares the listener wiring. It binds nothing; serve
// does.
func beginDaemonWebhooks(mgr *supervisor.Manager, sources *source.Manager, override string) *daemonWebhooks {
	return &daemonWebhooks{mgr: mgr, sources: sources, override: override}
}

// serve binds and serves the listener when an address names one.
//
// It holds d.mu throughout, so a reload that lands while it is building
// either runs first — and serve reads the reloaded config — or waits and then
// swaps the table of the server serve built. Neither leaves a listener
// serving a table older than the config.
func (d *daemonWebhooks) serve() {
	d.mu.Lock()
	defer d.mu.Unlock()
	cfg := d.mgr.Config()
	settings := webhook.SettingsFrom(cfg.Server, d.override)
	if settings.Addr == "" {
		if len(cfg.Webhooks) > 0 {
			log.Info("webhook listener off; [webhook.*] sources report no_listener",
				"hint", "set [server] webhook_listen, --webhook-listen or HARNESS_WEBHOOK_LISTEN")
		}
		return
	}

	opts := d.opts
	opts.Settings = settings
	opts.Firer = d.sources
	// The source manager's counters, not the server's own: a refusal counted
	// here and a firing counted there must be one source's numbers in
	// `harness triggers` and /metrics (SPEC-0014 REQ "Trigger Metrics").
	opts.Outcomes = d.sources.Counters()
	opts.Log = log.Default()
	srv := webhook.New(opts, cfg)
	if err := srv.Listen(); err != nil {
		log.Error("webhook listener disabled", "addr", settings.Addr, "err", err)
		return
	}
	// Warned here, not only at config load: an address from the flag or the
	// environment never passes through the config parser's check.
	if !settings.TLS() {
		if host, _, err := net.SplitHostPort(settings.Addr); err == nil && !metrics.IsLoopback(host) {
			log.Warn("webhook listener is not on loopback and serves no TLS: deliveries and their credentials cross the network in cleartext",
				"addr", srv.Addr(),
				"hint", "bind 127.0.0.1 behind a TLS-terminating proxy, or set [server] webhook_tls_cert_file and webhook_tls_key_file")
		}
	}

	done := make(chan struct{})
	d.srv, d.done = srv, done
	d.sources.SetWebhookListening(true)
	log.Info("webhook listener serving", "addr", srv.Addr(), "tls", settings.TLS())
	go func() {
		defer close(done)
		if err := srv.Serve(); err != nil {
			log.Error("webhook listener stopped", "err", err)
			d.sources.SetWebhookListening(false)
		}
	}()
}

// reload applies a reloaded config: a fresh route table, and a "restart
// required" warning when the listener settings changed. A listener that was
// off stays off until the daemon restarts, and says so.
func (d *daemonWebhooks) reload() {
	d.mu.Lock()
	defer d.mu.Unlock()
	cfg := d.mgr.Config()
	want := webhook.SettingsFrom(cfg.Server, d.override)
	srv := d.srv
	if srv == nil {
		if want.Addr != "" {
			log.Warn("webhook listener settings changed; restart the daemon to apply them (no listener is running)", "want_addr", want.Addr)
		}
		return
	}
	srv.Reload(cfg, want)
}

// addr is the bound address, "" when no listener is running.
func (d *daemonWebhooks) addr() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.srv == nil {
		return ""
	}
	return d.srv.Addr()
}

// shutdown stops accepting and gives in-flight deliveries webhook.ShutdownGrace
// to finish.
func (d *daemonWebhooks) shutdown() {
	d.mu.Lock()
	srv, done := d.srv, d.done
	d.mu.Unlock()
	if srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), webhook.ShutdownGrace)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Warn("webhook listener: in-flight deliveries cut off at shutdown", "err", err)
	}
	<-done
}

// wireWebhookReload composes the listener's reload onto the Manager's reload
// hook, after whatever is already registered — source reconciliation among
// it, so a route's bound state and its source's reported state agree.
// Governing: SPEC-0014 REQ "Source Reconciliation On Reload".
func wireWebhookReload(mgr *supervisor.Manager, d *daemonWebhooks) {
	prev := mgr.ReloadHook()
	mgr.SetReloadHook(func() {
		if prev != nil {
			prev()
		}
		d.reload()
	})
}
