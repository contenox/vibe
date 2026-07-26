package bedrock

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/contenox/beam/internal/models/modelrepo"
)

type bedrockStreamClient struct{ bedrockClient }

// Stream implements modelrepo.LLMStreamClient via Bedrock ConverseStream. The
// SDK decodes the binary event stream into a typed event union; we forward
// visible text deltas as parcels (parity with the other stream clients, whose
// StreamParcel carries no tool-call field — tool calls come from non-stream Chat).
func (c *bedrockStreamClient) Stream(ctx context.Context, messages []modelrepo.Message, args ...modelrepo.ChatArgument) (<-chan *modelrepo.StreamParcel, error) {
	in, _ := buildConverseInput(c.modelName, messages, chatConfigFromArgs(args), c.maxOutputTokens)
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

		var chunkCount int
		for ev := range stream.Events() {
			delta, ok := ev.(*types.ConverseStreamOutputMemberContentBlockDelta)
			if !ok {
				continue
			}
			td, ok := delta.Value.Delta.(*types.ContentBlockDeltaMemberText)
			if !ok || td.Value == "" {
				continue
			}
			chunkCount++
			select {
			case parcels <- &modelrepo.StreamParcel{Data: td.Value}:
			case <-ctx.Done():
				return
			}
		}
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

var _ modelrepo.LLMStreamClient = (*bedrockStreamClient)(nil)
