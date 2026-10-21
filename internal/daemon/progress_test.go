package daemon

// Last Call And Children On The Wire
//
// Issue #958 is the operator's report that a harness blocked for hours inside
// one tool call still reads `state: running`, because state is truthful about
// the PROCESS and the call is stuck elsewhere. Two fields make the wedge
// readable from the control socket: last_successful_call_at, the instant
// /metrics exports as harness_last_successful_call_timestamp (ADR-0020), and
// children, the supervised process's direct children with their age and CPU.
//
// The assertions are on the bytes the daemon writes, like the hold projection
// test: the absence of a field is half of the contract, and a decoded struct
// cannot show that a key was never written. A harness the observer never
// reported on carries no timestamp rather than a zero an operator would read
// as 1970; a harness that is not running carries no children rather than an
// empty list claiming none exist. The wedged-child case is a real `sh` with a
// real long-lived child of its own, so the ps columns the snapshot reads are
// exercised rather than imagined.
//
// Governing: ADR-0048 (proposed), ADR-0020; SPEC-0002 REQ "Control
// Operations"; issue #958.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/protocol"
)

const progressTOML = `
[harness.wedged]
harness = "generic"
args = ["-c", "sleep 300 & wait"]

[harness.quiet]
harness = "generic"
args = ["-c", "sleep 300"]
`

// fakeProgress is the watchdog's progress clock as the server sees it: the
// daemon wires the real guard with SetProgress, after the server is already
// serving.
type fakeProgress struct {
	at map[string]time.Time
}

func (p *fakeProgress) LastCall(name string) (time.Time, bool) {
	at, ok := p.at[name]
	return at, ok
}

func childOf(t *testing.T, info protocol.HarnessInfo) (protocol.ChildProcess, bool) {
	t.Helper()
	for _, kid := range info.Children {
		if kid.PID > 0 && kid.PID != info.PID {
			return kid, true
		}
	}
	return protocol.ChildProcess{}, false
}

func TestLastCallAndChildrenRideListAndDescribe(t *testing.T) {
	stalled := time.Now().Add(-23 * time.Hour).Truncate(time.Second)
	td := newTestDaemon(t, progressTOML, func(o *Options) {
		o.ChildrenInterval = time.Hour
	})
	td.srv.SetProgress(&fakeProgress{at: map[string]time.Time{"wedged": stalled}})

	td.mgr.Start("wedged")
	waitFor(t, "wedged running", func() bool {
		s, _ := td.mgr.Snapshot("wedged")
		return s.State == core.StateRunning && s.PID > 0
	})
	// The sample is what the surfaces read; take it now so the child the
	// harness spawned is in it.
	td.srv.procs.refresh()

	c := td.dial(t, nil)
	raw := rawControl(t, c, protocol.ControlReq{Op: protocol.OpDescribe, Name: "wedged"})
	var info protocol.HarnessInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		t.Fatalf("decode describe: %v (%s)", err, raw)
	}

	at, err := time.Parse(time.RFC3339Nano, info.LastSuccessfulCallAt)
	if err != nil {
		t.Fatalf("describe last_successful_call_at = %q: %v", info.LastSuccessfulCallAt, err)
	}
	if !at.Equal(stalled) {
		t.Errorf("describe last_successful_call_at = %v, want %v", at, stalled)
	}

	kid, ok := childOf(t, info)
	if !ok {
		t.Fatalf("describe carried no child of pid %d: %+v", info.PID, info.Children)
	}
	if kid.Command == "" {
		t.Errorf("child %+v names no command", kid)
	}
	started, err := time.Parse(time.RFC3339Nano, kid.StartedAt)
	if err != nil {
		t.Fatalf("child started_at = %q: %v", kid.StartedAt, err)
	}
	if started.Before(time.Now().Add(-5*time.Minute)) || started.After(time.Now()) {
		t.Errorf("child started_at = %v, want a moment ago", started)
	}
	// The wedged-child reading the issue asks for: this child has been alive
	// since the harness started and has burned almost nothing.
	if kid.CPUMs > 500 {
		t.Errorf("child cpu_ms = %d for a child up %s, want near zero", kid.CPUMs, time.Since(started))
	}

	var listed []protocol.HarnessInfo
	if err := json.Unmarshal(rawControl(t, c, protocol.ControlReq{Op: protocol.OpList}), &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	seen := map[string]protocol.HarnessInfo{}
	for _, h := range listed {
		seen[h.Name] = h
	}
	if got := seen["wedged"].LastSuccessfulCallAt; got != info.LastSuccessfulCallAt {
		t.Errorf("list last_successful_call_at = %q, want describe's %q", got, info.LastSuccessfulCallAt)
	}
	if _, ok := childOf(t, seen["wedged"]); !ok {
		t.Errorf("list carried no child of pid %d: %+v", seen["wedged"].PID, seen["wedged"].Children)
	}

	// A harness the observer has never reported on says so by saying nothing:
	// a zero here would read as a call 56 years ago.
	quiet := fieldsOf(t, rawControl(t, c, protocol.ControlReq{Op: protocol.OpDescribe, Name: "quiet"}))
	if v, ok := quiet["last_successful_call_at"]; ok {
		t.Errorf("a harness with no observed call carries last_successful_call_at: %s", v)
	}
}

// TestStoppedHarnessNamesNoChildren: the snapshot's pid is the last one the
// harness had, so inspecting it after the process is gone would name whoever
// owns that pid now. Absent means unknown, which is the truth.
func TestStoppedHarnessNamesNoChildren(t *testing.T) {
	stalled := time.Now().Add(-time.Hour).Truncate(time.Second)
	td := newTestDaemon(t, progressTOML, func(o *Options) {
		o.ChildrenInterval = time.Hour
	})
	td.srv.SetProgress(&fakeProgress{at: map[string]time.Time{"wedged": stalled}})

	td.mgr.Start("wedged")
	waitFor(t, "running", func() bool {
		s, _ := td.mgr.Snapshot("wedged")
		return s.State == core.StateRunning
	})
	td.srv.procs.refresh()
	c := td.dial(t, nil)
	running := fieldsOf(t, rawControl(t, c, protocol.ControlReq{Op: protocol.OpDescribe, Name: "wedged"}))
	if _, ok := running["children"]; !ok {
		t.Fatal("a running harness carries no children")
	}

	td.mgr.Stop("wedged")
	waitFor(t, "stopped", func() bool {
		s, _ := td.mgr.Snapshot("wedged")
		return s.State == core.StateStopped
	})
	stopped := fieldsOf(t, rawControl(t, c, protocol.ControlReq{Op: protocol.OpDescribe, Name: "wedged"}))
	if v, ok := stopped["children"]; ok {
		t.Errorf("a stopped harness names children of a pid it no longer owns: %s", v)
	}
	// Progress is the harness's, not the process's: it survives the stop, so
	// "it stopped making calls an hour ago, then exited" stays readable.
	if v, ok := stopped["last_successful_call_at"]; !ok {
		t.Errorf("a stopped harness lost last_successful_call_at (have %v)", v)
	} else if string(v) == `""` {
		t.Error("a stopped harness carries an empty last_successful_call_at")
	}
}

// TestServerWithoutProgressSourceLeavesTheFieldOff: a server built without a
// progress source must not guess one.
func TestServerWithoutProgressSourceLeavesTheFieldOff(t *testing.T) {
	td := newTestDaemon(t, progressTOML, func(o *Options) {
		o.ChildrenInterval = time.Hour
	})
	td.mgr.Start("quiet")
	waitFor(t, "running", func() bool {
		s, _ := td.mgr.Snapshot("quiet")
		return s.State == core.StateRunning
	})
	c := td.dial(t, nil)
	fields := fieldsOf(t, rawControl(t, c, protocol.ControlReq{Op: protocol.OpDescribe, Name: "quiet"}))
	if v, ok := fields["last_successful_call_at"]; ok {
		t.Errorf("a daemon with no progress source carried %s", v)
	}
}
