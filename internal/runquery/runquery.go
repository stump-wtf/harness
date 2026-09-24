// Package runquery is the one place a runs query is validated and a ledger
// record becomes a wire record, shared by the daemon's runs op and the CLI's
// offline read (SPEC-0022 REQ-15, REQ-16) so the two can never render the same
// record differently.
//
// Governing: SPEC-0022 REQ-4, REQ-14, REQ-15, REQ-16.
//
// @joestump 09/24/2026 - Added for harness#448.
package runquery

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/stump-wtf/harness/internal/ledger"
	"github.com/stump-wtf/harness/internal/protocol"
	"github.com/stump-wtf/harness/internal/supervisor"
)

// MaxLimit bounds one page of the runs op (REQ-15).
const MaxLimit = 1000

// Outcomes is every outcome a record can carry (SPEC-0008, SPEC-0022 REQ-5).
var Outcomes = []string{
	string(supervisor.OutcomeRunning), string(supervisor.OutcomeSuccess), string(supervisor.OutcomeFailed),
	string(supervisor.OutcomeTimedOut), string(supervisor.OutcomeSkipped), string(supervisor.OutcomeReplaced),
	string(supervisor.OutcomeMissed), string(supervisor.OutcomeCancelled), string(supervisor.OutcomeInterrupted),
	string(supervisor.OutcomeBudgetExceeded), string(supervisor.OutcomeQuotaParked),
	string(supervisor.OutcomeModelMismatch), string(supervisor.OutcomeModelUnattested),
}

// Triggers is every trigger a record can carry (SPEC-0008, SPEC-0014,
// SPEC-0022 REQ-3).
var Triggers = []string{
	string(supervisor.TriggerSchedule), string(supervisor.TriggerManual), string(supervisor.TriggerCatchUp),
	string(supervisor.TriggerChannel), string(supervisor.TriggerWebhook), string(supervisor.TriggerAutostart),
	string(supervisor.TriggerRestart), string(supervisor.TriggerRelease), string(supervisor.TriggerLease),
}

// ErrInvalid marks a query the caller got wrong.
var ErrInvalid = errors.New("invalid runs query")

// CheckValues rejects any of got not in valid, listing the valid ones (REQ-14
// "An unknown outcome": the list includes timed_out, the value an operator
// who typed "timeout" was after).
func CheckValues(what string, got, valid []string) error {
	for _, g := range got {
		if !slices.Contains(valid, g) {
			return fmt.Errorf("%w: unknown %s %q (valid: %s)", ErrInvalid, what, g, strings.Join(valid, ", "))
		}
	}
	return nil
}

// IsQuery reports a runs request that is not SPEC-0008's single-harness
// {name, limit}: one with no name (every harness), or with anything an old
// client could not have sent.
func IsQuery(req protocol.ControlReq) bool {
	return req.Name == "" || len(req.Names) > 0 || req.Since != "" || req.Until != "" || len(req.Outcomes) > 0 ||
		len(req.Triggers) > 0 || req.BeforeSeq > 0
}

// FromRequest turns a runs request into a ledger query, validating it.
func FromRequest(req protocol.ControlReq, defaultLimit int) (ledger.Query, error) {
	q := ledger.Query{Names: req.Names, Outcomes: req.Outcomes, Triggers: req.Triggers, BeforeSeq: req.BeforeSeq, Limit: req.Limit}
	if req.Name != "" && !slices.Contains(q.Names, req.Name) {
		q.Names = append(slices.Clone(q.Names), req.Name)
	}
	if q.Limit == 0 {
		q.Limit = defaultLimit
	}
	if q.Limit < 1 || q.Limit > MaxLimit {
		return ledger.Query{}, fmt.Errorf("%w: limit %d is outside 1–%d", ErrInvalid, q.Limit, MaxLimit)
	}
	var err error
	if q.Since, err = instant("since", req.Since); err != nil {
		return ledger.Query{}, err
	}
	if q.Until, err = instant("until", req.Until); err != nil {
		return ledger.Query{}, err
	}
	if err := CheckValues("outcome", q.Outcomes, Outcomes); err != nil {
		return ledger.Query{}, err
	}
	if err := CheckValues("trigger", q.Triggers, Triggers); err != nil {
		return ledger.Query{}, err
	}
	return q, nil
}

func instant(what, s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %s %q is not an RFC 3339 instant", ErrInvalid, what, s)
	}
	return t, nil
}

// ParseSince reads the CLI's --since: a duration back from now ("7d", "36h")
// or an RFC 3339 instant (REQ-14).
func ParseSince(s string, now time.Time) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	d, err := ParseDuration(s)
	if err != nil {
		return time.Time{}, fmt.Errorf("--since %q: want a duration (7d, 36h) or an RFC 3339 instant", s)
	}
	return now.Add(-d), nil
}

// ParseDuration is time.ParseDuration plus a whole-day suffix, "7d".
func ParseDuration(s string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(s, "d"); ok {
		var n int
		if _, err := fmt.Sscanf(days, "%d", &n); err == nil && fmt.Sprint(n) == days && n >= 0 {
			return time.Duration(n) * 24 * time.Hour, nil
		}
		return 0, fmt.Errorf("bad day count %q", s)
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("bad duration %q", s)
	}
	return d, nil
}

// Info projects a ledger record onto the wire. HasLog reports a log that is
// there to read with logs --run.
func Info(f ledger.Folded) protocol.RunInfo {
	info := protocol.RunInfo{
		Harness:       f.Harness,
		RunID:         f.RunID,
		Seq:           f.Seq,
		Trigger:       f.Trigger,
		Outcome:       f.Outcome,
		ExitCode:      f.ExitCode,
		Windows:       f.Windows,
		Source:        f.Source,
		EventID:       f.EventID,
		Reason:        f.Reason,
		TodoID:        f.TodoID,
		LogPruned:     f.LogPruned,
		Kind:          f.Kind,
		Attempt:       f.Attempt,
		Model:         f.Model,
		CostUSD:       f.CostUSD,
		CostSource:    f.CostSource,
		ModelCalls:    f.ModelCalls,
		Errors:        f.Errors,
		UsageComplete: f.UsageComplete,
		TraceURL:      f.TraceURL,
		Log:           f.Log,
		Override:      f.Override,
		Coalesced:     f.Coalesced,
		Imported:      f.Imported,
	}
	start := f.FirstAt
	if f.StartedAt != nil {
		start = *f.StartedAt
	}
	info.StartedAt = start.Format(time.RFC3339Nano)
	if f.EndedAt != nil {
		info.EndedAt = f.EndedAt.Format(time.RFC3339Nano)
		info.DurationMs = f.EndedAt.Sub(start).Milliseconds()
	}
	if f.DurationMs > 0 {
		info.DurationMs = f.DurationMs
	}
	if f.Window != nil {
		info.Window = f.Window.Format(time.RFC3339)
	}
	if f.FirstWindow != nil {
		info.FirstWindow = f.FirstWindow.Format(time.RFC3339)
	}
	if f.Tokens != nil {
		info.Tokens = &protocol.RunTokens{Input: f.Tokens.Input, Output: f.Tokens.Output, CacheRead: f.Tokens.CacheRead, CacheWrite: f.Tokens.CacheWrite}
	}
	for _, m := range f.Models {
		info.Models = append(info.Models, protocol.RunModel{Model: m.Model, Provider: m.Provider, OutputTokens: m.OutputTokens})
	}
	for _, s := range f.Sessions {
		info.Sessions = append(info.Sessions, protocol.RunSession{ID: s.ID, Adapter: s.Adapter, TraceID: s.TraceID})
	}
	if f.Mismatch != nil {
		info.Mismatch = &protocol.RunMismatch{Kind: f.Mismatch.Kind, ServedModel: f.Mismatch.ServedModel,
			ServedProvider: f.Mismatch.ServedProvider, At: f.Mismatch.At.Format(time.RFC3339Nano)}
	}
	if f.Log != "" && !f.LogPruned && f.Kind != ledger.KindResident {
		if _, err := os.Stat(f.Log); err == nil {
			info.HasLog = true
		}
	}
	return info
}

// Infos is Info over a list.
func Infos(recs []ledger.Folded) []protocol.RunInfo {
	out := make([]protocol.RunInfo, 0, len(recs))
	for _, f := range recs {
		out = append(out, Info(f))
	}
	return out
}
