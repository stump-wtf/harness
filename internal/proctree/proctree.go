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
// Governing: ADR-0045; issue #959.
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
