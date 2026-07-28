package bedrock

import (
	"context"
	"reflect"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/contenox/beam/internal/models/modelrepo"
)

func bedrockCacheFixtureMessages() []modelrepo.Message {
	return []modelrepo.Message{
		{Role: "system", Content: "be terse"},
		{Role: "user", Content: "hello"},
	}
}

func bedrockCacheFixtureConfig(hints *modelrepo.CacheHints) *modelrepo.ChatConfig {
	return &modelrepo.ChatConfig{
		Tools: []modelrepo.Tool{
			{Type: "function", Function: &modelrepo.FunctionTool{Name: "fs.read", Parameters: map[string]any{"type": "object"}}},
		},
		CacheHints: hints,
	}
}

func countSystemCachePoints(system []types.SystemContentBlock) int {
	n := 0
	for _, b := range system {
		if _, ok := b.(*types.SystemContentBlockMemberCachePoint); ok {
			n++
		}
	}
	return n
}

func countToolCachePoints(cfg *types.ToolConfiguration) int {
	if cfg == nil {
		return 0
	}
	n := 0
	for _, tl := range cfg.Tools {
		if _, ok := tl.(*types.ToolMemberCachePoint); ok {
			n++
		}
	}
	return n
}

func TestUnit_BedrockCachePoints_PlacedOnClaudeSystemAndTools(t *testing.T) {
	hints := &modelrepo.CacheHints{StableSystem: true, StableTools: true}
	in, _, err := buildConverseInput("us.anthropic.claude-sonnet-4-5-v1:0",
		bedrockCacheFixtureMessages(), bedrockCacheFixtureConfig(hints), 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := countToolCachePoints(in.ToolConfig); got != 1 {
		t.Fatalf("expected exactly one tool cachePoint, got %d", got)
	}
	// cachePoint must be the last entry so it caches the whole tool list.
	if _, ok := in.ToolConfig.Tools[len(in.ToolConfig.Tools)-1].(*types.ToolMemberCachePoint); !ok {
		t.Fatalf("tool cachePoint must be appended after the last tool spec")
	}
	if got := countSystemCachePoints(in.System); got != 1 {
		t.Fatalf("expected exactly one system cachePoint, got %d", got)
	}
	if _, ok := in.System[len(in.System)-1].(*types.SystemContentBlockMemberCachePoint); !ok {
		t.Fatalf("system cachePoint must be appended after the last system block")
	}
	// The type must be the documented default checkpoint.
	cp := in.System[len(in.System)-1].(*types.SystemContentBlockMemberCachePoint)
	if cp.Value.Type != types.CachePointTypeDefault {
		t.Fatalf("cachePoint type must be default, got %v", cp.Value.Type)
	}
}

func TestUnit_BedrockCachePoints_UnsupportedModelOmitsSilently(t *testing.T) {
	hints := &modelrepo.CacheHints{StableSystem: true, StableTools: true}
	in, _, err := buildConverseInput("meta.llama3-70b-instruct-v1:0",
		bedrockCacheFixtureMessages(), bedrockCacheFixtureConfig(hints), 0)
	if err != nil {
		t.Fatal(err)
	}
	if countToolCachePoints(in.ToolConfig) != 0 || countSystemCachePoints(in.System) != 0 {
		t.Fatalf("non-Claude/Nova model must get no cachePoints")
	}
}

func TestUnit_BedrockCachePoints_NeverChangeModelVisibleContent(t *testing.T) {
	// Hinted vs unhinted requests must differ only by appended cachePoint
	// union members: stripping them yields the identical request.
	plain, _, err := buildConverseInput("anthropic.claude-sonnet-4-5-v1:0",
		bedrockCacheFixtureMessages(), bedrockCacheFixtureConfig(nil), 0)
	if err != nil {
		t.Fatal(err)
	}
	hinted, _, err := buildConverseInput("anthropic.claude-sonnet-4-5-v1:0",
		bedrockCacheFixtureMessages(), bedrockCacheFixtureConfig(&modelrepo.CacheHints{
			StableSystem: true, StableTools: true,
		}), 0)
	if err != nil {
		t.Fatal(err)
	}

	var sys []types.SystemContentBlock
	for _, b := range hinted.System {
		if _, ok := b.(*types.SystemContentBlockMemberCachePoint); ok {
			continue
		}
		sys = append(sys, b)
	}
	hinted.System = sys
	if hinted.ToolConfig != nil {
		var tools []types.Tool
		for _, tl := range hinted.ToolConfig.Tools {
			if _, ok := tl.(*types.ToolMemberCachePoint); ok {
				continue
			}
			tools = append(tools, tl)
		}
		hinted.ToolConfig.Tools = tools
	}

	if !reflect.DeepEqual(plain.System, hinted.System) {
		t.Fatalf("system content changed by hints:\nplain:  %#v\nhinted: %#v", plain.System, hinted.System)
	}
	if !reflect.DeepEqual(plain.Messages, hinted.Messages) {
		t.Fatalf("messages changed by hints")
	}
	if len(plain.ToolConfig.Tools) != len(hinted.ToolConfig.Tools) {
		t.Fatalf("tool list changed by hints")
	}
}

func TestUnit_BedrockUsage_NormalizationRule(t *testing.T) {
	// Bedrock inputTokens excludes cache reads/writes; PromptTokens is the sum of the three.
	u := usageFromConverse(&types.TokenUsage{
		InputTokens:           aws.Int32(7),
		OutputTokens:          aws.Int32(11),
		TotalTokens:           aws.Int32(18), // wire total is untrusted
		CacheReadInputTokens:  aws.Int32(100),
		CacheWriteInputTokens: aws.Int32(30),
	})
	if u.PromptTokens != 137 || u.CacheReadTokens != 100 || u.CacheWriteTokens != 30 {
		t.Fatalf("normalization violated: %+v", u)
	}
	if u.CompletionTokens != 11 || u.TotalTokens != 148 {
		t.Fatalf("completion/total wrong: %+v", u)
	}
	if usageFromConverse(nil) != nil {
		t.Fatalf("nil usage must stay nil")
	}
}

func TestUnit_BedrockStream_MetadataCarriesCacheUsage(t *testing.T) {
	events := make(chan types.ConverseStreamOutput, 4)
	events <- &types.ConverseStreamOutputMemberContentBlockDelta{
		Value: types.ContentBlockDeltaEvent{
			ContentBlockIndex: aws.Int32(0),
			Delta:             &types.ContentBlockDeltaMemberText{Value: "hi"},
		},
	}
	events <- &types.ConverseStreamOutputMemberMessageStop{
		Value: types.MessageStopEvent{StopReason: types.StopReasonEndTurn},
	}
	events <- &types.ConverseStreamOutputMemberMetadata{
		Value: types.ConverseStreamMetadataEvent{
			Usage: &types.TokenUsage{
				InputTokens:           aws.Int32(5),
				OutputTokens:          aws.Int32(3),
				CacheReadInputTokens:  aws.Int32(60),
				CacheWriteInputTokens: aws.Int32(15),
			},
		},
	}
	close(events)

	parcels := make(chan *modelrepo.StreamParcel, 8)
	relayConverseEvents(context.Background(), events, nil, parcels)
	close(parcels)

	var term *modelrepo.StreamTerminal
	for p := range parcels {
		if p.Terminal != nil {
			term = p.Terminal
		}
	}
	if term == nil || term.Usage == nil {
		t.Fatal("terminal usage missing")
	}
	u := term.Usage
	if u.PromptTokens != 80 || u.CacheReadTokens != 60 || u.CacheWriteTokens != 15 || u.CompletionTokens != 3 {
		t.Fatalf("stream usage normalization violated: %+v", u)
	}
}
