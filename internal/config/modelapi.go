package config

// Model API Table
//
// Parses and validates the global [model_api] table (ADR-0036) into
// core.ModelAPIConfig. The api_key is never a value in harness.toml: it must
// be exactly one ${NAME} reference, and only the daemon's load resolves it,
// from the table's own env_file and nowhere else (ADR-0038). A CLI load
// checks the reference's shape and stops there, so no client command ever
// reads the credential.
//
// The table is global-only: it names the daemon's model endpoint and holds
// its credential, so a project file or harness_d drop-in must not be able to
// aim the daemon at a model API of its choosing.
//
// Governing: ADR-0036; ADR-0038; ADR-0042.

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/stump-wtf/harness/internal/core"
)

// rawModelAPI mirrors [model_api] before validation.
type rawModelAPI struct {
	BaseURL       string `toml:"base_url"`
	EnvFile       string `toml:"env_file"`
	APIKey        string `toml:"api_key"`
	ChatModel     string `toml:"chat_model"`
	UtilityModel  string `toml:"utility_model"`
	EmbeddingName string `toml:"embedding_model"`
}

// buildModelAPI validates rm into a core.ModelAPIConfig. It checks the
// api_key reference's shape only; ResolveModelAPIKey does the daemon-side
// expansion from env_file.
func buildModelAPI(filename string, data []byte, line int, rm rawModelAPI) (core.ModelAPIConfig, error) {
	keyLine := func(key string) int {
		if l := lineOfKeyInTable(data, "model_api", key); l > 0 {
			return l
		}
		return line
	}
	fail := func(key, format string, args ...any) (core.ModelAPIConfig, error) {
		return core.ModelAPIConfig{}, newError(filename, keyLine(key), "[model_api] %q: "+format, append([]any{key}, args...)...)
	}

	mc := core.ModelAPIConfig{
		ChatModel:      strings.TrimSpace(rm.ChatModel),
		UtilityModel:   strings.TrimSpace(rm.UtilityModel),
		EmbeddingModel: strings.TrimSpace(rm.EmbeddingName),
	}

	baseURL := strings.TrimSpace(rm.BaseURL)
	if baseURL == "" {
		return fail("base_url", "is required — [model_api] names the endpoint every daemon model call uses (an http/https root, e.g. \"http://127.0.0.1:4000/v1\")")
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fail("base_url", "must be an absolute http or https URL (got %q)", rm.BaseURL)
	}
	mc.BaseURL = baseURL

	envFile := strings.TrimSpace(rm.EnvFile)
	if rm.EnvFile != "" && envFile == "" {
		return fail("env_file", "must not be blank")
	}
	if envFile != "" {
		mc.EnvFile = resolveConfigPath(envFile, filename)
	}

	apiKeyRef := strings.TrimSpace(rm.APIKey)
	switch {
	case rm.APIKey != "" && apiKeyRef == "":
		return fail("api_key", "must not be blank")
	case apiKeyRef == "":
		// An endpoint with no credential is legitimate (a local gateway).
	case !isSoleRef(apiKeyRef):
		// The message must not echo the value: the whole point of failing
		// here is that this string may be a credential.
		// Governing: ADR-0036; ADR-0038.
		return core.ModelAPIConfig{}, newSentinelError(filename, keyLine("api_key"), ErrLiteralSecret,
			"[model_api] \"api_key\" must be exactly one reference to a name in env_file, e.g. api_key = \"${MODEL_API_KEY}\" — harness.toml is routinely committed, so a literal value is refused")
	case mc.EnvFile == "":
		return fail("api_key", "requires \"env_file\" (the file %s resolves from)", apiKeyRef)
	default:
		mc.APIKeyRef = apiKeyRef
	}
	return mc, nil
}

// ResolveModelAPIKey resolves cfg.ModelAPI.APIKeyRef from the table's own
// env_file into cfg.ModelAPI.APIKey. It is the daemon's load step: a CLI load
// never calls it, so only the daemon holds the credential (ADR-0036). A
// missing name fails the daemon's load naming the reference, never a value.
func ResolveModelAPIKey(cfg *core.Config) error {
	m := cfg.ModelAPI
	if m.APIKeyRef == "" {
		return nil
	}
	secret, err := newEnvResolver(m.EnvFile).expand(m.APIKeyRef)
	if err != nil {
		return err
	}
	if secret.Empty() {
		// Names the reference and the file, never the value.
		return fmt.Errorf("[model_api] \"api_key\" %s resolves to an empty value in env_file %q (an empty key authenticates nothing)", m.APIKeyRef, m.EnvFile)
	}
	cfg.ModelAPI.APIKey = secret
	return nil
}
