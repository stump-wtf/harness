package supervisor

// Quota Parks: The Manager's Half
//
// A harness whose provider refuses it for an exhausted quota is parked until
// the quota comes back, instead of being restarted into the same refusal
// (ADR-0027, SPEC-0021 REQ-12 to REQ-14). The supervisor's actor loop is the
// other half (holds.go, and the exit path in supervisor.go); this file owns
// the decision and the record.
//
// The Manager keeps one quota.Detector per harness, fed by the observer's
// "budget" subscription (internal/quota/feed, wired by the daemon) through
// QuotaObserve: error marks classified by internal/modelerr, tool calls as
// successes. It decides in two places:
//
//   - At an exit (ParkOnExit, on the exiting harness's own loop, before the
//     restart policy or the run's outcome counts the exit). It first brings
//     the detector up to date with everything the agent wrote, through the
//     quota sync seam: the observer scans now and the feed drains, so a run
//     that wrote its error and exited two seconds after it started is judged
//     on that error, not on whatever the last poll happened to see. Then a
//     one-shot that exited non-zero on a quota error, or a resident stuck on
//     quota, is parked.
//   - Live, for a resident still running (QuotaObserve): a quota error with a
//     reset time, or the third one in ten minutes, parks it on a goroutine of
//     its own (parkLive), which stops it at once through a quota hold. The
//     feed's goroutine never waits on a supervisor: an exit path waits on it.
//
// A park is written to state.json before anything stops (the after-hours
// lease's ordering), as `parks` entries keyed "harness:<name>" or
// "group:<quota_group>", carrying the reset instant, the matched rule's name,
// the backoff step and who triggered it, never error text (ADR-0008). Each
// entry decodes on its own at boot, so a malformed one costs that harness its
// park and nothing else. A group park parks every member with the same reset
// instant and records the members it parked, so a reload that removes a
// member's quota_group does not release it early (REQ-20).
//
// Admission's check 2 (quotaPark, the seam #470 left) refuses a parked
// harness: a one-shot firing is skipped quota_parked, a resident start is held
// for quota. The gate tick's quota clearer (quotaHoldCleared) releases the
// hold once the reset instant passes; the harness then goes back through
// admission (REQ-14).
//
// Lock order: budgetMu, then quotaMu (admission reads parks under budgetMu).
// quotaMu is never held across a save, an observer sync, or a command to a
// supervisor, and nothing holding mu or journalMu takes it.
//
// Governing: ADR-0027, ADR-0008; SPEC-0021 REQ-11 "Quota detection", REQ-12
// "Parking", REQ-13 "Park effects", REQ-14 "Release and hold reasons", REQ-19
// "Events and the durable log", REQ-20, REQ-21; design.md § "The park
// detector", § "What is persisted", § "Settled Questions".
//
// @joestump 10/04/2026 - Added for stump.wtf/harness#477.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"charm.land/log/v2"

	"github.com/stump-wtf/harness/internal/budget"
	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/modelerr"
	"github.com/stump-wtf/harness/internal/quota"
	"github.com/stump-wtf/harness/internal/runtrace"
)

// quotaSyncTimeout bounds how long an exit waits for the observer to read
// what the agent wrote (the quota sync seam). It blocks only the exiting
// harness's own loop; past it, the decision is made on what arrived, and the
// shortfall is logged.
const quotaSyncTimeout = 5 * time.Second

// Park keys in state.json's `parks` object (design.md § "What is persisted").
const (
	parkKeyHarness = "harness:"
	parkKeyGroup   = "group:"
)

// ParkInfo is a quota park in force on a harness: its own, or its quota
// group's (SPEC-0021 REQ-12, REQ-13).
type ParkInfo struct {
	// Until is the reset instant: the park ends AT it.
	Until time.Time
	// Rule names the classifier rule that matched (never error text).
	Rule string
	// Group is the quota group the park is on, "" for the harness's own.
	Group string
	// By is the harness whose refusal parked it.
	By string
	// Step is the backoff step the park was set at; Clamped marks a reset
	// cut to 8 days.
	Step    int
	Clamped bool
}

// QuotaExit is what the exit path tells the park detector about an exit.
type QuotaExit struct {
	// OneShot is a triggered harness's run; otherwise a resident's process.
	OneShot bool
	// Started is when the process started; the run's own outcomes are the
	// ones observed from then on.
	Started time.Time
	// Code is the exit status.
	Code int
}

// QuotaGate is the park detector as a supervisor's exit path reaches it
// (SPEC-0021 REQ-13: "The supervisor SHALL consult the detector before
// applying the restart policy to an exit").
type QuotaGate interface {
	// ParkOnExit is asked on the exiting harness's actor loop. It answers the
	// park that owns this exit, already written to state.json, or false. It
	// may wait, bounded, for the observer; it never calls back into the
	// asking supervisor.
	ParkOnExit(name string, ex QuotaExit) (ParkInfo, bool)
	// ParkOf is the park in force on name now, asked on the actor loop when
	// a park arrives by command: one the gate released while the command
	// was on its way is not applied.
	ParkOf(name string) (ParkInfo, bool)
}

// parkRecord is one park as state.json stores it.
type parkRecord struct {
	Until   time.Time `json:"until"`
	Rule    string    `json:"rule"`
	Step    int       `json:"step"`
	Clamped bool      `json:"clamped"`
	By      string    `json:"by"`
	// Members are a group park's members when it was applied (REQ-20).
	Members []string `json:"members,omitempty"`
}

// quotaBook is the Manager's park bookkeeping, guarded by quotaMu.
type quotaBook struct {
	det   map[string]*quota.Detector
	parks map[string]parkRecord
	// live marks a resident whose live park is being applied (parkLive),
	// so a burst of errors starts one goroutine, not one each. exiting
	// marks one whose exit is being decided (ParkOnExit): the marks its
	// sync brings in are the exit's to judge, not a live park's.
	live    map[string]bool
	exiting map[string]int
	// sync is the quota sync seam (SetQuotaSync); nil decides on what the
	// detector already has.
	sync func(context.Context) error
	// wg tracks the park goroutines; closed refuses new ones once Close
	// has begun waiting for them.
	wg     sync.WaitGroup
	closed bool
}

// SetQuotaSync installs the seam the exit path brings the park detector up to
// date through: the daemon passes its quota feed's Sync (internal/quota/feed),
// which has the observer scan now and the feed drain everything it read. nil
// removes it; the daemon does that before it stops the feed.
func (m *Manager) SetQuotaSync(fn func(context.Context) error) {
	m.quotaMu.Lock()
	defer m.quotaMu.Unlock()
	m.qb.sync = fn
}

// QuotaObserve feeds one model outcome the observer attributed to name: a
// successful call, or an error mark's note (the quota feed's Sink). It
// classifies the error with the one classifier (SPEC-0021 REQ-11) and, for a
// resident still running, parks it live once the detector calls for it. It
// is called on the feed's goroutine and never waits on a supervisor.
func (m *Manager) QuotaObserve(name, adapter string, at time.Time, success bool, note string) {
	now := m.now()
	o := quota.Success(at)
	if !success {
		o = quota.ErrorOutcome(adapter, note, at, now)
	}
	// Only a resident parks live; a one-shot's exit decides for it. Asked
	// only of a quota error, not of every tool call.
	resident := false
	if o.Class == modelerr.ClassQuota {
		if s := m.get(name); s != nil {
			resident = !s.Snapshot().Triggered
		}
	}
	m.quotaMu.Lock()
	m.detectorLocked(name).Observe(o)
	due := false
	if resident && !m.qb.live[name] && m.qb.exiting[name] == 0 {
		_, due = m.detectorLocked(name).Resident(now)
	}
	if due {
		// One live park at a time per harness: parkLive clears the mark.
		m.qb.live[name] = true
		if !m.goQuotaLocked(func() { m.parkLive(name) }) {
			delete(m.qb.live, name)
		}
	}
	m.quotaMu.Unlock()
}

// ParkOnExit implements QuotaGate.
func (m *Manager) ParkOnExit(name string, ex QuotaExit) (ParkInfo, bool) {
	m.quotaMu.Lock()
	m.qb.exiting[name]++
	m.quotaMu.Unlock()
	m.syncQuota(name)
	m.quotaMu.Lock()
	if m.qb.exiting[name]--; m.qb.exiting[name] == 0 {
		delete(m.qb.exiting, name)
	}
	m.quotaMu.Unlock()
	h, _ := m.budgetDef(name)
	members := m.groupMembers(h.Budget.QuotaGroup)
	now := m.now()
	m.quotaMu.Lock()
	if p, ok := m.parkOnLocked(name, h, now); ok {
		// A park already in force: a resident's exit belongs to it (its
		// group parked, or a live park beat the exit here). A one-shot's
		// run belongs to it only when the run itself ended on quota; one
		// that failed for another reason failed.
		if ex.OneShot {
			_, ok = m.detectorLocked(name).OneShot(ex.Started.Add(-runtrace.Slack), now)
		}
		m.quotaMu.Unlock()
		return p, ok
	}
	det := m.detectorLocked(name)
	var trig quota.Park
	var due bool
	if ex.OneShot {
		trig, due = det.OneShot(ex.Started.Add(-runtrace.Slack), now)
	} else {
		trig, due = det.Resident(now)
	}
	if !due {
		m.quotaMu.Unlock()
		return ParkInfo{}, false
	}
	p := m.recordParkLocked(name, h, members, trig, now)
	m.quotaMu.Unlock()
	m.parked(name, p, members)
	return p, true
}

// parkLive parks a running resident the detector found stuck on quota
// between exits: written to state.json first, then held for quota, which
// stops it at once with no restart (SPEC-0021 REQ-13).
func (m *Manager) parkLive(name string) {
	defer func() {
		m.quotaMu.Lock()
		delete(m.qb.live, name)
		m.quotaMu.Unlock()
	}()
	h, _ := m.budgetDef(name)
	members := m.groupMembers(h.Budget.QuotaGroup)
	now := m.now()
	m.quotaMu.Lock()
	p, ok := m.parkOnLocked(name, h, now)
	fresh := false
	if !ok {
		var trig quota.Park
		if trig, ok = m.detectorLocked(name).Resident(now); ok {
			p, fresh = m.recordParkLocked(name, h, members, trig, now), true
		}
	}
	m.quotaMu.Unlock()
	if !ok {
		return
	}
	if fresh {
		m.parked(name, p, members)
	}
	if s := m.get(name); s != nil {
		s.Park(p)
	}
}

// recordParkLocked turns the detector's call into a park in force: until the
// reset the error named, or for the backoff at its step; on name, or on its
// quota group with every member recorded. The evidence is spent and the next
// park's step grows (quota.Detector.Parked). Caller holds quotaMu.
func (m *Manager) recordParkLocked(name string, h core.Harness, members []string, trig quota.Park, now time.Time) ParkInfo {
	until := trig.Until
	if until.IsZero() {
		until = now.Add(quota.Backoff(trig.Step, h.Budget.EffectiveQuotaBackoff(), h.Budget.EffectiveQuotaBackoffMax()))
	}
	rec := parkRecord{Until: until, Rule: trig.Rule, Step: trig.Step, Clamped: trig.Clamped, By: name}
	key, group := parkKeyHarness+name, h.Budget.QuotaGroup
	if group != "" {
		key = parkKeyGroup + group
		rec.Members = members
	}
	m.qb.parks[key] = rec
	m.detectorLocked(name).Parked()
	return parkInfo(rec, group)
}

// parked carries out a park just recorded: state.json first, then the
// daemon log, then every other member of a group, each held for quota on a
// goroutine of its own (the exit path that calls this runs on the trigger's
// loop, and two members exiting together must not wait on each other).
func (m *Manager) parked(name string, p ParkInfo, members []string) {
	if err := m.Save(); err != nil {
		// The park still applies in memory; only a crash before the next
		// save would lose it, and the harness would then park again on its
		// first refusal.
		log.Error("could not persist a quota park before stopping the harness", "harness", name, "until", p.Until.Format(time.RFC3339), "err", err)
	}
	kv := []any{"harness", name, "rule", p.Rule, "until", p.Until.Format(time.RFC3339), "step", p.Step}
	if p.Clamped {
		kv = append(kv, "clamped", true)
	}
	if p.Group != "" {
		kv = append(kv, "quota_group", p.Group, "members", strings.Join(members, ","))
	}
	log.Warn("parked on an exhausted provider quota; no restart before the reset", kv...)
	for _, member := range members {
		if member == name {
			continue
		}
		s := m.get(member)
		if s == nil {
			continue
		}
		m.quotaMu.Lock()
		m.goQuotaLocked(func() { s.Park(p) })
		m.quotaMu.Unlock()
	}
}

// goQuotaLocked runs fn on a tracked goroutine, unless Close has begun.
// Caller holds quotaMu.
func (m *Manager) goQuotaLocked(fn func()) bool {
	if m.qb.closed {
		return false
	}
	m.qb.wg.Add(1)
	go func() {
		defer m.qb.wg.Done()
		fn()
	}()
	return true
}

// closeQuota stops new park goroutines and waits for those in flight. Close
// calls it after every supervisor has shut down, so a pending hold finds its
// supervisor gone and returns at once.
func (m *Manager) closeQuota() {
	m.quotaMu.Lock()
	m.qb.closed = true
	m.quotaMu.Unlock()
	m.qb.wg.Wait()
}

// syncQuota brings the detector up to date with what the agents wrote, bounded
// by quotaSyncTimeout. With no seam (no observer), the detector decides on
// what it has.
func (m *Manager) syncQuota(name string) {
	m.quotaMu.Lock()
	fn := m.qb.sync
	m.quotaMu.Unlock()
	if fn == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), quotaSyncTimeout)
	defer cancel()
	if err := fn(ctx); err != nil {
		log.Warn("quota check at exit decided before the observer finished reading the agent's transcript", "harness", name, "err", err)
	}
}

// detectorLocked is name's detector, made on first use. Caller holds quotaMu.
func (m *Manager) detectorLocked(name string) *quota.Detector {
	d := m.qb.det[name]
	if d == nil {
		d = &quota.Detector{}
		m.qb.det[name] = d
	}
	return d
}

// groupMembers lists the global harnesses in quota group, in config order;
// nil for no group. Budget keys are global-config only (REQ-1).
func (m *Manager) groupMembers(group string) []string {
	if group == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, name := range m.cfg.HarnessOrder {
		if m.provenance[name] == "" && m.cfg.Harnesses[name].Budget.QuotaGroup == group {
			out = append(out, name)
		}
	}
	return out
}

// parkOnLocked is the park in force on name at now: its own, or a group
// park it is a member of by config (h) or by the record (a member when the
// group was parked). With more than one, the one that ends last. Caller
// holds quotaMu.
func (m *Manager) parkOnLocked(name string, h core.Harness, now time.Time) (ParkInfo, bool) {
	var best ParkInfo
	found := false
	consider := func(rec parkRecord, group string) {
		if now.Before(rec.Until) && (!found || rec.Until.After(best.Until)) {
			best, found = parkInfo(rec, group), true
		}
	}
	if rec, ok := m.qb.parks[parkKeyHarness+name]; ok {
		consider(rec, "")
	}
	for key, rec := range m.qb.parks {
		group, ok := strings.CutPrefix(key, parkKeyGroup)
		if ok && (group == h.Budget.QuotaGroup || slices.Contains(rec.Members, name)) {
			consider(rec, group)
		}
	}
	return best, found
}

// parkInfo is rec as a ParkInfo on group ("" for a harness's own).
func parkInfo(rec parkRecord, group string) ParkInfo {
	return ParkInfo{Until: rec.Until, Rule: rec.Rule, Group: group, By: rec.By, Step: rec.Step, Clamped: rec.Clamped}
}

// pruneParksLocked drops parks that have ended by now, so state.json stops
// carrying them. Caller holds quotaMu.
func (m *Manager) pruneParksLocked(now time.Time) {
	pruned := false
	for key, rec := range m.qb.parks {
		if !now.Before(rec.Until) {
			delete(m.qb.parks, key)
			pruned = true
		}
	}
	if pruned {
		m.markDirty()
	}
}

// quotaPark is admission's check 2 (SPEC-0021 REQ-4, REQ-12): the park in
// force on name, or on its quota group, at now. A one-shot firing is then
// skipped quota_parked and a resident held for quota. Called with budgetMu
// held; it takes only quotaMu.
func (m *Manager) quotaPark(name string, h core.Harness, now time.Time) budget.Park {
	m.quotaMu.Lock()
	defer m.quotaMu.Unlock()
	p, ok := m.parkOnLocked(name, h, now)
	if !ok {
		return budget.Park{}
	}
	return budget.Park{Until: p.Until, Rule: p.Rule, Group: p.Group}
}

// quotaHoldCleared is the clearing hook for the quota hold reason
// (HoldClearer; SPEC-0021 REQ-14): no park is in force on name at now, the
// gate tick's clock. Ended parks are dropped on the way.
func (m *Manager) quotaHoldCleared(name string, now time.Time) bool {
	h, _ := m.budgetDef(name)
	m.quotaMu.Lock()
	defer m.quotaMu.Unlock()
	m.pruneParksLocked(now)
	_, parked := m.parkOnLocked(name, h, now)
	return !parked
}

// ParkOf reports the quota park in force on name now, its own or its
// group's, with the member that triggered it (SPEC-0021 REQ-13 Scenario "A
// group parks together"). ok is false when it is not parked.
func (m *Manager) ParkOf(name string) (ParkInfo, bool) {
	h, _ := m.budgetDef(name)
	now := m.now()
	m.quotaMu.Lock()
	defer m.quotaMu.Unlock()
	return m.parkOnLocked(name, h, now)
}

// persistedParks encodes the parks for state.json; nil when there are none,
// so the key is absent.
func (m *Manager) persistedParks() json.RawMessage {
	m.quotaMu.Lock()
	parks := make(map[string]parkRecord, len(m.qb.parks))
	for k, v := range m.qb.parks {
		parks[k] = v
	}
	m.quotaMu.Unlock()
	if len(parks) == 0 {
		return nil
	}
	b, err := json.Marshal(parks)
	if err != nil {
		log.Error("could not encode quota parks for state.json", "err", err)
		return nil
	}
	return b
}

// errMalformedPark marks a park record that does not decode.
var errMalformedPark = errors.New("malformed park")

// decodePark decodes one `parks` entry on its own (SPEC-0021 REQ-13 Scenario
// "A malformed park in state.json").
func decodePark(key string, data json.RawMessage) (parkRecord, error) {
	name, ok := strings.CutPrefix(key, parkKeyHarness)
	if !ok {
		name, ok = strings.CutPrefix(key, parkKeyGroup)
	}
	if !ok || name == "" {
		return parkRecord{}, fmt.Errorf("%w: key %q is neither harness:<name> nor group:<name>", errMalformedPark, key)
	}
	var rec parkRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return parkRecord{}, fmt.Errorf("%w: %w", errMalformedPark, err)
	}
	if rec.Until.IsZero() || rec.Step < 0 {
		return parkRecord{}, fmt.Errorf("%w: no reset instant", errMalformedPark)
	}
	return rec, nil
}

// restoreParks reads state.json's `parks` at boot, each entry on its own: a
// malformed one is logged and that harness (or group) boots unparked, while
// every other park and the rest of the state load normally. A park that ended
// while the daemon was down is dropped; each restored one seeds its
// trigger's backoff step, so the next park still doubles.
func (m *Manager) restoreParks(raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		log.Error("state.json: `parks` is not an object; every harness boots unparked", "err", err)
		return
	}
	now := m.now()
	m.quotaMu.Lock()
	defer m.quotaMu.Unlock()
	for key, data := range entries {
		rec, err := decodePark(key, data)
		if err != nil {
			log.Error("state.json: a quota park does not decode; it is dropped and its harness boots unparked", "key", key, "err", err)
			continue
		}
		if rec.By != "" {
			m.detectorLocked(rec.By).SetStep(rec.Step + 1)
		}
		if !now.Before(rec.Until) {
			log.Info("quota park ended while the daemon was down", "key", key, "until", rec.Until.Format(time.RFC3339))
			continue
		}
		m.qb.parks[key] = rec
	}
}

// bootParks holds every harness a restored park covers for quota, so it boots
// parked: listed as parked, and released by the gate tick at the reset. A
// resident Autostart starts anyway is refused by admission and held again;
// nothing makes a model call before the reset (SPEC-0021 REQ-13 Scenario "A
// restart during a park").
func (m *Manager) bootParks() {
	now := m.now()
	for _, s := range m.snapshotSupervisors() {
		name := s.Name()
		h, _ := m.budgetDef(name)
		m.quotaMu.Lock()
		p, ok := m.parkOnLocked(name, h, now)
		m.quotaMu.Unlock()
		if ok {
			log.Info("booting parked on a provider quota", "harness", name, "until", p.Until.Format(time.RFC3339), "rule", p.Rule)
			s.Park(p)
		}
	}
}

// seedQuotaSkipped re-derives, at boot, whether a one-shot is owed its
// catch-up when its park clears: its newest record is a quota_parked skip,
// so nothing has run since (the rule seedHoursSkipped applies to
// outside_hours skips).
func (m *Manager) seedQuotaSkipped(name string, s *Supervisor) {
	recs := m.Runs(name)
	if n := len(recs); n > 0 && recs[n-1].Outcome == OutcomeSkipped && recs[n-1].Reason == ReasonQuotaParked {
		s.send(command{kind: cmdSeedQuotaSkipped})
	}
}
