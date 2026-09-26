package switchboard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// loopbackCallback runs the OAuth redirect receiver: 127.0.0.1 on an
// EPHEMERAL port, accepting only GETs that carry OUR state, ignoring stray
// hits without aborting, answering with no-store / no-content CSP headers,
// and closing after the one valid callback.
//
// Governing: SPEC-0018 "Security Requirements" (loopback listener).
type loopbackCallback struct {
	listener net.Listener
	srv      *http.Server
	port     int
	// state is the value a callback must carry to be ours. It is fixed at
	// construction, before the authorization URL exists, so no callback is
	// ever judged against an unset state.
	state     string
	result    chan callbackResult
	closeOnce sync.Once
}

type callbackResult struct {
	code string
	err  error
}

// startLoopback binds the receiver for a login whose state is wantState. The
// port is ephemeral: Login binds before it registers the client, so the
// redirect URI it registers — and later sends on the authorize and token
// requests — names exactly the port this call bound.
func startLoopback(wantState string) (*loopbackCallback, error) {
	if wantState == "" {
		return nil, errors.New("switchboard: loopback listener needs a state")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("switchboard: bind loopback listener: %w", err)
	}
	cb := &loopbackCallback{
		listener: l,
		port:     l.Addr().(*net.TCPAddr).Port,
		state:    wantState,
		result:   make(chan callbackResult, 1),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", cb.handle)
	cb.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		// Serve returns ErrServerClosed on Shutdown; the login flow owns the
		// lifetime, so the error is deliberately discarded.
		_ = cb.srv.Serve(l)
	}()
	return cb, nil
}

// redirectURI is the exact URI this listener serves, port included.
func (c *loopbackCallback) redirectURI() string {
	return fmt.Sprintf("http://127.0.0.1:%d/callback", c.port)
}

func (c *loopbackCallback) handle(w http.ResponseWriter, r *http.Request) {
	// Security headers on EVERY response, valid or stray:
	// default-src 'none' because we serve nothing but a static line, and
	// no-store because a callback URL must not land in a browser cache.
	w.Header().Set("Content-Security-Policy", "default-src 'none'")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		fmt.Fprintln(w, "method not allowed")
		return
	}
	q := r.URL.Query()
	if q.Get("state") != c.state {
		// A stray hit (a scanner, a preflight, a stale or forged callback)
		// must not end the login, and must not take the one result slot the
		// real callback needs: reject it and keep waiting.
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintln(w, "unexpected callback")
		return
	}
	if code := q.Get("error"); code != "" {
		// Our state with an error (RFC 6749 §4.1.2.1): the operator denied
		// the request or the server refused it. End the login now rather
		// than waiting out the timeout. Both values are server-authored
		// text, so they are bounded and stripped of control characters.
		err := fmt.Errorf("%w: authorization server returned %s", ErrAuthFailed, boundText(code, 64))
		if desc := q.Get("error_description"); desc != "" {
			err = fmt.Errorf("%w: %s", err, boundText(desc, 200))
		}
		c.deliver(callbackResult{err: err})
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintln(w, "Authorization failed. You can close this tab.")
		return
	}
	code := q.Get("code")
	if code == "" {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintln(w, "missing code")
		return
	}
	c.deliver(callbackResult{code: code})
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "<!doctype html><title>Harness</title><p>Authorized. You can close this tab.</p>")
}

// deliver hands wait the first result for our state; a repeat of the same
// callback (a browser reload) finds the slot full and is dropped.
func (c *loopbackCallback) deliver(res callbackResult) {
	select {
	case c.result <- res:
	default:
	}
}

// wait blocks until the callback carrying our state arrives, the context
// lapses, or the 5-minute ceiling does, then closes the listener. The
// handler has already rejected every other state, so the first result is
// the answer: a code, or the authorization server's error.
func (c *loopbackCallback) wait(ctx context.Context) (string, error) {
	defer func() { _ = c.close() }()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(5 * time.Minute):
		return "", errors.New("switchboard: timed out waiting for the authorization callback")
	case res := <-c.result:
		return res.code, res.err
	}
}

// close shuts the listener down; it is idempotent, so Login can defer it on
// every path while wait closes it as soon as the callback lands.
func (c *loopbackCallback) close() error {
	var err error
	c.closeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		err = c.srv.Shutdown(ctx)
	})
	return err
}

// boundText caps server-authored text at max bytes and drops control
// characters, so it can go into an error a terminal prints.
func boundText(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if len(s) > max {
		cut := max
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut-- // never split a rune
		}
		s = s[:cut] + "…"
	}
	return s
}

// authorizeURLForNoBrowser is the URL --no-browser prints, with the reminder
// that the redirect must REACH this machine (an SSH port forward of the
// ephemeral port, until the device grant lands; stump.wtf/switchboard#308
// replaces this).
const noBrowserHint = "the redirect must reach this machine: forward the port, e.g. ssh -L %d:127.0.0.1:%d <host>"
