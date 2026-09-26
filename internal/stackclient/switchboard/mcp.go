package switchboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strings"
)

// unmarshal / unmarshalStrict / fetchInto / jsonString are small shared
// helpers.

func unmarshal(raw []byte, out any) error {
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("switchboard: decode response: %w", err)
	}
	return nil
}

func fetchInto(client *http.Client, req *http.Request, out any) error {
	body, err := doWithRetry(req.Context(), client, req)
	if err != nil {
		return err
	}
	return unmarshal(body, out)
}

func jsonString(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

// MCPListTodos reads the endpoint's own todo queue over Streamable HTTP,
// authenticated with the ENDPOINT token (not the operator grant). The
// self-test uses it to prove a todo reached `done` by content (REQ-25) —
// this is the only MCP call Harness makes.
func MCPListTodos(ctx context.Context, client *http.Client, mcpURL, endpointToken string) ([]Todo, error) {
	payload := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_todos","arguments":{"limit":200}}}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, mcpURL, strings.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+endpointToken)
	raw, header, se, err := doWithHeader(ctx, client, req)
	if se != nil {
		return nil, se
	}
	if err != nil {
		return nil, err
	}
	// Streamable HTTP may answer the POST with an event stream instead of a
	// JSON body; the Accept header above invites it.
	if mt, _, _ := mime.ParseMediaType(header.Get("Content-Type")); mt == "text/event-stream" {
		if raw, err = sseResponse(raw); err != nil {
			return nil, err
		}
	}
	var doc struct {
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if doc.Error != nil {
		return nil, fmt.Errorf("switchboard: MCP list_todos: %s", doc.Error.Message)
	}
	// A text item that is not a {"todos":[...]} page is an error, never
	// "zero todos": the self-test reads an empty result as "not done yet",
	// so a silently dropped page would be a false negative.
	var todos []Todo
	for _, c := range doc.Result.Content {
		if c.Type != "text" {
			continue
		}
		var page struct {
			Todos *[]Todo `json:"todos"`
		}
		if err := json.Unmarshal([]byte(c.Text), &page); err != nil {
			return nil, fmt.Errorf("switchboard: MCP list_todos: undecodable content: %w", err)
		}
		if page.Todos == nil {
			return nil, fmt.Errorf("switchboard: MCP list_todos: content has no todos: %s", boundText(c.Text, 200))
		}
		todos = append(todos, *page.Todos...)
	}
	return todos, nil
}

// sseResponse extracts the JSON-RPC reply to request id 1 from a
// text/event-stream body. An event's payload is its data: lines joined by
// "\n"; the last event whose payload carries "id":1 wins, since a server may
// send notifications on the stream before the response.
func sseResponse(raw []byte) ([]byte, error) {
	var found []byte
	var data []string
	flush := func() {
		if len(data) == 0 {
			return
		}
		payload := strings.Join(data, "\n")
		data = data[:0]
		var msg struct {
			ID json.RawMessage `json:"id"`
		}
		if json.Unmarshal([]byte(payload), &msg) == nil && string(msg.ID) == "1" {
			found = []byte(payload)
		}
	}
	for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	flush()
	if found == nil {
		return nil, errors.New("switchboard: MCP list_todos: event stream carried no response to the request")
	}
	return found, nil
}

// Todo is the slice of a Switchboard todo the self-test (and doctor) reads.
type Todo struct {
	ID     string `json:"id"`
	Queue  string `json:"queue"`
	State  string `json:"state"`
	Result string `json:"result,omitempty"`
}
