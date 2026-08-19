package scriptedtest

import (
	"strings"
	"testing"
)

// The script file is user-facing input, so its contract is pinned here: what a
// document may leave out, and what it may not get away with.
func TestUnit_ParseScriptDocument(t *testing.T) {
	t.Run("defaults name the fake", func(t *testing.T) {
		script, err := Parse("/tmp/dialog.json", []byte(`{"turns":[{"text":"hi"}]}`))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if script.Model != DefaultModelName {
			t.Errorf("model = %q, want %q so every surface printing a model name says test", script.Model, DefaultModelName)
		}
		if script.ContextLength != defaultContextLength {
			t.Errorf("context length = %d, want %d", script.ContextLength, defaultContextLength)
		}
	})

	t.Run("a bare turn list is a script", func(t *testing.T) {
		script, err := Parse("/tmp/dialog.json", []byte(`[{"text":"one"},{"text":"two"}]`))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if len(script.Turns) != 2 {
			t.Fatalf("turns = %d, want 2", len(script.Turns))
		}
	})

	t.Run("object arguments are handed over as JSON", func(t *testing.T) {
		script, err := Parse("/tmp/dialog.json", []byte(`{"turns":[{"tool_calls":[{"name":"git_diff","arguments":{"path":"."}}]}]}`))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		calls := toolCallsFor(script.Turns[0], 0)
		if got := calls[0].Function.Arguments; got != `{"path":"."}` {
			t.Errorf("arguments = %q, want the object verbatim", got)
		}
		if calls[0].ID == "" {
			t.Error("an omitted id must be generated: the engine pairs tool results by id")
		}
	})

	t.Run("string arguments stay raw", func(t *testing.T) {
		script, err := Parse("/tmp/dialog.json", []byte(`{"turns":[{"tool_calls":[{"name":"git_diff","arguments":"not json at all"}]}]}`))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		calls := toolCallsFor(script.Turns[0], 0)
		if got := calls[0].Function.Arguments; got != "not json at all" {
			t.Errorf("arguments = %q, want the raw string so malformed arguments stay scriptable", got)
		}
	})

	t.Run("missing arguments become an empty object", func(t *testing.T) {
		script, err := Parse("/tmp/dialog.json", []byte(`{"turns":[{"tool_calls":[{"name":"git_status"}]}]}`))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if got := toolCallsFor(script.Turns[0], 0)[0].Function.Arguments; got != "{}" {
			t.Errorf("arguments = %q, want {}", got)
		}
	})

	for name, doc := range map[string]string{
		"no turns":       `{"turns":[]}`,
		"empty turn":     `{"turns":[{}]}`,
		"nameless call":  `{"turns":[{"tool_calls":[{"arguments":{}}]}]}`,
		"unusable args":  `{"turns":[{"tool_calls":[{"name":"x","arguments":42}]}]}`,
		"not a document": `nonsense`,
	} {
		t.Run("rejects "+name, func(t *testing.T) {
			_, err := Parse("/tmp/dialog.json", []byte(doc))
			if err == nil {
				t.Fatal("an unusable script must be refused when it loads, not when the run is half done")
			}
			if !strings.Contains(err.Error(), "/tmp/dialog.json") {
				t.Errorf("error %q must name the script file", err)
			}
		})
	}
}

// Exhaustion is the whole safety property: no invented reply, and a message
// that says which file ran out and on which turn.
func TestUnit_ScriptExhaustionNamesFileAndTurn(t *testing.T) {
	script, err := Parse("/tmp/dialog.json", []byte(`{"turns":[{"text":"only one"}]}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, _, err := script.Next("chat"); err != nil {
		t.Fatalf("turn 0 is scripted: %v", err)
	}
	_, index, err := script.Next("stream")
	if err == nil {
		t.Fatal("past the last turn the script must fail, never fall back to a canned reply")
	}
	if index != 1 {
		t.Errorf("index = %d, want 1", index)
	}
	for _, want := range []string{"/tmp/dialog.json", "turn 1", "stream", "exhausted"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must contain %q", err, want)
		}
	}
}
