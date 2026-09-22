// Package governance holds repository-level checks on the architecture
// artifacts in docs/: that the ADRs and specs the code cites actually exist.
//
// Governing comments (`// Governing: ADR-0020, SPEC-0013 REQ-3`) are how a
// reader gets from code to the decision behind it, so a citation that points
// at nothing is worse than none — it sends the reader to the wrong document
// with confidence. Numbers drift when a draft is renumbered before it lands:
// the chatroom spec was drafted as SPEC-0015 and merged as SPEC-0009, and
// seven comments kept the draft number for weeks.
//
// @joestump-agent 09/21/2026 - Added with the chatroom citation fix.
package governance
