package runtrace

// Pi, OMP And Transcript Binding Sources
//
// Governing tests: SPEC-0017 REQ-13 "Pi And OMP Adapters" (the Pi reader at
// each CLI's session root, resolved from the harness's environment) and REQ-4
// "Transcript Binding" (a bound command harness correlates as the adapter it
// names).

import (
	"errors"
	"slices"
	"testing"

	"github.com/stump-wtf/agent-trace/tail"
)

func TestSourcesPiFollowsAgentDir(t *testing.T) {
	for _, tc := range []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"HOME": "/home/agent"}, "/home/agent/.pi/agent/sessions"},
		{map[string]string{"HOME": "/home/agent", "PI_CODING_AGENT_DIR": "/srv/pi"}, "/srv/pi/sessions"},
	} {
		got, err := Sources(Scope{Adapter: "pi", Env: tc.env})
		if err != nil {
			t.Fatalf("Sources(pi, %v): %v", tc.env, err)
		}
		if len(got) != 1 {
			t.Fatalf("Sources(pi) = %#v, want one reader", got)
		}
		if a, ok := got[0].(*tail.PiAdapter); !ok || a.Dir != tc.want {
			t.Errorf("Sources(pi, %v) = %#v, want a PiAdapter at %s", tc.env, got[0], tc.want)
		}
	}
	if !slices.Contains(DiscoveryEnvKeys, "PI_CODING_AGENT_DIR") {
		t.Error("PI_CODING_AGENT_DIR is not a discovery key, so an env_file relocating the agent directory is never read")
	}
}

// OMP ships unobserved: its sessions do not parse with agent-trace's Pi reader
// yet, so it reports no trajectory — the caller falls back to the log.
func TestSourcesOMPHasNoTrajectoryYet(t *testing.T) {
	if _, err := Sources(Scope{Adapter: "omp", Env: map[string]string{"HOME": "/home/agent"}}); !errors.Is(err, ErrNoTrajectory) {
		t.Fatalf("Sources(omp) err = %v, want ErrNoTrajectory", err)
	}
}

// pi and omp name exactly the agent they spawn, so neither is a possible
// author of another kind's session; an unbound command harness, like generic,
// is a possible author of anything.
func TestCouldWritePiOMPAndCommand(t *testing.T) {
	for _, tc := range []struct {
		adapter, kind string
		want          bool
	}{
		{"pi", "pi", true},
		{"pi", "claude-code", false},
		{"omp", "pi", false},
		{"crush", "pi", false},
		{"command", "claude-code", true},
		{"command", "pi", true},
		{"generic", "pi", true},
	} {
		if got := CouldWrite(tc.adapter, tc.kind); got != tc.want {
			t.Errorf("CouldWrite(%s, %s) = %v, want %v", tc.adapter, tc.kind, got, tc.want)
		}
	}
}
