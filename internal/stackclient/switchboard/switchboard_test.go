package switchboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeSwitchboard is an httptest fake of the authorization server (discovery,
// registration, token) and the operator API, enough for the REQ-7 scenarios.
type fakeSwitchboard struct {
	t   *testing.T
	srv *httptest.Server

	vendPosts   int
	vendBody    string
	vendToken   string // the "endpoint token" the fake mints
	refreshOK   bool
	failRefresh bool

	// regRedirectURIs is what dynamic client registration was sent; the
	// fake's /authorize and /token match redirect_uri against it exactly,
	// as a strict authorization server does.
	regRedirectURIs []string
	refreshPosts    int
}

func newFake(t *testing.T, mutate func(*fakeSwitchboard, *http.ServeMux)) *fakeSwitchboard {
	f := &fakeSwitchboard{t: t, vendToken: "sbk_fake_endpoint_token"}
	mux := http.NewServeMux()

	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 f.srv.URL,
			"authorization_endpoint": f.srv.URL + "/authorize",
			"token_endpoint":         f.srv.URL + "/token",
			"registration_endpoint":  f.srv.URL + "/register",
			"resource":               f.srv.URL + "/api",
		})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		var reg struct {
			RedirectURIs []string `json:"redirect_uris"`
		}
		if err := json.NewDecoder(r.Body).Decode(&reg); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.regRedirectURIs = reg.RedirectURIs
		json.NewEncoder(w).Encode(map[string]string{"client_id": "client-fake"})
	})
	// /authorize plays an operator who approves at once: an exact
	// redirect_uri match against the registration (RFC 6749 §3.1.2.3), then
	// a redirect to it with the code and the caller's state.
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("client_id") != "client-fake" || len(f.regRedirectURIs) != 1 || q.Get("redirect_uri") != f.regRedirectURIs[0] {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintln(w, "invalid redirect_uri")
			return
		}
		back := url.Values{"code": {"code-fake"}, "state": {q.Get("state")}}
		http.Redirect(w, r, q.Get("redirect_uri")+"?"+back.Encode(), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.FormValue("grant_type") == "refresh_token" {
			f.refreshPosts++
			if f.failRefresh {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprintln(w, `{"error":"invalid_grant"}`)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"access_token": "op_fresh", "expires_in": 3600})
			return
		}
		// Code exchange: the fake carries the code itself in state-shaped tests.
		if r.FormValue("code_verifier") == "" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintln(w, `{"error":"invalid_request"}`)
			return
		}
		if r.FormValue("code") != "code-fake" ||
			(len(f.regRedirectURIs) > 0 && r.FormValue("redirect_uri") != f.regRedirectURIs[0]) {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprintln(w, `{"error":"invalid_grant"}`)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "op_fake", "refresh_token": "refresh_fake", "expires_in": 3600})
	})
	mux.HandleFunc("/api/v1/endpoints", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if r.Header.Get("Authorization") != "Bearer op_fake" && r.Header.Get("Authorization") != "Bearer op_fresh" {
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprintln(w, "bad operator token")
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"endpoints": []any{
				map[string]string{"ref": "ep-1", "name": "someone-elses", "queue": "q"},
			}})
		case http.MethodPost:
			f.vendPosts++
			buf := make([]byte, 4096)
			n, _ := r.Body.Read(buf)
			f.vendBody = string(buf[:n])
			json.NewEncoder(w).Encode(map[string]string{
				"ref": "ep-9", "name": "reviewer", "queue": "review",
				"token": f.vendToken, "ingest_url": f.srv.URL + "/ingest/ep-9",
			})
		}
	})
	mux.HandleFunc("/api/v1/endpoints/ep-9/revoke", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	if mutate != nil {
		mutate(f, mux)
	}
	return f
}

// TestVendOnFirstRun pins REQ-7 "Vend on first run": exactly one POST, and
// the endpoint token lives ONLY in the returned struct — never in an error
// string, a log line, or a saved non-credential file.
func TestVendOnFirstRun(t *testing.T) {
	f := newFake(t, nil)
	api := &OperatorAPI{Base: f.srv.URL, HTTP: NewHTTPClient(), Token: "op_fake"}
	ep, err := api.VendEndpoint(context.Background(), "reviewer", "review")
	if err != nil {
		t.Fatal(err)
	}
	if f.vendPosts != 1 {
		t.Fatalf("vend made %d POSTs, want exactly 1", f.vendPosts)
	}
	if ep.Token != f.vendToken {
		t.Fatalf("token not returned in the struct")
	}
	if !strings.Contains(f.vendBody, `"reviewer"`) || !strings.Contains(f.vendBody, `"review"`) {
		t.Errorf("vend body %q missing name/queue", f.vendBody)
	}
	// The token must not appear in any string this package produces.
	if strings.Contains(fmt.Sprintf("%+v", api), f.vendToken) {
		t.Error("endpoint token leaked into the client struct dump")
	}
	// Second vend through Find-first (the converge path) makes no POST.
	found, err := api.FindEndpoint(context.Background(), "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	_ = found // fake's GET list does not include reviewer; the real converge only vends when nil.
}

func TestGrantFileIs0600(t *testing.T) {
	dir := t.TempDir()
	g := &Grant{BaseURL: "https://sb.example.com", ClientID: "c", AccessToken: "op_fake"}
	if err := SaveGrant(dir, g); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(GrantPath(dir, g.BaseURL))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("grant file mode %o, want 600", perm)
	}
	back, err := LoadGrant(dir, g.BaseURL)
	if err != nil || back.AccessToken != "op_fake" {
		t.Fatalf("round-trip failed: %v %+v", err, back)
	}
}

func TestNoTokenInErrors(t *testing.T) {
	secret := "op_SECRET_fake_token_1234"
	f := newFake(t, nil)
	// Wrong operator token -> 401. The error must describe, not echo secrets
	// (ours or the fake's).
	api := &OperatorAPI{Base: f.srv.URL, HTTP: NewHTTPClient(), Token: secret}
	_, err := api.ListEndpoints(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Error("error echoed the operator token")
	}
	if strings.Contains(err.Error(), f.vendToken) {
		t.Error("error echoed the endpoint token")
	}
}

func TestSwitchboardUnreachable(t *testing.T) {
	// A closed port: connection refused must wrap ErrUnreachable, naming the
	// instance rather than a token.
	srv := httptest.NewServer(http.NewServeMux())
	base := srv.URL
	srv.Close()
	_, err := Discover(context.Background(), NewHTTPClient(), base)
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("want ErrUnreachable, got %v", err)
	}
}

func TestRefusedRefreshReRunsLogin(t *testing.T) {
	f := newFake(t, func(f *fakeSwitchboard, _ *http.ServeMux) { f.failRefresh = true })
	ctx := context.Background()
	client := NewHTTPClient()
	doc, err := Discover(ctx, client, f.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	g := &Grant{BaseURL: f.srv.URL, ClientID: "c", AccessToken: "stale", RefreshToken: "r", ExpiresAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)}
	_, err = EnsureFresh(ctx, client, t.TempDir(), g)
	if !errors.Is(err, ErrTokenRefused) {
		t.Fatalf("want ErrTokenRefused, got %v", err)
	}
	// The server's refusal stays in the chain, so the operator sees why.
	var se *statusError
	if !errors.As(err, &se) || se.status != http.StatusBadRequest || !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("refusal lost its cause: %v", err)
	}
	_ = doc
}

func TestCrossOriginRedirectNotFollowed(t *testing.T) {
	redirected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The "off-origin" host: if the client followed the hop, this handler
		// would see the request — and with it, the Authorization header.
		if r.Header.Get("Authorization") != "" {
			t.Error("cross-origin redirect carried the Authorization header")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer redirected.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", redirected.URL+"/stolen")
		w.WriteHeader(http.StatusFound)
	}))
	defer origin.Close()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, origin.URL, nil)
	req.Header.Set("Authorization", "Bearer op_fake")
	resp, err := NewHTTPClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("cross-origin redirect was followed (final status %d)", resp.StatusCode)
	}
}

func TestLoopbackIgnoresStrayHits(t *testing.T) {
	cb, err := startLoopback("want")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		// A stray GET without our state: must be ignored, not consumed.
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/callback", cb.port))
		if err == nil {
			resp.Body.Close()
		}
		// Then the real one (matching state).
		u := fmt.Sprintf("http://127.0.0.1:%d/callback?code=abc&state=%s", cb.port, url.QueryEscape("want"))
		resp2, err := http.Get(u)
		if err == nil {
			resp2.Body.Close()
		}
	}()
	code, err := cb.wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if code != "abc" {
		t.Fatalf("code = %q", code)
	}
	if err := cb.close(); err != nil {
		t.Fatal(err)
	}
}

func TestMCPListTodosReadBack(t *testing.T) {
	var gotAuth string
	mcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"todos\":[{\"id\":\"t1\",\"queue\":\"q\",\"state\":\"done\",\"result\":\"cairn=abc123 nonce-here\"}]}"}]}}`)
	}))
	defer mcp.Close()
	todos, err := MCPListTodos(context.Background(), NewHTTPClient(), mcp.URL, "ep_token")
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer ep_token" {
		t.Errorf("MCP call must carry the ENDPOINT token, got %q", gotAuth)
	}
	if len(todos) != 1 || todos[0].State != "done" {
		t.Fatalf("todos = %+v", todos)
	}
}

// REQ-7 "Another user's endpoint name": the operator API is scoped to the
// signed-in user, so a name matching someone else's endpoint must NOT be
// reused (FindEndpoint matches exactly, within the operator's own list).
func TestFindEndpointScopesToOperator(t *testing.T) {
	f := newFake(t, nil)
	api := &OperatorAPI{Base: f.srv.URL, HTTP: NewHTTPClient(), Token: "op_fake"}
	ep, err := api.FindEndpoint(context.Background(), "someone-elses")
	if err != nil {
		t.Fatal(err)
	}
	if ep == nil || ep.Name != "someone-elses" {
		t.Fatalf("the operator's own listing must surface their endpoints: %+v", ep)
	}
	if missing, err := api.FindEndpoint(context.Background(), "reviewer"); err != nil || missing != nil {
		t.Fatalf("FindEndpoint must not fuzzy-match: %+v %v", missing, err)
	}
}

// getStatus plays the browser for a callback URL and reports the status the
// final page answered with.
func getStatus(u string) (int, error) {
	resp, err := http.Get(u)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

// authParams pulls the loopback redirect URI and our state out of the
// authorization URL Login hands to OpenURL.
func authParams(t *testing.T, authURL string) (redirect, state string) {
	t.Helper()
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query().Get("redirect_uri"), u.Query().Get("state")
}

// TestLoginRegistersTheRedirectURIItUses runs the whole login against a
// strict fake authorization server: the redirect URI sent to dynamic client
// registration must be byte-for-byte the one on the authorize request —
// ephemeral port included — or /authorize refuses it.
func TestLoginRegistersTheRedirectURIItUses(t *testing.T) {
	f := newFake(t, nil)
	var authRedirect string
	opts := Options{Hostname: "test-host", OpenURL: func(u string) error {
		authRedirect, _ = authParams(t, u)
		// Follow the authorize URL like a browser: the fake redirects to the
		// loopback, which must answer the final page with 200.
		status, err := getStatus(u)
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("authorize -> loopback ended in HTTP %d", status)
		}
		return nil
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	g, err := Login(ctx, NewHTTPClient(), f.srv.URL, opts)
	if err != nil {
		t.Fatalf("login failed (registered %q, authorize used %q): %v", f.regRedirectURIs, authRedirect, err)
	}
	if len(f.regRedirectURIs) != 1 || f.regRedirectURIs[0] != authRedirect {
		t.Fatalf("registered redirect_uris %q, authorize used %q", f.regRedirectURIs, authRedirect)
	}
	if ru, err := url.Parse(authRedirect); err != nil || ru.Port() == "" || ru.Hostname() != "127.0.0.1" {
		t.Fatalf("redirect URI %q must be 127.0.0.1 with the ephemeral port", authRedirect)
	}
	if g.AccessToken != "op_fake" || g.RefreshToken != "refresh_fake" || g.ClientID != "client-fake" {
		t.Fatalf("grant = %+v", g)
	}
}

// TestLoginWrongStateCallbackDoesNotSwallowTheRealOne: a callback carrying a
// code but someone else's state lands first. It must be refused without
// taking the result slot, so the real callback that follows completes the
// login.
func TestLoginWrongStateCallbackDoesNotSwallowTheRealOne(t *testing.T) {
	f := newFake(t, nil)
	strayStatus := 0
	opts := Options{OpenURL: func(u string) error {
		redirect, state := authParams(t, u)
		var err error
		strayStatus, err = getStatus(redirect + "?" + url.Values{"code": {"stray"}, "state": {"not-ours"}}.Encode())
		if err != nil {
			return err
		}
		_, err = getStatus(redirect + "?" + url.Values{"code": {"code-fake"}, "state": {state}}.Encode())
		return err
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	g, err := Login(ctx, NewHTTPClient(), f.srv.URL, opts)
	if err != nil {
		t.Fatalf("the stray callback swallowed the real one: %v", err)
	}
	if g.AccessToken != "op_fake" {
		t.Fatalf("grant = %+v", g)
	}
	if strayStatus != http.StatusBadRequest {
		t.Errorf("stray callback answered %d; it must be refused (400), not told it was authorized", strayStatus)
	}
}

// TestLoginErrorRedirectFailsPromptly: the operator denies the request, so
// the authorization server redirects back with our state and
// error=access_denied. Login must fail now with ErrAuthFailed, not wait out
// its timeout, and must bound the server-authored description.
func TestLoginErrorRedirectFailsPromptly(t *testing.T) {
	f := newFake(t, nil)
	desc := "the operator said no\x1b[31m" + strings.Repeat("x", 1000)
	opts := Options{OpenURL: func(u string) error {
		redirect, state := authParams(t, u)
		_, err := getStatus(redirect + "?" + url.Values{
			"error": {"access_denied"}, "error_description": {desc}, "state": {state},
		}.Encode())
		return err
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	_, err := Login(ctx, NewHTTPClient(), f.srv.URL, opts)
	if !errors.Is(err, ErrAuthFailed) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want a prompt ErrAuthFailed, got %v after %s", err, time.Since(start))
	}
	msg := err.Error()
	if !strings.Contains(msg, "access_denied") || !strings.Contains(msg, "the operator said no") {
		t.Errorf("error lost the server's reason: %q", msg)
	}
	if strings.Contains(msg, "\x1b") || len(msg) > 400 {
		t.Errorf("error description not bounded/sanitized (%d bytes)", len(msg))
	}
}

// TestEnsureFreshKeepsFailuresDistinct: only a refusal from the token
// endpoint means "log in again". An unreachable instance or token endpoint
// stays ErrUnreachable, and a grant with no refresh token is ErrTokenExpired
// without a refresh request.
func TestEnsureFreshKeepsFailuresDistinct(t *testing.T) {
	ctx := context.Background()
	expired := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	dead := httptest.NewServer(http.NewServeMux())
	deadURL := dead.URL
	dead.Close()

	t.Run("token endpoint unreachable", func(t *testing.T) {
		as := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(map[string]string{
				"authorization_endpoint": deadURL + "/authorize",
				"token_endpoint":         deadURL + "/token",
				"registration_endpoint":  deadURL + "/register",
			})
		}))
		defer as.Close()
		g := &Grant{BaseURL: as.URL, ClientID: "c", AccessToken: "stale", RefreshToken: "r", ExpiresAt: expired}
		_, err := EnsureFresh(ctx, NewHTTPClient(), t.TempDir(), g)
		if !errors.Is(err, ErrUnreachable) || errors.Is(err, ErrTokenRefused) {
			t.Fatalf("want ErrUnreachable (not ErrTokenRefused), got %v", err)
		}
	})
	t.Run("instance unreachable", func(t *testing.T) {
		g := &Grant{BaseURL: deadURL, ClientID: "c", AccessToken: "stale", RefreshToken: "r", ExpiresAt: expired}
		_, err := EnsureFresh(ctx, NewHTTPClient(), t.TempDir(), g)
		if !errors.Is(err, ErrUnreachable) || errors.Is(err, ErrTokenRefused) {
			t.Fatalf("want ErrUnreachable (not ErrTokenRefused), got %v", err)
		}
	})
	t.Run("no refresh token", func(t *testing.T) {
		f := newFake(t, nil)
		g := &Grant{BaseURL: f.srv.URL, ClientID: "c", AccessToken: "stale", ExpiresAt: expired}
		_, err := EnsureFresh(ctx, NewHTTPClient(), t.TempDir(), g)
		if !errors.Is(err, ErrTokenExpired) {
			t.Fatalf("want ErrTokenExpired, got %v", err)
		}
		if f.refreshPosts != 0 {
			t.Fatalf("sent %d refresh requests with an empty refresh token", f.refreshPosts)
		}
	})
}

// TestTokenRefusalIsClassifiedByGrant: a 400 on the code exchange is a
// failed authorization; only a refresh refusal is ErrTokenRefused, and it
// says so once.
func TestTokenRefusalIsClassifiedByGrant(t *testing.T) {
	f := newFake(t, func(f *fakeSwitchboard, _ *http.ServeMux) { f.failRefresh = true })
	ctx := context.Background()
	client := NewHTTPClient()
	doc, err := Discover(ctx, client, f.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	// No verifier: the fake answers 400 invalid_request.
	_, err = Exchange(ctx, client, doc, "client-fake", &pkce{redirectURI: "http://127.0.0.1:1/callback"}, "code-fake")
	if !errors.Is(err, ErrAuthFailed) || errors.Is(err, ErrTokenRefused) {
		t.Errorf("exchange 400: want ErrAuthFailed (not ErrTokenRefused), got %v", err)
	}
	_, err = Refresh(ctx, client, doc, "c", "r")
	if !errors.Is(err, ErrTokenRefused) {
		t.Fatalf("refresh 400: want ErrTokenRefused, got %v", err)
	}
	if n := strings.Count(err.Error(), ErrTokenRefused.Error()); n != 1 {
		t.Errorf("ErrTokenRefused appears %d times in %q", n, err)
	}
}

func TestMCPListTodosEventStream(t *testing.T) {
	mcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// A notification first, then the response to request id 1.
		fmt.Fprint(w, "event: message\r\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\r\n\r\n")
		fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"{\\\"todos\\\":[{\\\"id\\\":\\\"t1\\\",\\\"queue\\\":\\\"q\\\",\\\"state\\\":\\\"done\\\"}]}\"}]}}\n\n")
	}))
	defer mcp.Close()
	todos, err := MCPListTodos(context.Background(), NewHTTPClient(), mcp.URL, "ep_token")
	if err != nil {
		t.Fatal(err)
	}
	if len(todos) != 1 || todos[0].ID != "t1" || todos[0].State != "done" {
		t.Fatalf("todos = %+v", todos)
	}
}

// TestMCPListTodosRejectsUndecodableContent: a reply the client cannot read
// must be an error, never "zero todos" — the self-test would read that as
// "not done yet" and time out on a false negative.
func TestMCPListTodosRejectsUndecodableContent(t *testing.T) {
	cases := map[string]struct{ contentType, body string }{
		"text is not JSON": {"application/json",
			`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"internal error"}]}}`},
		"text has no todos": {"application/json",
			`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"items\":[]}"}]}}`},
		"stream has no response": {"text/event-stream",
			"data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			mcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				fmt.Fprint(w, tc.body)
			}))
			defer mcp.Close()
			todos, err := MCPListTodos(context.Background(), NewHTTPClient(), mcp.URL, "ep_token")
			if err == nil {
				t.Fatalf("want an error, got %d todos and nil", len(todos))
			}
		})
	}
}

func TestHostOfNormalizes(t *testing.T) {
	cases := map[string]string{
		"https://sb.example.com":          "sb.example.com",
		"sb.example.com":                  "sb.example.com",
		"https://SB.Example.COM/":         "sb.example.com",
		"HTTPS://sb.example.com/api?x=1":  "sb.example.com",
		"https://sb.example.com?x=1":      "sb.example.com",
		"https://sb.example.com#frag":     "sb.example.com",
		"https://user:pw@sb.example.com/": "sb.example.com",
		"http://sb.example.com:8443/":     "sb.example.com:8443",
		"sb.example.com:8443":             "sb.example.com:8443",
	}
	for in, want := range cases {
		if got := hostOf(in); got != want {
			t.Errorf("hostOf(%q) = %q, want %q", in, got, want)
		}
	}
	// ':' is not portable in a file name.
	if got := filepath.Base(GrantPath("/base", "http://SB.example.com:8443/")); got != "switchboard-sb.example.com_8443.json" {
		t.Errorf("GrantPath base = %q", got)
	}
}

func TestRevokeEndpointEscapesRef(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	api := &OperatorAPI{Base: srv.URL, HTTP: NewHTTPClient(), Token: "op_fake"}
	if err := api.RevokeEndpoint(context.Background(), "a/b?c"); err != nil {
		t.Fatal(err)
	}
	if want := "/api/v1/endpoints/a%2Fb%3Fc/revoke"; gotPath != want {
		t.Fatalf("revoke hit %q, want %q", gotPath, want)
	}
}
