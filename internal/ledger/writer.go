package ledger

// The Writer
//
// One goroutine owns the open day file and writes every line; everything else
// sends it work (REQ-20). Append assigns the seq under the ledger's lock and
// queues the encoded line, so the queue order IS the seq order, and a disk that
// recovers from an outage receives the lines in exactly the order the facts
// happened (REQ-6).
//
// Facts are synced, checkpoints are not. An `opened`, `closed` or `decided`
// line is appended with sync=true: Append waits, up to SyncTimeout (2s), for
// the writer to write it and fdatasync the file. An `updated` line is written
// at once but synced lazily, at least every FlushEvery (30s) and always before
// a later synced line, which fsync covers because it syncs the whole file.
//
// A failed write leaves the line at the head of the queue and retries it with
// backoff. While the head is failing the ledger is degraded, and a synced
// Append fails fast with ErrLedgerUnavailable instead of waiting out the 2s:
// supervision of a harness with no budget must not wait on a retry (REQ-6), and
// a crash-looping resident on a read-only disk would otherwise restart two
// seconds late, every time.
//
// A torn tail is left where it is. Readers skip it (fold.go), and the writer
// starts its next line with a newline when the file does not already end in
// one, so the torn fragment can never swallow the line after it.
//
// Governing: SPEC-0022 REQ-1, REQ-6, REQ-20; ADR-0028.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"charm.land/log/v2"
)

// Defaults for Options.
const (
	DefaultSyncTimeout = 2 * time.Second
	DefaultFlushEvery  = 30 * time.Second
	// DefaultWindow is how far back the in-memory index reaches (design:
	// "The fold and the in-memory index").
	DefaultWindow = 7 * 24 * time.Hour
	// DefaultTail is how many of each harness's newest records the index
	// keeps even when they are older than the window, so `harness runs NAME`
	// and `jobs` answer from memory for a harness that runs weekly.
	DefaultTail = 20
)

// Options tunes a Ledger. The zero value is production.
type Options struct {
	// Now is the clock (default time.Now).
	Now func() time.Time
	// SyncTimeout bounds a synced Append (default 2s).
	SyncTimeout time.Duration
	// FlushEvery bounds how long a written `updated` line waits for a sync
	// (default 30s).
	FlushEvery time.Duration
	// RetryMin and RetryMax bound the backoff between retries of a failed
	// write (default 250ms and 30s).
	RetryMin, RetryMax time.Duration
	// Window and Tail size the in-memory index (defaults 7 days and 20).
	Window time.Duration
	Tail   int
	// Logger receives append failures (default log.Default()).
	Logger *log.Logger
}

func (o Options) normalize() Options {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.SyncTimeout <= 0 {
		o.SyncTimeout = DefaultSyncTimeout
	}
	if o.FlushEvery <= 0 {
		o.FlushEvery = DefaultFlushEvery
	}
	if o.RetryMin <= 0 {
		o.RetryMin = 250 * time.Millisecond
	}
	if o.RetryMax <= 0 {
		o.RetryMax = 30 * time.Second
	}
	if o.Window <= 0 {
		o.Window = DefaultWindow
	}
	if o.Tail <= 0 {
		o.Tail = DefaultTail
	}
	if o.Logger == nil {
		o.Logger = log.Default()
	}
	return o
}

// Stats is what doctor and the metrics collector report about the ledger
// (REQ-18). A snapshot: every field is a copy.
type Stats struct {
	Dir string
	// NextSeq is the seq the next line will carry.
	NextSeq uint64
	// Queued counts lines accepted but not yet written.
	Queued int
	// AppendErrors counts failed writes and syncs, retries included.
	AppendErrors uint64
	// LastError is the most recent write or sync failure, if any.
	LastError   string
	LastErrorAt time.Time
	// Degraded is set while the head of the queue is failing.
	Degraded bool
	// Skipped counts unparseable lines the boot scan and backfill passed
	// over (a torn tail, a lost block).
	Skipped int
	// Truncated counts record fields cut to REQ-4's caps.
	Truncated uint64
	// Imported reports that the first-boot import has run (REQ-13).
	Imported bool
}

// Ledger is the run ledger: the single writer, and the in-memory index of what
// it wrote.
type Ledger struct {
	dir  string
	opts Options
	log  *log.Logger

	mu      sync.Mutex
	nextSeq uint64
	queue   []*pending
	idx     *index
	stats   Stats
	closed  bool
	// files is every day file the ledger knows, oldest first, with the seq of
	// its first line (0 for a file whose first line was not read).
	files []dayFile
	// scanned is how many of files (from the newest) the boot scan read.
	scanned int

	wake chan struct{}
	quit chan struct{}
	done chan struct{}

	// Owned by the writer goroutine.
	f       *os.File
	fday    string
	needNL  bool
	dirty   bool
	dirtyAt time.Time
}

// dayFile is one day file and the seq of its first line.
type dayFile struct {
	name     string
	firstSeq uint64
}

// pending is one queued line.
type pending struct {
	line Line
	data []byte
	sync bool
	// res receives the outcome of a synced line once: nil when it is on
	// stable storage, or the first failure. Buffered, so the writer never
	// blocks on a caller that timed out and left.
	res      chan error
	notified bool
	// written is set by the writer once the line is in the file.
	written bool
}

// Open opens the ledger in dir, creating it (mode 0700) when it does not exist,
// reads the recent day files into the index and recovers the next seq, and
// starts the writer. It does not fail: a directory that cannot be created yet
// (a read-only disk) leaves a ledger that queues lines and retries, which is
// what REQ-6 asks of an outage that starts before boot rather than after it.
// The error it returns is the boot scan's, for the caller to log.
func Open(dir string, opts Options) (*Ledger, error) {
	opts = opts.normalize()
	l := &Ledger{
		dir:  dir,
		opts: opts,
		log:  opts.Logger,
		idx:  newIndex(),
		wake: make(chan struct{}, 1),
		quit: make(chan struct{}),
		done: make(chan struct{}),
	}
	l.stats.Dir = dir
	var errs []error
	if err := os.MkdirAll(dir, 0o700); err != nil {
		errs = append(errs, fmt.Errorf("ledger: create %s: %w", dir, err))
	}
	if _, err := os.Stat(filepath.Join(dir, importedMarker)); err == nil {
		l.stats.Imported = true
	}
	if err := l.boot(); err != nil {
		errs = append(errs, err)
	}
	go l.run()
	return l, errors.Join(errs...)
}

// Dir is the ledger directory.
func (l *Ledger) Dir() string { return l.dir }

// boot lists the day files and reads the recent ones into the index.
func (l *Ledger) boot() error {
	names, err := listDays(l.dir)
	if err != nil {
		return err
	}
	for _, n := range names {
		l.files = append(l.files, dayFile{name: n})
	}
	now := l.opts.Now()
	cutoff := startOfDay(now).Add(-l.opts.Window)
	first := len(names)
	for i, n := range names {
		if d, ok := dayOf(n); ok && !d.Before(cutoff) {
			first = i
			break
		}
	}
	if first == len(names) && len(names) > 0 {
		// Nothing inside the window: read the newest file anyway, for the seq.
		first = len(names) - 1
	}
	var maxSeq uint64
	for i := first; i < len(names); i++ {
		fs, skipped, err := l.readInto(names[i], nil, func(ln Line) {
			if ln.Seq > maxSeq {
				maxSeq = ln.Seq
			}
		})
		l.files[i].firstSeq = fs
		l.stats.Skipped += skipped
		if err != nil {
			return fmt.Errorf("ledger: read %s: %w", names[i], err)
		}
	}
	l.scanned = len(names) - first
	l.nextSeq = maxSeq + 1
	if first > 0 {
		l.idx.globalFloor = l.files[first].firstSeq
		if d, ok := dayOf(names[first]); ok {
			l.idx.windowStart = d
		}
	} else {
		l.idx.globalFloor = 1
	}
	return nil
}

// readInto folds file name into the index, for the harnesses in want (all when
// nil), and returns the seq of its first line.
func (l *Ledger) readInto(name string, want map[string]bool, each func(Line)) (firstSeq uint64, skipped int, err error) {
	var lines []Line
	skipped, err = scanFile(filepath.Join(l.dir, name), func(ln Line) {
		if firstSeq == 0 || ln.Seq < firstSeq {
			firstSeq = ln.Seq
		}
		if each != nil {
			each(ln)
		}
		if want == nil || want[ln.Harness] {
			lines = append(lines, ln)
		}
	})
	l.mu.Lock()
	for _, ln := range lines {
		l.idx.apply(ln)
	}
	l.mu.Unlock()
	return firstSeq, skipped, err
}

// Append queues line ln and returns its seq. With sync, it waits until the
// line is on stable storage, at most SyncTimeout; a line that is not is still
// queued and will be written, in order, and the error says it is late
// (ErrLedgerUnavailable). Without sync it returns at once.
//
// V, Seq and (when zero) At are filled in here; LogPruned is never written.
func (l *Ledger) Append(ln Line, sync bool) (uint64, error) {
	seq, wait, err := l.Enqueue(ln, sync)
	if err != nil {
		return 0, err
	}
	return seq, wait()
}

// Enqueue is Append split in two: it queues the line and returns at once, with
// a wait that blocks as Append would. A caller that must order the line with
// something of its own (the Manager allocating the run id the line carries)
// enqueues under its own lock and waits outside it, so one harness's fsync
// never holds up another's allocation.
func (l *Ledger) Enqueue(ln Line, sync bool) (uint64, func() error, error) {
	switch ln.Type {
	case TypeOpened, TypeUpdated, TypeClosed, TypeDecided:
	default:
		return 0, nil, errorf(ErrLedgerCorrupt, ln, "unknown line type %q", ln.Type)
	}
	if ln.Harness == "" || ln.RunID < 1 {
		return 0, nil, errorf(ErrLedgerCorrupt, ln, "a line needs a harness and a run id")
	}
	ln.V = Version
	if ln.At.IsZero() {
		ln.At = l.opts.Now()
	}
	ln.At = ln.At.UTC()
	ln.LogPruned = false
	cut := capRecord(&ln.Record)
	if n, cutName := capString(ln.Harness); cutName {
		ln.Harness = n
		cut++
	}

	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return 0, nil, errorf(ErrClosed, ln, "append after close")
	}
	ln.Seq = l.nextSeq
	data, err := encode(ln)
	if err != nil {
		l.mu.Unlock()
		return 0, nil, err
	}
	l.nextSeq++
	l.stats.Truncated += uint64(cut)
	p := &pending{line: ln, data: data, sync: sync}
	if sync {
		p.res = make(chan error, 1)
	}
	l.queue = append(l.queue, p)
	l.idx.apply(ln)
	if day := startOfDay(l.opts.Now()); !day.Equal(l.idx.trimmedDay) {
		l.idx.trimmedDay = day
		l.idx.trim(l.opts.Now(), l.opts.Window, l.opts.Tail)
	}
	degraded := l.stats.Degraded
	l.mu.Unlock()
	l.poke()

	wait := func() error {
		if !sync {
			return nil
		}
		if degraded {
			return errorf(ErrLedgerUnavailable, ln, "queued behind a failing write; it will be retried in order")
		}
		t := time.NewTimer(l.opts.SyncTimeout)
		defer t.Stop()
		select {
		case err := <-p.res:
			if err != nil {
				return fmt.Errorf("%w: %w", errorf(ErrLedgerUnavailable, ln, "not written; queued for retry"), err)
			}
			return nil
		case <-t.C:
			return errorf(ErrLedgerUnavailable, ln, "not synced within %s; queued for retry", l.opts.SyncTimeout)
		}
	}
	return ln.Seq, wait, nil
}

func (l *Ledger) poke() {
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

// Stats returns a snapshot of the ledger's counters.
func (l *Ledger) Stats() Stats {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.stats
	s.NextSeq = l.nextSeq
	s.Queued = len(l.queue)
	return s
}

// Close drains the queue, waiting at most timeout, syncs and closes the file.
// Lines still queued when the timeout passes are reported in the error: they
// are lost, and the caller logs how many.
func (l *Ledger) Close(timeout time.Duration) error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	l.mu.Unlock()
	close(l.quit)
	select {
	case <-l.done:
	case <-time.After(timeout):
		return fmt.Errorf("%w: %d lines still queued after %s", ErrLedgerUnavailable, l.Stats().Queued, timeout)
	}
	if q := l.Stats().Queued; q > 0 {
		return fmt.Errorf("%w: %d lines could not be written before shutdown", ErrLedgerUnavailable, q)
	}
	return nil
}

// run is the writer goroutine.
func (l *Ledger) run() {
	defer close(l.done)
	defer l.closeFile()
	backoff := time.Duration(0)
	var flush *time.Timer
	for {
		l.mu.Lock()
		batch := slices.Clone(l.queue)
		l.mu.Unlock()

		if len(batch) > 0 {
			n, err := l.writeBatch(batch)
			l.mu.Lock()
			l.queue = l.queue[n:]
			if err != nil {
				l.stats.AppendErrors++
				l.stats.LastError = err.Error()
				l.stats.LastErrorAt = l.opts.Now()
				l.stats.Degraded = true
				// Everyone waiting behind the failure hears now rather than
				// at their timeout: the head will not move until the disk
				// does.
				for _, p := range l.queue {
					if p.sync && !p.notified {
						p.notified = true
						p.res <- err
					}
				}
			} else {
				l.stats.Degraded = false
			}
			queued := len(l.queue)
			l.mu.Unlock()
			if err != nil {
				head := batch[n].line
				l.log.Error("ledger append failed; retrying in order",
					"harness", head.Harness, "run_id", head.RunID, "seq", head.Seq, "queued", queued, "err", err)
				backoff = min(max(backoff*2, l.opts.RetryMin), l.opts.RetryMax)
				select {
				case <-time.After(backoff):
				case <-l.quit:
					// One last attempt at the drain, then give up: Close
					// reports what is left.
					l.mu.Lock()
					rest := slices.Clone(l.queue)
					l.mu.Unlock()
					if m, err := l.writeBatch(rest); err == nil || m > 0 {
						l.mu.Lock()
						l.queue = l.queue[m:]
						l.mu.Unlock()
					}
					return
				}
				continue
			}
			backoff = 0
			continue
		}

		if l.dirty {
			wait := max(l.opts.FlushEvery-l.opts.Now().Sub(l.dirtyAt), 0)
			if flush == nil {
				flush = time.NewTimer(wait)
			} else {
				flush.Reset(wait)
			}
		}
		var flushC <-chan time.Time
		if flush != nil && l.dirty {
			flushC = flush.C
		}
		select {
		case <-l.wake:
		case <-flushC:
			if err := l.syncFile(); err != nil {
				l.log.Error("ledger sync failed", "err", err)
			}
		case <-l.quit:
			l.mu.Lock()
			empty := len(l.queue) == 0
			l.mu.Unlock()
			if empty {
				return
			}
			// Drain what arrived with the quit; the loop returns once empty.
			select {
			case l.wake <- struct{}{}:
			default:
			}
		}
	}
}

// writeBatch writes lines in order and syncs when any of them asks for it, a
// buffered line has waited FlushEvery, or a write failed after some landed. It
// returns how many leading lines are done (written, and synced when they asked
// to be) and may leave the queue, and the first failure.
//
// A line is written once. One that landed but whose sync failed stays queued
// marked written, and the retry syncs it rather than writing it again: a second
// copy would repeat its seq, which REQ-1 forbids.
func (l *Ledger) writeBatch(batch []*pending) (int, error) {
	w := 0
	var werr error
	for ; w < len(batch); w++ {
		p := batch[w]
		if p.written {
			continue
		}
		if err := l.writeLine(p); err != nil {
			werr = err
			break
		}
		p.written = true
	}
	flushDue := l.dirty && l.opts.Now().Sub(l.dirtyAt) >= l.opts.FlushEvery
	needSync := flushDue || werr != nil || slices.ContainsFunc(batch[:w], func(p *pending) bool { return p.sync })
	if needSync {
		if err := l.syncFile(); err != nil {
			l.dropFile()
			// Buffered lines ahead of the first synced one may leave; the
			// synced ones wait for a sync that succeeds.
			n := slices.IndexFunc(batch[:w], func(p *pending) bool { return p.sync })
			if n < 0 {
				n = w
			}
			return n, err
		}
	}
	if werr != nil {
		// Reopen before the retry, so a short write's torn tail is seen and
		// the retried line starts on a line of its own.
		l.dropFile()
	}
	l.ack(batch[:w])
	return w, werr
}

// ack tells synced lines' callers they are durable.
func (l *Ledger) ack(batch []*pending) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, p := range batch {
		if p.sync && !p.notified {
			p.notified = true
			p.res <- nil
		}
	}
}

// writeLine appends one line to the file of its UTC day.
func (l *Ledger) writeLine(p *pending) error {
	day := dayName(p.line.At)
	if l.f == nil || l.fday != day {
		if err := l.openDay(day, p.line.Seq); err != nil {
			return errorf(ErrLedgerUnavailable, p.line, "%v", err)
		}
	}
	data := p.data
	if l.needNL {
		data = append([]byte{'\n'}, data...)
	}
	if _, err := writeFile(l.f, data); err != nil {
		return errorf(ErrLedgerUnavailable, p.line, "write %s: %v", day, err)
	}
	l.needNL = false
	if !l.dirty {
		l.dirty, l.dirtyAt = true, l.opts.Now()
	}
	return nil
}

// openDay makes day the open file, syncing and closing the previous one.
func (l *Ledger) openDay(day string, seq uint64) error {
	if l.f != nil {
		if err := l.syncFile(); err != nil {
			return err
		}
		l.dropFile()
	}
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", l.dir, err)
	}
	path := filepath.Join(l.dir, day)
	f, err := openFile(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	needNL, err := endsTorn(path)
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("read the tail of %s: %w", path, err)
	}
	l.f, l.fday, l.needNL = f, day, needNL
	l.noteFile(day, seq)
	return nil
}

// noteFile records a day file the writer created or appended to.
func (l *Ledger) noteFile(day string, seq uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	i := sort.Search(len(l.files), func(i int) bool { return l.files[i].name >= day })
	if i < len(l.files) && l.files[i].name == day {
		if l.files[i].firstSeq == 0 {
			l.files[i].firstSeq = seq
		}
		return
	}
	l.files = slices.Insert(l.files, i, dayFile{name: day, firstSeq: seq})
}

// syncFile fdatasyncs the open file if anything was written since the last
// sync. With no file open (a failed write dropped it) there is nothing to sync
// through yet; the dirty mark stays, and the reopened file is synced instead,
// which reaches the same inode.
func (l *Ledger) syncFile() error {
	if l.f == nil || !l.dirty {
		return nil
	}
	if err := syncFileFn(l.f); err != nil {
		return fmt.Errorf("%w: sync %s: %w", ErrLedgerUnavailable, l.fday, err)
	}
	l.dirty = false
	return nil
}

// dropFile closes the open file without syncing it.
func (l *Ledger) dropFile() {
	if l.f != nil {
		_ = l.f.Close()
		l.f = nil
	}
}

// closeFile syncs and closes the open file on the writer's way out. A failed
// sync here is visible in Close's drain count, not in this call.
func (l *Ledger) closeFile() {
	if err := l.syncFile(); err != nil {
		l.log.Error("ledger sync at close failed", "err", err)
	}
	l.dropFile()
}

// openFile opens a day file for appending, creating it private to the daemon's
// user (REQ-1). A variable so a test can fail it, the way a read-only or full
// disk does, without depending on file modes a root CI runner ignores.
var openFile = func(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
}

// writeFile and syncFileFn are the writer's two other disk operations, variables
// for the same reason as openFile.
var (
	writeFile  = func(f *os.File, b []byte) (int, error) { return f.Write(b) }
	syncFileFn = func(f *os.File) error { return f.Sync() }
)

// endsTorn reports a non-empty file whose last byte is not a newline.
func endsTorn(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return false, err
	}
	if st.Size() == 0 {
		return false, nil
	}
	b := make([]byte, 1)
	if _, err := f.ReadAt(b, st.Size()-1); err != nil {
		return false, err
	}
	return b[0] != '\n', nil
}

// listDays returns the day file names in dir, oldest first. A missing
// directory is an empty ledger.
func listDays(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("ledger: list %s: %w", dir, err)
	}
	var out []string
	for _, e := range entries {
		if _, ok := dayOf(e.Name()); ok && !e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

func startOfDay(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// errorf wraps sentinel with the line's identity (REQ-20: harness, run_id and
// seq at each boundary).
func errorf(sentinel error, ln Line, format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	var id []string
	if ln.Harness != "" {
		id = append(id, "harness "+ln.Harness)
	}
	if ln.RunID > 0 {
		id = append(id, fmt.Sprintf("run %d", ln.RunID))
	}
	if ln.Seq > 0 {
		id = append(id, fmt.Sprintf("seq %d", ln.Seq))
	}
	if len(id) > 0 {
		msg = strings.Join(id, ", ") + ": " + msg
	}
	return fmt.Errorf("%w: %s", sentinel, msg)
}
