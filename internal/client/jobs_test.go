package client

// @joestump 09/23/2026 - Added in review of #585: ProtoMinor 11's note
// said the client refuses --event against an older daemon, and nothing did.

import (
	"strings"
	"testing"

	"github.com/stump-wtf/harness/internal/protocol"
)

// TestTriggerWithEventRefusesAnOlderDaemon: a daemon before ProtoMinor 11
// ignores ControlReq.Event, so sending one would start a run with no event.
// The refusal must happen before any request is written — the Client here has
// no connection, so reaching the wire would panic rather than pass.
func TestTriggerWithEventRefusesAnOlderDaemon(t *testing.T) {
	for _, v := range []string{"1.10", "1", ""} {
		c := &Client{daemon: protocol.Hello{ProtoVersion: v}}
		_, err := c.TriggerWithEvent("pr-review", []byte(`{"version":1}`))
		if err == nil || !strings.Contains(err.Error(), "predates trigger events") {
			t.Errorf("proto %q: err = %v, want a refusal", v, err)
		}
	}
}

func TestProtoMinor(t *testing.T) {
	cases := map[string]struct {
		minor int
		ok    bool
	}{
		"1.11": {11, true},
		"1.9":  {9, true},
		"1":    {0, false},
		"x.y":  {0, false},
	}
	for in, want := range cases {
		if m, ok := protoMinor(in); m != want.minor || ok != want.ok {
			t.Errorf("protoMinor(%q) = %d, %v; want %d, %v", in, m, ok, want.minor, want.ok)
		}
	}
	if m, ok := protoMinor(protocol.ProtoVersion); !ok || m < eventTriggerMinor {
		t.Errorf("this build's own proto %q would refuse its own --event", protocol.ProtoVersion)
	}
}

// TestPersonaKeysRefuseAnOlderDaemon: a daemon before ProtoMinor 13 ignores
// allowed_tools and mcp_config, so the one-shot would run unrestricted. Both
// project_up and scratch_run must refuse before the wire (no connection here:
// reaching it would panic).
func TestPersonaKeysRefuseAnOlderDaemon(t *testing.T) {
	defs := []protocol.ProjectHarness{
		{Name: "reviewer", AllowedTools: []string{"Read"}},
		{Name: "reviewer", MCPConfig: "/p/mcp.json"},
		{Name: "reviewer", SystemPromptFile: "/p/system.md"},
	}
	for _, v := range []string{"1.12", "1", ""} {
		c := &Client{daemon: protocol.Hello{ProtoVersion: v}}
		for _, def := range defs {
			if _, err := c.ProjectUp("p", []protocol.ProjectHarness{{Name: "plain"}, def}); err == nil || !strings.Contains(err.Error(), "predates system_prompt_file") {
				t.Errorf("project up, proto %q, %+v: err = %v, want a refusal", v, def, err)
			}
			if _, err := c.ScratchRun(def, ""); err == nil || !strings.Contains(err.Error(), "predates system_prompt_file") {
				t.Errorf("scratch run, proto %q, %+v: err = %v, want a refusal", v, def, err)
			}
		}
	}
	current := &Client{daemon: protocol.Hello{ProtoVersion: protocol.ProtoVersion}}
	if err := current.checkPersonaKeys(defs); err != nil {
		t.Errorf("this build's own proto %q refuses its own persona keys: %v", protocol.ProtoVersion, err)
	}
	old := &Client{daemon: protocol.Hello{ProtoVersion: "1.12"}}
	if err := old.checkPersonaKeys([]protocol.ProjectHarness{{Name: "plain"}}); err != nil {
		t.Errorf("a definition without persona keys must still reach an older daemon: %v", err)
	}
}
