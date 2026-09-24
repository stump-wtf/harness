package mergetrain

// Marker Tests
//
// Governing tests: SPEC-0025 REQ-9 — the marker round-trips, matches on PR and
// head whatever the cause, and does not match another PR or head.

import "testing"

func TestMarker(t *testing.T) {
	m := Marker(12, head, CauseRed)
	if m != "<!-- harness-mergetrain v1 pr=12 head="+head+" cause=red -->" {
		t.Fatalf("Marker = %q", m)
	}
	body := []string{"unrelated", "@x the train failed.\n\n" + m}
	if !HasMarker(body, 12, head) {
		t.Fatal("marker not found")
	}
	if !HasMarker([]string{Marker(12, head, CauseTimeout)}, 12, head) {
		t.Fatal("a different cause on the same head did not dedupe")
	}
	if HasMarker(body, 1, head) || HasMarker(body, 12, stale) || HasMarker(body, 120, head) {
		t.Fatal("marker matched another PR or head")
	}
	if HasMarker(nil, 12, head) {
		t.Fatal("matched no comments")
	}
}
