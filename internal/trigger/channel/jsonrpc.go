package channel

// The JSON-RPC layer: just enough to hold a listen-only MCP session.
//
// A server on the other end may send three things down the stream: the
// notifications we are here for, a `ping` request it expects an answer to, and
// anything else its implementation decides to ask. The third case is the one
// worth being explicit about — answering `-32601` rather than staying silent
// is what lets the server distinguish a client that cannot do something from a
// client that has stopped reading.
//
// Governing: ADR-0021; SPEC-0014 REQ "Channel Listener Session", REQ "Channel
// Notification Handling".
//
// @joestump 09/23/2026 - Introduced with the SPEC-0014 channel listener (#471).

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// jsonrpcVersion is the only version this client speaks.
const jsonrpcVersion = "2.0"

// methodNotFound is the JSON-RPC code for an unimplemented method.
const methodNotFound = -32601

// message is one JSON-RPC frame, in either direction. The shape is
// deliberately loose: ID present plus Method is a request, Method alone is a
// notification, and ID plus Result or Error is a response.
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// rpcError is a JSON-RPC error object.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("jsonrpc error %d: %s", e.Code, e.Message) }

// isRequest reports whether m expects a reply: a method AND an id. A
// notification carries a method and no id, and a response carries an id and no
// method.
func (m message) isRequest() bool { return m.Method != "" && len(m.ID) > 0 }

// isNotification reports whether m is a notification.
func (m message) isNotification() bool { return m.Method != "" && len(m.ID) == 0 }

// emptyResult replies to a request with an empty result, which is what `ping`
// wants.
func emptyResult(id json.RawMessage) message {
	return message{JSONRPC: jsonrpcVersion, ID: id, Result: json.RawMessage(`{}`)}
}

// notImplemented replies to a request this client does not serve.
func notImplemented(id json.RawMessage, method string) message {
	return message{
		JSONRPC: jsonrpcVersion,
		ID:      id,
		Error:   &rpcError{Code: methodNotFound, Message: "harness is a listen-only channel client and does not implement " + method},
	}
}

// ChannelNotification is the method a doorbell arrives as.
const ChannelNotification = "notifications/claude/channel"

// maxParamsBytes caps a notification's serialized params (REQ "Channel
// Notification Handling"). A doorbell is counts, ids and a line of prose; 64
// KiB is far above any honest one, and a server that sends more is either
// broken or trying something.
const maxParamsBytes = 64 * 1024

// channelParams is a doorbell's payload. Meta is decoded into a map of
// json.RawMessage rather than strings so a non-string value is a REJECTION
// with a named field, not a silent zero — `meta.todo_id` arriving as the
// number 7 instead of the string "7" is the exact scenario REQ "Channel
// Notification Handling" calls out, and encoding/json would otherwise just
// fail the whole decode with a message naming no field.
type channelParams struct {
	Content json.RawMessage            `json:"content"`
	Meta    map[string]json.RawMessage `json:"meta"`
}

// parseChannelNotification validates a doorbell's params and returns its
// content and meta.
//
// Everything it rejects, it rejects by NAME, because the operator reading the
// log line is trying to work out what their server sent.
func parseChannelNotification(params json.RawMessage) (string, map[string]string, error) {
	if len(params) > maxParamsBytes {
		return "", nil, fmt.Errorf("params are %d bytes, over the %d-byte limit", len(params), maxParamsBytes)
	}
	if len(params) == 0 {
		return "", nil, fmt.Errorf("the notification carries no params")
	}
	var p channelParams
	if err := json.Unmarshal(params, &p); err != nil {
		return "", nil, fmt.Errorf("params are not an object: %w", err)
	}
	content, ok := jsonString(p.Content)
	if !ok {
		return "", nil, fmt.Errorf("content must be a string")
	}
	if len(p.Meta) == 0 {
		return content, nil, nil
	}
	meta := make(map[string]string, len(p.Meta))
	for k, raw := range p.Meta {
		v, ok := jsonString(raw)
		if !ok {
			return "", nil, fmt.Errorf("meta.%s must be a string", k)
		}
		meta[k] = v
	}
	return content, meta, nil
}

// jsonString decodes raw as a JSON string. A JSON null is NOT a string:
// encoding/json unmarshals null into a string as a silent no-op, which would
// let `content: null` through as "" and fire.
func jsonString(raw json.RawMessage) (string, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '"' {
		return "", false
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", false
	}
	return v, true
}
