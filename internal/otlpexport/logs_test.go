package otlpexport

// Logs, Transport and Result Tests
//
// Governing tests: SPEC-0014 REQ-6 (the LogRecord mapping), REQ-10 (response
// classification, Retry-After, partialSuccess, gzip, timeout).
//
// @joestump-agent 09/21/2026 - Added for harness#391.

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSendLogsWireShape(t *testing.T) {
	var body []byte
	var ctype string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/logs" {
			t.Errorf("path = %s", r.URL.Path)
		}
		ctype = r.Header.Get("Content-Type")
		body, _ = io.ReadAll(r.Body)
	}))
	defer srv.Close()

	at := time.Date(2026, 9, 21, 10, 0, 0, 5, time.UTC)
	res := SendLogs(context.Background(), Endpoint{URL: srv.URL + "/v1/logs"},
		Resource{Attributes: []KeyValue{{"service.name", "harness"}}},
		Scope{Name: "github.com/stump-wtf/harness/telemetry", Version: "v1"},
		[]LogRecord{{
			Time: at, ObservedTime: at.Add(time.Second),
			SeverityNumber: 17, SeverityText: "ERROR", Body: "quota exhausted",
			Attributes: []KeyValue{{"agent.tool.is_error", false}, {"agent.model", ""}, {"agent.item.seq", 7}, {"agent.targets", []string{"a.go"}}},
			TraceID:    "0123456789abcdef0123456789abcdef", SpanID: "0123456789abcdef",
		}})
	if !res.OK() {
		t.Fatalf("send: %+v", res)
	}
	if ctype != "application/json" {
		t.Fatalf("content-type %q", ctype)
	}
	// Decode generically: the assertion is on the bytes a collector reads.
	var got struct {
		ResourceLogs []struct {
			Resource struct {
				Attributes []map[string]any `json:"attributes"`
			} `json:"resource"`
			ScopeLogs []struct {
				Scope      map[string]string `json:"scope"`
				LogRecords []map[string]any  `json:"logRecords"`
			} `json:"scopeLogs"`
		} `json:"resourceLogs"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	sl := got.ResourceLogs[0].ScopeLogs[0]
	if sl.Scope["name"] != "github.com/stump-wtf/harness/telemetry" {
		t.Fatalf("scope %v", sl.Scope)
	}
	rec := sl.LogRecords[0]
	if rec["timeUnixNano"] != strconv.FormatInt(at.UnixNano(), 10) || rec["observedTimeUnixNano"] != strconv.FormatInt(at.Add(time.Second).UnixNano(), 10) {
		t.Fatalf("timestamps must be decimal strings: %v / %v", rec["timeUnixNano"], rec["observedTimeUnixNano"])
	}
	if rec["severityNumber"] != float64(17) || rec["severityText"] != "ERROR" {
		t.Fatalf("severity %v %v", rec["severityNumber"], rec["severityText"])
	}
	if b := rec["body"].(map[string]any); b["stringValue"] != "quota exhausted" {
		t.Fatalf("body %v", b)
	}
	if rec["traceId"] != "0123456789abcdef0123456789abcdef" || rec["spanId"] != "0123456789abcdef" {
		t.Fatalf("ids %v %v", rec["traceId"], rec["spanId"])
	}
	attrs := map[string]map[string]any{}
	for _, a := range rec["attributes"].([]any) {
		kv := a.(map[string]any)
		attrs[kv["key"].(string)] = kv["value"].(map[string]any)
	}
	// Every AnyValue carries exactly one field, zero values included.
	for key, want := range map[string]string{
		"agent.tool.is_error": "boolValue", "agent.model": "stringValue",
		"agent.item.seq": "intValue", "agent.targets": "arrayValue",
	} {
		v := attrs[key]
		if len(v) != 1 {
			t.Errorf("%s AnyValue = %v, want exactly one field", key, v)
		}
		if _, ok := v[want]; !ok {
			t.Errorf("%s AnyValue = %v, want %s", key, v, want)
		}
	}
	if attrs["agent.tool.is_error"]["boolValue"] != false || attrs["agent.item.seq"]["intValue"] != "7" {
		t.Fatalf("values %v", attrs)
	}
}

func TestSendClassifiesResponses(t *testing.T) {
	for _, tc := range []struct {
		status    int
		retryable bool
	}{
		{200, false}, {400, false}, {401, false}, {403, false}, {404, false}, {500, false},
		{429, true}, {502, true}, {503, true}, {504, true},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(tc.status) }))
		r := SendLogs(context.Background(), Endpoint{URL: srv.URL}, Resource{}, Scope{}, nil)
		srv.Close()
		if r.OK() != (tc.status == 200) || r.Retryable != tc.retryable || r.StatusCode != tc.status {
			t.Errorf("%d: ok=%v retryable=%v status=%d", tc.status, r.OK(), r.Retryable, r.StatusCode)
		}
		if tc.status != 200 && r.Class() != "http_"+strconv.Itoa(tc.status) {
			t.Errorf("%d: class %q", tc.status, r.Class())
		}
	}
}

func TestSendNetworkErrorIsRetryableAndHidesTheURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL + "/v1/logs?api_key=supersecretvalue"
	srv.Close() // connection refused from here on
	r := SendLogs(context.Background(), Endpoint{URL: url, Headers: map[string]string{"Authorization": "Bearer hunter2hunter2"}}, Resource{}, Scope{}, nil)
	if r.OK() || !r.Retryable || r.Class() != "network" {
		t.Fatalf("result %+v", r)
	}
	if msg := r.Err.Error(); strings.Contains(msg, "supersecretvalue") || strings.Contains(msg, "hunter2") {
		t.Fatalf("error leaks a credential: %q", msg)
	}
}

func TestSendTimeoutIsRetryable(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	start := time.Now()
	r := SendLogs(context.Background(), Endpoint{URL: srv.URL, Timeout: 50 * time.Millisecond}, Resource{}, Scope{}, nil)
	if r.OK() || !r.Retryable {
		t.Fatalf("result %+v", r)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("per-request timeout not applied")
	}
}

func TestSendRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	r := SendLogs(context.Background(), Endpoint{URL: srv.URL}, Resource{}, Scope{}, nil)
	if r.RetryAfter != 7*time.Second || !r.Retryable {
		t.Fatalf("result %+v", r)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Duration{
		"":                              0,
		"7":                             7 * time.Second,
		"0":                             0,
		"-3":                            0,
		"soon":                          0,
		"Mon, 21 Sep 2026 10:00:30 GMT": 30 * time.Second,
		"Mon, 21 Sep 2026 09:00:00 GMT": 0,
	} {
		if got := ParseRetryAfter(in, now); got != want {
			t.Errorf("%q = %s, want %s", in, got, want)
		}
	}
}

func TestPartialSuccess(t *testing.T) {
	for _, body := range []string{
		`{"partialSuccess":{"rejectedLogRecords":"3","errorMessage":"too big"}}`,
		`{"partialSuccess":{"rejectedLogRecords":3,"errorMessage":"too big"}}`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }))
		r := SendLogs(context.Background(), Endpoint{URL: srv.URL}, Resource{}, Scope{}, nil)
		srv.Close()
		if !r.OK() || r.Rejected != 3 || r.ErrorMessage != "too big" {
			t.Errorf("%s: %+v", body, r)
		}
	}
}

func TestSendGzip(t *testing.T) {
	var plain []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Encoding") != "gzip" {
			t.Errorf("content-encoding %q", r.Header.Get("Content-Encoding"))
		}
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		plain, _ = io.ReadAll(zr)
	}))
	defer srv.Close()
	r := SendLogs(context.Background(), Endpoint{URL: srv.URL, Gzip: true}, Resource{}, Scope{}, []LogRecord{{Body: "zipped"}})
	if !r.OK() || !strings.Contains(string(plain), "zipped") {
		t.Fatalf("result %+v body %s", r, plain)
	}
}
