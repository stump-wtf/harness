package main

// Production Table Width Guards
//
// The width tests in table_test.go each build their OWN header set, so none of
// them covers a table the CLI actually renders — TestTableRowNeverExceedsBudget
// still uses {NAME, STATE, ENABLED, RESTARTS, DESCRIPTION}, a column list
// `harness list` stopped rendering when ENABLED was dropped for SCHEDULE/NEXT.
// A column added to a production table therefore changes nothing those tests
// can see.
//
// What actually goes wrong is NOT overflow. defaultColumnWidths holds the
// budget: at an 80-column terminal (a 64-cell budget) the six production
// columns resolve to [12 12 18 10 9 3] and a seventh to [12 12 18 10 9 1 2].
// Both sum to exactly 64. The table stays perfectly aligned and becomes
// useless — one character of DESCRIPTION, two of a model name. So the property
// to assert is minimum USABLE width per column, not total width; a width
// assertion cannot fail here, which is how the naive version of this file
// passed while proving nothing.
//
// Governing: issue #343.
//
// @joestump-agent 09/12/2026 - Added while sizing the MODEL column (#343).

import (
	"bytes"
	"testing"
)

// ttyBudget is the cell budget an 80-column terminal actually gets:
// resolveTableWidth applies tableWidthRatio, so 80 columns is a 64-cell
// budget, not 80. A table written to a bytes.Buffer gets defaultTableWidth
// instead, which is why a piped test can look fine while a real terminal is
// unreadable.
const ttyBudget = int(float64(80) * tableWidthRatio)

// minReadable is the narrowest a column may resolve to and still carry
// information. Three cells fits an ellipsis plus a character; below that the
// cell is decoration. DESCRIPTION at 3 is already at this floor today, which
// is precisely why there is no room for a seventh column without paying for
// it.
const minReadable = 3

// productionHeaders are the header sets the CLI renders. Keep in step with
// printHarnessTable (verbs.go) and printJobsTable (jobs.go) — a column added
// there and not here leaves that table unguarded, which is the gap this file
// exists to close.
var productionHeaders = map[string][]string{
	"list": {"NAME", "STATE", "SCHEDULE", "NEXT", "RESTARTS", "DESCRIPTION"},
	"jobs": {"NAME", "STATE", "SCHEDULE", "NEXT", "LAST RUN", "FAILS"},
}

// resolveProductionWidths lays out one production header set at an 80-column
// terminal's budget and returns the resolved per-column widths.
func resolveProductionWidths(t *testing.T, headers []string) []int {
	t.Helper()
	var buf bytes.Buffer
	tt := NewTable(&buf, headers...)
	tt.width = ttyBudget
	cells := make([]string, len(headers))
	for i := range cells {
		// Long content in every cell: the shape that squeezes NAME to its
		// floor and drives each flex column to its minimum.
		cells[i] = "crush-switchboard-2 weekly studio blog drafting that runs long"
	}
	tt.Row(cells...)
	if err := tt.Flush(); err != nil {
		t.Fatal(err)
	}
	return tt.widths
}

// TestProductionTablesStayReadableAtEightyColumns is the guard a MODEL column
// has to satisfy: every column of every table the CLI renders must resolve to
// a width that can still carry information at an 80-column terminal.
func TestProductionTablesStayReadableAtEightyColumns(t *testing.T) {
	t.Parallel()
	for name, headers := range productionHeaders {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			widths := resolveProductionWidths(t, headers)
			for i, w := range widths {
				if w < minReadable {
					t.Errorf("%s: column %q resolved to %d cells (want >= %d) at an 80-column terminal; widths=%v",
						name, headers[i], w, minReadable, widths)
				}
			}
		})
	}
}

// TestReadabilityGuardCanFail proves the guard above is not vacuous.
//
// A guard that passes whatever the table contains is worth nothing — it is the
// shape this file was written to replace, and the first version of this file
// was exactly that: it asserted total width, which defaultColumnWidths always
// satisfies, and so could never fail. Adding a seventh column must trip the
// readability floor. If this test ever stops observing a starved column, the
// width machinery has changed and the guard above needs re-deriving rather
// than trusting.
func TestReadabilityGuardCanFail(t *testing.T) {
	t.Parallel()
	headers := append(append([]string{}, productionHeaders["list"]...), "MODEL")
	widths := resolveProductionWidths(t, headers)

	starved := 0
	for _, w := range widths {
		if w < minReadable {
			starved++
		}
	}
	if starved == 0 {
		t.Errorf("a seventh column starved no column below %d cells (widths=%v) — "+
			"the readability guard cannot fail and is therefore not guarding anything",
			minReadable, widths)
	}
}
