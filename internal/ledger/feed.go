package ledger

// The Run Feed And Replay
//
// Consumers that count runs (the metrics collector, telemetry export) read
// them here, never from the lifecycle bus: the feed publishes a record only
// once its line is committed, so a count can never include a run the ledger
// could still lose (SPEC-0022 REQ-10, REQ-11).
//
// Subscribe is the lossy half. Publishing never waits: a subscriber whose
// buffer is full misses that record, and the miss is counted against its name
// (Stats.FeedDropped), so a slow consumer costs itself, never supervision.
//
// Since is the lossless half, for a consumer that must see every record (a
// lease completing a todo, a sweep): it replays every committed line after a
// seq, in order, from a ring of the last RingLines lines or, further back,
// from the day files, so a consumer can resume after a restart from the last
// seq it processed.
//
// Governing: SPEC-0022 REQ-10; design "The run feed and replay".
//
// @joestump 09/24/2026 - Added for harness#450.

import (
	"cmp"
	"iter"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

// RingLines is how many recent committed lines Since serves from memory.
const RingLines = 10_000

// Committed is one record as a committed line left it.
type Committed struct {
	// Seq is the committed line's seq.
	Seq uint64
	// Type is the line's type: opened, closed or decided.
	Type Type
	// Record is the run's folded record after the line.
	Record Folded
}

func newFeed() feed {
	return feed{subs: map[*subscriber]struct{}{}, dropped: map[string]uint64{}}
}

// subscriber is one feed consumer.
type subscriber struct {
	name string
	ch   chan Committed
}

// feed is the ledger's subscribers and ring. Guarded by Ledger.mu.
type feed struct {
	subs    map[*subscriber]struct{}
	dropped map[string]uint64
	ring    []Line
}

// Subscribe registers a feed consumer with a buffer of buf records. Records
// arrive in seq order, after their line is committed: opened, closed and
// decided lines (checkpoints are not facts, and nothing counts them). A full
// buffer drops the record for this subscriber and counts it. The cancel
// function unregisters and closes the channel; Close closes it too.
func (l *Ledger) Subscribe(name string, buf int) (<-chan Committed, func()) {
	if buf < 1 {
		buf = 1
	}
	s := &subscriber{name: name, ch: make(chan Committed, buf)}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.feed.dropped[name]; !ok {
		l.feed.dropped[name] = 0
	}
	if l.closed {
		close(s.ch)
		return s.ch, func() {}
	}
	l.feed.subs[s] = struct{}{}
	var once sync.Once
	return s.ch, func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			if _, ok := l.feed.subs[s]; ok {
				delete(l.feed.subs, s)
				close(s.ch)
			}
		})
	}
}

// publishLocked records a committed line in the ring and, for a fact, fans the
// record out. Caller holds mu.
func (l *Ledger) publishLocked(ln Line) {
	l.feed.ring = append(l.feed.ring, ln)
	if over := len(l.feed.ring) - RingLines; over > 0 {
		l.feed.ring = slices.Delete(l.feed.ring, 0, over)
	}
	if ln.Type == TypeUpdated || len(l.feed.subs) == 0 {
		return
	}
	f := l.idx.recs[key{ln.Harness, ln.RunID}]
	if f == nil {
		return
	}
	c := Committed{Seq: ln.Seq, Type: ln.Type, Record: *f}
	for s := range l.feed.subs {
		select {
		case s.ch <- c:
		default:
			l.feed.dropped[s.name]++
		}
	}
}

// closeFeedLocked closes every subscriber at shutdown. Caller holds mu.
func (l *Ledger) closeFeedLocked() {
	for s := range l.feed.subs {
		close(s.ch)
	}
	clear(l.feed.subs)
}

// Since yields every committed line with a seq above seq, in seq order: from
// the ring when it reaches back that far, and otherwise from the day files
// first. A line still queued behind a failing disk is not committed and is not
// yielded; a later Since picks it up. A file that cannot be read is yielded as
// an error and ends the sequence.
func (l *Ledger) Since(seq uint64) iter.Seq2[Line, error] {
	return func(yield func(Line, error) bool) {
		l.mu.Lock()
		ring := slices.Clone(l.feed.ring)
		files := slices.Clone(l.files)
		// Every line below the queue's head is committed; a line at or
		// above it may be on disk but not yet acknowledged, and must not be
		// read ahead of the commit.
		bound := l.nextSeq
		if len(l.queue) > 0 {
			bound = l.queue[0].line.Seq
		}
		l.mu.Unlock()

		last := seq
		if len(ring) == 0 || ring[0].Seq > seq+1 {
			// Older than memory: the files hold every committed line.
			var lines []Line
			for _, df := range files {
				if _, err := scanFile(filepath.Join(l.dir, df.name), func(ln Line) {
					if ln.Seq > seq {
						lines = append(lines, ln)
					}
				}); err != nil {
					yield(Line{}, err)
					return
				}
			}
			slices.SortFunc(lines, func(a, b Line) int { return cmp.Compare(a.Seq, b.Seq) })
			for _, ln := range lines {
				if ln.Seq <= last {
					continue
				}
				if ln.Seq >= bound || (len(ring) > 0 && ln.Seq >= ring[0].Seq) {
					break // the rest is the ring's, or not committed yet
				}
				if !yield(ln, nil) {
					return
				}
				last = ln.Seq
			}
		}
		for _, ln := range ring {
			if ln.Seq <= last {
				continue
			}
			if !yield(ln, nil) {
				return
			}
			last = ln.Seq
		}
	}
}

// FeedDropped returns each subscriber's miss count.
func (l *Ledger) FeedDropped() map[string]uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]uint64, len(l.feed.dropped))
	for k, v := range l.feed.dropped {
		out[k] = v
	}
	return out
}

// statFile is a file's size.
func statFile(path string) (int64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// Bytes is the total size of the ledger's day files (harness_ledger_bytes,
// REQ-11; doctor's size against max_mb, REQ-18).
func (l *Ledger) Bytes() int64 {
	l.mu.Lock()
	files := slices.Clone(l.files)
	l.mu.Unlock()
	var n int64
	for _, df := range files {
		if st, err := statFile(filepath.Join(l.dir, df.name)); err == nil {
			n += st
		}
	}
	return n
}
