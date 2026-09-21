// Package telemetry exports what supervised agents do — every tool call and
// mark the daemon's observer attributes to a harness — as OTLP logs, OTLP
// traces and a local JSONL events file.
//
// # Telemetry Pipeline
//
// Each enabled signal is a sink with the same three stages and its own
// observer subscription, so one stalled destination cannot hold another's
// items: convert (gate, redact, cap, map to the signal's unit) on an intake
// goroutine, a bounded drop-oldest queue, and a delivery goroutine that
// batches and sends with at most one request in flight. Nothing here ever
// blocks the observer, the supervisor or the Manager's lock: the observer's
// own fan-out never waits on a subscriber, the gate's one Manager read is a
// short snapshot, and every export runs on telemetry's goroutines (ADR-0007).
//
// Consent is checked twice and first. The daemon builds a Pipeline only when
// [telemetry] names a destination — with none there is no subscription, no
// file and no socket (REQ-1). And every item is gated against its harness's
// current definition before anything else touches it: an item from a harness
// that has not opted in is dropped before redaction, queueing or counting,
// and a reload that changes export_telemetry applies to the next item.
//
// Governing: ADR-0021; SPEC-0014 (all REQs); ADR-0007; ADR-0008.
//
// @joestump-agent 09/21/2026 - Added for harness#391.
package telemetry

import (
	"context"
	"net/http"
	"sync"
	"time"

	"charm.land/log/v2"
	"github.com/stump-wtf/agent-trace/otel"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/observe"
	"github.com/stump-wtf/harness/internal/otlpexport"
	"github.com/stump-wtf/harness/internal/supervisor"
)

// Subscriber is what the pipeline needs from the observer.
// *observe.Observer satisfies it.
type Subscriber interface {
	Subscribe(name string, buf int) (<-chan observe.Event, func())
	Stats() observe.Stats
}

// Source is what the pipeline needs from the supervisor: the harness's current
// definition, for the gate, and its state, for the traces exit trigger.
// *supervisor.Manager satisfies it.
type Source interface {
	HarnessDef(name string) (core.Harness, bool)
	Snapshot(name string) (supervisor.Snapshot, bool)
}

// SubscriptionName is the observer subscription a signal takes.
func SubscriptionName(signal string) string { return "telemetry." + signal }

// Options are the pipeline's seams; the zero value is production.
type Options struct {
	Logger *log.Logger
	Now    func() time.Time
	// Sleep waits between retries; it must return early when ctx ends.
	Sleep func(ctx context.Context, d time.Duration) error
	// Jitter picks a delay in [0, max] (default: uniform random).
	Jitter func(max time.Duration) time.Duration
	// Client sends OTLP requests (default http.DefaultClient).
	Client *http.Client
}

// Pipeline is a running telemetry export.
type Pipeline struct {
	res  *Resolved
	sub  Subscriber
	src  Source
	log  *log.Logger
	now  func() time.Time
	conv *converter

	stats   map[string]*signalStats
	queues  map[string]interface{ length() int }
	cancels []func()
	intakes sync.WaitGroup
	loops   []interface {
		stopCh() chan struct{}
		doneCh() chan struct{}
	}

	stopCancel context.CancelFunc // ends retry waits
	sendCancel context.CancelFunc // abandons in-flight requests
	deadline   chan struct{}      // closed when the shutdown budget is spent
	evFile     *eventsFile

	shutdownOnce sync.Once
	report       ShutdownReport
}

type loopHandle struct{ stop, done chan struct{} }

func (l loopHandle) stopCh() chan struct{} { return l.stop }
func (l loopHandle) doneCh() chan struct{} { return l.done }

// New subscribes to the observer for every enabled signal and starts the
// pipeline. res must be non-nil and Enabled.
func New(res *Resolved, sub Subscriber, src Source, opts Options) *Pipeline {
	if opts.Logger == nil {
		opts.Logger = log.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Sleep == nil {
		opts.Sleep = sleepCtx
	}
	if opts.Jitter == nil {
		opts.Jitter = fullJitter
	}
	cfg := res.Config
	stopCtx, stopCancel := context.WithCancel(context.Background())
	sendCtx, sendCancel := context.WithCancel(context.Background())
	p := &Pipeline{
		res: res, sub: sub, src: src, log: opts.Logger, now: opts.Now,
		conv:       &converter{omitPrompts: cfg.OmitPrompts, ids: newIDRegistry(opts.Now)},
		stats:      map[string]*signalStats{},
		queues:     map[string]interface{ length() int }{},
		stopCancel: stopCancel, sendCancel: sendCancel,
		deadline: make(chan struct{}),
	}
	flog := newFailureLog(opts.Logger, opts.Now)
	resource := otlpexport.Resource{Attributes: res.Resource}
	scope := otlpexport.Scope{Name: ScopeName, Version: res.Version}

	if sig := res.Logs; sig != nil {
		st := &signalStats{}
		q := newQueue[Record](cfg.QueueSize, st)
		ep := sig.Endpoint()
		ep.Client = opts.Client
		s := &otlpSender[Record]{
			signal: SignalLogs, stats: st, flog: flog, now: opts.Now, sleep: opts.Sleep, jitter: opts.Jitter,
			stopCtx: stopCtx, sendCtx: sendCtx,
			send: func(ctx context.Context, batch []Record) otlpexport.Result {
				recs := make([]otlpexport.LogRecord, len(batch))
				for i, r := range batch {
					recs[i] = r.LogRecord()
				}
				return otlpexport.SendLogs(ctx, ep, resource, scope, recs)
			},
		}
		startLoopFor(p, SignalLogs, st, q, s.deliver)
		p.startIntake(SignalLogs, func(ev observe.Event) { q.push(p.conv.record(ev), time.Now()) }, nil, nil)
	}

	if sig := res.Traces; sig != nil {
		st := &signalStats{}
		q := newQueue[otel.Span](cfg.QueueSize, st)
		ep := sig.Endpoint()
		ep.Client = opts.Client
		s := &otlpSender[otel.Span]{
			signal: SignalTraces, stats: st, flog: flog, now: opts.Now, sleep: opts.Sleep, jitter: opts.Jitter,
			stopCtx: stopCtx, sendCtx: sendCtx,
			send: func(ctx context.Context, batch []otel.Span) otlpexport.Result {
				return otlpexport.SendSpans(ctx, ep, resource, scope, batch)
			},
		}
		startLoopFor(p, SignalTraces, st, q, s.deliver)
		acc := &traceAccumulator{
			conv: p.conv, omitPrompts: cfg.OmitPrompts, idle: cfg.IdleFlush, now: opts.Now,
			running: runningFunc(src), stats: st, log: opts.Logger,
			sessions: map[string]*traceSession{},
			emit: func(spans []otel.Span) {
				at := time.Now()
				for _, sp := range spans {
					q.push(sp, at)
				}
			},
		}
		p.startIntake(SignalTraces, acc.add, acc.tick, acc.flushAll)
	}

	if path := cfg.EventsFile; path != "" {
		st := &signalStats{}
		q := newQueue[[]byte](cfg.QueueSize, st)
		p.evFile = newEventsFile(path, cfg.EventsFileMaxMB, cfg.EventsFileKeep)
		w := p.evFile
		deliver := func(batch [][]byte, _ bool) {
			var buf []byte
			for _, line := range batch {
				buf = append(buf, line...)
			}
			if err := w.write(buf); err != nil {
				st.reqPermanent.Add(1)
				st.failed.Add(uint64(len(batch)))
				st.setError(err.Error())
				flog.warn(SignalEventsFile, "write", "telemetry events file write failed; batch dropped",
					"lines", len(batch), "err", err)
				return
			}
			st.reqSuccess.Add(1)
			st.exported.Add(uint64(len(batch)))
			st.lastSuccess.Store(p.now().UnixNano())
		}
		startLoopFor(p, SignalEventsFile, st, q, deliver)
		p.startIntake(SignalEventsFile, func(ev observe.Event) {
			line, err := p.conv.record(ev).JSONLine(res.Resource)
			if err != nil {
				st.failed.Add(1)
				st.setError(err.Error())
				return
			}
			q.push(line, time.Now())
		}, nil, nil)
	}
	return p
}

// startLoopFor runs a signal's delivery goroutine.
func startLoopFor[T any](p *Pipeline, signal string, st *signalStats, q *queue[T], deliver func([]T, bool)) {
	b := &batchLoop[T]{
		// Batching runs on the wall clock; the injectable clock is for the
		// retry policy, which tests drive through minutes in microseconds.
		q: q, size: p.res.Config.BatchSize, interval: p.res.Config.BatchInterval, now: time.Now,
		deliver: deliver, stop: make(chan struct{}), done: make(chan struct{}), deadline: p.deadline,
	}
	p.stats[signal] = st
	p.queues[signal] = q
	p.loops = append(p.loops, loopHandle{b.stop, b.done})
	go b.run()
}

// startIntake subscribes signal to the observer and runs its intake
// goroutine: gate, then handle. tick, when set, runs every batch_interval;
// final, when set, runs once after the subscription closes.
func (p *Pipeline) startIntake(signal string, handle func(observe.Event), tick, final func()) {
	// The buffer absorbs one observer scan's burst; the queue behind it is
	// what bounds memory during an outage.
	ch, cancel := p.sub.Subscribe(SubscriptionName(signal), p.res.Config.QueueSize)
	p.cancels = append(p.cancels, cancel)
	p.intakes.Add(1)
	go func() {
		defer p.intakes.Done()
		var tc <-chan time.Time
		if tick != nil {
			t := time.NewTicker(p.res.Config.BatchInterval)
			defer t.Stop()
			tc = t.C
		}
		for {
			select {
			case ev, ok := <-ch:
				if !ok {
					if final != nil {
						final()
					}
					return
				}
				if p.contributes(ev.Harness) {
					handle(ev)
				}
			case <-tc:
				tick()
			}
		}
	}()
}

// contributes is the per-item gate (SPEC-0014 REQ-1): the harness's CURRENT
// definition, so a reload applies to the next item. A harness the daemon no
// longer knows does not contribute.
func (p *Pipeline) contributes(name string) bool {
	h, ok := p.src.HarnessDef(name)
	return ok && core.ContributesTelemetry(h, p.res.Config.ExportAll)
}

// ShutdownReport is what the shutdown flush lost, per signal.
type ShutdownReport struct {
	// Lost counts units that were queued or in flight when the deadline hit:
	// dropped_queue plus failed accumulated during the shutdown flush.
	Lost map[string]uint64
	// TimedOut reports that the budget ran out before every signal finished.
	TimedOut bool
}

// Shutdown stops intake, exports every session's unsent spans, gives each
// queued batch one attempt and closes the events file — all within ctx
// (SPEC-0014 REQ-11) — then logs what was lost in one line. It is idempotent.
func (p *Pipeline) Shutdown(ctx context.Context) ShutdownReport {
	p.shutdownOnce.Do(func() {
		before := map[string]uint64{}
		for name, st := range p.stats {
			before[name] = st.droppedQueue.Load() + st.failed.Load()
		}

		// 1. Stop taking items; intake drains what the observer already
		// handed it, and the traces intake exports every session.
		for _, c := range p.cancels {
			c()
		}
		intakeDone := make(chan struct{})
		go func() { p.intakes.Wait(); close(intakeDone) }()
		timedOut := false
		select {
		case <-intakeDone:
		case <-ctx.Done():
			timedOut = true
		}

		// 2–3. Flush every queue: one attempt per batch, no retry waits.
		p.stopCancel()
		for _, l := range p.loops {
			close(l.stopCh())
		}
		allDone := make(chan struct{})
		go func() {
			for _, l := range p.loops {
				<-l.doneCh()
			}
			close(allDone)
		}()
		select {
		case <-allDone:
		case <-ctx.Done():
			timedOut = true
			close(p.deadline)
			p.sendCancel()
			<-allDone
		}
		if !timedOut {
			close(p.deadline)
		}
		p.sendCancel()

		// 4. The events file: flush and close. Its loop has exited, so
		// nothing else touches it.
		if p.evFile != nil {
			if err := p.evFile.flushClose(); err != nil {
				p.log.Warn("telemetry events file close failed", "err", err)
			}
		}

		p.report = ShutdownReport{Lost: map[string]uint64{}, TimedOut: timedOut}
		kv := []any{"timed_out", timedOut}
		for name, st := range p.stats {
			lost := st.droppedQueue.Load() + st.failed.Load() - before[name]
			p.report.Lost[name] = lost
			kv = append(kv, name+"_lost", lost)
		}
		p.log.Info("telemetry flushed", kv...)
	})
	return p.report
}

// Stats is a snapshot of every enabled signal's counters (SPEC-0014 REQ-12).
func (p *Pipeline) Stats() map[string]SignalStats {
	obs := p.sub.Stats()
	out := make(map[string]SignalStats, len(p.stats))
	for name, st := range p.stats {
		s := st.snapshot()
		s.QueueLength = p.queues[name].length()
		s.DroppedObserver = obs.Dropped[SubscriptionName(name)]
		out[name] = s
	}
	return out
}
