package chatservice

import (
	"context"
	"fmt"

	"github.com/contenox/beam/internal/kernel/taskengine"
)

// ChainExecutor runs a task chain over a history. It is satisfied by
// enginesvc.Engine.TaskService (execservice.TasksEnvService) — the seam lets
// CompactHistory stay free of the engine and cobra wiring so both the CLI
// (`session fork --summary`) and the ACP `/compact` command can call it.
type ChainExecutor interface {
	Execute(ctx context.Context, chain *taskengine.TaskChainDefinition, input any, inputType taskengine.DataType) (any, taskengine.DataType, []taskengine.CapturedStateUnit, error)
}

// CompactHistory summarizes the older portion of a conversation into a single
// <compact-summary> user message. Leading system messages and the last `keep`
// messages are preserved verbatim; everything between them is replaced by the
// summary the chain produces.
//
// The caller is responsible for setting the chain's template vars (model,
// provider, …) on ctx via taskengine.WithTemplateVars before calling.
func CompactHistory(ctx context.Context, exec ChainExecutor, chain *taskengine.TaskChainDefinition, history []taskengine.Message, keep int) ([]taskengine.Message, error) {
	sysEnd := 0
	for sysEnd < len(history) && history[sysEnd].Role == "system" {
		sysEnd++
	}
	if len(history)-sysEnd <= keep {
		return nil, fmt.Errorf("session too short to summarize (have %d non-system messages, keep=%d)", len(history)-sysEnd, keep)
	}
	compactEnd := len(history) - keep
	toCompact := taskengine.ChatHistory{Messages: history[sysEnd:compactEnd]}

	out, _, _, err := exec.Execute(ctx, chain, toCompact, taskengine.DataTypeChatHistory)
	if err != nil {
		return nil, fmt.Errorf("compaction chain failed: %w", err)
	}
	compactHist, ok := out.(taskengine.ChatHistory)
	if !ok || len(compactHist.Messages) == 0 {
		return nil, fmt.Errorf("compaction returned empty result")
	}
	summaryContent := compactHist.Messages[len(compactHist.Messages)-1].Content

	// Stamp the summary with the timestamp of the last compacted message so it
	// sorts into the gap it fills — messages are persisted and reloaded by
	// added_at ASC, so time.Now() would float the summary past the kept recent
	// messages and corrupt the conversation order. compactEnd-1 >= sysEnd is
	// guaranteed by the keep check above.
	summaryTimestamp := history[compactEnd-1].Timestamp

	spliced := make([]taskengine.Message, 0, sysEnd+1+keep)
	spliced = append(spliced, history[:sysEnd]...)
	spliced = append(spliced, taskengine.Message{
		Role:      "user",
		Content:   fmt.Sprintf("<compact-summary>\n%s\n</compact-summary>", summaryContent),
		Timestamp: summaryTimestamp,
	})
	spliced = append(spliced, history[compactEnd:]...)
	return spliced, nil
}
