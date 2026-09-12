package daemon

// Governing: ADR-0007 (the daemon tees raw PTY output to a rotating per-harness
// log under $XDG_STATE_HOME/harness/logs/<name>.log; `harness logs <name>`
// reads it for live and dead harnesses alike). SPEC-0002 REQ "Control
// Operations" ("logs"). This reads the active log file's tail; rotated backups
// are intentionally out of scope for the tail view.

import (
	"os"
	"path/filepath"
	"strings"

	"gitea.stump.rocks/stump.wtf/harness/internal/redact"
)

// redactTail masks credentials in a durable-log tail, line by line.
//
// The log records whatever the harnessed program printed, verbatim — a
// token-bearing git remote, an `Authorization:` header, a `--password` flag.
// Every reader of a log goes through this function or its sibling in jobs.go,
// so `--raw`, `--follow`, the peek pane, a generic harness's fallback and
// `--json` are covered in one place rather than each client being trusted to
// remember.
//
// Line by line, because that is the granularity redact's rules are written
// for, and because it leaves the tail's structure intact: `--raw` still shows
// the shape of what ran, only not the secret inside it.
//
// New output is also masked before it reaches disk (ptyHistory, in
// internal/supervisor); this is what covers logs already written. redact.String
// is idempotent, so a line masked at write time passes through unchanged.
//
// Governing: ADR-0008 (secrets, as amended for #312), SPEC-0002 REQ "Control
// Operations" ("logs"); issue #312.
func redactTail(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	lines := strings.Split(string(b), "\n")
	for i, ln := range lines {
		lines[i] = redact.String(ln)
	}
	return strings.Join(lines, "\n")
}

// readLogTail returns the last `lines` lines of the harness's active log file,
// or "" if the log does not exist yet. Best-effort: a read error yields "".
func readLogTail(dir, name string, lines int) string {
	if dir == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(dir, name+".log"))
	if err != nil {
		return ""
	}
	return redactTail(tailLines(data, lines))
}

// tailLines returns the last n lines of data (preserving trailing newline
// shape). A trailing newline is not counted as an empty final line.
func tailLines(data []byte, n int) []byte {
	if n <= 0 || len(data) == 0 {
		return data
	}
	// Ignore a single trailing newline when counting boundaries so "a\nb\n"
	// with n=1 returns "b\n", not "".
	end := len(data)
	search := data
	if search[end-1] == '\n' {
		search = search[:end-1]
	}
	count := 0
	for i := len(search) - 1; i >= 0; i-- {
		if search[i] == '\n' {
			count++
			if count == n {
				return data[i+1:]
			}
		}
	}
	return data // fewer than n lines: return all
}
