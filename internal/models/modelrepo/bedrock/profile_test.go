package bedrock

import (
	"errors"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	bedrocktypes "github.com/aws/aws-sdk-go-v2/service/bedrock/types"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	runtimetypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/contenox/contenox/internal/models/modelrepo"
	"github.com/stretchr/testify/require"
)

// ON_DEMAND models keep their base id; profile-only models resolve to the
// geo-prefixed system profile (preferring geographic over global); models
// with neither are excluded from the catalog.
func TestUnit_ResolveInvocableModelID(t *testing.T) {
	profiles := []string{
		"us.anthropic.claude-sonnet-4-5-20250929-v1:0",
		"global.anthropic.claude-sonnet-4-5-20250929-v1:0",
		"us.meta.llama4-maverick-17b-instruct-v1:0",
	}

	id, ok := resolveInvocableModelID("anthropic.claude-3-haiku-20240307-v1:0",
		[]bedrocktypes.InferenceType{bedrocktypes.InferenceTypeOnDemand}, profiles)
	require.True(t, ok)
	require.Equal(t, "anthropic.claude-3-haiku-20240307-v1:0", id, "ON_DEMAND models invoke by base id")

	id, ok = resolveInvocableModelID("anthropic.claude-sonnet-4-5-20250929-v1:0",
		[]bedrocktypes.InferenceType{"INFERENCE_PROFILE"}, profiles)
	require.True(t, ok)
	require.Equal(t, "us.anthropic.claude-sonnet-4-5-20250929-v1:0", id,
		"profile-only models resolve to the geographic profile, not global")

	_, ok = resolveInvocableModelID("anthropic.claude-opus-4-6-v1",
		[]bedrocktypes.InferenceType{"INFERENCE_PROFILE"}, profiles)
	require.False(t, ok, "a profile-only model without a matching profile is uninvocable and must be excluded")

	id, ok = resolveInvocableModelID("cohere.command-r-plus-v1:0",
		[]bedrocktypes.InferenceType{bedrocktypes.InferenceTypeProvisioned, bedrocktypes.InferenceTypeOnDemand}, nil)
	require.True(t, ok)
	require.Equal(t, "cohere.command-r-plus-v1:0", id)

	id, ok = resolveInvocableModelID("anthropic.claude-sonnet-4-5-20250929-v1:0",
		nil, []string{"global.anthropic.claude-sonnet-4-5-20250929-v1:0"})
	require.True(t, ok)
	require.Equal(t, "global.anthropic.claude-sonnet-4-5-20250929-v1:0", id,
		"global profile is used when no geographic one exists")
}

func TestUnit_BedrockBaseModelID(t *testing.T) {
	require.Equal(t, "anthropic.claude-sonnet-4-5-v1:0", bedrockBaseModelID("us.anthropic.claude-sonnet-4-5-v1:0"))
	require.Equal(t, "anthropic.claude-opus-4-6-v1", bedrockBaseModelID("global.anthropic.claude-opus-4-6-v1"))
	require.Equal(t, "anthropic.claude-x", bedrockBaseModelID("apac.anthropic.claude-x"))
	require.Equal(t, "meta.llama3-70b-instruct-v1:0", bedrockBaseModelID("meta.llama3-70b-instruct-v1:0"))
}

// Claude models get the documented thinking config; non-Claude models refuse
// loudly instead of silently dropping the request; adaptive Claude
// generations accept levels (already reasoning) but refuse think=off.
func TestUnit_ApplyBedrockThinking(t *testing.T) {
	think := func(level string) *modelrepo.ChatConfig {
		return &modelrepo.ChatConfig{Think: &level}
	}

	t.Run("claude budget model maps to thinking enabled with budget", func(t *testing.T) {
		in, _, err := buildConverseInput("us.anthropic.claude-sonnet-4-5-20250929-v1:0",
			[]modelrepo.Message{{Role: "user", Content: "hi"}}, think("high"), 0)
		require.NoError(t, err)
		require.NotNil(t, in.AdditionalModelRequestFields)
		b, err := in.AdditionalModelRequestFields.MarshalSmithyDocument()
		require.NoError(t, err)
		require.JSONEq(t, `{"thinking":{"type":"enabled","budget_tokens":4096}}`, string(b))
		require.NotNil(t, in.InferenceConfig)
		require.NotNil(t, in.InferenceConfig.MaxTokens)
		require.Greater(t, *in.InferenceConfig.MaxTokens, int32(4096), "max_tokens must exceed budget_tokens")
	})

	t.Run("think off maps to thinking disabled", func(t *testing.T) {
		in, _, err := buildConverseInput("anthropic.claude-3-7-sonnet-20250219-v1:0",
			[]modelrepo.Message{{Role: "user", Content: "hi"}}, think("off"), 0)
		require.NoError(t, err)
		require.NotNil(t, in.AdditionalModelRequestFields)
		b, err := in.AdditionalModelRequestFields.MarshalSmithyDocument()
		require.NoError(t, err)
		require.JSONEq(t, `{"thinking":{"type":"disabled"}}`, string(b))
	})

	t.Run("non-claude model refuses loudly", func(t *testing.T) {
		_, _, err := buildConverseInput("meta.llama3-70b-instruct-v1:0",
			[]modelrepo.Message{{Role: "user", Content: "hi"}}, think("high"), 0)
		require.Error(t, err)
		require.Contains(t, err.Error(), "meta.llama3-70b-instruct-v1:0")
		require.Contains(t, err.Error(), "drop think")
	})

	t.Run("adaptive claude accepts levels without a config", func(t *testing.T) {
		in, _, err := buildConverseInput("us.anthropic.claude-fable-5-v1:0",
			[]modelrepo.Message{{Role: "user", Content: "hi"}}, think("high"), 0)
		require.NoError(t, err)
		require.Nil(t, in.AdditionalModelRequestFields, "adaptive models reject an explicit thinking config")
	})

	t.Run("adaptive claude refuses think off", func(t *testing.T) {
		_, _, err := buildConverseInput("us.anthropic.claude-fable-5-v1:0",
			[]modelrepo.Message{{Role: "user", Content: "hi"}}, think("off"), 0)
		require.Error(t, err)
		require.Contains(t, err.Error(), "cannot disable thinking")
	})

	t.Run("no think leaves the request untouched", func(t *testing.T) {
		in, _, err := buildConverseInput("meta.llama3-70b-instruct-v1:0",
			[]modelrepo.Message{{Role: "user", Content: "hi"}}, &modelrepo.ChatConfig{}, 0)
		require.NoError(t, err)
		require.Nil(t, in.AdditionalModelRequestFields)
	})
}

// Non-streaming decode of reasoningContent blocks into Message.Thinking.
func TestUnit_DecodeConverse_ReasoningContent(t *testing.T) {
	out := &bedrockruntime.ConverseOutput{
		Output: &runtimetypes.ConverseOutputMemberMessage{Value: runtimetypes.Message{
			Role: runtimetypes.ConversationRoleAssistant,
			Content: []runtimetypes.ContentBlock{
				&runtimetypes.ContentBlockMemberReasoningContent{
					Value: &runtimetypes.ReasoningContentBlockMemberReasoningText{
						Value: runtimetypes.ReasoningTextBlock{
							Text:      aws.String("thinking it through"),
							Signature: aws.String("sig-1"),
						},
					},
				},
				&runtimetypes.ContentBlockMemberText{Value: "the answer"},
			},
		}},
	}
	res, err := decodeConverse(out, nil)
	require.NoError(t, err)
	require.Equal(t, "the answer", res.Message.Content)
	require.Equal(t, "thinking it through", res.Message.Thinking)
}

func TestUnit_ClassifyBedrockError(t *testing.T) {
	throttle := fmt.Errorf("op failed: %w", &runtimetypes.ThrottlingException{Message: aws.String("Too many requests")})
	require.ErrorIs(t, classifyBedrockError(throttle), modelrepo.ErrRateLimited)

	overflow := fmt.Errorf("op failed: %w", &runtimetypes.ValidationException{Message: aws.String("Input is too long for requested model.")})
	require.ErrorIs(t, classifyBedrockError(overflow), modelrepo.ErrContextLengthExceeded)

	otherValidation := fmt.Errorf("op failed: %w", &runtimetypes.ValidationException{Message: aws.String("The provided model identifier is invalid.")})
	err := classifyBedrockError(otherValidation)
	require.False(t, errors.Is(err, modelrepo.ErrContextLengthExceeded))
	require.False(t, errors.Is(err, modelrepo.ErrRateLimited))

	require.NoError(t, classifyBedrockError(nil))
}
