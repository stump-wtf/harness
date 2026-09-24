package source

// The webhook half of the source manager's state: what each `[webhook.*]`
// source reports.
//
// A webhook source has no session to supervise — the listener serves it — so
// its state is derived, not observed: `disabled` for `enabled = false`,
// `unbound` when no harness lists it, and otherwise `listening` or
// `no_listener` according to whether the daemon's webhook listener is bound.
// It is re-derived at Start, on every Reconcile, and whenever the listener's
// own state is reported.
//
// Governing: ADR-0021; SPEC-0014 REQ "Webhook Listener" ("a [webhook.*] table
// with no listener configured is valid. Its state SHALL be no_listener"), REQ
// "Trigger Visibility".
//
// @joestump 09/23/2026 - Introduced with the SPEC-0014 webhook listener (#458).

import (
	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/trigger"
)

// SetWebhookListening records whether the webhook listener is bound, and
// re-derives every webhook source's state from it. The daemon calls it once
// the listener binds; a daemon with no listener never calls it, and its bound
// webhook sources report `no_listener`.
func (m *Manager) SetWebhookListening(on bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.webhookListening = on
	if m.closed || m.ctx == nil {
		return
	}
	m.reconcileWebhooksLocked(m.config())
}

// reconcileWebhooksLocked brings every webhook source's reported state in line
// with cfg. A source whose state is unchanged keeps its record — and so its
// counters and its `since` — and one no longer declared is forgotten. Caller
// holds m.mu.
func (m *Manager) reconcileWebhooksLocked(cfg *core.Config) {
	want := map[string]trigger.SourceState{}
	for _, src := range cfg.OrderedWebhooks() {
		ref := core.SourceKindWebhook + "." + src.Name
		switch {
		case !src.Enabled:
			want[ref] = trigger.StateDisabled
		case len(cfg.BoundHarnesses(ref)) == 0:
			want[ref] = trigger.StateUnbound
		case m.webhookListening:
			want[ref] = trigger.StateListening
		default:
			want[ref] = trigger.StateNoListener
		}
	}
	for ref, st := range m.sources {
		if st.Kind() != core.SourceKindWebhook {
			continue
		}
		if _, still := want[ref]; !still {
			delete(m.sources, ref)
		}
	}
	for _, src := range cfg.OrderedWebhooks() {
		ref := core.SourceKindWebhook + "." + src.Name
		if st := m.sources[ref]; st != nil && st.status.State == want[ref] {
			continue
		}
		m.setStatusLocked(ref, core.SourceKindWebhook, want[ref], "")
	}
}
