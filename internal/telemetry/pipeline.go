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
// Governing: ADR-0022; SPEC-0015 (all REQs); ADR-0007; ADR-0008.
//
// @joestump-agent 09/21/2026 - Added for harness#391.
//
// @joestump-agent 09/21/2026 - Made the shutdown flush strictly bounded: a
// loop stuck in an events-file write is abandoned and counted at the deadline.
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
	// Client sends OTLP requests (default: otlpexport's client, which
	// does not follow redirects).
	Client *http.Client
	// OpenEventsFile opens the events file for append (default: os.OpenFile,
	// O_APPEND|O_CREATE, 0600). A test seam for writes that fail or hang.
	OpenEventsFile func(path string) (EventsFileHandle, error)
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
	loops   []deliveryLoop
	evLoop  deliveryLoop // the events file's loop, when enabled

	stopCancel context.CancelFunc // ends retry waits
	sendCancel context.CancelFunc // abandons in-flight requests
	deadline   chan struct{}      // closed when the shutdown budget is spent
	evFile     *eventsFile

	shutdownOnce sync.Once
	report       ShutdownReport
}

// deliveryLoop is a running batchLoop, whatever its unit type.
type deliveryLoop interface {
	stopCh() chan struct{}
	doneCh() chan struct{}
	abandon() uint64
}

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
		conv:       &converter{omitPrompts: cfg.OmitPrompts},
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
		if opts.OpenEventsFile != nil {
			p.evFile.openFile = opts.OpenEventsFile
		}
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
		p.evLoop = startLoopFor(p, SignalEventsFile, st, q, deliver)
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
func startLoopFor[T any](p *Pipeline, signal string, st *signalStats, q *queue[T], deliver func([]T, bool)) deliveryLoop {
	b := &batchLoop[T]{
		// Batching runs on the wall clock; the injectable clock is for the
		// retry policy, which tests drive through minutes in microseconds.
		q: q, size: p.res.Config.BatchSize, interval: p.res.Config.BatchInterval, now: time.Now,
		deliver: deliver, stop: make(chan struct{}), done: make(chan struct{}), deadline: p.deadline,
	}
	p.stats[signal] = st
	p.queues[signal] = q
	p.loops = append(p.loops, b)
	go b.run()
	return b
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

// contributes is the per-item gate (SPEC-0015 REQ-1): the harness's CURRENT
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
// (SPEC-0015 REQ-11) — then logs what was lost in one line. It is idempotent.
//
// The budget is strict even when a delivery ignores cancellation. An OTLP
// request stops when sendCancel fires, but an events-file write blocked on a
// hung mount does not return for anything, so the flush phases run against a
// context that ends a little before ctx (shutdownReserve), and the reserve is
// spent waiting for the loops to notice. A loop still inside a delivery when
// ctx ends is abandoned: its queue is counted dropped_queue, its in-flight
// batch failed, and Shutdown returns without it.
func (p *Pipeline) Shutdown(ctx context.Context) ShutdownReport {
	p.shutdownOnce.Do(func() {
		before := map[string]uint64{}
		for name, st := range p.stats {
			before[name] = st.droppedQueue.Load() + st.failed.Load()
		}
		flushCtx, cancelFlush := withShutdownReserve(ctx)
		defer cancelFlush()

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
		case <-flushCtx.Done():
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
		abandoned := map[deliveryLoop]bool{}
		select {
		case <-allDone:
		case <-flushCtx.Done():
			timedOut = true
			close(p.deadline)
			p.sendCancel()
			select {
			case <-allDone:
			case <-ctx.Done():
				for _, l := range p.loops {
					select {
					case <-l.doneCh():
					default:
						abandoned[l] = true
						l.abandon()
					}
				}
			}
		}
		if !timedOut {
			close(p.deadline)
		}
		p.sendCancel()

		// 4. The events file: flush and close, unless its loop is still
		// stuck in a write (closing under it would race the write). A sync
		// on a hung mount can block too, so the close is bounded as well.
		if p.evFile != nil && !abandoned[p.evLoop] {
			closed := make(chan error, 1)
			go func() { closed <- p.evFile.flushClose() }()
			select {
			case err := <-closed:
				if err != nil {
					p.log.Warn("telemetry events file close failed", "err", err)
				}
			case <-ctx.Done():
				timedOut = true
				p.log.Warn("telemetry events file close abandoned at the shutdown deadline", "path", p.evFile.path)
			}
		}

		p.report = ShutdownReport{Lost: map[string]uint64{}, TimedOut: timedOut}
		kv := []any{"timed_out", timedOut}
		for name, st := range p.stats {
			lost := st.droppedQueue.Load() + st.failed.Load() - before[name]
			p.report.Lost[name] = lost
			kv = append(kv, name+"_lost", lost)
		}
		if len(abandoned) > 0 {
			kv = append(kv, "abandoned_writes", len(abandoned))
		}
		p.log.Info("telemetry flushed", kv...)
	})
	return p.report
}

// Shutdown reserve bounds: a tenth of the budget, at most 250ms, kept back
// from the flush phases so a loop has time to see sendCancel and count its
// batch before Shutdown must return.
const maxShutdownReserve = 250 * time.Millisecond

// withShutdownReserve returns a context that ends shutdownReserve before
// ctx's deadline (or with ctx, when it has none).
func withShutdownReserve(ctx context.Context) (context.Context, context.CancelFunc) {
	dl, ok := ctx.Deadline()
	if !ok {
		return context.WithCancel(ctx)
	}
	reserve := min(time.Until(dl)/10, maxShutdownReserve)
	return context.WithDeadline(ctx, dl.Add(-reserve))
}

// Stats is a snapshot of every enabled signal's counters (SPEC-0015 REQ-12).
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
