package mergetrain

// Comment Marker
//
// Every failure comment ends with a machine-readable marker. It is the
// one-comment-per-head dedupe (it lives on the forge, so a restart does not
// forget it) and what a Switchboard routing rule matches to make the author's
// todo (ADR-0032 option E1).
//
// Governing: SPEC-0025 REQ-9.

import (
	"fmt"
	"regexp"
	"strconv"
)

// Failure causes, as they appear in the marker.
const (
	CauseConflict     = "conflict"
	CauseRed          = "red"
	CauseTimeout      = "timeout"
	CauseMergeRefused = "merge-refused"
	CauseVerifyFailed = "verify-failed"
)

var markerRE = regexp.MustCompile(`<!-- harness-mergetrain v1 pr=(\d+) head=([0-9a-f]+) cause=([a-z-]+) -->`)

// Marker is the marker line for a failure of pr at head.
func Marker(pr int, head, cause string) string {
	return fmt.Sprintf("<!-- harness-mergetrain v1 pr=%d head=%s cause=%s -->", pr, head, cause)
}

// HasMarker reports whether any comment carries a marker for pr at head,
// whatever its cause.
func HasMarker(comments []string, pr int, head string) bool {
	want := strconv.Itoa(pr)
	for _, c := range comments {
		for _, m := range markerRE.FindAllStringSubmatch(c, -1) {
			if m[1] == want && m[2] == head {
				return true
			}
		}
	}
	return false
}
