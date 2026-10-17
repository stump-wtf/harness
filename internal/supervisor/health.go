package supervisor

// Per-Harness Health Probe
//
// HealthProber is the daemon-side half of the `health_cmd` feature (issue
// #960, ADR-0045 proposed): an operator-owned probe program the daemon runs
// on an interval while a harness is up. The probe encodes the domain
// knowledge the daemon cannot have ("my queue has items older than 15
// minutes"); the prober owns scheduling, the consecutive-failure streak,
// cooldown, the action (restart or notify), the log line, and the bus event
// the notify Watcher turns into the `health` alert. Like the [notify] hook it
// is a daemon-side hook, not a supervised harness (ADR-0033): it gets no
// PTY, no restart policy, and no run record.
//
// The probe inherits the daemon's environment plus HARNESS_NAME,
// HARNESS_PID, HARNESS_STATE and HARNESS_LAST_SUCCESSFUL_CALL (the last
// successful probe, RFC 3339, empty when there is none — the env name is
// shared with the stall_timeout watchdog so one probe body serves both).
// health_cmd is exec'd directly, not through a shell: the config file is
// trusted for what it runs, but the config text is never a script. The exec
// itself lives in internal/healthprobe, outside this package: the admission
// funnel check holds every process start in supervisor to spawn's exec
// sites, and a probe start is not a harness start — it takes no admission
// and no run budget, exactly like the notify hook.
//
// A probe runs only while the harness is enabled and up: a stopped harness
// has no process to interrogate, and a streak that accrued before a restart
// or an operator stop must not fire an action at something the operator is
// working on. The streak and the cooldown live in memory only — like the
// session guard's flags, a daemon restart re-learns them.
//
// Governing: ADR-0045 (proposed); stump.wtf/harness#960.

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/healthprobe"
)

// healthProbeRing is how many probe results describe shows per harness.
const healthProbeRing = 10

// healthProbeOutputBytes caps one probe's stored output (tail kept).
const healthProbeOutputBytes = 4096

// DefaultHealthProbeTick is how often the prober rescans for due probes.
// health_interval is honored within this resolution; intervals below it are
// pointless for a liveness probe.
const DefaultHealthProbeTick = 5 * time.Second

// HealthProbe is one probe result, oldest first in the prober's ring.
type HealthProbe struct {
	At       time.Time `json:"at"`
	OK       bool      `json:"ok"`
	ExitCode int       `json:"exit_code"`
	Output   string    `json:"output,omitempty"`
}

// healthState is one harness's streak bookkeeping. Guarded by
// HealthProber.mu along with the ring.
type healthState struct {
	consecutive int
	lastAction  time.Time
	lastSuccess time.Time
	nextDue     time.Time
	lastOK      *bool // nil until the first probe completes
	probes      []HealthProbe
}

// HealthProber runs health_cmd for every harness that sets it, counts
// consecutive failures, and takes health_action when the streak trips.
type HealthProber struct {
	mgr     *Manager
	tick    time.Duration
	timeout time.Duration

	mu    sync.Mutex
	state map[string]*healthState

	stop chan struct{}
	done chan struct{}
}

// NewHealthProber builds a prober over mgr. A zero tick means
// DefaultHealthProbeTick (tests shorten it).
func NewHealthProber(mgr *Manager, tick time.Duration) *HealthProber {
	if tick <= 0 {
		tick = DefaultHealthProbeTick
	}
	return &HealthProber{
		mgr:     mgr,
		tick:    tick,
		timeout: core.DefaultHealthProbeTimeout,
		state:   map[string]*healthState{},
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
}

// Start runs the sweep loop. Idempotent calls after the first are ignored
// only insofar as Close has not run; daemon boot calls it once.
func (p *HealthProber) Start() {
	go p.loop()
}

// Close stops the loop and waits for the in-flight sweep to finish.
// Probes still running are cancelled by the context. Idempotent.
func (p *HealthProber) Close() {
	select {
	case <-p.stop:
	default:
		close(p.stop)
	}
	<-p.done
}

func (p *HealthProber) loop() {
	defer close(p.done)
	t := time.NewTicker(p.tick)
	defer t.Stop()
	for {
		select {
		case <-p.stop:
			return
		case now := <-t.C:
			p.sweep(now)
		}
	}
}

// sweep probes every harness whose interval has elapsed. Called with the
// prober goroutine only.
func (p *HealthProber) sweep(now time.Time) {
	cfg := p.mgr.Config()
	if cfg == nil {
		return
	}
	for _, name := range cfg.HarnessOrder {
		h, ok := cfg.Harnesses[name]
		if !ok || h.HealthCmd == "" {
			continue
		}
		st := p.stateFor(name)
		snap, ok := p.mgr.Snapshot(name)
		up := ok && snap.Enabled && (snap.State == core.StateRunning || snap.State == core.StateStarting)
		if !up {
			// Nothing to interrogate, and no stale streak: an operator
			// stop must not be answered with a probe verdict.
			st.consecutive = 0
			st.nextDue = time.Time{}
			continue
		}
		if st.nextDue.IsZero() {
			st.nextDue = now.Add(h.HealthInterval)
			continue
		}
		if now.Before(st.nextDue) {
			continue
		}
		st.nextDue = now.Add(h.HealthInterval)
		p.probe(name, h, snap, st, now)
	}
}

// probe runs one health_cmd and updates the streak. Runs on the prober
// goroutine; the probe itself may take up to the timeout.
func (p *HealthProber) probe(name string, h core.Harness, snap Snapshot, st *healthState, now time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), p.timeout)
	defer cancel()

	res := healthprobe.Run(ctx, h.HealthCmd, append(os.Environ(),
		"HARNESS_NAME="+name,
		"HARNESS_PID="+strconv.Itoa(snap.PID),
		"HARNESS_STATE="+string(snap.State),
		"HARNESS_LAST_SUCCESSFUL_CALL="+formatProbeTime(st.lastSuccess),
	))
	exit := res.ExitCode
	output := tailString(res.Output, healthProbeOutputBytes)
	ok := res.OK

	p.mu.Lock()
	st.probes = append(st.probes, HealthProbe{At: now, OK: ok, ExitCode: exit, Output: output})
	if len(st.probes) > healthProbeRing {
		st.probes = st.probes[len(st.probes)-healthProbeRing:]
	}
	tripped := false
	streak := 0
	if ok {
		st.consecutive = 0
		st.lastSuccess = now
		st.lastOK = &ok
	} else {
		no := false
		st.lastOK = &no
		st.consecutive++
		failures := h.HealthFailures
		if failures <= 0 {
			failures = core.DefaultHealthFailures
		}
		cooldownDone := st.lastAction.IsZero() || !now.Before(st.lastAction.Add(h.HealthInterval))
		if st.consecutive >= failures && cooldownDone {
			tripped = true
			streak = st.consecutive
			st.consecutive = 0
			st.lastAction = now
		}
	}
	p.mu.Unlock()

	if !tripped {
		return
	}

	action := string(h.HealthAction)
	if action == "" {
		action = string(core.HealthRestart)
	}
	if h.HealthAction == core.HealthRestart || h.HealthAction == "" {
		p.mgr.restartBy(name, "health", "")
	}
	p.mgr.bus.Publish(Event{
		Kind:   EventHealth,
		Name:   name,
		Time:   now,
		Action: action,
		Streak: streak,
		Code:   exit,
		Output: output,
	})
}

// stateFor returns name's bookkeeping, creating it on first sight. The
// prober goroutine is the only caller.
func (p *HealthProber) stateFor(name string) *healthState {
	p.mu.Lock()
	defer p.mu.Unlock()
	st, ok := p.state[name]
	if !ok {
		st = &healthState{}
		p.state[name] = st
	}
	return st
}

// Flag reports name's health as "ok" or "failing" — empty before the first
// probe completes, so describe says nothing it has not observed.
func (p *HealthProber) Flag(name string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	st, ok := p.state[name]
	if !ok || st.lastOK == nil {
		return ""
	}
	if *st.lastOK {
		return "ok"
	}
	return "failing"
}

// Probes returns name's last ring results, oldest first (a copy).
func (p *HealthProber) Probes(name string) []HealthProbe {
	p.mu.Lock()
	defer p.mu.Unlock()
	st, ok := p.state[name]
	if !ok || len(st.probes) == 0 {
		return nil
	}
	out := make([]HealthProbe, len(st.probes))
	copy(out, st.probes)
	return out
}

// tailString keeps the last n bytes of s (probe output is written forward;
// the end is the interesting part), trimmed.
func tailString(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return strings.TrimSpace(s[len(s)-n:])
}

func formatProbeTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}
