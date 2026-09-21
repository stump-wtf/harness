package metrics

// Listener Tests
//
// Governing tests: SPEC-0013 REQ-1; design.md "Listener". The daemon-level
// refusal (a non-loopback bind with no token stops `harness daemon` before it
// serves) is exercised against the real binary in cmd/harness; these pin the
// policy it calls and the HTTP surface.
//
// @joestump-agent 09/21/2026 - Added for harness#356.

import (
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeToken(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "metrics.token")
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestResolveListener(t *testing.T) {
	good := writeToken(t, "s3cret-token\n", 0o600)
	loose := writeToken(t, "s3cret-token", 0o644)
	empty := writeToken(t, " \n", 0o600)

	for _, tc := range []struct {
		name      string
		listen    string
		tokenFile string
		wantAddr  string
		wantToken string
		wantLoose bool
		wantErr   error // nil, ErrNeedsToken, or errAny
	}{
		{name: "default is loopback", listen: "", wantAddr: DefaultListen},
		{name: "off", listen: ListenOff, wantAddr: ""},
		{name: "explicit loopback v4", listen: "127.0.0.1:9999", wantAddr: "127.0.0.1:9999"},
		{name: "loopback v6", listen: "[::1]:9999", wantAddr: "[::1]:9999"},
		{name: "localhost", listen: "localhost:9999", wantAddr: "localhost:9999"},
		{name: "all interfaces without token", listen: "0.0.0.0:9999", wantErr: ErrNeedsToken},
		{name: "empty host without token", listen: ":9999", wantErr: ErrNeedsToken},
		{name: "lan address without token", listen: "192.168.1.10:9999", wantErr: ErrNeedsToken},
		{name: "all interfaces with token", listen: "0.0.0.0:9999", tokenFile: good, wantAddr: "0.0.0.0:9999", wantToken: "s3cret-token"},
		{name: "loopback with token keeps it", listen: "", tokenFile: good, wantAddr: DefaultListen, wantToken: "s3cret-token"},
		{name: "loose token file still works", listen: "0.0.0.0:9999", tokenFile: loose, wantAddr: "0.0.0.0:9999", wantToken: "s3cret-token", wantLoose: true},
		{name: "empty token file refused", listen: "0.0.0.0:9999", tokenFile: empty, wantErr: errAny},
		{name: "missing token file refused even on loopback", listen: "", tokenFile: filepath.Join(t.TempDir(), "nope"), wantErr: errAny},
		{name: "unparseable address", listen: "nonsense", wantErr: errAny},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, err := ResolveListener(tc.listen, tc.tokenFile)
			switch {
			case tc.wantErr == nil && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != nil && err == nil:
				t.Fatalf("resolved %+v, want an error", l)
			case tc.wantErr == ErrNeedsToken && !errors.Is(err, ErrNeedsToken):
				t.Fatalf("error %v, want ErrNeedsToken", err)
			}
			if err != nil {
				if strings.Contains(err.Error(), "s3cret") {
					t.Errorf("error leaks the token: %v", err)
				}
				return
			}
			if l.Addr != tc.wantAddr || l.Token != tc.wantToken || l.TokenFileLoose != tc.wantLoose {
				t.Errorf("resolved %+v, want addr %q token %q loose %v", l, tc.wantAddr, tc.wantToken, tc.wantLoose)
			}
		})
	}
}

// errAny marks a case that must fail with any error.
var errAny = errors.New("any error")

func TestIsLoopback(t *testing.T) {
	for host, want := range map[string]bool{
		"127.0.0.1": true, "127.1.2.3": true, "::1": true, "localhost": true, "LOCALHOST": true, "fe80::1%lo0": false,
		"": false, "0.0.0.0": false, "::": false, "10.0.0.1": false, "example.com": false,
	} {
		if got := IsLoopback(host); got != want {
			t.Errorf("IsLoopback(%q) = %v, want %v", host, got, want)
		}
	}
}

func get(t *testing.T, url, auth string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

// The listener serves Prometheus text format (REQ-1: text/plain;
// version=0.0.4), and only at /metrics.
func TestListenServesTextFormat(t *testing.T) {
	src := newFakeSource()
	src.add(crushHarness("worker"), runningSnap())
	m := newTestMetrics(t, src, Options{})
	srv, err := Listen(Listener{Addr: "127.0.0.1:0"}, m.Handler())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(t.Context()) })

	resp, body := get(t, "http://"+srv.Addr()+"/metrics", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Errorf("Content-Type = %q, want text/plain; version=0.0.4", ct)
	}
	if v := parse(t, body).must(t, "harness_harness_state", lbls("harness", "worker", "state", "running")); v != 1 {
		t.Errorf("running = %v over HTTP", v)
	}
	if resp, _ := get(t, "http://"+srv.Addr()+"/", ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("/ status %d, want 404", resp.StatusCode)
	}
}

// With a token, a request without the exact bearer is refused.
func TestListenRequiresBearerToken(t *testing.T) {
	m := newTestMetrics(t, newFakeSource(), Options{})
	srv, err := Listen(Listener{Addr: "127.0.0.1:0", Token: "s3cret-token"}, m.Handler())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(t.Context()) })
	url := "http://" + srv.Addr() + "/metrics"

	for _, auth := range []string{"", "Bearer wrong", "Bearer s3cret-token-and-more", "s3cret-token", "Basic czNjcmV0LXRva2Vu"} {
		resp, body := get(t, url, auth)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("auth %q: status %d, want 401", auth, resp.StatusCode)
		}
		if resp.Header.Get("WWW-Authenticate") == "" {
			t.Errorf("auth %q: no WWW-Authenticate challenge", auth)
		}
		if strings.Contains(body, "harness_") {
			t.Errorf("auth %q: body leaked metrics", auth)
		}
	}
	if resp, _ := get(t, url, "Bearer s3cret-token"); resp.StatusCode != http.StatusOK {
		t.Errorf("correct token: status %d", resp.StatusCode)
	}
}

// A port that is already taken is an error the daemon logs and survives; see
// Listen's comment for why it does not exit.
func TestListenReportsBindFailure(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	m := newTestMetrics(t, newFakeSource(), Options{})
	if srv, err := Listen(Listener{Addr: held.Addr().String()}, m.Handler()); err == nil {
		_ = srv.Shutdown(t.Context())
		t.Fatal("bound a port another listener holds")
	}
	if _, err := Listen(Listener{}, m.Handler()); err == nil {
		t.Error("Listen with the listener off returned no error")
	}
}
