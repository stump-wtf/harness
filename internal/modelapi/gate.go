package modelapi

// Gate-Call Layer
//
// The layer every input screen's model calls go through (ADR-0042,
// SPEC-0031 REQ-8): per-guard slot pools, one deadline per call covering
// the slot wait and the whole round trip, REQ-6's error classes as
// sentinels, hash-then-redact-then-chunk input preparation, model
// attestation and per-guard counters. Nothing in the daemon constructs a
// Gate yet; #838's guard kinds plug into it and #840 is the first caller
// that waits on one.
//
// Governing: ADR-0042, ADR-0036, SPEC-0031 REQ-6, REQ-8.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/stump-wtf/harness/internal/ledger"
	"github.com/stump-wtf/harness/internal/redact"
)

// Entry is the closed set of entry points that may wait on a gate call
// (SPEC-0031 REQ-8 "Named entry points only"). A new entry point is a spec
// amendment that names it; loadout is reserved for SPEC-0032 (#834) and is
// unused in this epic.
type Entry string

const (
	// EntryEvent is admitting an event (REQ-11).
	EntryEvent Entry = "event"
	// EntryPackage is installing or upgrading a stable package (REQ-18).
	EntryPackage Entry = "package"
	// EntryLoadout is spawning a routed one-shot (SPEC-0032).
	EntryLoadout Entry = "loadout"
)

// validEntry reports whether e is one of the closed set.
func validEntry(e Entry) bool {
	switch e {
	case EntryEvent, EntryPackage, EntryLoadout:
		return true
	}
	return false
}

// REQ-6's closed error-class set. A call that cannot produce a score yields
// exactly one class; there is never a retry within a screen, and a new
// class is a spec amendment.
var (
	ErrTimeout       = errors.New("timeout")
	ErrTransport     = errors.New("transport")
	ErrHTTPStatus    = errors.New("http_status")
	ErrParse         = errors.New("parse")
	ErrNoLogprobs    = errors.New("no_logprobs")
	ErrModelMismatch = errors.New("model_mismatch")
	ErrTooLarge      = errors.New("too_large")
	ErrInterrupted   = errors.New("interrupted")
)

// classOf maps a class sentinel to its REQ-6 class name.
func classOf(target error) string {
	for _, c := range []struct {
		sentinel error
		name     string
	}{
		{ErrTimeout, "timeout"},
		{ErrTransport, "transport"},
		{ErrHTTPStatus, "http_status"},
		{ErrParse, "parse"},
		{ErrNoLogprobs, "no_logprobs"},
		{ErrModelMismatch, "model_mismatch"},
		{ErrTooLarge, "too_large"},
		{ErrInterrupted, "interrupted"},
	} {
		if errors.Is(target, c.sentinel) {
			return c.name
		}
	}
	return ""
}

// GateCallError is a gate call's failure: a REQ-6 class sentinel wrapped
// with the guard and policy that produced it, so errors.Is answers the
// class while the message names the configuration.
type GateCallError struct {
	Guard  string
	Policy string
	// Entry is the entry point the screen was gating.
	Entry Entry
	// Err is the class sentinel (one of the eight above).
	Err error
}

func (e *GateCallError) Error() string {
	return fmt.Sprintf("guard %q policy %q entry %q: %v", e.Guard, e.Policy, e.Entry, e.Err)
}

// Unwrap exposes the class sentinel.
func (e *GateCallError) Unwrap() error { return e.Err }

// Class is the REQ-6 class name of this failure.
func (e *GateCallError) Class() string { return classOf(e.Err) }

// errShutdown is the cancel cause Shutdown uses, so a screen cancelled by
// daemon shutdown reads as interrupted and never as timeout
// (SPEC-0031 REQ-8 "Held at shutdown").
var errShutdown = errors.New("daemon shutdown")

// CallSpec is one guard's call within a screen. Entry is required for a
// gate call — a call cannot be built without one (REQ-8 "Named entry
// points only") — while the probe path omits it, because a probe is not a
// gate call and can never withhold work.
type CallSpec struct {
	// Entry is the entry point being gated; required for a gate call.
	Entry Entry
	// Guard names the guard (its slot pool and counters), for errors.
	Guard string
	// Policy names the policy within the guard, for errors.
	Policy string
	// Model is the guard's model, attested byte for byte against the
	// response's model (REQ-6).
	Model string
	// Timeout bounds the whole call from the slot request to a parsed
	// response or an error; the slot wait counts against it (REQ-8).
	Timeout time.Duration
	// MaxConcurrency is the guard's slot-pool size.
	MaxConcurrency int
	// MaxTokens, Temperature, Logprobs and TopLogprobs shape the request.
	MaxTokens   int
	Temperature *float64
	Logprobs    *bool
	TopLogprobs *int
}

// validateGate checks the fields a gate call requires, including a valid
// entry point from the closed set.
func (s CallSpec) validateGate() error {
	if s.Entry == "" {
		return fmt.Errorf("guard %q policy %q: a gate call needs an entry point (event, package or loadout) — SPEC-0031 REQ-8 names the closed set", s.Guard, s.Policy)
	}
	if !validEntry(s.Entry) {
		return fmt.Errorf("guard %q policy %q: unknown entry point %q (want event, package or loadout)", s.Guard, s.Policy, s.Entry)
	}
	return s.validateCall()
}

// validateCall checks the fields every model call through a guard's pool
// requires, entry point aside.
func (s CallSpec) validateCall() error {
	switch {
	case strings.TrimSpace(s.Guard) == "":
		return errors.New("model call needs a guard name")
	case strings.TrimSpace(s.Model) == "":
		return fmt.Errorf("guard %q: model call needs a model name", s.Guard)
	case s.Timeout <= 0:
		return fmt.Errorf("guard %q policy %q: model call needs a positive timeout", s.Guard, s.Policy)
	case s.MaxConcurrency < 1:
		return fmt.Errorf("guard %q policy %q: model call needs max_concurrency of at least 1", s.Guard, s.Policy)
	}
	return nil
}

// CallResult is one guard's outcome within a screen (or one probe).
type CallResult struct {
	// Entry is the entry point that was gated; empty for a probe.
	Entry Entry
	// Guard and Policy name the configuration that produced the call.
	Guard, Policy string
	// Hash is the SHA-256 of the screened text BEFORE redaction, 64
	// lowercase hex digits (REQ-8 "Redacted input").
	Hash string
	// Content is choices[0].message.content.
	Content string
	// Logprobs is choices[0].logprobs.content, nil when absent.
	Logprobs []ContentLogprobs
	// Usage is the response's usage object; zero when the response carried
	// none, leaving the token counters unchanged.
	Usage Usage
	// ServedModel is the model the response named, sanitized (256 bytes,
	// no control characters).
	ServedModel string
	// Err is the REQ-6 class failure, a *GateCallError; nil on success.
	Err error
	// StartedAt is when the call asked for its slot.
	StartedAt time.Time
	// Duration runs from the slot request to a parsed response or an error.
	Duration time.Duration
}

// Prepare does the mandatory hash-then-redact-then-chunk order's first two
// steps over the whole screened text: the SHA-256 is taken BEFORE
// redaction, then the redactor runs ONCE over the whole text, and only
// then may a chunker (#838) cut it. Chunking first could send a credential
// cut by a chunk boundary as an unredacted prefix.
// Governing: SPEC-0031 REQ-8 "Redacted input".
func Prepare(text string) (hash, redacted string) {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:]), redact.String(text)
}

// Gate is the daemon-wide gate-call layer: one slot pool and one counter
// set per guard, shared by every screen and by no utility call (REQ-8
// "Bounded concurrency"). Its zero value is not usable; call NewGate.
type Gate struct {
	client *Client

	mu       sync.Mutex
	pools    map[string]chan struct{}
	counters map[string]*guardCounters

	// ctx is the gate's lifecycle context; Shutdown cancels it with
	// errShutdown so in-flight screens read as interrupted.
	ctx    context.Context
	cancel context.CancelCauseFunc

	now func() time.Time
}

// NewGate returns a gate issuing its calls through client.
func NewGate(client *Client) *Gate {
	ctx, cancel := context.WithCancelCause(context.Background())
	return &Gate{
		client:   client,
		pools:    map[string]chan struct{}{},
		counters: map[string]*guardCounters{},
		ctx:      ctx,
		cancel:   cancel,
		now:      time.Now,
	}
}

// Shutdown cancels every in-flight gate call with a distinct cause, so its
// class is interrupted and never timeout. It is idempotent.
func (g *Gate) Shutdown() { g.cancel(errShutdown) }

// guardCounters are the plain per-guard counters (REQ-8 "Metered on its
// own"; #840 exports them as REQ-17's series). Nothing here touches a
// SPEC-0021 budget.
type guardCounters struct {
	mu               sync.Mutex
	callsByResult    map[string]int64
	errorsByClass    map[string]int64
	promptTokens     int64
	completionTokens int64
	lastDuration     time.Duration
}

func (c *guardCounters) record(result, class string, usage Usage, d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.callsByResult[result]++
	if class != "" {
		c.errorsByClass[class]++
	}
	c.promptTokens += int64(usage.PromptTokens)
	c.completionTokens += int64(usage.CompletionTokens)
	c.lastDuration = d
}

// CountersSnapshot is a point-in-time copy of one guard's counters.
type CountersSnapshot struct {
	CallsByResult    map[string]int64
	ErrorsByClass    map[string]int64
	PromptTokens     int64
	CompletionTokens int64
	LastDuration     time.Duration
}

// Counters returns a copy of guard's counters, zero-valued when the guard
// has never been called.
func (g *Gate) Counters(guard string) CountersSnapshot {
	g.mu.Lock()
	c, ok := g.counters[guard]
	g.mu.Unlock()
	if !ok {
		return CountersSnapshot{
			CallsByResult: map[string]int64{},
			ErrorsByClass: map[string]int64{},
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	calls := make(map[string]int64, len(c.callsByResult))
	for k, v := range c.callsByResult {
		calls[k] = v
	}
	classes := make(map[string]int64, len(c.errorsByClass))
	for k, v := range c.errorsByClass {
		classes[k] = v
	}
	return CountersSnapshot{
		CallsByResult:    calls,
		ErrorsByClass:    classes,
		PromptTokens:     c.promptTokens,
		CompletionTokens: c.completionTokens,
		LastDuration:     c.lastDuration,
	}
}

// pool returns guard's slot pool, creating it at the guard's configured
// size on first use. The pool is daemon-wide and shared by every screen
// that names the guard, and by no utility call.
func (g *Gate) pool(guard string, size int) chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	if p, ok := g.pools[guard]; ok {
		return p
	}
	p := make(chan struct{}, size)
	g.pools[guard] = p
	return p
}

func (g *Gate) countersFor(guard string) *guardCounters {
	g.mu.Lock()
	defer g.mu.Unlock()
	if c, ok := g.counters[guard]; ok {
		return c
	}
	c := &guardCounters{
		callsByResult: map[string]int64{},
		errorsByClass: map[string]int64{},
	}
	g.counters[guard] = c
	return c
}

// Screen issues one call per spec over the same screened text, all at
// once, and returns each spec's result in spec order. A screen therefore
// ends within the largest timeout among its guards (REQ-8 "Bounded
// concurrency"), and no call is ever retried within it (REQ-6).
func (g *Gate) Screen(ctx context.Context, text string, specs ...CallSpec) []CallResult {
	hash, redacted := Prepare(text)
	results := make([]CallResult, len(specs))
	var wg sync.WaitGroup
	for i, spec := range specs {
		if err := spec.validateGate(); err != nil {
			results[i] = CallResult{Entry: spec.Entry, Guard: spec.Guard, Policy: spec.Policy, Hash: hash, Err: &GateCallError{
				Guard: spec.Guard, Policy: spec.Policy, Entry: spec.Entry, Err: fmt.Errorf("invalid gate call: %w", err)},
			}
			continue
		}
		wg.Add(1)
		go func(i int, spec CallSpec) {
			defer wg.Done()
			results[i] = g.call(ctx, spec, hash, redacted, true)
		}(i, spec)
	}
	wg.Wait()
	return results
}

// Probe runs one call through a guard's pool and counters without an
// entry point: a probe is not a gate call, can never withhold work, and
// counts in its own probe series. #842's probe action and doctor rows use
// it.
func (g *Gate) Probe(ctx context.Context, spec CallSpec, text string) CallResult {
	if err := spec.validateCall(); err != nil {
		return CallResult{Guard: spec.Guard, Policy: spec.Policy, Err: &GateCallError{
			Guard: spec.Guard, Policy: spec.Policy, Err: fmt.Errorf("invalid probe: %w", err)},
		}
	}
	hash, redacted := Prepare(text)
	return g.call(ctx, spec, hash, redacted, false)
}

// call performs one model call: slot, request, classification,
// attestation, counters. gate distinguishes a gate call from a probe for
// the error wrapper and the counters' result keys.
func (g *Gate) call(ctx context.Context, spec CallSpec, hash, text string, gate bool) (res CallResult) {
	start := g.now()
	res = CallResult{Entry: spec.Entry, Guard: spec.Guard, Policy: spec.Policy, Hash: hash}
	defer func() {
		res.StartedAt = start
		res.Duration = g.now().Sub(start)
		class := ""
		if res.Err != nil {
			class = res.Err.(*GateCallError).Class()
		}
		result := "ok"
		if class != "" {
			result = "error"
		}
		if !gate {
			result = "probe:" + result
		}
		g.countersFor(spec.Guard).record(result, class, res.Usage, res.Duration)
	}()

	// The gate's lifecycle: a call cancelled by daemon shutdown ends as
	// interrupted, with the shutdown cause preserved through the chain
	// (REQ-8 "Held at shutdown").
	callCtx, callCancel := context.WithCancelCause(ctx)
	defer callCancel(nil)
	stop := context.AfterFunc(g.ctx, func() { callCancel(context.Cause(g.ctx)) })
	defer stop()
	if err := g.ctx.Err(); err != nil {
		res.Err = &GateCallError{Guard: spec.Guard, Policy: spec.Policy, Entry: spec.Entry, Err: ErrInterrupted}
		return res
	}

	// One deadline covers the slot wait, the request and the body read
	// (REQ-8 "Bounded concurrency").
	deadlineCtx, deadlineCancel := context.WithTimeout(callCtx, spec.Timeout)
	defer deadlineCancel()

	pool := g.pool(spec.Guard, spec.MaxConcurrency)
	select {
	case pool <- struct{}{}:
		defer func() { <-pool }()
		// The slot may arrive together with an already-expired deadline
		// (a wait that consumed the whole budget): abandon the call
		// before it ever reaches the guard.
		if err := deadlineCtx.Err(); err != nil {
			res.Err = g.classifyCtx(deadlineCtx, spec)
			return res
		}
	case <-deadlineCtx.Done():
		res.Err = g.classifyCtx(deadlineCtx, spec)
		return res
	}

	req := &Request{
		Model:       spec.Model,
		Messages:    []Message{{Role: "user", Content: text}},
		MaxTokens:   spec.MaxTokens,
		Temperature: spec.Temperature,
		N:           1,
		Stream:      false,
		Logprobs:    spec.Logprobs,
		TopLogprobs: spec.TopLogprobs,
	}
	resp, err := g.client.Do(deadlineCtx, req)
	if err != nil {
		res.Err = g.classify(callCtx, deadlineCtx, err, spec)
		return res
	}
	res.Usage = resp.Usage
	if len(resp.Choices) == 0 {
		res.Err = &GateCallError{Guard: spec.Guard, Policy: spec.Policy, Entry: spec.Entry,
			Err: fmt.Errorf("%w: response carries no choices", ErrParse)}
		return res
	}
	res.ServedModel = SanitizeModel(resp.Model)
	res.Content = resp.Choices[0].Message.Content
	if resp.Choices[0].Logprobs != nil {
		res.Logprobs = resp.Choices[0].Logprobs.Content
	}

	// Attestation is byte for byte and comes before any use of the
	// response: a different or missing model is model_mismatch even when
	// the response carries a usable score (REQ-6).
	if resp.Model != spec.Model {
		res.Err = &GateCallError{Guard: spec.Guard, Policy: spec.Policy, Entry: spec.Entry,
			Err: fmt.Errorf("%w: guard model %q, served %q", ErrModelMismatch, spec.Model, res.ServedModel)}
		return res
	}

	// A call that asked for logprobs and cannot read them is no_logprobs,
	// whatever the message text says (REQ-3, REQ-6).
	if spec.Logprobs != nil && *spec.Logprobs {
		if res.Logprobs == nil {
			res.Err = &GateCallError{Guard: spec.Guard, Policy: spec.Policy, Entry: spec.Entry,
				Err: fmt.Errorf("%w: response carries no choices[0].logprobs", ErrNoLogprobs)}
			return res
		}
		for _, cl := range res.Logprobs {
			if len(cl.TopLogprobs) == 0 {
				res.Err = &GateCallError{Guard: spec.Guard, Policy: spec.Policy, Entry: spec.Entry,
					Err: fmt.Errorf("%w: response logprobs carry no top_logprobs", ErrNoLogprobs)}
				return res
			}
		}
	}
	return res
}

// classifyCtx maps a context failure to its class: an expired deadline is
// timeout, anything cancelled (shutdown included) is interrupted.
func (g *Gate) classifyCtx(ctx context.Context, spec CallSpec) *GateCallError {
	if errors.Is(context.Cause(ctx), errShutdown) || errors.Is(ctx.Err(), errShutdown) {
		return &GateCallError{Guard: spec.Guard, Policy: spec.Policy, Entry: spec.Entry,
			Err: fmt.Errorf("%w: daemon shut down", ErrInterrupted)}
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &GateCallError{Guard: spec.Guard, Policy: spec.Policy, Entry: spec.Entry,
			Err: fmt.Errorf("%w: no parsed response within %s (slot wait included)", ErrTimeout, spec.Timeout)}
	}
	return &GateCallError{Guard: spec.Guard, Policy: spec.Policy, Entry: spec.Entry,
		Err: fmt.Errorf("%w: %v", ErrInterrupted, ctx.Err())}
}

// classify maps a client failure to REQ-6's closed set, wrapped with the
// guard and policy. The callCtx is checked first: a context failure wins
// over the transport error that carried it, and the shutdown cause
// preserved on callCtx keeps shutdown reading as interrupted.
func (g *Gate) classify(callCtx, deadlineCtx context.Context, err error, spec CallSpec) *GateCallError {
	var se *StatusError
	switch {
	case errors.Is(context.Cause(callCtx), errShutdown):
		return &GateCallError{Guard: spec.Guard, Policy: spec.Policy, Entry: spec.Entry,
			Err: fmt.Errorf("%w: daemon shut down", ErrInterrupted)}
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(context.Cause(deadlineCtx), context.DeadlineExceeded):
		return &GateCallError{Guard: spec.Guard, Policy: spec.Policy, Entry: spec.Entry,
			Err: fmt.Errorf("%w: no parsed response within %s (slot wait included)", ErrTimeout, spec.Timeout)}
	case errors.Is(err, context.Canceled):
		return g.classifyCtx(callCtx, spec)
	case errors.Is(err, ErrBodyTooLarge):
		return &GateCallError{Guard: spec.Guard, Policy: spec.Policy, Entry: spec.Entry,
			Err: fmt.Errorf("%w: %v", ErrParse, err)}
	case errors.Is(err, ErrDecode):
		return &GateCallError{Guard: spec.Guard, Policy: spec.Policy, Entry: spec.Entry,
			Err: fmt.Errorf("%w: %v", ErrParse, err)}
	case errors.As(err, &se):
		return &GateCallError{Guard: spec.Guard, Policy: spec.Policy, Entry: spec.Entry,
			Err: fmt.Errorf("%w: %d", ErrHTTPStatus, se.Code)}
	default:
		// A connection, TLS or read failure.
		return &GateCallError{Guard: spec.Guard, Policy: spec.Policy, Entry: spec.Entry,
			Err: fmt.Errorf("%w: %v", ErrTransport, err)}
	}
}

// SanitizeModel renders a served model name safe to record: control
// characters removed, then capped at the ledger's MaxStringBytes (256) on
// a rune boundary (REQ-6 "served_model").
func SanitizeModel(m string) string {
	var b strings.Builder
	b.Grow(len(m))
	for _, r := range m {
		if unicode.IsControl(r) {
			continue
		}
		b.WriteRune(r)
	}
	s := b.String()
	if len(s) > ledger.MaxStringBytes {
		s = s[:ledger.MaxStringBytes]
		// Do not leave half a rune at the cap.
		for !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return s
}
