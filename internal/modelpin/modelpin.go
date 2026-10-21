// Package modelpin holds the model pin table: the parsed shape of a
// [harness.<name>.model_pin] block, its validation, and the types route and
// attestation capability checks consult (SPEC-0020, ADR-0026).
//
// The package is deliberately free of adapter and config knowledge: the
// per-adapter route offer and evidence tables live in internal/adapter,
// where SPEC-0020 puts them. A pin declares ONE upstream model, the route
// that may serve it, the providers allowed to serve it, and the data policy
// the request must carry; fallbacks are not a thing a pin can allow.
//
// Governing: ADR-0026, SPEC-0020 REQ-1 (this file), REQ-2 (adapter tables).
package modelpin

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// Route is the path a pinned model is served through (SPEC-0020 REQ-1).
type Route string

// Routes in table order. Governing: SPEC-0020 REQ-1.
const (
	RouteOpenRouter Route = "openrouter"
	RouteAnthropic  Route = "anthropic"
	RouteLiteLLM    Route = "litellm"
)

// RouteValues lists the accepted "route" values, for error messages.
var RouteValues = []Route{RouteOpenRouter, RouteAnthropic, RouteLiteLLM}

// Data policies a pin may request. Governing: SPEC-0020 REQ-1.
const (
	DataPolicyZDR             = "zdr"
	DataPolicyDenyCollection  = "deny-collection"
	DataPolicyProviderDefault = "provider-default"
)

// DataPolicyValues lists the accepted "data_policy" values.
var DataPolicyValues = []string{DataPolicyZDR, DataPolicyDenyCollection, DataPolicyProviderDefault}

// Attestation depths. `full` attests model and provider, `model` only the
// served model. Governing: SPEC-0020 REQ-1, REQ-8.
const (
	AttestFull  = "full"
	AttestModel = "model"
)

// AttestValues lists the accepted "attest" values.
var AttestValues = []string{AttestFull, AttestModel}

// Mismatch actions for a call the pin rejects. Governing: SPEC-0020 REQ-1,
// REQ-12 (hold) and REQ-13 (fail-run).
const (
	OnMismatchHold    = "hold"
	OnMismatchFailRun = "fail-run"
)

// OnMismatchValues lists the accepted "on_mismatch" values.
var OnMismatchValues = []string{OnMismatchHold, OnMismatchFailRun}

// Sentinel errors for the pin checks (SPEC-0020 "Error Handling Standards").
// ErrMismatch and ErrUnattestedEvidence ride the attestation path the
// renderer stories build; they are defined here so every error this spec's
// checks wrap names a sentinel.
var (
	ErrRouteNotOffered    = errors.New("route not offered")
	ErrCapabilityGap      = errors.New("attestation capability gap")
	ErrUnbeatableOverride = errors.New("unbeatable override")
	ErrMismatch           = errors.New("model mismatch")
	ErrUnattestedEvidence = errors.New("unattested evidence")
)

// Capability is what an adapter's transcripts can evidence for a route:
// nothing, the served model, or model and upstream provider.
type Capability int

const (
	CapabilityNone Capability = iota
	CapabilityModel
	CapabilityModelProvider
)

// String renders the capability for error messages (SPEC-0020 REQ-8 names
// the missing evidence, so the gap must be sayable).
func (c Capability) String() string {
	switch c {
	case CapabilityModel:
		return "model"
	case CapabilityModelProvider:
		return "model+provider"
	default:
		return "none"
	}
}

// Pin is the parsed [harness.<name>.model_pin] table with defaults applied.
// Providers distinguishes absent (nil) from an explicit empty list, the
// same presence semantics rawHarness keys use, because the anthropic route
// rejects the key on presence.
type Pin struct {
	Route          Route
	Model          string
	Providers      []string
	AllowFallbacks bool
	DataPolicy     string
	Attest         string
	OnMismatch     string
	AcceptServed   []string
	GatewayModel   string
	GatewayURL     string
}

// providerSlug is the upstream provider slug pattern (SPEC-0020 REQ-1).
var providerSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*(/[a-z0-9-]+)?$`)

// Validate checks one pin table against the REQ-1 rules, assuming defaults
// are already applied (the config loader fills them; a zero value fails).
// Errors are unprefixed: the caller wraps them with the harness name and
// the declaring file and line.
func Validate(p Pin) error {
	switch p.Route {
	case RouteOpenRouter, RouteAnthropic, RouteLiteLLM:
	default:
		return fmt.Errorf("\"route\" must be one of: openrouter, anthropic, litellm (got %q)", string(p.Route))
	}
	if p.Model == "" {
		return errors.New(`"model" is required: a pin must name one model`)
	}
	if strings.ContainsFunc(p.Model, func(r rune) bool { return r == ' ' || r == '\t' || r == '\n' }) {
		return fmt.Errorf(`"model" must be a single token (model ids carry no whitespace), got %q`, p.Model)
	}
	switch p.DataPolicy {
	case DataPolicyZDR, DataPolicyDenyCollection, DataPolicyProviderDefault:
	default:
		return fmt.Errorf(`"data_policy" must be one of: zdr, deny-collection, provider-default (got %q)`, p.DataPolicy)
	}
	switch p.Attest {
	case AttestFull, AttestModel:
	default:
		return fmt.Errorf(`"attest" must be one of: full, model (got %q)`, p.Attest)
	}
	switch p.OnMismatch {
	case OnMismatchHold, OnMismatchFailRun:
	default:
		return fmt.Errorf(`"on_mismatch" must be one of: hold, fail-run (got %q)`, p.OnMismatch)
	}
	if p.AllowFallbacks {
		return errors.New(`"allow_fallbacks = true": a pin that allows fallbacks is not a pin`)
	}
	for i, prov := range p.Providers {
		if !providerSlug.MatchString(prov) {
			return fmt.Errorf(`"providers[%d]" is not a provider slug (want ^[a-z0-9][a-z0-9-]*(/[a-z0-9-]+)?$): %q`, i, prov)
		}
	}
	for i, served := range p.AcceptServed {
		if served == "" || strings.ContainsFunc(served, func(r rune) bool { return r == ' ' || r == '\t' || r == '\n' }) {
			return fmt.Errorf(`"accept_served[%d]" must be a single model id token, got %q`, i, served)
		}
	}
	switch p.Route {
	case RouteOpenRouter:
		if p.GatewayModel != "" {
			return errors.New(`"gateway_model" is a litellm key; the openrouter route names the upstream model directly`)
		}
		if p.GatewayURL != "" {
			return errors.New(`"gateway_url" is a litellm key; the openrouter route names the upstream model directly`)
		}
		if p.Model == "openrouter/auto" {
			return fmt.Errorf(`"model" must name one model, not an auto-router (%q)`, p.Model)
		}
		if strings.Contains(p.Model, ":") {
			return fmt.Errorf(`"model" must not carry a routing variant (no ":"): a pin selects one model, got %q`, p.Model)
		}
		if p.Providers == nil || len(p.Providers) == 0 {
			return errors.New(`"providers" is required on the openrouter route: a pin names who may serve the model`)
		}
	case RouteAnthropic:
		if p.Providers != nil {
			return errors.New(`"providers" is rejected on the anthropic route: first-party is the only provider`)
		}
		if p.GatewayModel != "" {
			return errors.New(`"gateway_model" is a litellm key; the anthropic route names the upstream model directly`)
		}
		if p.GatewayURL != "" {
			return errors.New(`"gateway_url" is a litellm key; the anthropic route names the upstream model directly`)
		}
		if p.DataPolicy != DataPolicyProviderDefault {
			return fmt.Errorf(`"data_policy = %q" cannot be carried on the anthropic route: zero data retention there is an account agreement Harness cannot request, only "provider-default" is expressible`, p.DataPolicy)
		}
	case RouteLiteLLM:
		if p.GatewayModel == "" {
			return errors.New(`"gateway_model" is required on the litellm route: it is the alias the client requests`)
		}
		if strings.ContainsFunc(p.GatewayModel, func(r rune) bool { return r == ' ' || r == '\t' || r == '\n' }) {
			return fmt.Errorf(`"gateway_model" must be a single token, got %q`, p.GatewayModel)
		}
		if p.GatewayURL == "" {
			return errors.New(`"gateway_url" is required on the litellm route`)
		}
		return validateGatewayURL(p.GatewayURL)
	}
	return nil
}

// validateGatewayURL enforces the REQ-1 URL rule: https, unless the gateway
// is loopback (an operator running LiteLLM beside the daemon).
func validateGatewayURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf(`"gateway_url" is not a valid URL: %q`, raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if isLoopbackHost(u.Hostname()) {
			return nil
		}
		return fmt.Errorf(`"gateway_url" must be https unless the gateway is loopback: %q`, raw)
	default:
		return fmt.Errorf(`"gateway_url" scheme must be https (or http on loopback), got %q`, u.Scheme)
	}
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Equal reports whether two pins express the same table. Pointer equality
// would make every wire round-trip look like a pin change; the project
// re-up compares definitions by value (SPEC-0020 REQ-18).
func Equal(a, b *Pin) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Route == b.Route && a.Model == b.Model &&
		slices.Equal(a.Providers, b.Providers) &&
		a.AllowFallbacks == b.AllowFallbacks &&
		a.DataPolicy == b.DataPolicy && a.Attest == b.Attest &&
		a.OnMismatch == b.OnMismatch &&
		slices.Equal(a.AcceptServed, b.AcceptServed) &&
		a.GatewayModel == b.GatewayModel && a.GatewayURL == b.GatewayURL
}
