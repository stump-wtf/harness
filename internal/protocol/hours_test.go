package protocol

// Governing: ADR-0019 (operating hours), SPEC-0012 REQ "Operating Hours
// Visibility". Round-trip and omission tests for the operating-hours
// projection on HarnessInfo and the harness_hours_changed event — the wire
// contract a daemon populates (internal/daemon) and a client (cmd/harness,
// internal/tui) reads.
//
// @joestump-agent 09/22/2026 - Added for stump.wtf/harness#385.

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestHarnessInfoOperatingHoursRoundTrip pins the seven fields SPEC-0012
// names for a gated harness: every one of them survives a marshal/unmarshal
// round trip with its value intact.
func TestHarnessInfoOperatingHoursRoundTrip(t *testing.T) {
	want := HarnessInfo{
		Name:           "claude-src",
		State:          "running",
		OperatingHours: "TZ=America/Los_Angeles Mon-Fri 09:00-13:00",
		InHours:        true,
		HoursNext:      "2026-09-22T13:00:00-07:00",
		Held:           false,
		ClosingUntil:   "2026-09-22T13:15:00-07:00",
		HoursShutdown:  "graceful",
		LeaseUntil:     "2026-09-22T21:00:00-07:00",
	}
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got HarnessInfo
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip mismatch\n got %+v\nwant %+v", got, want)
	}

	// The wire field names are the spec's own vocabulary, not the Go field
	// names — assert the JSON itself, not just the round trip, so a rename
	// of the Go field alone (json tag left stale) cannot pass silently.
	for _, field := range []string{
		`"operating_hours":`, `"in_hours":`, `"hours_next":`, `"held":`,
		// Held is false above (zero value) and carries no omitempty (see
		// HarnessInfo's doc — it stays meaningful, unlike the timestamps
		// below), so it MUST still be present on the wire.
		`"closing_until":`, `"hours_shutdown":`, `"lease_until":`,
	} {
		if !strings.Contains(string(raw), field) {
			t.Errorf("marshaled JSON missing %s field: %s", field, raw)
		}
	}
}

// TestHarnessInfoOperatingHoursOmission covers the three timestamps the spec
// requires to be absent when their condition does not hold: hours_next for a
// whole-week expression (nothing resolved), closing_until outside a close,
// and lease_until outside a lease. An ungated harness (OperatingHours empty)
// omits operating_hours and hours_shutdown too; in_hours and held carry no
// omitempty (like Enabled and Flapping) and stay present as their harmless
// zero value (false), so a client can read them without a nil check.
func TestHarnessInfoOperatingHoursOmission(t *testing.T) {
	tests := []struct {
		name    string
		info    HarnessInfo
		absent  []string
		present []string
	}{
		{
			name: "ungated harness omits every timestamp/mode field but keeps the always-present bools",
			info: HarnessInfo{Name: "always-on", State: "running"},
			absent: []string{
				`"operating_hours"`, `"hours_next"`, `"closing_until"`,
				`"hours_shutdown"`, `"lease_until"`,
			},
			present: []string{`"in_hours":false`, `"held":false`},
		},
		{
			name: "gated, in hours, not leased, not closing: only the always-present fields show",
			info: HarnessInfo{
				Name: "claude-src", State: "running",
				OperatingHours: "Mon-Fri 09:00-13:00", InHours: true,
				HoursNext: "2026-09-22T13:00:00-07:00", HoursShutdown: "graceful",
			},
			present: []string{`"operating_hours"`, `"hours_next"`, `"hours_shutdown"`},
			absent:  []string{`"closing_until"`, `"lease_until"`},
		},
		{
			name: "whole-week expression omits hours_next: nothing to resolve",
			info: HarnessInfo{
				Name: "always-gated", State: "running",
				OperatingHours: "Mon-Sun 00:00-24:00", InHours: true, HoursShutdown: "graceful",
			},
			absent: []string{`"hours_next"`, `"closing_until"`, `"lease_until"`},
		},
		{
			name: "closing carries closing_until, not lease_until",
			info: HarnessInfo{
				Name: "claude-src", State: "running", Held: true,
				OperatingHours: "Mon-Fri 09:00-13:00", ClosingUntil: "2026-09-22T13:15:00-07:00",
				HoursShutdown: "graceful",
			},
			present: []string{`"closing_until"`},
			absent:  []string{`"lease_until"`, `"hours_next"`},
		},
		{
			name: "a lease carries lease_until, not closing_until",
			info: HarnessInfo{
				Name: "claude-src", State: "running",
				OperatingHours: "Mon-Fri 09:00-13:00", LeaseUntil: "2026-09-22T21:00:00-07:00",
				HoursShutdown: "graceful",
			},
			present: []string{`"lease_until"`},
			absent:  []string{`"closing_until"`},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.info)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			s := string(raw)
			for _, field := range tc.absent {
				if strings.Contains(s, field) {
					t.Errorf("expected %s absent, got: %s", field, s)
				}
			}
			for _, field := range tc.present {
				if !strings.Contains(s, field) {
					t.Errorf("expected %s present, got: %s", field, s)
				}
			}
		})
	}
}

// TestEventMsgHoursChangedRoundTrip pins harness_hours_changed's own two
// fields, and that HoursNext is omitted when the event carries none (the
// gate pass never invents one for a whole-week expression).
func TestEventMsgHoursChangedRoundTrip(t *testing.T) {
	want := EventMsg{Kind: EvHoursChanged, Name: "claude-src", InHours: true, HoursNext: "2026-09-22T13:00:00-07:00"}
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got EventMsg
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got != want {
		t.Errorf("round trip mismatch\n got %+v\nwant %+v", got, want)
	}
	if string(want.Kind) != "harness_hours_changed" {
		t.Errorf("EvHoursChanged = %q, want the literal wire name from SPEC-0012", want.Kind)
	}

	noNext := EventMsg{Kind: EvHoursChanged, Name: "always-gated", InHours: true}
	raw, err = json.Marshal(noNext)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), `"hours_next"`) {
		t.Errorf("hours_next present with no HoursNext set: %s", raw)
	}
}
