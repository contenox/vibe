//go:build !openvino || !openvino_genai

package ovsession

import (
	"context"
	"errors"
)

// GenAIAvailable reports whether the OpenVINO GenAI session backend was built.
const GenAIAvailable = false

// GenAIConfig controls construction of an OpenVINO GenAI session.
type GenAIConfig struct {
	Device                      string
	KVCachePrecision            string
	CacheSize                   int
	CacheSizeExplicit           bool
	DynamicSplitFuse            *bool
	EnablePrefixCaching         *bool
	UseSparseAttention          *bool
	NumLastDenseTokensInPrefill int
	XAttentionThreshold         float32
	XAttentionBlockSize         int
	XAttentionStride            int
	UseCacheEviction            *bool
	CacheEvictStartSize         int
	CacheEvictRecentSize        int
	CacheEvictMaxSize           int
	LoRAAdapters                []GenAILoRAAdapter
}

// GenAILoRAAdapter is one OpenVINO LoRA adapter to apply to a session: a
// safetensors file plus its effective scale (alpha).
type GenAILoRAAdapter struct {
	Path  string
	Alpha float32
}

// GenerateOptions controls a single GenAI generation call.
type GenerateOptions struct {
	MaxNewTokens     int
	Temperature      *float64
	TopP             *float64
	TopK             *int
	Seed             *int
	StructuredOutput StructuredOutput
	ParserProtocols  []string
}

// StructuredOutput selects an OpenVINO structured-output primitive plus its
// payload for this generation call.
type StructuredOutput struct {
	Protocol string
	Payload  string
}

// PipelineMetrics mirrors the OpenVINO GenAI PipelineMetrics fields used by
// the local runtime.
type PipelineMetrics struct {
	Requests          uint64
	ScheduledRequests uint64
	CacheUsage        float32
	MaxCacheUsage     float32
	AvgCacheUsage     float32
	InferenceDuration float32
	CacheSizeInBytes  uint64
}

// DeviceInfo describes one OpenVINO device and any memory telemetry exposed by
// that plugin.
type DeviceInfo struct {
	Index             int
	Name              string
	Description       string
	Type              string
	MemoryFree        uint64
	MemoryTotal       uint64
	MemoryFreeKnown   bool
	MemoryTotalKnown  bool
	SharedWithDisplay bool
}

// RuntimeInfo describes the linked OpenVINO runtime and available devices.
type RuntimeInfo struct {
	RuntimeName        string
	RuntimeDigest      string
	RuntimeSystemInfo  string
	SupportsGPUOffload bool
	Devices            []DeviceInfo
}

// ModelKVProfile is unavailable without the native OpenVINO GenAI shim.
type ModelKVProfile struct {
	MaxPositionEmbeddings int
	NumHiddenLayers       int
	NumKeyValueHeads      int
	NumAttentionHeads     int
	HiddenSize            int
	HeadDim               int
	SlidingWindow         int
	GlobalLayers          int
	WindowedLayers        int
}

// ChatTemplateProbe is unavailable without the native OpenVINO GenAI shim.
type ChatTemplateProbe struct {
	FormatName              string
	ThinkingStartTag        string
	SupportsToolCalls       bool
	SupportsThinking        bool
	SupportsReasoningEffort bool
}

// GenAIResult is the generated text plus the pipeline metrics observed for the
// request.
type GenAIResult struct {
	Text       string
	ParsedJSON string
	Metrics    PipelineMetrics
}

// Runtime reports that the native GenAI backend is not compiled in.
func Runtime() (RuntimeInfo, error) {
	return RuntimeInfo{}, errors.New("openvino GenAI backend is not compiled in")
}

// Device reports that the native GenAI backend is not compiled in.
func Device(_ string) (DeviceInfo, error) {
	return DeviceInfo{}, errors.New("openvino GenAI backend is not compiled in")
}

// InspectModelKVProfile reports that the native GenAI backend is not compiled in.
func InspectModelKVProfile(_ string) (ModelKVProfile, error) {
	return ModelKVProfile{}, errors.New("openvino GenAI backend is not compiled in")
}

// ProbeChatTemplate reports that the native GenAI backend is not compiled in.
func ProbeChatTemplate(_ string) (ChatTemplateProbe, error) {
	return ChatTemplateProbe{}, errors.New("openvino GenAI backend is not compiled in")
}

// StreamChunk carries a decoded text delta or a terminal stream error.
type StreamChunk struct {
	Text     string
	Thinking string
	Error    error
}

// GenAISession is unavailable without the openvino and openvino_genai build
// tags.
type GenAISession struct{}

// NewGenAI reports that the native GenAI backend is not compiled in.
func NewGenAI(_ string, _ GenAIConfig) (*GenAISession, error) {
	return nil, errors.New("openvino GenAI backend is not compiled in")
}

// Generate reports that the native GenAI backend is not compiled in.
func (s *GenAISession) Generate(_ context.Context, _ string, _ GenerateOptions) (GenAIResult, error) {
	return GenAIResult{}, errors.New("openvino GenAI backend is not compiled in")
}

// PrefillTokens reports that the native GenAI backend is not compiled in.
func (s *GenAISession) PrefillTokens(_ context.Context, _ []int) error {
	return errors.New("openvino GenAI backend is not compiled in")
}

// GenerateTokens reports that the native GenAI backend is not compiled in.
func (s *GenAISession) GenerateTokens(_ context.Context, _ []int, _ GenerateOptions) (GenAIResult, error) {
	return GenAIResult{}, errors.New("openvino GenAI backend is not compiled in")
}

// Stream reports that the native GenAI backend is not compiled in.
func (s *GenAISession) Stream(_ context.Context, _ string, _ GenerateOptions) (<-chan StreamChunk, error) {
	return nil, errors.New("openvino GenAI backend is not compiled in")
}

// StreamTokens reports that the native GenAI backend is not compiled in.
func (s *GenAISession) StreamTokens(_ context.Context, _ []int, _ GenerateOptions) (<-chan StreamChunk, error) {
	return nil, errors.New("openvino GenAI backend is not compiled in")
}

// Close is a no-op for the stub.
func (s *GenAISession) Close() error {
	return nil
}

// ChatMessage is one role/content turn for chat-template rendering.
type ChatMessage struct {
	Role       string
	Content    string
	ToolCalls  string `json:",omitempty"`
	ToolCallID string `json:",omitempty"`
}

// ApplyChatTemplate reports that the native GenAI backend is not compiled in.
func (s *GenAISession) ApplyChatTemplate(_ []ChatMessage, _ string) (string, error) {
	return "", errors.New("openvino GenAI backend is not compiled in")
}

// ApplyChatTemplateWithPrompt reports that the native GenAI backend is not
// compiled in.
func (s *GenAISession) ApplyChatTemplateWithPrompt(_ []ChatMessage, _ string, _ bool) (string, error) {
	return "", errors.New("openvino GenAI backend is not compiled in")
}

// ApplyChatTemplateWithOptions reports that the native GenAI backend is not
// compiled in.
func (s *GenAISession) ApplyChatTemplateWithOptions(_ []ChatMessage, _ string, _ bool, _ *bool, _ string) (string, error) {
	return "", errors.New("openvino GenAI backend is not compiled in")
}

// Tokenize reports that the native GenAI backend is not compiled in.
func (s *GenAISession) Tokenize(_ context.Context, _ string, _ bool) ([]int, error) {
	return nil, errors.New("openvino GenAI backend is not compiled in")
}
