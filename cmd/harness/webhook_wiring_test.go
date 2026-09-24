package main

// The webhook-listener wiring tests.
//
// They drive the pieces THE DAEMON builds — startDaemonSources,
// beginDaemonWebhooks/serve, wireSourceReload/wireWebhookReload, and the
// Manager daemonManagerOptions builds — and follow one bearer delivery over a
// real TCP connection to a finished run record with trigger `webhook` and a
// `0600` event file. A test that assembled any of those itself would pass
// against a daemon that wired none of them (#315).
//
// The binary-level test proves the rest: that runDaemon itself opens no port
// unless asked, and that HARNESS_WEBHOOK_LISTEN reaches the listener.
//
// Governing: ADR-0021; SPEC-0014 REQ "Webhook Listener", REQ "Webhook
// Verification", REQ "Firing", REQ "Event Delivery To The Run", REQ "Source
// Reconciliation On Reload"; SPEC-0010 REQ "Precedence Order".
//
// @joestump 09/23/2026 - Introduced with the SPEC-0014 webhook listener (#458).

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/attach"
	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/supervisor"
	"github.com/stump-wtf/harness/internal/trigger"
)

const wiringToken = "wiring-Chai8aeY-token"

// webhookWiringConfig is one bearer source bound by one harness that records
// the event file path it was handed.
func webhookWiringConfig(fields string, enabled bool) *core.Config {
	h := core.Harness{
		Name: "ci-run", Adapter: "generic", Backend: core.BackendNative,
		Args:    []string{"-c", `printf 'F:EVENT=[%s]\n' "${HARNESS_EVENT_FILE-unset}" | tee '` + fields + `'`},
		Restart: core.RestartNo,

		Triggers:  []string{"webhook.ci"},
		Timeout:   core.DefaultRunTimeout,
		OnOverlap: core.OverlapQueue,
		KeepRuns:  core.DefaultKeepRuns,
	}
	return &core.Config{
		Harnesses:    map[string]core.Harness{h.Name: h},
		Profiles:     map[string]core.Profile{},
		HarnessOrder: []string{h.Name},
		Webhooks: map[string]core.WebhookSource{"ci": {
			Name: "ci", Verify: core.VerifyBearer, Secret: core.Secret(wiringToken),
			DeliveryHeader: "X-Delivery",
			MaxBody:        core.DefaultWebhookMaxBody, RateLimit: core.DefaultWebhookRateLimit, Enabled: enabled,
		}},
		WebhookOrder: []string{"ci"},
	}
}

func newWiringManager(t *testing.T, cfg *core.Config) *supervisor.Manager {
	t.Helper()
	tmp := t.TempDir()
	reg := attach.NewRegistry(100)
	opts := daemonManagerOptions(reg)
	opts.StatePath = filepath.Join(tmp, "state.json")
	opts.LogDir = filepath.Join(tmp, "logs")
	opts.JobsDir = filepath.Join(tmp, "jobs")
	opts.Policy.StopGrace = 200 * time.Millisecond
	mgr := supervisor.NewManager(cfg, opts)
	t.Cleanup(mgr.Close)
	reg.SetController(mgr)
	if err := mgr.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	return mgr
}

func postHook(t *testing.T, url, auth, delivery, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest("POST", url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if delivery != "" {
		req.Header.Set("X-Delivery", delivery)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func TestDaemonWiringFiresAHarnessFromABearerWebhook(t *testing.T) {
	fields := filepath.Join(t.TempDir(), "fields")
	cfg := webhookWiringConfig(fields, true)
	mgr := newWiringManager(t, cfg)

	sources := startDaemonSources(mgr)
	t.Cleanup(sources.Close)
	wireSourceReload(mgr, sources)
	webhooks := beginDaemonWebhooks(mgr, sources, "127.0.0.1:0")
	wireWebhookReload(mgr, webhooks)
	webhooks.serve()
	t.Cleanup(webhooks.shutdown)

	addr := webhooks.addr()
	if addr == "" {
		t.Fatal("the daemon's webhook listener did not bind")
	}
	if st, _ := sources.StatusOf("webhook.ci"); st.State != trigger.StateListening {
		t.Errorf("webhook.ci state = %q, want listening", st.State)
	}
	url := "http://" + addr + "/hooks/ci"

	// A wrong token fires nothing.
	if code, _ := postHook(t, url, "Bearer nope", "", `{}`); code != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d, want 401", code)
	}
	if runs := mgr.Runs("ci-run"); len(runs) != 0 {
		t.Fatalf("an unauthenticated delivery made a run: %+v", runs)
	}

	const sentinel = "build 42 failed Iex4ohng"
	code, body := postHook(t, url, "Bearer "+wiringToken, "del-7", `{"msg":"`+sentinel+`"}`)
	if code != http.StatusAccepted {
		t.Fatalf("bearer delivery: %d %s", code, body)
	}
	var resp struct {
		Webhook  string `json:"webhook"`
		EventID  string `json:"event_id"`
		Decision string `json:"decision"`
		Firings  []struct {
			Harness  string `json:"harness"`
			Decision string `json:"decision"`
			RunID    *int   `json:"run_id"`
		} `json:"firings"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("202 body: %v\n%s", err, body)
	}
	if resp.Webhook != "ci" || resp.EventID != "del-7" || resp.Decision != "fired" ||
		len(resp.Firings) != 1 || resp.Firings[0].Harness != "ci-run" || resp.Firings[0].Decision != "started" || resp.Firings[0].RunID == nil {
		t.Fatalf("202 body = %s", body)
	}

	var rec supervisor.RunRecord
	deadline := time.Now().Add(15 * time.Second)
	for {
		runs := mgr.Runs("ci-run")
		if len(runs) == 1 && runs[0].Outcome != supervisor.OutcomeRunning {
			rec = runs[0]
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the delivery never produced a finished run: %+v", runs)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rec.RunID != *resp.Firings[0].RunID {
		t.Errorf("run id %d, the 202 said %d", rec.RunID, *resp.Firings[0].RunID)
	}
	if rec.Outcome != supervisor.OutcomeSuccess {
		t.Errorf("run outcome = %s", rec.Outcome)
	}
	if rec.Trigger != supervisor.TriggerWebhook || rec.Source != "webhook.ci" || rec.EventID != "del-7" {
		t.Errorf("record = %+v, want trigger webhook, source webhook.ci, event_id del-7", rec)
	}

	eventFile := strings.TrimSuffix(mgr.RunLogPath("ci-run", rec.RunID), ".log") + ".event.json"
	info, err := os.Stat(eventFile)
	if err != nil {
		t.Fatalf("no event file at %s: %v", eventFile, err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("event file mode = %o, want 600", perm)
	}
	raw, err := os.ReadFile(eventFile)
	if err != nil {
		t.Fatal(err)
	}
	env, err := trigger.ParseEnvelope(raw, 0)
	if err != nil {
		t.Fatalf("event file does not parse: %v", err)
	}
	if env.Webhook == nil || !strings.Contains(string(env.Webhook.Body), sentinel) {
		t.Errorf("event file = %s, want the body verbatim", raw)
	}
	if strings.Contains(string(raw), wiringToken) {
		t.Error("the event file carries the bearer token")
	}
	got, err := os.ReadFile(fields)
	if err != nil {
		t.Fatalf("the spawned process wrote no fields file: %v", err)
	}
	if want := "F:EVENT=[" + eventFile + "]"; !strings.Contains(string(got), want) {
		t.Errorf("the process did not receive HARNESS_EVENT_FILE:\nwant %q\ngot  %q", want, got)
	}
	runLog, _ := os.ReadFile(mgr.RunLogPath("ci-run", rec.RunID))
	if strings.Contains(string(runLog), sentinel) {
		t.Error("the delivery's body reached the run's log")
	}

	// The reload hook is composed, not replaced: a reload that disables the
	// source takes the route down, and the source reports it.
	mgr.Reload(webhookWiringConfig(fields, false))
	if code, _ := postHook(t, url, "Bearer "+wiringToken, "del-8", `{}`); code != http.StatusNotFound {
		t.Errorf("after a reload disabled the source: %d, want 404", code)
	}
	if st, _ := sources.StatusOf("webhook.ci"); st.State != trigger.StateDisabled {
		t.Errorf("after reload: webhook.ci state = %q, want disabled", st.State)
	}
}

// TestDaemonWebhookListenerOffWithoutAnAddress: a [webhook.*] source bound by a
// harness opens no port when nothing names an address, and reports
// no_listener. The same wiring given an address does listen, so the check
// can fire.
func TestDaemonWebhookListenerOffWithoutAnAddress(t *testing.T) {
	cfg := webhookWiringConfig(filepath.Join(t.TempDir(), "f"), true)
	mgr := newWiringManager(t, cfg)
	sources := startDaemonSources(mgr)
	t.Cleanup(sources.Close)

	off := beginDaemonWebhooks(mgr, sources, "")
	off.serve()
	t.Cleanup(off.shutdown)
	if off.srv != nil || off.addr() != "" {
		t.Fatalf("a listener was built with no address: %v", off.addr())
	}
	if st, _ := sources.StatusOf("webhook.ci"); st.State != trigger.StateNoListener {
		t.Errorf("webhook.ci state = %q, want no_listener", st.State)
	}

	on := beginDaemonWebhooks(mgr, sources, "127.0.0.1:0")
	on.serve()
	t.Cleanup(on.shutdown)
	c, err := net.DialTimeout("tcp", on.addr(), time.Second)
	if err != nil {
		t.Fatalf("with an address, nothing listens: %v", err)
	}
	_ = c.Close()
}

// TestDaemonBinaryWebhookListenIsOptIn runs the real binary: without an
// address the port it would use refuses connections; with
// HARNESS_WEBHOOK_LISTEN naming that port, /healthz answers and the route is
// served (and authenticated).
func TestDaemonBinaryWebhookListenIsOptIn(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary; skipped under -short")
	}
	bin := buildHarnessBinary(t)
	env, _ := isolatedEnv(t)
	dir := shortSockDir(t)
	if err := os.WriteFile(filepath.Join(dir, "w.env"), []byte("CI_TOKEN="+wiringToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "harness.toml")
	if err := os.WriteFile(cfgPath, []byte(`
[webhook.ci]
verify = "bearer"
env_file = "w.env"
secret = "${CI_TOKEN}"

[harness.ci-run]
harness = "claude-code"
prompt = "look at the event"
triggers = ["webhook.ci"]
`), 0o600); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().String()
	_ = ln.Close()

	start := func(extra ...string) (socket string) {
		socket = filepath.Join(shortSockDir(t), "h.sock")
		d := startDaemonProc(t, bin, append(append([]string{}, env...), extra...),
			"daemon", "run", "--socket", socket, "--config", cfgPath)
		d.waitReady(t, bin, env, socket)
		return socket
	}

	// Off: the daemon is up and serving its socket, and the port is closed.
	start()
	if c, err := net.DialTimeout("tcp", port, time.Second); err == nil {
		_ = c.Close()
		t.Fatalf("with no webhook_listen, something accepts on %s", port)
	}

	// On, via the environment alone.
	start("HARNESS_WEBHOOK_LISTEN=" + port)
	client := &http.Client{Timeout: 2 * time.Second}
	var resp *http.Response
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err = client.Get("http://" + port + "/healthz")
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("HARNESS_WEBHOOK_LISTEN=%s: listener never answered: %v", port, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != "ok" {
		t.Errorf("/healthz = %d %q", resp.StatusCode, b)
	}
	if code, _ := postHook(t, "http://"+port+"/hooks/ci", "", "", `{}`); code != http.StatusUnauthorized {
		t.Errorf("unauthenticated POST /hooks/ci = %d, want 401", code)
	}
}

// TestWebhookListenOverrideComesOnlyFromFlagOrEnv: the daemon carries a flag
// or HARNESS_WEBHOOK_LISTEN as an override, flag first, and never freezes the
// file's value (which internal/config re-reads on every reload).
func TestWebhookListenOverrideComesOnlyFromFlagOrEnv(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "harness.toml")
	if err := os.WriteFile(cfgPath, []byte("[server]\nwebhook_listen = \"127.0.0.1:1111\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARNESS_CONFIG", cfgPath)
	t.Setenv("HARNESS_WEBHOOK_LISTEN", "")
	resolve := func(args ...string) daemonOpts {
		t.Helper()
		g := &globalOpts{}
		cmd := newDaemonCmd(g)
		if err := cmd.ParseFlags(args); err != nil {
			t.Fatal(err)
		}
		d := daemonOpts{}
		if err := resolveDaemonSettings(cmd, g, &d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	if d := resolve(); d.webhookListen != "" {
		t.Errorf("file only: override = %q, want empty", d.webhookListen)
	}
	t.Setenv("HARNESS_WEBHOOK_LISTEN", "127.0.0.1:2222")
	if d := resolve(); d.webhookListen != "127.0.0.1:2222" {
		t.Errorf("env: override = %q", d.webhookListen)
	}
	d := resolve("--webhook-listen", "127.0.0.1:3333")
	if d.webhookListen != "127.0.0.1:3333" {
		t.Errorf("flag over env: override = %q", d.webhookListen)
	}
	if args := strings.Join(d.childArgs(), " "); !strings.Contains(args, "--webhook-listen 127.0.0.1:3333") {
		t.Errorf("--detach child args drop the override: %s", args)
	}
}
