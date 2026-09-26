// Package persona owns the named persona templates behind `harness init`
// (SPEC-0018 REQ-4): a template is a persona.toml plus a prompt.md and a
// system.md, rendered with Go text/template over init ANSWERS ONLY. The
// render API cannot accept event or todo content — that is ADR-0021's
// data-not-instructions boundary, and this package sits on the config side
// of it.
//
// Governing: ADR-0024, SPEC-0018 REQ-4 (persona templates), REQ-29 (error
// handling standards).
package persona

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// embedded carries the built-in templates. Each lives in
// templates/<name>/{persona.toml,prompt.md,system.md}; a user template of
// the same name shadows it.
//
//go:embed templates/*/*
var embedded embed.FS

// Mode says how a persona runs: resident harnesses stay up and wait for a
// doorbell; one-shots run once per trigger or schedule.
type Mode string

const (
	ModeResident Mode = "resident"
	ModeOneShot  Mode = "one-shot"
)

// Sentinels wrap every failure this package reports (SPEC-0018 REQ-29), so a
// caller can classify without string-matching. All carry context: the file
// and key at fault.
var (
	// ErrInvalid marks a malformed template: bad TOML, an unknown key, a
	// missing file, or a bad value.
	ErrInvalid = errors.New("persona: invalid template")
	// ErrNotFound marks a requested template name that exists neither
	// built-in nor in the user's templates directory.
	ErrNotFound = errors.New("persona: template not found")
)

// Template is one validated persona. Every field is config truth; none of it
// is secret and none of it is event content.
type Template struct {
	// Name is the directory name and the `--template` answer value.
	Name string
	// Source is built-in or the user template's path, for --list-templates.
	Source string
	// Shadowed reports a user template overriding a built-in of the same
	// name, noted by --list-templates.
	Shadowed bool

	Description   string
	Mode          Mode
	Client        string   // default client: crush | claude-code | codex | pi-omp
	Model         string   // optional single-token model id / family alias
	Queues        []string // Switchboard queues the persona drains
	Triggers      []string // event source bindings for the drop-in
	MCPServers    []string // server names the persona needs (reference only)
	AllowedTools  []string
	MaxRunsPerDay int // one-shots only; 0 means unset. Surfaced in a drop-in only when the binary supports run budgets (ADR-0027 / SPEC-0021).

	// Prompt and System are the raw (unrendered) template texts.
	Prompt string
	System string
}

// raw mirrors persona.toml before validation.
type raw struct {
	Description   string   `toml:"description"`
	Mode          string   `toml:"mode"`
	Client        string   `toml:"client"`
	Model         string   `toml:"model"`
	Queues        []string `toml:"queues"`
	Triggers      []string `toml:"triggers"`
	MCPServers    []string `toml:"mcp_servers"`
	AllowedTools  []string `toml:"allowed_tools"`
	MaxRunsPerDay int      `toml:"max_runs_per_day"`
}

// ValidClients are the client kinds init may write a drop-in for
// (SPEC-0018 REQ-3). pi-omp is the command kind's client; init refuses it on
// binaries without the command kind.
var ValidClients = map[string]bool{
	"crush":       true,
	"claude-code": true,
	"codex":       true,
	"pi-omp":      true,
}

// loadDir validates one template directory: persona.toml parsed and checked,
// prompt.md and system.md read. Every failure names the file and the key and
// wraps a sentinel (REQ-29), and every failure happens HERE — before any
// network call, because this package makes none.
func loadDir(name, source, dir string, fsys fs.FS) (*Template, error) {
	t := &Template{Name: name, Source: source}
	rawToml, err := fs.ReadFile(fsys, joinFS(fsys, dir, "persona.toml"))
	if err != nil {
		return nil, fmt.Errorf("%w: %s/persona.toml is missing: %w", ErrInvalid, dir, err)
	}
	var r raw
	meta, err := toml.Decode(string(rawToml), &r)
	if err != nil {
		return nil, fmt.Errorf("%w: %s/persona.toml: %w", ErrInvalid, dir, err)
	}
	// An unknown key is a typo until proven otherwise: `allowed_tool` would
	// otherwise load as a persona with no tool restriction at all.
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("%w: %s/persona.toml: unknown key %s", ErrInvalid, dir, strings.Join(keys, ", "))
	}
	t.Description = strings.TrimSpace(r.Description)
	t.Mode = Mode(strings.TrimSpace(r.Mode))
	t.Client = strings.TrimSpace(r.Client)
	t.Model = strings.TrimSpace(r.Model)
	t.MaxRunsPerDay = r.MaxRunsPerDay
	for _, l := range []struct {
		key string
		in  []string
		out *[]string
	}{
		{"queues", r.Queues, &t.Queues},
		{"triggers", r.Triggers, &t.Triggers},
		{"mcp_servers", r.MCPServers, &t.MCPServers},
		{"allowed_tools", r.AllowedTools, &t.AllowedTools},
	} {
		for i, v := range l.in {
			v = strings.TrimSpace(v)
			if v == "" {
				return nil, fmt.Errorf("%w: %s/persona.toml: %q entry %d must not be blank", ErrInvalid, dir, l.key, i)
			}
			*l.out = append(*l.out, v)
		}
	}

	if t.Description == "" {
		return nil, fmt.Errorf("%w: %s/persona.toml: \"description\" must not be blank", ErrInvalid, dir)
	}
	switch t.Mode {
	case ModeResident, ModeOneShot:
	case "":
		return nil, fmt.Errorf("%w: %s/persona.toml: \"mode\" is required (resident or one-shot)", ErrInvalid, dir)
	default:
		return nil, fmt.Errorf("%w: %s/persona.toml: unknown \"mode\" %q (want resident or one-shot)", ErrInvalid, dir, t.Mode)
	}
	if !ValidClients[t.Client] {
		return nil, fmt.Errorf("%w: %s/persona.toml: unknown \"client\" %q (want one of: crush, claude-code, codex, pi-omp)", ErrInvalid, dir, t.Client)
	}
	if strings.ContainsFunc(t.Model, func(r rune) bool { return r == ' ' || r == '\t' }) {
		return nil, fmt.Errorf("%w: %s/persona.toml: \"model\" must be a single token (model ids carry no whitespace)", ErrInvalid, dir)
	}
	if t.Mode == ModeOneShot && t.MaxRunsPerDay < 0 {
		return nil, fmt.Errorf("%w: %s/persona.toml: \"max_runs_per_day\" must not be negative", ErrInvalid, dir)
	}
	if t.Mode == ModeResident && t.MaxRunsPerDay != 0 {
		return nil, fmt.Errorf("%w: %s/persona.toml: \"max_runs_per_day\" only applies to mode = \"one-shot\"", ErrInvalid, dir)
	}

	if t.Prompt, err = readNonEmpty(fsys, dir, "prompt.md"); err != nil {
		return nil, err
	}
	if t.System, err = readNonEmpty(fsys, dir, "system.md"); err != nil {
		return nil, err
	}
	return t, nil
}

func readNonEmpty(fsys fs.FS, dir, name string) (string, error) {
	b, err := fs.ReadFile(fsys, joinFS(fsys, dir, name))
	if err != nil {
		return "", fmt.Errorf("%w: %s/%s is missing: %w", ErrInvalid, dir, name, err)
	}
	if strings.TrimSpace(string(b)) == "" {
		return "", fmt.Errorf("%w: %s/%s is empty", ErrInvalid, dir, name)
	}
	return string(b), nil
}

// UserDir is $XDG_CONFIG_HOME/harness/templates (falling back to ~/.config),
// where an operator adds a template or shadows a built-in.
func UserDir() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return filepath.Join(".config", "harness", "templates")
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "harness", "templates")
}

// builtinNames are the built-ins, in a deliberate review-loop order for
// --list-templates: the resident coordinator, the one-shot chain it hands
// work to, and the queue drainer.
var builtinNames = []string{"coordinator", "planner", "implementer", "reviewer", "verifier", "drainer"}

// Load resolves one template by name: the user's templates directory first
// (a user template shadows a built-in of the same name), then the embedded
// built-ins. A malformed template fails HERE, naming the file and the key —
// before any network call (REQ-4 "A malformed template").
func Load(name string) (*Template, error) {
	if strings.TrimSpace(name) == "" || strings.ContainsAny(name, "/\\") {
		return nil, fmt.Errorf("%w: %q is not a template name", ErrNotFound, name)
	}
	userPath := filepath.Join(UserDir(), name)
	if info, statErr := os.Stat(userPath); statErr == nil && info.IsDir() {
		// The directory exists, so ANY load failure is a malformed template,
		// never "not found": a typo in persona.toml must fail here, naming
		// the file and the key (REQ-4 "A malformed template"), not fall
		// through to a built-in of the same name.
		user, err := loadDir(name, userPath, userPath, osFS{})
		if err != nil {
			return nil, err
		}
		user.Shadowed = isBuiltin(name)
		return user, nil
	}
	for _, b := range builtinNames {
		if b == name {
			return loadDir(name, "built-in", "templates/"+name, embedded)
		}
	}
	return nil, fmt.Errorf("%w: %q (looked in %s and the built-ins)", ErrNotFound, name, UserDir())
}

// List returns every template: user templates first, then the built-ins they
// do not shadow (the shadow wins, and its Shadowed flag notes the override).
// Hidden directories (.git, when an operator versions their templates) are
// not templates and are skipped.
func List() ([]*Template, error) {
	var out []*Template
	userSeen := map[string]bool{}
	userDir := UserDir()
	entries, err := os.ReadDir(userDir)
	if err == nil {
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			t, err := loadDir(e.Name(), filepath.Join(userDir, e.Name()), filepath.Join(userDir, e.Name()), osFS{})
			if err != nil {
				return nil, err
			}
			t.Shadowed = isBuiltin(e.Name())
			userSeen[e.Name()] = true
			out = append(out, t)
		}
	}
	for _, b := range builtinNames {
		if userSeen[b] {
			continue
		}
		t, err := loadDir(b, "built-in", "templates/"+b, embedded)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

func isBuiltin(name string) bool {
	for _, b := range builtinNames {
		if b == name {
			return true
		}
	}
	return false
}

// joinFS joins template paths for fsys: embed.FS takes slash paths on every
// OS, while osFS opens real filesystem paths.
func joinFS(fsys fs.FS, elem ...string) string {
	if _, ok := fsys.(osFS); ok {
		return filepath.Join(elem...)
	}
	return path.Join(elem...)
}

// osFS adapts *os.FS-less environments: fs.FS over the real filesystem.
type osFS struct{}

func (osFS) Open(name string) (fs.File, error) { return os.Open(name) }
