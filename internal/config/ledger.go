package config

// Ledger Table
//
// Parses the global [ledger] table (SPEC-0022 REQ-19) into core.LedgerConfig:
// trace_url, retention (at least 1d, default 90d) and max_mb (at least 16,
// default 256).
// It is global-only: the run ledger is one per daemon, so a project file that
// carries it is refused (project.go), and a harness_d drop-in refuses it with
// every other table it does not know.
//
// trace_url is a link template with exactly one substitution, {trace_id}
// (REQ-9). Any other placeholder fails the load, naming it: a template that
// silently rendered "{harness}" into every link would be broken in a way no
// one notices until they click one.
//
// Governing: ADR-0028; SPEC-0022 REQ-9, REQ-19.
//
// @joestump 09/24/2026 - Added for harness#459 (trace_url).

import (
	"regexp"
	"time"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/ledger"
)

// rawLedger mirrors the [ledger] table before validation.
type rawLedger struct {
	TraceURL  string `toml:"trace_url"`
	Retention string `toml:"retention"`
	MaxMB     *int   `toml:"max_mb"`
}

// placeholderRE finds {…} placeholders in a template.
var placeholderRE = regexp.MustCompile(`\{[^{}]*\}`)

// buildLedger validates a [ledger] table.
func buildLedger(filename string, line int, rl rawLedger) (core.LedgerConfig, error) {
	for _, ph := range placeholderRE.FindAllString(rl.TraceURL, -1) {
		if ph != ledger.TraceIDPlaceholder {
			return core.LedgerConfig{}, newError(filename, line,
				"[ledger] trace_url: unsupported placeholder %s (the only substitution is %s)", ph, ledger.TraceIDPlaceholder)
		}
	}
	lc := core.LedgerConfig{TraceURL: rl.TraceURL}
	if rl.Retention != "" {
		d, err := ledger.ParseDuration(rl.Retention)
		if err != nil || d < 24*time.Hour {
			return core.LedgerConfig{}, newError(filename, line,
				"[ledger] retention %q: want a duration of at least 1d (e.g. \"90d\")", rl.Retention)
		}
		lc.Retention = d
	}
	if rl.MaxMB != nil {
		if *rl.MaxMB < minLedgerMaxMB {
			return core.LedgerConfig{}, newError(filename, line,
				"[ledger] max_mb %d: want at least %d", *rl.MaxMB, minLedgerMaxMB)
		}
		lc.MaxMB = *rl.MaxMB
	}
	return lc, nil
}

// minLedgerMaxMB is the smallest max_mb (REQ-12).
const minLedgerMaxMB = 16

// ledgerGlobalOnlyErr refuses a [ledger] table outside the global file.
func ledgerGlobalOnlyErr(filename string, line int) *Error {
	return newError(filename, line, "[ledger] is not allowed here: the run ledger is configured globally, in the daemon's harness.toml")
}
