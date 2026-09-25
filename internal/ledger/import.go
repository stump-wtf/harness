package ledger

// First-Boot Import
//
// Before this ledger, run history lived in state.json (SPEC-0008). The first
// boot of a daemon that has the ledger copies that history in, once, and the
// Manager then drops it from state.json (REQ-13). The marker file makes it
// idempotent; and because a record is a fold, a crash half way through an
// import that is then run again only re-folds the same values into the same
// records, so it cannot double a run.
//
// Governing: SPEC-0022 REQ-13; design "First-boot import".

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// importedMarker names the file whose presence says the import ran.
const importedMarker = ".imported"

// Imported reports whether the first-boot import has run.
func (l *Ledger) Imported() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stats.Imported
}

// Import appends lines, each already carrying Imported and its own At, in time
// order, syncs them, and writes the marker. Each line lands in the day file of
// its At. With no lines it writes only the marker: the import still ran.
func (l *Ledger) Import(lines []Line) error {
	lines = slices.Clone(lines)
	slices.SortStableFunc(lines, func(a, b Line) int { return cmp.Compare(a.At.UnixNano(), b.At.UnixNano()) })
	for i, ln := range lines {
		ln.Imported = true
		last := i == len(lines)-1
		// Only the last line waits: switching day files syncs the one left
		// behind, and the last line's sync covers its own file.
		if _, err := l.Append(ln, last); err != nil {
			return fmt.Errorf("ledger: import %d lines: %w", len(lines), err)
		}
	}
	marker := filepath.Join(l.dir, importedMarker)
	body := fmt.Sprintf("imported %d lines at %s\n", len(lines), l.opts.Now().UTC().Format(time.RFC3339))
	if err := os.WriteFile(marker, []byte(body), 0o600); err != nil {
		return fmt.Errorf("ledger: write %s: %w", marker, err)
	}
	l.mu.Lock()
	l.stats.Imported = true
	l.mu.Unlock()
	return nil
}
