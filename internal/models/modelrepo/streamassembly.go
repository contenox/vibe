package modelrepo

import (
	"fmt"
	"sort"
	"strings"
)

// StreamAssembler is the ONE place streamed deltas become a completed
// response. It is engine policy: taskexec (and the fixture harnesses that
// prove providers honor the raw-delta contract) consume it — providers must
// never assemble their own streams. It lives in this package only so that
// providers' tests can drive "real adapter → assembler" without an import
// cycle through the runtime-state catalog imports.
//
// Invariants enforced:
//   - tool-call fragments are grouped by ToolCallDelta.Index;
//   - the atomic fields (ID, Type, Name) of one index are set at most once —
//     a conflicting second value is a hard error carrying provider + model;
//   - ArgsFragment pieces are concatenated in arrival order;
//   - the finished calls are ordered by index;
//   - a successful stream carries exactly one Terminal parcel and nothing
//     after it; a stream that ends without Terminal or Error is an error.
type StreamAssembler struct {
	providerType string
	modelName    string

	content  strings.Builder
	thinking strings.Builder
	toolAcc  map[int]*streamToolAcc
	usage    TokenUsage
	sawUsage bool
	terminal *StreamTerminal
	err      error
}

type streamToolAcc struct {
	id           string
	typ          string
	name         string
	args         strings.Builder
	providerMeta map[string]string
}

// StreamResult is the assembled outcome of one stream.
type StreamResult struct {
	Content   string
	Thinking  string
	ToolCalls []ToolCall
	// FinishReason is the provider's verbatim finish reason from the Terminal
	// parcel — length/content_filter class values included, so callers can no
	// longer mistake a truncated or filtered response for a complete one.
	FinishReason string
	// Usage merges mid-stream Usage parcels with the Terminal usage; nil when
	// the provider reported none.
	Usage *TokenUsage
}

// NewStreamAssembler returns an assembler whose errors name the provider and
// model, so a contract violation is attributable without a debugger.
func NewStreamAssembler(providerType, modelName string) *StreamAssembler {
	return &StreamAssembler{
		providerType: providerType,
		modelName:    modelName,
		toolAcc:      map[int]*streamToolAcc{},
	}
}

func (a *StreamAssembler) violation(format string, args ...any) error {
	return fmt.Errorf("stream contract violation (provider=%s, model=%s): "+format,
		append([]any{a.providerType, a.modelName}, args...)...)
}

// Consume folds one parcel into the assembly. The first error — a provider
// Error parcel or a contract violation — is sticky and re-returned.
func (a *StreamAssembler) Consume(p *StreamParcel) error {
	if a.err != nil {
		return a.err
	}
	if p == nil {
		a.err = a.violation("nil parcel")
		return a.err
	}
	if p.Error != nil {
		a.err = fmt.Errorf("stream failed (provider=%s, model=%s): %w", a.providerType, a.modelName, p.Error)
		return a.err
	}
	if a.terminal != nil {
		a.err = a.violation("parcel received after the terminal parcel")
		return a.err
	}

	switch {
	case p.Terminal != nil:
		a.terminal = p.Terminal
		if p.Terminal.Usage != nil {
			a.mergeUsage(*p.Terminal.Usage)
		}
	case p.ToolCall != nil:
		if err := a.consumeToolCall(p.ToolCall); err != nil {
			a.err = err
			return a.err
		}
	case p.Usage != nil:
		a.mergeUsage(*p.Usage)
	default:
		a.content.WriteString(p.Data)
		a.thinking.WriteString(p.Thinking)
	}
	return nil
}

// consumeToolCall applies one fragment under the atomic-field rules: ID, Type,
// and Name may each be set once per index; a differing second value means the
// provider mixed up its fragment grouping and the result cannot be trusted.
func (a *StreamAssembler) consumeToolCall(d *ToolCallDelta) error {
	acc := a.toolAcc[d.Index]
	if acc == nil {
		acc = &streamToolAcc{}
		a.toolAcc[d.Index] = acc
	}
	set := func(slot *string, field, incoming string) error {
		if incoming == "" {
			return nil
		}
		if *slot != "" && *slot != incoming {
			return a.violation("tool call %d: conflicting %s (%q vs %q)", d.Index, field, *slot, incoming)
		}
		*slot = incoming
		return nil
	}
	if err := set(&acc.id, "id", d.ID); err != nil {
		return err
	}
	if err := set(&acc.typ, "type", d.Type); err != nil {
		return err
	}
	if err := set(&acc.name, "name", d.Name); err != nil {
		return err
	}
	acc.args.WriteString(d.ArgsFragment)
	for k, v := range d.ProviderMeta {
		if acc.providerMeta == nil {
			acc.providerMeta = map[string]string{}
		}
		acc.providerMeta[k] = v
	}
	return nil
}

func (a *StreamAssembler) mergeUsage(u TokenUsage) {
	a.sawUsage = true
	if u.PromptTokens != 0 {
		a.usage.PromptTokens = u.PromptTokens
	}
	if u.CompletionTokens != 0 {
		a.usage.CompletionTokens = u.CompletionTokens
	}
	if u.TotalTokens != 0 {
		a.usage.TotalTokens = u.TotalTokens
	}
	if u.CacheReadTokens != 0 {
		a.usage.CacheReadTokens = u.CacheReadTokens
	}
	if u.CacheWriteTokens != 0 {
		a.usage.CacheWriteTokens = u.CacheWriteTokens
	}
}

// Result finalizes the assembly. It fails when the stream errored, when a
// fragment group is unusable (no name), or when the stream ended without a
// Terminal parcel — a silently truncated connection must not read as success.
func (a *StreamAssembler) Result() (StreamResult, error) {
	if a.err != nil {
		return StreamResult{}, a.err
	}
	if a.terminal == nil {
		return StreamResult{}, a.violation("stream ended without a terminal parcel")
	}

	indexes := make([]int, 0, len(a.toolAcc))
	for idx := range a.toolAcc {
		indexes = append(indexes, idx)
	}
	sort.Ints(indexes)

	var calls []ToolCall
	for _, idx := range indexes {
		acc := a.toolAcc[idx]
		if acc.name == "" {
			return StreamResult{}, a.violation("tool call %d has no name", idx)
		}
		typ := acc.typ
		if typ == "" {
			typ = "function"
		}
		args := acc.args.String()
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		tc := ToolCall{ID: acc.id, Type: typ, ProviderMeta: acc.providerMeta}
		tc.Function.Name = acc.name
		tc.Function.Arguments = args
		calls = append(calls, tc)
	}

	res := StreamResult{
		Content:      a.content.String(),
		Thinking:     a.thinking.String(),
		ToolCalls:    calls,
		FinishReason: a.terminal.FinishReason,
	}
	if a.sawUsage {
		u := a.usage
		res.Usage = &u
	}
	return res, nil
}
