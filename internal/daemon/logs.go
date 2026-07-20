package daemon

// Governing: ADR-0007 (the daemon tees raw PTY output to a rotating per-harness
// log under $XDG_STATE_HOME/harness/logs/<name>.log; `harness logs <name>`
// reads it for live and dead harnesses alike). SPEC-0002 REQ "Control
// Operations" ("logs"). This reads the active log file's tail; rotated backups
// are intentionally out of scope for the tail view.

import (
	"os"
	"path/filepath"
)

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
	return string(tailLines(data, lines))
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
