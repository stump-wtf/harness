package daemon

// Re-extraction on replay — tests
//
// REQ-8's replay rule: `harness trigger --event` re-extracts the typed values
// from the stored body, so a hand-edited `typed` in a replay file is ignored
// and the replay renders exactly what the original run would have.
//
// Governing: SPEC-0017 REQ-8, REQ-7; issue #507.

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/trigger"
)

// reextractConfig is a config whose `gitea-pr` webhook source verifies as
// gitea, so reextract reads the scheme a real daemon would.
func reextractConfig() *core.Config {
	return &core.Config{
		Webhooks: map[string]core.WebhookSource{
			"gitea-pr": {Name: "gitea-pr", Verify: core.VerifyGitea},
		},
	}
}

// TestReextractIgnoresHandEditedTyped is REQ-8's replay criterion: an
// envelope whose `typed.repo` was edited to a lie is re-derived from the
// stored body, and the edited value is gone. The body is the extract
// package's real captured Gitea pull request.
func TestReextractIgnoresHandEditedTyped(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "trigger", "extract", "testdata", "gitea-pull_request.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	wh := &trigger.WebhookEvent{Event: "pull_request", Delivery: "delivery-1", ContentType: "application/json"}
	wh.SetBody("application/json", body)
	env := &trigger.Envelope{
		Version: trigger.EnvelopeVersion, Kind: trigger.KindWebhook,
		Source: "webhook.gitea-pr", EventID: "delivery-1",
		ReceivedAt: time.Now().UTC(), Webhook: wh,
		// The hand edit: a repo the body never said, plus a dash-leading
		// number no table would yield.
		Typed: map[string]any{"repo": "evil/example", "number": "-1"},
	}
	h := core.Harness{Name: "pr-review", Triggers: []string{"webhook.gitea-pr"}}

	reextract(h, env, reextractConfig())

	if env.Typed["repo"] != "stump.wtf/harness" {
		t.Errorf("typed repo after replay = %#v, want the body's own value", env.Typed["repo"])
	}
	if env.Typed["number"] != int64(412) {
		t.Errorf("typed number after replay = %#v, want 412", env.Typed["number"])
	}
	if env.Typed["action"] != "opened" || env.Typed["name"] != "pull_request" {
		t.Errorf("typed after replay = %#v", env.Typed)
	}
}

// TestReextractChannelAndUnknown: a channel envelope re-derives its meta
// keys, and an envelope naming a source the config does not declare yields
// no typed values at all rather than trusting the file's.
func TestReextractChannelAndUnknown(t *testing.T) {
	env := &trigger.Envelope{
		Version: trigger.EnvelopeVersion, Kind: trigger.KindChannel,
		Source: "channel.sb", EventID: "e1", ReceivedAt: time.Now().UTC(),
		Channel: &trigger.ChannelEvent{
			Content: "doorbell",
			Meta:    map[string]string{"todo_id": "td_1", "queue": "lane-l", "junk": "--bad"},
		},
		Typed: map[string]any{"meta.queue": "--tampered"},
	}
	h := core.Harness{Name: "sb", Triggers: []string{"channel.sb"}}
	reextract(h, env, reextractConfig())
	want := map[string]any{"meta.todo_id": "td_1", "meta.queue": "lane-l"}
	if len(env.Typed) != len(want) || env.Typed["meta.queue"] != "lane-l" || env.Typed["meta.todo_id"] != "td_1" {
		t.Errorf("typed after channel replay = %#v, want %#v", env.Typed, want)
	}

	env2 := &trigger.Envelope{
		Version: trigger.EnvelopeVersion, Kind: trigger.KindWebhook,
		Source: "webhook.undeclared", EventID: "e2", ReceivedAt: time.Now().UTC(),
		Webhook: &trigger.WebhookEvent{Event: "push", ContentType: "application/json"},
		Typed:   map[string]any{"repo": "smuggled/example"},
	}
	reextract(h, env2, reextractConfig())
	if env2.Typed != nil {
		t.Errorf("typed for an undeclared source = %#v, want nil", env2.Typed)
	}
}
