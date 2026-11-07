package relay

// client_test.go is the contract test SPEC-0019 asks for: one fake Switchboard
// run through both schema generations, so a drift in the drain-verb arguments
// fails CI instead of quietly degrading a live attempt.
//
// Governing: SPEC-0019 REQ-17, REQ-18, REQ-19, REQ-24.
//
// @joestump-agent 10/29/2026 - Introduced with the relay lease client
// (stump.wtf/harness#490).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/log/v2"
)

// claimPayload is what a server with attempt history answers claim_next with.
func claimPayload(token string) map[string]any {
	return map[string]any{
		"empty":       false,
		"attempt_seq": 3,
		"lease_token": token,
		"todo": map[string]any{
			"id":            "td_1",
			"queue":         "lane-m",
			"state":         "claimed",
			"title":         "handoff: example",
			"attempt":       3,
			"dead_letter":   false,
			"next_retry_at": nil,
		},
		"prior_attempts": []map[string]any{
			{"attempt_seq": 1, "state": "failed", "claimant": "lane-l", "summary": "looping: tools/list"},
		},
	}
}

func newTestClient(t *testing.T, f *FakeSwitchboard, headers map[string]string, logs *bytes.Buffer) *Client {
	t.Helper()
	logger := log.New(logs)
	logger.SetLevel(log.InfoLevel)
	return New(Options{
		Name:    "switchboard",
		URL:     f.URL(),
		Headers: headers,
		Log:     logger,
	})
}

// REQ-17: the lease session is a caller. A GET on the MCP URL would make Harness
// a doorbell target, which is the channel listener's job and never this one's.
func TestLeaseSessionIsPOSTOnly(t *testing.T) {
	f := newFake(t, true)
	var logs bytes.Buffer
	c := newTestClient(t, f, nil, &logs)
	ctx := context.Background()

	if err := c.Initialize(ctx); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	claimed, err := c.ClaimNext(ctx, ClaimOptions{Claimant: "handoff-m"})
	if err != nil {
		t.Fatalf("claim_next: %v", err)
	}
	if _, err := c.Heartbeat(ctx, claimed.Todo.ID, claimed.LeaseToken, 3600); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if err := c.Complete(ctx, CloseOptions{ID: claimed.Todo.ID, LeaseToken: claimed.LeaseToken, Summary: "done"}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	f.assertNoGET(t)
	if _, calls := countCalls(f); calls != 3 {
		t.Fatalf("served %d tools/call, want 3", calls)
	}
	if f.initCount() != 1 {
		t.Fatalf("initialized %d times, want 1 for one session", f.initCount())
	}
}

// REQ-17: the initialize must not offer claude/channel; advertising it is what
// makes a server treat the session as a notification target.
func TestInitializeDoesNotAdvertiseChannelCapability(t *testing.T) {
	f := newFake(t, true)
	var logs bytes.Buffer
	c := newTestClient(t, f, nil, &logs)
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	f.mu.Lock()
	params := f.lastInitParams
	f.mu.Unlock()
	caps, _ := params["capabilities"].(map[string]any)
	if exp, ok := caps["experimental"]; ok {
		t.Fatalf("initialize advertised capabilities.experimental %#v; REQ-17 forbids offering claude/channel", exp)
	}
	if info, _ := params["clientInfo"].(map[string]any); info["name"] != "harness" {
		t.Fatalf("clientInfo = %#v, want the daemon's own name", info)
	}
}

// REQ-19: a server whose schemas predate attempt history is marked unsupported,
// its missing arguments are named, and claim_next is refused with nothing sent.
func TestPreAttemptHistorySourceIsRefused(t *testing.T) {
	f := newFake(t, false)
	var logs bytes.Buffer
	c := newTestClient(t, f, nil, &logs)
	ctx := context.Background()

	err := c.Initialize(ctx)
	if !errors.Is(err, ErrLeaseSourceUnsupported) {
		t.Fatalf("initialize returned %v, want ErrLeaseSourceUnsupported", err)
	}
	if c.Supported() {
		t.Fatal("an unsupported source reported itself supported")
	}
	want := []string{
		"claim_next.claimant", "claim_next.require_fence",
		"complete.artifact", "complete.lease_token", "complete.summary",
		"fail.artifact", "fail.lease_token", "fail.summary",
		"heartbeat.lease_token",
		"release.lease_token", "release.summary",
	}
	missing := strings.Join(c.MissingArguments(), " ")
	for _, pair := range want {
		if !strings.Contains(missing, pair) {
			t.Errorf("missing list %q does not name %s", missing, pair)
		}
	}

	if _, err := c.ClaimNext(ctx, ClaimOptions{Claimant: "handoff-m"}); !errors.Is(err, ErrLeaseSourceUnsupported) {
		t.Fatalf("claim_next returned %v, want ErrLeaseSourceUnsupported", err)
	}
	if calls := f.toolsCalls(); len(calls) != 0 {
		t.Fatalf("an unsupported source received %d tools/call: %v; REQ-19 allows none", len(calls), calls)
	}
	// The refusal is one ERROR per initialize, and it names what is missing, so
	// an operator can act on the log line alone.
	if !strings.Contains(logs.String(), "claim_next.require_fence") || !strings.Contains(logs.String(), "ERROR") {
		t.Fatalf("log does not carry one ERROR naming the missing arguments:\n%s", logs.String())
	}
	if got := strings.Count(logs.String(), "lease source does not support attempt history"); got != 1 {
		t.Fatalf("logged the unsupported ERROR %d times for one initialize, want 1", got)
	}
}

// REQ-19 "The server is upgraded": re-initializing against upgraded schemas
// clears the state, and the next claim is fenced.
func TestUpgradedServerClearsUnsupported(t *testing.T) {
	f := newFake(t, false)
	var logs bytes.Buffer
	c := newTestClient(t, f, nil, &logs)
	ctx := context.Background()

	if err := c.Initialize(ctx); !errors.Is(err, ErrLeaseSourceUnsupported) {
		t.Fatalf("initialize: %v", err)
	}
	f.setResult("claim_next", claimPayload("lt-fencedtoken123"))
	f.setAttemptHistory(true)
	if err := c.Initialize(ctx); err != nil {
		t.Fatalf("re-initialize after upgrade: %v", err)
	}
	if !c.Supported() {
		t.Fatal("the upgraded source is still marked unsupported")
	}
	if len(c.MissingArguments()) != 0 {
		t.Fatalf("missing list %v after the upgrade, want empty", c.MissingArguments())
	}
	if _, err := c.ClaimNext(ctx, ClaimOptions{Queue: "lane-m", Claimant: "handoff-m"}); err != nil {
		t.Fatalf("claim_next after upgrade: %v", err)
	}
	calls := f.callsFor(VerbClaimNext)
	if len(calls) != 1 {
		t.Fatalf("served %d claim_next, want 1", len(calls))
	}
	if fenced, _ := calls[0].Args["require_fence"].(bool); !fenced {
		t.Fatalf("claim_next after the upgrade carried require_fence=%v; an unfenced claim is never sent", calls[0].Args["require_fence"])
	}
	if f.initCount() != 2 {
		t.Fatalf("initialized %d times, want 2", f.initCount())
	}
}

// REQ-19 "Schema declares the fence": the token the claim returned rides every
// later verb for the attempt.
func TestEveryHeartbeatCarriesTheLeaseToken(t *testing.T) {
	f := newFake(t, true)
	var logs bytes.Buffer
	c := newTestClient(t, f, nil, &logs)
	ctx := context.Background()

	claimed, err := c.ClaimNext(ctx, ClaimOptions{Claimant: "handoff-m"})
	if err != nil {
		t.Fatalf("claim_next: %v", err)
	}
	if claimed.LeaseToken != defaultLeaseToken {
		t.Fatalf("claim returned token %q, want the one the fake issued", claimed.LeaseToken)
	}
	if claimed.AttemptSeq != 3 || len(claimed.PriorAttempts) != 1 || claimed.PriorAttempts[0].Summary == "" {
		t.Fatalf("claim result did not read attempt_seq/prior_attempts: %+v", claimed)
	}
	if claimed.Todo == nil || claimed.Todo.ID != "td_1" || claimed.Todo.DeadLetter {
		t.Fatalf("claim result did not read todo.*: %+v", claimed.Todo)
	}

	for i := 0; i < 2; i++ {
		if _, err := c.Heartbeat(ctx, claimed.Todo.ID, claimed.LeaseToken, 1800); err != nil {
			t.Fatalf("heartbeat %d: %v", i, err)
		}
	}
	calls := f.callsFor(VerbHeartbeat)
	if len(calls) != 2 {
		t.Fatalf("served %d heartbeats, want 2", len(calls))
	}
	for i, call := range calls {
		if got, _ := call.Args["lease_token"].(string); got != claimed.LeaseToken {
			t.Fatalf("heartbeat %d carried lease_token %q, want the claim's token", i, got)
		}
	}

	// A heartbeat built without a token is refused client-side: an unfenced
	// extension would be indistinguishable from a lost lease.
	if _, err := c.Heartbeat(ctx, claimed.Todo.ID, "", 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("heartbeat without a token returned %v, want ErrInvalid", err)
	}
	if len(f.callsFor(VerbHeartbeat)) != 2 {
		t.Fatal("a tokenless heartbeat still reached the server")
	}
}

// REQ-24: `conflict` is its own sentinel. It must not look like a transport
// failure, because a retry loop would keep hammering a source that already took
// the todo away.
func TestConflictIsNotATransportError(t *testing.T) {
	f := newFake(t, true)
	var logs bytes.Buffer
	c := newTestClient(t, f, nil, &logs)
	ctx := context.Background()

	claimed, err := c.ClaimNext(ctx, ClaimOptions{})
	if err != nil {
		t.Fatalf("claim_next: %v", err)
	}
	f.setFailure(VerbHeartbeat, &ToolFailure{Code: "conflict", Message: "the lease is held by another attempt"})
	_, err = c.Heartbeat(ctx, claimed.Todo.ID, claimed.LeaseToken, 0)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("heartbeat returned %v, want ErrConflict", err)
	}
	if errors.Is(err, ErrTransport) || errors.Is(err, ErrProtocol) || errors.Is(err, ErrSessionExpired) {
		t.Fatalf("a conflict was reported as a transport failure: %v", err)
	}
	if calls := f.callsFor(VerbHeartbeat); len(calls) != 1 {
		t.Fatalf("a conflict was retried %d times; REQ-24 branches instead of retrying", len(calls))
	}

	for _, tc := range []struct {
		code   string
		wanted error
	}{{"not_found", ErrNotFound}, {"forbidden", ErrForbidden}, {"invalid", ErrInvalid}} {
		f.setFailure(VerbComplete, &ToolFailure{Code: tc.code, Message: "scripted " + tc.code})
		err := c.Complete(ctx, CloseOptions{ID: "td_1", LeaseToken: claimed.LeaseToken})
		if !errors.Is(err, tc.wanted) {
			t.Errorf("%s mapped to %v, want %v", tc.code, err, tc.wanted)
		}
	}

	// The JSON-RPC error shape carries the same code, and maps the same way.
	f.setFailure(VerbFail, &ToolFailure{Code: "not_found", Message: "no such todo", AsRPCError: true})
	if err := c.Fail(ctx, CloseOptions{ID: "td_gone", LeaseToken: claimed.LeaseToken}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a JSON-RPC error mapped to %v, want ErrNotFound", err)
	}
}

// REQ-18: neither the endpoint credential nor a lease token may come back in an
// error string or a log line, even when the server itself echoes them.
func TestCredentialsStayOutOfErrorsAndLogs(t *testing.T) {
	const (
		headerValue = "Bearer lease-header-secret-value"
		leaseToken  = "lt-secret-token-value-0123456789"
	)
	f := newFake(t, true)
	var logs bytes.Buffer
	c := newTestClient(t, f, map[string]string{"Authorization": headerValue}, &logs)
	ctx := context.Background()

	f.setResult("claim_next", claimPayload(leaseToken))
	claimed, err := c.ClaimNext(ctx, ClaimOptions{Claimant: "handoff-m"})
	if err != nil {
		t.Fatalf("claim_next: %v", err)
	}
	f.setFailure(VerbHeartbeat, &ToolFailure{
		Code:    "invalid",
		Message: "rejected " + leaseToken + " sent as " + headerValue,
	})
	_, err = c.Heartbeat(ctx, claimed.Todo.ID, claimed.LeaseToken, 0)
	if err == nil {
		t.Fatal("the scripted failure did not reach the caller")
	}
	for _, secret := range []string{leaseToken, headerValue} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("the error repeats a credential: %s", err)
		}
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("a log line repeats a credential:\n%s", logs.String())
		}
	}
	if claimed.LeaseToken != leaseToken {
		t.Fatal("the lease token must stay available in memory for the attempt that holds it")
	}
	// The check can only fail if redaction ran, so prove it fired rather than
	// trusting that the server's text happened to be clean.
	if !strings.Contains(err.Error(), redaction) {
		t.Fatalf("the server's text reached the error unredacted: %s", err)
	}
}

// REQ-19: every verb sends the arguments the contract names, and summary and
// artifact stay out of `result`.
func TestDrainVerbsSendTheEnumeratedArguments(t *testing.T) {
	f := newFake(t, true)
	var logs bytes.Buffer
	c := newTestClient(t, f, nil, &logs)
	ctx := context.Background()

	if _, err := c.ClaimNext(ctx, ClaimOptions{Queue: "lane-m", Claimant: "handoff-m", LeaseTTLSeconds: 7200}); err != nil {
		t.Fatalf("claim_next: %v", err)
	}
	claimed, _ := c.ClaimNext(ctx, ClaimOptions{})
	token := claimed.LeaseToken
	if _, err := c.Heartbeat(ctx, "td_1", token, 1800); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	closeArgs := CloseOptions{
		ID:         "td_1",
		LeaseToken: token,
		Summary:    "opened a PR",
		Artifact:   "https://gitea.stump.rocks/stump.wtf/harness/pulls/1",
		Result:     map[string]any{"pr": "https://gitea.stump.rocks/stump.wtf/harness/pulls/1"},
	}
	if err := c.Complete(ctx, closeArgs); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if err := c.Fail(ctx, closeArgs); err != nil {
		t.Fatalf("fail: %v", err)
	}
	if err := c.Release(ctx, ReleaseOptions{ID: "td_1", LeaseToken: token, Summary: "shutting down"}); err != nil {
		t.Fatalf("release: %v", err)
	}

	for _, verb := range []string{VerbHeartbeat, VerbComplete, VerbFail, VerbRelease} {
		calls := f.callsFor(verb)
		if len(calls) == 0 {
			t.Fatalf("%s was never sent", verb)
		}
		if got := argString(calls[len(calls)-1], "lease_token"); got != token {
			t.Errorf("%s carried lease_token %q, want the claim's token", verb, got)
		}
	}
	// The first claim carries the caller's queue and claimant; the fenced claim
	// is the one the contract is about, so check both on that call.
	claim := f.callsFor(VerbClaimNext)[0]
	if argString(claim, "claimant") != "handoff-m" || argString(claim, "queue") != "lane-m" {
		t.Errorf("claim_next arguments lost queue/claimant: %v", claim.Args)
	}
	if fenced, _ := claim.Args["require_fence"].(bool); !fenced {
		t.Errorf("claim_next carried require_fence=%v, want true", claim.Args["require_fence"])
	}
	for _, verb := range []string{VerbComplete, VerbFail} {
		calls := f.callsFor(verb)
		last := calls[len(calls)-1]
		if argString(last, "summary") == "" || argString(last, "artifact") == "" {
			t.Errorf("%s did not send summary/artifact as arguments: %v", verb, last.Args)
		}
		if insideResultHasCloseFields(last.Args["result"]) {
			t.Errorf("%s put summary/artifact inside result: %v", verb, last.Args["result"])
		}
	}
	release := f.callsFor(VerbRelease)[0]
	if argString(release, "summary") != "shutting down" {
		t.Errorf("release did not send its summary: %v", release.Args)
	}
	if _, ok := release.Args["artifact"]; ok {
		t.Errorf("release sent an artifact, which REQ-19 does not enumerate for that verb: %v", release.Args)
	}
}

// REQ-14/REQ-19: release is a grant. Without it the client says so and sends
// nothing, rather than trying a verb the endpoint will reject.
func TestReleaseAbsentFromTheGrantSendsNothing(t *testing.T) {
	f := newFake(t, true)
	f.setAdvertiseRelease(false)
	var logs bytes.Buffer
	c := newTestClient(t, f, nil, &logs)
	ctx := context.Background()

	if err := c.Initialize(ctx); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if c.ReleaseAdvertised() {
		t.Fatal("ReleaseAdvertised is true against a fake that never registered release")
	}
	if err := c.Release(ctx, ReleaseOptions{ID: "td_1", LeaseToken: defaultLeaseToken}); !errors.Is(err, ErrReleaseNotAdvertised) {
		t.Fatalf("release returned %v, want ErrReleaseNotAdvertised", err)
	}
	if calls := f.callsFor(VerbRelease); len(calls) != 0 {
		t.Fatalf("release reached the server %d times without being advertised", len(calls))
	}
}

// REQ-17: a server that expires the session is re-initialized and the verb is
// replayed, so an attempt does not die on an idle lease source.
func TestSessionExpiryReinitializesAndReplays(t *testing.T) {
	f := newFake(t, true)
	var logs bytes.Buffer
	c := newTestClient(t, f, nil, &logs)
	ctx := context.Background()

	if err := c.Initialize(ctx); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	before := f.initCount()
	f.expireSession()
	if _, err := c.Heartbeat(ctx, "td_1", defaultLeaseToken, 0); err != nil {
		t.Fatalf("heartbeat across an expired session: %v", err)
	}
	if got := f.initCount(); got != before+1 {
		t.Fatalf("initialized %d times, want one re-initialize", got)
	}
	if calls := f.callsFor(VerbHeartbeat); len(calls) != 1 {
		t.Fatalf("served %d heartbeats, want the replay only", len(calls))
	}
}

// Streamable HTTP lets a server answer a POST with an event stream; the reply
// must still be found.
func TestEventStreamAnswerDecodes(t *testing.T) {
	f := newFake(t, true)
	f.setStreamAnswers(true)
	var logs bytes.Buffer
	c := newTestClient(t, f, nil, &logs)
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("initialize over an event stream: %v", err)
	}
	claimed, err := c.ClaimNext(context.Background(), ClaimOptions{Claimant: "handoff-m"})
	if err != nil {
		t.Fatalf("claim_next over an event stream: %v", err)
	}
	if claimed.Todo == nil || claimed.Todo.Queue != "lane-m" {
		t.Fatalf("event-stream claim did not decode: %+v", claimed)
	}
	f.assertNoGET(t)
}

// The client documents itself as safe for concurrent use, and a server may
// answer a POST with an event stream. A reply matched by "whatever id was
// issued last" hands one caller another caller's reply, so the regression pins
// per-request correlation under a fan-out.
func TestConcurrentCallsMatchTheirOwnReplies(t *testing.T) {
	f := newFake(t, true)
	f.setStreamAnswers(true)
	f.setCallJitter(2 * time.Millisecond)
	var logs bytes.Buffer
	c := newTestClient(t, f, nil, &logs)
	ctx := context.Background()
	if err := c.Initialize(ctx); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	const callers, rounds = 8, 3
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, callers*rounds)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			<-start
			for r := 0; r < rounds; r++ {
				_, err := c.ClaimNext(ctx, ClaimOptions{Claimant: "fan-out"})
				errs[slot*rounds+r] = err
			}
		}(i)
	}
	close(start)
	wg.Wait()
	for slot, err := range errs {
		if err != nil {
			t.Fatalf("concurrent claim %d failed: %v", slot, err)
		}
	}
}

// The contract runs against one fake with both schema generations: the current
// one works end to end, the one that predates attempt history is refused. Drift
// in either direction fails here rather than in a live attempt.
func TestContractAcrossBothSchemaGenerations(t *testing.T) {
	f := newFake(t, true)
	var logs bytes.Buffer
	c := newTestClient(t, f, nil, &logs)
	ctx := context.Background()

	type step struct {
		name string
		run  func() error
	}
	steps := []step{
		{"claim_next", func() error {
			_, err := c.ClaimNext(ctx, ClaimOptions{Claimant: "contract"})
			return err
		}},
		{"heartbeat", func() error {
			_, err := c.Heartbeat(ctx, "td_1", defaultLeaseToken, 0)
			return err
		}},
		{"complete", func() error {
			return c.Complete(ctx, CloseOptions{ID: "td_1", LeaseToken: defaultLeaseToken, Summary: "ok"})
		}},
		{"fail", func() error {
			return c.Fail(ctx, CloseOptions{ID: "td_1", LeaseToken: defaultLeaseToken, Summary: "nope"})
		}},
		{"release", func() error {
			return c.Release(ctx, ReleaseOptions{ID: "td_1", LeaseToken: defaultLeaseToken})
		}},
	}

	for _, generation := range []struct {
		name    string
		attempt bool
	}{{"attempt-history", true}, {"pre-attempt-history", false}} {
		f.setAttemptHistory(generation.attempt)
		err := c.Initialize(ctx)
		if generation.attempt {
			if err != nil {
				t.Fatalf("%s: initialize: %v", generation.name, err)
			}
			for _, s := range steps {
				if err := s.run(); err != nil {
					t.Errorf("%s: %s: %v", generation.name, s.name, err)
				}
			}
			continue
		}
		if !errors.Is(err, ErrLeaseSourceUnsupported) {
			t.Fatalf("%s: initialize returned %v, want refusal", generation.name, err)
		}
		for _, s := range steps {
			if err := s.run(); !errors.Is(err, ErrLeaseSourceUnsupported) {
				t.Errorf("%s: %s returned %v, want ErrLeaseSourceUnsupported", generation.name, s.name, err)
			}
		}
	}
	f.assertNoGET(t)
}

// countCalls reports GETs and the number of tools/call served.
func countCalls(f *FakeSwitchboard) (gets, calls int) {
	gets, _, calls = f.counts()
	return gets, calls
}

func argString(call RecordedCall, key string) string {
	v, ok := call.Args[key]
	if !ok {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(raw)
}

// insideResultHasCloseFields reports whether a result payload carries a summary
// or an artifact, which is the misuse REQ-19 forbids: those two have their own
// arguments, and burying them in result is how an attempt's note would be lost.
func insideResultHasCloseFields(result any) bool {
	raw, err := json.Marshal(result)
	if err != nil {
		return false
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return false
	}
	_, hasSummary := probe["summary"]
	_, hasArtifact := probe["artifact"]
	return hasSummary || hasArtifact
}
