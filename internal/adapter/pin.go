// The v1 model-pin capability tables (SPEC-0020 REQ-2, REQ-8).
//
// Two questions get answered here at config load:
//
//   - PinOffers: does this adapter render this route at all? A pin on a
//     client/route pair it does not offer is a load error naming the
//     offered alternatives.
//   - PinCapability: what can this adapter's transcripts evidence for the
//     route? attest = "full" on a `model`-only capability is a load error
//     offering attest = "model".
//
// Both are data, exactly the v1 tables in SPEC-0020 REQ-2 and the
// model-pinning design's "Evidence capability" section. Until agent-trace
// emits per-message usage items, every evidence row is `model` at best,
// taken from session-level metadata — which makes attest = "full" a load
// error everywhere except the anthropic route, where first-party is
// attested by construction (REQ-8). The check states that plainly rather
// than pretending.
//
// As the renderer stories land, adapters implement PinRenderer and
// PinEvidence below and these tables become the fallback the interfaces
// override; the load-time check consults whichever the adapter declares.
//
// Governing: ADR-0026, SPEC-0020 REQ-2, REQ-8.

package adapter

import (
	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/modelpin"
)

// PinRenderer is implemented by adapters that can express a model pin.
// RenderPin and Overrides join it with the renderer stories (SPEC-0020
// REQ-3, REQ-4); an adapter that implements neither offers no routes,
// which is how SPEC-0020 REQ-2 makes codex and generic fail at load.
type PinRenderer interface {
	// PinRoutes returns the routes this adapter can render pins for.
	PinRoutes() []modelpin.Route
}

// PinEvidence is implemented by adapters whose transcripts can evidence
// what a pinned call actually used, per route (SPEC-0020 REQ-8).
type PinEvidence interface {
	// Evidence reports what the adapter's transcripts record for the route.
	Evidence(route modelpin.Route) modelpin.Capability
}

// pinOffered lists the routes each kind renders (SPEC-0020 REQ-2, v1).
// `command` offers every route for ATTESTATION ONLY (it renders nothing;
// the operator's argv owns routing), and a pin on it requires the
// `transcripts` binding — checked by the config loader, because that is
// where the binding is known.
var pinOffered = map[string][]modelpin.Route{
	"crush":             {modelpin.RouteOpenRouter, modelpin.RouteLiteLLM},
	core.AdapterPi:      {modelpin.RouteOpenRouter, modelpin.RouteLiteLLM},
	core.AdapterOMP:     {modelpin.RouteOpenRouter, modelpin.RouteLiteLLM},
	"claude-code":       {modelpin.RouteAnthropic, modelpin.RouteLiteLLM},
	"codex":             nil,
	"generic":           nil,
	core.AdapterCommand: {modelpin.RouteOpenRouter, modelpin.RouteAnthropic, modelpin.RouteLiteLLM},
}

// PinRoutes returns the routes kind offers for pins, in table order.
func PinRoutes(kind string) []modelpin.Route {
	return pinOffered[kind]
}

// PinOffers reports whether kind renders route at all.
func PinOffers(kind string, route modelpin.Route) bool {
	for _, r := range pinOffered[kind] {
		if r == route {
			return true
		}
	}
	return false
}

// pinEvidence records what transcripts evidence per kind and route
// (design "Evidence capability", SPEC-0020 REQ-1/REQ-8). Pi/OMP record the
// API provider on the assistant message, so their OpenRouter row evidences
// model+provider — which is what lets the REQ-1 "complete OpenRouter pin"
// scenario load with attest = "full" by default. Crush records its own
// provider ID, not the upstream one, so its row stays `model` until a
// generation ID rides its transcript (the REQ-8 generation lookup then
// upgrades it). The anthropic route attests first-party by construction
// when ANTHROPIC_BASE_URL is unset (REQ-8), which is the Claude Code row.
// litellm hides the upstream behind the gateway, so every litellm row is
// `model`: attest = "full" there is a load error offering attest = "model".
var pinEvidence = map[string]map[modelpin.Route]modelpin.Capability{
	"crush": {
		modelpin.RouteOpenRouter: modelpin.CapabilityModel,
		modelpin.RouteLiteLLM:    modelpin.CapabilityModel,
	},
	core.AdapterPi: {
		modelpin.RouteOpenRouter: modelpin.CapabilityModelProvider,
		modelpin.RouteLiteLLM:    modelpin.CapabilityModel,
	},
	core.AdapterOMP: {
		modelpin.RouteOpenRouter: modelpin.CapabilityModelProvider,
		modelpin.RouteLiteLLM:    modelpin.CapabilityModel,
	},
	"claude-code": {
		modelpin.RouteAnthropic: modelpin.CapabilityModelProvider,
		modelpin.RouteLiteLLM:   modelpin.CapabilityModel,
	},
	"codex":   {},
	"generic": {},
}

// PinCapability reports what kind's transcripts evidence for route. A
// `command` harness delegates to the kind its `transcripts` key names
// (SPEC-0020 REQ-2: the daemon only attests what that adapter records);
// an empty or unknown source evidences nothing.
func PinCapability(kind, transcripts string, route modelpin.Route) modelpin.Capability {
	if kind == core.AdapterCommand {
		kind = transcripts
	}
	if routes, ok := pinEvidence[kind]; ok {
		if c, ok := routes[route]; ok {
			return c
		}
	}
	return modelpin.CapabilityNone
}
