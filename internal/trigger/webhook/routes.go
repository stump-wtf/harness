package webhook

// The route table: which `/hooks/<name>` paths answer, and how each one
// verifies.
//
// A table is immutable once built and is swapped whole on reload through an
// atomic pointer. A request loads the pointer once, at route lookup, and
// carries that table through its whole pipeline — so a request in flight
// completes against the table it started with, however many reloads land
// while it is reading its body. That is REQ "Source Reconciliation On Reload"
// for the webhook half, and it needs no lock on the request path.
//
// Only a source that is enabled AND bound by at least one harness gets a
// route. Unknown, disabled and unbound names are all simply absent, which is
// what makes their 404s byte-identical (REQ "Webhook Routes"): there is one
// code path for "no such route", and it cannot tell the three apart because
// the table does not record why a name is missing.
//
// Governing: ADR-0021; SPEC-0014 REQ "Webhook Routes", REQ "Source
// Reconciliation On Reload", REQ "Concurrency Safety".
//
// @joestump 09/23/2026 - Introduced with the SPEC-0014 webhook listener (#458).
// @joestump 09/24/2026 - A route keeps its limits across a reload (#460).

import (
	"charm.land/log/v2"

	"github.com/stump-wtf/harness/internal/core"
)

// route is one served `/hooks/<name>`.
type route struct {
	// src is the source as the config declared it at build time.
	src core.WebhookSource
	// ref is the source reference, "webhook.<name>".
	ref string
	// verifier authenticates deliveries. Never nil: a source that cannot be
	// verified gets a refusing verifier (verify.go).
	verifier Verifier
	// limits is the route's de-duplication set and token bucket. Never nil,
	// and shared with the previous table's route when inherit carried it.
	limits *routeLimits
}

// table is an immutable route set.
type table struct {
	routes map[string]*route
}

// lookup returns the route for name, or nil.
func (t *table) lookup(name string) *route {
	if t == nil {
		return nil
	}
	return t.routes[name]
}

// buildTable builds the route table cfg describes. newVerifier is the scheme
// registry (NewVerifier in production; a test substitutes a spy).
//
// A source whose verifier cannot be built still gets a route, answering 401 to
// everything, and a warning here. Leaving it out would answer 404 — which
// tells an operator debugging a GitHub hook "you have the URL wrong", when the
// truth is "this scheme is not implemented yet".
func buildTable(cfg *core.Config, newVerifier NewVerifierFunc, logger *log.Logger) *table {
	t := &table{routes: map[string]*route{}}
	if cfg == nil {
		return t
	}
	for _, src := range cfg.OrderedWebhooks() {
		ref := core.SourceKindWebhook + "." + src.Name
		if !src.Enabled || len(cfg.BoundHarnesses(ref)) == 0 {
			continue
		}
		v, err := newVerifier(src)
		if err != nil || v == nil {
			if err == nil {
				err = ErrSchemeUnavailable
			}
			logger.Warn("webhook route refuses every delivery", "source", ref, "verify", string(src.Verify), "err", err.Error())
			v = refusing{why: err}
		}
		t.routes[src.Name] = &route{src: src, ref: ref, verifier: v, limits: newRouteLimits(src.RateLimit)}
	}
	return t
}

// inherit hands each route in t the limits of prev's route of the same name,
// when its rate_limit is unchanged, so a reload neither refills a drained
// bucket nor forgets the delivery IDs that fired. A changed rate_limit starts
// fresh: a bucket sized for the old limit is meaningless under the new one.
// A route absent from prev — new, or re-enabled — starts fresh as well.
//
// It mutates t, so it runs before t is published, never after.
// Governing: SPEC-0014 REQ "Source Reconciliation On Reload".
func (t *table) inherit(prev *table) {
	if t == nil || prev == nil {
		return
	}
	for name, rt := range t.routes {
		if old := prev.routes[name]; old != nil && old.limits.rateLimit == rt.limits.rateLimit {
			rt.limits = old.limits
		}
	}
}
