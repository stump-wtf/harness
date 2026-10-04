package quota

// Park Detector Tests
//
// SPEC-0021 REQ-11 and REQ-12 scenarios on the pure detector, with real
// provider notes run through the real classifier (internal/modelerr): what
// parks, until when, and what resets the count and the backoff.
//
// Governing: ADR-0027; SPEC-0021 REQ-11, REQ-12.
//
// @joestump 10/04/2026 - Added for stump.wtf/harness#477.

import (
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/modelerr"
)

// tarsNote is the 2026-10-04 incident's error mark, as agent-trace v0.7.1
// renders crush's finish part (message, then details).
const tarsNote = "Payment Required: You're out of credits. Add more at https://hyper.charm.land"

func la(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skipf("no tzdata: %v", err)
	}
	return loc
}

func quotaErr(at time.Time) Outcome {
	return ErrorOutcome("crush", "429 Too Many Requests: rate limit exceeded", at, at)
}

// REQ-12 Scenario "A reset time in the message": a quota error read at 12:40
// Los Angeles time parks until 15:00 Los Angeles time.
func TestAResetTimeInTheMessage(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 40, 0, 0, la(t))
	note := "rate_limit (429): 5-hour limit reached ∙ resets 3pm (America/Los_Angeles)"
	want := time.Date(2026, 9, 22, 15, 0, 0, 0, la(t))

	var resident Detector
	resident.Observe(ErrorOutcome("claude-code", note, now, now))
	p, ok := resident.Resident(now)
	if !ok || !p.Until.Equal(want) || p.Clamped {
		t.Fatalf("resident: park=%+v ok=%v, want until %v", p, ok, want)
	}
	if p.Rule != "common/rate limit" {
		t.Errorf("rule = %q, want common/rate limit", p.Rule)
	}

	var oneShot Detector
	oneShot.Observe(ErrorOutcome("claude-code", note, now, now))
	if p, ok := oneShot.OneShot(now.Add(-time.Minute), now); !ok || !p.Until.Equal(want) {
		t.Fatalf("one-shot: park=%+v ok=%v, want until %v", p, ok, want)
	}
}

// REQ-12 Scenario "A rate limit that recovers": two 429s, a successful tool
// call, then a third 429 inside the ten minutes does not park.
func TestARateLimitThatRecovers(t *testing.T) {
	t0 := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	var d Detector
	d.Observe(quotaErr(t0))
	d.Observe(quotaErr(t0.Add(time.Minute)))
	d.Observe(Success(t0.Add(2 * time.Minute)))
	d.Observe(quotaErr(t0.Add(3 * time.Minute)))
	if p, ok := d.Resident(t0.Add(3 * time.Minute)); ok {
		t.Fatalf("parked after a success reset the count: %+v", p)
	}

	// Without the success, the same three errors are stuck on quota.
	var stuck Detector
	stuck.Observe(quotaErr(t0))
	stuck.Observe(quotaErr(t0.Add(time.Minute)))
	stuck.Observe(quotaErr(t0.Add(3 * time.Minute)))
	p, ok := stuck.Resident(t0.Add(3 * time.Minute))
	if !ok || !p.Until.IsZero() || p.Step != 0 {
		t.Fatalf("three quota errors in ten minutes: park=%+v ok=%v, want a backoff park at step 0", p, ok)
	}
}

// Three errors spread over more than the window are not stuck: only the
// errors within ten minutes of the newest count.
func TestStuckNeedsThreeErrorsInTheWindow(t *testing.T) {
	t0 := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	var d Detector
	d.Observe(quotaErr(t0))
	d.Observe(quotaErr(t0.Add(6 * time.Minute)))
	if _, ok := d.Resident(t0.Add(6 * time.Minute)); ok {
		t.Fatal("parked on two errors")
	}
	d.Observe(quotaErr(t0.Add(11 * time.Minute)))
	if _, ok := d.Resident(t0.Add(11 * time.Minute)); ok {
		t.Fatal("parked on three errors spanning eleven minutes")
	}
	d.Observe(quotaErr(t0.Add(12 * time.Minute)))
	if _, ok := d.Resident(t0.Add(12 * time.Minute)); !ok {
		t.Fatal("not parked on three errors within ten minutes (6, 11, 12)")
	}
}

// REQ-12 Scenario "Backoff grows, then resets": quota_backoff = 15m,
// quota_backoff_max = 1h; parked four times in a row the parks last 15m, 30m,
// 1h and 1h, and after a successful call the next lasts 15m.
func TestBackoffGrowsThenResets(t *testing.T) {
	base, maxDur := 15*time.Minute, time.Hour
	t0 := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	var d Detector
	at := t0
	park := func() time.Duration {
		t.Helper()
		for range StuckErrors {
			at = at.Add(time.Second)
			d.Observe(quotaErr(at))
		}
		p, ok := d.Resident(at)
		if !ok {
			t.Fatal("not parked after three quota errors")
		}
		d.Parked()
		return Backoff(p.Step, base, maxDur)
	}
	var got []time.Duration
	for range 4 {
		got = append(got, park())
	}
	want := []time.Duration{15 * time.Minute, 30 * time.Minute, time.Hour, time.Hour}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("parks lasted %v, want %v", got, want)
		}
	}
	at = at.Add(time.Second)
	d.Observe(Success(at))
	if next := park(); next != 15*time.Minute {
		t.Fatalf("after a success the next park lasts %v, want 15m", next)
	}
}

// REQ-12 Scenario "A hostile reset time": an epoch far in the future parks
// for 8 days, flagged clamped (doctor shows the clamp).
func TestAHostileResetTime(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	var d Detector
	d.Observe(ErrorOutcome("claude-code", "usage limit reached|99999999999", now, now))
	p, ok := d.Resident(now)
	if !ok || !p.Clamped || !p.Until.Equal(now.Add(8*24*time.Hour)) {
		t.Fatalf("park=%+v ok=%v, want now+8d, clamped", p, ok)
	}
}

// REQ-11 Scenario "A crush quota error mark": a crush error mark whose
// details name litellm.RateLimitError is quota for the metrics series (the
// classifier metrics counts with) and for the park detector alike.
func TestACrushQuotaErrorMark(t *testing.T) {
	note := `Too Many Requests: litellm.RateLimitError: RateLimitError: OpenAIException - Rate limit reached`
	if class, _ := modelerr.Classify("crush", note); class != modelerr.ClassQuota {
		t.Fatalf("metrics class = %s, want quota", class)
	}
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	o := ErrorOutcome("crush", note, now, now)
	if o.Class != modelerr.ClassQuota || o.Rule != "crush/litellm.ratelimiterror" {
		t.Fatalf("detector outcome = %+v, want quota by crush/litellm.ratelimiterror", o)
	}
}

// The 2026-10-04 shape: crush's 402 "out of credits" is quota with no reset
// time, so a one-shot run that ends on it parks for the backoff.
func TestTheTarsNoteParksAOneShotForTheBackoff(t *testing.T) {
	start := time.Date(2026, 10, 4, 0, 5, 0, 0, time.UTC)
	var d Detector
	d.Observe(ErrorOutcome("crush", tarsNote, start.Add(2*time.Second), start.Add(2*time.Second)))
	p, ok := d.OneShot(start, start.Add(2*time.Second))
	if !ok || !p.Until.IsZero() || p.Rule != "common/payment required" || p.Step != 0 {
		t.Fatalf("park=%+v ok=%v, want a backoff park by common/payment required", p, ok)
	}
	if got := Backoff(p.Step, 15*time.Minute, 6*time.Hour); got != 15*time.Minute {
		t.Fatalf("first park lasts %v, want quota_backoff's default 15m", got)
	}
}

// A one-shot parks on its run's LAST classified outcome only: a success or a
// non-quota error after the quota error, or a quota error from before the run,
// does not park it; and nothing but quota ever parks.
func TestOneShotParksOnItsLastOutcome(t *testing.T) {
	start := time.Date(2026, 10, 4, 0, 5, 0, 0, time.UTC)
	at := func(s int) time.Time { return start.Add(time.Duration(s) * time.Second) }
	for _, tc := range []struct {
		name string
		feed []Outcome
		park bool
	}{
		{"quota last", []Outcome{Success(at(1)), quotaErr(at(2))}, true},
		{"success after quota", []Outcome{quotaErr(at(1)), Success(at(2))}, false},
		{"auth after quota", []Outcome{quotaErr(at(1)), ErrorOutcome("crush", "401 Unauthorized", at(2), at(2))}, false},
		{"auth only", []Outcome{ErrorOutcome("crush", "401 Unauthorized", at(1), at(1))}, false},
		{"quota before the run", []Outcome{quotaErr(at(-60))}, false},
		{"nothing observed", nil, false},
		{"quota last, delivered first", []Outcome{quotaErr(at(3)), Success(at(2))}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var d Detector
			for _, o := range tc.feed {
				d.Observe(o)
			}
			if _, ok := d.OneShot(start, at(5)); ok != tc.park {
				t.Fatalf("parked = %v, want %v", ok, tc.park)
			}
		})
	}
}

// A reset already behind the decision is no reset: a resident falls back to
// the stuck rule, a one-shot to the backoff.
func TestAPassedResetIsNoReset(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	var d Detector
	d.Observe(ErrorOutcome("crush", "429: please try again in 5m", now, now))
	if p, ok := d.Resident(now.Add(10 * time.Minute)); ok {
		t.Fatalf("resident parked on a reset that has passed and one error: %+v", p)
	}
	if p, ok := d.OneShot(now.Add(-time.Second), now.Add(10*time.Minute)); !ok || !p.Until.IsZero() {
		t.Fatalf("one-shot: park=%+v ok=%v, want a backoff park", p, ok)
	}
}

// A park spends its evidence: after Parked a released resident needs fresh
// errors to park again, and the step has grown.
func TestParkedSpendsTheEvidence(t *testing.T) {
	t0 := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	var d Detector
	for i := range StuckErrors {
		d.Observe(quotaErr(t0.Add(time.Duration(i) * time.Second)))
	}
	d.Parked()
	if _, ok := d.Resident(t0.Add(time.Minute)); ok {
		t.Fatal("still due after the park was applied")
	}
	if d.Step() != 1 {
		t.Fatalf("step = %d, want 1", d.Step())
	}
	d.SetStep(3)
	d.SetStep(2) // a restored step never lowers a live one
	if d.Step() != 3 {
		t.Fatalf("step = %d after seeding, want 3", d.Step())
	}
}

func TestBackoffBounds(t *testing.T) {
	for _, tc := range []struct {
		step           int
		base, max, out time.Duration
	}{
		{0, time.Minute, 6 * time.Hour, time.Minute},
		{1, time.Minute, 6 * time.Hour, 2 * time.Minute},
		{100, time.Minute, 6 * time.Hour, 6 * time.Hour},
		{0, 15 * time.Minute, 15 * time.Minute, 15 * time.Minute},
		{2, 7 * time.Minute, 20 * time.Minute, 20 * time.Minute},
	} {
		if got := Backoff(tc.step, tc.base, tc.max); got != tc.out {
			t.Errorf("Backoff(%d, %v, %v) = %v, want %v", tc.step, tc.base, tc.max, got, tc.out)
		}
	}
}
