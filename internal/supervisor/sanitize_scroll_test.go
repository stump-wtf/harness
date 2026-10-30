package supervisor

// Governing: ADR-0058 (proposed — a scroll shift is believed only from enough comparable
// rows, and the smallest believed shift wins), ADR-0007 (the durable log holds
// the rows that scrolled off). Issue #1021: asking the shift search for its
// largest match let periodic or near-identical screen content win a shift of
// nearly the whole screen (one row was enough "evidence") and the re-emitted
// prefix was rows still on screen, so `harness logs` showed segments of the
// same line repeated, out of order.

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// screenRows builds a full screen of n distinct content rows.
func screenRows(n int) []string {
	rows := make([]string, n)
	for i := range rows {
		rows[i] = fmt.Sprintf("row %02d", i)
	}
	return rows
}

// periodicRows builds n rows alternating between two strings, the shape a
// program makes when it prints the same separator pair over and over. Every
// odd shift of such a screen lines up on the rows it leaves to compare.
func periodicRows(n int) []string {
	rows := make([]string, n)
	for i := range rows {
		rows[i] = periodicRow(i)
	}
	return rows
}

func periodicRow(i int) string {
	if i%2 == 0 {
		return "================ separator ================"
	}
	return "---------------- rule -----------------"
}

// scrolled returns the screen a program shows after shifting rows up by k and
// painting k fresh rows of new content at the bottom.
func scrolled(rows []string, k int, fresh func(i int) string) []string {
	out := make([]string, 0, len(rows))
	out = append(out, rows[k:]...)
	for i := 0; i < k; i++ {
		out = append(out, fresh(i))
	}
	return out
}

func TestScrollCountMeasuresShifts(t *testing.T) {
	const h = 24
	all := screenRows(h)
	periodic := periodicRows(h)

	tests := []struct {
		name string
		prev []string
		cur  []string
		want int
	}{
		{
			name: "one printed line",
			prev: all,
			cur:  scrolled(all, 1, func(int) string { return "fresh" }),
			want: 1,
		},
		{
			name: "one printed line over a blank tail",
			prev: append(screenRows(21), "", "", ""),
			cur:  append(scrolled(screenRows(21), 1, func(int) string { return "fresh" }), "", ""),
			want: 1,
		},
		{
			name: "folded bottom row on a five-row screen (#1023)",
			prev: screenRows(5),
			cur:  []string{"row 01", "row 02", "row 03", "row 04fold", ""},
			want: 1,
		},
		{
			name: "eight rows painted at once",
			prev: all,
			cur:  scrolled(all, 8, func(i int) string { return fmt.Sprintf("new %d", i) }),
			want: 8,
		},
		{
			name: "periodic content scrolled by one row (#1021)",
			prev: periodic,
			cur:  scrolled(periodic, 1, func(int) string { return periodicRow(h) }),
			want: 1,
		},
		{
			name: "periodic content scrolled by nine rows",
			prev: periodic,
			cur:  scrolled(periodic, 9, func(i int) string { return fmt.Sprintf("new %d", i) }),
			want: 9,
		},
		{
			name: "shift proven by a single row cannot win",
			prev: all,
			cur:  append([]string{all[h-1]}, screenRows(h-1)...),
			want: 0,
		},
		{
			name: "single-row proof cannot win on a short screen",
			prev: screenRows(5),
			cur:  append([]string{screenRows(5)[4]}, screenRows(4)...),
			want: 0,
		},
		{
			name: "clear and repaint",
			prev: all,
			cur:  screenRows(h),
			want: 0,
		},
		{
			name: "two content rows on screen",
			prev: append(screenRows(2), make([]string, h-2)...),
			cur:  append([]string{all[1], "fresh"}, make([]string, h-2)...),
			want: 1,
		},
		{
			name: "one content row on screen",
			prev: append(screenRows(1), make([]string, h-1)...),
			cur:  append([]string{"fresh"}, make([]string, h-1)...),
			want: 0,
		},
		{
			name: "empty screens",
			prev: make([]string, h),
			cur:  make([]string, h),
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := scrollCount(tt.prev, tt.cur); got != tt.want {
				t.Errorf("scrollCount = %d, want %d\nprev = %q\ncur  = %q", got, tt.want, tt.prev, tt.cur)
			}
		})
	}
}

// TestScrollCountNeverClaimsStillVisibleRows pins the property #1021 broke,
// across a family of screens and shift distances: whatever the detector
// reports, every row in prev[:k] has to be a row that really left the screen,
// which for a pure shift means the reported distance is never larger than the
// true one.
func TestScrollCountNeverClaimsStillVisibleRows(t *testing.T) {
	screens := []struct {
		name string
		rows []string
	}{
		{"distinct rows", screenRows(24)},
		{"period two", periodicRows(24)},
		{"period one", repeatRows(24, "same row every time")},
		{"period three", repeatRows(24, "a", "b", "c")},
		{"near-identical rows", repeatRows(24, "step done", "step done", "step done ")},
	}
	for _, screen := range screens {
		for trueShift := 1; trueShift < len(screen.rows); trueShift++ {
			cur := scrolled(screen.rows, trueShift, func(i int) string {
				return fmt.Sprintf("fresh %d", i)
			})
			if got := scrollCount(screen.rows, cur); got > trueShift {
				t.Errorf("%s scrolled by %d: scrollCount = %d, re-emits %d still-visible row(s)",
					screen.name, trueShift, got, got-trueShift)
			}
		}
	}
}

// TestScrollCountEvidenceBandPinsTheTrade pins the one band where the evidence
// floor costs real rows. On an 80-row screen a genuine scroll by 77-79 rows
// leaves fewer than scrollEvidence rows to compare, so the rule reads the frame
// as a repaint and the rows the scroll evicted are gone from the screen: Flush
// records only what is still on it, so the transcript loses a screenful that
// main logged (the old largest-shift match was proven by two rows). That is
// the trade ADR-0058's Consequences states, and this pin keeps the 0 from
// being mistaken for a detector bug; the shift just below the band is still
// measured exactly.
func TestScrollCountEvidenceBandPinsTheTrade(t *testing.T) {
	const h = 80
	all := screenRows(h)
	for _, tt := range []struct {
		trueShift int
		want      int
	}{
		{76, 76},
		{77, 0},
		{78, 0},
		{79, 0},
	} {
		cur := scrolled(all, tt.trueShift, func(i int) string {
			return fmt.Sprintf("fresh %d", i)
		})
		if got := scrollCount(all, cur); got != tt.want {
			t.Errorf("80-row screen scrolled by %d: scrollCount = %d, want %d", tt.trueShift, got, tt.want)
		}
	}
}

// repeatRows tiles patterns over n rows.
func repeatRows(n int, patterns ...string) []string {
	rows := make([]string, n)
	for i := range rows {
		rows[i] = patterns[i%len(patterns)]
	}
	return rows
}

// TestPtyHistoryPeriodicScrollLogsEachRowOnce is the #1021 regression at the
// stream level: a screen full of period-two separators scrolled one row must
// log the one row that left the screen, not a twenty-row prefix of the
// previous screen. After the run ends, the log holds every printed row exactly
// once, in the order it was printed.
func TestPtyHistoryPeriodicScrollLogsEachRowOnce(t *testing.T) {
	var out bytes.Buffer
	h := newPtyHistory(&out, 80, 24)

	const rows = 24
	for i := 0; i < rows-1; i++ {
		if _, err := h.Write([]byte(periodicRow(i) + "\r\n")); err != nil {
			t.Fatal(err)
		}
	}
	// The 24th row lands on the bottom row, so its newline is what scrolls the
	// screen, by exactly one row.
	if _, err := h.Write([]byte(periodicRow(rows-1) + "\r\n")); err != nil {
		t.Fatal(err)
	}

	if got := strings.Count(out.String(), "\n"); got != 1 {
		t.Fatalf("a one-row scroll logged %d lines, want 1:\n%s", got, out.String())
	}
	if got, want := out.String(), periodicRow(0)+"\n"; got != want {
		t.Fatalf("logged %q, want the row that scrolled off (%q)", got, want)
	}

	h.Flush()
	want := strings.Join(append(periodicRows(rows), ""), "\n")
	if got := out.String(); got != want {
		t.Errorf("log is not the printed rows once each, in order\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// TestPtyHistorySteadyScrollLogsEachRowOnce keeps the fast path honest: an
// ordinary program printing distinct lines one at a time still has each line
// recorded once it scrolls off, with nothing duplicated or reordered.
func TestPtyHistorySteadyScrollLogsEachRowOnce(t *testing.T) {
	var out bytes.Buffer
	h := newPtyHistory(&out, 80, 24)

	const lines = 60
	for i := 0; i < lines; i++ {
		if _, err := h.Write([]byte(fmt.Sprintf("line %03d\r\n", i))); err != nil {
			t.Fatal(err)
		}
	}
	h.Flush()

	want := strings.Join(append(screenLines(lines), ""), "\n")
	if got := out.String(); got != want {
		t.Errorf("log is not every line once, in order\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func screenLines(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("line %03d", i)
	}
	return out
}
