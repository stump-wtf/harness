package metrics

// Metrics Listener
//
// /metrics is served by its own http.Server, separate from the SSH cockpit and
// independent of [server] enabled, which governs only that front door. It
// binds 127.0.0.1:10229 by default (REQ-1) and moves with [server]
// metrics_listen; moving it needs a daemon restart, because `harness reload`
// re-reads harness definitions and never rebinds a listener.
//
// Why 10229: every port from 9100 to 9999 in Prometheus's default port
// allocations list is taken by some exporter (no FREE rows as of 2026-09-21),
// so any choice there collides with something a fleet may already run beside
// the daemon. 10229 appears neither in that list nor in the IANA services
// file, and stays clear of the crowded 10000–10055 block the list does use.
//
// Loopback needs no auth. Anything else needs a bearer token, and a daemon
// asked to bind off loopback without one refuses to start (REQ-1): a
// non-loopback listener without auth is a configuration mistake, and serving
// agent inventory to the network until someone notices is worse than a loud
// refusal. The token is read from a file named by metrics_token_file:
//
//   - not harness.toml, which holds no secrets (ADR-0008);
//   - not a HARNESS_* variable, which SPEC-0010 REQ "Secrets Exclusion" bars
//     from carrying credentials;
//   - and not any daemon environment variable, because every supervised
//     harness inherits the daemon's environment at spawn — a token there
//     would be handed to each agent CLI the daemon runs, most of them with
//     permission prompts turned off.
//
// A file is also what the secret backend renders (Vault Agent / OpenBao
// templates), so the daemon never needs to know about it.
//
// There is an off switch, metrics_listen = "off". Loopback is not private on a
// shared host: every local account can connect to 127.0.0.1, and the daemon's
// only other local surface is a 0600 socket (ADR-0008). An operator on such a
// host needs a way to expose nothing.
//
// A loopback address that is already in use does not stop the daemon; see
// Listen.
//
// Governing: ADR-0020, SPEC-0013 REQ-1; ADR-0008; design.md "Listener".
//
// @joestump-agent 09/21/2026 - Added for harness#356.

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// DefaultPort is the pinned default metrics port; see the header for why.
const DefaultPort = 10229

// DefaultListen is the default bind address: loopback only (REQ-1).
const DefaultListen = "127.0.0.1:10229"

// ListenOff is the metrics_listen value that disables the listener.
const ListenOff = "off"

// Listener is a resolved, validated listener configuration.
type Listener struct {
	// Addr is the bind address; empty means the listener is off.
	Addr string
	// Token is the bearer token requests must carry; empty means none.
	Token string
	// TokenFileLoose reports a token file readable by group or others. The
	// token still works; the daemon warns.
	TokenFileLoose bool
}

// ErrNeedsToken reports a non-loopback bind with no token configured.
var ErrNeedsToken = errors.New("a non-loopback metrics listener requires a bearer token")

// ResolveListener turns [server] metrics_listen and metrics_token_file into a
// Listener, refusing the combinations the daemon must not start with: a
// non-loopback address without a token, and a token file that is named but
// unreadable or empty (serving without the auth an operator asked for would be
// the same mistake, arrived at by accident).
func ResolveListener(listen, tokenFile string) (Listener, error) {
	listen = strings.TrimSpace(listen)
	if listen == ListenOff {
		return Listener{}, nil
	}
	if listen == "" {
		listen = DefaultListen
	}
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return Listener{}, fmt.Errorf("metrics_listen %q: %w", listen, err)
	}
	l := Listener{Addr: listen}
	if tokenFile != "" {
		l.Token, l.TokenFileLoose, err = readToken(tokenFile)
		if err != nil {
			return Listener{}, err
		}
	}
	if !IsLoopback(host) && l.Token == "" {
		return Listener{}, fmt.Errorf("metrics_listen %q: %w: set [server] metrics_token_file to a file holding one, or bind 127.0.0.1", listen, ErrNeedsToken)
	}
	return l, nil
}

// IsLoopback reports whether host names only the loopback interface. An empty
// host (":10229") binds every interface and is not loopback.
func IsLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if i := strings.IndexByte(host, '%'); i >= 0 {
		host = host[:i] // an IPv6 zone does not change the address class
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// readToken reads and trims a token file. The value never appears in an error.
func readToken(path string) (token string, loose bool, err error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", false, fmt.Errorf("metrics_token_file: %w", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false, fmt.Errorf("metrics_token_file: %w", err)
	}
	token = strings.TrimSpace(string(raw))
	if token == "" {
		return "", false, fmt.Errorf("metrics_token_file %s is empty", path)
	}
	return token, info.Mode().Perm()&0o077 != 0, nil
}

// Server is a running metrics listener.
type Server struct {
	srv *http.Server
	ln  net.Listener
}

// Listen binds l.Addr and serves h at /metrics, behind the bearer check when
// l.Token is set. It binds synchronously, so a nil error means the port is
// held and a scrape will reach it.
//
// A bind failure is returned for the caller to log, and the daemon carries on
// without the listener rather than exiting. Supervising the agents is its job;
// refusing to do that because another process holds a port would turn an
// observability gap into an outage of every harness on the host. The gap is
// not silent: the scraper sees the target down (up == 0), which is the signal
// Prometheus already alerts on. That is the difference from a missing token,
// which is a configuration the daemon refuses outright.
func Listen(l Listener, h http.Handler) (*Server, error) {
	if l.Addr == "" {
		return nil, errors.New("metrics listener is off")
	}
	ln, err := net.Listen("tcp", l.Addr)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", requireToken(l.Token, h))
	s := &Server{
		srv: &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second},
		ln:  ln,
	}
	go func() { _ = s.srv.Serve(ln) }()
	return s, nil
}

// Addr is the bound address (with the real port when l.Addr asked for :0).
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Shutdown stops accepting and waits for in-flight scrapes, up to ctx.
func (s *Server) Shutdown(ctx context.Context) error { return s.srv.Shutdown(ctx) }

// requireToken wraps h in a constant-time bearer check. An empty token passes
// everything through: loopback without a token file is unauthenticated by
// design (REQ-1).
func requireToken(token string, h http.Handler) http.Handler {
	if token == "" {
		return h
	}
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="harness"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}
