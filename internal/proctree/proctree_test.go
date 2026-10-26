package proctree

// Governing: ADR-0045; issue #959.

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// table is the ps output every parse fixture feeds in: the root, two children,
// a grandchild, a command whose name has a space in it, and the junk a real
// table contains (a header-less but truncated line, a nonexistent pid).
const table = `  1     0  900 systemd
100    1  600 harness
101  100   30 node mcp server
102  100   20 git
103  101    5 chromium
bad    x  y  junk
104 999    7 orphan
`

func TestParseNamesRootFirstAndChildrenNearestFirst(t *testing.T) {
	lines, err := parse(table, 100, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []struct {
		pid, ppid int
		age       time.Duration
		name      string
	}{
		{100, 1, 600 * time.Second, "harness"},
		{101, 100, 30 * time.Second, "node mcp server"},
		{102, 100, 20 * time.Second, "git"},
		{103, 101, 5 * time.Second, "chromium"},
	}
	if len(lines) != len(want) {
		t.Fatalf("lines = %+v, want %d", lines, len(want))
	}
	for i, w := range want {
		g := lines[i]
		if g.PID != w.pid || g.Parent != w.ppid || g.Age != w.age || g.Name != w.name {
			t.Errorf("line %d = %+v, want pid=%d ppid=%d age=%v cmd=%s", i, g, w.pid, w.ppid, w.age, w.name)
		}
	}
}

func TestParseLimitCutsTheFarEndOfTheTree(t *testing.T) {
	lines, err := parse(table, 100, 3)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(lines) != 3 {
		t.Fatalf("lines = %+v, want the cap of 3", lines)
	}
	if lines[2].PID != 102 {
		t.Errorf("third line = %+v, want the nearest generation, not a grandchild", lines[2])
	}
}

func TestParseUnknownRootIsJustTheRoot(t *testing.T) {
	lines, err := parse(table, 4242, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(lines) != 1 || lines[0].PID != 4242 || lines[0].Parent != 0 {
		t.Fatalf("lines = %+v, want the root alone with no parent", lines)
	}
}

func TestParseEmptyTable(t *testing.T) {
	lines, err := parse("", 7, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(lines) != 1 || lines[0].PID != 7 || lines[0].Name != "" || lines[0].Age != 0 {
		t.Fatalf("lines = %+v, want the root alone and unknown", lines)
	}
}

func TestLineString(t *testing.T) {
	got := Line{PID: 12, Parent: 3, Age: 90 * time.Second, Name: "harness"}.String()
	if !strings.Contains(got, "pid=12") || !strings.Contains(got, "cmd=harness") || !strings.Contains(got, "age=1m30s") {
		t.Fatalf("String() = %q, want pid, age and cmd in it", got)
	}
}

// TestTreeFindsALiveChild: the parse fixtures prove the shape; this proves the
// ps arguments actually describe a real process, which is the only way to know
// the dump will name what a wedged agent is waiting on.
func TestTreeFindsALiveChild(t *testing.T) {
	if testing.Short() {
		t.Skip("ps(1) needed")
	}
	if _, err := exec.LookPath("ps"); err != nil {
		t.Skip("no ps(1) on this box")
	}
	child := exec.Command("sleep", "5")
	if err := child.Start(); err != nil {
		t.Skipf("cannot spawn a child: %v", err)
	}
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_, _ = child.Process.Wait()
	})

	lines, err := Tree(os.Getpid(), DefaultLimit)
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	if lines[0].PID != os.Getpid() {
		t.Fatalf("lines = %+v, want this process first", lines)
	}
	for _, l := range lines[1:] {
		if l.PID == child.Process.Pid {
			return
		}
	}
	t.Fatalf("the spawned sleep is not in the tree: %+v", lines)
}

// childTable is the pid/ppid/etime/time/comm dump the snapshot reads: the
// longest-lived child first, a name with a space in it, a bare-seconds elapsed
// column, and the junk a real table contains.
const childTable = `  1     0  90-01:02:03  12:34:56 systemd
100    1  23:10:00      0:05 harness
101  100  23:09:55      0:00 grep
102  100  05:12         0:03 node mcp server
103  100  10            0:00 sleep
bad    x   y            z  junk
104 999    7           0:01 orphan
`

var tableNow = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func TestParseClock(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"05:12", 5*time.Minute + 12*time.Second, true},
		{"1:02:03", time.Hour + 2*time.Minute + 3*time.Second, true},
		{"23-01:02:03", 23*24*time.Hour + time.Hour + 2*time.Minute + 3*time.Second, true},
		{"  10  ", 10 * time.Second, true},
		{"0:00", 0, true},
		{"", 0, false},
		{"abc", 0, false},
		{"1:2:3:4", 0, false},
		{"x-01:02", 0, false},
	}
	for _, c := range cases {
		got, ok := parseClock(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("parseClock(%q) = %v,%v want %v,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestChildrenAreEldestFirstWithAgeAndCPU(t *testing.T) {
	tbl := parseTable(childTable, tableNow)
	got := tbl.Children(100, 0)
	want := []Child{
		{PID: 101, Command: "grep", StartedAt: tableNow.Add(-23*time.Hour - 9*time.Minute - 55*time.Second), CPU: 0},
		{PID: 102, Command: "node mcp server", StartedAt: tableNow.Add(-5*time.Minute - 12*time.Second), CPU: 3 * time.Second},
		{PID: 103, Command: "sleep", StartedAt: tableNow.Add(-10 * time.Second), CPU: 0},
	}
	if len(got) != len(want) {
		t.Fatalf("children = %+v, want %d", got, len(want))
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("children[%d] = %+v, want %+v", i, got[i], w)
		}
	}
}

// TestChildrenCapKeepsTheEldest: the cap answers "what has been sitting here
// longest", so it cuts the newest, never the hung one (issue #958).
func TestChildrenCapKeepsTheEldest(t *testing.T) {
	got := parseTable(childTable, tableNow).Children(100, 2)
	if len(got) != 2 || got[0].PID != 101 || got[1].PID != 102 {
		t.Fatalf("children = %+v, want 101 then 102", got)
	}
}

func TestChildrenOfUnknownRootIsEmpty(t *testing.T) {
	if got := parseTable(childTable, tableNow).Children(4242, 0); len(got) != 0 {
		t.Fatalf("children = %+v, want none", got)
	}
}

func TestChildrenOfNilTableIsEmpty(t *testing.T) {
	var tbl *Table
	if got := tbl.Children(1, 0); len(got) != 0 {
		t.Fatalf("children = %+v, want none", got)
	}
}

// TestChildrenNamesALiveChild checks the columns the snapshot needs — elapsed
// and cumulative CPU — against a real process. A fixture cannot catch ps
// building that rejects one of them, and that failure looks exactly like a
// harness with no children.
func TestChildrenNamesALiveChild(t *testing.T) {
	if testing.Short() {
		t.Skip("ps(1) needed")
	}
	if _, err := exec.LookPath("ps"); err != nil {
		t.Skip("no ps(1) on this box")
	}
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Skipf("cannot spawn a child: %v", err)
	}
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_, _ = child.Process.Wait()
	})

	tbl, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	started := time.Now()
	for _, c := range tbl.Children(os.Getpid(), ChildLimit) {
		if c.PID != child.Process.Pid {
			continue
		}
		if c.StartedAt.IsZero() {
			// A skewed host clock clamps started_at to unknown rather than
			// future-dated (issue #1002); on a healthy clock this is the
			// elapsed column refusing to parse.
			t.Logf("child %+v has no started_at", c)
		} else if c.StartedAt.Before(started.Add(-time.Minute)) || c.StartedAt.After(time.Now().Add(time.Minute)) {
			t.Fatalf("child started_at = %v, want within a minute of now", c.StartedAt)
		}
		if c.CPU < 0 {
			t.Fatalf("child cpu = %v, want non-negative", c.CPU)
		}
		if c.Command == "" {
			t.Fatalf("child %+v names no command", c)
		}
		return
	}
	t.Fatalf("the spawned sleep is not among this process's children: %+v", tbl.Children(os.Getpid(), ChildLimit))
}

// TestFutureDatedSampleIsClamped feeds the skew the issue names: a table read
// on a host whose clock is hours ahead of the clock the reader reads by (the
// btime/uptime pair disagreeing with the wall clock, issue #1002). Every
// child's start must read as unknown, not as a moment decades from now. A
// table read now must not be clamped: the tolerance bounds wall skew, not
// age.
func TestFutureDatedSampleIsClamped(t *testing.T) {
	skewed := parseTable(childTable, time.Now().Add(24*time.Hour))
	for _, c := range skewed.Children(100, 0) {
		if !c.StartedAt.IsZero() {
			t.Errorf("child %+v started_at = %v, want the future-dated sample clamped to unknown", c, c.StartedAt)
		}
	}
	fresh := parseTable(childTable, time.Now())
	for _, c := range fresh.Children(100, 0) {
		if c.StartedAt.IsZero() {
			t.Errorf("child %+v lost a started_at a table read now should carry", c)
		}
	}
}
