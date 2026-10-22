package daemon

// The children sample (issue #958; ADR-0048).
//
// `list` and `describe` name the supervised process's direct children so an
// operator can see a harness blocked behind a hung child without running ps by
// hand. Reading the process table is a shell-out, and the surfaces that want
// the answer include the TUI's refresh loop, so the sample is taken on a timer
// by the server and the request path only reads what the last pass found. A
// child that started and finished inside one interval is never named: this
// answers "what has been sitting there for hours", not "what existed for a
// second", and the alternative is a ps per list.
//
// Where ps is missing (a container with no procps) the sample never fills and
// the fields stay absent, which the wire contract reads as unknown rather than
// as "no children".

import (
	"sync"
	"time"

	"charm.land/log/v2"

	"github.com/stump-wtf/harness/internal/proctree"
)

// defaultChildrenInterval is how stale the children sample may be. It is long
// enough that the ps costs nothing measurable against a daemon's other work,
// and short enough that a child spawned by the harness shows up in `describe`
// on the next look.
const defaultChildrenInterval = 15 * time.Second

// procSampler holds the latest process-table read. Create with newProcSampler,
// run with Start, stop with Close.
type procSampler struct {
	interval time.Duration

	mu    sync.Mutex
	table *proctree.Table
	// warned keeps a box with no ps(1) from logging once per interval.
	warned bool
	// started is set when the loop is running, stopped when Close has run. A
	// server that is built and closed without ever serving — every test that
	// boots one — has neither, and Close must not wait on a loop that never
	// began.
	started bool
	stopped bool

	stop chan struct{}
	done chan struct{}
}

func newProcSampler(interval time.Duration) *procSampler {
	if interval <= 0 {
		interval = defaultChildrenInterval
	}
	return &procSampler{
		interval: interval,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Start takes one read immediately and then refreshes on the interval. It is
// idempotent, and does nothing after Close.
func (p *procSampler) Start() {
	p.mu.Lock()
	run := !p.started && !p.stopped
	p.started = run
	p.mu.Unlock()
	if run {
		go p.loop()
	}
}

func (p *procSampler) loop() {
	defer close(p.done)
	p.refresh()
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-t.C:
			p.refresh()
		}
	}
}

// Close stops the loop and waits for the in-flight read. A sampler that never
// started has nothing to wait for.
func (p *procSampler) Close() {
	p.mu.Lock()
	run := p.started && !p.stopped
	p.stopped = true
	p.mu.Unlock()
	if !run {
		return
	}
	close(p.stop)
	<-p.done
}

// refresh reads the table once, keeping the previous sample when the read
// fails: a transient ps failure is not a reason to tell an operator nothing is
// running.
func (p *procSampler) refresh() {
	t, err := proctree.Load()
	if err != nil {
		p.mu.Lock()
		if !p.warned {
			p.warned = true
			log.Debug("children unavailable: cannot read the process table", "err", err,
				"hint", "harness list/describe will omit children")
		}
		p.mu.Unlock()
		return
	}
	p.mu.Lock()
	p.table = t
	p.mu.Unlock()
}

// current is the latest sample, nil until the first read succeeds. A nil
// *procSampler reads as no sample, so a server built without one projects no
// children instead of panicking.
func (p *procSampler) current() *proctree.Table {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.table
}
