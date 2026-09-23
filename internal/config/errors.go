package config

// Governing: ADR-0006 (config is source of truth; a parse error keeps the
// last-good config and surfaces the location) and SPEC-0001 REQ "Zero And
// Error States" — the reload banner "using last-good config; line 12: …" needs
// a location-carrying error, so every parse/validation failure reports the
// line it occurred on.

import (
	"errors"
	"fmt"
)

// Sentinel errors for the failure modes a caller needs to distinguish rather
// than merely report. SPEC-0014 REQ "Error Handling Standards" names them: a
// config error stays a location-carrying *Error, and wraps one of these so
// `errors.Is` answers "which kind of failure was that?" without anyone
// matching on message text.
//
// The runtime's own sentinels — a missing channel capability, a refused
// stream, an expired session, an authentication failure, an invalid event —
// belong to the packages that produce them; these two are the config's share.
// Governing: ADR-0021; SPEC-0014 REQ "Error Handling Standards".
var (
	// ErrUnknownSourceRef is a `triggers` entry naming a source no
	// [channel.*] or [webhook.*] table declares, in this file or a drop-in.
	ErrUnknownSourceRef = errors.New("unknown trigger source reference")
	// ErrLiteralSecret is a `[webhook.*] secret` holding a value rather than
	// exactly one ${NAME} reference. harness.toml is routinely committed to a
	// dotfiles repository, so this is refused rather than warned about.
	ErrLiteralSecret = errors.New("literal secret in config")
)

// Error is a configuration parse or validation failure that carries the source
// location. Line is 1-based; 0 means "no specific line" (whole-file error).
type Error struct {
	// File is the path (or logical name) the config came from.
	File string
	// Line is the 1-based source line, or 0 if unknown.
	Line int
	// Msg is the human-readable description, without the location prefix.
	Msg string
	// Err is an optional sentinel this failure is an instance of, so callers
	// can errors.Is it while still getting the file and line for display.
	// Deliberately NOT formatted into Msg: the sentinel's text is a category
	// ("unknown trigger source reference"), and Msg already says the specific
	// thing in the operator's own vocabulary.
	Err error
}

// Error implements the error interface, formatting "file:line: msg" so it reads
// well on a terminal; the TUI can pull Line directly for its banner.
func (e *Error) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("%s:%d: %s", e.File, e.Line, e.Msg)
	}
	return fmt.Sprintf("%s: %s", e.File, e.Msg)
}

// LineNumber returns the 1-based source line, or 0 if unknown. The TUI reload
// banner (SPEC-0001) uses this to render "line N: …".
func (e *Error) LineNumber() int { return e.Line }

// Unwrap exposes the sentinel this failure is an instance of, so errors.Is
// works through the location wrapper. nil for the majority of errors, which
// are one-off validation messages with no category worth testing for.
func (e *Error) Unwrap() error { return e.Err }

// newError builds a *Error at a given line.
func newError(file string, line int, format string, args ...any) *Error {
	return &Error{File: file, Line: line, Msg: fmt.Sprintf(format, args...)}
}

// newSentinelError builds a *Error that also satisfies errors.Is(err, kind).
func newSentinelError(file string, line int, kind error, format string, args ...any) *Error {
	return &Error{File: file, Line: line, Msg: fmt.Sprintf(format, args...), Err: kind}
}
