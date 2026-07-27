package bedrock

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	bedrocktypes "github.com/aws/aws-sdk-go-v2/service/bedrock/types"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"

	"github.com/contenox/beam/internal/models/modelrepo"
	"github.com/stretchr/testify/require"
)

func TestUnit_BuildConverseInput_RolesSystemToolsAndInference(t *testing.T) {
	maxTok := 256
	cfg := &modelrepo.ChatConfig{
		MaxTokens: &maxTok,
		Tools: []modelrepo.Tool{{
			Type:     "function",
			Function: &modelrepo.FunctionTool{Name: "fs.list", Description: "d", Parameters: map[string]any{"type": "object"}},
		}},
	}
	msgs := []modelrepo.Message{
		{Role: "system", Content: "be terse"},
		{Role: "user", Content: "list /tmp"},
		{Role: "assistant", ToolCalls: []modelrepo.ToolCall{tc("t1", "fs.list", `{"path":"/tmp"}`)}},
		{Role: "tool", ToolCallID: "t1", Content: `{"files":["a"]}`},
	}

	in, toOrig, _ := buildConverseInput("anthropic.claude-3-5-sonnet-20241022-v2:0", msgs, cfg, 0)

	require.Equal(t, "anthropic.claude-3-5-sonnet-20241022-v2:0", aws.ToString(in.ModelId))
	require.Len(t, in.System, 1)
	require.Equal(t, "be terse", in.System[0].(*types.SystemContentBlockMemberText).Value)
	require.NotNil(t, in.InferenceConfig)
	require.Equal(t, int32(256), *in.InferenceConfig.MaxTokens)
	require.NotNil(t, in.ToolConfig)
	require.Len(t, in.ToolConfig.Tools, 1)

	// Check mapping/sanitisation
	require.Equal(t, "fs.list", toOrig["fs_list"])

	// user, assistant(tool_use), user(tool_result)
	require.Len(t, in.Messages, 3)
	require.Equal(t, types.ConversationRoleUser, in.Messages[0].Role)
	require.Equal(t, types.ConversationRoleAssistant, in.Messages[1].Role)
	tu, ok := in.Messages[1].Content[0].(*types.ContentBlockMemberToolUse)
	require.True(t, ok, "assistant tool call must map to a tool_use block")
	require.Equal(t, "t1", aws.ToString(tu.Value.ToolUseId))
	require.Equal(t, "fs_list", aws.ToString(tu.Value.Name)) // The sanitized name sent to Bedrock
	require.Equal(t, types.ConversationRoleUser, in.Messages[2].Role)
	tr, ok := in.Messages[2].Content[0].(*types.ContentBlockMemberToolResult)
	require.True(t, ok, "tool message must map to a tool_result block")
	require.Equal(t, "t1", aws.ToString(tr.Value.ToolUseId))
}

func TestUnit_BuildConverseInput_ImageInputMapsToImageBlock(t *testing.T) {
	raw := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a} // PNG magic
	msgs := []modelrepo.Message{
		{Role: "user", Content: "describe this", Images: []modelrepo.ImagePart{{Data: raw, MimeType: "image/png"}}},
	}

	in, _, _ := buildConverseInput("anthropic.claude-3-5-sonnet-20241022-v2:0", msgs, &modelrepo.ChatConfig{}, 0)

	require.Len(t, in.Messages, 1)
	require.Equal(t, types.ConversationRoleUser, in.Messages[0].Role)
	// Text block first, then the image block.
	require.Len(t, in.Messages[0].Content, 2)

	txt, ok := in.Messages[0].Content[0].(*types.ContentBlockMemberText)
	require.True(t, ok, "first block must be text")
	require.Equal(t, "describe this", txt.Value)

	img, ok := in.Messages[0].Content[1].(*types.ContentBlockMemberImage)
	require.True(t, ok, "second block must be an image content block")
	require.Equal(t, types.ImageFormatPng, img.Value.Format)

	src, ok := img.Value.Source.(*types.ImageSourceMemberBytes)
	require.True(t, ok, "image source must be raw bytes")
	require.Equal(t, raw, src.Value)

	// A text-only user message still maps to a single text block.
	textOnly, _, _ := buildConverseInput("m", []modelrepo.Message{{Role: "user", Content: "just text"}}, &modelrepo.ChatConfig{}, 0)
	require.Len(t, textOnly.Messages, 1)
	require.Len(t, textOnly.Messages[0].Content, 1)
	tob, ok := textOnly.Messages[0].Content[0].(*types.ContentBlockMemberText)
	require.True(t, ok, "text-only message must produce a text block")
	require.Equal(t, "just text", tob.Value)
}

func TestUnit_BuildConverseInput_ImageFormatsAndUnknownSkipped(t *testing.T) {
	cases := []struct {
		mime string
		want types.ImageFormat
	}{
		{"image/png", types.ImageFormatPng},
		{"image/jpeg", types.ImageFormatJpeg},
		{"image/gif", types.ImageFormatGif},
		{"image/webp", types.ImageFormatWebp},
	}
	for _, c := range cases {
		t.Run(c.mime, func(t *testing.T) {
			in, _, _ := buildConverseInput("m", []modelrepo.Message{
				{Role: "user", Images: []modelrepo.ImagePart{{Data: []byte{1}, MimeType: c.mime}}},
			}, &modelrepo.ChatConfig{}, 0)
			require.Len(t, in.Messages, 1)
			require.Len(t, in.Messages[0].Content, 1)
			img, ok := in.Messages[0].Content[0].(*types.ContentBlockMemberImage)
			require.True(t, ok)
			require.Equal(t, c.want, img.Value.Format)
		})
	}

	// An unrecognised MIME type is skipped: a text-only-plus-bad-image message
	// yields only the text block, and an image-only message yields no message.
	in, _, _ := buildConverseInput("m", []modelrepo.Message{
		{Role: "user", Content: "hi", Images: []modelrepo.ImagePart{{Data: []byte{1}, MimeType: "image/tiff"}}},
	}, &modelrepo.ChatConfig{}, 0)
	require.Len(t, in.Messages, 1)
	require.Len(t, in.Messages[0].Content, 1)
	_, ok := in.Messages[0].Content[0].(*types.ContentBlockMemberText)
	require.True(t, ok, "unknown image type must be skipped, leaving only the text block")

	in, _, _ = buildConverseInput("m", []modelrepo.Message{
		{Role: "user", Images: []modelrepo.ImagePart{{Data: []byte{1}, MimeType: "application/pdf"}}},
	}, &modelrepo.ChatConfig{}, 0)
	require.Empty(t, in.Messages, "an image-only message with an unknown type produces no content")
}

func TestUnit_BuildConverseInput_ClampsMaxTokens(t *testing.T) {
	maxTok := 9000
	cfg := &modelrepo.ChatConfig{MaxTokens: &maxTok}

	in, _, _ := buildConverseInput("anthropic.claude-3-5-sonnet-20241022-v2:0", []modelrepo.Message{{Role: "user", Content: "hi"}}, cfg, 4096)

	require.NotNil(t, in.InferenceConfig)
	require.NotNil(t, in.InferenceConfig.MaxTokens)
	require.Equal(t, int32(4096), *in.InferenceConfig.MaxTokens)
}

func TestUnit_DecodeConverse_TextAndToolUse(t *testing.T) {
	out := &bedrockruntime.ConverseOutput{
		Output: &types.ConverseOutputMemberMessage{Value: types.Message{
			Role: types.ConversationRoleAssistant,
			Content: []types.ContentBlock{
				&types.ContentBlockMemberText{Value: "on it"},
				&types.ContentBlockMemberToolUse{Value: types.ToolUseBlock{
					ToolUseId: aws.String("t9"),
					Name:      aws.String("fs_list"),
					Input:     document.NewLazyDocument(map[string]any{"path": "/x"}),
				}},
			},
		}},
	}
	res, err := decodeConverse(out, map[string]string{"fs_list": "fs.list"})
	require.NoError(t, err)
	require.Equal(t, "on it", res.Message.Content)
	require.Len(t, res.ToolCalls, 1)
	require.Equal(t, "t9", res.ToolCalls[0].ID)
	require.Equal(t, "fs.list", res.ToolCalls[0].Function.Name)

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(res.ToolCalls[0].Function.Arguments), &got))
	require.Equal(t, "/x", got["path"])
}

// CanVision must be detected from the model's advertised input modalities
// (ListFoundationModels -> FoundationModelSummary.InputModalities), not a
// hardcoded model-name list. A summary listing the IMAGE modality is vision
// capable; a TEXT-only summary is not.
func TestUnit_ObservedFromSummary_CanVisionFromInputModalities(t *testing.T) {
	visionModel := observedFromSummary(bedrocktypes.FoundationModelSummary{
		ModelId:         aws.String("anthropic.claude-3-5-sonnet-20241022-v2:0"),
		InputModalities: []bedrocktypes.ModelModality{bedrocktypes.ModelModalityText, bedrocktypes.ModelModalityImage},
	}, "anthropic.claude-3-5-sonnet-20241022-v2:0")
	require.Equal(t, "anthropic.claude-3-5-sonnet-20241022-v2:0", visionModel.Name)
	require.True(t, visionModel.CanVision, "IMAGE input modality must set CanVision")
	require.True(t, visionModel.CanChat)
	require.False(t, visionModel.CanEmbed)

	textModel := observedFromSummary(bedrocktypes.FoundationModelSummary{
		ModelId:         aws.String("meta.llama3-70b-instruct-v1:0"),
		InputModalities: []bedrocktypes.ModelModality{bedrocktypes.ModelModalityText},
	}, "meta.llama3-70b-instruct-v1:0")
	require.False(t, textModel.CanVision, "TEXT-only input modality must not set CanVision")
	require.True(t, textModel.CanChat)

	// Detection is case-insensitive against the API's string value.
	lowerModel := observedFromSummary(bedrocktypes.FoundationModelSummary{
		ModelId:         aws.String("some.multimodal-v1:0"),
		InputModalities: []bedrocktypes.ModelModality{"image"},
	}, "some.multimodal-v1:0")
	require.True(t, lowerModel.CanVision, "modality comparison must be case-insensitive")

	// An embedding model must NOT advertise CanEmbed: the provider speaks the
	// Converse API only and GetEmbedConnection refuses, so advertising would
	// lie to the request router (catalog truth over half-support).
	embedModel := observedFromSummary(bedrocktypes.FoundationModelSummary{
		ModelId:         aws.String("amazon.titan-embed-text-v2:0"),
		InputModalities: []bedrocktypes.ModelModality{bedrocktypes.ModelModalityText},
	}, "amazon.titan-embed-text-v2:0")
	require.False(t, embedModel.CanEmbed, "bedrock embeddings are unimplemented; the catalog must not advertise them")
	require.False(t, embedModel.CanChat)
	require.False(t, embedModel.CanVision)
}

func TestUnit_RegionFromURL(t *testing.T) {
	require.Equal(t, "us-east-1", regionFromURL("https://bedrock-runtime.us-east-1.amazonaws.com"))
	require.Equal(t, "eu-west-3", regionFromURL("https://bedrock-runtime.eu-west-3.amazonaws.com/"))
	require.Equal(t, "us-east-1", regionFromURL("us-east-1")) // bare region
	require.Equal(t, "", regionFromURL(""))
}

// isAWSCredentialError reports whether err is an environment problem (missing,
// expired, or rejected AWS credentials) rather than a product defect. The
// system test skips on these so the gate stays green on machines without a
// live AWS login, per the TestSystem_ probe-and-skip convention.
func isAWSCredentialError(err error) bool {
	msg := err.Error()
	for _, s := range []string{
		"get credentials",
		"no EC2 IMDS role found",
		"expired",
		"InvalidClientTokenId",
		"UnrecognizedClientException",
		"AccessDeniedException",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

func TestSystem_BedrockCatalog_RegisteredAndChatCapable(t *testing.T) {
	cp, err := modelrepo.NewCatalogProvider(modelrepo.BackendSpec{
		Type:    "bedrock",
		BaseURL: "https://bedrock-runtime.us-east-1.amazonaws.com",
	})
	require.NoError(t, err, "bedrock must be registered in the catalog registry")
	require.Equal(t, "bedrock", cp.Type())

	models, err := cp.ListModels(context.TODO())
	if err != nil && isAWSCredentialError(err) {
		t.Skipf("skipping: no usable AWS credentials for Bedrock (%v)", err)
	}
	require.NoError(t, err)
	require.NotEmpty(t, models)
	require.True(t, models[0].CanChat)
	require.False(t, models[0].CanThink, "curated Bedrock model list must not infer thinking support")

	prov := cp.ProviderFor(models[0])
	require.Equal(t, "bedrock", prov.GetType())
	require.True(t, prov.CanChat())
	require.False(t, prov.CanEmbed())
	require.False(t, prov.CanThink())
}

func TestUnit_BedrockProvider_CanThinkFromCapabilityConfigOnly(t *testing.T) {
	provider := NewBedrockProvider("us-east-1", "", "anthropic.claude-3-7-sonnet-20250219-v1:0", modelrepo.CapabilityConfig{CanChat: true}, nil, nil)
	require.False(t, provider.CanThink(), "model name alone must not set CanThink")

	provider = NewBedrockProvider("us-east-1", "", "custom", modelrepo.CapabilityConfig{CanChat: true, CanThink: true}, nil, nil)
	require.True(t, provider.CanThink(), "explicit capability config must set CanThink")
}

func TestUnit_SanitizeToolName(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"", "tool"},
		{"abc", "abc"},
		{"a.b.c", "a_b_c"},
		{"fs.list", "fs_list"},
		{"-abc-", "abc"},
		{"_abc_", "abc"},
		{".", "tool"},
		{"__.-.__", "tool"},
		{strings.Repeat("a", 100), strings.Repeat("a", 64)},
		{strings.Repeat("a", 64) + "_", strings.Repeat("a", 64)},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := sanitizeToolName(tc.input)
			require.Equal(t, tc.expected, got)
		})
	}
}

func tc(id, name, args string) modelrepo.ToolCall {
	t := modelrepo.ToolCall{ID: id, Type: "function"}
	t.Function.Name = name
	t.Function.Arguments = args
	return t
}

// Regression: Bedrock rejects toolUse/toolResult blocks unless toolConfig is
// set ("The toolConfig field must be defined when using toolUse and toolResult
// content blocks"). Tasks without tools (recovery/summarise steps) still
// receive tool-bearing histories; those blocks must degrade to text.
func TestUnit_BuildConverseInput_NoToolsDegradesToolBlocksToText(t *testing.T) {
	msgs := []modelrepo.Message{
		{Role: "user", Content: "list /tmp"},
		{Role: "assistant", ToolCalls: []modelrepo.ToolCall{tc("t1", "fs.list", `{"path":"/tmp"}`)}},
		{Role: "tool", ToolCallID: "t1", Content: `{"files":["a"]}`},
		{Role: "user", Content: "summarise what happened"},
	}

	in, _, _ := buildConverseInput("deepseek.v3.2", msgs, &modelrepo.ChatConfig{}, 0)

	require.Nil(t, in.ToolConfig)
	for _, m := range in.Messages {
		for _, cb := range m.Content {
			switch cb.(type) {
			case *types.ContentBlockMemberToolUse, *types.ContentBlockMemberToolResult:
				t.Fatalf("tool content block %T sent without toolConfig", cb)
			}
		}
	}

	// The tool exchange must survive as text so the model keeps the context.
	require.Len(t, in.Messages, 3) // user, assistant(text), user(result text + question merged)
	assistantText := in.Messages[1].Content[0].(*types.ContentBlockMemberText).Value
	require.Contains(t, assistantText, "fs.list")
	require.Contains(t, assistantText, `{"path":"/tmp"}`)
	resultText := in.Messages[2].Content[0].(*types.ContentBlockMemberText).Value
	require.Contains(t, resultText, "t1")
	require.Contains(t, resultText, `{"files":["a"]}`)
}
