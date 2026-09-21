package telemetry

// Queues, Batches and Self-Telemetry
//
// Behind each signal's observer subscription sits a bounded queue of at most
// queue_size units (log records, spans or lines). When it is full the OLDEST
// unit is dropped and counted — during an outage the newest data is the data
// most likely to explain what is happening now — so memory is bounded by
// configuration and never grows with collector downtime (SPEC-0014 REQ-9).
//
// batchLoop drains a queue in batches of at most batch_size, flushing a partial
// batch once batch_interval has passed since its oldest unit was queued, with
// one batch in flight at a time. On shutdown it hands every remaining batch to
// the deliver function once, marked final (no retries), until the deadline;
// whatever is still queued then is counted as dropped_queue.
//
// signalStats holds one signal's cumulative counters as atomics, so Stats()
// never waits on an export (REQ-12). failureLog rate-limits export failures
// to one warn line per signal per failure kind per minute, with a running
// count (REQ-10).
//
// Governing: ADR-0021; SPEC-0014 REQ-9, REQ-10, REQ-11, REQ-12; ADR-0007.
//
// @joestump-agent 09/21/2026 - Added for harness#391.

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"charm.land/log/v2"

	"github.com/stump-wtf/harness/internal/redact"
)

// queue is a bounded FIFO ring that drops its oldest unit when full.
type queue[T any] struct {
	mu      sync.Mutex
	buf     []queued[T]
	head, n int
	notify  chan struct{}
	stats   *signalStats
}

type queued[T any] struct {
	v  T
	at time.Time
}

func newQueue[T any](capacity int, stats *signalStats) *queue[T] {
	return &queue[T]{buf: make([]queued[T], capacity), notify: make(chan struct{}, 1), stats: stats}
}

// push appends v, dropping (and counting) the oldest unit when full. It never
// blocks.
func (q *queue[T]) push(v T, at time.Time) {
	q.mu.Lock()
	if q.n == len(q.buf) {
		var zero queued[T]
		q.buf[q.head] = zero
		q.head = (q.head + 1) % len(q.buf)
		q.n--
		q.stats.droppedQueue.Add(1)
	}
	q.buf[(q.head+q.n)%len(q.buf)] = queued[T]{v, at}
	q.n++
	q.mu.Unlock()
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

// take removes up to max units from the front.
func (q *queue[T]) take(max int) []T {
	q.mu.Lock()
	defer q.mu.Unlock()
	k := min(max, q.n)
	out := make([]T, k)
	var zero queued[T]
	for i := range k {
		out[i] = q.buf[q.head].v
		q.buf[q.head] = zero
		q.head = (q.head + 1) % len(q.buf)
	}
	q.n -= k
	return out
}

// state reports the queue length and when its oldest unit was queued.
func (q *queue[T]) state() (int, time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.n == 0 {
		return 0, time.Time{}
	}
	return q.n, q.buf[q.head].at
}

func (q *queue[T]) length() int {
	n, _ := q.state()
	return n
}

// discard empties the queue, counting every unit as dropped_queue.
func (q *queue[T]) discard() int {
	q.mu.Lock()
	n := q.n
	for i := range q.buf {
		var zero queued[T]
		q.buf[i] = zero
	}
	q.head, q.n = 0, 0
	q.mu.Unlock()
	q.stats.droppedQueue.Add(uint64(n))
	return n
}

// batchLoop runs one signal's delivery. deliver gets each batch; final is set
// during shutdown, when a batch gets exactly one attempt.
type batchLoop[T any] struct {
	q        *queue[T]
	size     int
	interval time.Duration
	now      func() time.Time
	deliver  func(batch []T, final bool)

	stop chan struct{} // closed: flush what is queued, then exit
	done chan struct{}
	// deadline is closed when the shutdown budget is spent.
	deadline <-chan struct{}
}

func (b *batchLoop[T]) run() {
	defer close(b.done)
	for {
		select {
		case <-b.stop:
			b.flush()
			return
		default:
		}
		n, oldest := b.q.state()
		if n == 0 {
			select {
			case <-b.q.notify:
				continue
			case <-b.stop:
				b.flush()
				return
			}
		}
		if n < b.size {
			if wait := b.interval - b.now().Sub(oldest); wait > 0 {
				t := time.NewTimer(wait)
				select {
				case <-b.q.notify:
					t.Stop()
					continue
				case <-t.C:
				case <-b.stop:
					t.Stop()
					b.flush()
					return
				}
			}
		}
		if batch := b.q.take(b.size); len(batch) > 0 {
			b.deliver(batch, false)
		}
	}
}

// flush gives every queued batch one attempt until the deadline; the rest is
// counted as dropped.
func (b *batchLoop[T]) flush() {
	for b.q.length() > 0 {
		select {
		case <-b.deadline:
			b.q.discard()
			return
		default:
		}
		b.deliver(b.q.take(b.size), true)
	}
}

// signalStats is one signal's cumulative counters.
type signalStats struct {
	exported, rejected, failed, droppedQueue atomic.Uint64
	reqSuccess, reqRetryable, reqPermanent   atomic.Uint64
	lastSuccess                              atomic.Int64 // UnixNano, 0 before the first

	mu        sync.Mutex
	lastError string
}

func (s *signalStats) setError(msg string) {
	s.mu.Lock()
	s.lastError = capString(redact.String(msg), attrCap)
	s.mu.Unlock()
}

// SignalStats is one signal's self-telemetry snapshot (SPEC-0014 REQ-12).
// Units are log records, spans or lines.
type SignalStats struct {
	Exported        uint64
	Rejected        uint64
	Failed          uint64
	DroppedQueue    uint64
	DroppedObserver uint64

	RequestsSuccess   uint64
	RequestsRetryable uint64
	RequestsPermanent uint64

	QueueLength int
	// LastSuccess is zero until the first successful delivery.
	LastSuccess time.Time
	// LastError is redacted and capped.
	LastError string
}

func (s *signalStats) snapshot() SignalStats {
	out := SignalStats{
		Exported:          s.exported.Load(),
		Rejected:          s.rejected.Load(),
		Failed:            s.failed.Load(),
		DroppedQueue:      s.droppedQueue.Load(),
		RequestsSuccess:   s.reqSuccess.Load(),
		RequestsRetryable: s.reqRetryable.Load(),
		RequestsPermanent: s.reqPermanent.Load(),
	}
	if ns := s.lastSuccess.Load(); ns != 0 {
		out.LastSuccess = time.Unix(0, ns)
	}
	s.mu.Lock()
	out.LastError = s.lastError
	s.mu.Unlock()
	return out
}

// failureLog rate-limits export failure lines.
type failureLog struct {
	log *log.Logger
	now func() time.Time

	mu    sync.Mutex
	last  map[string]time.Time
	count map[string]uint64
}

func newFailureLog(l *log.Logger, now func() time.Time) *failureLog {
	return &failureLog{log: l, now: now, last: map[string]time.Time{}, count: map[string]uint64{}}
}

// failureLogInterval is the REQ-10 limit: one line per signal per kind per
// minute.
const failureLogInterval = time.Minute

// warn logs msg at most once a minute for (signal, kind). keyvals must be
// value-free: a status code, an error class, a count — never a header or a
// body.
func (f *failureLog) warn(signal, kind, msg string, keyvals ...any) {
	key := signal + "\x00" + kind
	now := f.now()
	f.mu.Lock()
	f.count[key]++
	n := f.count[key]
	last, seen := f.last[key]
	if seen && now.Sub(last) < failureLogInterval {
		f.mu.Unlock()
		return
	}
	f.last[key] = now
	f.mu.Unlock()
	f.log.Warn(msg, append([]any{"signal", signal, "kind", kind, "count", n}, keyvals...)...)
}

// sleepCtx waits d or until ctx is done, reporting ctx's error in that case.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
