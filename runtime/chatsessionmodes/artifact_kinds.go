package chatsessionmodes

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ArtifactKind is a typed discriminator for [ContextArtifact].
//
// The set of first-party kinds below forms the vocabulary that Beam's canvas
// can round-trip into the LLM context. Each has a dedicated renderer (see
// [renderersByKind]) that produces an LLM-friendly system-message body. Any
// kind that is NOT in this registry still passes the generic regex check in
// [BuildInjectedSystemMessages] and falls back to the flat format so older
// clients keep working.
type ArtifactKind string

const (
	// ArtifactKindFileExcerpt is the original UI-contributed kind: a byte-bounded
	// slice of a file selected in the Beam workspace. Payload: [FileExcerptPayload].
	ArtifactKindFileExcerpt ArtifactKind = "file_excerpt"

	// ArtifactKindTerminalOutput captures a recent block of terminal output that
	// the user chose to attach. The LLM is told this may be stale.
	// Payload: [TerminalOutputPayload].
	ArtifactKindTerminalOutput ArtifactKind = "terminal_output"

	// ArtifactKindOpenFile carries the full (still byte-bounded) content of the
	// file currently open or selected in Beam's files tree / editor.
	// Payload: [OpenFilePayload].
	ArtifactKindOpenFile ArtifactKind = "open_file"

	// ArtifactKindPlanStep carries a structured slice of plan state — one step,
	// its status, its summary handover, its optional failure class. Used when
	// the user or agent wants the agent to reason about a specific plan step.
	// Payload: [PlanStepPayload].
	ArtifactKindPlanStep ArtifactKind = "plan_step"

	// ArtifactKindCommandOutput is for structured tool-execution output (as
	// distinct from terminal chatter): typically `{command, output, exit_code}`.
	// Payload: [CommandOutputPayload].
	ArtifactKindCommandOutput ArtifactKind = "command_output"

	// ArtifactKindRuntimeState carries a named unit of taskengine runtime state
	// (from the captured-state sidebar). Payload: [RuntimeStatePayload].
	ArtifactKindRuntimeState ArtifactKind = "runtime_state"
)

// FirstPartyArtifactKinds returns every kind this package knows how to render
// natively. Useful for UI-side validation.
func FirstPartyArtifactKinds() []ArtifactKind {
	out := make([]ArtifactKind, 0, len(renderersByKind))
	for k := range renderersByKind {
		out = append(out, k)
	}
	return out
}

// ArtifactRenderer turns a raw payload into the body of a single system
// message. The returned string MUST NOT include the `[Context kind=…]` header —
// [BuildInjectedSystemMessages] prepends it uniformly so the LLM always sees a
// consistent delimiter.
type ArtifactRenderer func(payload json.RawMessage) (body string, err error)

// KindSpec binds a kind to its renderer and optional per-kind limits.
type KindSpec struct {
	Kind     ArtifactKind
	MaxBytes int // 0 → use maxArtifactPayloadBytes
	Render   ArtifactRenderer
}

// renderersByKind is the canonical registry. Extend it (and add a test case in
// context_test.go) every time a new first-party kind lands.
var renderersByKind = map[ArtifactKind]KindSpec{
	ArtifactKindFileExcerpt:    {Kind: ArtifactKindFileExcerpt, Render: renderFileExcerpt},
	ArtifactKindTerminalOutput: {Kind: ArtifactKindTerminalOutput, Render: renderTerminalOutput},
	ArtifactKindOpenFile:       {Kind: ArtifactKindOpenFile, Render: renderOpenFile},
	ArtifactKindPlanStep:       {Kind: ArtifactKindPlanStep, Render: renderPlanStep},
	ArtifactKindCommandOutput:  {Kind: ArtifactKindCommandOutput, Render: renderCommandOutput},
	ArtifactKindRuntimeState:   {Kind: ArtifactKindRuntimeState, Render: renderRuntimeState},
}

// lookupKindSpec returns the spec for kind, or (zero, false) when the kind is
// not registered. Callers fall back to the generic flat format for unknown kinds.
func lookupKindSpec(kind string) (KindSpec, bool) {
	spec, ok := renderersByKind[ArtifactKind(kind)]
	return spec, ok
}

// ------------------------------------------------------------------
// Typed payload shapes.
// ------------------------------------------------------------------

// FileExcerptPayload matches what packages/beam/src/lib/workspaceFileContext.ts
// emits today.
type FileExcerptPayload struct {
	Path      string `json:"path"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated,omitempty"`
}

// TerminalOutputPayload captures a user-attached block of terminal output.
type TerminalOutputPayload struct {
	SessionID   string `json:"session_id,omitempty"`
	Command     string `json:"command,omitempty"`
	Output      string `json:"output"`
	CapturedAt  string `json:"captured_at,omitempty"`
	Truncated   bool   `json:"truncated,omitempty"`
}

// OpenFilePayload is for the current file open/selected in Beam's files tree.
type OpenFilePayload struct {
	Path     string `json:"path"`
	Text     string `json:"text"`
	LineFrom int    `json:"line_from,omitempty"`
	LineTo   int    `json:"line_to,omitempty"`
}

// PlanStepPayload is a slice of plan state the user or agent wants to reason about.
type PlanStepPayload struct {
	PlanID        string `json:"plan_id"`
	Ordinal       int    `json:"ordinal"`
	Description   string `json:"description"`
	Status        string `json:"status"`
	Summary       string `json:"summary,omitempty"`
	FailureClass  string `json:"failure_class,omitempty"`
	LastFailure   string `json:"last_failure,omitempty"`
}

// CommandOutputPayload is the structured output of a command or tool run.
type CommandOutputPayload struct {
	Command  string `json:"command"`
	Output   string `json:"output"`
	ExitCode *int   `json:"exit_code,omitempty"`
}

// RuntimeStatePayload is one captured state unit pulled from the sidebar.
type RuntimeStatePayload struct {
	Name string          `json:"name"`
	Data json.RawMessage `json:"data,omitempty"`
}

// ------------------------------------------------------------------
// Renderers. Each MUST tolerate a missing-or-malformed payload by
// falling back to the raw JSON, so an adversarial client cannot break
// the turn just by sending an unexpected shape.
// ------------------------------------------------------------------

const rule = "---"

func renderFileExcerpt(payload json.RawMessage) (string, error) {
	var p FileExcerptPayload
	if err := json.Unmarshal(payload, &p); err != nil || strings.TrimSpace(p.Path) == "" {
		return fallbackJSONBody("file_excerpt", payload), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "File excerpt from %s", p.Path)
	if p.Truncated {
		b.WriteString(" (truncated)")
	}
	b.WriteString("\n")
	b.WriteString(rule)
	b.WriteString("\n")
	b.WriteString(p.Text)
	if !strings.HasSuffix(p.Text, "\n") {
		b.WriteString("\n")
	}
	b.WriteString(rule)
	return b.String(), nil
}

func renderTerminalOutput(payload json.RawMessage) (string, error) {
	var p TerminalOutputPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return fallbackJSONBody("terminal_output", payload), nil
	}
	var b strings.Builder
	b.WriteString("Recent terminal output (may be stale; the user's terminal is live and the agent did not produce this)")
	if p.CapturedAt != "" {
		fmt.Fprintf(&b, "\nCaptured: %s", p.CapturedAt)
	}
	if p.Command != "" {
		fmt.Fprintf(&b, "\nCommand: %s", p.Command)
	}
	if p.Truncated {
		b.WriteString("\n(output truncated)")
	}
	b.WriteString("\n")
	b.WriteString(rule)
	b.WriteString("\n")
	b.WriteString(p.Output)
	if !strings.HasSuffix(p.Output, "\n") {
		b.WriteString("\n")
	}
	b.WriteString(rule)
	return b.String(), nil
}

func renderOpenFile(payload json.RawMessage) (string, error) {
	var p OpenFilePayload
	if err := json.Unmarshal(payload, &p); err != nil || strings.TrimSpace(p.Path) == "" {
		return fallbackJSONBody("open_file", payload), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "File currently open in the workspace: %s", p.Path)
	if p.LineFrom > 0 && p.LineTo >= p.LineFrom {
		fmt.Fprintf(&b, " (lines %d-%d)", p.LineFrom, p.LineTo)
	}
	b.WriteString("\n")
	b.WriteString(rule)
	b.WriteString("\n")
	b.WriteString(p.Text)
	if !strings.HasSuffix(p.Text, "\n") {
		b.WriteString("\n")
	}
	b.WriteString(rule)
	return b.String(), nil
}

func renderPlanStep(payload json.RawMessage) (string, error) {
	var p PlanStepPayload
	if err := json.Unmarshal(payload, &p); err != nil || p.Ordinal == 0 {
		return fallbackJSONBody("plan_step", payload), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Plan step %d — status: %s", p.Ordinal, stringOrDash(p.Status))
	if p.FailureClass != "" {
		fmt.Fprintf(&b, " (failure_class=%s)", p.FailureClass)
	}
	b.WriteString("\n")
	b.WriteString("Description: ")
	b.WriteString(stringOrDash(p.Description))
	b.WriteString("\n")
	if p.Summary != "" {
		b.WriteString("Summary:\n")
		b.WriteString(p.Summary)
		if !strings.HasSuffix(p.Summary, "\n") {
			b.WriteString("\n")
		}
	}
	if p.LastFailure != "" {
		b.WriteString("Last failure summary:\n")
		b.WriteString(p.LastFailure)
		if !strings.HasSuffix(p.LastFailure, "\n") {
			b.WriteString("\n")
		}
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func renderCommandOutput(payload json.RawMessage) (string, error) {
	var p CommandOutputPayload
	if err := json.Unmarshal(payload, &p); err != nil || strings.TrimSpace(p.Command) == "" {
		return fallbackJSONBody("command_output", payload), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Command: %s", p.Command)
	if p.ExitCode != nil {
		fmt.Fprintf(&b, " (exit=%d)", *p.ExitCode)
	}
	b.WriteString("\n")
	b.WriteString(rule)
	b.WriteString("\n")
	b.WriteString(p.Output)
	if !strings.HasSuffix(p.Output, "\n") {
		b.WriteString("\n")
	}
	b.WriteString(rule)
	return b.String(), nil
}

func renderRuntimeState(payload json.RawMessage) (string, error) {
	var p RuntimeStatePayload
	if err := json.Unmarshal(payload, &p); err != nil || strings.TrimSpace(p.Name) == "" {
		return fallbackJSONBody("runtime_state", payload), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Captured runtime state: %s\n", p.Name)
	b.WriteString(rule)
	b.WriteString("\n")
	if len(p.Data) == 0 {
		b.WriteString("(no data)\n")
	} else {
		b.Write(p.Data)
		if !strings.HasSuffix(string(p.Data), "\n") {
			b.WriteString("\n")
		}
	}
	b.WriteString(rule)
	return b.String(), nil
}

// fallbackJSONBody is the flat body used when a payload does not match its
// kind's typed shape, or when the kind itself is unknown to the registry.
// Keeps existing clients working without forcing a schema migration.
func fallbackJSONBody(kind string, payload json.RawMessage) string {
	if len(payload) == 0 {
		return ""
	}
	return string(payload)
}

func stringOrDash(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "-"
	}
	return s
}
