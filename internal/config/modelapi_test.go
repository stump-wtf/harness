package config

// Model API Table Tests
//
// Governing tests: ADR-0036 and #837 — the section parses, a literal
// api_key is refused (naming the key, never the value), the ${NAME}
// reference resolves from the table's own env_file in the daemon's load
// only, and neither a project file nor a harness_d drop-in may carry the
// table.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stump-wtf/harness/internal/core"
)

func TestModelAPIAbsentIsZero(t *testing.T) {
	cfg, err := Parse([]byte("[harness.a]\nharness = \"crush\"\n"), "t.toml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ModelAPI != (core.ModelAPIConfig{}) {
		t.Fatalf("absent [model_api] parsed as %+v", cfg.ModelAPI)
	}
}

func TestModelAPIParses(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "model-api.env")
	if err := os.WriteFile(envFile, []byte("MODEL_API_KEY=sk-test-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(dir, "harness.toml")
	src := `
[model_api]
base_url = "http://127.0.0.1:4000/v1"
env_file = "model-api.env"
api_key = "${MODEL_API_KEY}"
chat_model = "gpt-oss-120b"
utility_model = "qwen3-30b-a3b"
embedding_model = "bge-m3"
`
	cfg, err := Parse([]byte(src), main)
	if err != nil {
		t.Fatal(err)
	}
	m := cfg.ModelAPI
	if m.BaseURL != "http://127.0.0.1:4000/v1" || m.EnvFile != envFile ||
		m.APIKeyRef != "${MODEL_API_KEY}" || m.ChatModel != "gpt-oss-120b" ||
		m.UtilityModel != "qwen3-30b-a3b" || m.EmbeddingModel != "bge-m3" {
		t.Fatalf("parsed = %+v", m)
	}
	// A CLI load checks the reference's shape only: the credential stays
	// unresolved.
	if !m.APIKey.Empty() {
		t.Fatal("Parse resolved the api key; only the daemon's load may")
	}
}

func TestModelAPILiteralKeyRefused(t *testing.T) {
	src := `
[model_api]
base_url = "http://127.0.0.1:4000/v1"
env_file = "model-api.env"
api_key = "sk-live-123"
`
	_, err := Parse([]byte(src), "t.toml")
	if err == nil {
		t.Fatal("literal api_key parsed")
	}
	if !strings.Contains(err.Error(), "api_key") {
		t.Fatalf("err does not name api_key: %v", err)
	}
	// The whole point of the refusal is that this string is a credential:
	// the error must never echo it.
	if strings.Contains(err.Error(), "sk-live-123") {
		t.Fatalf("err leaks the key: %v", err)
	}
	if !errors.Is(err, ErrLiteralSecret) {
		t.Fatalf("err = %v, want errors.Is ErrLiteralSecret", err)
	}
}

func TestModelAPIKeyWithoutEnvFile(t *testing.T) {
	_, err := Parse([]byte(`
[model_api]
base_url = "http://127.0.0.1:4000/v1"
api_key = "${MODEL_API_KEY}"
`), "t.toml")
	if err == nil || !strings.Contains(err.Error(), "env_file") {
		t.Fatalf("api_key without env_file err = %v, want a refusal naming env_file", err)
	}
}

func TestModelAPIRejectsBadBaseURL(t *testing.T) {
	for _, base := range []string{"", "not-a-url", "ftp://example.invalid/v1"} {
		src := "[model_api]\nbase_url = \"" + base + "\"\n"
		_, err := Parse([]byte(src), "t.toml")
		if err == nil || !strings.Contains(err.Error(), "base_url") {
			t.Fatalf("base_url %q err = %v, want a refusal naming base_url", base, err)
		}
	}
}

func TestModelAPIDuplicateTable(t *testing.T) {
	_, err := Parse([]byte("[model_api]\nbase_url = \"http://x/v1\"\n[model_api]\nbase_url = \"http://y/v1\"\n"), "t.toml")
	// TOML itself refuses a redefined table; the switch's duplicate guard
	// is defense in depth. Either way the error must name model_api.
	if err == nil || !strings.Contains(err.Error(), "model_api") {
		t.Fatalf("duplicate [model_api] err = %v, want a refusal", err)
	}
}

// TestModelAPIDaemonResolveFromEnvFile covers the daemon's load step: the
// reference resolves from the table's own env_file, a name the file lacks
// fails naming the reference, and an empty value is refused.
func TestModelAPIDaemonResolveFromEnvFile(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "model-api.env")
	if err := os.WriteFile(envFile, []byte("MODEL_API_KEY=sk-test-1\nOTHER=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := `
[model_api]
base_url = "http://127.0.0.1:4000/v1"
env_file = "` + envFile + `"
api_key = "${MODEL_API_KEY}"
`
	cfg, err := Parse([]byte(src), "t.toml")
	if err != nil {
		t.Fatal(err)
	}
	if err := ResolveModelAPIKey(cfg); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := cfg.ModelAPI.APIKey.Reveal(); got != "sk-test-1" {
		t.Fatalf("resolved key = %q", got)
	}
}

func TestModelAPIDaemonResolveMissingName(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "model-api.env")
	if err := os.WriteFile(envFile, []byte("OTHER=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Parse([]byte(`
[model_api]
base_url = "http://127.0.0.1:4000/v1"
env_file = "`+envFile+`"
api_key = "${MODEL_API_KEY}"
`), "t.toml")
	if err != nil {
		t.Fatal(err)
	}
	err = ResolveModelAPIKey(cfg)
	if err == nil || !strings.Contains(err.Error(), "MODEL_API_KEY") {
		t.Fatalf("missing name err = %v, want the reference named", err)
	}
}

func TestModelAPIDaemonResolveEmptyValue(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "model-api.env")
	if err := os.WriteFile(envFile, []byte("MODEL_API_KEY=\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Parse([]byte(`
[model_api]
base_url = "http://127.0.0.1:4000/v1"
env_file = "`+envFile+`"
api_key = "${MODEL_API_KEY}"
`), "t.toml")
	if err != nil {
		t.Fatal(err)
	}
	if err := ResolveModelAPIKey(cfg); err == nil {
		t.Fatal("empty resolved key accepted")
	}
}

// TestModelAPIResolveNoopWithoutTable keeps a credential-less daemon
// startable: no table, no resolution, no error.
func TestModelAPIResolveNoopWithoutTable(t *testing.T) {
	cfg, err := Parse([]byte("[harness.a]\nharness = \"crush\"\n"), "t.toml")
	if err != nil {
		t.Fatal(err)
	}
	if err := ResolveModelAPIKey(cfg); err != nil {
		t.Fatalf("resolve with no table: %v", err)
	}
}

func TestModelAPINotInProjectFile(t *testing.T) {
	_, err := ParseProject([]byte("[model_api]\nbase_url = \"http://127.0.0.1:4000/v1\"\n"), "harness.toml")
	if err == nil || !strings.Contains(err.Error(), "model_api") {
		t.Fatalf("project [model_api] err = %v, want a refusal", err)
	}
}

func TestModelAPINotInDropIn(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "harness.toml")
	dropins := filepath.Join(dir, "harness.d")
	if err := os.MkdirAll(dropins, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(main, []byte("[server]\nharness_d = \"harness.d\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dropins, "a.toml"), []byte("[model_api]\nbase_url = \"http://127.0.0.1:4000/v1\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(main); err == nil || !strings.Contains(err.Error(), "[model_api]") {
		t.Fatalf("drop-in [model_api] err = %v, want a refusal", err)
	}
}

// TestModelAPIADR0036ExampleLoads parses the configuration block ADR-0036
// publishes, so an operator who copies it keeps the whole config.
func TestModelAPIADR0036ExampleLoads(t *testing.T) {
	src := `
[model_api]
base_url        = "http://127.0.0.1:4000/v1"
env_file        = "model-api.env"
api_key         = "${MODEL_API_KEY}"
chat_model      = "gpt-oss-120b"
utility_model   = "qwen3-30b-a3b"
embedding_model = "bge-m3"
`
	cfg, err := Parse([]byte(src), "t.toml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ModelAPI.ChatModel != "gpt-oss-120b" || cfg.ModelAPI.UtilityModel != "qwen3-30b-a3b" ||
		cfg.ModelAPI.EmbeddingModel != "bge-m3" {
		t.Fatalf("ADR-0036 example parsed = %+v", cfg.ModelAPI)
	}
}
