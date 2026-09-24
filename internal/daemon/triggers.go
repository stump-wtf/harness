package daemon

// Trigger Visibility Over The Protocol
//
// The triggers op, the `triggers` field on the harness and job projections,
// and the trigger_source_changed event: the answer to "did anything hear the
// doorbell?" (SPEC-0014 REQ "Trigger Visibility").
//
// Everything a reply says about a source is derived from two daemon-side
// truths, never from the client: the live config (what is declared, bound and
// how it verifies) and the source manager's reported state (what is actually
// connected, listening, failing, and how often it fired). Neither carries a
// credential onto the wire: header names only, a channel URL without its
// query, and a last error the source manager already scrubbed.
//
// Governing: ADR-0021, ADR-0002 (control mirrors the CLI verbs), ADR-0008;
// SPEC-0014 REQ "Trigger Visibility", REQ "Credential Resolution", REQ "Error
// Handling Standards"; SPEC-0002 REQ "Control Operations", REQ "Event
// Subscription".
//
// @joestump 09/24/2026 - Introduced with `harness triggers` (#476).

import (
	"time"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/protocol"
	"github.com/stump-wtf/harness/internal/trigger"
	"github.com/stump-wtf/harness/internal/trigger/source"
)

// TriggerSources is the source manager as the protocol server reads it.
// *source.Manager implements it; nil means the daemon runs none, and every
// source then reports an empty state rather than a guessed one.
type TriggerSources interface {
	// Status returns every source's reported state.
	Status() []source.Status
	// StatusOf returns one source's reported state.
	StatusOf(ref string) (source.Status, bool)
}

// hooksPath is the route a webhook source is served on — the listener's own
// prefix (internal/trigger/webhook), restated because the protocol reports it
// whether or not a listener is running.
const hooksPath = "/hooks/"

// SetWebhook records the running webhook listener's bound address and whether
// it serves TLS, for daemon_info. An empty addr marks it as not running.
func (s *Server) SetWebhook(addr string, tls bool) {
	s.remoteMu.Lock()
	defer s.remoteMu.Unlock()
	s.webhookAddr, s.webhookTLS = addr, tls
}

// webhook reports what SetWebhook recorded.
func (s *Server) webhook() (string, bool) {
	s.remoteMu.Lock()
	defer s.remoteMu.Unlock()
	return s.webhookAddr, s.webhookTLS
}

// PublishTriggerSource broadcasts one source state change as
// trigger_source_changed. The daemon installs it as the source manager's
// state handler, which delivers every change, in order, per source.
func (s *Server) PublishTriggerSource(st source.Status) {
	s.broadcast(protocol.EventMsg{
		Kind:       protocol.EvTriggerSourceChanged,
		Source:     st.Source,
		SourceKind: st.Kind,
		State:      string(st.State),
		Error:      st.Reason,
	})
}

// opTriggers lists every declared source: channels, then webhooks, each in
// config order.
func (c *conn) opTriggers() []protocol.TriggerSourceInfo {
	cfg := c.srv.mgr.Config()
	out := []protocol.TriggerSourceInfo{}
	for _, src := range cfg.OrderedChannels() {
		info := c.sourceInfo(cfg, core.SourceKindChannel, src.Name, src.Description)
		info.URL = source.StripQuery(src.URL)
		info.Headers = src.HeaderNames()
		out = append(out, info)
	}
	for _, src := range cfg.OrderedWebhooks() {
		info := c.sourceInfo(cfg, core.SourceKindWebhook, src.Name, src.Description)
		info.Path = hooksPath + src.Name
		info.Verify = string(src.Verify)
		info.Events = append([]string(nil), src.Events...)
		out = append(out, info)
	}
	return out
}

// sourceInfo fills the fields every kind of source shares.
func (c *conn) sourceInfo(cfg *core.Config, kind, name, description string) protocol.TriggerSourceInfo {
	ref := kind + "." + name
	info := protocol.TriggerSourceInfo{
		Source:      ref,
		Kind:        kind,
		Harnesses:   append([]string{}, cfg.BoundHarnesses(ref)...),
		Counters:    map[string]int{},
		Description: description,
	}
	for _, o := range trigger.Outcomes {
		info.Counters[string(o)] = 0
	}
	st, ok := c.sourceStatus(ref)
	if !ok {
		return info
	}
	info.State = string(st.State)
	info.Error = st.Reason
	info.Since = stamp(st.Since)
	info.DownSince = stamp(st.DownSince)
	info.LastEvent = stamp(st.LastEvent)
	for o, n := range st.Counts {
		info.Counters[string(o)] = n
	}
	return info
}

// sourceStatus reads one source's state, reporting false when the daemon runs
// no source manager or the manager does not know the source.
func (c *conn) sourceStatus(ref string) (source.Status, bool) {
	if c.srv.triggers == nil {
		return source.Status{}, false
	}
	return c.srv.triggers.StatusOf(ref)
}

// triggerBindings projects a harness's `triggers` with each source's state.
func (c *conn) triggerBindings(h core.Harness) []protocol.TriggerBinding {
	if len(h.Triggers) == 0 {
		return nil
	}
	out := make([]protocol.TriggerBinding, 0, len(h.Triggers))
	for _, ref := range h.Triggers {
		b := protocol.TriggerBinding{Source: ref}
		if st, ok := c.sourceStatus(ref); ok {
			b.State = string(st.State)
		}
		out = append(out, b)
	}
	return out
}

// stamp renders t as RFC 3339, "" for the zero time.
func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}
