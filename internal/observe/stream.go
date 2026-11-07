package observe

// Stream Ingest
//
// A structured one-shot's stream is the record of what its run did: the
// agent prints every call and every result to stdout, and agent-trace
// normalizes those lines (SPEC-0006: the daemon holds no stream parser of its
// own). What it normalizes to belongs on this observer's fan-out like
// anything read from a store — redacted, attributed, delivered once — because
// a pipe run's transcript may never be flushed, and the daemon that has the
// exact record must not report less than the poller would (issue #995;
// ADR-0033 "One activity stream per session").
//
// Attribution here is not the store's puzzle. The bytes arrive from a run the
// supervisor is watching, so that run is the author and its harness is the
// answer: no candidate sets, no Slack, no floor but the daemon's own start.
// The claim a stream opens on its session is what keeps the count exact.
// While a session is claimed, the poller leaves it alone — it does not
// discover it, and it delivers nothing it reads from it — so a Claude run
// that streams its calls and writes a transcript of the same session is
// counted once, from the stream. When the stream ends the claim is released,
// the session gets a floor at that instant, and a later run that resumes the
// session is read from the store again (ADR-0033 "One source per session": a
// session announced on a run's stdout is claimed by that run).
//
// Nothing here slows a run. Lines move through a bounded queue; a queue that
// is full drops and counts, never blocks the pipe reader (ADR-0007). Every
// string still passes through redact on its way out, because the fan-out
// reaches consumers that ship events off the host (ADR-0008). A run whose
// adapter declares no stream parser is drained and counted, never parsed by
// hand.
//
// The run's final result record is deliberately not turned into an event
// here: the run's cost belongs to its ledger line, which is folded from the
// sealed stream at the run's end (issue #994), not to the event bus.
//
// Governing: issue #995, #915; ADR-0033 "Structured one-shots run on pipes",
// "One activity stream per session", "Normalization belongs to agent-trace";
// SPEC-0006 REQ "Run Correlation"; ADR-0007; ADR-0008.
//
// @joestump-agent 10/26/2026 - Added for stump.wtf/harness#995.

import (
	"context"
	"io"
	"strconv"
	"sync"
	"time"

	"github.com/stump-wtf/agent-trace/classify"
	"github.com/stump-wtf/agent-trace/redact"
	"github.com/stump-wtf/agent-trace/tail"

	"github.com/stump-wtf/harness/internal/runtrace"
	"github.com/stump-wtf/harness/internal/supervisor"
)

// Origin says which record an Event was read from. It is what lets a
// consumer tell a run's own stream from the transcript of the same session:
// both carry tool calls, and only one of them is the run Harness spawned
// (ADR-0033 "One activity stream per session").
type Origin string

const (
	// OriginTranscript is an item read by the poller from an agent-trace
	// store — the route a PTY-supervised harness is observed through.
	OriginTranscript Origin = "transcript"
	// OriginStream is an item normalized from a pipe run's live stdout.
	OriginStream Origin = "stream"
)

// streamQueueLines is how many of one run's stream lines wait for the
// parser. A line is one record, and the parser consumes lines as fast as
// they arrive, so a queue this deep is a parser that has stopped, not a run
// that is fast; past it lines drop and are counted rather than buffered.
const streamQueueLines = 512

// claim is one session a run's stream has taken over. The poller suppresses
// a claimed session; ended says its stream finished, and the scan that finds
// an ended claim hands the session back to the poller with a floor there.
type claim struct {
	harness   string
	runID     int
	claimedAt time.Time
	ended     time.Time
}

// streamFeed is one run's open ingest: the supervisor's sink on one side
// (supervisor.StreamSink), the parser's reader on the other.
type streamFeed struct {
	o       *Observer
	harness string
	runID   int
	ch      chan string
	once    sync.Once
}

// OpenStream opens the ingest for run runID of harness: the sink the
// supervisor hands to that run's stdout reader (supervisor.Manager.
// SetStreamIngestSink). Lines may be handed before the run's first scan; the
// feed parses on its own goroutine, so a run is never paced by a scan.
func (o *Observer) OpenStream(harness string, runID int) supervisor.StreamSink {
	feed := &streamFeed{
		o:       o,
		harness: harness,
		runID:   runID,
		ch:      make(chan string, streamQueueLines),
	}
	key := streamKey(harness, runID)
	o.streamsMu.Lock()
	if old, ok := o.streams[key]; ok {
		// One run id feeding twice is a run that never ended; the old feed
		// must go, or two parsers would fan the same session out.
		old.Close()
	}
	o.streams[key] = feed
	o.streamsMu.Unlock()
	o.count(func(s *Stats) { s.Streams++ })
	go o.pumpStream(feed)
	return feed
}

// CloseStream ends every open feed. The supervisor closes each feed with its
// run; this is for a shutdown that ends the observer first.
func (o *Observer) CloseStream() {
	o.streamsMu.Lock()
	feeds := make([]*streamFeed, 0, len(o.streams))
	for key, f := range o.streams {
		feeds = append(feeds, f)
		delete(o.streams, key)
	}
	o.streamsMu.Unlock()
	for _, f := range feeds {
		f.Close()
	}
}

// Line hands one masked line to the feed. It never blocks: a consumer that
// cannot keep up loses lines, and the loss is counted here, where it happens
// (ADR-0007).
func (f *streamFeed) Line(line string) {
	select {
	case f.ch <- line:
	default:
		f.o.count(func(s *Stats) { s.StreamDropped++ })
	}
}

// Close ends the feed's stream. Whichever of the run's readers and the run's
// end reaches it first wins; the second call is a no-op.
func (f *streamFeed) Close() {
	f.once.Do(func() { close(f.ch) })
}

// pumpStream normalizes one run's stream and fans it out. It runs on its own
// goroutine for the life of the run.
func (o *Observer) pumpStream(f *streamFeed) {
	key := streamKey(f.harness, f.runID)
	defer func() {
		o.streamsMu.Lock()
		if cur, ok := o.streams[key]; ok && cur == f {
			delete(o.streams, key)
		}
		o.streamsMu.Unlock()
		o.count(func(s *Stats) { s.Streams-- })
	}()

	h, _, ok := o.src.HarnessRecord(f.harness)
	if !ok {
		// A run the daemon no longer declares: nothing can be attributed, so
		// the lines are drained and counted. Parsing them would fan out
		// events with no harness to carry.
		o.drain(f)
		o.count(func(s *Stats) { s.Unattributed++ })
		o.log.Debug("agent observer: stream from an undeclared harness", "harness", f.harness, "run_id", f.runID)
		return
	}
	parser := o.opts.StreamParser(h)
	if parser == nil {
		// RunsOnPipes is what got us here, and it requires a declared
		// stream, so this is a config that changed under a running harness.
		o.drain(f)
		o.count(func(s *Stats) { s.StreamParseErrors++ })
		o.log.Debug("agent observer: no stream parser for harness", "harness", f.harness, "adapter", h.TrajectoryKind())
		return
	}

	var (
		sess tail.SessionMeta
		now  = o.opts.Now
	)
	handler := tail.StreamHandler{
		Meta: func(m tail.SessionMeta) {
			if m.ID == "" {
				return
			}
			if sess.ID == "" {
				sess = m
				o.claimStream(m, f, now())
			}
		},
		Event: func(ev classify.Event) { o.publishStream(f, sess, now, false, ev, classify.Mark{}) },
		Mark:  func(mk classify.Mark) { o.publishStream(f, sess, now, true, classify.Event{}, mk) },
	}
	meta, _, err := parser.ParseStream(o.ctx, &lineReader{ch: f.ch, ctx: o.ctx}, handler)
	if sess.ID == "" && meta.ID != "" {
		// A stream that names its session only in its final records still
		// claims it, so the transcript cannot be read as though it were new.
		sess = meta
		o.claimStream(meta, f, now())
	}
	if err != nil {
		o.count(func(s *Stats) { s.StreamParseErrors++ })
		o.log.Debug("agent observer: stream parse failed", "harness", f.harness, "run_id", f.runID, "err", err)
	}
	o.endStream(f, sess, now())
}

// drain consumes a feed nobody is parsing, so the run's reader never stalls
// on a full queue.
func (o *Observer) drain(f *streamFeed) {
	for range f.ch {
	}
}

// publishStream delivers one normalized item, attributed to the run that
// printed it.
func (o *Observer) publishStream(f *streamFeed, sess tail.SessionMeta, now func() time.Time, mark bool, ev classify.Event, mk classify.Mark) {
	observed := now()
	seen := observed
	if at, ok := parseTime(itemStamp(mark, ev, mk)); ok {
		if at.Before(o.opts.Since.Add(-runtrace.Slack)) {
			return // a record older than this daemon; the stream is live, not history
		}
		seen = at
	}
	h, _, ok := o.src.HarnessRecord(f.harness)
	if !ok {
		o.count(func(s *Stats) { s.Unattributed++ })
		return
	}
	meta := sess
	meta.Title = redact.Redact(meta.Title)
	out := Event{
		Harness:    f.harness,
		Adapter:    h.TrajectoryKind(),
		Session:    meta,
		Time:       seen,
		ObservedAt: observed,
		Origin:     OriginStream,
	}
	if mark {
		out.Kind, out.Mark = KindMark, redactMark(mk)
	} else {
		out.Kind, out.Tool = KindTool, redactTool(ev)
	}
	o.publish([]Event{out})
	o.count(func(s *Stats) { s.StreamItems++ })
}

// claimStream records that run f holds the session m names, so the poller
// stops delivering that session's transcript items (issue #995).
func (o *Observer) claimStream(m tail.SessionMeta, f *streamFeed, now time.Time) {
	id := string(m.Harness) + "/" + m.ID
	o.claimsMu.Lock()
	contested := false
	if c, ok := o.claims[id]; ok && c.ended.IsZero() && c.harness != f.harness {
		// Two live runs claim one session. The first keeps it — it is the one
		// the poller has been told to suppress — and the second is counted
		// where a peer always is.
		contested = true
	} else {
		o.claims[id] = claim{harness: f.harness, runID: f.runID, claimedAt: now}
	}
	o.claimsMu.Unlock()
	if contested {
		o.count(func(s *Stats) { s.Ambiguous++ })
		return
	}
	o.log.Debug("agent observer: session claimed by a run's stream", "session", id, "harness", f.harness, "run_id", f.runID)
}

// endStream releases the claim the feed held, so the scan that notices it
// hands the session back to the poller with a floor at this instant.
func (o *Observer) endStream(f *streamFeed, sess tail.SessionMeta, now time.Time) {
	if sess.ID == "" {
		return
	}
	id := string(sess.Harness) + "/" + sess.ID
	o.claimsMu.Lock()
	defer o.claimsMu.Unlock()
	if c, ok := o.claims[id]; ok && c.harness == f.harness && c.runID == f.runID {
		c.ended = now
		o.claims[id] = c
	}
}

// claimIsOpen reports whether a run's live stream is what feeds session id,
// which is the poller's cue to leave it alone.
func (o *Observer) claimIsOpen(id string) bool {
	o.claimsMu.Lock()
	defer o.claimsMu.Unlock()
	c, ok := o.claims[id]
	return ok && c.ended.IsZero()
}

// releaseClaims hands back every session whose stream has ended, and reports
// the floor each one comes back with. It runs on the scan goroutine, which is
// the only one that touches the tracked sessions and the tombstones.
func (o *Observer) releaseClaims(now time.Time) map[string]time.Time {
	o.claimsMu.Lock()
	released := make(map[string]time.Time)
	for id, c := range o.claims {
		if c.ended.IsZero() {
			continue
		}
		released[id] = c.ended
		delete(o.claims, id)
	}
	o.claimsMu.Unlock()
	return released
}

func streamKey(harness string, runID int) string {
	return harness + "/" + strconv.Itoa(runID)
}

// itemStamp is a normalized item's own timestamp, whichever payload is set.
func itemStamp(mark bool, ev classify.Event, mk classify.Mark) string {
	if mark {
		return mk.Timestamp
	}
	return ev.Timestamp
}

// lineReader turns a feed's lines into the io.Reader agent-trace's stream
// normalizer wants, holding one line at a time. EOF is the run's stream
// ending — or the observer stopping, which is the same answer to a parser.
type lineReader struct {
	ch  chan string
	ctx context.Context
	buf []byte
}

func (r *lineReader) Read(p []byte) (int, error) {
	for len(r.buf) == 0 {
		select {
		case line, ok := <-r.ch:
			if !ok {
				return 0, io.EOF
			}
			r.buf = append([]byte(line), '\n')
		case <-r.ctx.Done():
			return 0, io.EOF
		}
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}
