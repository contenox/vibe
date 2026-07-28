package bedrock

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/contenox/beam/internal/models/modelrepo"
)

// Drives a recorded ConverseStream event sequence through relayConverseEvents
// and the engine-side assembler; the SDK's binary eventstream transport
// itself is not exercised.
func TestUnit_Bedrock_RelayConverseEvents_GoldenFixture(t *testing.T) {
	t.Parallel()

	events := make(chan types.ConverseStreamOutput, 16)
	events <- &types.ConverseStreamOutputMemberContentBlockDelta{Value: types.ContentBlockDeltaEvent{
		ContentBlockIndex: aws.Int32(0),
		Delta:             &types.ContentBlockDeltaMemberReasoningContent{Value: &types.ReasoningContentBlockDeltaMemberText{Value: "pondering"}},
	}}
	events <- &types.ConverseStreamOutputMemberContentBlockDelta{Value: types.ContentBlockDeltaEvent{
		ContentBlockIndex: aws.Int32(0),
		Delta:             &types.ContentBlockDeltaMemberText{Value: "let me "},
	}}
	events <- &types.ConverseStreamOutputMemberContentBlockDelta{Value: types.ContentBlockDeltaEvent{
		ContentBlockIndex: aws.Int32(0),
		Delta:             &types.ContentBlockDeltaMemberText{Value: "check"},
	}}
	events <- &types.ConverseStreamOutputMemberContentBlockStart{Value: types.ContentBlockStartEvent{
		ContentBlockIndex: aws.Int32(1),
		Start: &types.ContentBlockStartMemberToolUse{Value: types.ToolUseBlockStart{
			ToolUseId: aws.String("tooluse_1"),
			Name:      aws.String("fs_list"),
		}},
	}}
	events <- &types.ConverseStreamOutputMemberContentBlockDelta{Value: types.ContentBlockDeltaEvent{
		ContentBlockIndex: aws.Int32(1),
		Delta:             &types.ContentBlockDeltaMemberToolUse{Value: types.ToolUseBlockDelta{Input: aws.String(`{"path":`)}},
	}}
	events <- &types.ConverseStreamOutputMemberContentBlockDelta{Value: types.ContentBlockDeltaEvent{
		ContentBlockIndex: aws.Int32(1),
		Delta:             &types.ContentBlockDeltaMemberToolUse{Value: types.ToolUseBlockDelta{Input: aws.String(`"/x"}`)}},
	}}
	events <- &types.ConverseStreamOutputMemberMessageStop{Value: types.MessageStopEvent{
		StopReason: types.StopReasonToolUse,
	}}
	events <- &types.ConverseStreamOutputMemberMetadata{Value: types.ConverseStreamMetadataEvent{
		Usage: &types.TokenUsage{InputTokens: aws.Int32(31), OutputTokens: aws.Int32(13), TotalTokens: aws.Int32(44)},
	}}
	close(events)

	parcels := make(chan *modelrepo.StreamParcel, 32)
	go func() {
		defer close(parcels)
		relayConverseEvents(context.Background(), events, map[string]string{"fs_list": "fs.list"}, parcels)
	}()

	asm := modelrepo.NewStreamAssembler("bedrock", "test-model")
	for parcel := range parcels {
		require.NoError(t, asm.Consume(parcel))
	}
	res, err := asm.Result()
	require.NoError(t, err)

	assert.Equal(t, "let me check", res.Content)
	assert.Equal(t, "pondering", res.Thinking)
	require.Len(t, res.ToolCalls, 1)
	assert.Equal(t, "tooluse_1", res.ToolCalls[0].ID)
	assert.Equal(t, "fs.list", res.ToolCalls[0].Function.Name, "sanitized tool name must translate back")
	assert.Equal(t, `{"path":"/x"}`, res.ToolCalls[0].Function.Arguments)
	assert.Equal(t, string(types.StopReasonToolUse), res.FinishReason)
	require.NotNil(t, res.Usage)
	assert.Equal(t, 31, res.Usage.PromptTokens)
	assert.Equal(t, 13, res.Usage.CompletionTokens)
	assert.Equal(t, 44, res.Usage.TotalTokens)
}

// A stream that ends without messageStop yields no terminal parcel, so the
// assembler refuses to call it success.
func TestUnit_Bedrock_RelayConverseEvents_TruncatedStreamHasNoTerminal(t *testing.T) {
	t.Parallel()

	events := make(chan types.ConverseStreamOutput, 2)
	events <- &types.ConverseStreamOutputMemberContentBlockDelta{Value: types.ContentBlockDeltaEvent{
		ContentBlockIndex: aws.Int32(0),
		Delta:             &types.ContentBlockDeltaMemberText{Value: "partial"},
	}}
	close(events)

	parcels := make(chan *modelrepo.StreamParcel, 4)
	go func() {
		defer close(parcels)
		relayConverseEvents(context.Background(), events, nil, parcels)
	}()

	asm := modelrepo.NewStreamAssembler("bedrock", "test-model")
	for parcel := range parcels {
		require.NoError(t, asm.Consume(parcel))
	}
	_, err := asm.Result()
	require.ErrorContains(t, err, "terminal")
}
