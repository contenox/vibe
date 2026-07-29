package vscodeagent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const jsonrpcVersion = "2.0"
const maxJSONRPCContentLength = 64 * 1024 * 1024

const (
	ErrParseError     = -32700
	ErrInvalidRequest = -32600
	ErrMethodNotFound = -32601
	ErrInvalidParams  = -32602
	ErrInternal       = -32603
)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Result  any              `json:"result,omitempty"`
	Error   *responseError   `json:"error,omitempty"`
}

type responseError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type notification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type framer struct {
	r *bufio.Reader
	w io.Writer
}

func newFramer(r io.Reader, w io.Writer) *framer {
	return &framer{r: bufio.NewReader(r), w: w}
}

func (f *framer) readPayload() ([]byte, error) {
	contentLength := -1
	for {
		line, err := f.r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("malformed header line %q", line)
		}
		if strings.EqualFold(strings.TrimSpace(key), "Content-Length") {
			n, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil || n < 0 {
				return nil, fmt.Errorf("invalid Content-Length %q", strings.TrimSpace(value))
			}
			if n > maxJSONRPCContentLength {
				return nil, fmt.Errorf("Content-Length %d exceeds maximum %d", n, maxJSONRPCContentLength)
			}
			contentLength = n
		}
	}
	if contentLength < 0 {
		return nil, fmt.Errorf("missing Content-Length header")
	}
	payload := make([]byte, contentLength)
	if _, err := io.ReadFull(f.r, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func (f *framer) writeMessage(v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(f.w, "Content-Length: %d\r\n\r\n", len(payload)); err != nil {
		return err
	}
	_, err = f.w.Write(payload)
	return err
}

func strictDecode(raw json.RawMessage, dst any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if dec.More() {
		return fmt.Errorf("multiple JSON values")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return fmt.Errorf("multiple JSON values")
	}
	return nil
}

func rawID(id json.RawMessage) *json.RawMessage {
	if len(id) == 0 {
		return nil
	}
	cp := append(json.RawMessage(nil), id...)
	return &cp
}

func rpcIDKey(id json.RawMessage) string {
	return string(bytes.TrimSpace(id))
}
