// The [harness.<name>.model_pin] sub-table: decoding, the defaults REQ-1
// defines, and the harness-level rules — mutual exclusion with `model`, the
// adapter's route offer (REQ-2), the `transcripts` binding a `command` pin
// needs, and the attestation capability check (REQ-8) that turns
// attest = "full" into a load error where the client's transcripts cannot
// evidence a provider.
//
// The sub-table decodes inside its harness's own primitive, so it is legal
// wherever the harness table is legal, project files and drop-ins included
// (SPEC-0020 REQ-1: "A project harness.toml MAY declare model_pin").
//
// Governing: ADR-0026, SPEC-0020 REQ-1, REQ-2, REQ-8.

package config

import (
	"fmt"
	"strings"

	"github.com/stump-wtf/harness/internal/adapter"
	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/modelpin"
)

// rawModelPin decodes [harness.<name>.model_pin]. Strings and slices are
// presence-checkable as written; the enum keys default at build time, not
// in the struct, so an explicit default reads identically to an omitted key
// (the REQ-1 defaults are the same values).
type rawModelPin struct {
	Route          string   `toml:"route"`
	Model          string   `toml:"model"`
	Providers      []string `toml:"providers"`
	AllowFallbacks bool     `toml:"allow_fallbacks"`
	DataPolicy     string   `toml:"data_policy"`
	Attest         string   `toml:"attest"`
	OnMismatch     string   `toml:"on_mismatch"`
	AcceptServed   []string `toml:"accept_served"`
	GatewayModel   string   `toml:"gateway_model"`
	GatewayURL     string   `toml:"gateway_url"`
}

// buildModelPin validates rh's pin (when it has one) against the table
// rules, the client, and the harness's own keys. It returns the parsed pin
// and the line to blame — the pin's own table header when one can be found,
// the harness's line otherwise. Errors keep their sentinel (an unoffered
// route, a capability gap) through the located error, per SPEC-0020's
// Error Handling Standards.
func buildModelPin(filename, name string, line int, rh rawHarness, kind string) (*modelpin.Pin, int, error) {
	if rh.ModelPin == nil {
		return nil, line, nil
	}
	pLine := rh.lineOfKey("model_pin", line)
	if strings.TrimSpace(rh.Model) != "" {
		err := fmt.Errorf(`"model_pin" and "model" are mutually exclusive — "model_pin" supplies the model; remove "model"`)
		return nil, rh.lineOfKey("model", pLine), newError(filename, rh.lineOfKey("model", pLine), "harness %q: %v", name, err)
	}
	rp := rh.ModelPin
	p := modelpin.Pin{
		Route:          modelpin.Route(strings.TrimSpace(rp.Route)),
		Model:          strings.TrimSpace(rp.Model),
		Providers:      rp.Providers,
		AllowFallbacks: rp.AllowFallbacks,
		DataPolicy:     orDefaultStr(rp.DataPolicy, modelpin.DataPolicyProviderDefault),
		Attest:         orDefaultStr(rp.Attest, modelpin.AttestFull),
		OnMismatch:     orDefaultStr(rp.OnMismatch, modelpin.OnMismatchHold),
		AcceptServed:   rp.AcceptServed,
		GatewayModel:   strings.TrimSpace(rp.GatewayModel),
		GatewayURL:     strings.TrimSpace(rp.GatewayURL),
	}
	// `route` is required; a blank one would otherwise read as "invalid"
	// without naming the omission.
	if p.Route == "" {
		return nil, pLine, newError(filename, pLine, "harness %q: \"model_pin\": \"route\" is required (want one of: openrouter, anthropic, litellm)", name)
	}
	if err := modelpin.Validate(p); err != nil {
		return nil, pLine, newError(filename, pLine, "harness %q: \"model_pin\": %v", name, err)
	}
	sentinel, msg := checkPin(kind, rh.Transcripts, &p)
	if msg != "" {
		return nil, pLine, newSentinelError(filename, pLine, sentinel, "harness %q: \"model_pin\": %s", name, msg)
	}
	return &p, pLine, nil
}

// checkPin is the client half of pin validation, shared by the config
// parsers and the project wire so the two front doors cannot drift: the
// adapter must render the route (REQ-2), a `command` pin must have a
// transcript binding (REQ-2), and attest = "full" must be evidenceable
// (REQ-8). It returns the sentinel category and the message body; the
// caller prefixes the harness name and, for config, the location.
func checkPin(kind, transcripts string, p *modelpin.Pin) (sentinel error, msg string) {
	if !adapter.PinOffers(kind, p.Route) {
		offered := adapter.PinRoutes(kind)
		if len(offered) == 0 {
			return modelpin.ErrRouteNotOffered, fmt.Sprintf(
				"%s offers no model-pin route in this version (route %q); unpinned, set \"model\" beside the prompt instead",
				kind, string(p.Route))
		}
		return modelpin.ErrRouteNotOffered, fmt.Sprintf(
			"%s does not offer route %q (offered here: %s)",
			kind, string(p.Route), joinRoutes(offered))
	}
	// #699 makes `transcripts` mandatory on every command harness; whichever
	// check fires, the error names `transcripts`.
	if kind == core.AdapterCommand && transcripts == "" {
		return nil, "a \"command\" pin requires \"transcripts\": nothing could be attested without a transcript binding"
	}
	if p.Attest == modelpin.AttestFull {
		if evidence := adapter.PinCapability(kind, transcripts, p.Route); evidence != modelpin.CapabilityModelProvider {
			return modelpin.ErrCapabilityGap, fmt.Sprintf(
				"attest = \"full\" needs model+provider evidence, but %s transcripts evidence %q on the %s route — set attest = \"model\"",
				pinEvidenceOwner(kind, transcripts), evidence, string(p.Route))
		}
	}
	return nil, ""
}

// ValidateModelPin re-runs every harness-level pin rule for an assembled
// core.Harness — the project wire carries the pin and re-validates it
// (SPEC-0020 REQ-18), so a pin that would not load from a file cannot be
// registered over the socket. Errors keep their sentinel for errors.Is.
func ValidateModelPin(name, kind, model, transcripts string, p *modelpin.Pin) error {
	if p == nil {
		return nil
	}
	if strings.TrimSpace(model) != "" {
		return fmt.Errorf("harness %q: \"model_pin\" and \"model\" are mutually exclusive — \"model_pin\" supplies the model", name)
	}
	if err := modelpin.Validate(*p); err != nil {
		return fmt.Errorf("harness %q: \"model_pin\": %w", name, err)
	}
	sentinel, msg := checkPin(kind, transcripts, p)
	if msg == "" {
		return nil
	}
	if sentinel != nil {
		return fmt.Errorf("harness %q: \"model_pin\": %s: %w", name, msg, sentinel)
	}
	return fmt.Errorf("harness %q: \"model_pin\": %s", name, msg)
}

// pinEvidenceOwner names whose transcripts a pin attests from: a command
// harness attests from the adapter its `transcripts` names.
func pinEvidenceOwner(kind, transcripts string) string {
	if kind == core.AdapterCommand {
		if transcripts == "" {
			return "the bound"
		}
		return "the bound " + transcripts + " adapter's"
	}
	return kind + "'s"
}

func joinRoutes(rs []modelpin.Route) string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = string(r)
	}
	return strings.Join(out, ", ")
}

func orDefaultStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
