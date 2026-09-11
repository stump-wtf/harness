package runtrace

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/agent-trace/tail"

	"gitea.stump.rocks/stump.wtf/harness/internal/redact"
	rt "gitea.stump.rocks/stump.wtf/harness/internal/runtrace/runtracetest"
)

// spawn is when the run under test began: nanosecond precision, the way the
// supervisor records it, a tenth of a second into the second crush truncates to.
var spawn = time.Date(2026, 9, 11, 11, 40, 0, 106_000_000, time.UTC)

// read is a view tool call and its result, the smallest thing crush records
// that classifies to a target.
func read(id, path string, at time.Time) []rt.CrushMessage {
	return []rt.CrushMessage{
		{Role: "assistant", At: at, Parts: rt.ToolCall(id, "view", map[string]any{"file_path": path})},
		{Role: "tool", At: at, Parts: rt.ToolResult(id, "contents")},
	}
}

func sess(id string, created time.Time, msgs ...[]rt.CrushMessage) rt.CrushSession {
	s := rt.CrushSession{ID: id, Created: created, Updated: created.Add(time.Minute)}
	for _, m := range msgs {
		s.Messages = append(s.Messages, m...)
	}
	return s
}

// crushHarness scopes a crush harness to a hermetic HOME, so no test reads the
// real ~/.local/share/crush registry of whoever runs it.
func crushHarness(t *testing.T, name, workdir string, env map[string]string, runs ...Window) Scope {
	t.Helper()
	e := map[string]string{"HOME": filepath.Join(t.TempDir(), "home")}
	for k, v := range env {
		e[k] = v
	}
	return Scope{Name: name, Adapter: "crush", Workdir: workdir, Env: e, Runs: runs}
}

func ids(sessions []Session) []string {
	out := []string{}
	for _, s := range sessions {
		out = append(out, s.Meta.ID)
	}
	return out
}

func attribute(t *testing.T, target Scope, w Window, peers ...Scope) Attribution {
	t.Helper()
	a, err := Attribute(context.Background(), target, w, peers, spawn.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("Attribute: %v", err)
	}
	return a
}

// TestAttributeByWorkdirAndWindow is issue #89's core acceptance: a session in
// the workdir started during the run is attributed; before the run, after it,
// or in another directory is not — including a directory the harness's own
// registry knows about.
func TestAttributeByWorkdirAndWindow(t *testing.T) {
	root := t.TempDir()
	work, other := filepath.Join(root, "sweeps"), filepath.Join(root, "src")
	rt.WriteCrushDB(t, filepath.Join(work, ".crush", "crush.db"),
		sess("before", spawn.Add(-time.Hour), read("c0", "a.go", spawn.Add(-time.Hour))),
		// Stored as 11:40:00, a tenth of a second BEFORE spawn: the truncation
		// Slack exists for.
		sess("during", spawn, read("c1", "b.go", spawn.Add(5*time.Second))),
		sess("after", spawn.Add(20*time.Minute), read("c2", "c.go", spawn.Add(20*time.Minute))),
	)
	rt.WriteCrushDB(t, filepath.Join(other, ".crush", "crush.db"),
		sess("elsewhere", spawn.Add(time.Minute), read("c3", "d.go", spawn.Add(time.Minute))))

	h := crushHarness(t, "stumpcloud-sweep-pdx", work, nil)
	rt.WriteProjects(t, filepath.Join(h.Env["HOME"], ".local", "share", "crush", "projects.json"), "",
		rt.Project{Path: other, DataDir: filepath.Join(other, ".crush"), LastAccessed: spawn})

	a := attribute(t, h, Window{Start: spawn, End: spawn.Add(16 * time.Minute)})
	if got := ids(a.Sessions); !reflect.DeepEqual(got, []string{"during"}) {
		t.Errorf("attributed = %v, want [during]", got)
	}
	if len(a.Excluded) != 0 {
		t.Errorf("excluded = %+v, want none", a.Excluded)
	}
}

// TestAttributeExcludesOverlappingPeers is the tars crush-signal /
// crush-switchboard shape: two long-running crush harnesses in one workdir with
// different CRUSH_GLOBAL_DATA, whose registries both point at the same
// <workdir>/.crush store. Neither may claim the other's session.
func TestAttributeExcludesOverlappingPeers(t *testing.T) {
	work := filepath.Join(t.TempDir(), "src")
	store := filepath.Join(work, ".crush")
	rt.WriteCrushDB(t, filepath.Join(store, "crush.db"),
		sess("shared", spawn.Add(2*time.Hour), read("c1", "x.go", spawn.Add(2*time.Hour))))

	signal := crushHarness(t, "crush-signal", work, map[string]string{"CRUSH_GLOBAL_DATA": filepath.Join(t.TempDir(), "crush-signal")}, Window{Start: spawn})
	switchboard := crushHarness(t, "crush-switchboard", work, map[string]string{"CRUSH_GLOBAL_DATA": filepath.Join(t.TempDir(), "crush-switchboard")}, Window{Start: spawn})
	for _, s := range []Scope{signal, switchboard} {
		rt.WriteProjects(t, filepath.Join(s.Env["CRUSH_GLOBAL_DATA"], "projects.json"), "",
			rt.Project{Path: work, DataDir: store, LastAccessed: spawn})
	}

	for _, tc := range []struct {
		target, peer Scope
	}{{signal, switchboard}, {switchboard, signal}} {
		a := attribute(t, tc.target, Window{Start: spawn}, tc.peer)
		if len(a.Sessions) != 0 {
			t.Errorf("%s attributed %v, want nothing: the session is %s's as much as its own", tc.target.Name, ids(a.Sessions), tc.peer.Name)
		}
		if len(a.Excluded) != 1 || !reflect.DeepEqual(a.Excluded[0].Claimants, []string{tc.target.Name, tc.peer.Name}) {
			t.Errorf("%s excluded = %+v, want the one session naming both claimants", tc.target.Name, a.Excluded)
		}
	}
}

// TestAttributeStaggeredRunsShareOneStore is the tars sweep shape: every
// scheduled sweep runs in ~/sweeps and writes one store, and only the window
// separates them.
func TestAttributeStaggeredRunsShareOneStore(t *testing.T) {
	work := filepath.Join(t.TempDir(), "sweeps")
	pdxRun := Window{Start: spawn, End: spawn.Add(16*time.Minute + 21*time.Second)}
	prRun := Window{Start: spawn.Add(110 * time.Minute), End: spawn.Add(130 * time.Minute)}
	rt.WriteCrushDB(t, filepath.Join(work, ".crush", "crush.db"),
		sess("e088ec4e", spawn.Add(2*time.Second), read("c1", "pdx.yaml", spawn.Add(10*time.Second))),
		sess("09a363b5", prRun.Start.Add(3*time.Second), read("c2", "pr.md", prRun.Start.Add(9*time.Second))),
	)
	pdx := crushHarness(t, "stumpcloud-sweep-pdx", work, nil, pdxRun)
	pr := crushHarness(t, "pr-sweep", work, nil, prRun)

	if got := ids(attribute(t, pdx, pdxRun, pr).Sessions); !reflect.DeepEqual(got, []string{"e088ec4e"}) {
		t.Errorf("pdx attributed %v, want [e088ec4e]", got)
	}
	if got := ids(attribute(t, pr, prRun, pdx).Sessions); !reflect.DeepEqual(got, []string{"09a363b5"}) {
		t.Errorf("pr-sweep attributed %v, want [09a363b5]", got)
	}
}

// TestAttributeFollowsCrushGlobalData: a harness whose crush data_directory is
// relocated is only discoverable through ITS registry, under its own
// CRUSH_GLOBAL_DATA — and the default registry, which describes a different
// instance, is not consulted.
func TestAttributeFollowsCrushGlobalData(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "work")
	mine, theirs := filepath.Join(root, "data-mine"), filepath.Join(root, "data-theirs")
	rt.WriteCrushDB(t, filepath.Join(mine, "crush.db"), sess("mine", spawn.Add(time.Second), read("c1", "m.go", spawn.Add(time.Second))))
	rt.WriteCrushDB(t, filepath.Join(theirs, "crush.db"), sess("theirs", spawn.Add(time.Second), read("c2", "t.go", spawn.Add(time.Second))))

	h := crushHarness(t, "relocated", work, map[string]string{"CRUSH_GLOBAL_DATA": filepath.Join(root, "global-mine")})
	rt.WriteProjects(t, filepath.Join(root, "global-mine", "projects.json"), "", rt.Project{Path: work, DataDir: mine, LastAccessed: spawn})
	rt.WriteProjects(t, filepath.Join(h.Env["HOME"], ".local", "share", "crush", "projects.json"), "", rt.Project{Path: work, DataDir: theirs, LastAccessed: spawn})

	if got := ids(attribute(t, h, Window{Start: spawn}).Sessions); !reflect.DeepEqual(got, []string{"mine"}) {
		t.Errorf("attributed %v, want [mine]", got)
	}
}

// TestAttributeSurvivesCorruptRegistry: the registry on tars was a valid
// document followed by a stray "}" and had not been rewritten in weeks.
// Discovery must not depend on it.
func TestAttributeSurvivesCorruptRegistry(t *testing.T) {
	work := filepath.Join(t.TempDir(), "sweeps")
	rt.WriteCrushDB(t, filepath.Join(work, ".crush", "crush.db"), sess("found", spawn.Add(time.Second), read("c1", "a.go", spawn.Add(time.Second))))
	h := crushHarness(t, "sweep", work, nil)
	rt.WriteProjects(t, filepath.Join(h.Env["HOME"], ".local", "share", "crush", "projects.json"), "}",
		rt.Project{Path: "/somewhere/else", DataDir: "/somewhere/else/.crush", LastAccessed: spawn.Add(-500 * time.Hour)})

	if got := ids(attribute(t, h, Window{Start: spawn}).Sessions); !reflect.DeepEqual(got, []string{"found"}) {
		t.Errorf("attributed %v, want [found]", got)
	}
}

// TestAttributeGenericPeerIsAClaimant: a generic harness runs an arbitrary
// command, which may be crush itself, so sharing the workdir during the run is
// enough to make a session ambiguous.
func TestAttributeGenericPeerIsAClaimant(t *testing.T) {
	work := filepath.Join(t.TempDir(), "w")
	rt.WriteCrushDB(t, filepath.Join(work, ".crush", "crush.db"), sess("s", spawn.Add(time.Second), read("c1", "a.go", spawn.Add(time.Second))))
	h := crushHarness(t, "agent", work, nil)
	wrapper := Scope{Name: "wrapper", Adapter: "generic", Workdir: work, Runs: []Window{{Start: spawn.Add(-time.Minute)}}}
	otherKind := Scope{Name: "claude", Adapter: "claude-code", Workdir: work, Runs: []Window{{Start: spawn.Add(-time.Minute)}}}

	a := attribute(t, h, Window{Start: spawn}, wrapper, otherKind)
	if len(a.Sessions) != 0 || len(a.Excluded) != 1 || !reflect.DeepEqual(a.Excluded[0].Claimants, []string{"agent", "wrapper"}) {
		t.Errorf("attribution = %+v, want the session excluded with claimants [agent wrapper] (a claude-code harness cannot write crush sessions)", a)
	}
}

// TestAttributeExplicitWindowSelectsAnEarlierRun is the seam `logs --run N`
// (#120) plugs into: the window is the caller's, not "the latest run".
func TestAttributeExplicitWindowSelectsAnEarlierRun(t *testing.T) {
	work := filepath.Join(t.TempDir(), "sweeps")
	yesterday := Window{Start: spawn.Add(-24 * time.Hour), End: spawn.Add(-24*time.Hour + 10*time.Minute)}
	today := Window{Start: spawn, End: spawn.Add(10 * time.Minute)}
	rt.WriteCrushDB(t, filepath.Join(work, ".crush", "crush.db"),
		sess("yesterday", yesterday.Start.Add(2*time.Second), read("c1", "a.go", yesterday.Start.Add(3*time.Second))),
		sess("today", today.Start.Add(2*time.Second), read("c2", "b.go", today.Start.Add(3*time.Second))),
	)
	h := crushHarness(t, "sweep", work, nil, yesterday, today)
	if got := ids(attribute(t, h, yesterday).Sessions); !reflect.DeepEqual(got, []string{"yesterday"}) {
		t.Errorf("attributed %v, want [yesterday]", got)
	}
}

func TestAttributeRefusesWhatItCannotCorrelate(t *testing.T) {
	if _, err := Attribute(context.Background(), Scope{Name: "ticker", Adapter: "generic", Workdir: "/w"}, Window{Start: spawn}, nil, spawn); !errors.Is(err, ErrNoTrajectory) {
		t.Errorf("generic: err = %v, want ErrNoTrajectory", err)
	}
	if _, err := Attribute(context.Background(), crushHarness(t, "nowhere", "", nil), Window{Start: spawn}, nil, spawn); !errors.Is(err, ErrNoWorkdir) {
		t.Errorf("no workdir: err = %v, want ErrNoWorkdir", err)
	}
}

// TestEventsStayInsideTheWindow: a session resumed in a later run carries
// events from both; only the run's own are reported, in order, after the
// session line.
func TestEventsStayInsideTheWindow(t *testing.T) {
	work := filepath.Join(t.TempDir(), "sweeps")
	run := Window{Start: spawn, End: spawn.Add(10 * time.Minute)}
	rt.WriteCrushDB(t, filepath.Join(work, ".crush", "crush.db"),
		sess("s1", spawn.Add(2*time.Second),
			read("c1", filepath.Join(work, "prompt.md"), spawn.Add(3*time.Second)),
			[]rt.CrushMessage{
				{Role: "assistant", At: spawn.Add(4 * time.Second), Parts: rt.ToolCall("c2", "bash", map[string]any{"command": "ssh nuc01 docker ps"})},
				{Role: "tool", At: spawn.Add(6 * time.Second), Parts: rt.ToolResult("c2", "CONTAINER ID")},
			},
			read("c3", filepath.Join(work, "later.md"), spawn.Add(3*time.Hour)),
		))
	h := crushHarness(t, "sweep", work, nil)
	a := attribute(t, h, run)
	entries, errs := Events(context.Background(), a, false, spawn.Add(24*time.Hour))
	if len(errs) != 0 {
		t.Fatalf("Events errors: %v", errs)
	}
	var got []string
	for _, e := range entries {
		if e.Kind == KindMark {
			continue // marks depend on the agent-trace version; the tool stream is what this pins
		}
		got = append(got, fmt.Sprintf("%s:%s:%s", e.Kind, e.Action, filepath.Base(e.Target)))
	}
	want := []string{"session:session:.", "tool:read:prompt.md", "tool:exec:."}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("entries = %v, want %v", got, want)
	}
	for _, e := range entries {
		if e.Ambiguous {
			t.Errorf("entry %+v flagged ambiguous in an unambiguous run", e)
		}
	}
}

// TestEntryDetailIsTheCommandOrThePath: classify's summary ends in a
// classification tally, and a sweep in a scratch workdir touches nothing
// "in repo". Rendered as-is, a real tars run was forty lines of
// "view -> 0 targets, 1 outside".
func TestEntryDetailIsTheCommandOrThePath(t *testing.T) {
	work := filepath.Join(t.TempDir(), "sweeps")
	prompt := filepath.Join(t.TempDir(), "config", "stumpcloud-sweep.prompt.md")
	rt.WriteCrushDB(t, filepath.Join(work, ".crush", "crush.db"),
		sess("s1", spawn.Add(2*time.Second),
			read("c1", prompt, spawn.Add(3*time.Second)),
			[]rt.CrushMessage{
				{Role: "assistant", At: spawn.Add(4 * time.Second), Parts: rt.ToolCall("c2", "bash", map[string]any{"command": "ssh nuc01 docker ps"})},
				{Role: "tool", At: spawn.Add(5 * time.Second), Parts: rt.ToolResult("c2", "CONTAINER ID")},
			},
		))
	a := attribute(t, crushHarness(t, "sweep", work, nil), Window{Start: spawn, End: spawn.Add(time.Minute)})
	entries, errs := Events(context.Background(), a, false, spawn.Add(time.Hour))
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	var tools []Entry
	for _, e := range entries {
		if e.Kind == KindTool {
			tools = append(tools, e)
		}
	}
	if len(tools) != 2 {
		t.Fatalf("tool entries = %+v, want 2", tools)
	}
	if tools[0].Target != prompt {
		t.Errorf("read target = %q, want the outside path %q", tools[0].Target, prompt)
	}
	if tools[1].Summary != "ssh nuc01 docker ps" {
		t.Errorf("exec summary = %q, want the bare command", tools[1].Summary)
	}
	if got := tallySuffix.ReplaceAllString("go test ./... -> 2 targets, 0 outside error", ""); got != "go test ./..." {
		t.Errorf("tally strip = %q, want the failure suffix gone too", got)
	}
}

func TestClaimant(t *testing.T) {
	run := Window{Start: spawn, End: spawn.Add(time.Hour)}
	meta := tail.SessionMeta{Harness: tail.HarnessCrush, Cwd: "/w", StartedAt: spawn.Add(time.Minute).Format(time.RFC3339)}
	for _, tc := range []struct {
		name   string
		scopes []Scope
		meta   tail.SessionMeta
		want   string
	}{
		{"sole claimant", []Scope{{Name: "sweep", Adapter: "crush", Workdir: "/w", Runs: []Window{run}}}, meta, "sweep"},
		{"trailing slash", []Scope{{Name: "sweep", Adapter: "crush", Workdir: "/w/", Runs: []Window{run}}}, meta, "sweep"},
		{"two claimants", []Scope{
			{Name: "a", Adapter: "crush", Workdir: "/w", Runs: []Window{run}},
			{Name: "b", Adapter: "crush", Workdir: "/w", Runs: []Window{run}},
		}, meta, ""},
		{"not running then", []Scope{{Name: "sweep", Adapter: "crush", Workdir: "/w", Runs: []Window{{Start: spawn.Add(2 * time.Hour)}}}}, meta, ""},
		{"other kind", []Scope{{Name: "claude", Adapter: "claude-code", Workdir: "/w", Runs: []Window{run}}}, meta, ""},
		{"generic only", []Scope{{Name: "wrapper", Adapter: "generic", Workdir: "/w", Runs: []Window{run}}}, meta, ""},
		{"subdirectory", []Scope{{Name: "sweep", Adapter: "crush", Workdir: "/", Runs: []Window{run}}}, meta, ""},
		{"no start time", []Scope{{Name: "sweep", Adapter: "crush", Workdir: "/w", Runs: []Window{run}}}, tail.SessionMeta{Harness: tail.HarnessCrush, Cwd: "/w"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Claimant(tc.meta, tc.scopes, spawn.Add(24*time.Hour))
			if got != tc.want || ok != (tc.want != "") {
				t.Errorf("Claimant = %q, %v; want %q", got, ok, tc.want)
			}
		})
	}
}

func TestSourcesFollowRelocatedStores(t *testing.T) {
	claude, err := Sources(Scope{Adapter: "claude-code", Env: map[string]string{"CLAUDE_CONFIG_DIR": "/cfg/claude"}})
	if err != nil {
		t.Fatal(err)
	}
	if a, ok := claude[0].(*tail.ClaudeCodeAdapter); !ok || a.Dir != "/cfg/claude/projects" {
		t.Errorf("claude-code source = %#v, want Dir /cfg/claude/projects", claude[0])
	}
	codex, err := Sources(Scope{Adapter: "codex", Env: map[string]string{"HOME": "/home/agent"}})
	if err != nil {
		t.Fatal(err)
	}
	if a, ok := codex[0].(*tail.CodexAdapter); !ok || a.Dir != "/home/agent/.codex/sessions" {
		t.Errorf("codex source = %#v, want Dir /home/agent/.codex/sessions", codex[0])
	}
	crush, err := Sources(Scope{Adapter: "crush", Workdir: "/nonexistent", Env: map[string]string{"XDG_DATA_HOME": "/xdg"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(crush) != 1 || crush[0].(*tail.CrushAdapter).ProjectsPath != "/xdg/crush/projects.json" {
		t.Errorf("crush sources = %#v, want only the XDG registry (no project store on disk)", crush)
	}
}

// TestEntriesAreRedacted: a transcript records commands verbatim, and a sweep
// that sets a token-bearing remote or sends an Authorization header must not
// put the credential in `harness logs` or its --json.
func TestEntriesAreRedacted(t *testing.T) {
	work := filepath.Join(t.TempDir(), "sweeps")
	rt.WriteCrushDB(t, filepath.Join(work, ".crush", "crush.db"),
		sess("s1", spawn.Add(2*time.Second), []rt.CrushMessage{
			{Role: "assistant", At: spawn.Add(3 * time.Second), Parts: rt.ToolCall("c1", "bash", map[string]any{
				"command": "git remote set-url origin https://joestump-agent:0123456789abcdef0123@gitea.stump.rocks/a/b.git",
			})},
			{Role: "tool", At: spawn.Add(4 * time.Second), Parts: rt.ToolResult("c1", "")},
			{Role: "assistant", At: spawn.Add(5 * time.Second), Parts: rt.FinishError("Unauthorized", "sent Authorization: Bearer sk-abcdefghijklmnopqrstuvwxyz")},
		}))
	a := attribute(t, crushHarness(t, "sweep", work, nil), Window{Start: spawn, End: spawn.Add(time.Minute)})
	entries, errs := Events(context.Background(), a, false, spawn.Add(time.Hour))
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	masked := 0
	for _, e := range entries {
		for _, s := range []string{e.Target, e.Summary} {
			if strings.Contains(s, "0123456789abcdef0123") || strings.Contains(s, "sk-abcdefghij") {
				t.Errorf("entry %s carries a credential: %q", e.ID, s)
			}
			if strings.Contains(s, redact.Mask) {
				masked++
			}
		}
	}
	if masked < 2 {
		t.Errorf("entries = %+v, want the remote's password and the bearer token both masked", entries)
	}
}
