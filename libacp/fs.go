package libacp

import "encoding/json"

type ReadTextFileRequest struct {
	SessionID SessionID       `json:"sessionId"`
	Path      string          `json:"path"`
	Line      *int            `json:"line,omitempty"`
	Limit     *int            `json:"limit,omitempty"`
	Meta      json.RawMessage `json:"_meta,omitempty"`
}

type ReadTextFileResponse struct {
	Content string          `json:"content"`
	Meta    json.RawMessage `json:"_meta,omitempty"`
}

type WriteTextFileRequest struct {
	SessionID SessionID       `json:"sessionId"`
	Path      string          `json:"path"`
	Content   string          `json:"content"`
	Meta      json.RawMessage `json:"_meta,omitempty"`
}

type WriteTextFileResponse struct {
	Meta json.RawMessage `json:"_meta,omitempty"`
}
