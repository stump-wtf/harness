package config

// Ledger Table
//
// Parses the global [ledger] table (SPEC-0022 REQ-19) into core.LedgerConfig.
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

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/ledger"
)

// rawLedger mirrors the [ledger] table before validation.
type rawLedger struct {
	TraceURL string `toml:"trace_url"`
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
	return core.LedgerConfig{TraceURL: rl.TraceURL}, nil
}

// ledgerGlobalOnlyErr refuses a [ledger] table outside the global file.
func ledgerGlobalOnlyErr(filename string, line int) *Error {
	return newError(filename, line, "[ledger] is not allowed here: the run ledger is configured globally, in the daemon's harness.toml")
}
