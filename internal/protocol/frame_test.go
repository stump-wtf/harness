package protocol

// Governing: SPEC-0002 REQ "Message Framing" — the length+type+payload framing,
// and specifically the "mixed traffic on one connection" scenario (control and
// attach frames interleave without corrupting either stream).

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
)

// pipeConn is an in-memory ReadWriteCloser backed by a bytes.Buffer, enough to
// round-trip frames through Conn without a socket.
type bufConn struct {
	*bytes.Buffer
}

func (bufConn) Close() error { return nil }

func TestFrameRoundTrip(t *testing.T) {
	tests := []struct {
		name    string
		typ     Type
		payload []byte
	}{
		{"empty payload ping", TypePing, nil},
		{"control json", TypeControlReq, []byte(`{"op":"list"}`)},
		{"attach raw bytes", TypeAttachData, EncodeAttach(7, []byte("\x1b[2Jhello"))},
		{"error frame", TypeError, []byte(`{"code":"unknown_harness"}`)},
		{"large-ish", TypeAttachData, bytes.Repeat([]byte("x"), 5000)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := NewConn(bufConn{&bytes.Buffer{}})
			if err := c.WriteFrame(tc.typ, tc.payload); err != nil {
				t.Fatalf("WriteFrame: %v", err)
			}
			got, err := c.ReadFrame()
			if err != nil {
				t.Fatalf("ReadFrame: %v", err)
			}
			if got.Type != tc.typ {
				t.Errorf("type = %s, want %s", got.Type, tc.typ)
			}
			if !bytes.Equal(got.Payload, tc.payload) {
				t.Errorf("payload = %q, want %q", got.Payload, tc.payload)
			}
		})
	}
}

// TestMixedTrafficOneConnection is SPEC-0002 REQ "Message Framing" scenario
// "Mixed traffic on one connection": an attach stream and a control request
// share the wire and neither corrupts the other. We write an interleaved
// sequence and confirm every frame reads back exactly, in order.
func TestMixedTrafficOneConnection(t *testing.T) {
	c := NewConn(bufConn{&bytes.Buffer{}})
	seq := []Frame{
		{TypeAttachData, EncodeAttach(1, []byte("output-chunk-A"))},
		{TypeControlReq, []byte(`{"id":9,"op":"start","name":"web"}`)},
		{TypeAttachData, EncodeAttach(1, []byte("output-chunk-B"))},
		{TypeControlResp, []byte(`{"id":9,"op":"start"}`)},
		{TypeAttachData, EncodeAttach(2, []byte("second-session"))},
	}
	for _, f := range seq {
		if err := c.WriteFrame(f.Type, f.Payload); err != nil {
			t.Fatalf("write %s: %v", f.Type, err)
		}
	}
	for i, want := range seq {
		got, err := c.ReadFrame()
		if err != nil {
			t.Fatalf("read #%d: %v", i, err)
		}
		if got.Type != want.Type || !bytes.Equal(got.Payload, want.Payload) {
			t.Fatalf("frame #%d = (%s,%q), want (%s,%q)", i, got.Type, got.Payload, want.Type, want.Payload)
		}
		if got.Type == TypeAttachData {
			// The session tag survives the round trip so multiplexing works.
			if _, _, err := DecodeAttach(got.Payload); err != nil {
				t.Errorf("frame #%d attach decode: %v", i, err)
			}
		}
	}
}

func TestAttachSessionTagging(t *testing.T) {
	payload := EncodeAttach(0xDEADBEEF, []byte("bytes"))
	id, rest, err := DecodeAttach(payload)
	if err != nil {
		t.Fatalf("DecodeAttach: %v", err)
	}
	if id != 0xDEADBEEF {
		t.Errorf("session id = %#x, want 0xDEADBEEF", id)
	}
	if string(rest) != "bytes" {
		t.Errorf("rest = %q, want bytes", rest)
	}
	if _, _, err := DecodeAttach([]byte{1, 2}); err == nil {
		t.Error("DecodeAttach on short payload: want error")
	}
}

func TestReadFrameRejectsOversize(t *testing.T) {
	var buf bytes.Buffer
	var lb [4]byte
	binary.BigEndian.PutUint32(lb[:], MaxFrameSize+1)
	buf.Write(lb[:])
	c := NewConn(bufConn{&buf})
	if _, err := c.ReadFrame(); err == nil {
		t.Fatal("want error on oversize length, got nil")
	}
}

func TestReadFrameRejectsZeroLength(t *testing.T) {
	var buf bytes.Buffer
	var lb [4]byte
	binary.BigEndian.PutUint32(lb[:], 0)
	buf.Write(lb[:])
	c := NewConn(bufConn{&buf})
	if _, err := c.ReadFrame(); err == nil {
		t.Fatal("want error on zero length, got nil")
	}
}

func TestReadFrameEOF(t *testing.T) {
	c := NewConn(bufConn{&bytes.Buffer{}})
	if _, err := c.ReadFrame(); err != io.EOF {
		t.Fatalf("ReadFrame on empty = %v, want io.EOF", err)
	}
}

func TestMajor(t *testing.T) {
	tests := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{"1.0", 1, false},
		{"2.7", 2, false},
		{"1", 1, false},
		{"", 0, true},
		{"x.y", 0, true},
	}
	for _, tc := range tests {
		got, err := Major(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("Major(%q) err = %v, wantErr %v", tc.in, err, tc.wantErr)
		}
		if err == nil && got != tc.want {
			t.Errorf("Major(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
