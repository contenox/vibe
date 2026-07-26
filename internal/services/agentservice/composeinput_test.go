package agentservice

import (
	"strings"
	"testing"
)

func TestUnit_ComposeUserInput_PlainInputPassesThrough(t *testing.T) {
	got := ComposeUserInput("hello", nil)
	if got != "hello" {
		t.Fatalf("expected plain input unchanged, got %q", got)
	}
}

func TestUnit_ComposeUserInput_ContextArtifactsInjected(t *testing.T) {
	contextBundle := map[string]any{
		"artifacts": []any{
			map[string]any{"kind": "file_excerpt", "payload": "func main() {}"},
			map[string]any{"kind": "terminal_output", "payload": "exit 0"},
		},
	}
	got := ComposeUserInput("what does this do?", contextBundle)

	if !strings.HasPrefix(got, "Additional context:\n") {
		t.Fatalf("expected context block prefix, got %q", got)
	}
	for _, want := range []string{"[file_excerpt] func main() {}", "[terminal_output] exit 0"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected %q in composed input, got %q", want, got)
		}
	}
	if !strings.HasSuffix(got, "\n\nwhat does this do?") {
		t.Fatalf("expected original input at the end, got %q", got)
	}
}

// The context block must be strictly *prepended* — the client's optimistic
// echo matcher relies on the original input remaining a suffix.
func TestUnit_ComposeUserInput_OriginalInputIsSuffix(t *testing.T) {
	contextBundle := map[string]any{
		"artifacts": []any{
			map[string]any{"kind": "plan_step", "payload": "step 1"},
		},
	}
	input := "go do the thing"
	got := ComposeUserInput(input, contextBundle)
	if !strings.HasSuffix(got, input) {
		t.Fatalf("composed input must end with the original input, got %q", got)
	}
}

func TestUnit_ComposeUserInput_MalformedContextIgnored(t *testing.T) {
	for name, bundle := range map[string]map[string]any{
		"empty artifacts":     {"artifacts": []any{}},
		"wrong artifact type": {"artifacts": "not-a-list"},
		"non-map entries":     {"artifacts": []any{"just-a-string"}},
		"no artifacts key":    {"other": true},
	} {
		if got := ComposeUserInput("hi", bundle); got != "hi" {
			t.Fatalf("%s: expected input unchanged, got %q", name, got)
		}
	}
}
