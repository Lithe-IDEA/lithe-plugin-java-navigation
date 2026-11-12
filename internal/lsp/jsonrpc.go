package lsp

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
)

// maxMessageSize bounds one incoming message; a document is at most a few
// MiB of text, sent once on open.
const maxMessageSize = 64 << 20

// JSON-RPC error codes the server answers with.
const (
	codeParseError       = -32700
	codeInvalidRequest   = -32600
	codeMethodNotFound   = -32601
	codeInvalidParams    = -32602
	codeInternalError    = -32603
	codeServerNotInit    = -32002
	codeRequestCancelled = -32800
)

// message is an incoming request, notification or response.
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// isRequest reports a request, which needs an answer; a notification has no
// id, and a response (to a request this server sent) has no method.
func (m *message) isRequest() bool { return len(m.ID) > 0 && m.Method != "" }

type responseError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result"`
	Error   *responseError  `json:"error,omitempty"`
}

// errorResponse omits result, which a response carrying an error must not
// have.
type errorResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Error   *responseError  `json:"error"`
}

type notification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

// conn reads and writes base-protocol messages: a Content-Length header,
// a blank line and a JSON body.
type conn struct {
	r  *bufio.Reader
	mu sync.Mutex
	w  io.Writer
}

func newConn(r io.Reader, w io.Writer) *conn {
	return &conn{r: bufio.NewReaderSize(r, 64<<10), w: w}
}

// read returns the next message; io.EOF when the client closed the stream.
func (c *conn) read() (*message, error) {
	tp := textproto.NewReader(c.r)
	header, err := tp.ReadMIMEHeader()
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, io.EOF
		}
		return nil, fmt.Errorf("read header: %w", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(header.Get("Content-Length")))
	if err != nil || n < 0 {
		return nil, fmt.Errorf("invalid Content-Length %q", header.Get("Content-Length"))
	}
	if n > maxMessageSize {
		return nil, fmt.Errorf("message of %d bytes exceeds %d", n, maxMessageSize)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(c.r, body); err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	var m message
	if err := json.Unmarshal(body, &m); err != nil {
		return &message{}, errParse
	}
	return &m, nil
}

// errParse marks a body that is not JSON; the stream itself is still in step.
var errParse = errors.New("parse error")

func (c *conn) write(v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := fmt.Fprintf(c.w, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return err
	}
	_, err = c.w.Write(body)
	return err
}

func (c *conn) reply(id json.RawMessage, result any) error {
	return c.write(response{JSONRPC: "2.0", ID: id, Result: result})
}

func (c *conn) replyError(id json.RawMessage, code int, msg string) error {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return c.write(errorResponse{JSONRPC: "2.0", ID: id, Error: &responseError{Code: code, Message: msg}})
}

func (c *conn) notify(method string, params any) error {
	return c.write(notification{JSONRPC: "2.0", Method: method, Params: params})
}
