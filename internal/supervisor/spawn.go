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

// defaultPTYCols/Rows size a freshly spawned PTY when no client viewport is
// known (ADR-0003; a real attach resizes it later).
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
//
// The child runs under a real, color-capable PTY (xpty), so it must advertise
// one: a daemon launched detached (launchd/systemd, a closed login shell) can
// carry a TERM that is missing or "dumb" and no COLORTERM, which makes
// full-screen harness apps render in black & white. We therefore guarantee
// sane terminal defaults — a color-capable TERM and COLORTERM=truecolor —
// for any key neither the daemon's environment nor the env_file already set.
func buildEnv(h core.Harness) ([]string, error) {
	extra, err := parseEnvFile(h.EnvFile)
	if err != nil {
		return nil, err
	}
	env := os.Environ()
	env = ensureTermEnv(env)
	env = append(env, extra...)
	return env, nil
}

// ensureTermEnv returns env with a color-capable TERM and COLORTERM=truecolor
// appended for any terminal key it does not already set. Existing values win:
// a daemon that already exports TERM=xterm-kitty (or an env_file that pins
// COLORTERM) is left untouched, so this only repairs a colorless default.
//
// exec.Cmd.Env semantics: later duplicates shadow earlier ones, so appending
// only when absent is both sufficient and the conservative choice — we never
// clobber a deliberate value with our generic default.
func ensureTermEnv(env []string) []string {
	var hasTERM, hasCOLORTERM bool
	for _, kv := range env {
		if k, _, ok := strings.Cut(kv, "="); ok {
			switch k {
			case "TERM":
				hasTERM = true
			case "COLORTERM":
				hasCOLORTERM = true
			}
		}
	}
	// Treat an empty or "dumb" TERM as absent: it advertises no color and no
	// cursor addressing, which is exactly the broken default we are repairing.
	if !hasTERM || envValue(env, "TERM") == "" || envValue(env, "TERM") == "dumb" {
		env = append(env, "TERM=xterm-256color")
	}
	if !hasCOLORTERM {
		env = append(env, "COLORTERM=truecolor")
	}
	return env
}

// envValue returns the value of the last KEY= entry in env ("" if unset).
func envValue(env []string, key string) string {
	val := ""
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			val = v
		}
	}
	return val
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

// execArgv resolves the executable and argv spawn runs: the configured cmd
// with {workdir}-expanded args, or — for a prompt harness (empty Cmd, ADR-0011
// spawn-time synthesis) — the argv core.AgentCommand builds from the prompt
// and the optional model selection. Prompt and model are passed verbatim:
// only configured args go through expandArgs's {workdir} substitution, never
// those two — a prompt legitimately containing "{workdir}" is instruction
// text, not a placeholder. The cmd path ignores Model entirely (config
// validation forbids the combination; a wire def carrying both spawns on its
// configured argv alone).
func execArgv(h core.Harness, workdir string) (string, []string) {
	if h.Cmd == "" && h.Prompt != "" {
		return core.AgentCommand(h.Prompt, h.Model)
	}
	return h.Cmd, expandArgs(h.Args, workdir)
}

// process is a live spawned harness: its PTY, the command handle (for signals
// and reaping), and its OS pid.
type process struct {
	pty xpty.Pty
	cmd *exec.Cmd
	pid int
}

// spawn launches h under a fresh PTY of cols×rows in its workdir with env_file
// loaded. The child is placed in its own session (Setsid) so the whole process
// group can be signalled on graceful stop (SPEC-0003 REQ "Graceful Stop"). The
// returned process's PTY is the raw byte stream the caller tees to logs.
//
// Governing: ADR-0003 (the native backend owns PTY sizing; the attach layer's
// smallest-attached-wins viewport is authoritative). The size is passed in
// rather than fixed at 80×24 because a harness is routinely (re)started while a
// client is already attached — a restart, a crash-restart, or `^b s` from
// attached mode. Spawning at 80×24 in that case leaves the child permanently
// undersized: the mux's recorded size already equals the client viewport, so
// its resize policy sees no change and never pushes a TIOCSWINSZ, and the app
// inside renders into an 80×24 box in the corner of a full-size window.
func spawn(h core.Harness, cols, rows int) (*process, error) {
	workdir := expandHome(h.Workdir)
	env, err := buildEnv(h)
	if err != nil {
		return nil, err
	}

	if cols < 1 {
		cols = defaultPTYCols
	}
	if rows < 1 {
		rows = defaultPTYRows
	}
	pty, err := xpty.NewPty(cols, rows)
	if err != nil {
		return nil, fmt.Errorf("supervisor: allocate pty: %w", err)
	}

	name, args := execArgv(h, workdir)
	cmd := exec.Command(name, args...)
	cmd.Dir = workdir
	cmd.Env = env
	// New session → child is a process-group leader (pgid == pid); a graceful
	// stop can signal the entire group (kill(-pid)) to reap child processes
	// like a shell's `sleep`.
	//
	// Setctty additionally makes the PTY the new session's *controlling*
	// terminal (Ctty 0 = the child's stdin, which xpty points at the slave).
	// Without it the child has no ctty, and the kernel has nobody to notify:
	// a TIOCSWINSZ changes the PTY's dimensions but raises no SIGWINCH, so a
	// full-screen app never learns it should re-lay-out and keeps painting its
	// old geometry into a correctly-sized window — the same symptom as spawning
	// at the wrong size, one layer down (ADR-0003). It is also what lets an
	// attached client's ^C reach the foreground process group as SIGINT.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}

	if err := pty.Start(cmd); err != nil {
		_ = pty.Close()
		return nil, fmt.Errorf("supervisor: start %q: %w", name, err)
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
