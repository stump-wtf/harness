package config

// Governing: ADR-0006 (config is source of truth; a parse error keeps the
// last-good config and surfaces the location) and SPEC-0001 REQ "Zero And
// Error States" — the reload banner "using last-good config; line 12: …" needs
// a location-carrying error, so every parse/validation failure reports the
// line it occurred on.

import "fmt"

// Error is a configuration parse or validation failure that carries the source
// location. Line is 1-based; 0 means "no specific line" (whole-file error).
type Error struct {
	// File is the path (or logical name) the config came from.
	File string
	// Line is the 1-based source line, or 0 if unknown.
	Line int
	// Msg is the human-readable description, without the location prefix.
	Msg string
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

// newError builds a *Error at a given line.
func newError(file string, line int, format string, args ...any) *Error {
	return &Error{File: file, Line: line, Msg: fmt.Sprintf(format, args...)}
}
