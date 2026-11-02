package mcp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
)

// HTTPHandler serves the MCP Streamable HTTP transport in its stateless form:
// every JSON-RPC message is a POST and the reply is a single application/json
// body (no SSE stream, no session id). GET and DELETE return 405, which the
// spec defines as "this server offers no standalone stream / sessions".
//
// Authentication is the caller's job (wrap the handler in middleware).
type HTTPHandler struct {
	Server *Server
	// MaxBodyBytes caps one request body. Zero means 10 MiB.
	MaxBodyBytes int64
}

func (h *HTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeHTTPError(w, http.StatusMethodNotAllowed, CodeInvalidRequest, "This MCP endpoint only accepts POST (stateless Streamable HTTP; no SSE stream).")
		return
	}
	if v := r.Header.Get("MCP-Protocol-Version"); v != "" && !slices.Contains(SupportedProtocolVersions, v) {
		writeHTTPError(w, http.StatusBadRequest, CodeInvalidRequest, "Unsupported MCP-Protocol-Version: "+v)
		return
	}
	limit := h.MaxBodyBytes
	if limit <= 0 {
		limit = 10 << 20
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			writeHTTPError(w, http.StatusRequestEntityTooLarge, CodeInvalidRequest, "Request body exceeds the size limit.")
			return
		}
		writeHTTPError(w, http.StatusBadRequest, CodeParseError, "Could not read request body.")
		return
	}

	resp := h.Server.HandleMessage(r.Context(), body)
	if resp == nil {
		// Only notifications/responses were received.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(resp)
}

func writeHTTPError(w http.ResponseWriter, status, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(mustMarshal(errorResponse(nil, code, msg)))
}

// MaxStdioMessageBytes caps one newline-delimited message on stdio. Uploads are
// base64 inside JSON, so this is generous.
const MaxStdioMessageBytes = 256 << 20

// ServeStdio runs the stdio transport: newline-delimited JSON-RPC messages on
// in, responses on out. Messages are processed in order. It returns nil when in
// reaches EOF or ctx is cancelled. Nothing but protocol messages may be written
// to out, so log elsewhere (stderr).
func (s *Server) ServeStdio(ctx context.Context, in io.Reader, out io.Writer) error {
	br := bufio.NewReaderSize(in, 64<<10)
	var mu sync.Mutex
	for {
		if ctx.Err() != nil {
			return nil
		}
		line, err := readLine(br, MaxStdioMessageBytes)
		if len(bytes.TrimSpace(line)) > 0 {
			if resp := s.HandleMessage(ctx, line); resp != nil {
				mu.Lock()
				_, werr := out.Write(append(resp, '\n'))
				mu.Unlock()
				if werr != nil {
					return werr
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			if errors.Is(err, errLineTooLong) {
				mu.Lock()
				_, werr := out.Write(append(mustMarshal(errorResponse(nil, CodeInvalidRequest, err.Error())), '\n'))
				mu.Unlock()
				if werr != nil {
					return werr
				}
				continue
			}
			return err
		}
	}
}

var errLineTooLong = errors.New("message exceeds the stdio size limit")

// readLine reads up to and excluding '\n'. An over-long line is drained and
// reported as errLineTooLong so the stream stays in sync.
func readLine(br *bufio.Reader, limit int) ([]byte, error) {
	var buf []byte
	tooLong := false
	for {
		chunk, err := br.ReadSlice('\n')
		if !tooLong {
			if len(buf)+len(chunk) > limit {
				tooLong = true
				buf = nil
			} else {
				buf = append(buf, chunk...)
			}
		}
		switch {
		case err == nil:
			if tooLong {
				return nil, errLineTooLong
			}
			return bytes.TrimRight(buf, "\r\n"), nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		default:
			if tooLong {
				return nil, errLineTooLong
			}
			return bytes.TrimRight(buf, "\r\n"), err
		}
	}
}

// isEventStream reports whether a Content-Type is text/event-stream.
func isEventStream(ct string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(ct)), "text/event-stream")
}
