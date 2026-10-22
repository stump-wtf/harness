// Package proctree lists the descendants of a process, for the post-mortem a
// stall writes to the log. A wedged agent is blocked on something — an MCP
// server that never answered, a child that outlived its command, a connection
// waiting on a dead peer — and the processes still attached to it are the
// shortest description of what.
//
// The process table is read through ps(1), which exists on Linux, macOS and
// the BSDs and needs no privileged access. Where it is missing (a container
// with no procps) Tree returns an error and the caller logs a line and carries
// on: a stall whose post-mortem lacks the tree is still a stall that was
// caught, and neither a shell-out nor a /proc walk belongs in the supervisor's
// hot paths.
//
// Governing: ADR-0045; issue #959. Load/Children add the child inspection
// list/describe expose (ADR-0048; issue #958): the same post-mortem an
// operator would otherwise run ps for by hand.
package proctree

import (
	"bufio"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DefaultLimit is how many processes a tree dump names by default: enough to
// see what an agent is waiting on, not a process listing.
const DefaultLimit = 12

// ChildLimit is how many direct children a listing names. It is deliberately
// looser than DefaultLimit: the question `list`/`describe` answer is which
// child is the eldest and whether it is doing anything, and a worker that
// fanned out a set of MCP servers still has to name the one that hung. Past
// the cap the eldest are still the ones reported.
const ChildLimit = 24

// Line is one process in the tree.
type Line struct {
	PID    int
	Parent int
	// Age is how long the process has been alive, from the process table's
	// own elapsed-seconds column.
	Age time.Duration
	// Name is the executable name as the process table reports it (truncated
	// by the kernel to 15 characters on Linux).
	Name string
}

// String renders one line the way the log and the notify payload want it.
func (l Line) String() string {
	return fmt.Sprintf("pid=%d ppid=%d age=%s cmd=%s", l.PID, l.Parent, l.Age, l.Name)
}

// psArgs is one portable dump of the whole table: pid, parent pid, elapsed
// seconds, command name. Trailing `=` suppresses the header.
var psArgs = []string{"-e", "-o", "pid=,ppid=,etimes=,comm="}

// Tree returns root's descendants, nearest first, capped at limit (limit <= 0
// means no cap). root itself is included first, so a reader can tell whose
// tree this was. A process the table does not know (already gone, or not
// ours) yields just root.
func Tree(root int, limit int) ([]Line, error) {
	out, err := exec.Command("ps", psArgs...).Output()
	if err != nil {
		return nil, fmt.Errorf("proctree: read the process table: %w", err)
	}
	return parse(string(out), root, limit)
}

// parse builds the tree from ps output. It is split out so the shape of the
// table it tolerates is testable without spawning processes.
func parse(table string, root, limit int) ([]Line, error) {
	children := map[int][]int{}
	ages := map[int]time.Duration{}
	names := map[int]string{}

	sc := bufio.NewScanner(strings.NewReader(table))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		secs, err := strconv.Atoi(fields[2])
		if err != nil {
			secs = 0
		}
		// The command is everything left over: `comm` is one field, but a
		// name with a space in it must not be cut in half.
		name := strings.Join(fields[3:], " ")
		children[ppid] = append(children[ppid], pid)
		ages[pid] = time.Duration(secs) * time.Second
		names[pid] = name
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	lines := []Line{{PID: root, Parent: parentOf(children, root), Age: ages[root], Name: names[root]}}
	// Breadth-first: the nearest children are the ones the agent is most
	// likely waiting on, and a deep tree cut off at the limit still names
	// them.
	for queue := []int{root}; len(queue) > 0 && (limit <= 0 || len(lines) < limit); {
		pid := queue[0]
		queue = queue[1:]
		kids := children[pid]
		sort.Ints(kids)
		for _, kid := range kids {
			queue = append(queue, kid)
			lines = append(lines, Line{PID: kid, Parent: pid, Age: ages[kid], Name: names[kid]})
			if limit > 0 && len(lines) >= limit {
				return lines, nil
			}
		}
	}
	return lines, nil
}

// parentOf reports root's parent, or 0 when the table does not say.
func parentOf(children map[int][]int, root int) int {
	for ppid, kids := range children {
		for _, kid := range kids {
			if kid == root {
				return ppid
			}
		}
	}
	return 0
}

// Child is one direct child of a supervised process, as the process table
// described it when the Table was read.
type Child struct {
	PID int
	// Command is the executable name as the table reports it (the kernel
	// truncates it to 15 characters on Linux), not the full argv: a child's
	// arguments can carry whatever the agent passed to them.
	Command string
	// StartedAt is when the child came up, from the table's own elapsed-time
	// column, so it is accurate to the second. Zero when the table would not
	// say.
	StartedAt time.Time
	// CPU is the child's cumulative user+system time. Age without CPU is the
	// wedged-child signal: a process alive for hours that has burned seconds
	// is blocked on something, not working.
	CPU time.Duration
}

// Table is one read of the whole process table. A listing of many harnesses
// pays for one ps with it instead of one per harness, which is what keeps
// naming children off the supervisor's hot paths (the package doc).
type Table struct {
	kids    map[int][]int
	elapsed map[int]time.Duration
	cpu     map[int]time.Duration
	names   map[int]string
	now     time.Time
}

// childArgs is the portable dump the snapshot needs: pid, parent pid, elapsed
// time, cumulative CPU time, command name. It is a separate dump from psArgs
// because the CPU column is only read here, and a ps build without it must not
// take the stall post-mortem's tree down with it.
var childArgs = []string{"-e", "-o", "pid=,ppid=,etime=,time=,comm="}

// Load reads the process table once. Where ps is missing (a container with no
// procps) it returns an error and the caller omits children rather than
// inventing them.
func Load() (*Table, error) {
	out, err := exec.Command("ps", childArgs...).Output()
	if err != nil {
		return nil, fmt.Errorf("proctree: read the process table: %w", err)
	}
	return parseTable(string(out), time.Now()), nil
}

// parseTable indexes one ps dump by pid. Split out so the columns it tolerates
// are testable without spawning processes.
func parseTable(table string, now time.Time) *Table {
	t := &Table{
		kids:    map[int][]int{},
		elapsed: map[int]time.Duration{},
		cpu:     map[int]time.Duration{},
		names:   map[int]string{},
		now:     now,
	}
	sc := bufio.NewScanner(strings.NewReader(table))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		// An unreadable clock is a missing value, not a bad line: the pid and
		// its command are still worth naming.
		if age, ok := parseClock(fields[2]); ok {
			t.elapsed[pid] = age
		}
		if cpu, ok := parseClock(fields[3]); ok {
			t.cpu[pid] = cpu
		}
		t.kids[ppid] = append(t.kids[ppid], pid)
		t.names[pid] = strings.Join(fields[4:], " ")
	}
	return t
}

// Children returns root's direct children, eldest first, capped at limit
// (limit <= 0 means ChildLimit). Eldest first because the question asked of
// this list is which in-flight child has been here longest — a hung grep from
// hour one must not be cut by a fan-out of children from hour nine. A nil
// Table yields nothing, so a caller that could not read the table reports no
// children instead of guessing.
func (t *Table) Children(root, limit int) []Child {
	if t == nil {
		return nil
	}
	if limit <= 0 {
		limit = ChildLimit
	}
	kids := append([]int(nil), t.kids[root]...)
	sort.SliceStable(kids, func(i, j int) bool {
		a, b := t.elapsed[kids[i]], t.elapsed[kids[j]]
		if a != b {
			return a > b
		}
		return kids[i] < kids[j]
	})
	if len(kids) > limit {
		kids = kids[:limit]
	}
	out := make([]Child, 0, len(kids))
	for _, pid := range kids {
		c := Child{PID: pid, Command: t.names[pid], CPU: t.cpu[pid]}
		if age, ok := t.elapsed[pid]; ok {
			c.StartedAt = t.now.Add(-age)
		}
		out = append(out, c)
	}
	return out
}

// parseClock reads one of ps's duration columns: "mm:ss", "hh:mm:ss", a bare
// count of seconds, and the "dd-" day prefix etime grows once a process is
// older than a day. Columns are fixed-width, so the padding is trimmed here.
func parseClock(s string) (time.Duration, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	var total time.Duration
	if i := strings.Index(s, "-"); i >= 0 {
		days, err := strconv.Atoi(s[:i])
		if err != nil {
			return 0, false
		}
		total += time.Duration(days) * 24 * time.Hour
		s = s[i+1:]
	}
	if !strings.Contains(s, ":") {
		secs, err := strconv.Atoi(s)
		if err != nil {
			return 0, false
		}
		return total + time.Duration(secs)*time.Second, true
	}
	parts := strings.Split(s, ":")
	if len(parts) > 3 {
		return 0, false
	}
	units := []time.Duration{time.Second, time.Minute, time.Hour}
	for i := len(parts) - 1; i >= 0; i-- {
		n, err := strconv.Atoi(strings.TrimSpace(parts[i]))
		if err != nil {
			return 0, false
		}
		total += time.Duration(n) * units[len(parts)-1-i]
	}
	return total, true
}
