package main

// --event's local pre-check (SPEC-0014 REQ "Manual Trigger With Event").
//
// @joestump 09/23/2026 - Added in review of #585: the pre-check capped
// at the 1 MiB default and refused replays the daemon would accept.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/trigger"
)

// TestReadEventArgDefersTheSizeCapToTheDaemon: a replay of a delivery bigger
// than 1 MiB is legitimate for a harness whose webhook source raised
// max_body, and only the daemon knows the harness's sources — so the CLI must
// not refuse it before dialling. A file over the ceiling no harness can have
// is still refused locally.
func TestReadEventArgDefersTheSizeCapToTheDaemon(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, bodyLen int) string {
		e := &trigger.Envelope{
			Version: trigger.EnvelopeVersion, Kind: trigger.KindWebhook,
			Source: "webhook.big", EventID: "d-1", ReceivedAt: time.Now().UTC(),
			Webhook: &trigger.WebhookEvent{BodyText: strings.Repeat("x", bodyLen)},
		}
		b, err := e.Encode()
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	big := write("big.event.json", 2<<20) // 2 MiB: over the default, under the ceiling
	b, err := readEventArg(big)
	if err != nil {
		t.Fatalf("a 2 MiB envelope was refused locally; only the daemon knows the harness's max_body: %v", err)
	}
	if len(b) <= int(trigger.DefaultMaxEventBytes) {
		t.Fatalf("control: the envelope is %d bytes, not over the 1 MiB default, so this proves nothing", len(b))
	}

	huge := write("huge.event.json", 26<<20) // over the 25 MiB ceiling
	if _, err := readEventArg(huge); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Errorf("an envelope over the max_body ceiling was not refused locally: %v", err)
	}
}
