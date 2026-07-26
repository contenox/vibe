package bedrock

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/contenox/beam/internal/models/modelrepo"
)

type bedrockStreamClient struct{ bedrockClient }

// Stream implements modelrepo.LLMStreamClient via Bedrock ConverseStream. The
// SDK decodes the binary event stream into a typed event union; relayConverse
// translates that union into raw-delta parcels per the modelrepo.StreamParcel
// contract — text / reasoning deltas, tool-call fragments (toolUse start
// carries id+name, toolUse deltas carry argument-JSON fragments), and a typed
// terminal parcel from messageStop + metadata. Assembly belongs to the
// engine-side modelrepo.StreamAssembler.
func (c *bedrockStreamClient) Stream(ctx context.Context, messages []modelrepo.Message, args ...modelrepo.ChatArgument) (<-chan *modelrepo.StreamParcel, error) {
	in, toOriginal := buildConverseInput(c.modelName, messages, chatConfigFromArgs(args), c.maxOutputTokens)
	streamIn := &bedrockruntime.ConverseStreamInput{
		ModelId:         in.ModelId,
		Messages:        in.Messages,
		System:          in.System,
		ToolConfig:      in.ToolConfig,
		InferenceConfig: in.InferenceConfig,
	}

	reportErr, reportChange, end := c.tracker.Start(ctx, "stream", "bedrock", "model", c.modelName)
	out, err := c.api.ConverseStream(ctx, streamIn)
	if err != nil {
		err = fmt.Errorf("bedrock converse-stream (model=%s): %w", c.modelName, err)
		reportErr(err)
		end()
		return nil, err
	}

	parcels := make(chan *modelrepo.StreamParcel)
	go func() {
		defer close(parcels)
		defer end()
		stream := out.GetStream()
		defer stream.Close()

		chunkCount := relayConverseEvents(ctx, stream.Events(), toOriginal, parcels)
		if err := stream.Err(); err != nil {
			reportErr(err)
			select {
			case parcels <- &modelrepo.StreamParcel{Error: fmt.Errorf("bedrock stream: %w", err)}:
			case <-ctx.Done():
			}
			return
		}
		reportChange("stream_completed", map[string]any{"chunk_count": chunkCount})
	}()
	return parcels, nil
}

// relayConverseEvents translates the ConverseStream event union into raw-delta
// parcels. toOriginal maps sanitized tool names (as sent to Bedrock) back to
// the caller's original names. It returns the number of parcels sent; the
// caller owns error reporting for the underlying stream.
//
// Bedrock indexes content blocks per message, so ContentBlockIndex groups the
// toolUse fragments of one call exactly as ToolCallDelta.Index requires. The
// terminal parcel is emitted when the stream ends: messageStop supplies the
// stop reason and the (later) metadata event supplies usage.
func relayConverseEvents(ctx context.Context, events <-chan types.ConverseStreamOutput, toOriginal map[string]string, parcels chan<- *modelrepo.StreamParcel) int {
	send := func(p *modelrepo.StreamParcel) bool {
		select {
		case parcels <- p:
			return true
		case <-ctx.Done():
			return false
		}
	}

	var (
		chunkCount   int
		stopReason   string
		usage        *modelrepo.TokenUsage
		sawTermEvent bool
	)
	for ev := range events {
		switch v := ev.(type) {
		case *types.ConverseStreamOutputMemberContentBlockStart:
			start, ok := v.Value.Start.(*types.ContentBlockStartMemberToolUse)
			if !ok {
				continue
			}
			name := aws.ToString(start.Value.Name)
			if orig, ok := toOriginal[name]; ok && orig != "" {
				name = orig
			}
			chunkCount++
			if !send(&modelrepo.StreamParcel{ToolCall: &modelrepo.ToolCallDelta{
				Index: int(aws.ToInt32(v.Value.ContentBlockIndex)),
				ID:    aws.ToString(start.Value.ToolUseId),
				Type:  "function",
				Name:  name,
			}}) {
				return chunkCount
			}

		case *types.ConverseStreamOutputMemberContentBlockDelta:
			switch d := v.Value.Delta.(type) {
			case *types.ContentBlockDeltaMemberText:
				if d.Value == "" {
					continue
				}
				chunkCount++
				if !send(&modelrepo.StreamParcel{Data: d.Value}) {
					return chunkCount
				}
			case *types.ContentBlockDeltaMemberReasoningContent:
				text, ok := d.Value.(*types.ReasoningContentBlockDeltaMemberText)
				if !ok || text.Value == "" {
					continue
				}
				chunkCount++
				if !send(&modelrepo.StreamParcel{Thinking: text.Value}) {
					return chunkCount
				}
			case *types.ContentBlockDeltaMemberToolUse:
				fragment := aws.ToString(d.Value.Input)
				if fragment == "" {
					continue
				}
				chunkCount++
				if !send(&modelrepo.StreamParcel{ToolCall: &modelrepo.ToolCallDelta{
					Index:        int(aws.ToInt32(v.Value.ContentBlockIndex)),
					ArgsFragment: fragment,
				}}) {
					return chunkCount
				}
			}

		case *types.ConverseStreamOutputMemberMessageStop:
			stopReason = string(v.Value.StopReason)
			sawTermEvent = true

		case *types.ConverseStreamOutputMemberMetadata:
			if v.Value.Usage != nil {
				usage = &modelrepo.TokenUsage{
					PromptTokens:     int(aws.ToInt32(v.Value.Usage.InputTokens)),
					CompletionTokens: int(aws.ToInt32(v.Value.Usage.OutputTokens)),
					TotalTokens:      int(aws.ToInt32(v.Value.Usage.TotalTokens)),
				}
			}
		}
	}

	if sawTermEvent {
		send(&modelrepo.StreamParcel{Terminal: &modelrepo.StreamTerminal{
			FinishReason: stopReason,
			Usage:        usage,
		}})
	}
	return chunkCount
}

var _ modelrepo.LLMStreamClient = (*bedrockStreamClient)(nil)
