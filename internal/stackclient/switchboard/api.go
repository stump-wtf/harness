package switchboard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Endpoint is one operator API endpoint record: the per-persona
// queue+token binding `harness init` vends.
type Endpoint struct {
	// Ref is the endpoint's stable reference (id or slug) used to revoke.
	Ref     string `json:"ref"`
	Name    string `json:"name"`
	Queue   string `json:"queue"`
	Token   string `json:"token,omitempty"` // present only on vend; never logged (REQ-29)
	Ingest  string `json:"ingest_url,omitempty"`
	BaseURL string `json:"base_url,omitempty"`
}

// OperatorAPI is the authenticated operator-API client.
type OperatorAPI struct {
	Base string
	HTTP *http.Client
	// Token is the operator's access token. It rides Authorization only; it
	// never appears in an error, a log line, or dry-run output (REQ-29).
	Token string
}

func (a *OperatorAPI) newRequest(ctx context.Context, method, path string, body string) (*http.Request, error) {
	// A nil io.Reader (not a nil *strings.Reader) for no body, so the
	// request really has none.
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(a.Base, "/")+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+a.Token)
	req.Header.Set("Accept", "application/json")
	return req, nil
}

// ListEndpoints returns the signed-in operator's endpoints (used to find a
// persona's endpoint before vend: an unchanged re-run must make NO mutating
// call, REQ-6).
func (a *OperatorAPI) ListEndpoints(ctx context.Context) ([]Endpoint, error) {
	req, err := a.newRequest(ctx, http.MethodGet, "/api/v1/endpoints", "")
	if err != nil {
		return nil, err
	}
	raw, err := doWithRetry(ctx, a.HTTP, req)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Endpoints []Endpoint `json:"endpoints"`
	}
	if err := unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	return doc.Endpoints, nil
}

// FindEndpoint returns the operator's endpoint named name, or nil.
func (a *OperatorAPI) FindEndpoint(ctx context.Context, name string) (*Endpoint, error) {
	eps, err := a.ListEndpoints(ctx)
	if err != nil {
		return nil, err
	}
	for i := range eps {
		if eps[i].Name == name {
			return &eps[i], nil
		}
	}
	return nil, nil
}

// VendEndpoint mints (or re-vends, for --rotate) the endpoint {name, queue}.
// Exactly one POST per call; the token lives ONLY in the returned struct.
func (a *OperatorAPI) VendEndpoint(ctx context.Context, name, queue string) (*Endpoint, error) {
	if name == "" || queue == "" {
		return nil, errors.New("switchboard: endpoint name and queue are required")
	}
	req, err := a.newRequest(ctx, http.MethodPost, "/api/v1/endpoints", `{"name":`+jsonString(name)+`,"queue":`+jsonString(queue)+`}`)
	if err != nil {
		return nil, err
	}
	var ep Endpoint
	if err := fetchInto(a.HTTP, req, &ep); err != nil {
		var se *statusError
		if errors.As(err, &se) && se.status == http.StatusConflict {
			return nil, fmt.Errorf("%w: %s", ErrAlreadyExists, se.bodySummary())
		}
		return nil, err
	}
	return &ep, nil
}

// RevokeEndpoint kills the endpoint's current credential, for --rotate: the
// rotate flow is revoke, re-vend, replace the token (REQ-6).
func (a *OperatorAPI) RevokeEndpoint(ctx context.Context, ref string) error {
	if ref == "" {
		return errors.New("switchboard: endpoint ref is required to revoke")
	}
	// PathEscape: a ref is server-issued, but a '/' or '?' in it must not
	// retarget the request at another path.
	req, err := a.newRequest(ctx, http.MethodPost, "/api/v1/endpoints/"+url.PathEscape(ref)+"/revoke", "")
	if err != nil {
		return err
	}
	_, err = doWithRetry(ctx, a.HTTP, req)
	return err
}
