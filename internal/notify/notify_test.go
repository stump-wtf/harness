package notify

// Dispatcher Tests
//
// Governing tests: SPEC-0003 REQ "Operator Notification"; issue #725. Every
// test runs a real hook — a shell script exec'd the way production execs one —
// and reads back what it actually received, so a dispatcher that built the
// right payload and then failed to hand it over cannot pass.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/stump-wtf/harness/internal/core"
)

// recorder is a hook that writes each delivery's HARNESS_NOTIFY_* environment
// and stdin into dir, atomically, one pair of files per run.
const recorder = `#!/bin/sh
dir="$1"
env | grep '^HARNESS_NOTIFY_' > "$dir/$$.env.tmp"
cat > "$dir/$$.json.tmp"
mv "$dir/$$.env.tmp" "$dir/$$.env"
mv "$dir/$$.json.tmp" "$dir/$$.json"
`

// Received is one delivery as the hook saw it.
type Received struct {
	Env     map[string]string
	Payload Payload
	Raw     string
}

// NewRecorder writes the recorder hook into a temp dir and returns its argv
// and the directory deliveries land in.
func NewRecorder(t testing.TB) (argv []string, dir string) {
	t.Helper()
	dir = t.TempDir()
	script := filepath.Join(dir, "hook.sh")
	if err := os.WriteFile(script, []byte(recorder), 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out")
	if err := os.Mkdir(out, 0o755); err != nil {
		t.Fatal(err)
	}
	return []string{script, out}, out
}

// ReadReceived returns every delivery the recorder has completed.
func ReadReceived(t testing.TB, dir string) []Received {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	slices.Sort(files)
	var out []Received
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var r Received
		r.Raw = string(raw)
		if err := json.Unmarshal(raw, &r.Payload); err != nil {
			t.Fatalf("hook stdin is not JSON: %v\n%s", err, raw)
		}
		envRaw, err := os.ReadFile(strings.TrimSuffix(f, ".json") + ".env")
		if err != nil {
			t.Fatal(err)
		}
		r.Env = map[string]string{}
		for _, line := range strings.Split(strings.TrimSpace(string(envRaw)), "\n") {
			k, v, _ := strings.Cut(line, "=")
			r.Env[k] = v
		}
		out = append(out, r)
	}
	return out
}

// WaitReceived polls until the recorder has n deliveries.
func WaitReceived(t testing.TB, dir string, n int) []Received {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := ReadReceived(t, dir)
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("hook ran %d times, want %d", len(got), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func quietLogger() *log.Logger { return log.New(nopWriter{}) }

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

func testConfig(argv []string) core.NotifyConfig {
	return core.NotifyConfig{
		Command:  argv,
		Events:   slices.Clone(core.DefaultNotifyEvents),
		Timeout:  5 * time.Second,
		Cooldown: time.Minute,
	}
}

func newTestDispatcher(t *testing.T, cfg core.NotifyConfig, opts Options) *Dispatcher {
	t.Helper()
	if opts.Hostname == nil {
		opts.Hostname = func() (string, error) { return "kitt", nil }
	}
	opts.Logger = quietLogger()
	d := New(cfg, opts)
	t.Cleanup(d.Close)
	return d
}

func TestDeliveryEnvAndStdin(t *testing.T) {
	argv, dir := NewRecorder(t)
	// A stale HARNESS_NOTIFY_* in the daemon's own environment must not
	// reach the hook alongside the real one.
	environ := func() []string { return append(os.Environ(), "HARNESS_NOTIFY_EVENT=stale") }
	d := newTestDispatcher(t, testConfig(argv), Options{Environ: environ})

	code := 1
	at := time.Date(2026, 9, 25, 20, 15, 16, 0, time.UTC)
	if !d.Notify(Notification{
		Event: core.NotifyFailed, Harness: "claude-rc", State: "failed",
		Message:  `claude-rc failed: "Error: You must be logged in to use Remote Control."`,
		Cause:    "Error: You must be logged in to use Remote Control.",
		Hint:     "harness logs claude-rc",
		Time:     at,
		ExitCode: &code, Restarts: 6,
	}) {
		t.Fatal("Notify refused a wanted event")
	}
	r := WaitReceived(t, dir, 1)[0]

	for k, want := range map[string]string{
		EnvEvent:   "failed",
		EnvHarness: "claude-rc",
		EnvHost:    "kitt",
		EnvState:   "failed",
		EnvMessage: `claude-rc failed: "Error: You must be logged in to use Remote Control."`,
	} {
		if r.Env[k] != want {
			t.Errorf("%s = %q, want %q", k, r.Env[k], want)
		}
	}
	p := r.Payload
	if p.Version != PayloadVersion || p.Event != "failed" || p.Harness != "claude-rc" || p.Host != "kitt" ||
		p.State != "failed" || p.Cause != "Error: You must be logged in to use Remote Control." ||
		p.Hint != "harness logs claude-rc" || p.Time != "2026-09-25T20:15:16Z" ||
		p.ExitCode == nil || *p.ExitCode != 1 || p.Restarts != 6 {
		t.Fatalf("payload = %+v\n%s", p, r.Raw)
	}
}

// The payload quotes agent output, which is where credentials turn up. They
// must be masked in the environment and on stdin alike.
func TestDeliveryIsRedacted(t *testing.T) {
	argv, dir := NewRecorder(t)
	d := newTestDispatcher(t, testConfig(argv), Options{})
	const secret = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	d.Notify(Notification{
		Event: core.NotifyFailed, Harness: "w",
		Message: "w failed: \"git push https://joe:" + secret + "@gitea.example/x.git\"\nsecond line",
		Cause:   "GITEA_TOKEN=" + secret,
	})
	r := WaitReceived(t, dir, 1)[0]
	if strings.Contains(r.Raw, secret) || strings.Contains(r.Env[EnvMessage], secret) {
		t.Fatalf("secret reached the hook:\nenv: %q\nstdin: %s", r.Env[EnvMessage], r.Raw)
	}
	if !strings.Contains(r.Payload.Message, "[REDACTED]") || !strings.Contains(r.Payload.Cause, "[REDACTED]") {
		t.Fatalf("nothing was masked — the check could not have failed: %+v", r.Payload)
	}
	if strings.Contains(r.Env[EnvMessage], "\n") || !strings.Contains(r.Env[EnvMessage], "second line") {
		t.Fatalf("message not folded onto one line: %q", r.Env[EnvMessage])
	}
}

func TestEventsFilter(t *testing.T) {
	argv, dir := NewRecorder(t)
	cfg := testConfig(argv)
	cfg.Events = []string{core.NotifyLoopStopped}
	d := newTestDispatcher(t, cfg, Options{})
	if d.Notify(Notification{Event: core.NotifyFailed, Harness: "a"}) {
		t.Fatal("an event not in `events` was queued")
	}
	if !d.Notify(Notification{Event: core.NotifyLoopStopped, Harness: "a"}) {
		t.Fatal("a listed event was refused")
	}
	got := WaitReceived(t, dir, 1)
	time.Sleep(100 * time.Millisecond)
	if got = ReadReceived(t, dir); len(got) != 1 || got[0].Payload.Event != core.NotifyLoopStopped {
		t.Fatalf("deliveries = %+v", got)
	}
}

func TestCooldownPerHarnessAndEvent(t *testing.T) {
	argv, dir := NewRecorder(t)
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	d := newTestDispatcher(t, testConfig(argv), Options{Now: func() time.Time { return now }})

	send := func(event, harness string) bool {
		return d.Notify(Notification{Event: event, Harness: harness})
	}
	if !send(core.NotifyFlapping, "a") {
		t.Fatal("first flapping refused")
	}
	if send(core.NotifyFlapping, "a") {
		t.Fatal("a repeat inside the cooldown was delivered")
	}
	// A different harness, or a different event, is its own key.
	if !send(core.NotifyFlapping, "b") || !send(core.NotifyFailed, "a") {
		t.Fatal("cooldown leaked across harnesses or events")
	}
	now = now.Add(time.Minute)
	if !send(core.NotifyFlapping, "a") {
		t.Fatal("still suppressed after the cooldown elapsed")
	}
	WaitReceived(t, dir, 4)
	if got := testutil.ToFloat64(d.deliveries.WithLabelValues(core.NotifyFlapping, ResultSuppressed)); got != 1 {
		t.Fatalf("suppressed counter = %v, want 1", got)
	}
}

// A harness brought back by an operator and failing again straight away must
// page again, or "recovered" is the last word on a dead harness.
func TestRecoveredClearsFailedCooldown(t *testing.T) {
	argv, dir := NewRecorder(t)
	d := newTestDispatcher(t, testConfig(argv), Options{})
	d.Notify(Notification{Event: core.NotifyFailed, Harness: "a"})
	d.Notify(Notification{Event: core.NotifyRecovered, Harness: "a"})
	if !d.Notify(Notification{Event: core.NotifyFailed, Harness: "a"}) {
		t.Fatal("a failure after recovery was suppressed by the earlier failure's cooldown")
	}
	WaitReceived(t, dir, 3)
}

func TestCooldownZeroDisablesDedupe(t *testing.T) {
	argv, dir := NewRecorder(t)
	cfg := testConfig(argv)
	cfg.Cooldown = 0
	d := newTestDispatcher(t, cfg, Options{})
	for range 3 {
		if !d.Notify(Notification{Event: core.NotifyFlapping, Harness: "a"}) {
			t.Fatal("suppressed with cooldown = 0")
		}
	}
	WaitReceived(t, dir, 3)
}

// The timeout kills the hook's whole process group: a script whose child
// keeps the output pipe open must not hold the delivery (or a worker) past
// it.
func TestTimeoutKillsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	script := filepath.Join(dir, "slow.sh")
	body := "#!/bin/sh\nsleep 30 &\necho $! > " + pidFile + "\nwait\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig([]string{script})
	cfg.Timeout = time.Second
	d := newTestDispatcher(t, cfg, Options{})

	start := time.Now()
	del := d.Test(context.Background())
	if del.Result != ResultTimeout {
		t.Fatalf("result = %+v, want timeout", del)
	}
	if took := time.Since(start); took > 4*time.Second {
		t.Fatalf("delivery took %s with a %s timeout: the child held it open", took, cfg.Timeout)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	deadline := time.Now().Add(3 * time.Second)
	for alive(pid) {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("the hook's child %d outlived the timeout", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := testutil.ToFloat64(d.deliveries.WithLabelValues(core.NotifyTest, ResultTimeout)); got != 1 {
		t.Fatalf("timeout counter = %v, want 1", got)
	}
}

// alive reports whether pid is a running process. A killed child whose
// parent is gone is reparented to PID 1, and in a CI container PID 1 may
// never reap it: it stays a zombie, which kill(pid, 0) still finds. A zombie
// has been killed, which is the property under test.
func alive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	if stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		// pid (comm) S ... — the state follows the last ')'.
		s := string(stat)
		if i := strings.LastIndexByte(s, ')'); i >= 0 && i+2 < len(s) {
			return s[i+2] != 'Z'
		}
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false // ps finds no such process
	}
	st := strings.TrimSpace(string(out))
	return st != "" && !strings.HasPrefix(st, "Z")
}

func TestFailingHookIsReported(t *testing.T) {
	cfg := testConfig([]string{"/bin/sh", "-c", "echo boom >&2; exit 3"})
	d := newTestDispatcher(t, cfg, Options{})
	del := d.Test(context.Background())
	if del.Result != ResultError || !strings.Contains(del.Error, "exit status 3") {
		t.Fatalf("delivery = %+v, want an exit-status error", del)
	}
	st := d.Status()
	if st.Last == nil || st.Last.Result != ResultError {
		t.Fatalf("status.Last = %+v, want the failed delivery", st.Last)
	}
	missing := newTestDispatcher(t, testConfig([]string{"/nonexistent/notify"}), Options{})
	if del := missing.Test(context.Background()); del.Result != ResultError {
		t.Fatalf("missing hook = %+v, want error", del)
	}
}

// Test ignores `events` and the cooldown: it is how the operator proves the
// hook works, whatever the table filters.
func TestTestIgnoresEventsAndCooldown(t *testing.T) {
	argv, dir := NewRecorder(t)
	cfg := testConfig(argv)
	cfg.Events = []string{core.NotifyRunFailed}
	d := newTestDispatcher(t, cfg, Options{})
	for range 2 {
		if del := d.Test(context.Background()); del.Result != ResultOK {
			t.Fatalf("test delivery = %+v", del)
		}
	}
	got := ReadReceived(t, dir)
	if len(got) != 2 || got[0].Payload.Event != core.NotifyTest || got[0].Env[EnvHost] != "kitt" {
		t.Fatalf("deliveries = %+v", got)
	}
	off := newTestDispatcher(t, core.NotifyConfig{}, Options{})
	if del := off.Test(context.Background()); del.Result != ResultError || !strings.Contains(del.Error, "not configured") {
		t.Fatalf("test with notify off = %+v", del)
	}
}

// Notify never blocks: with every worker busy and the queue full, the next
// notification is dropped and counted, not waited for.
func TestFullQueueDropsWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "block.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig([]string{script})
	cfg.Cooldown = 0
	d := newTestDispatcher(t, cfg, Options{Workers: 1, Queue: 1, ShutdownGrace: 50 * time.Millisecond})
	start := time.Now()
	var queued, dropped int
	for range 5 {
		if d.Notify(Notification{Event: core.NotifyFlapping, Harness: "a"}) {
			queued++
		} else {
			dropped++
		}
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("Notify blocked for %s", took)
	}
	if queued > 2 || dropped < 3 {
		t.Fatalf("queued %d dropped %d with one worker and a queue of one", queued, dropped)
	}
	if got := testutil.ToFloat64(d.deliveries.WithLabelValues(core.NotifyFlapping, ResultDropped)); got != float64(dropped) {
		t.Fatalf("dropped counter = %v, want %d", got, dropped)
	}
}

func TestSetConfigAppliesToNextNotification(t *testing.T) {
	argv, dir := NewRecorder(t)
	d := newTestDispatcher(t, core.NotifyConfig{}, Options{})
	if d.Notify(Notification{Event: core.NotifyFailed, Harness: "a"}) {
		t.Fatal("delivered with notify off")
	}
	d.SetConfig(testConfig(argv))
	if !d.Notify(Notification{Event: core.NotifyFailed, Harness: "a"}) {
		t.Fatal("not delivered after the reload turned notify on")
	}
	WaitReceived(t, dir, 1)
}
