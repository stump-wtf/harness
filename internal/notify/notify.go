// Package notify runs the operator's [notify] hook when a harness needs a
// human.
//
// # Operator Notification
//
// Harness parks things and, until this package, told nobody. On 2026-09-25
// claude remote-control on kitt exited 1 six times with "Error: You must be
// logged in to use Remote Control.", gave up into `failed`, and sat dead for
// thirteen hours until someone happened to look; the runaway tool-loop guard
// stopped two crush workers the day before, and a guard stop clears the
// enabled intent, so they stayed down just as quietly. Every one of those was
// in a log. None of them reached a person.
//
// The daemon does not know how its operator wants to be reached — Signal, a
// pager, ntfy, a mail — so it runs one program the operator names and hands it
// the event. That program is a daemon-side hook, not a supervised harness: no
// PTY, no restart policy, no run record, no trace. ADR-0033 binds what harness
// *supervises* to agents agent-trace can read; this is the daemon's own
// outbound alert, configured once in the global harness.toml (a project file
// cannot declare it), and it runs only when the daemon has something to say.
//
// The Dispatcher is the delivery half. Notify never blocks: it applies the
// event filter and the per-(harness, event) cooldown, then queues the delivery
// for a small worker pool, dropping — and counting — when the queue is full,
// because a slow hook must never back up the supervisor that reported the
// failure (ADR-0007). Each delivery execs argv without a shell, feeds the JSON
// payload on stdin and the same facts in HARNESS_NOTIFY_* variables, and kills
// the hook's whole process group at the timeout. Every string that leaves the
// daemon goes through internal/redact first: the payload quotes agent output,
// and agent output is where credentials turn up.
//
// Governing: SPEC-0003 REQ "Operator Notification"; ADR-0007 (a slow consumer
// never stalls the supervisor); ADR-0008 (credentials stay out of what harness
// emits); issue #725.
//
// @joestump-agent 09/26/2026 - Added for harness#725.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"charm.land/log/v2"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/redact"
)

// PayloadVersion is the stdin JSON schema version. Additive changes keep it;
// a rename or a removal bumps it.
const PayloadVersion = 1

// Delivery results: the `result` label of harness_notify_deliveries_total and
// Delivery.Result.
const (
	// ResultOK: the hook exited 0.
	ResultOK = "ok"
	// ResultError: the hook could not be started or exited non-zero.
	ResultError = "error"
	// ResultTimeout: the hook outlived the timeout and was killed.
	ResultTimeout = "timeout"
	// ResultDropped: the delivery queue was full; the hook never ran.
	ResultDropped = "dropped"
	// ResultSuppressed: the same (harness, event) was delivered inside the
	// cooldown; the hook never ran.
	ResultSuppressed = "suppressed"
)

// Environment variables every delivery sets.
const (
	EnvEvent   = "HARNESS_NOTIFY_EVENT"
	EnvHarness = "HARNESS_NOTIFY_HARNESS"
	EnvHost    = "HARNESS_NOTIFY_HOST"
	EnvState   = "HARNESS_NOTIFY_STATE"
	EnvMessage = "HARNESS_NOTIFY_MESSAGE"
)

// Defaults for the zero Options fields.
const (
	DefaultWorkers = 4
	DefaultQueue   = 64
	// DefaultShutdownGrace is how long Close lets queued and in-flight
	// deliveries finish before it kills them.
	DefaultShutdownGrace = 5 * time.Second
	// outputTail is how much of the hook's combined output is kept for the
	// log line when it fails.
	outputTail = 2 << 10
)

// Notification is one event, before the daemon adds its host and redacts it.
type Notification struct {
	Event   string
	Harness string
	// State is the harness's state as the event left it.
	State string
	// Message is the one-line human summary, cause and hint included.
	Message string
	// Cause is the most useful single fact behind the event: the agent's
	// last output line, the looping tool. Empty when there is none.
	Cause string
	// Hint is the command that shows more, e.g. "harness logs claude-rc".
	Hint string
	Time time.Time

	ExitCode *int
	Restarts int
	// Tool and Count describe a loop-guard stop.
	Tool  string
	Count int
	// RunID names the run of a run_failed event.
	RunID int
}

// Payload is the JSON the hook reads on stdin.
type Payload struct {
	Version  int    `json:"version"`
	Event    string `json:"event"`
	Harness  string `json:"harness,omitempty"`
	Host     string `json:"host"`
	State    string `json:"state,omitempty"`
	Message  string `json:"message"`
	Cause    string `json:"cause,omitempty"`
	Hint     string `json:"hint,omitempty"`
	Time     string `json:"time"`
	ExitCode *int   `json:"exit_code,omitempty"`
	Restarts int    `json:"restarts,omitempty"`
	Tool     string `json:"tool,omitempty"`
	Count    int    `json:"count,omitempty"`
	RunID    int    `json:"run_id,omitempty"`
}

// Delivery is the outcome of one notification.
type Delivery struct {
	Event    string
	Harness  string
	Result   string
	Error    string // why, when Result is not ok; redacted
	At       time.Time
	Duration time.Duration
}

// Status is what `harness doctor` shows.
type Status struct {
	Config core.NotifyConfig
	// Last is the most recent delivery that ran the hook, nil before one
	// has. Suppressed and dropped notifications are not deliveries.
	Last *Delivery
}

// Options configures a Dispatcher. The zero value is production.
type Options struct {
	Workers int
	Queue   int
	// Hostname names the machine in every payload (default os.Hostname).
	Hostname func() (string, error)
	// Environ is the environment the hook inherits (default os.Environ).
	Environ func() []string
	Now     func() time.Time
	Logger  *log.Logger
	// ShutdownGrace bounds Close (default DefaultShutdownGrace).
	ShutdownGrace time.Duration
}

type cooldownKey struct{ harness, event string }

type job struct {
	n   Notification
	cfg core.NotifyConfig
}

// Dispatcher delivers Notifications to the configured hook. Build it with New;
// it lives for the daemon's lifetime and follows reloads through SetConfig.
type Dispatcher struct {
	opts       Options
	host       string
	deliveries *prometheus.CounterVec

	mu     sync.Mutex
	cfg    core.NotifyConfig
	sent   map[cooldownKey]time.Time
	last   *Delivery
	closed bool

	queue    chan job
	wg       sync.WaitGroup
	root     context.Context
	cancel   context.CancelFunc
	closeOne sync.Once
}

// New builds a Dispatcher for cfg and starts its workers. A zero cfg is
// valid: notify is off until a reload turns it on.
func New(cfg core.NotifyConfig, opts Options) *Dispatcher {
	if opts.Workers <= 0 {
		opts.Workers = DefaultWorkers
	}
	if opts.Queue <= 0 {
		opts.Queue = DefaultQueue
	}
	if opts.Hostname == nil {
		opts.Hostname = os.Hostname
	}
	if opts.Environ == nil {
		opts.Environ = os.Environ
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = log.Default()
	}
	if opts.ShutdownGrace <= 0 {
		opts.ShutdownGrace = DefaultShutdownGrace
	}
	host, err := opts.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	root, cancel := context.WithCancel(context.Background())
	d := &Dispatcher{
		opts: opts,
		host: host,
		deliveries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "harness_notify_deliveries_total",
			Help: "Notify hook deliveries by event and result (ok, error, timeout, dropped, suppressed).",
		}, []string{"event", "result"}),
		cfg:    cfg,
		sent:   make(map[cooldownKey]time.Time),
		queue:  make(chan job, opts.Queue),
		root:   root,
		cancel: cancel,
	}
	for range opts.Workers {
		d.wg.Add(1)
		go d.work()
	}
	return d
}

// Host is the machine name every payload carries.
func (d *Dispatcher) Host() string { return d.host }

// Collector is the delivery counter, for the daemon's /metrics registry.
func (d *Dispatcher) Collector() prometheus.Collector { return d.deliveries }

// SetConfig swaps in a reloaded [notify] table. Deliveries already queued keep
// the table they were queued under; cooldowns carry over.
func (d *Dispatcher) SetConfig(cfg core.NotifyConfig) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cfg = cfg
}

// Config is the table in force.
func (d *Dispatcher) Config() core.NotifyConfig {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cfg
}

// Wants reports whether event would run the hook under the table in force.
func (d *Dispatcher) Wants(event string) bool { return d.Config().Wants(event) }

// Status reports the table in force and the last delivery.
func (d *Dispatcher) Status() Status {
	d.mu.Lock()
	defer d.mu.Unlock()
	st := Status{Config: d.cfg}
	if d.last != nil {
		l := *d.last
		st.Last = &l
	}
	return st
}

// Notify queues n for delivery and returns at once. It reports whether the
// hook will run: false when the event is not wanted, is inside its cooldown,
// or the queue is full (the last two are counted and logged).
//
// A `recovered` notification clears its harness's failed and loop_stopped
// cooldowns, so a harness that fails again straight after an operator brings
// it back pages again instead of leaving "recovered" as the last word.
func (d *Dispatcher) Notify(n Notification) bool {
	if n.Time.IsZero() {
		n.Time = d.opts.Now()
	}
	d.mu.Lock()
	cfg := d.cfg
	if d.closed || !cfg.Wants(n.Event) {
		d.mu.Unlock()
		return false
	}
	key := cooldownKey{n.Harness, n.Event}
	if prev, ok := d.sent[key]; ok && cfg.Cooldown > 0 && n.Time.Sub(prev) < cfg.Cooldown {
		d.mu.Unlock()
		d.deliveries.WithLabelValues(n.Event, ResultSuppressed).Inc()
		d.opts.Logger.Debug("notify: suppressed inside cooldown", "event", n.Event, "harness", n.Harness, "cooldown", cfg.Cooldown)
		return false
	}
	prev, hadPrev := d.sent[key]
	d.sent[key] = n.Time
	if n.Event == core.NotifyRecovered {
		delete(d.sent, cooldownKey{n.Harness, core.NotifyFailed})
		delete(d.sent, cooldownKey{n.Harness, core.NotifyLoopStopped})
	}
	select {
	case d.queue <- job{n: n, cfg: cfg}:
		d.mu.Unlock()
		return true
	default:
		// Not delivered, so it must not hold the cooldown against the next.
		if hadPrev {
			d.sent[key] = prev
		} else {
			delete(d.sent, key)
		}
		d.mu.Unlock()
		d.deliveries.WithLabelValues(n.Event, ResultDropped).Inc()
		d.opts.Logger.Warn("notify: queue full, dropping notification", "event", n.Event, "harness", n.Harness, "queue", cap(d.queue))
		return false
	}
}

// Test runs the hook once, now, with a `test` event, and returns the outcome.
// It ignores `events` and the cooldown — it is how an operator proves the
// hook works — and is bounded by the configured timeout.
func (d *Dispatcher) Test(ctx context.Context) Delivery {
	cfg := d.Config()
	n := Notification{
		Event:   core.NotifyTest,
		Message: fmt.Sprintf("test notification from harness on %s — the notify hook works", d.host),
		Hint:    "harness doctor",
		Time:    d.opts.Now(),
	}
	if !cfg.Enabled() {
		return Delivery{Event: n.Event, Result: ResultError, Error: "notify is not configured: add a [notify] table to harness.toml", At: n.Time}
	}
	return d.deliver(ctx, n, cfg)
}

// Close stops taking notifications, gives queued and running deliveries
// ShutdownGrace to finish, then kills what is left. Idempotent.
func (d *Dispatcher) Close() {
	d.closeOne.Do(func() {
		d.mu.Lock()
		d.closed = true
		close(d.queue)
		d.mu.Unlock()
		done := make(chan struct{})
		go func() { d.wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(d.opts.ShutdownGrace):
			d.cancel()
			<-done
		}
		d.cancel()
	})
}

func (d *Dispatcher) work() {
	defer d.wg.Done()
	for j := range d.queue {
		if d.root.Err() != nil {
			continue // shutting down past the grace: drain without running
		}
		d.deliver(d.root, j.n, j.cfg)
	}
}

// deliver runs the hook for n under cfg and records the outcome.
func (d *Dispatcher) deliver(parent context.Context, n Notification, cfg core.NotifyConfig) Delivery {
	p := d.payload(n)
	body, _ := json.Marshal(p)

	ctx, cancel := context.WithTimeout(parent, cfg.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, cfg.Command[0], cfg.Command[1:]...)
	cmd.Stdin = bytes.NewReader(body)
	cmd.Env = d.env(p)
	var out tailBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	// Its own process group, so the timeout takes the hook's children (a
	// curl, a signal-cli) with it rather than leaving them holding the pipes.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second

	start := time.Now()
	err := cmd.Run()
	del := Delivery{Event: n.Event, Harness: n.Harness, Result: ResultOK, At: n.Time, Duration: time.Since(start)}
	switch {
	case err == nil:
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		del.Result, del.Error = ResultTimeout, fmt.Sprintf("killed after %s", cfg.Timeout)
	default:
		del.Result, del.Error = ResultError, redact.String(err.Error())
	}

	d.deliveries.WithLabelValues(n.Event, del.Result).Inc()
	d.mu.Lock()
	last := del
	d.last = &last
	d.mu.Unlock()

	kv := []any{"event", n.Event, "harness", n.Harness, "result", del.Result, "took", del.Duration.Round(time.Millisecond)}
	if del.Result == ResultOK {
		d.opts.Logger.Info("notify: delivered", kv...)
	} else {
		kv = append(kv, "err", del.Error, "command", cfg.Command[0])
		if o := strings.TrimSpace(redact.String(out.String())); o != "" {
			kv = append(kv, "output", o)
		}
		d.opts.Logger.Error("notify: hook failed", kv...)
	}
	return del
}

// payload builds the redacted stdin JSON for n.
func (d *Dispatcher) payload(n Notification) Payload {
	return Payload{
		Version:  PayloadVersion,
		Event:    n.Event,
		Harness:  n.Harness,
		Host:     d.host,
		State:    n.State,
		Message:  oneLine(redact.String(n.Message)),
		Cause:    oneLine(redact.String(n.Cause)),
		Hint:     n.Hint,
		Time:     n.Time.UTC().Format(time.RFC3339),
		ExitCode: n.ExitCode,
		Restarts: n.Restarts,
		Tool:     n.Tool,
		Count:    n.Count,
		RunID:    n.RunID,
	}
}

// env is the daemon's environment with any inherited HARNESS_NOTIFY_* removed
// and this delivery's set.
func (d *Dispatcher) env(p Payload) []string {
	env := slices.DeleteFunc(slices.Clone(d.opts.Environ()), func(kv string) bool {
		return strings.HasPrefix(kv, "HARNESS_NOTIFY_")
	})
	return append(env,
		EnvEvent+"="+p.Event,
		EnvHarness+"="+p.Harness,
		EnvHost+"="+p.Host,
		EnvState+"="+p.State,
		EnvMessage+"="+p.Message,
	)
}

// oneLine folds s onto one line: the message is meant for a notification
// title, and an env var with a newline in it trips up naive scripts.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// tailBuffer keeps the last outputTail bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > outputTail {
		t.buf = t.buf[len(t.buf)-outputTail:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
