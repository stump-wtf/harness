package core

// Model API Table
//
// The parsed [model_api] table (ADR-0036): the daemon's one OpenAI-compatible
// endpoint and the model names the daemon's utility and gate calls use. The
// credential never appears in harness.toml: api_key is a ${NAME} reference
// resolved from the table's own env_file, and only by the daemon's load — a
// CLI load checks the reference's shape and stops there, so `harness doctor`
// and friends never read the key (ADR-0036; ADR-0038's single secret
// mechanism).
//
// Governing: ADR-0036; ADR-0038.

// ModelAPIConfig is the global [model_api] table; the zero value is absent
// and means the daemon makes no model calls.
type ModelAPIConfig struct {
	// BaseURL is the http/https API root (typically a /v1 root); the client
	// appends /chat/completions to it.
	BaseURL string
	// EnvFile is the file api_key's ${NAME} resolves from, resolved against
	// the config file's own directory like every env_file in the config.
	EnvFile string
	// APIKeyRef is the api_key exactly as written: one ${NAME} reference.
	// A literal is refused at parse, so this is always a reference shape
	// once the config loads.
	APIKeyRef string
	// APIKey is the resolved credential. It is empty until the daemon's
	// load calls config.ResolveModelAPIKey; a CLI load leaves it empty.
	APIKey Secret
	// ChatModel is the default conversational model name.
	ChatModel string
	// UtilityModel is the small, cheap model for daemon utility calls.
	UtilityModel string
	// EmbeddingModel names a concrete embedding model (not an alias), part
	// of the embedding cache key.
	EmbeddingModel string
}
