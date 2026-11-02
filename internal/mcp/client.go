package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
)

// Client calls a remote MCP server over Streamable HTTP. It accepts both plain
// JSON and SSE-framed replies, sends a bearer token when set, and performs the
// initialize handshake lazily on first use.
type Client struct {
	URL   string
	Token string
	Info  Implementation
	HTTP  *http.Client

	nextID atomic.Int64

	mu          sync.Mutex
	initialized bool
	version     string // negotiated protocol version
	sessionID   string // Mcp-Session-Id, if the server issued one
	serverInfo  Implementation
}

// ServerInfo returns the remote server's identity after initialization.
func (c *Client) ServerInfo() Implementation {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.serverInfo
}

// Initialize performs the initialize handshake if it has not happened yet.
func (c *Client) Initialize(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.initialized {
		return nil
	}
	var res struct {
		ProtocolVersion string         `json:"protocolVersion"`
		ServerInfo      Implementation `json:"serverInfo"`
	}
	params := map[string]any{
		"protocolVersion": LatestProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      c.Info,
	}
	if err := c.roundTrip(ctx, "initialize", params, &res, false); err != nil {
		return err
	}
	c.version = res.ProtocolVersion
	c.serverInfo = res.ServerInfo
	if err := c.roundTrip(ctx, "notifications/initialized", nil, nil, true); err != nil {
		return err
	}
	c.initialized = true
	return nil
}

// ListTools returns every tool the server offers, following pagination.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	var all []Tool
	cursor := ""
	for range 100 { // hard stop against a server that never ends pagination
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var res struct {
			Tools      []Tool `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := c.Call(ctx, "tools/list", params, &res); err != nil {
			return nil, err
		}
		all = append(all, res.Tools...)
		if res.NextCursor == "" {
			return all, nil
		}
		cursor = res.NextCursor
	}
	return all, nil
}

// CallTool invokes a remote tool.
func (c *Client) CallTool(ctx context.Context, name string, args any) (*CallToolResult, error) {
	var res CallToolResult
	if err := c.Call(ctx, "tools/call", map[string]any{"name": name, "arguments": args}, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// Call initializes if needed, then sends one request and decodes its result.
func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	if err := c.Initialize(ctx); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.roundTrip(ctx, method, params, result, false)
}

// roundTrip sends one message. c.mu must be held.
func (c *Client) roundTrip(ctx context.Context, method string, params, result any, notification bool) error {
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	var id int64
	if !notification {
		id = c.nextID.Add(1)
		msg["id"] = id
	}
	if params != nil {
		msg["params"] = params
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if c.version != "" {
		req.Header.Set("MCP-Protocol-Version", c.version)
	}
	if c.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", c.sessionID)
	}

	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("mcp %s: %w", method, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.sessionID = sid
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return &HTTPError{Status: resp.StatusCode, Body: strings.TrimSpace(string(excerpt))}
	}
	if notification {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}

	var raw []byte
	if isEventStream(resp.Header.Get("Content-Type")) {
		raw, err = readSSEResponse(resp.Body, id)
	} else {
		raw, err = io.ReadAll(resp.Body)
	}
	if err != nil {
		return fmt.Errorf("mcp %s: read response: %w", method, err)
	}

	var rpc Response
	if err := json.Unmarshal(raw, &rpc); err != nil {
		return fmt.Errorf("mcp %s: decode response: %w", method, err)
	}
	if rpc.Error != nil {
		return rpc.Error
	}
	if result != nil && len(rpc.Result) > 0 {
		if err := json.Unmarshal(rpc.Result, result); err != nil {
			return fmt.Errorf("mcp %s: decode result: %w", method, err)
		}
	}
	return nil
}

// HTTPError is a non-2xx reply from the MCP endpoint (e.g. 401 bad token).
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	msg := fmt.Sprintf("MCP endpoint returned HTTP %d", e.Status)
	switch e.Status {
	case http.StatusUnauthorized:
		msg += " (check the access token)"
	case http.StatusForbidden:
		msg += " (the token's scope does not include MCP)"
	case http.StatusNotFound:
		msg += " (check the URL; the endpoint is <APP_URL>/api/mcp)"
	}
	if e.Body != "" {
		msg += ": " + e.Body
	}
	return msg
}

// readSSEResponse scans an SSE stream for the JSON-RPC response with the given
// id and returns its raw JSON.
func readSSEResponse(r io.Reader, id int64) ([]byte, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), MaxStdioMessageBytes)
	var data bytes.Buffer
	flush := func() ([]byte, bool) {
		defer data.Reset()
		if data.Len() == 0 {
			return nil, false
		}
		var probe struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(data.Bytes(), &probe) != nil || probe.Method != "" {
			return nil, false // not a response (e.g. a server notification)
		}
		var got int64
		if json.Unmarshal(probe.ID, &got) != nil || got != id {
			return nil, false
		}
		return bytes.Clone(data.Bytes()), true
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if b, ok := flush(); ok {
				return b, nil
			}
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if b, ok := flush(); ok {
		return b, nil
	}
	return nil, fmt.Errorf("SSE stream ended without a response for request %d", id)
}
