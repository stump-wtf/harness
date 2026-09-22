package supervisor

// After-Hours Lease Tests
//
// SPEC-0012 REQ "After-Hours Lease": `harness start NAME` on a gated,
// out-of-hours harness creates a lease — persisted to state.json BEFORE the
// start — that keeps the gate from holding the harness until it ends. The
// file, not the in-memory struct, is asserted wherever the requirement is
// about durability: the restart-mid-lease scenario is a real two-manager
// round-trip through one state.json.
//
// Governing: ADR-0019, SPEC-0012 REQ "After-Hours Lease"; issue #383.
//
// @joestump-agent 09/21/2026 - Added for stump.wtf/harness#383.
//
// @joestump-agent 09/22/2026 - Asserted the write-before-start order at the
// spawn itself; the existing durability tests pass with it reversed.

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/hours"
)

// leaseWindow builds a Mon-Sun window from two wall-clock offsets around now,
// so a test is in or out of hours regardless of the host clock. Offsets may
// cross midnight; hours treats an end at or before its start as an overnight
// window, and `now` stays inside it by construction.
func leaseWindow(from, to time.Duration) string {
	now := time.Now()
	a, b := now.Add(from), now.Add(to)
	start, end := a.Format("15:04"), b.Format("15:04")
	if b.Before(a) {
		start, end = end, start
	}
	return "Mon-Sun " + start + "-" + end
}

// gatedWithHours returns a long-running harness carrying the parsed
// expression, with intent unset — StartFor is what records it, as a manual
// start does.
func gatedWithHours(t *testing.T, name, expr string) core.Harness {
	t.Helper()
	e, err := hours.Parse(expr)
	if err != nil {
		t.Fatalf("parse %q: %v", expr, err)
	}
	h := shHarness(name, "while true; do sleep 0.02; done", 0)
	h.OperatingHours = expr
	h.HoursShutdown = core.HoursShutdownImmediate
	h.HoursExpr = e
	return h
}

// waitLease polls until the state file records a lease for name, returning it.
func waitLease(t *testing.T, path, name string) time.Time {
	t.Helper()
	var until time.Time
	waitFor(t, 3*time.Second, "state.json records a lease", func() bool {
		ph, ok := readPersisted(t, path, name)
		if !ok || ph.LeaseUntil == nil {
			return false
		}
		until = *ph.LeaseUntil
		return true
	})
	return until
}

// SPEC-0012 REQ "After-Hours Lease": start on a gated, out-of-hours harness
// starts it, records intent, and persists the lease BEFORE starting — read
// back from the file, not the struct.
func TestStartForPersistsLeaseAndStarts(t *testing.T) {
	h := gatedWithHours(t, "night", leaseWindow(-2*time.Hour, -1*time.Hour)) // closed now
	m, path := newStateManager(t, managerCfg(h), fastPolicy())

	if err := m.StartFor(h.Name, time.Hour); err != nil {
		t.Fatalf("StartFor: %v", err)
	}
	waitFor(t, 3*time.Second, "running", func() bool {
		snap, _ := m.Snapshot(h.Name)
		return snap.State == core.StateRunning
	})
	snap, _ := m.Snapshot(h.Name)
	if !snap.Enabled {
		t.Fatal("leased start must record enabled intent, as any manual start does")
	}

	ph := waitPersisted(t, path, h.Name, core.StateRunning)
	if ph.LeaseUntil == nil {
		t.Fatal("state.json has no lease_until after StartFor")
	}
	if until := *ph.LeaseUntil; until.Before(time.Now().Add(50*time.Minute)) || until.After(time.Now().Add(70*time.Minute)) {
		t.Fatalf("lease end %v, want within a minute or two of now+1h", until)
	}
}

// An explicit --for on a harness to which no lease applies is rejected and
// starts nothing: ungated, gated-and-in-hours, and a non-positive length.
func TestStartForRejections(t *testing.T) {
	inHours := gatedWithHours(t, "daytime", leaseWindow(-1*time.Hour, time.Hour))
	plain := shHarness("plain", "while true; do sleep 0.02; done", 0)
	m, path := newStateManager(t, managerCfg(inHours, plain), fastPolicy())

	if err := m.StartFor(plain.Name, time.Hour); !errors.Is(err, ErrNoLease) {
		t.Fatalf("StartFor on an ungated harness = %v, want ErrNoLease", err)
	}
	if err := m.StartFor(inHours.Name, time.Hour); !errors.Is(err, ErrNoLease) {
		t.Fatalf("StartFor on an in-hours harness = %v, want ErrNoLease", err)
	}
	if err := m.StartFor(inHours.Name, -time.Minute); !errors.Is(err, ErrNoLease) {
		t.Fatalf("StartFor with a negative length = %v, want ErrNoLease", err)
	}
	// Nothing started, nothing leased.
	time.Sleep(50 * time.Millisecond)
	for _, name := range []string{plain.Name, inHours.Name} {
		snap, _ := m.Snapshot(name)
		if snap.State != core.StateStopped {
			t.Fatalf("%s started through a rejected lease: state=%s", name, snap.State)
		}
	}
	if _, ok := m.Lease(plain.Name); ok {
		t.Fatal("an ungated harness grew a lease")
	}
	if ph, ok := readPersisted(t, path, plain.Name); ok && ph.LeaseUntil != nil {
		t.Fatal("state.json recorded a lease for a rejected start")
	}
}

// Manager.Start itself never creates a lease — the lease is the control
// op's composition (StartFor, including the default length it applies out
// of hours). This pins the manager half of that split.
func TestPlainStartCreatesNoLease(t *testing.T) {
	h := gatedWithHours(t, "gated", leaseWindow(-2*time.Hour, -1*time.Hour))
	m, path := newStateManager(t, managerCfg(h), fastPolicy())

	if !m.Start(h.Name) {
		t.Fatal("Start returned false")
	}
	waitFor(t, 3*time.Second, "running", func() bool {
		snap, _ := m.Snapshot(h.Name)
		return snap.State == core.StateRunning
	})
	if _, ok := m.Lease(h.Name); ok {
		t.Fatal("Manager.Start created a lease; the control op composes leases through StartFor")
	}
	if ph, ok := readPersisted(t, path, h.Name); ok && ph.LeaseUntil != nil {
		t.Fatal("state.json recorded a lease from a plain Manager.Start")
	}
}

// Stop discards the lease in every gate state and lands enabled=false: the
// operator's stop is the final word, and no later window may start the
// harness with a lease the operator already killed.
func TestStopDiscardsLease(t *testing.T) {
	h := gatedWithHours(t, "leased", leaseWindow(-2*time.Hour, -1*time.Hour))
	m, path := newStateManager(t, managerCfg(h), fastPolicy())

	if err := m.StartFor(h.Name, time.Hour); err != nil {
		t.Fatalf("StartFor: %v", err)
	}
	waitFor(t, 3*time.Second, "running", func() bool {
		snap, _ := m.Snapshot(h.Name)
		return snap.State == core.StateRunning
	})

	if !m.Stop(h.Name) {
		t.Fatal("Stop returned false")
	}
	waitPersisted(t, path, h.Name, core.StateStopped)
	if _, ok := m.Lease(h.Name); ok {
		t.Fatal("Stop left the lease live")
	}
	// The lease-clear rides the debounced persist loop; poll for the write
	// rather than reading once at a moment that may predate it.
	waitFor(t, 3*time.Second, "state.json to drop the lease", func() bool {
		ph, ok := readPersisted(t, path, h.Name)
		return ok && ph.LeaseUntil == nil
	})
	ph := waitPersisted(t, path, h.Name, core.StateStopped)
	if ph.Enabled {
		t.Fatal("stop must clear enabled intent")
	}
}

// A start on an already-leased harness replaces the end with now+for.
func TestStartForReplacesLeaseEnd(t *testing.T) {
	h := gatedWithHours(t, "leased", leaseWindow(-2*time.Hour, -1*time.Hour))
	m, _ := newStateManager(t, managerCfg(h), fastPolicy())

	if err := m.StartFor(h.Name, time.Hour); err != nil {
		t.Fatalf("first StartFor: %v", err)
	}
	first, ok := m.Lease(h.Name)
	if !ok {
		t.Fatal("no lease after StartFor")
	}
	time.Sleep(20 * time.Millisecond)
	if err := m.StartFor(h.Name, 2*time.Hour); err != nil {
		t.Fatalf("second StartFor: %v", err)
	}
	second, ok := m.Lease(h.Name)
	if !ok {
		t.Fatal("no lease after the replacement")
	}
	if !second.After(first) {
		t.Fatalf("replacement end %v not after the original %v", second, first)
	}
}

// The discard rules are minute-precision wall-clock decisions, so they are
// exercised through leaseState with an injected clock rather than by waiting.
// Window: Mon-Sun 09:00-17:00. Three rules: a lease dies when its end passes;
// when its hours open first it is discarded (the harness continues as an
// in-hours harness); and a lease on a harness that lost its operating_hours
// is dead weight.
func TestLeaseDiscardRules(t *testing.T) {
	h := gatedWithHours(t, "gated", "Mon-Sun 09:00-17:00")
	m, _ := newStateManager(t, managerCfg(h), fastPolicy())

	mon := time.Date(2026, 9, 21, 0, 0, 0, 0, time.Local) // a Monday
	at := func(hh, mm int) time.Time {
		return mon.Add(time.Duration(hh)*time.Hour + time.Duration(mm)*time.Minute)
	}

	// Live while out of hours and before its end (20:00, ends 21:00).
	m.leases[h.Name] = at(21, 0)
	if _, ok := m.leaseState(h.Name, at(20, 0)); !ok {
		t.Fatal("a live lease answered none while out of hours and unexpired")
	}

	// Expired past its end -> none (the gate pass holds on this tick).
	if _, ok := m.leaseState(h.Name, at(21, 30)); ok {
		t.Fatal("an expired lease answered live")
	}

	// Hours opening first discards: ends 16:00, examined at 10:00 (in hours).
	m.leases[h.Name] = at(16, 0)
	if _, ok := m.leaseState(h.Name, at(8, 0)); !ok {
		t.Fatal("lease answered none at 08:00, out of hours and unexpired")
	}
	if _, ok := m.leaseState(h.Name, at(10, 0)); ok {
		t.Fatal("a lease was still live after its hours opened")
	}
	m.mu.Lock()
	_, stillThere := m.leases[h.Name]
	m.mu.Unlock()
	if stillThere {
		t.Fatal("a discarded lease stayed in the map")
	}

	// A lease on a harness that lost its operating_hours is dead weight.
	m.leases[h.Name] = at(21, 0)
	plain := shHarness("plain", "while true; do sleep 0.02; done", 0)
	m.cfg.Harnesses[h.Name] = plain
	if _, ok := m.leaseState(h.Name, at(20, 0)); ok {
		t.Fatal("a lease survived on an ungated harness")
	}
}

// SPEC-0012 Scenario "Restart mid-lease": a daemon restarted at 20:30 during
// a lease ending 21:00 boots the harness running and holds it at 21:00 — not
// later. This is the real round-trip: m1 writes state.json and goes away, m2
// is a fresh Manager over the same file. The post-Close file is exactly the
// crash-between-write-and-start shape (enabled, stopped, leased), so the boot
// half is also the write-before-start ordering guarantee's substance: the
// lease is on disk and boot resolves it with a bounded run.
func TestRestartMidLeaseRoundTrip(t *testing.T) {
	dir := t.TempDir()
	statePath := dir + "/state.json"
	h := gatedWithHours(t, "leased", leaseWindow(-2*time.Hour, -1*time.Hour))
	cfg := managerCfg(h)
	p := fastPolicy()

	m1 := NewManager(cfg, ManagerOptions{Policy: p, StatePath: statePath, LogDir: dir + "/logs"})
	if err := m1.StartFor(h.Name, time.Hour); err != nil {
		t.Fatalf("StartFor: %v", err)
	}
	waitFor(t, 3*time.Second, "m1 running", func() bool {
		snap, _ := m1.Snapshot(h.Name)
		return snap.State == core.StateRunning
	})
	// The lease outlives the daemon: Close shuts every supervisor down
	// (Shutdown, not Stop — a daemon restart is not an operator stop) and
	// flushes state.json with the lease still on it.
	m1.Close()

	// The crash-between shape, on disk: intent true, harness stopped, lease
	// live. A daemon that died right after the persisted write and before the
	// start leaves exactly this behind — and boot must resolve it by starting
	// the harness, never by leaving it unbounded.
	ph := waitPersisted(t, statePath, h.Name, core.StateStopped)
	if !ph.Enabled || ph.LeaseUntil == nil {
		t.Fatalf("post-close state.json = enabled(%v) lease(%v), want true+set", ph.Enabled, ph.LeaseUntil)
	}

	m2 := NewManager(cfg, ManagerOptions{Policy: p, StatePath: statePath, LogDir: dir + "/logs"})
	t.Cleanup(m2.Close)
	if err := m2.Restore(); err != nil {
		t.Fatal(err)
	}
	until, ok := m2.Lease(h.Name)
	if !ok {
		t.Fatal("lease did not survive the daemon restart")
	}
	if until.Before(time.Now().Add(55 * time.Minute)) {
		t.Fatalf("restored lease end %v, want ~now+1h", until)
	}
	// Boot honours the lease: enabled → starts running (not held).
	m2.Autostart()
	waitFor(t, 3*time.Second, "m2 running under the restored lease", func() bool {
		snap, _ := m2.Snapshot(h.Name)
		return snap.State == core.StateRunning
	})
	snap, _ := m2.Snapshot(h.Name)
	if snap.Held {
		t.Fatal("boot held a harness under a live lease")
	}
}

// The write-before-start ordering guarantee's substance: even when the leased
// run dies at once, the lease is on disk and boot resolves it with a bounded
// run — the harness comes back under the lease, never unbounded.
func TestLeaseSurvivesAFailingRun(t *testing.T) {
	dir := t.TempDir()
	statePath := dir + "/state.json"
	h := gatedWithHours(t, "flaky", leaseWindow(-2*time.Hour, -1*time.Hour))
	h.Args = []string{"-c", "exit 1"}
	h.Restart = core.RestartNo
	cfg := managerCfg(h)
	p := fastPolicy()

	m1 := NewManager(cfg, ManagerOptions{Policy: p, StatePath: statePath, LogDir: dir + "/logs"})
	if err := m1.StartFor(h.Name, time.Hour); err != nil {
		t.Fatalf("StartFor: %v", err)
	}
	ph := waitPersisted(t, statePath, h.Name, core.StateStopped)
	if ph.LeaseUntil == nil {
		t.Fatal("the run failed but the lease is not on disk — ordering broke")
	}
}

// A lease end carries no monotonic reading. The monotonic clock stops while
// the host sleeps, so a lease end compared against another monotonic time
// would outlast its wall-clock end by however long the host was suspended;
// the scheduler strips its tick for the same reason (SPEC-0008 REQ
// "Suspend-Safe Schedule Evaluation").
func TestStartForLeaseEndIsWallClockOnly(t *testing.T) {
	h := gatedWithHours(t, "night", leaseWindow(-2*time.Hour, -1*time.Hour)) // closed now
	m, _ := newStateManager(t, managerCfg(h), fastPolicy())
	if err := m.StartFor(h.Name, time.Hour); err != nil {
		t.Fatalf("StartFor: %v", err)
	}
	m.mu.Lock()
	until := m.leases[h.Name]
	m.mu.Unlock()
	if until != until.Round(0) {
		t.Fatalf("lease end %v carries a monotonic reading", until)
	}
}

// The ordering itself (design.md § "Leases in state.json": write
// lease_until, THEN start): at the instant the leased process is spawned,
// state.json on disk already carries the lease. The SizeFor hook runs on the
// actor loop inside the spawn, so it reads the file between "decided to
// start" and "process exists" — the window a crash would freeze. Neither
// TestLeaseSurvivesAFailingRun nor the round trip above can see the order: the
// debounced persist loop writes the lease a moment after the start anyway, so
// both pass with the Save moved after the Start.
func TestStartForWritesTheLeaseBeforeTheSpawn(t *testing.T) {
	h := gatedWithHours(t, "night", leaseWindow(-2*time.Hour, -1*time.Hour)) // closed now
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")

	var mu sync.Mutex
	spawns, leasedAtSpawn := 0, 0
	sizeFor := func(name string) (int, int) {
		data, err := os.ReadFile(statePath)
		var ps persistedState
		leased := err == nil && json.Unmarshal(data, &ps) == nil && ps.Harnesses[name].LeaseUntil != nil
		mu.Lock()
		spawns++
		if leased {
			leasedAtSpawn++
		}
		mu.Unlock()
		return 80, 24
	}
	m := NewManager(managerCfg(h), ManagerOptions{Policy: fastPolicy(), StatePath: statePath, LogDir: filepath.Join(dir, "logs"), SizeFor: sizeFor})
	t.Cleanup(m.Close)

	if err := m.StartFor(h.Name, time.Hour); err != nil {
		t.Fatalf("StartFor: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if spawns == 0 {
		t.Fatal("the spawn hook never ran: this test observed nothing")
	}
	if leasedAtSpawn != spawns {
		t.Fatalf("state.json carried the lease at %d of %d spawns; the lease must be durable before the process starts", leasedAtSpawn, spawns)
	}
}
