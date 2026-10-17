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
