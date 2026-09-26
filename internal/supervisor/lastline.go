package supervisor

// Last Output Line
//
// The most useful sentence in an alert about a dead harness is usually the
// last thing its agent printed: claude remote-control exiting 1 six times with
// "Error: You must be logged in to use Remote Control." says what to do in a
// way no exit code can. This reads it back from the durable log, which
// interleaves the sanitized output history with the daemon's own lines — the
// charmbracelet lifecycle lines logEvent writes and the "[harness] …" notes —
// so those are skipped: an alert that quotes "INFO state changed from=… to=
// failed" back at the operator has told them nothing.
//
// Governing: ADR-0007 (the durable log is the record of what a harness
// printed); SPEC-0003 REQ "Operator Notification"; issue #725.

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"regexp"
	"strings"
)

// lastLineWindow is how much of the log's tail is read. The line wanted sits
// just before the exit, and the log is up to 8 MiB, so the whole file is never
// worth reading.
const lastLineWindow = 64 << 10

// maxLastLine caps the returned line, which is going into a one-line alert.
const maxLastLine = 240

// daemonLine matches a line the daemon itself wrote into the durable log:
// newEventLogger's timestamp and a charmbracelet level. Any level, not just
// INFO: guard notes (LogLifecycle) are written at INFO today, but an alert
// quoting a daemon WARN back is as unhelpful as quoting an INFO.
var daemonLine = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} (?:DEBU|INFO|WARN|ERRO|FATA) `)

// LastOutputLine returns the last non-blank line of the log at path that the
// harnessed program printed, trimmed and capped, or "" when there is none (no
// log, an empty one, or only daemon lines in its tail).
func LastOutputLine(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	off := info.Size() - lastLineWindow
	if off < 0 {
		off = 0
	}
	buf, err := io.ReadAll(io.NewSectionReader(f, off, info.Size()-off))
	if err != nil {
		return ""
	}
	if off > 0 {
		// The window almost certainly starts mid-line; drop the fragment.
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			buf = buf[i+1:]
		}
	}
	last := ""
	sc := bufio.NewScanner(bytes.NewReader(buf))
	sc.Buffer(make([]byte, 0, 64<<10), lastLineWindow+1)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || daemonLine.MatchString(line) || strings.HasPrefix(line, "[harness] ") {
			continue
		}
		last = line
	}
	if r := []rune(last); len(r) > maxLastLine {
		last = string(r[:maxLastLine-1]) + "…"
	}
	return last
}
