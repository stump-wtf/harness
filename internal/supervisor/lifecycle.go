package supervisor

// Lifecycle Lines
//
// The supervisor writes its lifecycle events — state changes, exits, flapping —
// into the same durable log that holds the sanitized output history (#279).
// This reads them back, so `harness logs` can interleave them with agent
// activity and so run correlation can recover the run windows of harnesses
// whose snapshot only remembers the latest run.
//
// These lines share a file with whatever the harnessed program printed, and a
// program can print a line that looks like one. So nothing here decides what a
// harness is credited with: the attributed window comes from the supervisor's
// own snapshot, and log-derived runs only ever add claimants to a session —
// which can hide a session from `harness logs`, never misattribute one.
//
// Governing: ADR-0007 (amended, #279 — lifecycle events are charmbracelet/log
// lines in the durable log), SPEC-0006 REQ "Run Correlation".
//
// @joestump-agent 09/11/2026 - Added for harness#302 and harness#89.

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	clog "github.com/charmbracelet/log"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
)

// LifecycleEntry is one lifecycle line read back from a durable log.
type LifecycleEntry struct {
	// Time is second-truncated local time, as newEventLogger wrote it.
	Time time.Time
	// Msg is "state changed", "exited" or "flapping" — the three logEvent
	// messages.
	Msg string
	// Fields holds the key=value pairs: from/to, code, restarts/next_retry_in.
	Fields map[string]string
}

// RunSpan is one run recovered from lifecycle lines.
type RunSpan struct {
	Start time.Time
	// End is zero when no end was recorded: the run is still going, or the
	// daemon died with it.
	End time.Time
	// ExitCode is set when the run ended with a recorded exit.
	ExitCode *int
}

// lifecycleLine matches exactly what logEvent writes: newEventLogger's
// timestamp, the INFO level, one of the three messages, and unquoted
// key=value pairs (every value logEvent passes is a single token).
var lifecycleLine = regexp.MustCompile(`^(\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}) INFO (state changed|exited|flapping)((?: [a-z_]+=[^\s"]+)*)$`)

// ReadLifecycle returns the lifecycle lines of name's durable log — rotated
// backups oldest first, then the active file — skipping any file last written
// before since (a zero since reads everything). A missing log is no lines, not
// an error.
func ReadLifecycle(dir, name string, since time.Time) ([]LifecycleEntry, error) {
	if dir == "" {
		return nil, nil
	}
	files, err := filepath.Glob(filepath.Join(dir, name+"-*.log"))
	if err != nil {
		return nil, fmt.Errorf("supervisor: list rotated logs for %s: %w", name, err)
	}
	files = slices.DeleteFunc(files, func(p string) bool { return !isBackupOf(name, p) })
	slices.Sort(files) // the rotation stamp sorts chronologically
	files = append(files, filepath.Join(dir, name+".log"))

	var out []LifecycleEntry
	for _, path := range files {
		info, err := os.Stat(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return out, fmt.Errorf("supervisor: stat log %s: %w", path, err)
		}
		if !since.IsZero() && info.ModTime().Before(since) {
			continue
		}
		entries, err := readLifecycleFile(path)
		out = append(out, entries...)
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

func readLifecycleFile(path string) ([]LifecycleEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("supervisor: open log %s: %w", path, err)
	}
	defer f.Close()
	var out []LifecycleEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		// Cheap reject before the regexp: almost every line is output history.
		if len(line) < 25 || line[4] != '/' || line[19:25] != " INFO " {
			continue
		}
		m := lifecycleLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		t, err := time.ParseInLocation(clog.DefaultTimeFormat, m[1], time.Local)
		if err != nil {
			continue
		}
		fields := map[string]string{}
		for _, kv := range strings.Fields(m[3]) {
			k, v, _ := strings.Cut(kv, "=")
			fields[k] = v
		}
		out = append(out, LifecycleEntry{Time: t, Msg: m[2], Fields: fields})
	}
	if err := sc.Err(); err != nil {
		return out, fmt.Errorf("supervisor: read log %s: %w", path, err)
	}
	return out, nil
}

// RunSpans folds lifecycle lines into runs. A run starts at a transition to
// starting and ends at the next exit, or at a transition to stopped (a graceful
// stop consumes the exit itself and logs no "exited" line). A start that
// follows a start with no end closes the earlier run there. Ends are rounded up
// a second, because the stamps are truncated.
//
// A transition to running with no run open also starts one. The first start
// after a daemon boot moves to starting before the durable log is opened, so
// that run's log begins at "from=starting to=running" (seen on tars, every
// crush agent after the 2026-09-11 daemon restart).
func RunSpans(entries []LifecycleEntry) []RunSpan {
	var out []RunSpan
	open := -1
	for _, e := range entries {
		to := e.Fields["to"]
		switch {
		case e.Msg == "state changed" && to == string(core.StateStarting):
			if open >= 0 {
				out[open].End = e.Time
			}
			out = append(out, RunSpan{Start: e.Time})
			open = len(out) - 1
		case open < 0 && e.Msg == "state changed" && to == string(core.StateRunning):
			out = append(out, RunSpan{Start: e.Time})
			open = len(out) - 1
		case open >= 0 && e.Msg == "exited":
			out[open].End = e.Time.Add(time.Second)
			if code, err := strconv.Atoi(e.Fields["code"]); err == nil {
				out[open].ExitCode = &code
			}
			open = -1
		case open >= 0 && e.Msg == "state changed" && e.Fields["to"] == string(core.StateStopped):
			out[open].End = e.Time.Add(time.Second)
			open = -1
		}
	}
	return out
}
