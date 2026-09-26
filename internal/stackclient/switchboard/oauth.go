// Package switchboard is the only code in Harness that talks to Switchboard,
// and only the CLI uses it: the daemon never calls Switchboard (ADR-0019 /
// ADR-0021 as narrowed by ADR-0024). It covers OAuth-as-the-user (RFC 8414
// discovery, RFC 7591 dynamic client registration, authorization code + PKCE
// S256 with resource binding), the operator endpoint API, and an MCP
// list_todos read for the self-test.
//
// Governing: ADR-0024, SPEC-0018 REQ-7; SPEC-0018 "Security Requirements".
package switchboard

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Sentinels (SPEC-0018 REQ-29). Never formatted with a token inside.
var (
	ErrUnreachable   = errors.New("switchboard: instance unreachable")
	ErrAuthFailed    = errors.New("switchboard: authorization failed")
	ErrTokenExpired  = errors.New("switchboard: grant expired; re-run the login")
	ErrTokenRefused  = errors.New("switchboard: token refresh refused; re-run the login")
	ErrAPI           = errors.New("switchboard: operator API error")
	ErrAlreadyExists = errors.New("switchboard: endpoint already exists")
)

// discoveryDocument is the RFC 8414 / OAuth 2.0 Authorization Server
// Metadata subset this client needs.
type discoveryDocument struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	RegistrationEndpoint  string `json:"registration_endpoint"`
	// Resource is the RFC 8703-style resource indicator the grant binds:
	// <base>/api. Authorizing against it is what makes the grant the
	// OPERATOR's rather than an instance-global credential (REQ-27).
	Resource string `json:"resource"`
}

// Discover fetches the authorization-server metadata from
// <base>/.well-known/oauth-authorization-server.
func Discover(ctx context.Context, client *http.Client, baseURL string) (*discoveryDocument, error) {
	base := strings.TrimSuffix(baseURL, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/.well-known/oauth-authorization-server", nil)
	if err != nil {
		return nil, err
	}
	doc := &discoveryDocument{}
	if err := fetchJSON(client, req, doc); err != nil {
		return nil, fmt.Errorf("%w: discovery: %w", ErrUnreachable, err)
	}
	if doc.AuthorizationEndpoint == "" || doc.TokenEndpoint == "" || doc.RegistrationEndpoint == "" {
		return nil, fmt.Errorf("%w: discovery document at %s is missing an endpoint", ErrAuthFailed, baseURL)
	}
	if doc.Resource == "" {
		doc.Resource = base + "/api"
	}
	return doc, nil
}

// registration is the RFC 7591 dynamic-client-registration response.
type registration struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret,omitempty"`
}

// RegisterClient dynamically registers this CLI as an OAuth client, named
// for the host it runs on so the instance's authorizations page says which
// machine asked. redirectURI is the loopback listener's exact URI, port
// included: an authorization server that matches redirect URIs exactly
// (RFC 6749 §3.1.2.3) rejects an authorize request whose redirect_uri differs
// from the registered one.
func RegisterClient(ctx context.Context, client *http.Client, doc *discoveryDocument, hostname, redirectURI string) (*registration, error) {
	body, _ := json.Marshal(map[string]any{
		"client_name":                fmt.Sprintf("harness (%s)", hostname),
		"redirect_uris":              []string{redirectURI},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
		"scope":                      "operator",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, doc.RegistrationEndpoint, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	reg := &registration{}
	if err := fetchJSON(client, req, reg); err != nil {
		return nil, fmt.Errorf("%w: client registration: %w", ErrAuthFailed, err)
	}
	if reg.ClientID == "" {
		return nil, fmt.Errorf("%w: client registration returned no client_id", ErrAuthFailed)
	}
	return reg, nil
}

// pkce holds the S256 verifier/challenge pair (RFC 7636), the state, and the
// loopback redirect URI (set by Login once the listener has bound its port).
type pkce struct {
	verifier    string
	challenge   string
	redirectURI string
	state       string
}

func newPKCE() (*pkce, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	state := make([]byte, 16)
	if _, err := rand.Read(state); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	return &pkce{
		verifier:  base64.RawURLEncoding.EncodeToString(raw),
		challenge: base64.RawURLEncoding.EncodeToString(sum[:]),
		state:     base64.RawURLEncoding.EncodeToString(state),
	}, nil
}

// AuthURL builds the authorization redirect target: code + PKCE S256, with
// resource = <base>/api so the grant binds the operator (REQ-7).
func (p *pkce) AuthURL(doc *discoveryDocument, clientID string) string {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", p.redirectURI)
	q.Set("scope", "operator")
	q.Set("state", p.state)
	q.Set("code_challenge", p.challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("resource", doc.Resource)
	return doc.AuthorizationEndpoint + "?" + q.Encode()
}

// tokenResponse is the token endpoint's reply, shared by the code exchange
// and the refresh.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresIn    int    `json:"expires_in,omitempty"`
	TokenType    string `json:"token_type"`
}

// Exchange swaps the authorization code for tokens.
func Exchange(ctx context.Context, client *http.Client, doc *discoveryDocument, clientID string, p *pkce, code string) (*tokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", clientID)
	form.Set("code", code)
	form.Set("redirect_uri", p.redirectURI)
	form.Set("code_verifier", p.verifier)
	form.Set("resource", doc.Resource)
	return tokenRequest(ctx, client, doc.TokenEndpoint, form)
}

// Refresh renews an expiring grant. A refusal (400/401 from the token
// endpoint) means the operator revoked it or it expired server-side, and is
// ErrTokenRefused: the caller re-runs the login. A transport failure stays
// ErrUnreachable, because the grant itself may be fine.
func Refresh(ctx context.Context, client *http.Client, doc *discoveryDocument, clientID, refreshToken string) (*tokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("client_id", clientID)
	form.Set("refresh_token", refreshToken)
	form.Set("resource", doc.Resource)
	tok, err := tokenRequest(ctx, client, doc.TokenEndpoint, form)
	if err != nil {
		var se *statusError
		if errors.As(err, &se) && (se.status == http.StatusBadRequest || se.status == http.StatusUnauthorized) {
			return nil, fmt.Errorf("%w: %w", ErrTokenRefused, se)
		}
		return nil, err
	}
	return tok, nil
}

// tokenRequest posts form to the token endpoint. What a refusal means depends
// on the grant (a failed authorization on the code exchange, a refused grant
// on a refresh), so it returns transport failures as ErrUnreachable and
// everything else as ErrAuthFailed wrapping the *statusError; Refresh
// reclassifies its own refusals.
func tokenRequest(ctx context.Context, client *http.Client, endpoint string, form url.Values) (*tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp := &tokenResponse{}
	if err := fetchJSON(client, req, resp); err != nil {
		if errors.Is(err, ErrUnreachable) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: token endpoint: %w", ErrAuthFailed, err)
	}
	if resp.AccessToken == "" {
		return nil, fmt.Errorf("%w: token endpoint returned no access token", ErrAuthFailed)
	}
	return resp, nil
}

// expiryAt converts ExpiresIn to an absolute time; a token without expiry is
// treated as stale after an hour so refresh still runs.
func (t *tokenResponse) expiryAt(now time.Time) time.Time {
	if t.ExpiresIn <= 0 {
		return now.Add(time.Hour)
	}
	return now.Add(time.Duration(t.ExpiresIn) * time.Second)
}
