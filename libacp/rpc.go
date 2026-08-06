package libacp

import (
	"bytes"
	"encoding/json"
	"fmt"
)

type RequestIDKind uint8

const (
	RequestIDKindNull RequestIDKind = iota
	RequestIDKindNumber
	RequestIDKindString
)

type RequestID struct {
	Kind   RequestIDKind
	Number int64
	String string
}

func NewRequestIDNull() RequestID          { return RequestID{Kind: RequestIDKindNull} }
func NewRequestIDNumber(n int64) RequestID { return RequestID{Kind: RequestIDKindNumber, Number: n} }
func NewRequestIDString(s string) RequestID {
	return RequestID{Kind: RequestIDKindString, String: s}
}

func (r RequestID) MarshalJSON() ([]byte, error) {
	switch r.Kind {
	case RequestIDKindNull:
		return []byte("null"), nil
	case RequestIDKindNumber:
		return json.Marshal(r.Number)
	case RequestIDKindString:
		return json.Marshal(r.String)
	}
	return nil, fmt.Errorf("libacp: unknown RequestID kind %d", r.Kind)
}

func (r *RequestID) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if bytes.Equal(trimmed, []byte("null")) {
		r.Kind = RequestIDKindNull
		return nil
	}
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return fmt.Errorf("libacp: invalid string RequestID: %w", err)
		}
		r.Kind = RequestIDKindString
		r.String = s
		return nil
	}
	var n int64
	if err := json.Unmarshal(trimmed, &n); err != nil {
		return fmt.Errorf("libacp: invalid numeric RequestID: %w", err)
	}
	r.Kind = RequestIDKindNumber
	r.Number = n
	return nil
}

func (r RequestID) String_() string {
	switch r.Kind {
	case RequestIDKindNull:
		return "null"
	case RequestIDKindNumber:
		return fmt.Sprintf("%d", r.Number)
	case RequestIDKindString:
		return r.String
	}
	return ""
}

func (r RequestID) Equal(other RequestID) bool {
	if r.Kind != other.Kind {
		return false
	}
	switch r.Kind {
	case RequestIDKindNull:
		return true
	case RequestIDKindNumber:
		return r.Number == other.Number
	case RequestIDKindString:
		return r.String == other.String
	}
	return false
}

type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      RequestID       `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type Notification struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      RequestID       `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

func NewRequest(id RequestID, method string, params json.RawMessage) Request {
	return Request{JSONRPC: "2.0", ID: id, Method: method, Params: params}
}

func NewNotification(method string, params json.RawMessage) Notification {
	return Notification{JSONRPC: "2.0", Method: method, Params: params}
}

func NewResultResponse(id RequestID, result json.RawMessage) Response {
	return Response{JSONRPC: "2.0", ID: id, Result: result}
}

func NewErrorResponse(id RequestID, err *Error) Response {
	return Response{JSONRPC: "2.0", ID: id, Error: err}
}

type incomingMessage struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  *string          `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *Error           `json:"error,omitempty"`
}

type IncomingKind uint8

const (
	IncomingKindUnknown IncomingKind = iota
	IncomingKindRequest
	IncomingKindNotification
	IncomingKindResponse
)

type Incoming struct {
	Kind         IncomingKind
	Request      Request
	Notification Notification
	Response     Response
}

func ParseIncoming(data []byte) (Incoming, error) {
	var raw incomingMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return Incoming{}, fmt.Errorf("libacp: parse message: %w", err)
	}
	if raw.JSONRPC != "" && raw.JSONRPC != "2.0" {
		return Incoming{}, fmt.Errorf("libacp: unsupported jsonrpc version %q", raw.JSONRPC)
	}

	hasMethod := raw.Method != nil
	hasID := raw.ID != nil

	if hasMethod && hasID {
		var id RequestID
		if err := id.UnmarshalJSON(*raw.ID); err != nil {
			return Incoming{}, err
		}
		return Incoming{
			Kind: IncomingKindRequest,
			Request: Request{
				JSONRPC: "2.0", ID: id, Method: *raw.Method, Params: raw.Params,
			},
		}, nil
	}
	if hasMethod && !hasID {
		return Incoming{
			Kind: IncomingKindNotification,
			Notification: Notification{
				JSONRPC: "2.0", Method: *raw.Method, Params: raw.Params,
			},
		}, nil
	}
	if !hasMethod && hasID {
		var id RequestID
		if err := id.UnmarshalJSON(*raw.ID); err != nil {
			return Incoming{}, err
		}
		return Incoming{
			Kind: IncomingKindResponse,
			Response: Response{
				JSONRPC: "2.0", ID: id, Result: raw.Result, Error: raw.Error,
			},
		}, nil
	}
	if !hasMethod && (raw.Error != nil || raw.Result != nil) {
		// A response with "id":null — JSON-RPC uses it for errors answering
		// requests whose id could not be determined (parse errors). The null id
		// unmarshals to a nil pointer above, so hasID is false here.
		return Incoming{
			Kind: IncomingKindResponse,
			Response: Response{
				JSONRPC: "2.0", ID: NewRequestIDNull(), Result: raw.Result, Error: raw.Error,
			},
		}, nil
	}
	return Incoming{}, fmt.Errorf("libacp: message has neither method nor id")
}
