package switchboard

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"runtime"
	"time"
)

// Options configure the interactive login.
type Options struct {
	BaseURL string
	// NoBrowser prints the authorization URL instead of opening one; the
	// redirect must then reach this machine (SSH port forward).
	NoBrowser bool
	// OpenURL is the exec seam tests record instead of launching a browser.
	OpenURL func(url string) error
	// Hostname names this machine in the registered client.
	Hostname string
}

// Login runs the full OAuth-as-the-user flow and returns the grant to save.
// It performs: discovery (RFC 8414), dynamic client registration (RFC 7591),
// authorization code + PKCE S256 with resource binding, and the token
// exchange. Tokens live only in the returned Grant and the credential file.
func Login(ctx context.Context, client *http.Client, base string, opts Options) (*Grant, error) {
	doc, err := Discover(ctx, client, base)
	if err != nil {
		return nil, err
	}
	p, err := newPKCE()
	if err != nil {
		return nil, err
	}
	// Bind the loopback BEFORE registering: the registered redirect URI must
	// be the exact one (ephemeral port included) that the authorize and
	// token requests carry, or an exact-matching server refuses the login.
	cb, err := startLoopback(p.state)
	if err != nil {
		return nil, err
	}
	// close is idempotent: this covers every error return below, and wait
	// has already closed it on the happy path.
	defer func() { _ = cb.close() }()
	p.redirectURI = cb.redirectURI()
	reg, err := RegisterClient(ctx, client, doc, opts.Hostname, p.redirectURI)
	if err != nil {
		return nil, err
	}
	authURL := p.AuthURL(doc, reg.ClientID)
	if opts.NoBrowser {
		fmt.Printf("Open this URL to authorize harness:\n\n  %s\n\nThen: %s\n", authURL, fmt.Sprintf(noBrowserHint, cb.port, cb.port))
	} else if opts.OpenURL != nil {
		if err := opts.OpenURL(authURL); err != nil {
			return nil, fmt.Errorf("%w: open browser: %w", ErrAuthFailed, err)
		}
	} else if err := openBrowser(authURL); err != nil {
		fmt.Printf("Open this URL to authorize harness:\n\n  %s\n", authURL)
	}
	code, err := cb.wait(ctx)
	if err != nil {
		return nil, err
	}
	tok, err := Exchange(ctx, client, doc, reg.ClientID, p, code)
	if err != nil {
		return nil, err
	}
	return &Grant{
		BaseURL:      base,
		ClientID:     reg.ClientID,
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    tok.expiryAt(time.Now()).UTC().Format(time.RFC3339),
		Resource:     doc.Resource,
	}, nil
}

// EnsureFresh returns an access token, refreshing first when the grant is
// near expiry. Only a refused refresh is ErrTokenRefused (re-run the login);
// an expiring grant with no refresh token is ErrTokenExpired, and an
// unreachable instance stays ErrUnreachable, because telling the operator
// to log in again over a network blip sends them the wrong way.
func EnsureFresh(ctx context.Context, client *http.Client, baseDir string, g *Grant) (string, error) {
	if !g.Expiring(time.Now()) {
		return g.AccessToken, nil
	}
	if g.RefreshToken == "" {
		return "", ErrTokenExpired
	}
	doc, err := Discover(ctx, client, g.BaseURL)
	if err != nil {
		return "", fmt.Errorf("switchboard: refresh grant: %w", err)
	}
	tok, err := Refresh(ctx, client, doc, g.ClientID, g.RefreshToken)
	if err != nil {
		return "", fmt.Errorf("switchboard: refresh grant: %w", err)
	}
	g.AccessToken = tok.AccessToken
	g.RefreshToken = firstNonEmpty(tok.RefreshToken, g.RefreshToken)
	g.ExpiresAt = tok.expiryAt(time.Now()).UTC().Format(time.RFC3339)
	if err := SaveGrant(baseDir, g); err != nil {
		return "", err
	}
	return g.AccessToken, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// openBrowser is best-effort; a headless host prints the URL via the
// fallback branch in Login.
func openBrowser(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()
	case "linux":
		return exec.Command("xdg-open", url).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	default:
		return errors.New("no browser opener for this platform")
	}
}
