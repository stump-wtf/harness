package telemetry

// Events File
//
// The zero-network path: every contributing item appended to a local JSONL
// file as one whole line, for Vector, Fluent Bit or Promtail/Alloy to tail
// (SPEC-0015 REQ-8). The file is 0600 and any directory created for it 0700 —
// it is a persistent copy of transcript content.
//
// Rotation is by rename, which is what inode-tracking tailers expect
// (copy-truncate loses lines): when appending a batch would take the file past
// events_file_max_mb, events.jsonl.(n-1) becomes .n down to events.jsonl →
// .1, the file beyond events_file_keep is deleted, and a fresh file is
// opened. A batch is written with a single write of whole lines, so a tailer
// never reads half a record followed by another.
//
// A write failure never stops the daemon or another signal: the batch counts
// as failed, the error is logged (rate-limited), the file is closed, and the
// next batch tries to open it again.
//
// Governing: ADR-0022; SPEC-0015 REQ-8, REQ-11.
//
// @joestump-agent 09/21/2026 - Added for harness#391.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// eventsFile is the rotating writer. It is used from one goroutine only.
type eventsFile struct {
	path     string
	maxBytes int64
	keep     int

	f    *os.File
	size int64
}

func newEventsFile(path string, maxMB, keep int) *eventsFile {
	return &eventsFile{path: path, maxBytes: int64(maxMB) << 20, keep: keep}
}

// open opens (creating) the file for append.
func (w *eventsFile) open() error {
	if w.f != nil {
		return nil
	}
	dir := filepath.Dir(w.path)
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create events file directory: %w", err)
		}
	}
	f, err := os.OpenFile(w.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("open events file: %w", err)
	}
	// The file may predate this daemon with a looser mode; it is ours.
	_ = f.Chmod(0o600)
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("stat events file: %w", err)
	}
	w.f, w.size = f, info.Size()
	return nil
}

// write appends data, whole lines only, rotating first when it would overflow.
func (w *eventsFile) write(data []byte) error {
	if err := w.open(); err != nil {
		return err
	}
	if w.size > 0 && w.size+int64(len(data)) > w.maxBytes {
		if err := w.rotate(); err != nil {
			w.close()
			return err
		}
		if err := w.open(); err != nil {
			return err
		}
	}
	n, err := w.f.Write(data)
	w.size += int64(n)
	if err != nil {
		w.close()
		return fmt.Errorf("write events file: %w", err)
	}
	return nil
}

// rotate shifts events.jsonl → .1 → … → .keep, dropping what falls off.
func (w *eventsFile) rotate() error {
	w.close()
	if err := os.Remove(w.rotated(w.keep)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("rotate events file: %w", err)
	}
	for i := w.keep - 1; i >= 1; i-- {
		if err := os.Rename(w.rotated(i), w.rotated(i+1)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("rotate events file: %w", err)
		}
	}
	if err := os.Rename(w.path, w.rotated(1)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("rotate events file: %w", err)
	}
	return nil
}

func (w *eventsFile) rotated(i int) string { return fmt.Sprintf("%s.%d", w.path, i) }

// flushClose syncs and closes the file.
func (w *eventsFile) flushClose() error {
	if w.f == nil {
		return nil
	}
	err := w.f.Sync()
	if cerr := w.f.Close(); err == nil {
		err = cerr
	}
	w.f, w.size = nil, 0
	return err
}

func (w *eventsFile) close() {
	if w.f != nil {
		_ = w.f.Close()
		w.f, w.size = nil, 0
	}
}
