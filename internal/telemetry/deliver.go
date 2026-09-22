package telemetry

// OTLP Delivery
//
// One batch, one request at a time, with the SPEC-0015 REQ-10 retry policy:
// 429, 502, 503, 504 and network errors or timeouts are retried with
// exponential backoff from 1s, doubling to a 30s cap, with full jitter — or
// exactly the collector's Retry-After when it sent one. A batch still
// undelivered five minutes after its first attempt is given up and counted
// failed; so is any other status, at once. A 2xx with partialSuccess counts
// the rejected units as rejected and the rest as exported.
//
// While a batch retries, its queue keeps accepting (and, when full, dropping)
// units, so an outage costs counted telemetry and nothing else. Shutdown
// interrupts a retry wait and gives the batch one last attempt, bounded by
// the shutdown deadline.
//
// Governing: ADR-0022; SPEC-0015 REQ-10, REQ-11; ADR-0007; ADR-0008 (a
// failure is logged by status or error class, never with a header or body).
//
// @joestump-agent 09/21/2026 - Added for harness#391.

import (
	"context"
	"math/rand/v2"
	"time"

	"github.com/stump-wtf/harness/internal/otlpexport"
	"github.com/stump-wtf/harness/internal/redact"
)

// Retry policy constants (SPEC-0015 REQ-10).
const (
	retryBase   = time.Second
	retryCap    = 30 * time.Second
	retryGiveUp = 5 * time.Minute
)

// otlpSender delivers batches of one signal.
type otlpSender[T any] struct {
	signal string
	send   func(ctx context.Context, batch []T) otlpexport.Result
	stats  *signalStats
	flog   *failureLog
	now    func() time.Time
	sleep  func(ctx context.Context, d time.Duration) error
	jitter func(max time.Duration) time.Duration

	// stopCtx is cancelled when shutdown begins: retry waits end early.
	stopCtx context.Context
	// sendCtx is cancelled at the shutdown deadline: an in-flight request is
	// abandoned and its batch counted failed.
	sendCtx context.Context
}

// fullJitter is a uniform delay in [0, max].
func fullJitter(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(max) + 1))
}

// backoff is the pre-jitter delay before retry number attempt (0-based).
func backoff(attempt int) time.Duration {
	d := retryBase
	for range attempt {
		d *= 2
		if d >= retryCap {
			return retryCap
		}
	}
	return d
}

// deliver sends batch, retrying unless final.
func (s *otlpSender[T]) deliver(batch []T, final bool) {
	n := uint64(len(batch))
	start := s.now()
	for attempt := 0; ; attempt++ {
		res := s.send(s.sendCtx, batch)
		if res.OK() {
			s.stats.reqSuccess.Add(1)
			rej := uint64(max(res.Rejected, 0))
			rej = min(rej, n)
			s.stats.rejected.Add(rej)
			s.stats.exported.Add(n - rej)
			s.stats.lastSuccess.Store(s.now().UnixNano())
			if rej > 0 {
				msg := capString(redact.String(res.ErrorMessage), attrCap)
				s.stats.setError("partial success: " + msg)
				s.flog.warn(s.signal, "partial_success", "telemetry collector rejected part of a batch",
					"rejected", rej, "message", msg)
			}
			return
		}
		s.stats.setError(res.Err.Error())
		if !res.Retryable {
			s.stats.reqPermanent.Add(1)
			s.stats.failed.Add(n)
			s.flog.warn(s.signal, res.Class(), "telemetry export failed; batch dropped (not retryable)",
				"status", res.StatusCode, "units", n)
			return
		}
		s.stats.reqRetryable.Add(1)
		if final || s.stopCtx.Err() != nil {
			s.stats.failed.Add(n)
			s.flog.warn(s.signal, res.Class(), "telemetry export failed during shutdown; batch dropped",
				"status", res.StatusCode, "units", n)
			return
		}
		delay := res.RetryAfter
		if delay <= 0 {
			delay = s.jitter(backoff(attempt))
		}
		if s.now().Sub(start)+delay >= retryGiveUp {
			s.stats.failed.Add(n)
			s.flog.warn(s.signal, "gave_up", "telemetry export gave up on a batch after retrying for 5m",
				"last", res.Class(), "units", n)
			return
		}
		s.flog.warn(s.signal, res.Class(), "telemetry export failed; retrying",
			"status", res.StatusCode, "retry_in", delay.Round(time.Millisecond))
		if err := s.sleep(s.stopCtx, delay); err != nil {
			// Shutdown began: one last attempt, no more waiting.
			final = true
		}
	}
}
