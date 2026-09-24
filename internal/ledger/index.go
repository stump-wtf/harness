package ledger

// The Index
//
// The journal keeps folded records in memory so the questions asked every few
// seconds (`jobs`, `runs NAME`, `trigger --wait`, a coalesced skip) never touch
// disk. It holds every record whose first line falls in the last 7 days
// (Window), and, for each harness, at least its newest 20 records (Tail) however
// old: a weekly job's `harness runs` answers from memory too. Older ranges are
// read from the day files through the same fold.
//
// The index knows what it does NOT hold. floor[h] is a seq below which records
// of harness h may be missing, and globalFloor the same for harnesses it has
// never seen. A query answered from memory is trusted only when those floors
// prove nothing older could have belonged in the answer; otherwise it reads the
// files. That is the difference between "this harness has 3 runs" and "this
// harness has 3 runs that I happen to remember".
//
// Governing: SPEC-0022 REQ-2, REQ-7, REQ-15; design "The fold and the in-memory
// index".

import (
	"cmp"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"time"
)

type key struct {
	harness string
	id      int
}

type index struct {
	recs map[key]*Folded
	// by holds each harness's records ordered by Seq, oldest first.
	by map[string][]*Folded
	// floor[h]: every record of h with Seq >= floor[h] is here. Missing means
	// globalFloor.
	floor map[string]uint64
	// globalFloor: every record with Seq >= globalFloor is here. 1 means the
	// index holds the whole ledger.
	globalFloor uint64
	// windowStart: every record that started at or after it is here. Zero
	// means the whole ledger.
	windowStart time.Time
	// maxRunID is the highest run id seen per harness.
	maxRunID map[string]int
	// trimmedDay is the UTC day the index was last trimmed on.
	trimmedDay time.Time
}

func newIndex() *index {
	return &index{
		recs:        map[key]*Folded{},
		by:          map[string][]*Folded{},
		floor:       map[string]uint64{},
		maxRunID:    map[string]int{},
		globalFloor: 1,
	}
}

// apply folds one line in.
func (x *index) apply(l Line) {
	k := key{l.Harness, l.RunID}
	f := x.recs[k]
	if f == nil {
		f = &Folded{}
		x.recs[k] = f
		f.apply(l)
		list := x.by[l.Harness]
		i, _ := slices.BinarySearchFunc(list, f.Seq, func(e *Folded, s uint64) int { return cmp.Compare(e.Seq, s) })
		x.by[l.Harness] = slices.Insert(list, i, f)
	} else {
		before := f.Seq
		f.apply(l)
		if f.Seq != before {
			slices.SortFunc(x.by[l.Harness], func(a, b *Folded) int { return cmp.Compare(a.Seq, b.Seq) })
		}
	}
	if l.RunID > x.maxRunID[l.Harness] {
		x.maxRunID[l.Harness] = l.RunID
	}
}

func (x *index) floorOf(h string) uint64 {
	if f, ok := x.floor[h]; ok {
		return f
	}
	return x.globalFloor
}

// trim drops closed records that are both outside the window and beyond each
// harness's tail, raising the floors past what it dropped. Open records are
// never dropped: reconciliation and the close path need them.
func (x *index) trim(now time.Time, window time.Duration, tail int) {
	cutoff := now.Add(-window)
	for h, list := range x.by {
		keep := list[:0:0]
		var raised uint64
		for i, f := range list {
			newest := len(list) - i
			if newest > tail && f.Closed && f.when().Before(cutoff) {
				delete(x.recs, key{h, f.RunID})
				raised = max(raised, f.Seq+1)
				continue
			}
			keep = append(keep, f)
		}
		x.by[h] = keep
		if raised > x.floorOf(h) {
			x.floor[h] = raised
			x.globalFloor = max(x.globalFloor, raised)
		}
	}
	if x.windowStart.Before(cutoff) {
		x.windowStart = cutoff
	}
}

// Query selects records (REQ-15).
type Query struct {
	// Names limits the harnesses; empty means every harness.
	Names []string
	// Since and Until bound the record's start (or, lacking one, its first
	// line). Zero means unbounded.
	Since, Until time.Time
	// Outcomes and Triggers keep only records with one of these values.
	Outcomes, Triggers []string
	// Limit caps the result; 0 means unbounded.
	Limit int
	// BeforeSeq keeps only records whose Seq is below it: the paging cursor.
	BeforeSeq uint64
}

func (q Query) match(f *Folded) bool {
	if len(q.Names) > 0 && !slices.Contains(q.Names, f.Harness) {
		return false
	}
	if q.BeforeSeq > 0 && f.Seq >= q.BeforeSeq {
		return false
	}
	w := f.when()
	if !q.Since.IsZero() && w.Before(q.Since) {
		return false
	}
	if !q.Until.IsZero() && w.After(q.Until) {
		return false
	}
	if len(q.Outcomes) > 0 && !slices.Contains(q.Outcomes, f.Outcome) {
		return false
	}
	if len(q.Triggers) > 0 && !slices.Contains(q.Triggers, f.Trigger) {
		return false
	}
	return true
}

// fromMemory answers q from the index, and reports whether the answer is
// complete: whether the floors prove no record in the files could belong in it.
// Caller holds the ledger's lock.
func (x *index) fromMemory(q Query) ([]Folded, bool) {
	var out []Folded
	collect := func(list []*Folded) {
		for _, f := range list {
			if q.match(f) {
				out = append(out, *f)
			}
		}
	}
	thr := uint64(1)
	if len(q.Names) > 0 {
		for _, h := range q.Names {
			collect(x.by[h])
			thr = max(thr, x.floorOf(h))
		}
	} else {
		thr = x.globalFloor
		for h, list := range x.by {
			collect(list)
			thr = max(thr, x.floorOf(h))
		}
	}
	sortNewestFirst(out)
	switch {
	case thr <= 1:
		// The index holds everything there is.
	case !q.Since.IsZero() && !x.windowStart.IsZero() && !q.Since.Before(x.windowStart):
		// The range is inside the window, and the window is whole.
	case q.Limit > 0 && len(out) >= q.Limit && out[q.Limit-1].Seq >= thr:
		// The newest Limit matches are all above every floor, so nothing
		// below a floor could outrank them.
	default:
		return nil, false
	}
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, true
}

func sortNewestFirst(out []Folded) {
	slices.SortFunc(out, func(a, b Folded) int { return cmp.Compare(b.Seq, a.Seq) })
}

// Query returns the records q selects, newest first, and the Seq of the oldest
// one returned (0 when none), which is the next page's BeforeSeq (REQ-15).
// It answers from memory when the index can prove its answer whole, and reads
// the day files otherwise.
func (l *Ledger) Query(q Query) ([]Folded, uint64, error) {
	l.mu.Lock()
	out, ok := l.idx.fromMemory(q)
	l.mu.Unlock()
	if !ok {
		var err error
		out, err = l.fromFiles(q)
		if err != nil {
			return nil, 0, err
		}
	}
	markPruned(out)
	var oldest uint64
	if len(out) > 0 {
		oldest = out[len(out)-1].Seq
	}
	return out, oldest, nil
}

// fromFiles folds every day file, overlays what the index holds (lines still
// queued behind a failing disk exist only there), and applies q.
func (l *Ledger) fromFiles(q Query) ([]Folded, error) {
	l.mu.Lock()
	files := slices.Clone(l.files)
	l.mu.Unlock()
	want := func(h string) bool { return len(q.Names) == 0 || slices.Contains(q.Names, h) }
	recs := map[key]*Folded{}
	for _, df := range files {
		_, err := scanFile(filepath.Join(l.dir, df.name), func(ln Line) {
			if !want(ln.Harness) {
				return
			}
			k := key{ln.Harness, ln.RunID}
			f := recs[k]
			if f == nil {
				f = &Folded{}
				recs[k] = f
			}
			f.apply(ln)
		})
		if err != nil {
			return nil, errors.Join(ErrLedgerUnavailable, err)
		}
	}
	l.mu.Lock()
	for k, mem := range l.idx.recs {
		if !want(k.harness) {
			continue
		}
		f := recs[k]
		if f == nil {
			c := *mem
			recs[k] = &c
			continue
		}
		overlay(f, mem)
	}
	l.mu.Unlock()
	var out []Folded
	for _, f := range recs {
		if q.match(f) {
			out = append(out, *f)
		}
	}
	sortNewestFirst(out)
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

// overlay merges the index's copy of a record over the files' copy: the index
// has seen every line the files have for it since boot, and possibly more.
func overlay(f, mem *Folded) {
	if mem.LastSeq >= f.LastSeq {
		mergeOver(&f.Record, mem.Record)
		f.LastSeq = mem.LastSeq
	} else {
		mergeUnder(&f.Record, mem.Record)
	}
	f.HasOpened = f.HasOpened || mem.HasOpened
	f.Closed = f.Closed || mem.Closed
	if mem.Seq < f.Seq {
		f.Seq, f.FirstAt = mem.Seq, mem.FirstAt
	}
}

// markPruned sets LogPruned on records whose log has been deleted (REQ-12).
// Read, not written: keep_runs deletes the file, and the file's absence is the
// fact, so there is nothing for a line to get wrong.
func markPruned(out []Folded) {
	for i := range out {
		if out[i].Log == "" {
			continue
		}
		if _, err := os.Stat(out[i].Log); errors.Is(err, os.ErrNotExist) {
			out[i].LogPruned = true
		}
	}
}

// Get returns one record.
func (l *Ledger) Get(harness string, id int) (Folded, bool, error) {
	l.mu.Lock()
	f, ok := l.idx.recs[key{harness, id}]
	var c Folded
	if ok {
		c = *f
	}
	l.mu.Unlock()
	if ok && !c.Partial() {
		out := []Folded{c}
		markPruned(out)
		return out[0], true, nil
	}
	recs, err := l.fromFiles(Query{Names: []string{harness}})
	if err != nil {
		return Folded{}, false, err
	}
	for _, r := range recs {
		if r.RunID == id {
			out := []Folded{r}
			markPruned(out)
			return out[0], true, nil
		}
	}
	if ok {
		return c, true, nil
	}
	return Folded{}, false, nil
}

// Records returns every record of harness the index holds, oldest first: the
// last Window of it, and at least its newest Tail.
func (l *Ledger) Records(harness string) []Folded {
	l.mu.Lock()
	defer l.mu.Unlock()
	list := l.idx.by[harness]
	out := make([]Folded, len(list))
	for i, f := range list {
		out[i] = *f
	}
	markPruned(out)
	return out
}

// OpenRecords returns every record the index holds that is opened and not
// closed, oldest first.
func (l *Ledger) OpenRecords() []Folded {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []Folded
	for _, f := range l.idx.recs {
		if f.Open() {
			out = append(out, *f)
		}
	}
	slices.SortFunc(out, func(a, b Folded) int { return cmp.Compare(a.Seq, b.Seq) })
	return out
}

// MaxRunID returns the highest run id the index has seen for harness: a floor
// for the id allocator, so a run id can never be reissued while its record is
// still in the ledger.
func (l *Ledger) MaxRunID(harness string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.idx.maxRunID[harness]
}

// Backfill reads older day files, newest first, until every harness in names
// has at least Tail records in the index or the files run out. It runs once at
// boot, after Open and before reconciliation, so an open record older than the
// window (a resident up for a fortnight) is found and closed rather than left
// open forever.
func (l *Ledger) Backfill(names []string) error {
	l.mu.Lock()
	unread := l.files[:len(l.files)-l.scanned]
	unread = slices.Clone(unread)
	want := map[string]bool{}
	for _, h := range names {
		if len(l.idx.by[h]) < l.opts.Tail {
			want[h] = true
		}
	}
	l.mu.Unlock()
	var errs []error
	for i := len(unread) - 1; i >= 0 && len(want) > 0; i-- {
		fs, skipped, err := l.readInto(unread[i].name, want, nil)
		if err != nil {
			errs = append(errs, err)
		}
		l.mu.Lock()
		l.stats.Skipped += skipped
		for j := range l.files {
			if l.files[j].name == unread[i].name && l.files[j].firstSeq == 0 {
				l.files[j].firstSeq = fs
			}
		}
		for h := range want {
			if fs > 0 {
				l.idx.floor[h] = fs
			}
			if len(l.idx.by[h]) >= l.opts.Tail {
				delete(want, h)
			}
		}
		l.mu.Unlock()
	}
	l.mu.Lock()
	for h := range want {
		l.idx.floor[h] = 1 // read everything there is
	}
	l.mu.Unlock()
	return errors.Join(errs...)
}
