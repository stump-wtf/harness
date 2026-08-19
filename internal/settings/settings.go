// Package settings resolves Harness's process-level configuration from four
// sources in one fixed order: an explicit command-line flag, a HARNESS_*
// environment variable, the TOML file, then the compiled default.
//
// Scope is deliberately narrow. This package owns *scalars* — where the socket
// lives, how loud the log is, whether the SSH server is on. It does not touch
// [harness.*] or [profile.*]; those stay with internal/config, which parses them
// through BurntSushi/toml and reports failures as a config.Error carrying a
// source line. SPEC-0001 REQ "Zero And Error States" renders that line in the
// reload banner ("using last-good config; line 12: …"), and Viper cannot produce
// it — Viper lowercases keys, flattens structure, and discards toml.MetaData.
//
// So two readers touch the same file on purpose, each owning different keys.
// That is a seam, not an oversight; see ADR-0016's consequences. Do not "unify"
// it without first solving the line-number problem.
//
// Why not lean on Viper's AutomaticEnv alone: it treats an exported-but-empty
// variable as a value, and it answers for any key ever registered. SPEC-0010
// requires that HARNESS_FOO="" fall through (it is overwhelmingly a shell
// artifact like `export HARNESS_SOCKET=$UNSET_VAR`, never a request for an empty
// socket path) and that the recognized set be an explicit, enumerable table. So
// the environment step is explicit while Viper carries defaults, the file layer,
// and the flag binding.
//
// Governing: ADR-0016 (Cobra for commands, Viper for process config), SPEC-0010
// REQ "Environment Variable Namespace", REQ "Precedence Order", REQ
// "Environment Value Validation", REQ "Source Attribution".
//
// @joestump-agent 08/19/2026 - Introduced with the ADR-0016 environment layer.
package settings

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

// Source names where a resolved value came from. Reported by `harness doctor`
// so "which one won?" is answerable without reading code.
type Source string

const (
	SourceFlag    Source = "flag"
	SourceEnv     Source = "env"
	SourceFile    Source = "file"
	SourceDefault Source = "default"
)

// Kind is a setting's value type. It drives parsing and the error text a bad
// value produces.
type Kind int

const (
	KindString Kind = iota
	KindBool
	KindInt
)

// ErrNoConfigFile reports that no file existed at the resolved path. It is a
// sentinel because SPEC-0010 REQ "Fileless Operation" makes absence non-fatal
// while keeping an unparseable file fatal, and callers must tell those apart
// without string-matching.
var ErrNoConfigFile = errors.New("no config file at the resolved path")

// Setting describes one process-level scalar: how it is spelled as a flag, as an
// environment variable, and as a key in the TOML file.
type Setting struct {
	// Name is the canonical identifier, matching the flag name.
	Name string
	// Env is the full environment variable, e.g. HARNESS_SSH_LISTEN.
	Env string
	// FileKey is the dotted TOML path, e.g. "server.listen". Empty when the
	// setting has no file representation.
	FileKey string
	// Kind drives parsing and the "accepted form" half of an error message.
	Kind Kind
	// Default is the compiled fallback.
	Default any
	// Desc is one line for `harness doctor`.
	Desc string
}

// Registry is the recognized set, in report order. Adding a setting here is the
// only step needed to give it flag/env/file/default resolution and doctor
// reporting.
//
// HARNESS_DETACH_READY_FD is deliberately absent: it is internal IPC between
// `daemon --detach` and its forked child, not operator configuration, and
// SPEC-0010 reserves rather than reuses it.
var Registry = []Setting{
	{Name: "socket", Env: "HARNESS_SOCKET", Kind: KindString, Desc: "daemon socket path"},
	{Name: "config", Env: "HARNESS_CONFIG", Kind: KindString, Desc: "harness.toml path"},
	{Name: "json", Env: "HARNESS_JSON", Kind: KindBool, Default: false, Desc: "machine-readable output"},
	{Name: "log-level", Env: "HARNESS_LOG_LEVEL", FileKey: "daemon.log_level", Kind: KindString, Default: "info", Desc: "log level"},
	{Name: "log-file", Env: "HARNESS_LOG_FILE", FileKey: "daemon.log_file", Kind: KindString, Default: "", Desc: "log file (empty = stderr)"},
	{Name: "scrollback", Env: "HARNESS_SCROLLBACK", FileKey: "daemon.scrollback", Kind: KindInt, Desc: "scrollback ring depth (lines)"},
	{Name: "ssh", Env: "HARNESS_SSH", FileKey: "server.enabled", Kind: KindBool, Default: false, Desc: "remote SSH server enabled"},
	{Name: "ssh-listen", Env: "HARNESS_SSH_LISTEN", FileKey: "server.listen", Kind: KindString, Default: "", Desc: "SSH bind address"},
	{Name: "watch-config", Env: "HARNESS_WATCH_CONFIG", FileKey: "daemon.watch_config", Kind: KindBool, Default: true, Desc: "watch the config file for changes"},
}

// logLevels is the accepted set for log-level, listed in errors so a typo tells
// the operator what to type instead.
var logLevels = []string{"debug", "info", "warn", "error"}

// Resolved is one setting's outcome: the value and which source supplied it.
type Resolved struct {
	Setting Setting
	Value   any
	Source  Source
}

// String renders the value for display. A bool prints as true/false and an unset
// string prints as an empty string rather than "<nil>".
func (r Resolved) String() string {
	if r.Value == nil {
		return ""
	}
	return fmt.Sprint(r.Value)
}

// Resolver walks the precedence ladder. Build one with New, point it at a config
// file, bind any flags, then Resolve.
type Resolver struct {
	v        *viper.Viper
	flags    *pflag.FlagSet
	defaults map[string]any
	fileErr  error
}

// New builds a Resolver seeded with the registry's compiled defaults. Callers
// override a default with SetDefault when it is computed at runtime (the socket
// and config paths depend on the XDG environment, and scrollback comes from the
// attach package).
func New() *Resolver {
	v := viper.New()
	v.SetEnvPrefix("HARNESS")
	v.SetEnvKeyReplacer(strings.NewReplacer("-", "_", ".", "_"))

	r := &Resolver{v: v, defaults: map[string]any{}}
	for _, s := range Registry {
		if s.Default != nil {
			r.defaults[s.Name] = s.Default
		}
	}
	return r
}

// SetDefault overrides a compiled default with a runtime-computed one.
func (r *Resolver) SetDefault(name string, value any) { r.defaults[name] = value }

// ReadConfigFile loads path for its scalar keys only. A missing file yields
// ErrNoConfigFile, which callers treat as non-fatal per SPEC-0010 REQ "Fileless
// Operation"; a present-but-unparseable file yields its parse error.
//
// This reads the same file internal/config reads, for different keys. See the
// package comment.
func (r *Resolver) ReadConfigFile(path string) error {
	if path == "" {
		r.fileErr = ErrNoConfigFile
		return ErrNoConfigFile
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			r.fileErr = ErrNoConfigFile
			return ErrNoConfigFile
		}
		r.fileErr = err
		return err
	}
	r.v.SetConfigFile(path)
	r.v.SetConfigType("toml")
	if err := r.v.ReadInConfig(); err != nil {
		r.fileErr = err
		return fmt.Errorf("read config %s: %w", path, err)
	}
	return nil
}

// BindFlags associates a parsed flag set with the resolver. Only flags the user
// actually typed participate: SPEC-0010 REQ "Precedence Order" ranks a flag
// above the environment on the strength of Changed, never on the flag holding a
// non-zero value. Ranking on "differs from default" would break `--json=false`
// and would silently ignore anyone who passed the default explicitly.
func (r *Resolver) BindFlags(fs *pflag.FlagSet) { r.flags = fs }

// Resolve walks the ladder for one setting.
func (r *Resolver) Resolve(name string) (Resolved, error) {
	s, ok := lookup(name)
	if !ok {
		return Resolved{}, fmt.Errorf("settings: unknown setting %q", name)
	}

	// 1. An explicitly typed flag wins outright.
	if r.flags != nil {
		if f := r.flags.Lookup(s.Name); f != nil && f.Changed {
			v, err := parse(s, f.Value.String(), "--"+s.Name)
			if err != nil {
				return Resolved{}, err
			}
			return Resolved{Setting: s, Value: v, Source: SourceFlag}, nil
		}
	}

	// 2. The environment, treating empty as absent.
	if raw, present := os.LookupEnv(s.Env); present && raw != "" {
		v, err := parse(s, raw, s.Env)
		if err != nil {
			return Resolved{}, err
		}
		return Resolved{Setting: s, Value: v, Source: SourceEnv}, nil
	}

	// 3. The file, for settings that have a representation in it.
	if s.FileKey != "" && r.v.IsSet(s.FileKey) {
		v, err := parse(s, fmt.Sprint(r.v.Get(s.FileKey)), s.FileKey)
		if err != nil {
			return Resolved{}, err
		}
		return Resolved{Setting: s, Value: v, Source: SourceFile}, nil
	}

	// 4. The compiled default.
	return Resolved{Setting: s, Value: r.defaults[s.Name], Source: SourceDefault}, nil
}

// ResolveAll walks every registered setting, in registry order. It returns the
// first error rather than a partial report, because a malformed value must stop
// startup rather than be quietly replaced by a default.
func (r *Resolver) ResolveAll() ([]Resolved, error) {
	out := make([]Resolved, 0, len(Registry))
	for _, s := range Registry {
		got, err := r.Resolve(s.Name)
		if err != nil {
			return nil, err
		}
		out = append(out, got)
	}
	return out, nil
}

// String resolves a string setting, discarding the source.
func (r *Resolver) String(name string) (string, error) {
	got, err := r.Resolve(name)
	if err != nil {
		return "", err
	}
	s, _ := got.Value.(string)
	return s, nil
}

// Bool resolves a bool setting.
func (r *Resolver) Bool(name string) (bool, error) {
	got, err := r.Resolve(name)
	if err != nil {
		return false, err
	}
	b, _ := got.Value.(bool)
	return b, nil
}

// Int resolves an int setting.
func (r *Resolver) Int(name string) (int, error) {
	got, err := r.Resolve(name)
	if err != nil {
		return 0, err
	}
	i, _ := got.Value.(int)
	return i, nil
}

// parse converts a raw string to the setting's type, naming the origin in any
// error so an operator can tell HARNESS_SCROLLBACK=lots from --scrollback=lots.
// SPEC-0010 REQ "Environment Value Validation" forbids coercing or ignoring a
// bad value.
func parse(s Setting, raw, origin string) (any, error) {
	switch s.Kind {
	case KindBool:
		b, err := parseBool(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: invalid value %q: expected a boolean (%s)",
				origin, raw, strings.Join(boolSpellings(), ", "))
		}
		return b, nil

	case KindInt:
		i, err := strconv.Atoi(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: invalid value %q: expected an integer", origin, raw)
		}
		return i, nil

	default:
		if s.Name == "log-level" && !validLogLevel(raw) {
			return nil, fmt.Errorf("%s: invalid value %q: expected one of %s",
				origin, raw, strings.Join(logLevels, ", "))
		}
		return raw, nil
	}
}

// parseBool accepts the spellings SPEC-0010 REQ "Environment Value Validation"
// lists. strconv.ParseBool covers 1/0/true/false/t/f but not yes/no/on/off,
// which are the spellings people actually reach for in a systemd unit.
func parseBool(raw string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "t", "yes", "y", "on":
		return true, nil
	case "0", "false", "f", "no", "n", "off":
		return false, nil
	}
	return false, fmt.Errorf("not a boolean")
}

func boolSpellings() []string {
	return []string{"1", "0", "true", "false", "yes", "no", "on", "off"}
}

func validLogLevel(raw string) bool {
	for _, l := range logLevels {
		if strings.EqualFold(raw, l) {
			return true
		}
	}
	return false
}

func lookup(name string) (Setting, bool) {
	for _, s := range Registry {
		if s.Name == name {
			return s, true
		}
	}
	return Setting{}, false
}

// Names returns the registered setting names, sorted, for help and tests.
func Names() []string {
	out := make([]string, 0, len(Registry))
	for _, s := range Registry {
		out = append(out, s.Name)
	}
	sort.Strings(out)
	return out
}
