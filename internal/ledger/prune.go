package ledger

// Retention
//
// The ledger is bounded by time, so a busy harness and a quiet one keep the
// same window, and by size, as a backstop (SPEC-0022 REQ-12). The prune runs
// in the writer goroutine, the one owner of the files, at boot and at each
// UTC day rollover:
//
//  1. every day file whose day ended more than Retention ago goes, oldest
//     first;
//  2. then the oldest remaining files, until the total is within MaxBytes;
//  3. today's file is never deleted, whatever it holds.
//
// A run still open when the file holding its `opened` line is due to go would
// lose its opening, so the prune first writes a fresh `opened` line carrying
// the record as it folds now into today's file, syncs it, and only then
// deletes. Nothing else a record needs is lost with an old file: every later
// line is in a later file.
//
// Governing: SPEC-0022 REQ-12, REQ-19; design "Retention".
//
// @joestump 09/24/2026 - Added for harness#463.

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// Retention defaults (REQ-12).
const (
	DefaultRetention = 90 * 24 * time.Hour
	DefaultMaxBytes  = 256 << 20
)

// SetRetention changes the limits the next prune applies (REQ-19: a reload
// takes effect at the next prune, without reopening the ledger).
func (l *Ledger) SetRetention(retention time.Duration, maxBytes int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if retention > 0 {
		l.opts.Retention = retention
	}
	if maxBytes > 0 {
		l.opts.MaxBytes = maxBytes
	}
}

// OldestDay is the day of the oldest file kept, or zero for an empty ledger.
func (l *Ledger) OldestDay() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.files) == 0 {
		return time.Time{}
	}
	d, _ := dayOf(l.files[0].name)
	return d
}

// pruneIfDue prunes once per UTC day. Writer goroutine only.
func (l *Ledger) pruneIfDue() {
	now := l.opts.Now()
	day := startOfDay(now)
	if day.Equal(l.prunedDay) {
		return
	}
	if err := l.prune(now); err != nil {
		l.log.Error("ledger prune incomplete; retrying at the next rollover", "err", err)
		// Leave prunedDay alone only for a carry-forward that could not be
		// written: deleting a file whose open run was not carried forward is
		// the one loss this avoids.
		return
	}
	l.prunedDay = day
}

// prune applies retention then the size cap. Writer goroutine only.
func (l *Ledger) prune(now time.Time) error {
	l.mu.Lock()
	files := slices.Clone(l.files)
	retention, maxBytes := l.opts.Retention, l.opts.MaxBytes
	l.mu.Unlock()
	today := dayName(now)

	sizes := make(map[string]int64, len(files))
	var total int64
	for _, df := range files {
		if n, err := statFile(filepath.Join(l.dir, df.name)); err == nil {
			sizes[df.name] = n
			total += n
		}
	}
	var doomed []string
	cutoff := now.Add(-retention)
	for _, df := range files {
		if df.name == today {
			continue
		}
		d, ok := dayOf(df.name)
		if !ok {
			continue
		}
		expired := d.Add(24 * time.Hour).Before(cutoff)
		if expired || total > maxBytes {
			doomed = append(doomed, df.name)
			total -= sizes[df.name]
		}
	}
	var errs []error
	for _, name := range doomed {
		if err := l.carryForward(name, now); err != nil {
			errs = append(errs, err)
			return errors.Join(errs...) // stop: later files are newer, keep them all
		}
		if l.f != nil && l.fday == name {
			l.closeFile()
		}
		if err := os.Remove(filepath.Join(l.dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
			continue
		}
		l.mu.Lock()
		l.files = slices.DeleteFunc(l.files, func(df dayFile) bool { return df.name == name })
		l.stats.Pruned++
		l.mu.Unlock()
	}
	return errors.Join(errs...)
}

// carryForward writes a fresh `opened` line into today's file, synced, for
// every run still open whose opening line is in file name.
func (l *Ledger) carryForward(name string, now time.Time) error {
	var ids []key
	if _, err := scanFile(filepath.Join(l.dir, name), func(ln Line) {
		if ln.Type == TypeOpened {
			ids = append(ids, key{ln.Harness, ln.RunID})
		}
	}); err != nil {
		return err
	}
	l.mu.Lock()
	var carry []*pending
	for _, k := range ids {
		f := l.idx.recs[k]
		if f == nil || !f.Open() {
			continue
		}
		ln := Line{V: Version, Seq: l.nextSeq, Type: TypeOpened, At: now.UTC(), Harness: k.harness, RunID: k.id, Record: f.Record}
		ln.LogPruned = false
		data, err := encode(ln)
		if err != nil {
			l.mu.Unlock()
			return err
		}
		l.nextSeq++
		p := &pending{line: ln, data: data, sync: true, res: make(chan error, 1)}
		l.queue = append(l.queue, p)
		carry = append(carry, p)
	}
	batch := slices.Clone(l.queue)
	l.mu.Unlock()
	if len(carry) == 0 {
		return nil
	}
	// Write through everything queued, the carried lines included, and sync:
	// the old file must not go before its open runs are safe elsewhere.
	n, err := l.writeBatch(batch)
	l.mu.Lock()
	l.commitLocked(n)
	l.stats.CarriedForward += uint64(min(n, len(carry)))
	l.mu.Unlock()
	if err != nil {
		return err
	}
	for _, p := range carry {
		if !p.written {
			return errorf(ErrLedgerUnavailable, p.line, "carry-forward not written")
		}
	}
	return nil
}
