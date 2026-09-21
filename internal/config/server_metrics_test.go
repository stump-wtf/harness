package config

// Metrics Listener Keys
//
// [server] metrics_listen and metrics_token_file configure the Prometheus
// listener (ADR-0020, SPEC-0013 REQ-1). The parser owns syntax only: an
// address that could never bind is a load error with a line number, and the
// token FILE is carried as a path and never opened here — whether a
// non-loopback bind has a usable token is the daemon's startup refusal, since
// the token must never pass through harness.toml (ADR-0008).
//
// @joestump-agent 09/21/2026 - Added with the metrics listener (harness#356).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServerMetricsKeys(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("cannot determine home dir: %v", err)
	}
	cfg, err := Parse([]byte(`
[server]
metrics_listen = "0.0.0.0:10229"
metrics_token_file = "~/.config/harness/metrics.token"
`), "test.toml")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	sc := cfg.Server
	if sc.MetricsListen != "0.0.0.0:10229" {
		t.Errorf("MetricsListen = %q", sc.MetricsListen)
	}
	if want := filepath.Join(home, ".config/harness/metrics.token"); sc.MetricsTokenFile != want {
		t.Errorf("MetricsTokenFile = %q, want %q", sc.MetricsTokenFile, want)
	}
	// Independent of the SSH remote: configuring metrics must not require,
	// or imply, enabled = true.
	if sc.Enabled {
		t.Error("metrics keys turned the SSH server on")
	}
}

func TestServerMetricsListenValidation(t *testing.T) {
	for _, tc := range []struct {
		listen  string
		wantErr bool
	}{
		{"", false},
		{"off", false},
		{"127.0.0.1:10229", false},
		{"[::1]:9999", false},
		{":10229", false},
		{"localhost:0", false},
		{"127.0.0.1", true},       // no port
		{"127.0.0.1:http", true},  // named port: never what an operator meant
		{"127.0.0.1:70000", true}, // out of range
		{"on", true},
	} {
		t.Run(tc.listen, func(t *testing.T) {
			_, err := Parse([]byte("[server]\nmetrics_listen = \""+tc.listen+"\"\n"), "test.toml")
			if (err != nil) != tc.wantErr {
				t.Fatalf("metrics_listen %q: err = %v, wantErr %v", tc.listen, err, tc.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "metrics_listen") {
				t.Errorf("error does not name the key: %v", err)
			}
		})
	}
}
