# Plan: OpenVINO Go Bindings — Embedded LLM Inference for Contenox

## Goal

Ship contenox with built-in LLM inference via OpenVINO — no Ollama, no external
server, no API keys. One binary that runs AI task chains on CPU, locally, offline.

Target UX:
```
contenox init
contenox model pull gemma4-e4b
contenox "fix the tests"
```

---

## Why OpenVINO (not llama.cpp)

| | OpenVINO | llama.cpp |
|---|---|---|
| Intel CPU optimization | Native (AVX2/AVX-512/AMX/NPU) | Via SYCL backend (add-on) |
| Tokenization | Built-in (compiled as IR model) | Requires separate tokenizer lib |
| Chat templates | Applied automatically from Jinja | Manual |
| Model format | OpenVINO IR (pre-optimized, quantized) | GGUF |
| Strategic value | Own the inference stack, differentiated | Same backend as Ollama |
| Go ecosystem | No bindings exist — we'd be first | yzma/kronk exist |
| AMD/ARM CPU | Supported (fallback path) | Supported |
| GPU | Intel iGPU/dGPU, plus CPU | CUDA/Metal/Vulkan/SYCL |

Building Go bindings for openvino-genai makes contenox the first Go project with
native OpenVINO LLM inference. This is a standalone OSS contribution that drives
adoption back to contenox.

---

## Target C API Surface

The openvino-genai project (`openvinotoolkit/openvino.genai`) ships a C API at
`src/c/include/openvino/genai/c/`. The API is small and purpose-built for LLM
inference pipelines.

### Headers to wrap

| Header | Purpose | Priority |
|---|---|---|
| `llm_pipeline.h` | Pipeline create/generate/stream/chat | P0 — core |
| `generation_config.h` | Temperature, top_k, max_tokens, etc. | P0 — core |
| `chat_history.h` | Multi-turn conversation management | P0 — needed for chat |
| `json_container.h` | JSON data for messages/tools | P0 — dependency of chat_history |
| `perf_metrics.h` | TTFT, throughput, token counts | P1 — observability |
| `vlm_pipeline.h` | Vision-language models | P2 — future |
| `whisper_pipeline.h` | Speech-to-text | P3 — future |

### Core function count (P0 scope)

```
llm_pipeline.h:      ~12 functions
generation_config.h:  ~30 functions (mostly setters)
chat_history.h:       ~14 functions
json_container.h:     ~7 functions
```

**Total P0 surface: ~63 functions.** This is a tractable binding project.

---

## Key C API Functions

### LLM Pipeline (llm_pipeline.h)

```c
// Opaque handles
typedef struct ov_genai_llm_pipeline_opaque ov_genai_llm_pipeline;
typedef struct ov_genai_decoded_results_opaque ov_genai_decoded_results;

// Lifecycle
ov_status_e ov_genai_llm_pipeline_create(
    const char* models_path,    // directory with .xml/.bin + tokenizer
    const char* device,         // "CPU", "GPU", "NPU"
    const size_t property_args_size,
    ov_genai_llm_pipeline** pipe,
    ...);                       // variadic properties (needs C shim)
void ov_genai_llm_pipeline_free(ov_genai_llm_pipeline* pipe);

// Generate (single prompt)
ov_status_e ov_genai_llm_pipeline_generate(
    ov_genai_llm_pipeline* pipe,
    const char* inputs,
    const ov_genai_generation_config* config,
    const streamer_callback* streamer,     // NULL for non-streaming
    ov_genai_decoded_results** results);

// Generate (multi-turn chat)
ov_status_e ov_genai_llm_pipeline_generate_with_history(
    ov_genai_llm_pipeline* pipe,
    const ov_genai_chat_history* history,
    const ov_genai_generation_config* config,
    const streamer_callback* streamer,
    ov_genai_decoded_results** results);

// Chat session
ov_status_e ov_genai_llm_pipeline_start_chat(ov_genai_llm_pipeline* pipe);
ov_status_e ov_genai_llm_pipeline_finish_chat(ov_genai_llm_pipeline* pipe);

// Results
ov_status_e ov_genai_decoded_results_get_string(
    const ov_genai_decoded_results* results,
    char* output,          // NULL on first call to get size
    size_t* output_size);
ov_status_e ov_genai_decoded_results_get_perf_metrics(
    const ov_genai_decoded_results* results,
    ov_genai_perf_metrics** metrics);
```

### Streaming Callback

```c
typedef enum {
    OV_GENAI_STREAMING_STATUS_RUNNING = 0,
    OV_GENAI_STREAMING_STATUS_STOP    = 1,
    OV_GENAI_STREAMING_STATUS_CANCEL  = 2,
} ov_genai_streaming_status_e;

typedef struct {
    ov_genai_streaming_status_e (*callback_func)(const char* str, void* args);
    void* args;
} streamer_callback;
```

### Generation Config (generation_config.h)

```c
ov_genai_generation_config_create(ov_genai_generation_config** config);
ov_genai_generation_config_free(ov_genai_generation_config* handle);

// Key setters (all return ov_status_e):
ov_genai_generation_config_set_max_new_tokens(config, size_t)
ov_genai_generation_config_set_temperature(config, float)
ov_genai_generation_config_set_top_p(config, float)
ov_genai_generation_config_set_top_k(config, size_t)
ov_genai_generation_config_set_do_sample(config, bool)
ov_genai_generation_config_set_repetition_penalty(config, float)
ov_genai_generation_config_set_stop_strings(config, const char** strings, size_t count)
ov_genai_generation_config_set_stop_token_ids(config, const int64_t* ids, size_t count)
// ... ~20 more setters for beam search, penalties, etc.
```

### Chat History (chat_history.h)

```c
ov_genai_chat_history_create(ov_genai_chat_history** history);
ov_genai_chat_history_free(ov_genai_chat_history* history);
ov_genai_chat_history_push_back(history, const ov_genai_json_container* message);
ov_genai_chat_history_set_tools(history, const ov_genai_json_container* tools);
ov_genai_chat_history_size(history, size_t* size);
ov_genai_chat_history_clear(history);
// Messages are JSON: {"role":"user","content":"..."}
```

### JSON Container (json_container.h)

```c
ov_genai_json_container_create_from_json_string(
    ov_genai_json_container** container,
    const char* json_str);
ov_genai_json_container_to_json_string(
    container, char* output, size_t* output_size);
ov_genai_json_container_free(ov_genai_json_container* container);
```

---

## CGo Considerations

### Variadic function problem

`ov_genai_llm_pipeline_create` is variadic (`...`). CGo cannot call C variadic
functions. Solution: write a small C shim file that wraps the variadic call:

```c
// shim.c — compiled alongside the Go package
#include <openvino/genai/c/llm_pipeline.h>

ov_status_e ov_genai_llm_pipeline_create_simple(
    const char* models_path,
    const char* device,
    ov_genai_llm_pipeline** pipe) {
    return ov_genai_llm_pipeline_create(models_path, device, 0, pipe);
}

ov_status_e ov_genai_llm_pipeline_create_with_cache(
    const char* models_path,
    const char* device,
    const char* cache_dir,
    ov_genai_llm_pipeline** pipe) {
    return ov_genai_llm_pipeline_create(
        models_path, device, 2, pipe,
        "cache_dir", cache_dir);
}
```

### String output pattern

OpenVINO C API uses a two-call pattern for string results:
1. Call with `output=NULL` → get `output_size`
2. Allocate buffer, call again → get string

Wrap this in Go:

```go
func (r *DecodedResults) String() (string, error) {
    var size C.size_t
    if status := C.ov_genai_decoded_results_get_string(r.ptr, nil, &size); status != 0 {
        return "", statusError(status)
    }
    buf := make([]byte, size)
    if status := C.ov_genai_decoded_results_get_string(r.ptr, (*C.char)(unsafe.Pointer(&buf[0])), &size); status != 0 {
        return "", statusError(status)
    }
    return string(buf[:size-1]), nil // trim null terminator
}
```

### Streaming callback

The streamer callback crosses the CGo boundary. Use `cgo.NewHandle` to pass Go
state through the `void* args` parameter:

```go
//export goStreamerCallback
func goStreamerCallback(str *C.char, args unsafe.Pointer) C.ov_genai_streaming_status_e {
    h := cgo.Handle(args)
    ch := h.Value().(chan string)
    ch <- C.GoString(str)
    return C.OV_GENAI_STREAMING_STATUS_RUNNING
}
```

---

## Go Package Design

### Package: `openvino` (standalone, reusable)

```
github.com/contenox/openvino-go/
├── openvino.go           // Pipeline, Config, ChatHistory types
├── shim.c                // C wrappers for variadic functions
├── shim.h
├── status.go             // ov_status_e → Go error mapping
├── config.go             // GenerationConfig with builder pattern
├── chat.go               // ChatHistory + JSON container wrappers
├── stream.go             // Streaming callback bridge
├── metrics.go            // PerfMetrics
├── openvino_test.go      // Integration tests (need a model dir)
└── README.md
```

### Go API sketch

```go
package openvino

// Pipeline wraps ov_genai_llm_pipeline.
type Pipeline struct { ptr *C.ov_genai_llm_pipeline }

func NewPipeline(modelDir, device string) (*Pipeline, error)
func (p *Pipeline) Close()
func (p *Pipeline) Generate(prompt string, opts ...ConfigOption) (string, error)
func (p *Pipeline) GenerateWithHistory(history *ChatHistory, opts ...ConfigOption) (string, error)
func (p *Pipeline) Stream(prompt string, opts ...ConfigOption) (<-chan string, error)
func (p *Pipeline) StreamWithHistory(history *ChatHistory, opts ...ConfigOption) (<-chan string, error)
func (p *Pipeline) StartChat() error
func (p *Pipeline) FinishChat() error

// ConfigOption follows the contenox options pattern.
type ConfigOption func(*GenerationConfig)

func WithMaxNewTokens(n int) ConfigOption
func WithTemperature(t float32) ConfigOption
func WithTopP(p float32) ConfigOption
func WithTopK(k int) ConfigOption
func WithStopStrings(ss ...string) ConfigOption

// ChatHistory wraps ov_genai_chat_history.
type ChatHistory struct { ptr *C.ov_genai_chat_history }

func NewChatHistory() *ChatHistory
func (h *ChatHistory) Close()
func (h *ChatHistory) AddUserMessage(content string)
func (h *ChatHistory) AddAssistantMessage(content string)
func (h *ChatHistory) AddSystemMessage(content string)
func (h *ChatHistory) SetTools(toolsJSON string)
func (h *ChatHistory) Len() int
func (h *ChatHistory) Clear()

// Metrics wraps ov_genai_perf_metrics.
type Metrics struct {
    TTFT            float32 // time to first token (ms)
    TPOT            float32 // time per output token (ms)
    Throughput      float32 // tokens/sec
    InputTokens     int
    OutputTokens    int
}
```

---

## Integration into Contenox

### New model provider: `internal/modelrepo/openvino/`

Implements the existing `modelrepo.Provider` interface:

```go
type openvinoProvider struct {
    pipeline *openvino.Pipeline
    model    string
    caps     modelrepo.CapabilityConfig
}

func (p *openvinoProvider) GetChatConnection(ctx context.Context, backendID string) (modelrepo.LLMChatClient, error) {
    return &openvinoChat{pipeline: p.pipeline}, nil
}

func (p *openvinoProvider) GetStreamConnection(ctx context.Context, backendID string) (modelrepo.LLMStreamClient, error) {
    return &openvinoStream{pipeline: p.pipeline}, nil
}
```

Implements `LLMChatClient.Chat()` by:
1. Converting `[]modelrepo.Message` → `openvino.ChatHistory`
2. Mapping `ChatConfig.Tools` → `ChatHistory.SetTools()`
3. Calling `Pipeline.GenerateWithHistory()`
4. Parsing tool calls from the response (if model returns them)
5. Returning `ChatResult` with message and tool calls

### Backend type: `"local"` or `"openvino"`

```
contenox backend add local --type openvino --model-dir ~/.contenox/models/gemma4-e4b-int4
```

Or auto-detected: if `~/.contenox/models/` contains OpenVINO IR files, register
as a local backend automatically.

### Model management

```
contenox model pull gemma4-e4b          # downloads INT4 from HuggingFace
contenox model pull gemma4-e4b --format fp16
contenox model list                     # shows local + remote models
contenox model remove gemma4-e4b
```

Models stored at `~/.contenox/models/<name>/` with standard OpenVINO IR layout.

---

## Model Format & Distribution

### OpenVINO IR model directory layout

```
~/.contenox/models/gemma4-e4b-int4/
├── openvino_model.xml              # Model graph
├── openvino_model.bin              # Weights (~5 GB for E4B INT4)
├── openvino_tokenizer.xml          # Tokenizer (compiled as OV model)
├── openvino_tokenizer.bin
├── openvino_detokenizer.xml
├── openvino_detokenizer.bin
├── config.json                     # HuggingFace model config
├── generation_config.json          # Default generation params
├── tokenizer.json
├── tokenizer_config.json
├── special_tokens_map.json
└── chat_template.jinja             # Applied automatically
```

Tokenization is **fully automatic** — the pipeline loads the tokenizer from the
model directory. Chat templates are applied before tokenization. No external
tokenizer library needed.

### Converting models

Pre-converted models available on HuggingFace under `OpenVINO/` org:
- `OpenVINO/gemma-3-4b-it-int4-cw-ov` (available now)
- Gemma 4 variants will follow the same pattern

Manual conversion via `optimum-intel`:
```bash
pip install optimum-intel[openvino] nncf
optimum-cli export openvino \
    --model google/gemma-4-e4b \
    --weight-format int4 \
    ./gemma4-e4b-int4
```

### Download in contenox

`contenox model pull` would download from a registry (HuggingFace or self-hosted):
- Use HuggingFace Hub HTTP API to download snapshot
- Verify checksums
- Progress bar in terminal
- ~5 GB for Gemma 4 E4B INT4

---

## Runtime Dependencies

### Shared libraries needed at runtime

```
libopenvino_genai_c.so          # C API wrapper (what we link against)
libopenvino_genai.so            # C++ GenAI implementation
libopenvino.so                  # OpenVINO Runtime core
libopenvino_c.so                # OpenVINO Runtime C API
libopenvino_intel_cpu_plugin.so # CPU inference plugin
libopenvino_tokenizers.so       # Tokenizer plugin
```

### Distribution options

**Option A: System install (simplest for dev)**
```bash
# APT (Ubuntu/Debian)
sudo apt install openvino openvino-genai-dev

# Or from Intel archive
tar xf l_openvino_toolkit_ubuntu22_2026.x_x86_64.tgz
source setupvars.sh
```

CGo links against system-installed libraries. User must install OpenVINO Runtime.

**Option B: Vendored .so files (best for "one binary" story)**

Ship OpenVINO shared libraries alongside the contenox binary or embed them.
Set `RPATH` or `LD_LIBRARY_PATH` at runtime. The contenox binary + libs folder
becomes the full distribution.

```
contenox-linux-amd64/
├── contenox                    # Go binary
└── lib/
    ├── libopenvino_genai_c.so
    ├── libopenvino_genai.so
    ├── libopenvino.so
    ├── libopenvino_c.so
    ├── libopenvino_intel_cpu_plugin.so
    └── libopenvino_tokenizers.so
```

**Option C: Static linking (ideal but harder)**

OpenVINO can be built with static libs. Would produce a true single binary.
Requires building OpenVINO from source with `-DBUILD_SHARED_LIBS=OFF`. More
build complexity but best distribution story.

### System requirements

- **OS:** Linux (Ubuntu 20.04+, RHEL 9+), macOS, Windows
- **CPU:** x86_64 with SSE4.2 minimum; AVX2/AVX-512/AMX for best performance
- **ARM:** aarch64 supported via ARM CPU plugin
- **RAM:** ~5 GB for Gemma 4 E4B INT4 weights + KV cache overhead

---

## Expected Performance

Based on published OpenVINO benchmarks for similar-sized models (INT4, CPU):

| Hardware | Model Size | Expected tok/s |
|---|---|---|
| Intel Core i7 12th gen | 4B INT4 | ~20-30 |
| Intel Core Ultra 7/9 | 4B INT4 | ~25-40 |
| Intel Xeon (server) | 4B INT4 | ~50-150+ |
| AMD Zen 4 | 4B INT4 | ~15-25 |
| Apple M-series (ARM) | 4B INT4 | ~15-25 (via ARM plugin) |

20+ tokens/sec on a developer laptop is usable for interactive chat and tool
calling. The model loads in a few seconds; first-token latency is typically
under 1 second for 4B models.

---

## Implementation Phases

### Phase 1: Go bindings package (2-3 weeks)

**Deliverable:** `github.com/contenox/openvino-go` — standalone, reusable

- [ ] C shim for variadic `llm_pipeline_create`
- [ ] `Pipeline` type: create, close, generate (blocking)
- [ ] `GenerationConfig` with option functions
- [ ] `ChatHistory` + `JSONContainer` wrappers
- [ ] Streaming via callback bridge
- [ ] `PerfMetrics` wrapper
- [ ] Error mapping (`ov_status_e` → Go errors)
- [ ] Integration tests with a small model (Gemma 4 E2B or TinyLlama)
- [ ] CI: GitHub Actions with OpenVINO Runtime installed
- [ ] README + usage examples

### Phase 2: Contenox provider integration (1-2 weeks)

**Deliverable:** `internal/modelrepo/openvino/` provider

- [ ] Implement `modelrepo.Provider` interface
- [ ] Implement `LLMChatClient` — message conversion, tool call parsing
- [ ] Implement `LLMStreamClient` — streaming via channel
- [ ] Backend type `"openvino"` in `runtimetypes`
- [ ] Auto-detection of local models in `~/.contenox/models/`
- [ ] Wire into `runtimestate` backend cycle

### Phase 3: Model management (1 week)

**Deliverable:** `contenox model pull/list/remove`

- [ ] HuggingFace Hub download (HTTP API, no Python dependency)
- [ ] Progress bar for large downloads
- [ ] Model directory validation (check required files exist)
- [ ] `contenox model list` shows local models + sizes
- [ ] `contenox model remove` cleans up

### Phase 4: Zero-config experience (1 week)

**Deliverable:** `contenox init` works without Ollama

- [ ] `contenox init` detects no backend → offers to download a default model
- [ ] Auto-register local OpenVINO backend on first model pull
- [ ] `contenox init --provider local` skips cloud provider setup
- [ ] Update default chains to work with local model capabilities
- [ ] First-run experience: init → pull → chat in 3 commands

### Phase 5: Distribution & packaging (ongoing)

- [ ] Vendored .so distribution for Linux x86_64
- [ ] macOS support (ARM + Intel)
- [ ] Windows support
- [ ] Static linking investigation
- [ ] `.deb`/`.rpm` packages with OpenVINO libs included
- [ ] Homebrew formula

---

## Open Questions

1. **Tool calling format:** Gemma 4 supports function calling, but does the
   OpenVINO GenAI pipeline handle tool-call parsing, or do we need to parse the
   model's raw output ourselves? The chat_history API has `set_tools()` which
   suggests the pipeline handles tool formatting, but response parsing may be
   manual.

2. **Concurrent inference:** Can multiple goroutines call `Generate` on the same
   pipeline, or do we need a pool of pipelines? The C API docs don't specify
   thread safety. Likely need one pipeline per concurrent request, or a mutex.

3. **KV cache management:** The pipeline handles KV cache internally for chat
   sessions (`start_chat`/`finish_chat`). Need to understand memory growth
   characteristics for long conversations and whether we need to manage cache
   eviction.

4. **NPU support:** Intel NPU (Neural Processing Unit) on recent laptop CPUs
   could offload inference. The pipeline accepts `"NPU"` as device. Worth
   testing but not blocking for initial release.

5. **Model registry:** Should `contenox model pull` use HuggingFace directly, or
   should we host a curated registry of tested/validated model conversions?
   HuggingFace is faster to ship; own registry gives quality control.

---

## References

- OpenVINO GenAI repo: https://github.com/openvinotoolkit/openvino.genai
- C API headers: `src/c/include/openvino/genai/c/` in above repo
- C samples: `samples/c/text_generation/` in above repo
- Pre-converted models: https://huggingface.co/OpenVINO
- Model conversion: https://huggingface.co/docs/optimum-intel/en/openvino/export
- OpenVINO install: https://docs.openvino.ai/2025/get-started/install-openvino.html
- Gemma 4 model card: https://ai.google.dev/gemma/docs/core
