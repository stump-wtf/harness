package core

// Telemetry Configuration
//
// The parsed global [telemetry] table: which export destinations the operator
// consented to, which harnesses contribute by default, and the tuning of the
// queues behind them. It lives in core, beside Config, so the config parser
// can fill it and the daemon and internal/telemetry can read it without either
// importing the other.
//
// Nothing here resolves the environment. Endpoints and headers may also come
// from the standard OTEL_EXPORTER_OTLP_* variables and from EnvFile, and that
// resolution belongs to the running daemon (telemetry.Resolve), not to a file a
// TUI edit form round-trips. Compression and Timeout are therefore left at
// their zero value when the table does not set them, so the resolver can say
// whether a setting came from the file or from its default.
//
// Governing: ADR-0022; SPEC-0015 REQ-1, REQ-2.
//
// @joestump-agent 09/21/2026 - Added for harness#391.

import "time"

// Telemetry defaults (SPEC-0015 REQ-2).
const (
	DefaultTelemetryTimeout         = 10 * time.Second
	DefaultTelemetryQueueSize       = 2048
	DefaultTelemetryBatchSize       = 512
	DefaultTelemetryBatchInterval   = 5 * time.Second
	DefaultTelemetryIdleFlush       = 5 * time.Minute
	DefaultTelemetryShutdownTimeout = 5 * time.Second
	DefaultTelemetryEventsFileMaxMB = 100
	DefaultTelemetryEventsFileKeep  = 5
)

// TelemetryConfig is the global [telemetry] table.
type TelemetryConfig struct {
	// Logs, Traces and EventsFile are the three destinations. At least one
	// must be on for anything to be exported; environment variables alone
	// never enable a signal (SPEC-0015 REQ-1).
	Logs   bool
	Traces bool
	// EventsFile is the resolved path of the local JSONL sink; empty is off.
	EventsFile string

	// ExportAll makes every harness without an explicit export_telemetry
	// contribute.
	ExportAll bool
	// OmitPrompts replaces user-message text with a placeholder and drops the
	// session title from everything exported.
	OmitPrompts bool

	// Endpoint is the OTLP/HTTP base URL from the file; /v1/logs and
	// /v1/traces are appended. Empty leaves it to the environment.
	Endpoint string
	// EnvFile is the resolved path of a file supplying OTEL_EXPORTER_OTLP_*
	// values — the collector credential's home, since harness.toml carries
	// no headers (ADR-0008).
	EnvFile string
	// Compression is "none", "gzip", or "" when the file does not set it.
	Compression string
	// Timeout bounds one export request; zero when the file does not set it.
	Timeout time.Duration

	// Tuning; the parser fills every one of these with its default.
	QueueSize       int
	BatchSize       int
	BatchInterval   time.Duration
	IdleFlush       time.Duration
	ShutdownTimeout time.Duration
	EventsFileMaxMB int
	EventsFileKeep  int
}

// DefaultTelemetryConfig is the table with every tuning default filled in and
// every destination off — what an absent [telemetry] table means.
func DefaultTelemetryConfig() TelemetryConfig {
	return TelemetryConfig{
		QueueSize:       DefaultTelemetryQueueSize,
		BatchSize:       DefaultTelemetryBatchSize,
		BatchInterval:   DefaultTelemetryBatchInterval,
		IdleFlush:       DefaultTelemetryIdleFlush,
		ShutdownTimeout: DefaultTelemetryShutdownTimeout,
		EventsFileMaxMB: DefaultTelemetryEventsFileMaxMB,
		EventsFileKeep:  DefaultTelemetryEventsFileKeep,
	}
}

// WithDefaults fills every zero tuning field with its default, so a
// hand-built config (a test, the fileless daemon) behaves like a parsed one.
func (t TelemetryConfig) WithDefaults() TelemetryConfig {
	d := DefaultTelemetryConfig()
	if t.QueueSize <= 0 {
		t.QueueSize = d.QueueSize
	}
	if t.BatchSize <= 0 {
		t.BatchSize = d.BatchSize
	}
	if t.BatchSize > t.QueueSize {
		t.BatchSize = t.QueueSize
	}
	if t.BatchInterval <= 0 {
		t.BatchInterval = d.BatchInterval
	}
	if t.IdleFlush <= 0 {
		t.IdleFlush = d.IdleFlush
	}
	if t.ShutdownTimeout <= 0 {
		t.ShutdownTimeout = d.ShutdownTimeout
	}
	if t.EventsFileMaxMB <= 0 {
		t.EventsFileMaxMB = d.EventsFileMaxMB
	}
	if t.EventsFileKeep <= 0 {
		t.EventsFileKeep = d.EventsFileKeep
	}
	return t
}

// Enabled reports whether any destination is configured — the first of the
// two consents SPEC-0015 REQ-1 requires.
func (t TelemetryConfig) Enabled() bool {
	return t.Logs || t.Traces || t.EventsFile != ""
}

// OTLPEnabled reports whether either network signal is on.
func (t TelemetryConfig) OTLPEnabled() bool { return t.Logs || t.Traces }

// ContributesTelemetry is the second consent: whether h's items may be
// exported. An explicit per-harness value wins; unset follows exportAll.
//
//	export_telemetry | export_all | contributes
//	false            | any        | no
//	true             | any        | yes
//	unset            | true       | yes
//	unset            | false      | no
//
// Governing: SPEC-0015 REQ-1.
func ContributesTelemetry(h Harness, exportAll bool) bool {
	if h.ExportTelemetry != nil {
		return *h.ExportTelemetry
	}
	return exportAll
}
