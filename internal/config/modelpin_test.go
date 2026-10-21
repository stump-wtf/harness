// Config-load coverage for the model pin table (SPEC-0020 REQ-1, REQ-2,
// REQ-8 scenarios). Every acceptance scenario the issue names is one of
// these cases.

package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/modelpin"
)

func loadPinText(t *testing.T, text string) (*core.Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "harness.toml")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

func TestPinCompleteOpenRouterPin(t *testing.T) {
	cfg, err := loadPinText(t, `
[harness.worker]
harness = "omp"
prompt = "go"
[harness.worker.model_pin]
route = "openrouter"
model = "z-ai/glm-5.3-flash"
providers = ["deepinfra"]
data_policy = "zdr"
`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	pin := cfg.Harnesses["worker"].ModelPin
	if pin == nil || pin.Attest != modelpin.AttestFull || pin.OnMismatch != modelpin.OnMismatchHold {
		t.Fatalf("defaults not applied: %+v", pin)
	}
}

func TestPinAndModelMutuallyExclusive(t *testing.T) {
	_, err := loadPinText(t, `
[harness.worker]
harness = "crush"
prompt = "go"
model = "x"
[harness.worker.model_pin]
route = "openrouter"
model = "z-ai/glm-5.3-flash"
providers = ["deepinfra"]
`)
	if err == nil {
		t.Fatal("model + model_pin loaded")
	}
	if !strings.Contains(err.Error(), "model_pin") || !strings.Contains(err.Error(), "model") {
		t.Fatalf("error must name both keys: %v", err)
	}
}

func TestPinFallbacksRejected(t *testing.T) {
	_, err := loadPinText(t, `
[harness.worker]
harness = "crush"
prompt = "go"
[harness.worker.model_pin]
route = "openrouter"
model = "x"
providers = ["p"]
allow_fallbacks = true
`)
	if err == nil || !strings.Contains(err.Error(), "not a pin") {
		t.Fatalf("allow_fallbacks = true loaded: %v", err)
	}
	var cfgErr *Error
	if !errors.As(err, &cfgErr) || cfgErr.Line == 0 {
		t.Fatalf("error must be located: %v", err)
	}
}

func TestPinAutoRouterRejected(t *testing.T) {
	_, err := loadPinText(t, `
[harness.worker]
harness = "crush"
prompt = "go"
[harness.worker.model_pin]
route = "openrouter"
model = "openrouter/auto"
providers = ["openrouter"]
`)
	if err == nil || !strings.Contains(err.Error(), "one model") {
		t.Fatalf("auto-router accepted: %v", err)
	}
}

func TestPinAnthropicZDRRejected(t *testing.T) {
	_, err := loadPinText(t, `
[harness.worker]
harness = "claude-code"
prompt = "go"
[harness.worker.model_pin]
route = "anthropic"
model = "claude-opus-5"
data_policy = "zdr"
`)
	if err == nil || !strings.Contains(err.Error(), "cannot be carried") {
		t.Fatalf("zdr on anthropic accepted: %v", err)
	}
}

func TestPinCodexRejected(t *testing.T) {
	_, err := loadPinText(t, `
[harness.worker]
harness = "codex"
prompt = "go"
[harness.worker.model_pin]
route = "openrouter"
model = "x"
providers = ["p"]
attest = "model"
`)
	if err == nil || !strings.Contains(err.Error(), "codex") || !strings.Contains(err.Error(), "no model-pin route") {
		t.Fatalf("codex pin accepted or misworded: %v", err)
	}
	if !errors.Is(err, modelpin.ErrRouteNotOffered) {
		t.Fatalf("sentinel lost: %v", err)
	}
}

func TestPinClaudeCodeOpenRouterListsAlternatives(t *testing.T) {
	_, err := loadPinText(t, `
[harness.worker]
harness = "claude-code"
prompt = "go"
[harness.worker.model_pin]
route = "openrouter"
model = "x"
providers = ["p"]
attest = "model"
`)
	if err == nil || !strings.Contains(err.Error(), "anthropic") || !strings.Contains(err.Error(), "litellm") {
		t.Fatalf("offered alternatives missing: %v", err)
	}
}

func TestPinCommandRequiresTranscripts(t *testing.T) {
	_, err := loadPinText(t, `
[harness.worker]
harness = "command"
argv = ["myagent", "{{model}}"]
[harness.worker.model_pin]
route = "anthropic"
model = "claude-opus-5"
attest = "model"
`)
	if err == nil || !strings.Contains(err.Error(), "transcripts") {
		t.Fatalf("command pin without transcripts accepted: %v", err)
	}
}

// REQ-8: the gap is driven by the capability table, not per-client tests:
// crush's OpenRouter transcripts evidence model only, so the default
// attest = "full" fails offering attest = "model"; the same pin on omp,
// whose transcripts record the provider, loads.
func TestPinCapabilityGapFromTable(t *testing.T) {
	pin := `
[harness.%s.model_pin]
route = "openrouter"
model = "z-ai/glm-5.3-flash"
providers = ["deepinfra"]
`
	_, err := loadPinText(t, "[harness.crushh]\nharness = \"crush\"\nprompt = \"go\"\n"+strings.Replace(pin, "%s", "crushh", 1))
	if err == nil || !strings.Contains(err.Error(), "attest = \"model\"") {
		t.Fatalf("capability gap not offered: %v", err)
	}
	if !errors.Is(err, modelpin.ErrCapabilityGap) {
		t.Fatalf("sentinel lost: %v", err)
	}
	if _, err := loadPinText(t, "[harness.omph]\nharness = \"omp\"\nprompt = \"go\"\n"+strings.Replace(pin, "%s", "omph", 1)); err != nil {
		t.Fatalf("omp full attestation rejected: %v", err)
	}
}

// A pin is valid on a resident harness, and model_pin alone (no model, no
// prompt requirement) must not borrow the `model` prompt rule.
func TestPinOnResidentHarness(t *testing.T) {
	_, err := loadPinText(t, `
[harness.worker]
harness = "crush"
args = ["crush"]
[harness.worker.model_pin]
route = "litellm"
model = "x"
gateway_model = "glm-pin"
gateway_url = "http://127.0.0.1:4000"
attest = "model"
`)
	if err != nil {
		t.Fatalf("resident pin rejected: %v", err)
	}
}
