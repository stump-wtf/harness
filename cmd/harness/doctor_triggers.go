package main

// Doctor: Trigger Sources
//
// The setups that fail quietly. Every one of them leaves the daemon running,
// `harness list` calm, and an event harness that simply never fires:
//
//   - a webhook listener on a non-loopback address without TLS (deliveries
//     and their credentials cross the network in cleartext);
//   - a `[webhook.*]` source with no listener to serve it (`no_listener`);
//   - a source `env_file` that group or other can read;
//   - a channel source in `error` — retrying only at the ceiling, because
//     trying sooner will not fix a wrong credential or a server that is not a
//     channel server.
//
// Judged from the daemon's own report when it is reachable — the listener it
// actually bound and the states its sources actually reached — and from the
// config when it is not, so a stopped daemon still gets the config-only half.
// triggersCheck is pure over its inputs so each condition has a test that
// shows it can fire (CLAUDE.md "A zero").
//
// Governing: ADR-0021; SPEC-0014 REQ "Webhook Listener", REQ "Credential
// Resolution", REQ "Trigger Visibility"; SPEC-0001 REQ "Zero And Error
// States".
//
// @joestump 09/24/2026 - Introduced for stump.wtf/harness#476.

import (
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/stump-wtf/harness/internal/cliui"
	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/metrics"
	"github.com/stump-wtf/harness/internal/protocol"
	"github.com/stump-wtf/harness/internal/trigger"
)

// triggerInputs is what triggersCheck judges.
type triggerInputs struct {
	// cfg is the loaded config; nil when it failed to load.
	cfg *core.Config
	// daemon is daemon_info, nil when the daemon was unreachable. Its
	// WebhookAddr/WebhookTLS are the listener that actually bound.
	daemon *protocol.DaemonInfo
	// sources is the triggers op's reply, nil when it could not be fetched
	// (daemon down, or older than ProtoMinor 13).
	sources []protocol.TriggerSourceInfo
	// stat reads a file's mode; os.Stat in production.
	stat func(string) (os.FileInfo, error)
}

// triggersCheck returns the "triggers" row, or nil when the config declares
// no source and no listener is running — there is nothing to judge, and a
// row saying so on every doctor run would be noise.
func triggersCheck(in triggerInputs) *check {
	if in.cfg == nil {
		return nil
	}
	nSources := len(in.cfg.Channels) + len(in.cfg.Webhooks)
	listenerAddr, listenerTLS := triggerListener(in)
	if nSources == 0 && listenerAddr == "" {
		return nil
	}

	var (
		warns, errs []string
		hints       []string
	)
	if listenerAddr != "" && !listenerTLS && !loopbackAddr(listenerAddr) {
		warns = append(warns, fmt.Sprintf("webhook listener on %s serves no TLS", listenerAddr))
		hints = append(hints, "bind 127.0.0.1 behind a TLS-terminating proxy, or set [server] webhook_tls_cert_file and webhook_tls_key_file")
	}
	if nl := noListenerSources(in); len(nl) > 0 {
		warns = append(warns, fmt.Sprintf("no listener serves %s", strings.Join(nl, ", ")))
		hints = append(hints, "set [server] webhook_listen (or --webhook-listen / HARNESS_WEBHOOK_LISTEN) and restart the daemon")
	}
	if lax := laxEnvFiles(in); len(lax) > 0 {
		warns = append(warns, fmt.Sprintf("env_file readable by group or other: %s", strings.Join(lax, ", ")))
		hints = append(hints, "chmod 600 each env_file named")
	}
	for _, s := range in.sources {
		if s.Kind == core.SourceKindChannel && s.State == string(trigger.StateError) {
			errs = append(errs, fmt.Sprintf("%s is in error: %s", s.Source, orDash(s.Error)))
		}
	}
	if len(errs) > 0 {
		hints = append(hints, "fix the channel's url or credential; `harness triggers` shows the last error")
	}

	switch {
	case len(errs) > 0:
		return &check{name: "triggers", level: cliui.LevelError,
			detail: strings.Join(append(errs, warns...), "; "), hint: strings.Join(hints, "; ")}
	case len(warns) > 0:
		return &check{name: "triggers", level: cliui.LevelWarn,
			detail: strings.Join(warns, "; "), hint: strings.Join(hints, "; ")}
	}
	detail := fmt.Sprintf("%d source(s) healthy", nSources)
	if in.sources == nil {
		detail = fmt.Sprintf("%d source(s) configured (daemon not consulted)", nSources)
	}
	return &check{name: "triggers", level: cliui.LevelSuccess, detail: detail}
}

// triggerListener is the webhook listener to judge: the one the daemon bound
// when it is reachable, otherwise the one the config asks for.
func triggerListener(in triggerInputs) (addr string, tls bool) {
	if in.daemon != nil {
		return in.daemon.WebhookAddr, in.daemon.WebhookTLS
	}
	sc := in.cfg.Server
	return strings.TrimSpace(sc.WebhookListen), sc.WebhookTLSCertFile != "" && sc.WebhookTLSKeyFile != ""
}

// loopbackAddr reports whether a host:port binds only loopback. An empty host
// (":8443") binds every interface, which is not.
func loopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	return metrics.IsLoopback(host)
}

// noListenerSources are the webhook sources no listener serves: those the
// daemon reports `no_listener`, or — with no daemon to ask — the enabled,
// bound ones when the config names no listener.
func noListenerSources(in triggerInputs) []string {
	var out []string
	if in.sources != nil {
		for _, s := range in.sources {
			if s.State == string(trigger.StateNoListener) {
				out = append(out, s.Source)
			}
		}
		return out
	}
	if in.daemon != nil || strings.TrimSpace(in.cfg.Server.WebhookListen) != "" {
		return nil
	}
	for _, src := range in.cfg.OrderedWebhooks() {
		ref := core.SourceKindWebhook + "." + src.Name
		if src.Enabled && len(in.cfg.BoundHarnesses(ref)) > 0 {
			out = append(out, ref)
		}
	}
	return out
}

// laxEnvFiles are the sources whose env_file group or other can read, as
// "source (path)". A file that cannot be stat'ed is not reported here: config
// load already refused it.
func laxEnvFiles(in triggerInputs) []string {
	stat := in.stat
	if stat == nil {
		stat = os.Stat
	}
	var out []string
	check := func(ref, path string) {
		if path == "" {
			return
		}
		if info, err := stat(path); err == nil && info.Mode().Perm()&0o077 != 0 {
			out = append(out, fmt.Sprintf("%s (%s)", ref, path))
		}
	}
	for _, src := range in.cfg.OrderedChannels() {
		check(core.SourceKindChannel+"."+src.Name, src.EnvFile)
	}
	for _, src := range in.cfg.OrderedWebhooks() {
		check(core.SourceKindWebhook+"."+src.Name, src.EnvFile)
	}
	return out
}
