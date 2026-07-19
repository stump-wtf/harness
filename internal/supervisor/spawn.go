package supervisor

// Governing: ADR-0005 (each running harness has a supervisor goroutine that
// spawns `cmd args` under a PTY in `workdir`, with `env_file` loaded);
// ADR-0003 (native backend runs the process under the daemon's own PTY via
// x/xpty); ADR-0008 (secrets stay in env_file — loaded into the child's
// environment at spawn, never copied into state/logs we write).

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/charmbracelet/x/xpty"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
)

// defaultPTYCols/Rows size a freshly spawned PTY before any client attaches
// (ADR-0003; a real attach resizes it later).
const (
	defaultPTYCols = 80
	defaultPTYRows = 24
)

// expandHome expands a leading ~ (or ~/) in p to the user's home directory.
func expandHome(p string) string {
	if p == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
		return p
	}
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// parseEnvFile reads a KEY=VALUE env file into a slice of "KEY=VALUE" strings
// suitable for exec.Cmd.Env. Blank lines and #-comments are skipped; a leading
// `export ` is tolerated; surrounding single or double quotes on the value are
// stripped. A missing path is not an error (the harness simply has no extra
// env); other read errors are surfaced. Secrets stay here (ADR-0008).
func parseEnvFile(path string) ([]string, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	path = expandHome(path)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("supervisor: open env_file: %w", err)
	}
	defer f.Close()

	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue // not a KEY=VALUE line
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		val = unquote(val)
		out = append(out, key+"="+val)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("supervisor: read env_file: %w", err)
	}
	return out, nil
}

// unquote strips a single matching pair of surrounding single or double quotes.
func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// buildEnv composes the child environment: the daemon's own environment plus
// the parsed env_file (env_file wins on key collisions, appended last).
func buildEnv(h core.Harness) ([]string, error) {
	extra, err := parseEnvFile(h.EnvFile)
	if err != nil {
		return nil, err
	}
	env := os.Environ()
	env = append(env, extra...)
	return env, nil
}

// expandArgs substitutes the {workdir} placeholder in each arg with the
// expanded working directory (ADR-0006 documents {workdir} expansion happening
// at spawn time in the supervisor, not the config parser).
func expandArgs(args []string, workdir string) []string {
	if len(args) == 0 {
		return nil
	}
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = strings.ReplaceAll(a, "{workdir}", workdir)
	}
	return out
}

// process is a live spawned harness: its PTY, the command handle (for signals
// and reaping), and its OS pid.
type process struct {
	pty xpty.Pty
	cmd *exec.Cmd
	pid int
}

// spawn launches h under a fresh PTY in its workdir with env_file loaded. The
// child is placed in its own session (Setsid) so the whole process group can be
// signalled on graceful stop (SPEC-0003 REQ "Graceful Stop"). The returned
// process's PTY is the raw byte stream the caller tees to logs.
func spawn(h core.Harness) (*process, error) {
	workdir := expandHome(h.Workdir)
	env, err := buildEnv(h)
	if err != nil {
		return nil, err
	}

	pty, err := xpty.NewPty(defaultPTYCols, defaultPTYRows)
	if err != nil {
		return nil, fmt.Errorf("supervisor: allocate pty: %w", err)
	}

	cmd := exec.Command(h.Cmd, expandArgs(h.Args, workdir)...)
	cmd.Dir = workdir
	cmd.Env = env
	// New session → child is a process-group leader (pgid == pid); a graceful
	// stop can signal the entire group (kill(-pid)) to reap child processes
	// like a shell's `sleep`.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := pty.Start(cmd); err != nil {
		_ = pty.Close()
		return nil, fmt.Errorf("supervisor: start %q: %w", h.Cmd, err)
	}
	return &process{pty: pty, cmd: cmd, pid: cmd.Process.Pid}, nil
}

// signalGroup sends sig to the child's process group, falling back to the
// single process if the group send fails.
func (p *process) signalGroup(sig syscall.Signal) {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-p.pid, sig); err != nil {
		_ = p.cmd.Process.Signal(sig)
	}
}
