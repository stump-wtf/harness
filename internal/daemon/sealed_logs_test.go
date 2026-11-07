package daemon

// Sealed Log Reader Tests
//
// A closed run's log is compressed to <id>.log.zst in the background (ADR-0007
// as amended). Every reader the daemon serves a run log from must not notice:
// `harness logs --run`, raw and events alike, and the runs op's has_log and
// log_pruned. These read a run log in each form it can be in on disk — plain
// while open, compressed once sealed, and both at once after a crash between
// the rename and the removal.
//
// Governing: ADR-0007 (as amended for sealed compression); SPEC-0008 REQ
// "Per-Run Logs"; SPEC-0022 REQ-12; GitHub
// https://github.com/stump-wtf/harness/issues/18.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stump-wtf/harness/internal/client"
	"github.com/stump-wtf/harness/internal/protocol"
	"github.com/stump-wtf/harness/internal/sealedlog"
	"github.com/stump-wtf/harness/internal/supervisor"
)

// TestRunLogServedAfterCompression drives the whole path over the wire: a run
// closes, its log is sealed and compressed, and the daemon still serves it,
// still says it has one, and does not call it pruned.
func TestRunLogServedAfterCompression(t *testing.T) {
	dir := t.TempDir()
	td, _, _ := newJobsDaemonWith(t, func(o *supervisor.ManagerOptions) { o.CompressLogs = true },
		scheduledSh("nightly", "echo from-a-compressed-run; exit 0", dir))
	c := td.dial(t, nil)
	if _, err := c.Trigger("nightly"); err != nil {
		t.Fatal(err)
	}
	waitRunsOver(t, c, "nightly", finishedN(1))
	td.mgr.WaitSealed()

	path := td.mgr.RunLogPath("nightly", 1)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("plain run log still on disk after the run closed (err %v)", err)
	}
	if _, err := os.Stat(sealedlog.Compressed(path)); err != nil {
		t.Fatalf("compressed run log missing: %v", err)
	}

	rd, err := c.Runs("nightly", 0)
	if err != nil {
		t.Fatal(err)
	}
	if r := rd.Runs[0]; !r.HasLog || r.LogPruned {
		t.Errorf("runs reports has_log %v, log_pruned %v for a compressed log; want true, false", r.HasLog, r.LogPruned)
	}
	raw, err := c.RunLogs("nightly", 1, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw.Text, "from-a-compressed-run") || len(raw.Notices) > 0 {
		t.Errorf("raw --run 1 = %q, notices %v", raw.Text, raw.Notices)
	}
	events, err := c.LogEvents("nightly", client.LogOptions{Run: 1, Lines: 100})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(events.Text, "from-a-compressed-run") {
		t.Errorf("events --run 1 text:\n%s", events.Text)
	}
}

// TestPipeRunStreamServedAfterCompression: a pipe run's stream seals to
// .stream.jsonl.zst beside its log (ADR-0033), and the readable rendering
// reads through the seal — raw shows the stream's entries, events carries
// them as entries — where a bare os.Open would read nothing at all.
func TestPipeRunStreamServedAfterCompression(t *testing.T) {
	dir := t.TempDir()
	const sid = "abcdef12-3456-7890-abcd-ef1234567890"
	fakeClaude(t, `echo '{"type":"system","subtype":"init","session_id":"`+sid+`"}'
echo '{"type":"result","subtype":"success","is_error":false,"num_turns":1,"result":"readable through the seal","session_id":"`+sid+`"}'
`)
	td, _, _ := newJobsDaemonWith(t, func(o *supervisor.ManagerOptions) { o.CompressLogs = true },
		pipeOneShot("sweep", dir))
	c := td.dial(t, nil)
	if _, err := c.Trigger("sweep"); err != nil {
		t.Fatal(err)
	}
	waitRunsOver(t, c, "sweep", finishedN(1))
	td.mgr.WaitSealed()

	stream := supervisor.StreamPathFor(td.mgr.RunLogPath("sweep", 1))
	if _, err := os.Stat(stream); !os.IsNotExist(err) {
		t.Fatalf("plain stream still on disk after the run closed (err %v)", err)
	}
	if _, err := os.Stat(sealedlog.Compressed(stream)); err != nil {
		t.Fatalf("compressed stream missing: %v", err)
	}

	raw, err := c.RunLogs("sweep", 1, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw.Text, "abcdef12") || !strings.Contains(raw.Text, "readable through the seal") {
		t.Fatalf("the sealed stream's entries do not render:\n%s", raw.Text)
	}
	if strings.Contains(raw.Text, `{"type":"`) {
		t.Errorf("the sealed stream renders as raw JSON instead of entries:\n%s", raw.Text)
	}
	events, err := c.LogEvents("sweep", client.LogOptions{Run: 1, Lines: 100})
	if err != nil {
		t.Fatal(err)
	}
	var sess, result bool
	for _, e := range events.Entries {
		if e.Kind == protocol.LogEntrySession {
			sess = true
		}
		if strings.Contains(e.Summary, "readable through the seal") {
			result = true
		}
	}
	if !sess || !result {
		t.Errorf("the events view lost the sealed stream's entries (session %v, result %v): %+v", sess, result, events.Entries)
	}
}

// TestReadRunLogTailEitherForm: the tail, and its masking, are the same in
// every form a run log can be in.
func TestReadRunLogTailEitherForm(t *testing.T) {
	remote := logCredFixtures.Replace("<PW_REMOTE>")
	var body strings.Builder
	for i := range 50 {
		body.WriteString("line ")
		body.WriteString(strings.Repeat("x", i))
		body.WriteString("\n")
	}
	body.WriteString("git remote set-url origin " + remote + "\n")
	body.WriteString("last line\n")
	want := redactTail(tailLines([]byte(body.String()), 3))

	for _, form := range []string{"plain", "compressed", "both"} {
		t.Run(form, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "1.log")
			if err := os.WriteFile(path, []byte(body.String()), 0o600); err != nil {
				t.Fatal(err)
			}
			if form != "plain" {
				if err := sealedlog.CompressFile(path); err != nil {
					t.Fatal(err)
				}
			}
			if form == "both" {
				if err := os.WriteFile(path, []byte(body.String()), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got, ok := readRunLogTail(path, 3)
			if !ok || got != want {
				t.Errorf("readRunLogTail = %q, %v; want %q", got, ok, want)
			}
			if strings.Contains(got, logPwSecret) {
				t.Errorf("tail leaked a credential: %q", got)
			}
		})
	}
	if _, ok := readRunLogTail(filepath.Join(t.TempDir(), "9.log"), 3); ok {
		t.Error("readRunLogTail reported a log that exists in neither form")
	}
}
