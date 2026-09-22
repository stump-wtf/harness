package channel

// SSE decoder tests, plus the fuzz target.
//
// The decoder is the only thing between a network socket and the JSON-RPC
// layer, and its failure modes are quiet: a mis-framed field turns one
// doorbell into none, or into two. So the table below is mostly about the
// awkward shapes of the grammar rather than the happy path, and the fuzz
// target exists because a hostile stream is exactly what an unauthenticated
// reader is exposed to.
//
// Governing: ADR-0021; SPEC-0014 REQ "Channel Listener Session".
//
// @joestump 09/23/2026 - Introduced with the SPEC-0014 channel listener (#471).

import (
	"strings"
	"testing"
)

func decodeAll(t *testing.T, in string) []Event {
	t.Helper()
	d := NewDecoder(strings.NewReader(in))
	var out []Event
	for {
		ev, ok := d.Next()
		if !ok {
			break
		}
		out = append(out, ev)
	}
	if err := d.Err(); err != nil {
		t.Fatalf("decode %q: %v", in, err)
	}
	return out
}

func TestDecoderGrammar(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []Event
	}{
		{
			name: "one event",
			in:   "data: hello\n\n",
			want: []Event{{Data: "hello"}},
		},
		{
			// The one that matters most: a JSON payload the server chose to
			// wrap arrives as several data lines, and joining them with "\n"
			// is what makes it parse.
			name: "multi-line data joins with newlines",
			in:   "data: {\ndata:   \"a\": 1\ndata: }\n\n",
			want: []Event{{Data: "{\n  \"a\": 1\n}"}},
		},
		{
			name: "exactly one leading space is stripped",
			in:   "data:  two spaces\n\n",
			want: []Event{{Data: " two spaces"}},
		},
		{
			name: "no space at all",
			in:   "data:none\n\n",
			want: []Event{{Data: "none"}},
		},
		{
			name: "event and id fields",
			in:   "event: message\nid: 7\ndata: x\n\n",
			want: []Event{{ID: "7", Name: "message", Data: "x"}},
		},
		{
			// The SSE grammar says an event with no id: keeps the last one
			// seen, which is what makes Last-Event-ID resumption work across
			// a burst.
			name: "an id persists to later events",
			in:   "id: 7\ndata: a\n\ndata: b\n\n",
			want: []Event{{ID: "7", Data: "a"}, {ID: "7", Data: "b"}},
		},
		{
			name: "comments and keep-alives fire nothing",
			in:   ": keep-alive\n\n\ndata: x\n\n",
			want: []Event{{Data: "x"}},
		},
		{
			name: "CRLF line endings",
			in:   "data: x\r\n\r\n",
			want: []Event{{Data: "x"}},
		},
		{
			name: "a field with no colon has an empty value",
			in:   "data\ndata: x\n\n",
			want: []Event{{Data: "\nx"}},
		},
		{
			name: "unknown fields are ignored",
			in:   "retry: 1000\nfoo: bar\ndata: x\n\n",
			want: []Event{{Data: "x"}},
		},
		{
			name: "an unterminated event is not dispatched",
			in:   "data: x\n",
			want: nil,
		},
		{
			name: "empty input",
			in:   "",
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decodeAll(t, tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("decoded %d events, want %d: %+v", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("event %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestDecoderTracksLastID(t *testing.T) {
	d := NewDecoder(strings.NewReader("id: a\ndata: 1\n\nid: b\ndata: 2\n\n"))
	if _, ok := d.Next(); !ok {
		t.Fatal("no first event")
	}
	if d.LastID() != "a" {
		t.Errorf("LastID after the first event = %q", d.LastID())
	}
	if _, ok := d.Next(); !ok {
		t.Fatal("no second event")
	}
	if d.LastID() != "b" {
		t.Errorf("LastID after the second event = %q", d.LastID())
	}
}

// FuzzDecoder drives arbitrary bytes through the decoder. The property is
// modest and exactly right for a parser on an untrusted stream: it must not
// panic, and it must terminate. A hostile server is the only thing between
// this decoder and the daemon's process.
//
// Run longer with: go test ./internal/trigger/channel -fuzz FuzzDecoder
func FuzzDecoder(f *testing.F) {
	seeds := []string{
		"data: hello\n\n",
		"event: message\nid: 1\ndata: {\"jsonrpc\":\"2.0\"}\n\n",
		": comment\n\n",
		"data\n\n",
		"id: \x00bad\ndata: x\n\n",
		"data: a\ndata: b\ndata: c\n\n",
		strings.Repeat("data: x\n", 64) + "\n",
		"\r\n\r\n\r\n",
		"data:",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		d := NewDecoder(strings.NewReader(in))
		for n := 0; ; n++ {
			if _, ok := d.Next(); !ok {
				break
			}
			if n > len(in)+16 {
				// More events than the input could possibly frame means the
				// decoder is not consuming — the shape a parser bug takes
				// when it turns into a hang rather than a crash.
				t.Fatalf("decoder produced more events than the input can frame (%d from %d bytes)", n, len(in))
			}
		}
		_ = d.Err()
	})
}
