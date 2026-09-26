package supervisor

// Template Render Failures
//
// TL;DR: a command harness's argv is rendered at spawn (spawn.go). When that
// render fails, nothing is exec'd, and the failure is never silent: a run with
// a record is closed `skipped` with reason `template_unresolved` and the
// missing path's name; a start with no record fails like any spawn failure,
// with a log line naming the path. Either way the metrics counter moves.
// Nothing here ever handles a rendered value: the error names a path, and a
// path is all that reaches the log, the record or the bus.
//
// Governing: ADR-0023, SPEC-0017 REQ-11 "Rendering", REQ "Error Handling
// Standards" (scenario "A render failure is never silent"); design.md §
// "Where rendering happens in spawn".
//
// @joestump-agent 09/24/2026 - Added for #503.

import (
	"errors"
	"time"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/tmpl"
)

// classifyRenderFailure reports whether a spawn error is a template render
// failure, which kind, and — for an unresolved value — the missing path.
func classifyRenderFailure(err error) (reason, path string, ok bool) {
	var unres *tmpl.UnresolvedError
	switch {
	case errors.As(err, &unres):
		return RenderFailureUnresolved, unres.Path, true
	case errors.Is(err, tmpl.ErrGrammar):
		return RenderFailureGrammar, "", true
	}
	return "", "", false
}

// onRenderFailure handles a spawn that failed while rendering its templates.
// It reports whether it settled the start itself: true for a recorded run
// whose required value was absent, which becomes a skip; false for everything
// else, which the caller treats as the spawn failure it is.
//
// The skip lands the harness in stopped, not failed: no process ran, so
// nothing failed, and the next firing is the retry exactly as it is for a run
// the overlap policy skipped. It is not a crash either, so the restart policy
// never sees it.
func (s *Supervisor) onRenderFailure(err error) bool {
	reason, path, ok := classifyRenderFailure(err)
	if !ok {
		return false
	}
	if s.bus != nil {
		s.bus.Publish(Event{Kind: EventTemplateRenderFailed, Name: s.harness.Name, Time: time.Now(), RenderFailure: reason})
	}
	if reason == RenderFailureUnresolved && s.run != nil {
		run := s.run
		s.logEvent("run skipped", "run_id", run.rec.RunID, "reason", string(ReasonTemplateUnresolved), "path", path)
		run.rec.MissingPath = path
		run.rec.Coalesced = 1
		s.transition(core.StateStopped)
		// finishRunWith sets the reason: finishRun would clear it.
		s.finishRunWith(OutcomeSkipped, nil, ReasonTemplateUnresolved)
		return true
	}
	kv := []any{"reason", "template_" + reason}
	if path != "" {
		kv = append(kv, "path", path)
	}
	// The spawn failed before beginStart opened the harness log, and this
	// line is the start error's only durable account.
	s.ensureLog()
	s.logEvent("start failed", kv...)
	return false
}
