package main

// Webhook filtering and limits, followed through the daemon's own wiring to
// the run history: an ignored event, a redelivery and a rate-limited delivery
// each leave the history exactly as it was, and the firings around them each
// add one record. The listener-level tests prove the decisions; this proves
// the daemon's source manager and supervisor never hear about the refused
// ones (#315).
//
// Governing: SPEC-0014 REQ "Webhook Filtering", REQ "Webhook Rate Limit".

import (
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/supervisor"
)

func TestDaemonWiringRefusedDeliveriesMakeNoRecord(t *testing.T) {
	cfg := webhookWiringConfig(filepath.Join(t.TempDir(), "fields"), true)
	src := cfg.Webhooks["ci"]
	src.EventHeader, src.Events = "X-Event", []string{"push"}
	src.RateLimit = core.RateLimit{Count: 2, Window: time.Hour}
	cfg.Webhooks["ci"] = src
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

	post := func(event, delivery string) (*http.Response, string) {
		t.Helper()
		req, err := http.NewRequest("POST", url, strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+wiringToken)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Event", event)
		req.Header.Set("X-Delivery", delivery)
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	decision := func(body string) string {
		var v struct{ Decision string }
		_ = json.Unmarshal([]byte(body), &v)
		return v.Decision
	}
	// waitRuns waits for n finished runs, then checks there are no more.
	waitRuns := func(n int) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for {
			runs := mgr.Runs("ci-run")
			done := 0
			for _, r := range runs {
				if r.Outcome != supervisor.OutcomeRunning {
					done++
				}
			}
			if len(runs) == n && done == n {
				return
			}
			if len(runs) > n || time.Now().After(deadline) {
				t.Fatalf("run history = %+v, want %d finished runs", runs, n)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	// noNewRun checks, right after a refused delivery's response, that the
	// history did not grow. The 202 of a firing returns only once each
	// harness has decided, so a record it made would already be there.
	noNewRun := func(what string, n int) {
		t.Helper()
		if runs := mgr.Runs("ci-run"); len(runs) != n {
			t.Fatalf("%s made a run record: %+v", what, runs)
		}
	}

	if resp, body := post("push", "del-1"); resp.StatusCode != http.StatusAccepted || decision(body) != "fired" {
		t.Fatalf("first delivery: %d %s", resp.StatusCode, body)
	}
	waitRuns(1)

	if resp, body := post("push", "del-1"); resp.StatusCode != http.StatusAccepted || decision(body) != "duplicate" {
		t.Errorf("redelivery: %d %s, want 202 duplicate", resp.StatusCode, body)
	}
	noNewRun("a redelivery", 1)

	if resp, body := post("ping", "del-ping"); resp.StatusCode != http.StatusAccepted || decision(body) != "ignored" {
		t.Errorf("unlisted event: %d %s, want 202 ignored", resp.StatusCode, body)
	}
	noNewRun("an ignored event", 1)

	if resp, body := post("push", "del-2"); resp.StatusCode != http.StatusAccepted || decision(body) != "fired" {
		t.Fatalf("second delivery: %d %s", resp.StatusCode, body)
	}
	waitRuns(2)

	resp, body := post("push", "del-3")
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" {
		t.Errorf("third firing inside 2/h: %d Retry-After %q %s, want 429 with Retry-After", resp.StatusCode, resp.Header.Get("Retry-After"), body)
	}
	noNewRun("a rate-limited delivery", 2)
}
