package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testServer() *Server {
	reg := &Registry{}
	reg.Add(Tool{Name: "echo", Description: "echo back"}, func(_ context.Context, args json.RawMessage) (*CallToolResult, error) {
		var a struct {
			Text string `json:"text"`
		}
		if err := DecodeArgs(args, &a); err != nil {
			return nil, err
		}
		return JSONResult(map[string]string{"text": a.Text}), nil
	})
	reg.Add(Tool{Name: "fail"}, func(context.Context, json.RawMessage) (*CallToolResult, error) {
		return nil, errors.New("boom")
	})
	return &Server{Info: Implementation{Name: "t", Version: "1"}, Instructions: "hi", Tools: reg}
}

func call(t *testing.T, s *Server, msg string) map[string]any {
	t.Helper()
	out := s.HandleMessage(context.Background(), []byte(msg))
	if out == nil {
		t.Fatalf("no response to %s", msg)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("decode %s: %v", out, err)
	}
	return m
}

func TestInitializeNegotiatesVersion(t *testing.T) {
	s := testServer()
	for _, tc := range []struct{ asked, want string }{
		{"2025-06-18", "2025-06-18"},
		{"2024-11-05", "2024-11-05"},
		{"1999-01-01", LatestProtocolVersion},
	} {
		m := call(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"`+tc.asked+`","capabilities":{},"clientInfo":{"name":"c","version":"0"}}}`)
		res := m["result"].(map[string]any)
		if res["protocolVersion"] != tc.want {
			t.Errorf("asked %s: got %v, want %s", tc.asked, res["protocolVersion"], tc.want)
		}
		if _, ok := res["capabilities"].(map[string]any)["tools"]; !ok {
			t.Errorf("tools capability missing: %v", res)
		}
		if res["instructions"] != "hi" {
			t.Errorf("instructions = %v", res["instructions"])
		}
	}
}

func TestNotificationsGetNoResponse(t *testing.T) {
	s := testServer()
	if out := s.HandleMessage(context.Background(), []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); out != nil {
		t.Fatalf("notification produced %s", out)
	}
}

func TestToolsListAndCall(t *testing.T) {
	s := testServer()
	m := call(t, s, `{"jsonrpc":"2.0","id":"a","method":"tools/list"}`)
	tools := m["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 2 || tools[0].(map[string]any)["name"] != "echo" {
		t.Fatalf("tools = %v", tools)
	}
	if tools[0].(map[string]any)["inputSchema"] == nil {
		t.Fatal("inputSchema must default to an object schema")
	}

	m = call(t, s, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hey"}}}`)
	res := m["result"].(map[string]any)
	if res["isError"] == true {
		t.Fatalf("unexpected error result: %v", res)
	}
	if res["structuredContent"].(map[string]any)["text"] != "hey" {
		t.Fatalf("structuredContent = %v", res["structuredContent"])
	}
	if m["id"] != float64(2) {
		t.Fatalf("id not echoed: %v", m["id"])
	}
}

func TestToolErrorsAreResults(t *testing.T) {
	s := testServer()
	m := call(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fail"}}`)
	res := m["result"].(map[string]any)
	if res["isError"] != true || !strings.Contains(res["content"].([]any)[0].(map[string]any)["text"].(string), "boom") {
		t.Fatalf("want isError result with message, got %v", m)
	}
	// Unknown argument fields are reported to the model, not silently ignored.
	m = call(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"txt":"x"}}}`)
	if m["result"].(map[string]any)["isError"] != true {
		t.Fatalf("want isError for unknown field, got %v", m)
	}
}

func TestProtocolErrors(t *testing.T) {
	s := testServer()
	for _, tc := range []struct {
		msg  string
		code float64
	}{
		{`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nope"}}`, CodeInvalidParams},
		{`{"jsonrpc":"2.0","id":1,"method":"bogus"}`, CodeMethodNotFound},
		{`{not json`, CodeParseError},
		{`{"jsonrpc":"1.0","id":1,"method":"ping"}`, CodeInvalidRequest},
	} {
		m := call(t, s, tc.msg)
		e, ok := m["error"].(map[string]any)
		if !ok || e["code"] != tc.code {
			t.Errorf("%s: got %v, want code %v", tc.msg, m, tc.code)
		}
	}
}

func TestBatch(t *testing.T) {
	s := testServer()
	out := s.HandleMessage(context.Background(), []byte(`[{"jsonrpc":"2.0","id":1,"method":"ping"},{"jsonrpc":"2.0","method":"notifications/initialized"},{"jsonrpc":"2.0","id":2,"method":"ping"}]`))
	var arr []map[string]any
	if err := json.Unmarshal(out, &arr); err != nil || len(arr) != 2 {
		t.Fatalf("batch response = %s (%v)", out, err)
	}
}

func TestHTTPHandler(t *testing.T) {
	h := &HTTPHandler{Server: testServer(), MaxBodyBytes: 1024}

	post := func(body string, hdr map[string]string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, r)
		return rr
	}

	rr := post(`{"jsonrpc":"2.0","id":1,"method":"ping"}`, nil)
	if rr.Code != 200 || !strings.HasPrefix(rr.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("ping: %d %s", rr.Code, rr.Header())
	}
	if rr := post(`{"jsonrpc":"2.0","method":"notifications/initialized"}`, nil); rr.Code != http.StatusAccepted {
		t.Fatalf("notification status = %d, want 202", rr.Code)
	}
	if rr := post(`{"jsonrpc":"2.0","id":1,"method":"ping"}`, map[string]string{"MCP-Protocol-Version": "1900-01-01"}); rr.Code != http.StatusBadRequest {
		t.Fatalf("bad version status = %d, want 400", rr.Code)
	}
	if rr := post(`{"jsonrpc":"2.0","id":1,"method":"ping","params":{"pad":"`+strings.Repeat("x", 2048)+`"}}`, nil); rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized status = %d, want 413", rr.Code)
	}
	r := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusMethodNotAllowed || rr.Header().Get("Allow") != "POST" {
		t.Fatalf("GET: %d Allow=%q", rr.Code, rr.Header().Get("Allow"))
	}
}

func TestServeStdio(t *testing.T) {
	s := testServer()
	in := strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}` + "\n" +
			`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" +
			"\n" +
			`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"text":"x"}}}` + "\r\n")
	var out bytes.Buffer
	if err := s.ServeStdio(context.Background(), in, &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 responses, got %d: %q", len(lines), out.String())
	}
	if !strings.Contains(lines[1], `"id":2`) {
		t.Fatalf("second response = %s", lines[1])
	}
}

func TestClientAgainstHTTPHandler(t *testing.T) {
	var sawAuth, sawVersion string
	h := &HTTPHandler{Server: testServer()}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		if v := r.Header.Get("MCP-Protocol-Version"); v != "" {
			sawVersion = v
		}
		h.ServeHTTP(w, r)
	}))
	defer ts.Close()

	c := &Client{URL: ts.URL, Token: "tok", Info: Implementation{Name: "c", Version: "1"}}
	tools, err := c.ListTools(context.Background())
	if err != nil || len(tools) != 2 {
		t.Fatalf("ListTools = %v, %v", tools, err)
	}
	res, err := c.CallTool(context.Background(), "echo", map[string]string{"text": "yo"})
	if err != nil || res.IsError || !strings.Contains(res.Content[0].Text, "yo") {
		t.Fatalf("CallTool = %+v, %v", res, err)
	}
	if sawAuth != "Bearer tok" || sawVersion != LatestProtocolVersion {
		t.Fatalf("headers: auth=%q version=%q", sawAuth, sawVersion)
	}
	if c.ServerInfo().Name != "t" {
		t.Fatalf("server info = %+v", c.ServerInfo())
	}
	_, err = c.CallTool(context.Background(), "missing", nil)
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != CodeInvalidParams {
		t.Fatalf("unknown tool err = %v", err)
	}
}

func TestClientReadsSSEAndHTTPErrors(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"unauthorized"}}`))
			return
		}
		var req Request
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		// A notification first, then the real response split across data lines.
		_, _ = w.Write([]byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"jsonrpc\":\"2.0\",\"id\":" + string(req.ID) + ",\ndata: \"result\":{\"protocolVersion\":\"2025-06-18\",\"serverInfo\":{\"name\":\"sse\",\"version\":\"1\"},\"tools\":[]}}\n\n"))
	}))
	defer ts.Close()

	c := &Client{URL: ts.URL, Token: "good"}
	if _, err := c.ListTools(context.Background()); err != nil {
		t.Fatalf("SSE ListTools: %v", err)
	}
	if c.ServerInfo().Name != "sse" {
		t.Fatalf("server info = %+v", c.ServerInfo())
	}

	bad := &Client{URL: ts.URL, Token: "bad"}
	_, err := bad.ListTools(context.Background())
	var he *HTTPError
	if !errors.As(err, &he) || he.Status != 401 || !strings.Contains(err.Error(), "access token") {
		t.Fatalf("want 401 HTTPError, got %v", err)
	}
}
