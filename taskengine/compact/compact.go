// Package compact summarizes older messages of a running ChatHistory when it
// approaches the model's context window, replacing them with one synthetic
// user message of the form:
//
//	<compact-summary>
//	  ...summary text...
//	</compact-summary>
//
// Designed to be invoked at the head of a chat_completion task — i.e. between
// tool rounds in an agentic loop — so an executor that has just appended large
// tool results does not blow context on the next assistant turn.
//
// Compaction is conservative:
//   - The most recent KeepRecent messages are preserved verbatim.
//   - A single non-agentic LLM call produces the summary; it must not recurse.
//   - On consecutive failures the State circuit-breaks and Maybe becomes a
//     no-op for the rest of the step, letting Shift / hard truncation take over.
package compact

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Policy controls when and how Maybe summarizes history.
type Policy struct {
	// TriggerFraction is the fraction of token_limit that triggers compaction.
	// Defaults to 0.85 when zero.
	TriggerFraction float64 `yaml:"trigger_fraction,omitempty" json:"trigger_fraction,omitempty"`
	// KeepRecent is the number of trailing messages preserved verbatim.
	// Defaults to 10 when zero.
	KeepRecent int `yaml:"keep_recent,omitempty" json:"keep_recent,omitempty"`
	// Model is the LLM used for compaction. Empty means the caller's default.
	Model string `yaml:"model,omitempty" json:"model,omitempty"`
	// Provider, if non-empty, restricts the compaction call to a provider type.
	Provider string `yaml:"provider,omitempty" json:"provider,omitempty"`
	// MaxFailures is the consecutive-failure threshold that disables compaction
	// for the remainder of the step. Defaults to 3 when zero.
	MaxFailures int `yaml:"max_failures,omitempty" json:"max_failures,omitempty"`
	// MinReplacedMessages requires at least this many messages to be replaced
	// for a compaction to be considered worthwhile. Defaults to 4.
	MinReplacedMessages int `yaml:"min_replaced_messages,omitempty" json:"min_replaced_messages,omitempty"`
}

func (p Policy) triggerFraction() float64 {
	if p.TriggerFraction <= 0 {
		return 0.85
	}
	return p.TriggerFraction
}

func (p Policy) keepRecent() int {
	if p.KeepRecent <= 0 {
		return 10
	}
	return p.KeepRecent
}

func (p Policy) maxFailures() int {
	if p.MaxFailures <= 0 {
		return 3
	}
	return p.MaxFailures
}

func (p Policy) minReplaced() int {
	if p.MinReplacedMessages <= 0 {
		return 4
	}
	return p.MinReplacedMessages
}

// State carries circuit-breaker counters across Maybe calls within one step.
// Safe for concurrent reads via the embedded mutex.
type State struct {
	mu                  sync.Mutex
	ConsecutiveFailures int
	Disabled            bool
	LastCompactedAt     time.Time
	Compactions         int
}

// Snapshot returns a shallow copy for inspection. Safe for concurrent use.
func (s *State) Snapshot() (failures, compactions int, disabled bool, lastAt time.Time) {
	if s == nil {
		return 0, 0, false, time.Time{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ConsecutiveFailures, s.Compactions, s.Disabled, s.LastCompactedAt
}

// Message is the minimal shape compact needs from a chat history. It mirrors
// the relevant fields of taskengine.Message without importing taskengine,
// so this package stays a leaf utility.
type Message struct {
	Role    string
	Content string
	// HasToolCalls is true when the message originated from an assistant turn
	// that requested tool calls. Such messages cannot safely be elided in
	// isolation because the following tool-result message references them.
	HasToolCalls bool
	// ToolCallID is non-empty for tool-result messages.
	ToolCallID string
}

// Caller invokes a single non-streaming LLM call and returns the assistant text.
// The compact package does not import any provider package; the wiring code
// (taskexec.go) supplies a closure that bridges to llmrepo and llmretry.
type Caller func(ctx context.Context, model, systemInstruction, prompt string) (string, error)

// TokenCounter returns the token count for s under model. Used to estimate
// pre/post-compaction usage. May return (0, nil) if no counter is available.
type TokenCounter func(ctx context.Context, model, s string) (int, error)

// Result reports what Maybe did. Compacted is true only when messages were
// actually replaced; UsageBefore/After are the totalled token counts when a
// counter was supplied.
//
// When Compacted is true the caller must apply the splice on its own typed
// message slice as:
//
//	out := append([]M{}, msgs[:ReplaceFrom]...)
//	out = append(out, syntheticUserMessage(SyntheticContent))
//	out = append(out, msgs[ReplaceTo:]...)
//
// SyntheticContent is the user-visible content for the replacement message
// (already wrapped in <compact-summary> tags).
type Result struct {
	Compacted        bool
	Replaced         int
	ReplaceFrom      int
	ReplaceTo        int
	SyntheticContent string
	UsageBefore      int
	UsageAfter       int
	SummaryChars     int
	Skipped          string // human-readable reason when Compacted is false
}

// Maybe inspects msgs and decides whether to compact. When triggered it calls
// the LLM, builds the synthetic <compact-summary> content, and returns a
// splice plan (ReplaceFrom..ReplaceTo, SyntheticContent) for the caller to
// apply on its own typed message slice.
//
// Maybe never mutates msgs. The caller owns the typed slice (e.g.
// taskengine.Message with CallTools, Timestamps, etc.) and applies the splice
// while preserving any per-message metadata on the kept tail.
func Maybe(
	ctx context.Context,
	p Policy,
	st *State,
	msgs []Message,
	tokenLimit int,
	count TokenCounter,
	call Caller,
) (Result, error) {
	if st == nil {
		st = &State{}
	}
	st.mu.Lock()
	disabled := st.Disabled
	st.mu.Unlock()
	if disabled {
		return Result{Skipped: "disabled by circuit breaker"}, nil
	}
	if tokenLimit <= 0 {
		return Result{Skipped: "no token limit"}, nil
	}
	keep := p.keepRecent()
	// Identify the leading run of system messages — they are never replaced.
	leadSystem := 0
	for leadSystem < len(msgs) && msgs[leadSystem].Role == "system" {
		leadSystem++
	}
	if len(msgs)-leadSystem <= keep+p.minReplaced() {
		return Result{Skipped: "history too short"}, nil
	}

	// Estimate current usage. Fall back to char-count/4 when no counter exists
	// or counting fails — both are tolerable since compaction is best-effort.
	usage, err := totalTokens(ctx, msgs, count, p.Model)
	if err != nil || usage == 0 {
		usage = approxTokens(msgs)
	}
	threshold := int(p.triggerFraction() * float64(tokenLimit))
	if usage < threshold {
		return Result{UsageBefore: usage, Skipped: fmt.Sprintf("under threshold (%d < %d)", usage, threshold)}, nil
	}

	// Determine how many messages between leadSystem and len-keep to replace,
	// but never sever a tool-call ↔ tool-result pair.
	cutoff := safeCutoff(msgs, len(msgs)-keep)
	if cutoff-leadSystem < p.minReplaced() {
		return Result{
			UsageBefore: usage,
			Skipped:     fmt.Sprintf("safe cutoff %d (after %d system) below min %d", cutoff, leadSystem, p.minReplaced()),
		}, nil
	}

	older := msgs[leadSystem:cutoff]
	prompt := buildCompactionPrompt(older)
	summary, callErr := call(ctx, p.Model, compactionSystemInstruction(), prompt)
	if callErr != nil {
		st.mu.Lock()
		st.ConsecutiveFailures++
		if st.ConsecutiveFailures >= p.maxFailures() {
			st.Disabled = true
		}
		st.mu.Unlock()
		return Result{UsageBefore: usage, Skipped: "compaction call failed"}, callErr
	}
	summary = strings.TrimSpace(summary)
	if summary == "" {
		st.mu.Lock()
		st.ConsecutiveFailures++
		if st.ConsecutiveFailures >= p.maxFailures() {
			st.Disabled = true
		}
		st.mu.Unlock()
		return Result{UsageBefore: usage, Skipped: "empty summary"}, fmt.Errorf("compact: empty summary returned")
	}

	st.mu.Lock()
	st.ConsecutiveFailures = 0
	st.Compactions++
	st.LastCompactedAt = time.Now()
	st.mu.Unlock()

	synthetic := "<compact-summary>\n" + summary + "\n</compact-summary>"

	return Result{
		Compacted:        true,
		Replaced:         cutoff - leadSystem,
		ReplaceFrom:      leadSystem,
		ReplaceTo:        cutoff,
		SyntheticContent: synthetic,
		UsageBefore:      usage,
		// UsageAfter is approximated by replacing the older messages with the
		// summary's char-length-based estimate. The real post-counter is the
		// caller's responsibility once it applies the splice.
		UsageAfter:   usage - approxTokens(older) + len(synthetic)/4,
		SummaryChars: len(summary),
	}, nil
}

// safeCutoff returns the largest n <= want such that msgs[:n] contains no
// dangling assistant tool-call message whose tool-result is in msgs[n:].
// In practice we walk backwards from want until the message at the boundary
// is a clean break (tool result followed by a non-tool message).
func safeCutoff(msgs []Message, want int) int {
	if want < 0 {
		return 0
	}
	if want > len(msgs) {
		want = len(msgs)
	}
	// Walk backwards from want, retreating past any tool-result so its parent
	// tool-call message is also kept (or also dropped) atomically.
	for i := want; i > 0; i-- {
		if i < len(msgs) && msgs[i].ToolCallID != "" {
			// Boundary lands on a tool-result; pull boundary back so the
			// preceding assistant message (with tool calls) and its result
			// stay together in the post-boundary portion.
			continue
		}
		if i > 0 && msgs[i-1].HasToolCalls {
			// Last kept message is an assistant tool-call; pull the boundary
			// forward... but here we are walking backward, so retreat further
			// to push that assistant turn into the kept tail.
			continue
		}
		return i
	}
	return 0
}

func buildCompactionPrompt(older []Message) string {
	var b strings.Builder
	b.WriteString("Summarize the following conversation transcript so that an assistant can continue the task without losing critical context. Focus on:\n")
	b.WriteString("- decisions made and their rationale\n")
	b.WriteString("- files read or modified, and key observations from them\n")
	b.WriteString("- tool results that influenced subsequent steps\n")
	b.WriteString("- unresolved questions or pending sub-tasks\n\n")
	b.WriteString("Omit verbatim tool output and chit-chat. Output only the summary, no preamble.\n\n")
	b.WriteString("--- transcript ---\n")
	for _, m := range older {
		role := m.Role
		if role == "" {
			role = "?"
		}
		b.WriteString("[")
		b.WriteString(role)
		if m.ToolCallID != "" {
			b.WriteString(" tool_result")
		} else if m.HasToolCalls {
			b.WriteString(" tool_call")
		}
		b.WriteString("] ")
		b.WriteString(strings.TrimSpace(m.Content))
		b.WriteString("\n")
	}
	return b.String()
}

func compactionSystemInstruction() string {
	return "You are a conversation compaction assistant. Produce a concise, high-density summary that preserves the information a downstream agent needs to continue its work. Do not call tools. Do not add commentary about the summarization itself. Reply with the summary text only."
}

func totalTokens(ctx context.Context, msgs []Message, count TokenCounter, model string) (int, error) {
	if count == nil {
		return 0, nil
	}
	total := 0
	for _, m := range msgs {
		n, err := count(ctx, model, m.Content)
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

func approxTokens(msgs []Message) int {
	chars := 0
	for _, m := range msgs {
		chars += len(m.Content)
	}
	// ~4 chars/token is a reasonable cross-tokenizer approximation.
	return chars / 4
}
