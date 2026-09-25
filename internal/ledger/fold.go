package ledger

// The Fold
//
// A record is the fold, in seq order, of every line with its (harness, run_id):
// later values replace earlier ones field by field (REQ-2). "A field the line
// sets" is "a field that is not its zero value", which is what omitempty writes,
// so the merge walks Record's fields by reflection rather than by a hand-kept
// list that a new field could be forgotten from.
//
// Reading tolerates the ledger it was designed to tolerate: a line that does
// not parse (a torn tail from a daemon killed mid-write, or a disk that lost a
// block) is skipped and counted, never fatal, and every complete line around it
// still folds. Unknown fields are ignored, so a newer daemon's lines fold in an
// older reader.
//
// Governing: SPEC-0022 REQ-2, REQ-4 (field caps), REQ-20.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"reflect"
	"slices"
	"unicode/utf8"
)

// apply folds line l into f, which is the zero Folded for a record's first
// line.
func (f *Folded) apply(l Line) {
	if f.Seq == 0 || l.Seq < f.Seq {
		f.Seq = l.Seq
		f.FirstAt = l.At
	}
	f.Harness, f.RunID = l.Harness, l.RunID
	if l.Seq >= f.LastSeq {
		f.LastSeq = l.Seq
		mergeOver(&f.Record, l.Record)
	} else {
		// An older line arriving after a newer one: backfill (index.go) reads
		// older files after newer ones. It may only fill what nothing later
		// has set.
		mergeUnder(&f.Record, l.Record)
	}
	switch l.Type {
	case TypeOpened:
		f.HasOpened = true
	case TypeClosed, TypeDecided:
		f.Closed = true
	}
}

// mergeOver copies every field src sets onto dst.
func mergeOver(dst *Record, src Record) {
	d, s := reflect.ValueOf(dst).Elem(), reflect.ValueOf(src)
	for i := range s.NumField() {
		if v := s.Field(i); !v.IsZero() {
			d.Field(i).Set(v)
		}
	}
}

// mergeUnder copies the fields src sets onto dst only where dst has none.
func mergeUnder(dst *Record, src Record) {
	d, s := reflect.ValueOf(dst).Elem(), reflect.ValueOf(src)
	for i := range s.NumField() {
		if v := s.Field(i); !v.IsZero() && d.Field(i).IsZero() {
			d.Field(i).Set(v)
		}
	}
}

// capRecord bounds r to REQ-4's limits in place, and returns how many fields it
// cut. A cut is counted, never an error: a 4 KiB todo id is somebody else's
// mistake, and the run it belongs to still deserves a record.
func capRecord(r *Record) int {
	n := 0
	v := reflect.ValueOf(r).Elem()
	for i := range v.NumField() {
		if f := v.Field(i); f.Kind() == reflect.String {
			s, cut := capString(f.String())
			if cut {
				f.SetString(s)
				n++
			}
		}
	}
	if len(r.Models) > MaxListEntries {
		r.Models = r.Models[:MaxListEntries]
		n++
	}
	for i := range r.Models {
		n += capInto(&r.Models[i].Model) + capInto(&r.Models[i].Provider)
	}
	if len(r.Sessions) > MaxListEntries {
		r.Sessions = r.Sessions[:MaxListEntries]
		n++
	}
	for i := range r.Sessions {
		n += capInto(&r.Sessions[i].ID) + capInto(&r.Sessions[i].Adapter) + capInto(&r.Sessions[i].TraceID)
	}
	if r.Mismatch != nil {
		m := *r.Mismatch
		n += capInto(&m.Kind) + capInto(&m.ServedModel) + capInto(&m.ServedProvider)
		r.Mismatch = &m
	}
	if len(r.Errors) > 0 {
		// Classes are a fixed vocabulary (SPEC-0013 REQ-3), so this only
		// fires on a bug upstream; keep the first classes in sorted order so
		// the cut is at least deterministic.
		keys := slices.Sorted(maps.Keys(r.Errors))
		out := make(map[string]int, min(len(keys), MaxListEntries))
		for i, k := range keys {
			if i == MaxListEntries {
				n++
				break
			}
			ck, cut := capString(k)
			if cut {
				n++
			}
			out[ck] += r.Errors[k]
		}
		r.Errors = out
	}
	return n
}

func capInto(s *string) int {
	c, cut := capString(*s)
	if !cut {
		return 0
	}
	*s = c
	return 1
}

// capString cuts s to MaxStringBytes on a rune boundary.
func capString(s string) (string, bool) {
	if len(s) <= MaxStringBytes {
		return s, false
	}
	cut := MaxStringBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

// encode renders l as one JSONL line, newline included.
func encode(l Line) ([]byte, error) {
	b, err := json.Marshal(l)
	if err != nil {
		return nil, errors.Join(ErrLedgerCorrupt, err)
	}
	b = append(b, '\n')
	if len(b) > MaxLineBytes {
		return nil, errorf(ErrLedgerCorrupt, l, "line is %d bytes, over the %d cap", len(b), MaxLineBytes)
	}
	return b, nil
}

// scanFile calls fn for every line of the file at path that parses, in file
// order, and returns how many it skipped. A missing file is not an error: a
// prune or a clean ledger can remove one between a listing and the read.
func scanFile(path string, fn func(Line)) (skipped int, err error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	defer func() { _ = f.Close() }()
	return scan(f, fn)
}

// scan is scanFile over any reader.
func scan(r io.Reader, fn func(Line)) (skipped int, err error) {
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		raw, rerr := br.ReadBytes('\n')
		if len(bytes.TrimSpace(raw)) > 0 {
			var l Line
			if json.Unmarshal(raw, &l) != nil || l.Harness == "" || l.RunID < 1 || l.Seq == 0 {
				skipped++
			} else {
				fn(l)
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return skipped, nil
			}
			return skipped, rerr
		}
	}
}
