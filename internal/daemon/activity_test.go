package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/client"
	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
	rt "gitea.stump.rocks/stump.wtf/harness/internal/runtrace/runtracetest"
	"gitea.stump.rocks/stump.wtf/harness/internal/supervisor"
)

// local builds a wall-clock time on 2026-09-11 in the daemon's zone, which is
// the zone its lifecycle lines are written in.
func local(h, m, s int) time.Time { return time.Date(2026, 9, 11, h, m, s, 0, time.Local) }

func lifecycleLine(t time.Time, rest string) string {
	return t.Format("2006/01/02 15:04:05") + " INFO " + rest + "\n"
}

// hermeticHome keeps discovery off the developer's real crush registry.
func hermeticHome(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("CRUSH_GLOBAL_DATA", "")
}

// sweepsDaemon is two scheduled-style crush harnesses sharing one workdir and
// one store — the tars ~/sweeps layout — plus a generic harness. Nothing is
// started: the runs exist only in the durable logs, which is exactly the
// post-mortem case (a harness that is no longer running, whose snapshot this
// daemon never saw).
func sweepsDaemon(t *testing.T) (*testDaemon, string) {
	t.Helper()
	hermeticHome(t)
	work := filepath.Join(t.TempDir(), "sweeps")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	td := newTestDaemon(t, fmt.Sprintf(`
[harness.sweep-pdx]
harness = "crush"
prompt = "sweep pdx"
auto_accept = true
workdir = %[1]q
restart = "no"

[harness.sweep-pr]
harness = "crush"
prompt = "sweep prs"
auto_accept = true
workdir = %[1]q
restart = "no"

[harness.ticker]
harness = "generic"
args = ["-c", "sleep 60"]
`, work))
	return td, work
}

func writeLog(t *testing.T, td *testDaemon, name, body string) {
	t.Helper()
	dir := td.mgr.LogDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".log"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func viewCall(id, path string, at time.Time) []rt.CrushMessage {
	return []rt.CrushMessage{
		{Role: "assistant", At: at, Parts: rt.ToolCall(id, "view", map[string]any{"file_path": path})},
		{Role: "tool", At: at, Parts: rt.ToolResult(id, "ok")},
	}
}

// TestLogsEventsPostMortem is `harness logs stumpcloud-sweep-pdx` the morning
// after: the run is over, the daemon's snapshot has no start for it, and the
// reply still reconstructs the run from the durable log and attributes only
// that run's session — not the sibling sweep's, from the same store.
func TestLogsEventsPostMortem(t *testing.T) {
	td, work := sweepsDaemon(t)
	rt.WriteCrushDB(t, filepath.Join(work, ".crush", "crush.db"),
		// The finish error shares the exit's second, as it did on tars: the
		// error is what ended the run.
		rt.CrushSession{ID: "e088ec4e-pdx", Created: local(7, 40, 2), Updated: local(7, 56, 21),
			Messages: append(viewCall("c1", filepath.Join(work, "pdx.yaml"), local(7, 40, 30)),
				rt.CrushMessage{Role: "assistant", At: local(7, 56, 21), Parts: rt.FinishError("Bad Request", "litellm.ContextWindowExceededError")})},
		rt.CrushSession{ID: "09a363b5-pr", Created: local(9, 30, 3), Updated: local(9, 50, 24),
			Messages: viewCall("c2", filepath.Join(work, "prs.md"), local(9, 31, 0))},
	)
	writeLog(t, td, "sweep-pdx",
		lifecycleLine(local(7, 40, 0), "state changed from=stopped to=starting")+
			lifecycleLine(local(7, 40, 0), "state changed from=starting to=running")+
			"Now reading pdx.yaml to get the enabled services for nuc01.\n"+
			lifecycleLine(local(7, 56, 21), "exited code=1")+
			lifecycleLine(local(7, 56, 21), "state changed from=running to=failed"))
	writeLog(t, td, "sweep-pr",
		lifecycleLine(local(9, 30, 0), "state changed from=stopped to=starting")+
			lifecycleLine(local(9, 50, 24), "exited code=0"))

	ld, err := td.dial(t, nil).LogEvents("sweep-pdx", client.LogOptions{})
	if err != nil {
		t.Fatalf("LogEvents: %v", err)
	}
	if ld.Source != protocol.LogSourceAgentTrace {
		t.Fatalf("source = %q, want %q (text: %q)", ld.Source, protocol.LogSourceAgentTrace, ld.Text)
	}
	if ld.Run == nil || ld.Run.ExitCode == nil || *ld.Run.ExitCode != 1 || !strings.HasPrefix(ld.Run.Start, local(7, 40, 0).Format("2006-01-02T15:04:05")) {
		t.Fatalf("run = %+v, want the 07:40 run that exited 1", ld.Run)
	}
	var got []string
	for _, e := range ld.Entries {
		if e.Kind == protocol.LogEntryMark {
			continue
		}
		got = append(got, e.Kind+":"+e.Action+":"+filepath.Base(e.Target)+":"+e.Summary)
	}
	want := []string{
		"lifecycle:state:.:stopped → starting",
		"lifecycle:state:.:starting → running",
		"session:session:.:e088ec4e · crush · test/model · sweeps — e088ec4e",
		"tool:read:pdx.yaml:" + ld.Entries[3].Summary,
		"lifecycle:exited:.:code=1",
		"lifecycle:state:.:running → failed",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("entries:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// The reason the run died is in the session, not the process output: crush
	// persists the provider error as a finish part (agent-trace#98).
	var died bool
	for i, e := range ld.Entries {
		if e.Kind == protocol.LogEntryMark && e.Action == "error" {
			died = e.Error && strings.Contains(e.Summary, "ContextWindowExceededError") &&
				i+1 < len(ld.Entries) && ld.Entries[i+1].Action == "exited"
		}
	}
	if !died {
		t.Errorf("entries = %+v, want the provider error as an error mark immediately before the exit", ld.Entries)
	}
	if len(ld.Excluded) != 0 || ld.Text != "" {
		t.Errorf("excluded = %+v, text = %q; want neither for an unambiguous run", ld.Excluded, ld.Text)
	}
	if !containsNotice(ld, "recovered from the durable log") {
		t.Errorf("notices = %q, want the window's provenance stated", ld.Notices)
	}
}

// TestLogsEventsExcludesOverlappingPeer: when two harnesses' runs overlap in
// one workdir, a session inside the overlap is shown for neither by default,
// the exclusion is reported, and --include-ambiguous shows it flagged.
func TestLogsEventsExcludesOverlappingPeer(t *testing.T) {
	td, work := sweepsDaemon(t)
	rt.WriteCrushDB(t, filepath.Join(work, ".crush", "crush.db"),
		rt.CrushSession{ID: "overlap", Created: local(8, 0, 5), Messages: viewCall("c1", "a.go", local(8, 0, 6))})
	writeLog(t, td, "sweep-pdx", lifecycleLine(local(8, 0, 0), "state changed from=stopped to=starting")+lifecycleLine(local(8, 30, 0), "exited code=0"))
	writeLog(t, td, "sweep-pr", lifecycleLine(local(7, 55, 0), "state changed from=stopped to=starting")+lifecycleLine(local(8, 20, 0), "exited code=0"))
	c := td.dial(t, nil)

	ld, err := c.LogEvents("sweep-pdx", client.LogOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ld.Entries {
		if e.Session == "overlap" {
			t.Fatalf("an ambiguous session was attributed: %+v", e)
		}
	}
	if len(ld.Excluded) != 1 || strings.Join(ld.Excluded[0].Claimants, ",") != "sweep-pdx,sweep-pr" {
		t.Errorf("excluded = %+v, want the overlap session naming both harnesses", ld.Excluded)
	}
	if !containsNotice(ld, "sweep-pr") || ld.Text == "" {
		t.Errorf("notices = %q, text = %q; want the exclusion explained and the log tail as fallback", ld.Notices, ld.Text)
	}

	ld, err = c.LogEvents("sweep-pdx", client.LogOptions{IncludeAmbiguous: true})
	if err != nil {
		t.Fatal(err)
	}
	var flagged bool
	for _, e := range ld.Entries {
		if e.Session == "overlap" {
			if !e.Ambiguous {
				t.Errorf("entry %+v shown without its ambiguous flag", e)
			}
			flagged = true
		}
	}
	if !flagged {
		t.Error("--include-ambiguous did not show the excluded session")
	}
}

// requireExcludedByPeer runs sweep-pdx in work alongside one peer (a TOML
// table, named peer) whose run overlaps pdx's, and requires the session inside
// the overlap to be excluded naming both.
func requireExcludedByPeer(t *testing.T, work, peer, peerTable string) {
	t.Helper()
	td := newTestDaemon(t, fmt.Sprintf(`
[harness.sweep-pdx]
harness = "crush"
prompt = "sweep pdx"
auto_accept = true
workdir = %q
restart = "no"

[harness.%s]
%s
`, work, peer, peerTable))
	rt.WriteCrushDB(t, filepath.Join(work, ".crush", "crush.db"),
		rt.CrushSession{ID: "overlap", Created: local(8, 0, 5), Messages: viewCall("c1", "a.go", local(8, 0, 6))})
	writeLog(t, td, "sweep-pdx", lifecycleLine(local(8, 0, 0), "state changed from=stopped to=starting")+lifecycleLine(local(8, 30, 0), "exited code=0"))
	writeLog(t, td, peer, lifecycleLine(local(7, 55, 0), "state changed from=stopped to=starting")+lifecycleLine(local(8, 20, 0), "exited code=0"))

	ld, err := td.dial(t, nil).LogEvents("sweep-pdx", client.LogOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ld.Entries {
		if e.Session == "overlap" {
			t.Fatalf("a session %s could have written was attributed to sweep-pdx: %+v", peer, e)
		}
	}
	if len(ld.Excluded) != 1 || strings.Join(ld.Excluded[0].Claimants, ",") != "sweep-pdx,"+peer {
		t.Errorf("excluded = %+v, want the overlap session naming sweep-pdx and %s", ld.Excluded, peer)
	}
}

// TestLogsEventsPeerThroughSymlinkIsAClaimant: a peer whose workdir is a
// symlink to the target's writes the same store, and the project-store source
// stamps its sessions with the target's spelling — so it must still count.
func TestLogsEventsPeerThroughSymlinkIsAClaimant(t *testing.T) {
	hermeticHome(t)
	root := t.TempDir()
	work, alias := filepath.Join(root, "sweeps"), filepath.Join(root, "sweeps-link")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(work, alias); err != nil {
		t.Fatal(err)
	}
	requireExcludedByPeer(t, work, "sweep-alias", fmt.Sprintf("harness = \"crush\"\nprompt = \"alias\"\nauto_accept = true\nworkdir = %q\nrestart = \"no\"", alias))
}

// TestLogsEventsPeerWithoutWorkdirIsAClaimant: a harness with no workdir is
// spawned in the daemon's own directory; when that is the target's workdir the
// two share a store.
func TestLogsEventsPeerWithoutWorkdirIsAClaimant(t *testing.T) {
	hermeticHome(t)
	work := filepath.Join(t.TempDir(), "sweeps")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(work)
	requireExcludedByPeer(t, work, "bare", "harness = \"crush\"\nprompt = \"bare\"\nauto_accept = true\nrestart = \"no\"")
}

// TestLogsEventsGenericFallsBackToText: a harness with no native transcript
// answers with its durable log, unmarked, so the client prints it as before.
func TestLogsEventsGenericFallsBackToText(t *testing.T) {
	td, _ := sweepsDaemon(t)
	writeLog(t, td, "ticker", "tick 1\ntick 2\n")
	ld, err := td.dial(t, nil).LogEvents("ticker", client.LogOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if ld.Source != "" || ld.Text != "tick 1\ntick 2\n" || len(ld.Entries) != 0 {
		t.Errorf("reply = %+v, want the raw tail with no source", ld)
	}
}

// TestLogsRawIsUnchanged: without Events the op is byte-for-byte what it was,
// which is what the peek pane and --raw depend on.
func TestLogsRawIsUnchanged(t *testing.T) {
	td, _ := sweepsDaemon(t)
	body := lifecycleLine(local(7, 40, 0), "state changed from=stopped to=starting") + "\x1b[2Jrepaint\n"
	writeLog(t, td, "sweep-pdx", body)
	ld, err := td.dial(t, nil).Logs("sweep-pdx", 10)
	if err != nil {
		t.Fatal(err)
	}
	if ld.Text != body || ld.Source != "" || ld.Entries != nil {
		t.Errorf("raw reply = %+v, want exactly the file", ld)
	}
}

func TestListCarriesRunBounds(t *testing.T) {
	td, _ := sweepsDaemon(t)
	c := td.dial(t, nil)
	if _, err := c.Start("ticker"); err != nil {
		t.Fatal(err)
	}
	hs, err := c.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hs {
		if h.Name != "ticker" {
			continue
		}
		if _, err := time.Parse(time.RFC3339Nano, h.LastStarted); err != nil {
			t.Errorf("ticker LastStarted = %q, want an RFC 3339 start", h.LastStarted)
		}
		return
	}
	t.Fatal("ticker missing from list")
}

func TestRunWindowPrecedence(t *testing.T) {
	started, exited := local(7, 40, 0).Add(106*time.Millisecond), local(7, 56, 21)
	code := 1
	spans := []supervisor.RunSpan{{Start: local(7, 40, 0), End: local(7, 56, 22), ExitCode: &code}}

	w, _, _, err := runWindow(protocol.ControlReq{Since: local(1, 0, 0).Format(time.RFC3339Nano), Until: local(2, 0, 0).Format(time.RFC3339Nano)},
		supervisor.Snapshot{LastStarted: started, LastExitAt: exited}, spans, time.Time{})
	if err != nil || !w.Start.Equal(local(1, 0, 0)) || !w.End.Equal(local(2, 0, 0)) {
		t.Errorf("explicit window = %+v, %v; want 01:00–02:00", w, err)
	}

	w, exit, _, _ := runWindow(protocol.ControlReq{}, supervisor.Snapshot{LastStarted: started, LastExitAt: exited, LastExitCode: 1}, spans, time.Time{})
	if !w.Start.Equal(started) || !w.End.Equal(exited) || exit == nil || *exit != 1 {
		t.Errorf("snapshot window = %+v exit %v; want the supervisor's own record", w, exit)
	}

	w, exit, _, _ = runWindow(protocol.ControlReq{}, supervisor.Snapshot{LastStarted: started, PID: 42}, spans, time.Time{})
	if !w.Open() || exit != nil {
		t.Errorf("running window = %+v; want it open", w)
	}

	// Not running, no exit after the start: the daemon died with the run. The
	// log's matching span closes it.
	w, exit, _, _ = runWindow(protocol.ControlReq{}, supervisor.Snapshot{LastStarted: started, LastExitAt: started.Add(-time.Hour)}, spans, time.Time{})
	if !w.End.Equal(local(7, 56, 22)) || exit == nil {
		t.Errorf("recovered end = %+v exit %v; want the span's end", w, exit)
	}

	w, _, note, _ := runWindow(protocol.ControlReq{}, supervisor.Snapshot{}, spans, time.Time{})
	if !w.Start.Equal(local(7, 40, 0)) || note == "" {
		t.Errorf("log-derived window = %+v note %q; want the last span, with its provenance noted", w, note)
	}

	// A dead run with no span to close it began before this daemon did, and a
	// harness does not outlive the daemon that spawned it: the window closes at
	// the daemon's start instead of vouching for every session since.
	boot := started.Add(2 * time.Hour)
	w, _, note, _ = runWindow(protocol.ControlReq{}, supervisor.Snapshot{LastStarted: started, LastExitAt: started.Add(-time.Hour)}, nil, boot)
	if !w.End.Equal(boot) || note == "" {
		t.Errorf("dead run = %+v note %q; want it closed at the daemon's start, noted", w, note)
	}
	w, _, _, _ = runWindow(protocol.ControlReq{}, supervisor.Snapshot{}, []supervisor.RunSpan{{Start: local(7, 40, 0)}}, boot)
	if !w.End.Equal(boot) {
		t.Errorf("log-derived dead run = %+v; want it closed at the daemon's start", w)
	}
	w, _, _, _ = runWindow(protocol.ControlReq{}, supervisor.Snapshot{LastStarted: started, LastExitAt: started.Add(-time.Hour)}, nil, started.Add(-time.Minute))
	if !w.Open() {
		t.Errorf("run begun under this daemon = %+v; want it left open", w)
	}

	if _, _, _, err := runWindow(protocol.ControlReq{Since: "yesterday"}, supervisor.Snapshot{}, nil, time.Time{}); !errors.Is(err, errBadWindow) {
		t.Errorf("bad since: err = %v, want errBadWindow", err)
	}
}

func containsNotice(ld protocol.LogsData, sub string) bool {
	for _, n := range ld.Notices {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}
