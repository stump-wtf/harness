package main

// The GitHub preset, end to end through the daemon's own wiring: a
// `[webhook.*]` table parsed by config.Load, served by the listener
// beginDaemonWebhooks builds, fired through the source manager into a
// finished run record and a 0600 event file.
//
// The signature is a literal computed with `openssl dgst -sha256 -hmac`, not by
// this test signing with crypto/hmac, so a verifier that agrees only with
// itself cannot pass it.
//
// Governing: ADR-0021; SPEC-0014 REQ "Webhook Verification", REQ "Webhook
// Source Table", REQ "Event Delivery To The Run".
//
// @joestump 09/24/2026 - Introduced with the HMAC schemes and presets (#462).

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/config"
	"github.com/stump-wtf/harness/internal/supervisor"
	"github.com/stump-wtf/harness/internal/trigger"
)

const (
	ghWiringSecret = "gh-wiring-Voh3ieFa-secret"
	ghWiringBody   = `{"action":"completed", "note":"sentinel Oor8ahZe"}`
	// printf '%s' "$ghWiringBody" | openssl dgst -sha256 -hmac "$ghWiringSecret"
	ghWiringSig = "sha256=553b2541b5d3d5b52db8a3487af0a14036fe8909d3846f4011cc842f7ca010eb"
)

func postGitHub(t *testing.T, url, sig, delivery, body string) int {
	t.Helper()
	req, err := http.NewRequest("POST", url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", "workflow_run")
	req.Header.Set("X-GitHub-Delivery", delivery)
	req.Header.Set("X-Hub-Signature-256", sig)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func TestDaemonWiringFiresAHarnessFromAGitHubWebhook(t *testing.T) {
	// The three-line table an operator writes, through the real parser.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "s.env"), []byte("GH_HOOK_SECRET="+ghWiringSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "harness.toml")
	if err := os.WriteFile(cfgPath, []byte("[webhook.ci]\nverify = \"github\"\nenv_file = \"s.env\"\nsecret = \"${GH_HOOK_SECRET}\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	parsed, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	fields := filepath.Join(t.TempDir(), "fields")
	cfg := webhookWiringConfig(fields, true)
	cfg.Webhooks["ci"] = parsed.Webhooks["ci"]
	mgr := newWiringManager(t, cfg)

	sources := startDaemonSources(mgr, nil)
	t.Cleanup(sources.Close)
	wireSourceReload(mgr, sources)
	webhooks := beginDaemonWebhooks(mgr, sources, "127.0.0.1:0")
	wireWebhookReload(mgr, webhooks)
	webhooks.serve()
	t.Cleanup(webhooks.shutdown)
	if webhooks.addr() == "" {
		t.Fatal("the daemon's webhook listener did not bind")
	}
	url := "http://" + webhooks.addr() + "/hooks/ci"

	// The right signature over a body one byte different fires nothing.
	altered := strings.Replace(ghWiringBody, "completed", "completeD", 1)
	if code := postGitHub(t, url, ghWiringSig, "gh-del-0", altered); code != http.StatusUnauthorized {
		t.Fatalf("altered body: %d, want 401", code)
	}
	if runs := mgr.Runs("ci-run"); len(runs) != 0 {
		t.Fatalf("a delivery that failed verification made a run: %+v", runs)
	}

	if code := postGitHub(t, url, ghWiringSig, "gh-del-1", ghWiringBody); code != http.StatusAccepted {
		t.Fatalf("signed delivery: %d, want 202", code)
	}
	var rec supervisor.RunRecord
	deadline := time.Now().Add(15 * time.Second)
	for {
		runs := mgr.Runs("ci-run")
		if len(runs) == 1 && runs[0].Outcome != supervisor.OutcomeRunning {
			rec = runs[0]
			break
		}
		if len(runs) > 1 {
			t.Fatalf("one delivery made %d runs", len(runs))
		}
		if time.Now().After(deadline) {
			t.Fatalf("the signed delivery never produced a finished run: %+v", runs)
		}
		time.Sleep(10 * time.Millisecond)
	}
	// event_id comes from X-GitHub-Delivery: the preset's delivery header,
	// filled in by the parser, not configured.
	if rec.Trigger != supervisor.TriggerWebhook || rec.Source != "webhook.ci" || rec.EventID != "gh-del-1" {
		t.Errorf("record = %+v, want trigger webhook, source webhook.ci, event_id gh-del-1", rec)
	}

	eventFile := strings.TrimSuffix(mgr.RunLogPath("ci-run", rec.RunID), ".log") + ".event.json"
	info, err := os.Stat(eventFile)
	if err != nil {
		t.Fatalf("no event file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("event file mode = %o, want 600", perm)
	}
	raw, err := os.ReadFile(eventFile)
	if err != nil {
		t.Fatal(err)
	}
	env, err := trigger.ParseEnvelope(raw, 0)
	if err != nil {
		t.Fatalf("event file does not parse: %v", err)
	}
	// The event file is indented for reading, which re-indents a JSON body;
	// compare the two compacted.
	var got, want bytes.Buffer
	if env.Webhook == nil || json.Compact(&got, env.Webhook.Body) != nil || json.Compact(&want, []byte(ghWiringBody)) != nil ||
		got.String() != want.String() || env.Webhook.Event != "workflow_run" {
		t.Errorf("event file = %s, want the signed body and event workflow_run", raw)
	}
	hex := strings.TrimPrefix(ghWiringSig, "sha256=")
	for _, leak := range []string{hex, "X-Hub-Signature-256", ghWiringSecret} {
		if strings.Contains(strings.ToLower(string(raw)), strings.ToLower(leak)) {
			t.Errorf("the event file carries %q:\n%s", leak, raw)
		}
	}
}
