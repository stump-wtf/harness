package settings

// Precedence Ladder Tests
//
// SPEC-0010 states the ladder as one table, so it is tested as one table. Each
// case sets up some combination of flag / environment / file / default and
// asserts both the winning value AND the reported source — a test that only
// checked the value would pass while doctor lied about where it came from.
//
// Governing: ADR-0016, SPEC-0010 REQ "Precedence Order", REQ "Environment
// Variable Namespace", REQ "Environment Value Validation", REQ "Fileless
// Operation".
//
// @joestump-agent 08/19/2026 - Introduced with the ADR-0016 environment layer.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

// newTestFlags builds a flag set matching the real one closely enough for the
// ladder, and marks the named flags as explicitly typed.
func newTestFlags(t *testing.T, changed map[string]string) *pflag.FlagSet {
	t.Helper()
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	fs.String("socket", "", "")
	fs.String("config", "", "")
	fs.Bool("json", false, "")
	fs.String("log-level", "info", "")
	fs.String("log-file", "", "")
	fs.Int("scrollback", 0, "")
	fs.Bool("ssh", false, "")
	fs.String("ssh-listen", "", "")
	fs.Bool("watch-config", true, "")

	for name, val := range changed {
		if err := fs.Set(name, val); err != nil {
			t.Fatalf("set --%s=%s: %v", name, val, err)
		}
	}
	return fs
}

// writeConfig drops a TOML file carrying the given body and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "harness.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPrecedenceLadder(t *testing.T) {
	const cfgBody = `
[daemon]
log_level = "warn"
watch_config = false

[server]
enabled = false
listen = "127.0.0.1:2222"
`

	tests := []struct {
		name       string
		setting    string
		flags      map[string]string
		env        map[string]string
		withFile   bool
		wantValue  any
		wantSource Source
	}{
		{
			name:       "flag beats environment",
			setting:    "socket",
			flags:      map[string]string{"socket": "/tmp/y.sock"},
			env:        map[string]string{"HARNESS_SOCKET": "/tmp/x.sock"},
			wantValue:  "/tmp/y.sock",
			wantSource: SourceFlag,
		},
		{
			name:       "environment beats file",
			setting:    "log-level",
			env:        map[string]string{"HARNESS_LOG_LEVEL": "debug"},
			withFile:   true,
			wantValue:  "debug",
			wantSource: SourceEnv,
		},
		{
			name:       "file beats default",
			setting:    "ssh-listen",
			withFile:   true,
			wantValue:  "127.0.0.1:2222",
			wantSource: SourceFile,
		},
		{
			name:       "default when nothing else supplies it",
			setting:    "log-level",
			wantValue:  "info",
			wantSource: SourceDefault,
		},
		{
			name:       "environment beats file for booleans",
			setting:    "watch-config",
			env:        map[string]string{"HARNESS_WATCH_CONFIG": "1"},
			withFile:   true,
			wantValue:  true,
			wantSource: SourceEnv,
		},
		{
			name:       "file supplies a false boolean",
			setting:    "watch-config",
			withFile:   true,
			wantValue:  false,
			wantSource: SourceFile,
		},
		{
			// The trap this guards: ranking on "value differs from default"
			// instead of on Changed would let an untyped flag mask the
			// environment, and would break --json=false entirely.
			name:       "unchanged flag does not mask the environment",
			setting:    "log-level",
			env:        map[string]string{"HARNESS_LOG_LEVEL": "debug"},
			wantValue:  "debug",
			wantSource: SourceEnv,
		},
		{
			// An exported-but-empty variable is a shell artifact
			// (export HARNESS_SOCKET=$UNSET), never a request for an empty path.
			name:       "empty environment variable falls through",
			setting:    "log-level",
			env:        map[string]string{"HARNESS_LOG_LEVEL": ""},
			withFile:   true,
			wantValue:  "warn",
			wantSource: SourceFile,
		},
		{
			name:       "explicitly typed false flag still wins",
			setting:    "json",
			flags:      map[string]string{"json": "false"},
			env:        map[string]string{"HARNESS_JSON": "true"},
			wantValue:  false,
			wantSource: SourceFlag,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			r := New()
			r.BindFlags(newTestFlags(t, tc.flags))
			if tc.withFile {
				if err := r.ReadConfigFile(writeConfig(t, cfgBody)); err != nil {
					t.Fatalf("read config: %v", err)
				}
			}

			got, err := r.Resolve(tc.setting)
			if err != nil {
				t.Fatalf("resolve %s: %v", tc.setting, err)
			}
			if got.Value != tc.wantValue {
				t.Errorf("value = %#v, want %#v", got.Value, tc.wantValue)
			}
			if got.Source != tc.wantSource {
				t.Errorf("source = %q, want %q", got.Source, tc.wantSource)
			}
		})
	}
}

// TestInvalidValuesAreFatal pins SPEC-0010 REQ "Environment Value Validation":
// a bad value must fail loudly, naming the variable, the value, and the accepted
// form. Silently falling back to the default is the failure mode that makes an
// operator think their configuration took effect when it did not.
func TestInvalidValuesAreFatal(t *testing.T) {
	tests := []struct {
		name        string
		env         string
		value       string
		setting     string
		wantsInText []string
	}{
		{
			name:        "non-numeric scrollback",
			env:         "HARNESS_SCROLLBACK",
			value:       "lots",
			setting:     "scrollback",
			wantsInText: []string{"HARNESS_SCROLLBACK", "lots", "integer"},
		},
		{
			name:        "unknown log level",
			env:         "HARNESS_LOG_LEVEL",
			value:       "verbose",
			setting:     "log-level",
			wantsInText: []string{"HARNESS_LOG_LEVEL", "verbose", "debug", "info", "warn", "error"},
		},
		{
			name:        "non-boolean ssh",
			env:         "HARNESS_SSH",
			value:       "maybe",
			setting:     "ssh",
			wantsInText: []string{"HARNESS_SSH", "maybe", "boolean"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.env, tc.value)

			r := New()
			r.BindFlags(newTestFlags(t, nil))

			_, err := r.Resolve(tc.setting)
			if err == nil {
				t.Fatalf("resolve %s with %s=%s = nil error, want failure", tc.setting, tc.env, tc.value)
			}
			for _, want := range tc.wantsInText {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

// TestBooleanSpellings pins the accepted set. strconv.ParseBool covers
// 1/0/true/false but not yes/no/on/off, which are what people actually write in
// a systemd unit.
func TestBooleanSpellings(t *testing.T) {
	truthy := []string{"1", "true", "TRUE", "t", "yes", "Y", "on", "ON"}
	falsy := []string{"0", "false", "FALSE", "f", "no", "N", "off", "OFF"}

	for _, raw := range truthy {
		t.Run("true/"+raw, func(t *testing.T) {
			t.Setenv("HARNESS_SSH", raw)
			r := New()
			r.BindFlags(newTestFlags(t, nil))
			got, err := r.Bool("ssh")
			if err != nil {
				t.Fatalf("HARNESS_SSH=%s: %v", raw, err)
			}
			if !got {
				t.Errorf("HARNESS_SSH=%s resolved false, want true", raw)
			}
		})
	}
	for _, raw := range falsy {
		t.Run("false/"+raw, func(t *testing.T) {
			t.Setenv("HARNESS_SSH", raw)
			r := New()
			r.BindFlags(newTestFlags(t, nil))
			got, err := r.Bool("ssh")
			if err != nil {
				t.Fatalf("HARNESS_SSH=%s: %v", raw, err)
			}
			if got {
				t.Errorf("HARNESS_SSH=%s resolved true, want false", raw)
			}
		})
	}
}

// TestMissingConfigFileIsSentinel pins SPEC-0010 REQ "Fileless Operation": an
// absent file is a distinguishable, non-fatal condition, so a container with no
// TOML can come up on environment variables alone. An unparseable file stays
// fatal, and the two must be tellable apart without string-matching.
func TestMissingConfigFileIsSentinel(t *testing.T) {
	r := New()
	err := r.ReadConfigFile(filepath.Join(t.TempDir(), "absent.toml"))
	if !errors.Is(err, ErrNoConfigFile) {
		t.Fatalf("absent file error = %v, want ErrNoConfigFile", err)
	}

	bad := writeConfig(t, "this is not = valid toml [[[")
	err = New().ReadConfigFile(bad)
	if err == nil {
		t.Fatal("unparseable file returned nil error")
	}
	if errors.Is(err, ErrNoConfigFile) {
		t.Error("unparseable file reported as absent; the two must be distinguishable")
	}
}

// TestResolveAllCoversRegistry guards against a setting being added to the
// registry but never resolvable — which would make it silently inert and, worse,
// silently absent from doctor's report.
func TestResolveAllCoversRegistry(t *testing.T) {
	r := New()
	r.BindFlags(newTestFlags(t, nil))

	got, err := r.ResolveAll()
	if err != nil {
		t.Fatalf("resolve all: %v", err)
	}
	if len(got) != len(Registry) {
		t.Fatalf("resolved %d settings, registry has %d", len(got), len(Registry))
	}
	for _, res := range got {
		if res.Setting.Env == "" {
			t.Errorf("setting %q has no environment variable", res.Setting.Name)
		}
		if !strings.HasPrefix(res.Setting.Env, "HARNESS_") {
			t.Errorf("setting %q env %q is outside the HARNESS_ namespace", res.Setting.Name, res.Setting.Env)
		}
	}
}

// TestReservedNameIsNotASetting pins SPEC-0010: HARNESS_DETACH_READY_FD is
// internal IPC between `daemon --detach` and its child. It predates this layer,
// it is not operator configuration, and it must never be reassigned or reported
// as a setting.
func TestReservedNameIsNotASetting(t *testing.T) {
	for _, s := range Registry {
		if s.Env == "HARNESS_DETACH_READY_FD" {
			t.Fatal("HARNESS_DETACH_READY_FD is reserved internal IPC, not a setting")
		}
	}
}

// TestNoCredentialSettings pins SPEC-0010 REQ "Secrets Exclusion". ADR-0008 puts
// credentials in env_file; this namespace must not become a second, more
// tempting home for them. The check is a name heuristic, which is exactly the
// point — it fires on the next person who adds HARNESS_API_TOKEN.
func TestNoCredentialSettings(t *testing.T) {
	banned := []string{"token", "secret", "password", "passwd", "key", "credential", "auth"}
	for _, s := range Registry {
		lower := strings.ToLower(s.Name)
		for _, b := range banned {
			if strings.Contains(lower, b) {
				t.Errorf("setting %q looks like a credential; secrets belong in env_file per ADR-0008", s.Name)
			}
		}
	}
}
