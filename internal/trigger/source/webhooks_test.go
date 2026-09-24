package source

// Webhook source states. The property is the state the manager REPORTS for
// each [webhook.*] source, because that is what `harness triggers` and the
// metrics will render: a bound source with no listener must say
// `no_listener`, never pretend to be served.
//
// Governing: SPEC-0014 REQ "Webhook Listener", REQ "Trigger Visibility".

import (
	"context"
	"testing"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/trigger"
)

func webhookConfig(sources map[string]bool, bound ...string) *core.Config {
	cfg := &core.Config{Harnesses: map[string]core.Harness{}, Webhooks: map[string]core.WebhookSource{}}
	for _, name := range []string{"ci", "off", "lonely"} {
		enabled, ok := sources[name]
		if !ok {
			continue
		}
		cfg.Webhooks[name] = core.WebhookSource{Name: name, Verify: core.VerifyBearer, Enabled: enabled}
		cfg.WebhookOrder = append(cfg.WebhookOrder, name)
	}
	for _, name := range bound {
		h := core.Harness{Name: "h-" + name, Triggers: []string{"webhook." + name}}
		cfg.Harnesses[h.Name] = h
		cfg.HarnessOrder = append(cfg.HarnessOrder, h.Name)
	}
	return cfg
}

func stateOf(t *testing.T, m *Manager, ref string) trigger.SourceState {
	t.Helper()
	st, ok := m.StatusOf(ref)
	if !ok {
		return ""
	}
	return st.State
}

func TestWebhookSourceStates(t *testing.T) {
	cfg := webhookConfig(map[string]bool{"ci": true, "off": false, "lonely": true}, "ci", "off")
	current := cfg
	m := New(Options{Runner: &fakeRunner{}, Config: func() *core.Config { return current }})
	m.Start(context.Background())
	t.Cleanup(m.Close)

	want := map[string]trigger.SourceState{
		"webhook.ci":     trigger.StateNoListener,
		"webhook.off":    trigger.StateDisabled,
		"webhook.lonely": trigger.StateUnbound,
	}
	for ref, w := range want {
		if got := stateOf(t, m, ref); got != w {
			t.Errorf("before the listener: %s = %q, want %q", ref, got, w)
		}
	}

	m.SetWebhookListening(true)
	want["webhook.ci"] = trigger.StateListening
	for ref, w := range want {
		if got := stateOf(t, m, ref); got != w {
			t.Errorf("listening: %s = %q, want %q", ref, got, w)
		}
	}

	// A reload that binds `lonely` and drops `off` from the config.
	current = webhookConfig(map[string]bool{"ci": true, "lonely": true}, "ci", "lonely")
	m.Reconcile(current)
	if got := stateOf(t, m, "webhook.lonely"); got != trigger.StateListening {
		t.Errorf("after reload: webhook.lonely = %q, want listening", got)
	}
	if _, ok := m.StatusOf("webhook.off"); ok {
		t.Error("a source removed from the config is still reported")
	}

	m.SetWebhookListening(false)
	if got := stateOf(t, m, "webhook.ci"); got != trigger.StateNoListener {
		t.Errorf("listener gone: webhook.ci = %q, want no_listener", got)
	}
}

// TestWebhookStateKeepsItsRecordWhenUnchanged: a reload that leaves a source's
// state alone keeps its record, so `since` does not reset on every rewrite.
func TestWebhookStateKeepsItsRecordWhenUnchanged(t *testing.T) {
	cfg := webhookConfig(map[string]bool{"ci": true}, "ci")
	m := New(Options{Runner: &fakeRunner{}, Config: func() *core.Config { return cfg }})
	m.Start(context.Background())
	t.Cleanup(m.Close)
	m.SetWebhookListening(true)
	before, _ := m.StatusOf("webhook.ci")
	m.Reconcile(cfg)
	after, _ := m.StatusOf("webhook.ci")
	if !after.Since.Equal(before.Since) {
		t.Errorf("an unchanged reload reset since: %v -> %v", before.Since, after.Since)
	}
}
