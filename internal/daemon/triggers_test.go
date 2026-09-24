package daemon

// Trigger visibility over the protocol, from the daemon's side: jobs lists a
// harness only its sources fire, job_run_* says which source fired a run, and
// a client at the previous minor still lists harnesses. The source manager's
// half — states, counters, trigger_source_changed — is driven through the
// daemon's own wiring in cmd/harness/triggers_wiring_test.go.
//
// Governing: ADR-0021; SPEC-0014 REQ "Trigger Visibility"; SPEC-0002 REQ
// "Handshake And Versioning".
//
// @joestump 09/24/2026 - Introduced for stump.wtf/harness#476.

import (
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/protocol"
)

// TestJobsListsAnEventOnlyHarness: jobs includes every TRIGGERED harness, and
// one with no schedule carries its triggers and no next window.
func TestJobsListsAnEventOnlyHarness(t *testing.T) {
	dir := t.TempDir()
	td, _, _ := newJobsDaemon(t,
		scheduledSh("nightly", "exit 0", dir),
		triggeredSh("pr-review", "exit 0", dir, "webhook.gitea-pr", "channel.sb"),
	)
	c := td.dial(t, nil)

	jobs, err := c.Jobs()
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]protocol.JobInfo{}
	for _, j := range jobs {
		by[j.Name] = j
	}
	pr, ok := by["pr-review"]
	if !ok {
		t.Fatalf("jobs = %+v, want the event-only harness listed", jobs)
	}
	if pr.Schedule != "" || pr.NextRun != "" {
		t.Errorf("pr-review schedule=%q next=%q, want neither", pr.Schedule, pr.NextRun)
	}
	if len(pr.Triggers) != 2 || pr.Triggers[0].Source != "webhook.gitea-pr" || pr.Triggers[1].Source != "channel.sb" {
		t.Errorf("pr-review triggers = %+v, want both sources in config order", pr.Triggers)
	}
	// The control: the scheduled one still has its window, so the absence
	// above is about the harness, not a scheduler that reports nothing.
	if n := by["nightly"]; n.NextRun == "" || len(n.Triggers) != 0 {
		t.Errorf("nightly = %+v, want a next window and no triggers", n)
	}

	// The harness projection carries the same triggers.
	info, err := c.Describe("pr-review")
	if err != nil {
		t.Fatal(err)
	}
	if got := protocol.TriggerRefs(info.Triggers); len(got) != 2 || got[0] != "webhook.gitea-pr" {
		t.Errorf("describe triggers = %v", got)
	}
}

// TestJobRunEventsCarryTheirSource: a run fired with an event says which
// source it came from on job_run_started and job_run_finished.
func TestJobRunEventsCarryTheirSource(t *testing.T) {
	dir := t.TempDir()
	td, _, _ := newJobsDaemon(t, triggeredSh("pr-review", "exit 0", dir, "webhook.gitea-pr"))
	sub := td.dial(t, []string{"events"})
	ctl := td.dial(t, nil)

	if _, err := ctl.TriggerWithEvent("pr-review", envelopeBytes(t, "webhook.gitea-pr", "del-9")); err != nil {
		t.Fatal(err)
	}
	pc := sub.Conn()
	_ = sub.SetReadDeadline(time.Now().Add(10 * time.Second))
	seen := map[protocol.EventKind]protocol.EventMsg{}
	for len(seen) < 2 {
		f, err := pc.ReadFrame()
		if err != nil {
			t.Fatalf("waiting for job_run_*: %v (seen %+v)", err, seen)
		}
		switch f.Type {
		case protocol.TypeEvent:
			ev := decodeEvent(t, f.Payload)
			if ev.Name == "pr-review" && (ev.Kind == protocol.EvJobRunStarted || ev.Kind == protocol.EvJobRunFinished) {
				seen[ev.Kind] = ev
			}
		case protocol.TypePing:
			_ = pc.WriteFrame(protocol.TypePong, nil)
		}
	}
	for kind, ev := range seen {
		if ev.Source != "webhook.gitea-pr" {
			t.Errorf("%s source = %q, want webhook.gitea-pr", kind, ev.Source)
		}
	}
}

// TestPreviousMinorClientStillLists: a client that says it speaks the
// previous minor still gets every harness from list, triggered ones included,
// and can decode the reply with only the fields it knew.
func TestPreviousMinorClientStillLists(t *testing.T) {
	dir := t.TempDir()
	td, _, _ := newJobsDaemon(t,
		triggeredSh("pr-review", "exit 0", dir, "webhook.gitea-pr"),
		scheduledSh("nightly", "exit 0", dir),
	)
	raw, err := net.Dial("unix", td.socket)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(5 * time.Second))
	pc := protocol.NewConn(raw)
	previous := "1.11"
	if protocol.ProtoVersion == previous {
		t.Fatalf("ProtoVersion is still %s: this test pins the bump", previous)
	}
	if err := pc.WriteJSON(protocol.TypeHello, &protocol.Hello{ProtoVersion: previous, ClientVersion: "old"}); err != nil {
		t.Fatal(err)
	}
	if f, err := pc.ReadFrame(); err != nil || f.Type != protocol.TypeHello {
		t.Fatalf("hello reply = %v, %v", f.Type, err)
	}
	if err := pc.WriteJSON(protocol.TypeControlReq, &protocol.ControlReq{ID: 1, Op: protocol.OpList}); err != nil {
		t.Fatal(err)
	}
	for {
		f, err := pc.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		if f.Type == protocol.TypePing {
			_ = pc.WriteFrame(protocol.TypePong, nil)
			continue
		}
		if f.Type != protocol.TypeControlResp {
			t.Fatalf("list answered %s", f.Type)
		}
		var resp protocol.ControlResp
		if err := json.Unmarshal(f.Payload, &resp); err != nil {
			t.Fatal(err)
		}
		// The shape an 1.11 client decodes into: no Triggers field.
		var old []struct {
			Name     string `json:"name"`
			State    string `json:"state"`
			Schedule string `json:"schedule"`
		}
		if err := json.Unmarshal(resp.Data, &old); err != nil {
			t.Fatalf("a 1.11-shaped client cannot decode list: %v", err)
		}
		if len(old) != 2 || old[0].Name != "pr-review" || old[1].Name != "nightly" {
			t.Fatalf("list to a 1.11 client = %+v, want both harnesses", old)
		}
		return
	}
}
