package switchboard

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Grant is the persisted OAuth grant for one Switchboard instance: what
// `switchboard login` produces and what every later API call consumes. It
// lives at $XDG_CONFIG_HOME/harness/credentials/switchboard-<host>.json,
// mode 0600 (SPEC-0018 REQ-7; ADR-0008 treats it as a secret file).
type Grant struct {
	BaseURL      string `json:"base_url"`
	ClientID     string `json:"client_id"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresAt    string `json:"expires_at,omitempty"` // RFC 3339
	Resource     string `json:"resource,omitempty"`
}

// GrantPath is the credential file for base's host.
func GrantPath(baseDir, baseURL string) string {
	// ':' (a port, an IPv6 literal) is not portable in a file name.
	host := strings.ReplaceAll(hostOf(baseURL), ":", "_")
	return filepath.Join(baseDir, "credentials", "switchboard-"+host+".json")
}

// SaveGrant writes the grant atomically at mode 0600. A looser file is a
// leaked operator credential, so the mode is not negotiable and the test
// suite pins it.
func SaveGrant(baseDir string, g *Grant) error {
	path := GrantPath(baseDir, g.BaseURL)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("switchboard: create credentials dir: %w", err)
	}
	raw, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return err
	}
	// Write a temp file, then rename over the grant: a pre-existing grant is
	// replaced wholesale, never appended to or truncated in place. The
	// Chmod covers a stale temp file left at a looser mode by a crash, which
	// os.WriteFile would reuse without changing its mode.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("switchboard: write grant: %w", err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("switchboard: install grant: %w", err)
	}
	return nil
}

// LoadGrant reads the grant for baseURL.
func LoadGrant(baseDir, baseURL string) (*Grant, error) {
	raw, err := os.ReadFile(GrantPath(baseDir, baseURL))
	if err != nil {
		return nil, fmt.Errorf("switchboard: no saved grant (%w); run the login", err)
	}
	g := &Grant{}
	if err := json.Unmarshal(raw, g); err != nil {
		return nil, fmt.Errorf("switchboard: saved grant is corrupt: %w", err)
	}
	return g, nil
}

// Expiring reports whether the access token is within refresh distance of
// expiry (or already past it).
func (g *Grant) Expiring(now time.Time) bool {
	if g.ExpiresAt == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, g.ExpiresAt)
	if err != nil {
		return true
	}
	return now.After(t.Add(-2 * time.Minute))
}

// hostOf is the lowercased host[:port] of a base URL, so
// "https://SB.example.com/", "sb.example.com" and "HTTPS://sb.example.com?x"
// share one credential file while different ports do not. Scheme-less input
// is parsed as https; userinfo, path, query and fragment are dropped.
func hostOf(raw string) string {
	s := strings.TrimSpace(raw)
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	if u, err := url.Parse(s); err == nil && u.Host != "" {
		return strings.ToLower(u.Host)
	}
	// Unparseable: keep what precedes the first '/', '?' or '#' after the
	// scheme, so a bad base URL still maps to one file and never to a path.
	s = s[strings.Index(s, "://")+len("://"):]
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	return strings.ToLower(s)
}
