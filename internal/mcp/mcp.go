// Package mcp is a small, dependency-free implementation of the Model Context
// Protocol (https://modelcontextprotocol.io) covering what Paper Plane needs:
// the tools capability over JSON-RPC 2.0, served either over Streamable HTTP
// (stateless POST with JSON responses) or over stdio (newline-delimited JSON).
// A matching Client lets a local process talk to a remote MCP endpoint.
//
// The server side is transport-neutral: Server.HandleMessage takes one raw
// JSON-RPC message (or batch) and returns the raw response, so HTTP, stdio, and
// tests all share one code path. Tools are supplied by a ToolProvider, which is
// either a static Registry or something dynamic such as a proxy.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
)

// LatestProtocolVersion is the newest MCP revision this package speaks.
const LatestProtocolVersion = "2025-11-25"

// SupportedProtocolVersions lists every revision accepted during negotiation,
// newest first. The subset used here (tools, ping) is identical across them.
var SupportedProtocolVersions = []string{
	"2025-11-25",
	"2025-06-18",
	"2025-03-26",
	"2024-11-05",
}

// JSON-RPC 2.0 error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// --- wire types ---

// Request is a JSON-RPC request or notification (ID absent).
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// isNotification reports whether the message carries no id.
func (r *Request) isNotification() bool { return len(r.ID) == 0 }

// Response is a JSON-RPC response.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is a JSON-RPC error object. It also implements error so clients can
// return it directly.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("mcp error %d: %s", e.Code, e.Message) }

// Implementation names a client or server.
type Implementation struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

// Tool describes one callable tool.
type Tool struct {
	Name        string           `json:"name"`
	Title       string           `json:"title,omitempty"`
	Description string           `json:"description,omitempty"`
	InputSchema json.RawMessage  `json:"inputSchema"`
	Annotations *ToolAnnotations `json:"annotations,omitempty"`
}

// ToolAnnotations are behavioural hints for clients (e.g. whether to ask the
// user before running a destructive tool).
type ToolAnnotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    *bool  `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool  `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool  `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool  `json:"openWorldHint,omitempty"`
}

// Content is one item of tool output. Only text content is produced here.
type Content struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// CallToolResult is the result of tools/call. IsError marks a tool-level
// failure the model should see and can react to (as opposed to a protocol
// error).
type CallToolResult struct {
	Content           []Content `json:"content"`
	StructuredContent any       `json:"structuredContent,omitempty"`
	IsError           bool      `json:"isError,omitempty"`
}

// TextResult returns a successful result with a single text item.
func TextResult(text string) *CallToolResult {
	return &CallToolResult{Content: []Content{{Type: "text", Text: text}}}
}

// ErrorResult returns a tool-level error result with the given message.
func ErrorResult(msg string) *CallToolResult {
	return &CallToolResult{Content: []Content{{Type: "text", Text: msg}}, IsError: true}
}

// JSONResult returns v as indented JSON text, and also as structuredContent
// when v encodes to a JSON object (the only shape structuredContent allows).
func JSONResult(v any) *CallToolResult {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return ErrorResult("encode result: " + err.Error())
	}
	res := TextResult(string(b))
	if bytes.HasPrefix(bytes.TrimSpace(b), []byte("{")) {
		res.StructuredContent = json.RawMessage(b)
	}
	return res
}

// Bool returns a pointer to b, for ToolAnnotations hints.
func Bool(b bool) *bool { return &b }

// --- tool providers ---

// ToolProvider supplies the tool list and executes calls. CallTool returns
// ErrUnknownTool for a name it does not serve; any other error becomes a
// tool-level error result.
type ToolProvider interface {
	ListTools(ctx context.Context) ([]Tool, error)
	CallTool(ctx context.Context, name string, args json.RawMessage) (*CallToolResult, error)
}

// ErrUnknownTool is returned by a ToolProvider for an unknown tool name.
var ErrUnknownTool = &RPCError{Code: CodeInvalidParams, Message: "unknown tool"}

// ToolHandler executes one tool with its raw JSON arguments.
type ToolHandler func(ctx context.Context, args json.RawMessage) (*CallToolResult, error)

// Registry is a static ToolProvider.
type Registry struct {
	tools    []Tool
	handlers map[string]ToolHandler
}

// Add registers a tool. It panics on a duplicate name (a programming error).
func (r *Registry) Add(t Tool, h ToolHandler) {
	if r.handlers == nil {
		r.handlers = map[string]ToolHandler{}
	}
	if _, dup := r.handlers[t.Name]; dup {
		panic("mcp: duplicate tool " + t.Name)
	}
	if len(t.InputSchema) == 0 {
		t.InputSchema = json.RawMessage(`{"type":"object"}`)
	}
	r.tools = append(r.tools, t)
	r.handlers[t.Name] = h
}

// ListTools returns the registered tools in registration order.
func (r *Registry) ListTools(context.Context) ([]Tool, error) {
	return slices.Clone(r.tools), nil
}

// CallTool runs the named tool.
func (r *Registry) CallTool(ctx context.Context, name string, args json.RawMessage) (*CallToolResult, error) {
	h, ok := r.handlers[name]
	if !ok {
		return nil, ErrUnknownTool
	}
	return h(ctx, args)
}

// DecodeArgs strictly decodes tool arguments into dst (unknown fields are
// rejected so typos surface to the model). Empty args decode as {}.
func DecodeArgs(args json.RawMessage, dst any) error {
	if len(bytes.TrimSpace(args)) == 0 || bytes.Equal(bytes.TrimSpace(args), []byte("null")) {
		args = json.RawMessage(`{}`)
	}
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

// --- server ---

// Server answers MCP requests using a ToolProvider.
type Server struct {
	Info         Implementation
	Instructions string
	Tools        ToolProvider
	Log          *slog.Logger
}

type initializeParams struct {
	ProtocolVersion string         `json:"protocolVersion"`
	ClientInfo      Implementation `json:"clientInfo"`
}

type callToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// HandleMessage processes one raw JSON-RPC message or batch and returns the raw
// response to send, or nil when there is nothing to send (only notifications).
func (s *Server) HandleMessage(ctx context.Context, raw []byte) []byte {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var batch []json.RawMessage
		if err := json.Unmarshal(trimmed, &batch); err != nil || len(batch) == 0 {
			return mustMarshal(errorResponse(nil, CodeParseError, "invalid JSON-RPC batch"))
		}
		var out []*Response
		for _, m := range batch {
			if resp := s.handleOne(ctx, m); resp != nil {
				out = append(out, resp)
			}
		}
		if len(out) == 0 {
			return nil
		}
		return mustMarshal(out)
	}
	resp := s.handleOne(ctx, trimmed)
	if resp == nil {
		return nil
	}
	return mustMarshal(resp)
}

func (s *Server) handleOne(ctx context.Context, raw []byte) *Response {
	var req Request
	if err := json.Unmarshal(raw, &req); err != nil {
		return errorResponse(nil, CodeParseError, "parse error: "+err.Error())
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		if req.Method == "" && len(req.ID) > 0 {
			// A response sent to us (we never issue server→client requests).
			return nil
		}
		return errorResponse(req.ID, CodeInvalidRequest, "invalid JSON-RPC 2.0 request")
	}
	if req.isNotification() {
		// notifications/initialized, notifications/cancelled, ...: nothing to do.
		return nil
	}

	result, rpcErr := s.dispatch(ctx, &req)
	if rpcErr != nil {
		return &Response{JSONRPC: "2.0", ID: req.ID, Error: rpcErr}
	}
	b, err := json.Marshal(result)
	if err != nil {
		return errorResponse(req.ID, CodeInternalError, "encode result: "+err.Error())
	}
	return &Response{JSONRPC: "2.0", ID: req.ID, Result: b}
}

func (s *Server) dispatch(ctx context.Context, req *Request) (any, *RPCError) {
	switch req.Method {
	case "initialize":
		var p initializeParams
		if len(req.Params) > 0 {
			if err := json.Unmarshal(req.Params, &p); err != nil {
				return nil, &RPCError{Code: CodeInvalidParams, Message: "invalid initialize params"}
			}
		}
		version := LatestProtocolVersion
		if slices.Contains(SupportedProtocolVersions, p.ProtocolVersion) {
			version = p.ProtocolVersion
		}
		s.logger().Info("mcp initialize", "client", p.ClientInfo.Name, "client_version", p.ClientInfo.Version, "protocol", version)
		out := map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      s.Info,
		}
		if s.Instructions != "" {
			out["instructions"] = s.Instructions
		}
		return out, nil

	case "ping":
		return map[string]any{}, nil

	case "tools/list":
		tools, err := s.Tools.ListTools(ctx)
		if err != nil {
			s.logger().Error("mcp list tools", "error", err)
			return nil, &RPCError{Code: CodeInternalError, Message: "list tools: " + err.Error()}
		}
		if tools == nil {
			tools = []Tool{}
		}
		return map[string]any{"tools": tools}, nil

	case "tools/call":
		var p callToolParams
		if err := json.Unmarshal(req.Params, &p); err != nil || p.Name == "" {
			return nil, &RPCError{Code: CodeInvalidParams, Message: "tools/call requires params.name"}
		}
		res, err := s.Tools.CallTool(ctx, p.Name, p.Arguments)
		if err != nil {
			if rpcErr, ok := err.(*RPCError); ok {
				if rpcErr == ErrUnknownTool {
					return nil, &RPCError{Code: CodeInvalidParams, Message: "unknown tool: " + p.Name}
				}
				return nil, rpcErr
			}
			return ErrorResult(err.Error()), nil
		}
		if res == nil {
			res = TextResult("ok")
		}
		if res.Content == nil {
			res.Content = []Content{}
		}
		return res, nil

	case "resources/list":
		return map[string]any{"resources": []any{}}, nil
	case "prompts/list":
		return map[string]any{"prompts": []any{}}, nil
	}
	return nil, &RPCError{Code: CodeMethodNotFound, Message: "method not found: " + req.Method}
}

func (s *Server) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func errorResponse(id json.RawMessage, code int, msg string) *Response {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return &Response{JSONRPC: "2.0", ID: id, Error: &RPCError{Code: code, Message: msg}}
}

func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		// Only our own response types are marshalled here; they always encode.
		panic("mcp: marshal response: " + err.Error())
	}
	return b
}
