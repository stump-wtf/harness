package daemon

// Merge Train Runner Tests
//
// Governing tests: SPEC-0025 REQ-1, REQ-11, REQ-15; #604 — nothing starts
// when the train is disabled; an enabled train runs its drivers and stops
// cleanly, releasing their locks; an enabled train with no token refuses; a
// repo whose lock is held elsewhere is skipped, not raced.

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/forge"
	"github.com/stump-wtf/harness/internal/forge/fake"
	"github.com/stump-wtf/harness/internal/mergetrain"
)

type recLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *recLog) add(level string, m interface{}, kv []interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf("%s %v %v", level, m, kv))
}
func (l *recLog) Info(m interface{}, kv ...interface{})  { l.add("INFO", m, kv) }
func (l *recLog) Warn(m interface{}, kv ...interface{})  { l.add("WARN", m, kv) }
func (l *recLog) Error(m interface{}, kv ...interface{}) { l.add("ERROR", m, kv) }
func (l *recLog) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func enabledConfig(repos ...string) core.MergeTrainConfig {
	c := core.DefaultMergeTrainConfig()
	c.Enabled = true
	c.Repos = repos
	c.PollInterval = 10 * time.Millisecond
	c.CITimeout = time.Second
	c.ForgeBaseURL = "https://gitea.example"
	c.ForgeTokenEnv = "MT_TOKEN"
	return c
}

func opts(t *testing.T, c core.MergeTrainConfig, f forge.Forge, log mergetrain.Logger) MergeTrainOptions {
	return MergeTrainOptions{
		Config:   c,
		Getenv:   func(k string) string { return map[string]string{"MT_TOKEN": "tok"}[k] },
		StateDir: t.TempDir(),
		Log:      log,
		NewForge: func(baseURL, token, cacheDir string) (forge.Forge, error) {
			if baseURL != "https://gitea.example" || token != "tok" || !strings.HasSuffix(cacheDir, filepath.Join("mergetrain", "cache")) {
				return nil, fmt.Errorf("NewForge(%q, token set=%v, %q)", baseURL, token != "", cacheDir)
			}
			return f, nil
		},
		TrainPollMin: time.Millisecond,
		RetryPause:   time.Millisecond,
	}
}

func TestMergeTrainDisabledStartsNothing(t *testing.T) {
	f := fake.New()
	o := opts(t, core.DefaultMergeTrainConfig(), f, &recLog{})
	called := false
	o.NewForge = func(string, string, string) (forge.Forge, error) { called = true; return f, nil }
	m, err := StartMergeTrain(context.Background(), o)
	if err != nil || m != nil {
		t.Fatalf("StartMergeTrain(disabled) = %v, %v; want nil, nil", m, err)
	}
	if called || m.Running() != 0 {
		t.Fatal("a disabled train built a forge or a driver")
	}
	m.Stop() // nil-safe
	time.Sleep(30 * time.Millisecond)
	if n := len(f.Calls()); n != 0 {
		t.Fatalf("a disabled train made %d forge calls", n)
	}
}

func TestMergeTrainRunsAndStopsCleanly(t *testing.T) {
	f := fake.New()
	f.SetBranch("stump.wtf/harness", "main", "base")
	f.SetBranch("stump.wtf/cairn", "main", "base")
	log := &recLog{}
	o := opts(t, enabledConfig("stump.wtf/harness", "stump.wtf/cairn"), f, log)
	m, err := StartMergeTrain(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if m.Running() != 2 {
		t.Fatalf("running %d drivers, want 2", m.Running())
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		n := 0
		for _, c := range f.Calls() {
			if c.Method == "ListOpenPRs" {
				n++
			}
		}
		if n >= 4 { // both drivers ticked at least twice
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("drivers did not tick: %v", f.Methods())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if txt := log.text(); !strings.Contains(txt, "WARN merge train enabled") || !strings.Contains(txt, "stump.wtf/harness,stump.wtf/cairn") || !strings.Contains(txt, "mode report") {
		t.Fatalf("no loud start line naming mode and repos:\n%s", txt)
	}

	done := make(chan struct{})
	go func() { m.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return")
	}
	// Stopped means stopped: no more calls, and the locks are free.
	before := len(f.Calls())
	time.Sleep(50 * time.Millisecond)
	if after := len(f.Calls()); after != before {
		t.Fatalf("%d forge calls after Stop", after-before)
	}
	for _, r := range []string{"stump.wtf/harness", "stump.wtf/cairn"} {
		l, err := mergetrain.AcquireLock(filepath.Join(o.StateDir, "mergetrain"), r)
		if err != nil {
			t.Fatalf("%s lock still held after Stop: %v", r, err)
		}
		_ = l.Release()
	}
}

func TestMergeTrainNoTokenRefuses(t *testing.T) {
	o := opts(t, enabledConfig("stump.wtf/harness"), fake.New(), &recLog{})
	o.Getenv = func(string) string { return "" }
	m, err := StartMergeTrain(context.Background(), o)
	if err == nil || m != nil || !strings.Contains(err.Error(), "$MT_TOKEN is empty") {
		t.Fatalf("StartMergeTrain without a token = %v, %v", m, err)
	}
}

func TestMergeTrainLockedRepoSkipped(t *testing.T) {
	f := fake.New()
	log := &recLog{}
	o := opts(t, enabledConfig("stump.wtf/harness", "stump.wtf/cairn"), f, log)
	held, err := mergetrain.AcquireLock(filepath.Join(o.StateDir, "mergetrain"), "stump.wtf/harness")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	m, err := StartMergeTrain(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	if m.Running() != 1 {
		t.Fatalf("running %d, want 1 (harness skipped)", m.Running())
	}
	if !strings.Contains(log.text(), "ERROR merge train: repo skipped") {
		t.Fatalf("skip not logged at error:\n%s", log.text())
	}
}
