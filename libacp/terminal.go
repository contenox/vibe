package libacp

import "encoding/json"

type CreateTerminalRequest struct {
	SessionID       SessionID       `json:"sessionId"`
	Command         string          `json:"command"`
	Args            []string        `json:"args,omitempty"`
	Env             []EnvVariable   `json:"env,omitempty"`
	Cwd             string          `json:"cwd,omitempty"`
	OutputByteLimit *int64          `json:"outputByteLimit,omitempty"`
	Meta            json.RawMessage `json:"_meta,omitempty"`
}

type CreateTerminalResponse struct {
	TerminalID string          `json:"terminalId"`
	Meta       json.RawMessage `json:"_meta,omitempty"`
}

type TerminalExitStatus struct {
	ExitCode *int    `json:"exitCode,omitempty"`
	Signal   *string `json:"signal,omitempty"`
}

type TerminalOutputRequest struct {
	SessionID  SessionID       `json:"sessionId"`
	TerminalID string          `json:"terminalId"`
	Meta       json.RawMessage `json:"_meta,omitempty"`
}

type TerminalOutputResponse struct {
	Output     string              `json:"output"`
	Truncated  bool                `json:"truncated"`
	ExitStatus *TerminalExitStatus `json:"exitStatus,omitempty"`
	Meta       json.RawMessage     `json:"_meta,omitempty"`
}

type WaitForTerminalExitRequest struct {
	SessionID  SessionID       `json:"sessionId"`
	TerminalID string          `json:"terminalId"`
	Meta       json.RawMessage `json:"_meta,omitempty"`
}

type WaitForTerminalExitResponse struct {
	ExitCode *int            `json:"exitCode,omitempty"`
	Signal   *string         `json:"signal,omitempty"`
	Meta     json.RawMessage `json:"_meta,omitempty"`
}

type KillTerminalRequest struct {
	SessionID  SessionID       `json:"sessionId"`
	TerminalID string          `json:"terminalId"`
	Meta       json.RawMessage `json:"_meta,omitempty"`
}

type KillTerminalResponse struct {
	Meta json.RawMessage `json:"_meta,omitempty"`
}

type ReleaseTerminalRequest struct {
	SessionID  SessionID       `json:"sessionId"`
	TerminalID string          `json:"terminalId"`
	Meta       json.RawMessage `json:"_meta,omitempty"`
}

type ReleaseTerminalResponse struct {
	Meta json.RawMessage `json:"_meta,omitempty"`
}
