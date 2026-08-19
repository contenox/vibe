package scriptedtest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	// DefaultModelName is what the catalog exposes when a script names no model; it says "test" so every surface that prints a model name says it too.
	DefaultModelName = "scripted-test"

	defaultContextLength   = 32768
	defaultEmbedDimensions = 64
)

type Capabilities struct {
	Chat   *bool `json:"chat,omitempty"`
	Stream *bool `json:"stream,omitempty"`
	Prompt *bool `json:"prompt,omitempty"`
	Embed  *bool `json:"embed,omitempty"`
	Think  *bool `json:"think,omitempty"`
	Vision *bool `json:"vision,omitempty"`
	Audio  *bool `json:"audio,omitempty"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens,omitempty"`
	CompletionTokens int `json:"completion_tokens,omitempty"`
	TotalTokens      int `json:"total_tokens,omitempty"`
}

type ToolCall struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name"`
	// Arguments is a JSON object, or a JSON string holding raw argument text.
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

type Turn struct {
	Text         string     `json:"text,omitempty"`
	Thinking     string     `json:"thinking,omitempty"`
	ToolCalls    []ToolCall `json:"tool_calls,omitempty"`
	FinishReason string     `json:"finish_reason,omitempty"`
	Usage        *Usage     `json:"usage,omitempty"`
}

type document struct {
	Model           string        `json:"model,omitempty"`
	ContextLength   int           `json:"context_length,omitempty"`
	MaxOutputTokens int           `json:"max_output_tokens,omitempty"`
	EmbedDimensions int           `json:"embed_dimensions,omitempty"`
	Capabilities    *Capabilities `json:"capabilities,omitempty"`
	Turns           []Turn        `json:"turns"`
}

// Script replays one recorded dialog; its cursor is process-global per file, so consecutive model turns are consumed in order across the whole chain run.
type Script struct {
	Path            string
	Model           string
	ContextLength   int
	MaxOutputTokens int
	EmbedDimensions int
	Capabilities    Capabilities
	Turns           []Turn

	mu     sync.Mutex
	cursor int
}

type cacheEntry struct {
	script  *Script
	modTime int64
	size    int64
}

var (
	cacheMu sync.Mutex
	cache   = map[string]*cacheEntry{}
)

// ResolvePath accepts a bare path or a "file://" URL.
func ResolvePath(baseURL string) (string, error) {
	raw := strings.TrimSpace(baseURL)
	raw = strings.TrimPrefix(raw, "file://")
	if raw == "" {
		return "", fmt.Errorf("scripted-test backend has no script path: register it with 'contenox backend add <name> --type scripted-test --script ./dialog.json'")
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		return "", fmt.Errorf("scripted-test script path %q is unusable: %w", raw, err)
	}
	return abs, nil
}

// Load returns the shared Script for path, reloading (and rewinding) it whenever the file changed on disk.
func Load(path string) (*Script, error) {
	abs, err := ResolvePath(path)
	if err != nil {
		return nil, err
	}

	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("scripted-test script %q cannot be read: %w", abs, err)
	}

	cacheMu.Lock()
	defer cacheMu.Unlock()
	if entry, ok := cache[abs]; ok && entry.modTime == info.ModTime().UnixNano() && entry.size == info.Size() {
		return entry.script, nil
	}

	raw, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("scripted-test script %q cannot be read: %w", abs, err)
	}
	script, err := Parse(abs, raw)
	if err != nil {
		return nil, err
	}
	cache[abs] = &cacheEntry{script: script, modTime: info.ModTime().UnixNano(), size: info.Size()}
	return script, nil
}

func Parse(path string, raw []byte) (*Script, error) {
	doc := document{}
	trimmed := strings.TrimSpace(string(raw))
	switch {
	case trimmed == "":
		return nil, fmt.Errorf("scripted-test script %q is empty", path)
	case strings.HasPrefix(trimmed, "["):
		if err := json.Unmarshal(raw, &doc.Turns); err != nil {
			return nil, fmt.Errorf("scripted-test script %q is not a valid turn list: %w", path, err)
		}
	default:
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("scripted-test script %q is not a valid script: %w", path, err)
		}
	}

	if len(doc.Turns) == 0 {
		return nil, fmt.Errorf("scripted-test script %q declares no turns", path)
	}
	for i, turn := range doc.Turns {
		if turn.Text == "" && turn.Thinking == "" && len(turn.ToolCalls) == 0 {
			return nil, fmt.Errorf("scripted-test script %q turn %d is empty: give it \"text\", \"thinking\" or \"tool_calls\"", path, i)
		}
		for j, call := range turn.ToolCalls {
			if strings.TrimSpace(call.Name) == "" {
				return nil, fmt.Errorf("scripted-test script %q turn %d tool call %d has no \"name\"", path, i, j)
			}
			if _, err := callArguments(call); err != nil {
				return nil, fmt.Errorf("scripted-test script %q turn %d tool call %q: %w", path, i, call.Name, err)
			}
		}
	}

	script := &Script{
		Path:            path,
		Model:           strings.TrimSpace(doc.Model),
		ContextLength:   doc.ContextLength,
		MaxOutputTokens: doc.MaxOutputTokens,
		EmbedDimensions: doc.EmbedDimensions,
		Turns:           doc.Turns,
	}
	if script.Model == "" {
		script.Model = DefaultModelName
	}
	if script.ContextLength <= 0 {
		script.ContextLength = defaultContextLength
	}
	if script.EmbedDimensions <= 0 {
		script.EmbedDimensions = defaultEmbedDimensions
	}
	if doc.Capabilities != nil {
		script.Capabilities = *doc.Capabilities
	}
	return script, nil
}

// Next consumes the turn the dialog is standing on; past the last turn it fails naming the script and the turn index instead of inventing a reply.
func (s *Script) Next(operation string) (Turn, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := s.cursor
	if index >= len(s.Turns) {
		return Turn{}, index, fmt.Errorf(
			"scripted-test script %q is exhausted: %s asked for turn %d but the script holds %d turn(s); add the missing turn or shorten the run",
			s.Path, operation, index, len(s.Turns))
	}
	s.cursor++
	return s.Turns[index], index, nil
}

func (s *Script) Position() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cursor
}

func flag(v *bool, fallback bool) bool {
	if v == nil {
		return fallback
	}
	return *v
}

func callArguments(call ToolCall) (string, error) {
	raw := strings.TrimSpace(string(call.Arguments))
	if raw == "" || raw == "null" {
		return "{}", nil
	}
	if strings.HasPrefix(raw, "\"") {
		var literal string
		if err := json.Unmarshal(call.Arguments, &literal); err != nil {
			return "", fmt.Errorf("\"arguments\" is not a readable JSON string: %w", err)
		}
		if strings.TrimSpace(literal) == "" {
			return "{}", nil
		}
		return literal, nil
	}
	if !strings.HasPrefix(raw, "{") {
		return "", fmt.Errorf("\"arguments\" must be a JSON object or a JSON string, got %s", raw)
	}
	return raw, nil
}
