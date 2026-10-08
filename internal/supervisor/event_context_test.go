package supervisor

// Event Context Tests
//
// REQ-7's event tier, end to end: a triggered command harness renders
// event.* from the envelope's validated typed values, a required event path
// that a push delivery cannot answer skips the run with
// template_unresolved (REQ-11), and the raw payload never reaches the
// child's argv.
//
// The payloads are the extract package's real captured deliveries, read
// from its testdata so a change in one place re-tests the other.
//
// Governing: SPEC-0017 REQ-7 "Template Context", REQ-11 "Rendering"; issue
// #507.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/trigger"
	"github.com/stump-wtf/harness/internal/trigger/extract"
)

// giteaPREnvelope builds the envelope a verified Gitea pull_request delivery
// becomes, typed by the same extractor production uses.
func giteaPREnvelope(t *testing.T, fixture string) *trigger.Envelope {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "trigger", "extract", "testdata", fixture))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	wh := &trigger.WebhookEvent{Event: "pull_request", Delivery: "delivery-1", ContentType: "application/json"}
	wh.SetBody("application/json", body)
	return &trigger.Envelope{
		Version:    trigger.EnvelopeVersion,
		Kind:       trigger.KindWebhook,
		Source:     "webhook.gitea-pr",
		EventID:    "delivery-1",
		ReceivedAt: time.Date(2026, 10, 8, 6, 4, 15, 0, time.UTC),
		Webhook:    wh,
		Typed:      extract.FromWebhook(core.VerifyGitea, "pull_request", wh.Body),
	}
}

// TestTriggeredCommandRendersEventContext is REQ-8's "A Gitea pull request"
// as the agent's process sees it: a run fired by the fixture's delivery
// passes the validated typed values to the child as arguments, one per
// element, and the raw payload body never becomes one.
func TestTriggeredCommandRendersEventContext(t *testing.T) {
	e := newRunsEnv(t)
	h, out := commandTriggered(t, "pr", "review", "--repo", "{{event.repo}}", "--num", "{{event.number}}", "--by", "{{event.action}}", "--url", "{{event.url?}}", "--sha", "{{event.sha}}", "{{event.meta.queue?}}")
	h.Triggers = []string{"webhook.gitea-pr"}
	m, _ := e.manager(t, sweepCfg(h), fastPolicy())

	env := giteaPREnvelope(t, "gitea-pull_request.json")
	m.StartRun("pr", RunRequest{Trigger: TriggerWebhook, Source: env.Source, Event: env})
	waitRuns(t, m, "pr", "the run finishes", outcomesAre(OutcomeSuccess))

	p := readProbe(t, out)
	want := []string{
		"review", "--repo", "stump.wtf/harness", "--num", "412",
		"--by", "opened", "--url", "https://gitea.stump.rocks/stump.wtf/harness/pulls/412",
		"--sha", "4590c0727e7f7087c9b3bede0830e3130ef6f43d", "",
	}
	if !slices.Equal(p.Args, want) {
		t.Errorf("child received %q, want %q", p.Args, want)
	}
	// The run started; the log line is enough to know the spawn path ran.
	if !strings.Contains(readText(t, filepath.Join(e.logs, "pr.log")), "run started") {
		t.Errorf("run log missing the start line:\n%s", readText(t, filepath.Join(e.logs, "pr.log")))
	}
}

// TestCommandEventNumberUnresolvedOnPush is REQ-11's "Unresolved required
// value": a push delivery has no number, so a required {{event.number}}
// spawns nothing and the run record is skipped with reason
// template_unresolved and the path's name.
func TestCommandEventNumberUnresolvedOnPush(t *testing.T) {
	e := newRunsEnv(t)
	h, out := commandTriggered(t, "ci", "check", "--num", "{{event.number}}")
	h.Triggers = []string{"webhook.gitea-pr"}
	m, _ := e.manager(t, sweepCfg(h), fastPolicy())

	env := giteaPREnvelope(t, "gitea-push.json")
	m.StartRun("ci", RunRequest{Trigger: TriggerWebhook, Source: env.Source, Event: env})
	rec := waitRuns(t, m, "ci", "the skipped run is recorded", outcomesAre(OutcomeSkipped))[0]
	if rec.Reason != ReasonTemplateUnresolved || rec.MissingPath != core.PathEventNumber {
		t.Errorf("record reason/path = %q/%q, want template_unresolved/event.number", rec.Reason, rec.MissingPath)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("the probe ran for a push delivery (stat: %v)", err)
	}
}

// TestRenderContextEventTier asserts the tier directly: present when the run
// carries an event, absent (never empty) when it does not, so a required
// reference fails the render instead of passing an empty argument.
func TestRenderContextEventTier(t *testing.T) {
	started := time.Date(2026, 10, 8, 6, 4, 15, 0, time.UTC)
	env := giteaPREnvelope(t, "gitea-pull_request.json")
	run := RunEnv{
		RunID: 7, Trigger: TriggerWebhook, Source: env.Source, StartedAt: started,
		EventFile: "/logs/run-7.event.json",
		Event: &EventContext{
			File:       "/logs/run-7.event.json",
			ID:         env.TemplateID(),
			Kind:       string(env.Kind),
			Source:     env.Source,
			ReceivedAt: env.ReceivedAt.UTC().Format(time.RFC3339),
			Typed:      env.Typed,
		},
	}
	ctx := renderContext(core.Harness{Name: "r"}, "/w", run, started)
	for path, want := range map[string]string{
		core.PathEventFile:       "/logs/run-7.event.json",
		core.PathEventID:         "delivery-1",
		core.PathEventKind:       "webhook",
		core.PathEventSource:     "webhook.gitea-pr",
		core.PathEventReceivedAt: "2026-10-08T06:04:15Z",
		core.PathEventName:       "pull_request",
		core.PathEventAction:     "opened",
		core.PathEventRepo:       "stump.wtf/harness",
		core.PathEventNumber:     "412",
		core.PathEventSHA:        "4590c0727e7f7087c9b3bede0830e3130ef6f43d",
	} {
		if got, ok := ctx.Lookup(path); !ok || got != want {
			t.Errorf("%s = %q,%v want %q", path, got, ok, want)
		}
	}
	// A scheduled firing carries no event: every event.* path is absent,
	// which is REQ-7's "Optional event field on a scheduled firing" from the
	// render side (the optional form renders empty; a required one fails).
	sched := renderContext(core.Harness{Name: "r"}, "/w", RunEnv{RunID: 7, Trigger: TriggerSchedule, StartedAt: started}, started)
	for _, path := range []string{core.PathEventFile, core.PathEventID, core.PathEventKind, core.PathEventSource, core.PathEventName, core.PathEventRepo, core.PathEventNumber} {
		if v, ok := sched.Lookup(path); ok {
			t.Errorf("%s present (%q) on a scheduled firing", path, v)
		}
	}
	// A non-ID-shaped event_id yields a daemon-generated one that is itself
	// ID-shaped, and is stable across renders of the same envelope.
	env.EventID = "not an id!!"
	if id := env.TemplateID(); !isTemplateID(id) || id != env.TemplateID() {
		t.Errorf("TemplateID = %q, want a stable daemon-generated ID shape", id)
	}
}

func isTemplateID(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
			c == '_' || c == '.' || c == ':' || c == '-'
		if i == 0 && !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			return false
		}
		if !ok {
			return false
		}
	}
	return true
}

// TestEventFileCarriesTyped: the written envelope holds the typed object,
// and the file the agent reads and the template render agree.
func TestEventFileCarriesTyped(t *testing.T) {
	e := newRunsEnv(t)
	h, _ := commandTriggered(t, "pr", "echo", "ran")
	h.Triggers = []string{"webhook.gitea-pr"}
	m, _ := e.manager(t, sweepCfg(h), fastPolicy())

	env := giteaPREnvelope(t, "gitea-pull_request.json")
	m.StartRun("pr", RunRequest{Trigger: TriggerWebhook, Source: env.Source, Event: env})
	rec := waitRuns(t, m, "pr", "the run finishes", outcomesAre(OutcomeSuccess))[0]

	b := readText(t, eventPath(m, "pr", rec.RunID))
	var onDisk struct {
		Version int            `json:"version"`
		Typed   map[string]any `json:"typed"`
	}
	if err := json.Unmarshal([]byte(b), &onDisk); err != nil {
		t.Fatalf("event file is not JSON: %v", err)
	}
	if onDisk.Version != trigger.EnvelopeVersion {
		t.Errorf("envelope version = %d, want %d (typed is additive)", onDisk.Version, trigger.EnvelopeVersion)
	}
	if onDisk.Typed["repo"] != "stump.wtf/harness" || onDisk.Typed["number"] != float64(412) {
		t.Errorf("typed on disk = %#v", onDisk.Typed)
	}
}
