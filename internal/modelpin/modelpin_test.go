package modelpin

import (
	"strings"
	"testing"
)

func TestValidateOpenRouter(t *testing.T) {
	good := Pin{Route: RouteOpenRouter, Model: "z-ai/glm-5.3-flash", Providers: []string{"deepinfra"}, DataPolicy: DataPolicyZDR, Attest: AttestFull, OnMismatch: OnMismatchHold}
	if err := Validate(good); err != nil {
		t.Fatalf("complete OpenRouter pin rejected: %v", err)
	}
	for _, tc := range []struct {
		name string
		pin  Pin
		want string
	}{
		{"variant suffix", Pin{Route: RouteOpenRouter, Model: "anthropic/claude-x:thinking", Providers: []string{"anthropic"}, DataPolicy: DataPolicyProviderDefault, Attest: AttestModel, OnMismatch: OnMismatchHold}, ":"},
		{"auto router", Pin{Route: RouteOpenRouter, Model: "openrouter/auto", Providers: []string{"openrouter"}, DataPolicy: DataPolicyProviderDefault, Attest: AttestModel, OnMismatch: OnMismatchHold}, "one model"},
		{"no providers", Pin{Route: RouteOpenRouter, Model: "z-ai/glm-5.3-flash", DataPolicy: DataPolicyProviderDefault, Attest: AttestModel, OnMismatch: OnMismatchHold}, "providers"},
		{"bad slug", Pin{Route: RouteOpenRouter, Model: "x/y", Providers: []string{"Deep Infra"}, DataPolicy: DataPolicyProviderDefault, Attest: AttestModel, OnMismatch: OnMismatchHold}, "slug"},
		{"fallbacks", Pin{Route: RouteOpenRouter, Model: "x", Providers: []string{"x"}, AllowFallbacks: true, DataPolicy: DataPolicyProviderDefault, Attest: AttestModel, OnMismatch: OnMismatchHold}, "not a pin"},
		{"gateway leak", Pin{Route: RouteOpenRouter, Model: "x", Providers: []string{"x"}, DataPolicy: DataPolicyProviderDefault, Attest: AttestModel, OnMismatch: OnMismatchHold, GatewayModel: "alias"}, "gateway_model"},
	} {
		err := Validate(tc.pin)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want error naming %q", tc.name, err, tc.want)
		}
	}
}

func TestValidateAnthropic(t *testing.T) {
	ok := Pin{Route: RouteAnthropic, Model: "claude-opus-5", DataPolicy: DataPolicyProviderDefault, Attest: AttestFull, OnMismatch: OnMismatchHold}
	if err := Validate(ok); err != nil {
		t.Fatalf("anthropic pin rejected: %v", err)
	}
	zdr := ok
	zdr.DataPolicy = DataPolicyZDR
	err := Validate(zdr)
	if err == nil || !strings.Contains(err.Error(), "cannot be carried") {
		t.Fatalf("zdr on anthropic: got %v, want route-cannot-carry error", err)
	}
	withProviders := ok
	withProviders.Providers = []string{"first-party"}
	if err := Validate(withProviders); err == nil {
		t.Fatal("providers accepted on the anthropic route")
	}
}

func TestValidateLiteLLM(t *testing.T) {
	base := Pin{Route: RouteLiteLLM, Model: "z-ai/glm-5.3-flash", DataPolicy: DataPolicyProviderDefault, Attest: AttestModel, OnMismatch: OnMismatchHold, GatewayModel: "glm-pin", GatewayURL: "https://gw.example:4000"}
	if err := Validate(base); err != nil {
		t.Fatalf("litellm pin rejected: %v", err)
	}
	noAlias := base
	noAlias.GatewayModel = ""
	if err := Validate(noAlias); err == nil || !strings.Contains(err.Error(), "gateway_model") {
		t.Fatalf("missing gateway_model: %v", err)
	}
	for _, tc := range []struct{ url, want string }{
		{"http://gw.example:4000", "https"},
		{"http://localhost:4000", ""},
		{"http://127.0.0.1:4000", ""},
		{"ftp://gw.example", "scheme"},
	} {
		p := base
		p.GatewayURL = tc.url
		err := Validate(p)
		if tc.want == "" && err != nil {
			t.Errorf("gateway %s rejected: %v", tc.url, err)
		}
		if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("gateway %s: got %v, want naming %q", tc.url, err, tc.want)
		}
	}
}

func TestEqual(t *testing.T) {
	a := &Pin{Route: RouteOpenRouter, Model: "x", Providers: []string{"d"}}
	b := &Pin{Route: RouteOpenRouter, Model: "x", Providers: []string{"d"}}
	if !Equal(a, b) || !Equal(nil, nil) || Equal(a, nil) {
		t.Fatal("Equal disagrees with value semantics")
	}
	b.Providers = append(b.Providers, "n")
	if Equal(a, b) {
		t.Fatal("provider change missed")
	}
}
