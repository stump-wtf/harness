package main

// Daemon Notify Wiring
//
// Builds the [notify] hook's dispatcher over the daemon's own Manager and
// connects every event source to it: the lifecycle bus (give-up into failed,
// flapping, failed runs, recovery), the runaway tool-loop guard's stops, and
// the session guard's rotations. These are functions, like
// daemonManagerOptions, so the wiring tests drive what the daemon itself
// builds (#315): a test that hand-built a dispatcher and called it would pass
// against a daemon that never connected one.
//
// The dispatcher lives for the daemon's lifetime and follows reloads, so a
// [notify] table added, changed or removed in harness.toml applies without a
// restart — the same way a harness definition does.
//
// Governing: SPEC-0003 REQ "Operator Notification"; issue #725.
//
// @joestump-agent 09/26/2026 - Added for harness#725.

import (
	"time"

	"charm.land/log/v2"

	"github.com/stump-wtf/harness/internal/loopguard"
	"github.com/stump-wtf/harness/internal/notify"
	"github.com/stump-wtf/harness/internal/supervisor"
)

// daemonNotifier is the running notify pipeline: the dispatcher that runs the
// hook, and the watcher that feeds it from the lifecycle bus and the guards.
type daemonNotifier struct {
	d *notify.Dispatcher
	w *notify.Watcher
}

// startDaemonNotify builds the dispatcher for the Manager's current [notify]
// table and subscribes the watcher to its lifecycle bus. runDaemon calls it
// before Autostart, so a harness that gives up during boot is reported. It
// always returns a pipeline: with no [notify] table it delivers nothing until
// a reload adds one.
func startDaemonNotify(mgr *supervisor.Manager, opts notify.Options) *daemonNotifier {
	cfg := mgr.Config().Notify
	d := notify.New(cfg, opts)
	logNotifyConfig(cfg.Enabled(), d)
	return &daemonNotifier{d: d, w: notify.Watch(mgr, d)}
}

// logNotifyConfig says, once, whether alerts will reach anyone.
func logNotifyConfig(on bool, d *notify.Dispatcher) {
	if !on {
		log.Info("notify hook off", "hint", "add a [notify] table to harness.toml to be alerted when a harness fails")
		return
	}
	c := d.Config()
	log.Info("notify hook active", "command", c.Command[0], "events", c.Events, "timeout", c.Timeout, "cooldown", c.Cooldown)
}

// wireNotifyReload composes the dispatcher's reload onto the Manager's reload
// hook, preserving whatever is already registered (see wireSourceReload for
// why composing, not replacing, is the whole point).
func wireNotifyReload(mgr *supervisor.Manager, n *daemonNotifier) {
	prev := mgr.ReloadHook()
	mgr.SetReloadHook(func() {
		if prev != nil {
			prev()
		}
		next := mgr.Config().Notify
		if next.Equal(n.d.Config()) {
			return
		}
		n.d.SetConfig(next)
		logNotifyConfig(next.Enabled(), n.d)
	})
}

// loopGuardOptions connects the guard's stops to the notifier.
func (n *daemonNotifier) loopGuardOptions(opts loopguard.Options) loopguard.Options {
	if n != nil {
		opts.OnTrip = n.w.LoopStopped
	}
	return opts
}

// registerMetrics adds harness_notify_deliveries_total to the daemon's
// /metrics registry, when the listener is on.
func (n *daemonNotifier) registerMetrics(dm *daemonMetrics) {
	if n == nil || dm == nil || dm.m == nil {
		return
	}
	dm.m.Registry().MustRegister(n.d.Collector())
}

// Close stops the watcher, then gives deliveries in flight their grace.
func (n *daemonNotifier) Close() {
	if n == nil {
		return
	}
	n.w.Close()
	n.d.Close()
}

// startDaemonSessionGuard builds the session guard (issue #347), reports its
// rotations to the notifier, registers it on the Manager and starts it. Zero
// interval and lookback take the defaults.
func startDaemonSessionGuard(mgr *supervisor.Manager, n *daemonNotifier, interval, lookback time.Duration) *supervisor.SessionGuard {
	g := supervisor.NewSessionGuard(mgr, interval, lookback)
	if n != nil {
		g.OnRotate(n.w.SessionRotated)
	}
	mgr.SetSessionGuard(g)
	g.Start()
	return g
}
