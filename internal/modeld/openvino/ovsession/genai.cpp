//go:build openvino && openvino_genai

#include "genai.h"

#include <any>
#include <array>
#include <atomic>
#include <algorithm>
#include <cctype>
#include <cmath>
#include <condition_variable>
#include <cstdint>
#include <cstdlib>
#include <cstring>
#include <deque>
#include <exception>
#include <filesystem>
#include <fstream>
#include <functional>
#include <limits>
#include <list>
#include <map>
#include <memory>
#include <mutex>
#include <numeric>
#include <optional>
#include <set>
#include <sstream>
#include <stdexcept>
#include <string>
#include <thread>
#include <tuple>
#include <utility>
#include <vector>

#include <openvino/core/version.hpp>
#include <openvino/openvino.hpp>
#include <openvino/runtime/intel_gpu/properties.hpp>
#include <openvino/runtime/intel_npu/properties.hpp>
#include <openvino/runtime/properties.hpp>

#define protected public
#include <openvino/genai/continuous_batching_pipeline.hpp>
#include "continuous_batching/pipeline_impl.hpp"
#undef protected

#include <openvino/genai/generation_config.hpp>
#include <openvino/genai/lora_adapter.hpp>
#include <openvino/genai/parsers.hpp>
#include <openvino/genai/scheduler_config.hpp>
#include <openvino/genai/sparse_attention.hpp>
#include <openvino/genai/streamer_base.hpp>
#include <openvino/genai/chat_history.hpp>
#include <openvino/genai/tokenizer.hpp>
#include <openvino/genai/json_container.hpp>

static void write_buf(char *dst, size_t dst_len, const std::string &value) {
    if (!dst || dst_len == 0) return;
    std::strncpy(dst, value.c_str(), dst_len - 1);
    dst[dst_len - 1] = '\0';
}

static char *alloc_c_string(const std::string &value, const char *what) {
    void *buf = std::malloc(value.size() + 1);
    if (!buf) {
        throw std::runtime_error(std::string("allocate OpenVINO GenAI ") + what);
    }
    auto *out = reinterpret_cast<char *>(buf);
    if (!value.empty()) {
        std::memcpy(out, value.data(), value.size());
    }
    out[value.size()] = '\0';
    return out;
}

static void clear_c_output(char **out, size_t *out_len) {
    if (out) {
        *out = nullptr;
    }
    if (out_len) {
        *out_len = 0;
    }
}

static void free_c_output(char **out, size_t *out_len) {
    if (out && *out) {
        std::free(*out);
    }
    clear_c_output(out, out_len);
}

static void set_c_output(const std::string &value, char **out, size_t *out_len, const char *what) {
    if (!out || !out_len) {
        throw std::runtime_error(std::string("OpenVINO GenAI ") + what + " output pointer is nil");
    }
    char *buf = alloc_c_string(value, what);
    *out = buf;
    *out_len = value.size();
}

template <size_t N>
static void write_fixed(char (&dst)[N], const std::string &value) {
    write_buf(dst, N, value);
}

static std::string upper_device_base(const std::string &device) {
    auto base = device.substr(0, device.find('.'));
    std::transform(base.begin(), base.end(), base.begin(), [](unsigned char c) {
        return static_cast<char>(std::toupper(c));
    });
    return base;
}

static bool is_accelerator_device(const std::string &device) {
    const auto base = upper_device_base(device);
    return base == "GPU" || base == "NPU";
}

static std::string select_available_device(const std::vector<std::string> &devices, const std::string &requested) {
    if (requested.empty()) {
        return "CPU";
    }
    for (const auto &dev : devices) {
        if (dev == requested) {
            return dev;
        }
    }
    const auto req_base = upper_device_base(requested);
    for (const auto &dev : devices) {
        if (upper_device_base(dev) == req_base) {
            return dev;
        }
    }
    return requested;
}

static void fill_device_info(ov::Core &core, const std::string &device, int index, cx_ov_device_info *out) {
    if (!out) return;
    std::memset(out, 0, sizeof(*out));
    out->index = index;
    write_fixed(out->name, device);

    const auto base = upper_device_base(device);
    std::string type = base;
    std::string description;
    bool shared = false;

    try {
        description = core.get_property(device, ov::device::full_name);
    } catch (...) {
        description = device;
    }

    try {
        const auto dev_type = core.get_property(device, ov::device::type);
        if (dev_type == ov::device::Type::INTEGRATED) {
            shared = true;
            if (!description.empty()) {
                description += " (integrated)";
            }
        } else if (dev_type == ov::device::Type::DISCRETE && !description.empty()) {
            description += " (discrete)";
        }
    } catch (...) {
    }

    if (base == "GPU") {
        type = shared ? "igpu" : "gpu";
        try {
            out->memory_total = core.get_property(device, ov::intel_gpu::device_total_mem_size);
            out->memory_total_known = 1;
        } catch (...) {
        }
        try {
            auto free = core.get_property(device, ov::intel_gpu::hint::available_device_mem);
            out->memory_free = static_cast<uint64_t>(free);
            out->memory_free_known = 1;
        } catch (...) {
        }
    } else if (base == "NPU") {
        type = "accel";
        try {
            out->memory_total = core.get_property(device, ov::intel_npu::device_total_mem_size);
            out->memory_total_known = 1;
        } catch (...) {
        }
        try {
            auto allocated = core.get_property(device, ov::intel_npu::device_alloc_mem_size);
            if (out->memory_total > allocated) {
                out->memory_free = out->memory_total - allocated;
            } else {
                out->memory_free = 0;
            }
            out->memory_free_known = 1;
        } catch (...) {
        }
    } else if (base == "CPU") {
        type = "cpu";
    }

    out->shared_with_display = shared ? 1 : 0;
    write_fixed(out->description, description);
    write_fixed(out->type, type);
}

extern "C" int cx_ov_runtime_info_get(cx_ov_runtime_info *out, char *err, size_t err_len) {
    try {
        if (!out) {
            throw std::runtime_error("runtime info output pointer is null");
        }
        std::memset(out, 0, sizeof(*out));
        write_fixed(out->runtime_name, "OpenVINO GenAI");

        const auto version = ov::get_openvino_version();
        std::string digest = version.buildNumber ? version.buildNumber : "";
        std::string desc = version.description ? version.description : "";
        write_fixed(out->runtime_digest, digest);
        write_fixed(out->system_info, desc);

        ov::Core core;
        auto devices = core.get_available_devices();
        out->device_count = std::min(devices.size(), static_cast<size_t>(16));
        for (size_t i = 0; i < out->device_count; ++i) {
            fill_device_info(core, devices[i], static_cast<int>(i), &out->devices[i]);
            if (is_accelerator_device(devices[i])) {
                out->supports_gpu_offload = 1;
            }
        }
        return 0;
    } catch (const std::exception &e) {
        write_buf(err, err_len, e.what());
        return 1;
    } catch (...) {
        write_buf(err, err_len, "unknown OpenVINO runtime info error");
        return 1;
    }
}

extern "C" int cx_ov_device_info_get(const char *device, cx_ov_device_info *out, char *err, size_t err_len) {
    try {
        if (!out) {
            throw std::runtime_error("device info output pointer is null");
        }
        ov::Core core;
        auto devices = core.get_available_devices();
        const std::string selected = select_available_device(devices, device ? std::string(device) : std::string("CPU"));
        fill_device_info(core, selected, 0, out);
        return 0;
    } catch (const std::exception &e) {
        write_buf(err, err_len, e.what());
        return 1;
    } catch (...) {
        write_buf(err, err_len, "unknown OpenVINO device info error");
        return 1;
    }
}

static ov::genai::SchedulerConfig scheduler_config_from(const cx_genai_session_config *config) {
    ov::genai::SchedulerConfig cfg;
    cfg.cache_size = config ? config->cache_size : 1;
    cfg.dynamic_split_fuse = !config || config->dynamic_split_fuse != 0;
    cfg.enable_prefix_caching = !config || config->enable_prefix_caching != 0;
    cfg.use_sparse_attention = !config || config->use_sparse_attention != 0;
    cfg.sparse_attention_config.mode = ov::genai::SparseAttentionMode::XATTENTION;
    cfg.sparse_attention_config.num_last_dense_tokens_in_prefill =
        config ? config->num_last_dense_tokens_in_prefill : 10;
    cfg.sparse_attention_config.xattention_threshold =
        config ? config->xattention_threshold : 0.9f;
    cfg.sparse_attention_config.xattention_block_size =
        config ? config->xattention_block_size : 128;
    cfg.sparse_attention_config.xattention_stride =
        config ? config->xattention_stride : 16;
    if (config && config->use_cache_eviction != 0) {
        // Native sink + recent + evictable-middle eviction. This is the OpenVINO
        // expression of the residency policy: keep the prefix (sinks) and a
        // recent window hot, evict the middle by attention importance.
        cfg.use_cache_eviction = true;
        cfg.cache_eviction_config = ov::genai::CacheEvictionConfig(
            config->cache_evict_start_size,
            config->cache_evict_recent_size,
            config->cache_evict_max_size,
            ov::genai::AggregationMode::NORM_SUM);
    }
    cfg.validate();
    return cfg;
}

static ov::Any kv_precision_from(const cx_genai_session_config *config) {
    std::string precision = (config && config->kv_cache_precision && config->kv_cache_precision[0])
        ? std::string(config->kv_cache_precision)
        : std::string("f16");
    if (precision == "f16" || precision == "FP16" || precision == "fp16") {
        return ov::element::f16;
    }
    if (precision == "f32" || precision == "FP32" || precision == "fp32") {
        return ov::element::f32;
    }
    if (precision == "u8" || precision == "U8") {
        return ov::element::u8;
    }
    if (precision == "i8" || precision == "I8") {
        return ov::element::i8;
    }
    throw std::runtime_error("unsupported KV_CACHE_PRECISION: " + precision);
}

static std::optional<int> json_int_field(const ov::genai::JsonContainer &root, const std::string &key) {
    if (!root.contains(key)) {
        return std::nullopt;
    }
    if (auto v = root[key].as_int()) {
        return static_cast<int>(*v);
    }
    return std::nullopt;
}

static std::string lower_ascii(std::string value) {
    std::transform(value.begin(), value.end(), value.begin(), [](unsigned char c) {
        return static_cast<char>(std::tolower(c));
    });
    return value;
}

static bool layer_type_is_windowed(const std::string &raw) {
    const auto v = lower_ascii(raw);
    return v == "sliding_attention" || v == "sliding" || v == "local_attention" ||
           v == "local" || v == "windowed_attention" || v == "windowed";
}

static bool layer_type_is_global(const std::string &raw) {
    const auto v = lower_ascii(raw);
    return v == "full_attention" || v == "full" || v == "global_attention" ||
           v == "global";
}

static bool pattern_char_is_windowed(char c) {
    c = static_cast<char>(std::tolower(static_cast<unsigned char>(c)));
    return c == 'l' || c == 's' || c == 'w';
}

static bool pattern_char_is_global(char c) {
    c = static_cast<char>(std::tolower(static_cast<unsigned char>(c)));
    return c == 'g' || c == 'f';
}

static void count_layer_types(const ov::genai::JsonContainer &layers, int num_layers, int &global, int &windowed) {
    const size_t n = std::min(static_cast<size_t>(std::max(num_layers, 0)), layers.size());
    for (size_t i = 0; i < n; ++i) {
        auto layer = layers[i].as_string();
        if (layer && layer_type_is_windowed(*layer)) {
            ++windowed;
        } else {
            ++global;
        }
    }
    global += std::max(num_layers - static_cast<int>(n), 0);
}

static void count_pattern_array(const ov::genai::JsonContainer &pattern, int num_layers, int &global, int &windowed) {
    const size_t n = std::min(static_cast<size_t>(std::max(num_layers, 0)), pattern.size());
    for (size_t i = 0; i < n; ++i) {
        const auto item = pattern[i];
        if (auto b = item.as_bool()) {
            *b ? ++windowed : ++global;
        } else if (auto s = item.as_string()) {
            if (layer_type_is_windowed(*s)) {
                ++windowed;
            } else if (layer_type_is_global(*s)) {
                ++global;
            } else {
                ++global;
            }
        } else if (auto v = item.as_int()) {
            *v != 0 ? ++windowed : ++global;
        } else {
            ++global;
        }
    }
    global += std::max(num_layers - static_cast<int>(n), 0);
}

static void count_pattern_string(const std::string &pattern, int num_layers, int &global, int &windowed) {
    if (pattern.empty()) {
        global = num_layers;
        return;
    }
    for (int i = 0; i < num_layers; ++i) {
        const char c = pattern[static_cast<size_t>(i) % pattern.size()];
        if (pattern_char_is_windowed(c)) {
            ++windowed;
        } else if (pattern_char_is_global(c)) {
            ++global;
        } else {
            ++global;
        }
    }
}

static void derive_attention_layer_split(const ov::genai::JsonContainer &cfg, cx_ov_model_kv_profile *out) {
    out->global_layers = out->num_hidden_layers;
    out->windowed_layers = 0;
    if (out->num_hidden_layers <= 0 || out->sliding_window <= 0) {
        return;
    }
    int global = 0;
    int windowed = 0;
    if (cfg.contains("layer_types") && cfg["layer_types"].is_array()) {
        count_layer_types(cfg["layer_types"], out->num_hidden_layers, global, windowed);
    } else if (cfg.contains("sliding_window_pattern")) {
        const auto pattern = cfg["sliding_window_pattern"];
        if (auto stride = pattern.as_int()) {
            if (*stride > 0) {
                global = (out->num_hidden_layers + static_cast<int>(*stride) - 1) / static_cast<int>(*stride);
                windowed = out->num_hidden_layers - global;
            }
        } else if (pattern.is_array()) {
            count_pattern_array(pattern, out->num_hidden_layers, global, windowed);
        } else if (auto s = pattern.as_string()) {
            count_pattern_string(*s, out->num_hidden_layers, global, windowed);
        }
    } else {
        windowed = out->num_hidden_layers;
    }
    out->global_layers = std::max(global, 0);
    out->windowed_layers = std::max(windowed, 0);
    if (out->global_layers + out->windowed_layers > out->num_hidden_layers) {
        out->windowed_layers = std::max(out->num_hidden_layers - out->global_layers, 0);
    }
}

static std::filesystem::path model_config_path_for(const std::string &model_path);

static cx_ov_model_kv_profile load_model_kv_profile(const std::string &model_path) {
    cx_ov_model_kv_profile out{};
    const auto config_path = model_config_path_for(model_path);
    std::ifstream in(config_path);
    if (!in.good()) {
        return out;
    }

    std::ostringstream raw;
    raw << in.rdbuf();
    auto root = ov::genai::JsonContainer::from_json_string(raw.str());
    auto cfg = root;
    if (root.contains("text_config") && root["text_config"].is_object()) {
        cfg = root["text_config"];
    } else if (root.contains("llm_config") && root["llm_config"].is_object()) {
        // VLM exports of the InternVL family nest the language model's
        // architecture under llm_config instead of text_config.
        cfg = root["llm_config"];
    }
    out.max_position_embeddings = json_int_field(cfg, "max_position_embeddings").value_or(0);
    out.num_hidden_layers = json_int_field(cfg, "num_hidden_layers").value_or(0);
    out.num_key_value_heads = json_int_field(cfg, "num_key_value_heads").value_or(0);
    out.num_attention_heads = json_int_field(cfg, "num_attention_heads").value_or(0);
    out.hidden_size = json_int_field(cfg, "hidden_size").value_or(0);
    out.head_dim = json_int_field(cfg, "head_dim").value_or(0);
    out.sliding_window = json_int_field(cfg, "sliding_window").value_or(0);
    derive_attention_layer_split(cfg, &out);
    return out;
}

extern "C" int cx_ov_model_kv_profile_get(const char *model_dir, cx_ov_model_kv_profile *out, char *err, size_t err_len) {
    try {
        if (!out) {
            throw std::runtime_error("model KV profile output pointer is null");
        }
        std::memset(out, 0, sizeof(*out));
        if (!model_dir || !model_dir[0]) {
            return 0;
        }
        *out = load_model_kv_profile(model_dir);
        return 0;
    } catch (const std::exception &e) {
        write_buf(err, err_len, e.what());
        return 1;
    } catch (...) {
        write_buf(err, err_len, "unknown OpenVINO model KV profile error");
        return 1;
    }
}

static std::string render_chat_template_probe(
        ov::genai::Tokenizer &tokenizer,
        const std::optional<ov::genai::JsonContainer> &tools,
        const std::optional<ov::genai::JsonContainer> &extra_context) {
    std::vector<ov::AnyMap> msgs;
    msgs.push_back(ov::AnyMap{
        {"role", std::string("user")},
        {"content", std::string("cx probe ping")},
    });
    ov::genai::ChatHistory history(msgs);
    return tokenizer.apply_chat_template(history, true, std::string{}, tools, extra_context);
}

static std::optional<ov::genai::JsonContainer> probe_extra_context_bool(const std::string &key, bool value) {
    ov::AnyMap extra;
    extra[key] = value;
    return ov::genai::JsonContainer(extra);
}

static std::optional<ov::genai::JsonContainer> probe_extra_context_string(const std::string &key, const std::string &value) {
    ov::AnyMap extra;
    extra[key] = value;
    return ov::genai::JsonContainer(extra);
}

static bool contains_any(const std::string &s, const std::vector<std::string> &needles) {
    for (const auto &needle : needles) {
        if (s.find(needle) != std::string::npos) {
            return true;
        }
    }
    return false;
}

static std::string detect_thinking_start_tag(const std::vector<std::string> &prompts) {
    const std::vector<std::string> tags = {
        "<think>",
        "<|START_THINKING|>",
        "<|channel|>analysis",
        "<|start_header_id|>analysis<|end_header_id|>",
    };
    for (const auto &tag : tags) {
        for (const auto &prompt : prompts) {
            if (prompt.find(tag) != std::string::npos) {
                return tag;
            }
        }
    }
    return {};
}

extern "C" int cx_ov_chat_template_probe_get(const char *model_dir, cx_ov_chat_template_probe *out, char *err, size_t err_len) {
    try {
        if (!out) {
            throw std::runtime_error("chat template probe output pointer is null");
        }
        std::memset(out, 0, sizeof(*out));
        if (!model_dir || !model_dir[0]) {
            return 0;
        }

        ov::genai::Tokenizer tokenizer{std::filesystem::path(model_dir)};
        if (tokenizer.get_chat_template().empty()) {
            return 0;
        }

        const auto plain = render_chat_template_probe(tokenizer, std::nullopt, std::nullopt);
        write_fixed(out->format_name, "openvino:minja");

        try {
            auto tools = ov::genai::JsonContainer::from_json_string(
                R"([{"type":"function","function":{"name":"cx_probe_tool","description":"cx probe tool","parameters":{"type":"object","properties":{"cx_probe_arg":{"type":"string","description":"probe arg"}},"required":["cx_probe_arg"]}}}])");
            const auto with_tools = render_chat_template_probe(tokenizer, tools, std::nullopt);
            out->supports_tool_calls =
                (with_tools != plain && contains_any(with_tools, {"cx_probe_tool", "cx_probe_arg"})) ? 1 : 0;
        } catch (...) {
            out->supports_tool_calls = 0;
        }

        std::vector<std::string> thinking_prompts;
        thinking_prompts.push_back(plain);
        try {
            const auto thinking_on = render_chat_template_probe(
                tokenizer, std::nullopt, probe_extra_context_bool("enable_thinking", true));
            const auto thinking_off = render_chat_template_probe(
                tokenizer, std::nullopt, probe_extra_context_bool("enable_thinking", false));
            thinking_prompts.push_back(thinking_on);
            thinking_prompts.push_back(thinking_off);
            out->supports_thinking =
                (thinking_on != thinking_off || thinking_on != plain || thinking_off != plain) ? 1 : 0;
        } catch (...) {
            out->supports_thinking = 0;
        }

        const auto thinking_tag = detect_thinking_start_tag(thinking_prompts);
        write_fixed(out->thinking_start_tag, thinking_tag);

        try {
            const auto low = render_chat_template_probe(
                tokenizer, std::nullopt, probe_extra_context_string("reasoning_effort", "low"));
            const auto high = render_chat_template_probe(
                tokenizer, std::nullopt, probe_extra_context_string("reasoning_effort", "high"));
            out->supports_reasoning_effort = (low != high) ? 1 : 0;
        } catch (...) {
            out->supports_reasoning_effort = 0;
        }

        return 0;
    } catch (const std::exception &e) {
        write_buf(err, err_len, e.what());
        return 1;
    } catch (...) {
        write_buf(err, err_len, "unknown OpenVINO chat template probe error");
        return 1;
    }
}

struct model_rope_config {
    double theta = 10000.0;
    std::string scaling_json;
};

static std::filesystem::path model_config_path_for(const std::string &model_path) {
    std::filesystem::path base(model_path);
    std::error_code ec;
    if (std::filesystem::is_regular_file(base, ec)) {
        base = base.parent_path();
    }
    return base / "config.json";
}

static model_rope_config load_model_rope_config(const std::string &model_path) {
    model_rope_config cfg;
    const auto config_path = model_config_path_for(model_path);
    std::ifstream in(config_path);
    if (!in.good()) {
        return cfg;
    }

    std::ostringstream raw;
    raw << in.rdbuf();
    auto root = ov::genai::JsonContainer::from_json_string(raw.str());
    if (root.contains("rope_theta") && !root["rope_theta"].is_null()) {
        std::optional<double> theta = root["rope_theta"].as_double();
        if (!theta) {
            if (auto as_int = root["rope_theta"].as_int()) {
                theta = static_cast<double>(*as_int);
            }
        }
        if (!theta || !std::isfinite(*theta) || *theta <= 0.0) {
            throw std::runtime_error("OpenVINO model config rope_theta must be a positive number");
        }
        cfg.theta = *theta;
    }
    if (root.contains("rope_scaling")) {
        auto scaling = root["rope_scaling"];
        if (!scaling.is_null()) {
            cfg.scaling_json = scaling.to_json_string();
        }
    }
    return cfg;
}

struct cx_genai_session {
    std::unique_ptr<ov::genai::ContinuousBatchingPipeline> pipe;
    std::atomic<bool> cancel_requested{false};
    std::string kv_cache_precision = "f16";
    double rope_theta = 10000.0;
    std::string rope_scaling_json;
    // Dynamic LoRA adapters registered on this pipeline, if any. Held so every
    // generation config built from get_config() activates the same adapter set at
    // the same alpha. nullopt = base model, no adapters.
    std::optional<ov::genai::AdapterConfig> adapters;

    std::mutex mu;
    std::condition_variable cv;
    bool stopping = false;
    bool busy = false;
    bool has_task = false;
    bool done = false;
    std::function<void()> task;
    std::exception_ptr task_error;
    std::thread worker;

    cx_genai_session() : worker([this] { loop(); }) {}

    ~cx_genai_session() {
        shutdown();
    }

    void run(std::function<void()> fn) {
        std::unique_lock<std::mutex> lock(mu);
        cv.wait(lock, [this] { return (!busy && !has_task) || stopping; });
        if (stopping) {
            throw std::runtime_error("OpenVINO GenAI session is closing");
        }

        busy = true;
        done = false;
        task_error = nullptr;
        task = std::move(fn);
        has_task = true;
        cv.notify_all();

        cv.wait(lock, [this] { return done; });
        busy = false;
        cv.notify_all();

        if (task_error) {
            std::rethrow_exception(task_error);
        }
    }

    void shutdown() {
        if (!worker.joinable()) {
            return;
        }

        try {
            run([this] { pipe.reset(); });
        } catch (...) {
            // Destructors must not throw. The caller-facing free path is best-effort.
        }

        {
            std::lock_guard<std::mutex> lock(mu);
            stopping = true;
            cv.notify_all();
        }
        worker.join();
    }

    void loop() {
        for (;;) {
            std::function<void()> fn;
            {
                std::unique_lock<std::mutex> lock(mu);
                cv.wait(lock, [this] { return has_task || stopping; });
                if (stopping && !has_task) {
                    return;
                }
                fn = std::move(task);
                task = nullptr;
                has_task = false;
            }

            std::exception_ptr err;
            try {
                fn();
            } catch (...) {
                err = std::current_exception();
            }

            {
                std::lock_guard<std::mutex> lock(mu);
                task_error = err;
                done = true;
                cv.notify_all();
            }
        }
    }
};

struct cx_genai_stream_chunk {
    std::string text;
    std::string thinking;
};

struct cx_genai_stream {
    std::mutex mu;
    std::condition_variable cv;
    std::deque<cx_genai_stream_chunk> chunks;
    bool done = false;
    int rc = 0;
    std::string error;

    void push(const std::string &text, const std::string &thinking) {
        if (text.empty() && thinking.empty()) return;
        std::lock_guard<std::mutex> lock(mu);
        chunks.push_back(cx_genai_stream_chunk{text, thinking});
        cv.notify_all();
    }

    void finish(int code, const std::string &message = "") {
        std::lock_guard<std::mutex> lock(mu);
        rc = code;
        error = message;
        done = true;
        cv.notify_all();
    }
};

// Seed from the model's own generation config (eos_token_id, sampling defaults,
// repetition_penalty from generation_config.json) so model behavior is honored;
// override only the fields the caller specifies. apply_chat_template stays false
// because the prompt is already templated with the model's own template upstream.
static ov::genai::GenerationConfig generation_config_from(ov::genai::GenerationConfig gen,
                                                          size_t max_new_tokens,
                                                          float temperature,
                                                          int use_temperature,
                                                          float top_p,
                                                          int use_top_p,
                                                          size_t top_k,
                                                          int use_top_k,
                                                          size_t seed,
                                                          int use_seed) {
    if (max_new_tokens > 0) {
        gen.max_new_tokens = max_new_tokens;
    } else if (gen.max_new_tokens == 0 || gen.max_new_tokens == SIZE_MAX) {
        gen.max_new_tokens = 256;
    }
    // Some HF generation_config.json files carry a small total-token max_length
    // (for example 2048). ContinuousBatchingPipeline still asserts max_length
    // against prompt_len even when max_new_tokens is set, so clear the inherited
    // total cap and let the explicit prompt-exclusive budget govern generation.
    gen.max_length = SIZE_MAX;
    gen.apply_chat_template = false;
    if (use_temperature) {
        gen.temperature = temperature;
        gen.do_sample = temperature > 0.0f;
    }
    if (use_top_p) {
        gen.top_p = top_p;
        gen.do_sample = true;
    }
    if (use_top_k) {
        gen.top_k = top_k;
        gen.do_sample = true;
    }
    if (use_seed) {
        gen.rng_seed = seed;
    }
    gen.validate();
    return gen;
}

static ov::genai::GenerationConfig prefill_config_from(ov::genai::GenerationConfig gen) {
    gen.max_new_tokens = 0;
    gen.max_length = SIZE_MAX;
    gen.min_new_tokens = 0;
    gen.echo = true;
    gen.apply_chat_template = false;
    gen.validate();
    return gen;
}

static ov::Tensor tensor_from_tokens(const std::vector<int64_t> &tokens) {
    ov::Tensor input(ov::element::i64, ov::Shape{1, tokens.size()});
    if (!tokens.empty()) {
        std::copy(tokens.begin(), tokens.end(), input.data<int64_t>());
    }
    return input;
}

static std::string decode_first_generation(ov::genai::Tokenizer tokenizer,
                                           const std::vector<ov::genai::EncodedGenerationResult> &results,
                                           bool canceled) {
    if (results.empty() || results[0].m_generation_ids.empty()) {
        if (canceled) {
            return std::string();
        }
        throw std::runtime_error("OpenVINO GenAI returned no generation");
    }
    return tokenizer.decode(results[0].m_generation_ids[0]);
}

static void copy_metrics(const ov::genai::PipelineMetrics &src, cx_genai_metrics *dst) {
    if (!dst) return;
    dst->requests = src.requests;
    dst->scheduled_requests = src.scheduled_requests;
    dst->cache_usage = src.cache_usage;
    dst->max_cache_usage = src.max_cache_usage;
    dst->avg_cache_usage = src.avg_cache_usage;
    dst->inference_duration = src.inference_duration;
    dst->cache_size_in_bytes = src.cache_size_in_bytes;
}

static std::vector<std::string> split_protocols(const char *value) {
    std::vector<std::string> out;
    if (!value || !value[0]) {
        return out;
    }

    std::stringstream ss{std::string(value)};
    std::string item;
    while (std::getline(ss, item)) {
        while (!item.empty() && (item.back() == '\r' || item.back() == '\n' || item.back() == ' ' || item.back() == '\t')) {
            item.pop_back();
        }
        size_t start = 0;
        while (start < item.size() && (item[start] == ' ' || item[start] == '\t')) {
            start++;
        }
        if (start > 0) {
            item.erase(0, start);
        }
        if (!item.empty()) {
            out.push_back(item);
        }
    }
    return out;
}

static std::shared_ptr<ov::genai::Parser> parser_for_protocol(const std::string &protocol) {
    if (protocol == "openvino:llama3_pythonic_tool_parser") {
        return std::make_shared<ov::genai::Llama3PythonicToolParser>();
    }
    if (protocol == "openvino:llama3_json_tool_parser") {
        return std::make_shared<ov::genai::Llama3JsonToolParser>();
    }
    if (protocol == "openvino:reasoning_parser") {
        return std::make_shared<ov::genai::ReasoningParser>(/*expect_open_tag=*/true, /*keep_original_content=*/false);
    }
    if (protocol == "openvino:deepseek_r1_reasoning_parser") {
        return std::make_shared<ov::genai::ReasoningParser>(/*expect_open_tag=*/false, /*keep_original_content=*/false);
    }
    if (protocol == "openvino:phi4_reasoning_parser") {
        return std::make_shared<ov::genai::ReasoningParser>(/*expect_open_tag=*/true, /*keep_original_content=*/false);
    }
    if (protocol == "openvino:vllm_parser_wrapper") {
        throw std::runtime_error("openvino:vllm_parser_wrapper is a Python OpenVINO GenAI binding and is not available in the native C++ session bridge");
    }
    if (protocol == "openvino:reasoning_incremental_parser" ||
        protocol == "openvino:deepseek_r1_reasoning_incremental_parser" ||
        protocol == "openvino:phi4_reasoning_incremental_parser") {
        throw std::runtime_error(protocol + " requires the streaming parser bridge; non-stream chat generation accepts complete-output Parser protocols only");
    }
    throw std::runtime_error("unsupported OpenVINO parser protocol: " + protocol);
}

static std::shared_ptr<ov::genai::IncrementalParser> incremental_parser_for_protocol(const std::string &protocol) {
    if (protocol == "openvino:reasoning_incremental_parser") {
        return std::make_shared<ov::genai::ReasoningIncrementalParser>(/*expect_open_tag=*/true, /*keep_original_content=*/false);
    }
    if (protocol == "openvino:deepseek_r1_reasoning_incremental_parser") {
        return std::make_shared<ov::genai::ReasoningIncrementalParser>(/*expect_open_tag=*/false, /*keep_original_content=*/false);
    }
    if (protocol == "openvino:phi4_reasoning_incremental_parser") {
        return std::make_shared<ov::genai::ReasoningIncrementalParser>(/*expect_open_tag=*/true, /*keep_original_content=*/false);
    }
    if (protocol == "openvino:llama3_pythonic_tool_parser" ||
        protocol == "openvino:llama3_json_tool_parser" ||
        protocol == "openvino:reasoning_parser" ||
        protocol == "openvino:deepseek_r1_reasoning_parser" ||
        protocol == "openvino:phi4_reasoning_parser") {
        throw std::runtime_error(protocol + " requires the complete-output parser bridge; stream generation accepts incremental Parser protocols only");
    }
    if (protocol == "openvino:vllm_parser_wrapper") {
        throw std::runtime_error("openvino:vllm_parser_wrapper is a Python OpenVINO GenAI binding and is not available in the native C++ session bridge");
    }
    throw std::runtime_error("unsupported OpenVINO incremental parser protocol: " + protocol);
}

static std::vector<std::shared_ptr<ov::genai::IncrementalParser>> incremental_parsers_for_protocols(const std::vector<std::string> &protocols) {
    std::vector<std::shared_ptr<ov::genai::IncrementalParser>> parsers;
    parsers.reserve(protocols.size());
    for (const auto &protocol : protocols) {
        parsers.push_back(incremental_parser_for_protocol(protocol));
    }
    return parsers;
}

static std::vector<std::shared_ptr<ov::genai::Parser>> parsers_for_protocols(const std::vector<std::string> &protocols) {
    std::vector<std::shared_ptr<ov::genai::Parser>> parsers;
    parsers.reserve(protocols.size());
    for (const auto &protocol : protocols) {
        parsers.push_back(parser_for_protocol(protocol));
    }
    return parsers;
}

static void apply_structured_output(ov::genai::GenerationConfig &gen,
                                    const char *protocol_c,
                                    const char *payload_c) {
    std::string protocol = (protocol_c && protocol_c[0]) ? std::string(protocol_c) : std::string();
    if (protocol.empty()) {
        return;
    }
    std::string payload = (payload_c && payload_c[0]) ? std::string(payload_c) : std::string();
    ov::genai::StructuredOutputConfig cfg;
    if (protocol == "openvino:json_schema") {
        cfg.json_schema = payload;
    } else if (protocol == "openvino:regex") {
        cfg.regex = payload;
    } else if (protocol == "openvino:ebnf") {
        cfg.grammar = payload;
    } else if (protocol == "openvino:const_string") {
        cfg.structural_tags_config = ov::genai::StructuredOutputConfig::StructuralTag{
            ov::genai::StructuredOutputConfig::ConstString(payload)};
    } else if (protocol == "openvino:any_text") {
        cfg.structural_tags_config = ov::genai::StructuredOutputConfig::StructuralTag{
            ov::genai::StructuredOutputConfig::AnyText{}};
    } else if (protocol == "openvino:structural_tag" ||
               protocol == "openvino:triggered_tags" ||
               protocol == "openvino:tags_with_separator" ||
               protocol == "openvino:concat" ||
               protocol == "openvino:union") {
        cfg.structural_tags_config = ov::genai::StructuredOutputConfig::StructuralTag{payload};
    } else {
        throw std::runtime_error("unsupported OpenVINO structured-output protocol: " + protocol);
    }
    cfg.validate();
    gen.structured_output_config = cfg;
}

static std::string parse_generated(const std::vector<std::shared_ptr<ov::genai::Parser>> &parsers,
                                   const std::string &generated) {
    if (parsers.empty()) {
        return std::string();
    }
    ov::genai::JsonContainer message;
    message["content"] = generated;
    for (auto &parser : parsers) {
        parser->parse(message);
    }
    return message.to_json_string();
}

static std::string json_string_field(const ov::genai::JsonContainer &message, const std::string &key) {
    if (!message.contains(key)) {
        return std::string();
    }
    auto value = message[key].as_string();
    return value.has_value() ? *value : std::string();
}

static constexpr std::array<uint8_t, 8> k_cold_kv_magic{{'O', 'V', 'C', 'K', 'V', '0', '1', '\0'}};
static constexpr uint64_t k_cold_kv_version = 1;

// Private OpenVINO GenAI cold-KV plumbing. These mirrors match the pinned
// 2026.2.0.0 private headers included above; keep all direct layout access here.
struct genai_scheduler_access {
    bool m_can_use_partial_preemption;
    ov::genai::SchedulerConfig m_config;
    std::shared_ptr<ov::genai::CacheOrchestrator> m_cache_orchestrator;
};

struct genai_block_manager_access {
    ov::genai::BlockAllocator m_allocator;
    bool m_enable_prefix_caching;
    size_t m_block_size;
    size_t m_num_layers;
    size_t m_fixed_blocks_per_sequence;
    std::map<uint64_t, ov::genai::BlocksPerLayer> m_prefix_hash_to_occupied_block_map;
    std::map<uint64_t, std::vector<ov::genai::BlocksPerLayer>> m_block_table;
    std::mutex m_cached_blocks_map_mutex;
};

static genai_block_manager_access &genai_private(ov::genai::BlockManager &block_manager) {
    return *reinterpret_cast<genai_block_manager_access *>(&block_manager);
}

class cold_kv_writer {
public:
    std::vector<uint8_t> data;

    template <typename T>
    void pod(T value) {
        const auto *src = reinterpret_cast<const uint8_t *>(&value);
        data.insert(data.end(), src, src + sizeof(T));
    }

    void bytes(const void *src, size_t n) {
        if (n == 0) {
            return;
        }
        if (!src) {
            throw std::runtime_error("cold KV payload source bytes are null");
        }
        const auto *p = reinterpret_cast<const uint8_t *>(src);
        data.insert(data.end(), p, p + n);
    }

    void string(const std::string &value) {
        pod<uint64_t>(value.size());
        bytes(value.data(), value.size());
    }
};

class cold_kv_reader {
    const uint8_t *data_;
    size_t len_;
    size_t off_ = 0;

public:
    cold_kv_reader(const uint8_t *data, size_t len) : data_(data), len_(len) {}

    template <typename T>
    T pod() {
        if (off_ > len_ || len_ - off_ < sizeof(T)) {
            throw std::runtime_error("cold KV payload is truncated");
        }
        T value{};
        std::memcpy(&value, data_ + off_, sizeof(T));
        off_ += sizeof(T);
        return value;
    }

    std::vector<uint8_t> bytes(size_t n) {
        if (off_ > len_ || len_ - off_ < n) {
            throw std::runtime_error("cold KV payload is truncated");
        }
        std::vector<uint8_t> out(data_ + off_, data_ + off_ + n);
        off_ += n;
        return out;
    }

    std::string string() {
        const auto n = checked_size(pod<uint64_t>(), "cold KV string");
        auto raw = bytes(n);
        return std::string(reinterpret_cast<const char *>(raw.data()), raw.size());
    }

    void expect_finished() const {
        if (off_ != len_) {
            throw std::runtime_error("cold KV payload has trailing bytes");
        }
    }

    static size_t checked_size(uint64_t value, const char *what) {
        if (value > static_cast<uint64_t>(std::numeric_limits<size_t>::max())) {
            throw std::runtime_error(std::string(what) + " size exceeds host size_t");
        }
        return static_cast<size_t>(value);
    }
};

static size_t checked_size(uint64_t value, const char *what) {
    return cold_kv_reader::checked_size(value, what);
}

static std::shared_ptr<ov::genai::CacheOrchestrator> genai_cache_orchestrator(cx_genai_session *s) {
    if (!s || !s->pipe) {
        throw std::runtime_error("OpenVINO GenAI session is nil or closed");
    }
    auto impl = std::dynamic_pointer_cast<ov::genai::ContinuousBatchingPipeline::ContinuousBatchingImpl>(s->pipe->m_impl);
    if (!impl) {
        throw std::runtime_error("OpenVINO GenAI pipeline is not continuous batching");
    }
    if (!impl->m_scheduler) {
        throw std::runtime_error("OpenVINO GenAI scheduler cache is not initialized");
    }
    auto orchestrator = reinterpret_cast<genai_scheduler_access *>(impl->m_scheduler.get())->m_cache_orchestrator;
    if (!orchestrator) {
        throw std::runtime_error("OpenVINO GenAI scheduler cache is not initialized");
    }
    return orchestrator;
}

static const ov::genai::KVCacheManager &genai_kv_cache_manager(const ov::genai::CacheOrchestrator &orchestrator) {
    const auto &base = orchestrator.get_cache_manager(ov::genai::CacheType::KV_CACHE);
    const auto *kv = dynamic_cast<const ov::genai::KVCacheManager *>(&base);
    if (!kv) {
        throw std::runtime_error("OpenVINO GenAI KV cache manager is not available");
    }
    return *kv;
}

static bool genai_cold_kv_supported_locked(cx_genai_session *s) {
    try {
        if (!s) {
            return false;
        }
        const auto &precision = s->kv_cache_precision;
        const bool float_kv = precision == "f16" || precision == "fp16" || precision == "FP16" ||
                              precision == "f32" || precision == "fp32" || precision == "FP32";
        if (!float_kv) {
            return false;
        }
        auto orchestrator = genai_cache_orchestrator(s);
        auto &block_manager = orchestrator->get_block_manager(ov::genai::CacheType::KV_CACHE);
        auto &block_private = genai_private(block_manager);
        const auto &kv_manager = genai_kv_cache_manager(*orchestrator);
        const auto block_layers = block_manager.get_num_layers();
        const auto kv_layers = kv_manager.get_num_layers();
        return block_private.m_enable_prefix_caching &&
               block_manager.get_block_size() > 0 &&
               block_manager.get_total_number_of_kv_blocks() > 0 &&
               kv_layers > 0 &&
               (block_layers == 1 || block_layers == kv_layers);
    } catch (...) {
        return false;
    }
}

static size_t prefix_hash_make(const std::vector<int64_t> &tokens,
                               size_t content_length,
                               size_t block_size,
                               const std::vector<int64_t> &prefix_hashes) {
    if (content_length == 0 || block_size == 0 || content_length > tokens.size()) {
        throw std::runtime_error("invalid OpenVINO prefix hash input");
    }

    size_t block_start = content_length - (content_length % block_size);
    if (block_start == content_length) {
        block_start -= block_size;
    }
    const size_t filled_blocks = block_start / block_size;
    if (filled_blocks > prefix_hashes.size()) {
        throw std::runtime_error("invalid OpenVINO prefix hash chain");
    }

    std::vector<int64_t> content;
    if (filled_blocks > 0) {
        content.emplace_back(prefix_hashes[filled_blocks - 1]);
    }
    content.insert(content.end(), tokens.begin() + block_start, tokens.begin() + content_length);
    const char *raw = reinterpret_cast<const char *>(content.data());
    const std::size_t raw_len = content.size() * sizeof(content[0]);
    return std::hash<std::string_view>{}(std::string_view(raw, raw_len));
}

static size_t prefix_hash_for_tokens(const std::vector<int64_t> &tokens,
                                     size_t content_length,
                                     size_t block_size) {
    if (content_length == 0 || content_length > tokens.size()) {
        throw std::runtime_error("OpenVINO cold KV prefix tokens do not cover the requested content length");
    }
    std::vector<int64_t> prefix_hashes;
    for (size_t cur = block_size; cur <= content_length; cur += block_size) {
        prefix_hashes.push_back(static_cast<int64_t>(prefix_hash_make(tokens, cur, block_size, prefix_hashes)));
    }
    if (content_length % block_size == 0) {
        return static_cast<size_t>(prefix_hashes[content_length / block_size - 1]);
    }
    return prefix_hash_make(tokens, content_length, block_size, prefix_hashes);
}

static std::vector<size_t> cold_content_lengths_for_range(size_t start,
                                                          size_t end,
                                                          size_t prefix_len,
                                                          size_t block_size) {
    if (block_size == 0) {
        throw std::runtime_error("OpenVINO KV block size is zero");
    }
    if (start >= end || end > prefix_len) {
        throw std::runtime_error("OpenVINO cold KV range is outside prefix tokens");
    }
    const size_t first_block = start / block_size;
    const size_t last_block = (end - 1) / block_size;
    std::vector<size_t> out;
    out.reserve(last_block - first_block + 1);
    for (size_t block = first_block; block <= last_block; ++block) {
        const size_t content_len = std::min((block + 1) * block_size, prefix_len);
        if (content_len == 0 || content_len <= block * block_size) {
            throw std::runtime_error("OpenVINO cold KV range maps to an empty KV block");
        }
        out.push_back(content_len);
    }
    return out;
}

static ov::genai::BlocksPerLayer find_prefix_blocks_locked(ov::genai::BlockManager &block_manager,
                                                           size_t hash) {
    auto &block_private = genai_private(block_manager);
    auto occupied_it = block_private.m_prefix_hash_to_occupied_block_map.find(static_cast<uint64_t>(hash));
    if (occupied_it != block_private.m_prefix_hash_to_occupied_block_map.end()) {
        return occupied_it->second;
    }
    return {};
}

static ov::genai::BlocksPerLayer ensure_import_blocks_locked(ov::genai::BlockManager &block_manager,
                                                             size_t hash) {
    auto blocks = find_prefix_blocks_locked(block_manager, hash);
    if (!blocks.empty()) {
        return blocks;
    }
    auto &block_private = genai_private(block_manager);
    if (!block_private.m_allocator.can_allocate_blocks(1)) {
        throw std::runtime_error("OpenVINO GenAI has no free KV blocks for cold import");
    }
    blocks = block_private.m_allocator.allocate_block(hash, block_private.m_prefix_hash_to_occupied_block_map);
    if (blocks.empty()) {
        throw std::runtime_error("OpenVINO GenAI failed to allocate KV blocks for cold import");
    }
    block_private.m_allocator.free(blocks, block_private.m_prefix_hash_to_occupied_block_map);
    blocks = find_prefix_blocks_locked(block_manager, hash);
    if (blocks.empty()) {
        throw std::runtime_error("OpenVINO GenAI failed to register imported KV blocks");
    }
    return blocks;
}

static size_t cache_block_index_for_layer(const ov::genai::BlocksPerLayer &blocks,
                                          size_t decoder_layer,
                                          size_t decoder_layers) {
    if (blocks.empty()) {
        throw std::runtime_error("OpenVINO prefix cache block entry is empty");
    }
    if (blocks.size() == 1) {
        return static_cast<size_t>(blocks[0]->get_index());
    }
    if (blocks.size() != decoder_layers || decoder_layer >= blocks.size()) {
        throw std::runtime_error("OpenVINO prefix cache block layer count does not match KV tensors");
    }
    return static_cast<size_t>(blocks[decoder_layer]->get_index());
}

static size_t cache_block_stride_bytes(const ov::Tensor &cache) {
    const auto shape = cache.get_shape();
    if (shape.empty()) {
        throw std::runtime_error("OpenVINO KV cache tensor rank is zero");
    }
    size_t elems = 1;
    for (auto it = std::next(shape.begin()); it != shape.end(); ++it) {
        if (*it != 0 && elems > std::numeric_limits<size_t>::max() / *it) {
            throw std::runtime_error("OpenVINO KV cache tensor block shape overflows size_t");
        }
        elems *= *it;
    }
    const auto elem_type = cache.get_element_type();
    const size_t bits = (elem_type == ov::element::u4 || elem_type == ov::element::i4)
        ? 4
        : elem_type.size() * 8;
    if (bits == 0 || elems > (std::numeric_limits<size_t>::max() - 7) / bits) {
        throw std::runtime_error("OpenVINO KV cache tensor block byte size overflows size_t");
    }
    return (elems * bits + 7) / 8;
}

static std::pair<ov::Coordinate, ov::Coordinate> cache_block_roi_coords(const ov::Tensor &cache,
                                                                        size_t block_index) {
    auto shape = cache.get_shape();
    if (shape.empty()) {
        throw std::runtime_error("OpenVINO KV cache tensor rank is zero");
    }
    if (block_index >= shape[0]) {
        throw std::runtime_error("OpenVINO KV cache block index is outside tensor shape");
    }
    ov::Coordinate begin(shape.size(), 0);
    ov::Coordinate end = shape;
    begin[0] = block_index;
    end[0] = block_index + 1;
    return {begin, end};
}

static std::vector<uint8_t> read_cache_block_bytes(ov::Tensor cache, size_t block_index) {
    if (cache.is<ov::RemoteTensor>()) {
        auto coords = cache_block_roi_coords(cache, block_index);
        ov::RemoteTensor roi(cache, coords.first, coords.second);
        ov::Tensor host(roi.get_element_type(), roi.get_shape());
        roi.copy_to(host);
        const size_t n = host.get_byte_size();
        std::vector<uint8_t> out(n);
        if (n > 0) {
            std::memcpy(out.data(), host.data(), n);
        }
        return out;
    }

    const size_t stride = cache_block_stride_bytes(cache);
    auto shape = cache.get_shape();
    if (block_index >= shape[0]) {
        throw std::runtime_error("OpenVINO KV cache block index is outside tensor shape");
    }
    std::vector<uint8_t> out(stride);
    if (stride > 0) {
        const auto *src = reinterpret_cast<const uint8_t *>(cache.data()) + block_index * stride;
        std::memcpy(out.data(), src, stride);
    }
    return out;
}

static void write_cache_block_bytes(ov::Tensor cache, size_t block_index, const std::vector<uint8_t> &bytes) {
    if (cache.is<ov::RemoteTensor>()) {
        auto coords = cache_block_roi_coords(cache, block_index);
        ov::RemoteTensor roi(cache, coords.first, coords.second);
        ov::Tensor host(roi.get_element_type(), roi.get_shape());
        if (host.get_byte_size() != bytes.size()) {
            throw std::runtime_error("OpenVINO cold KV payload block size does not match remote tensor");
        }
        if (!bytes.empty()) {
            std::memcpy(host.data(), bytes.data(), bytes.size());
        }
        roi.copy_from(host);
        return;
    }

    const size_t stride = cache_block_stride_bytes(cache);
    if (stride != bytes.size()) {
        throw std::runtime_error("OpenVINO cold KV payload block size does not match tensor");
    }
    auto shape = cache.get_shape();
    if (block_index >= shape[0]) {
        throw std::runtime_error("OpenVINO KV cache block index is outside tensor shape");
    }
    if (!bytes.empty()) {
        auto *dst = reinterpret_cast<uint8_t *>(cache.data()) + block_index * stride;
        std::memcpy(dst, bytes.data(), bytes.size());
    }
}

static size_t cache_content_block_start(size_t content_len, size_t block_size) {
    if (content_len == 0 || block_size == 0) {
        throw std::runtime_error("OpenVINO cold KV content length is invalid");
    }
    size_t block_start = content_len - (content_len % block_size);
    if (block_start == content_len) {
        block_start -= block_size;
    }
    return block_start;
}

static void validate_shifted_cache_layout(const ov::Tensor &cache, size_t block_size, const char *name) {
    const auto shape = cache.get_shape();
    if (shape.size() != 4) {
        throw std::runtime_error(std::string("OpenVINO shifted cold KV import requires rank-4 ") + name + " cache tensors");
    }
    if (shape[2] != block_size) {
        throw std::runtime_error(std::string("OpenVINO shifted cold KV import ") + name + " cache block axis does not match block manager");
    }
    if (shape[3] == 0) {
        throw std::runtime_error(std::string("OpenVINO shifted cold KV import ") + name + " cache head size is zero");
    }
    const auto elem_type = cache.get_element_type();
    if (elem_type != ov::element::f32 && elem_type != ov::element::f16 && elem_type != ov::element::bf16) {
        throw std::runtime_error("OpenVINO shifted cold KV import requires f32/f16/bf16 KV cache precision");
    }
}

static size_t shifted_cache_elem_size(const ov::Tensor &cache) {
    const auto elem_type = cache.get_element_type();
    if (elem_type == ov::element::f32 || elem_type == ov::element::f16 || elem_type == ov::element::bf16) {
        return elem_type.size();
    }
    throw std::runtime_error("OpenVINO shifted cold KV import requires f32/f16/bf16 KV cache precision");
}

static size_t shifted_cache_token_offset_bytes(const ov::Shape &shape,
                                               size_t elem_size,
                                               size_t head,
                                               size_t token_offset,
                                               size_t dim) {
    const size_t block_size = shape[2];
    const size_t head_size = shape[3];
    return ((head * block_size + token_offset) * head_size + dim) * elem_size;
}

template <typename T>
static float shifted_cache_read_scalar(const std::vector<uint8_t> &bytes, size_t off) {
    if (off > bytes.size() || bytes.size() - off < sizeof(T)) {
        throw std::runtime_error("OpenVINO shifted cold KV scalar read is outside cache block");
    }
    T value{};
    std::memcpy(&value, bytes.data() + off, sizeof(T));
    return static_cast<float>(value);
}

template <typename T>
static void shifted_cache_write_scalar(std::vector<uint8_t> &bytes, size_t off, float value) {
    if (off > bytes.size() || bytes.size() - off < sizeof(T)) {
        throw std::runtime_error("OpenVINO shifted cold KV scalar write is outside cache block");
    }
    T out = static_cast<T>(value);
    std::memcpy(bytes.data() + off, &out, sizeof(T));
}

static float shifted_cache_read_scalar(const std::vector<uint8_t> &bytes,
                                       size_t off,
                                       const ov::element::Type &elem_type) {
    if (elem_type == ov::element::f32) {
        return shifted_cache_read_scalar<float>(bytes, off);
    }
    if (elem_type == ov::element::f16) {
        return shifted_cache_read_scalar<ov::float16>(bytes, off);
    }
    if (elem_type == ov::element::bf16) {
        return shifted_cache_read_scalar<ov::bfloat16>(bytes, off);
    }
    throw std::runtime_error("OpenVINO shifted cold KV import requires f32/f16/bf16 KV cache precision");
}

static void shifted_cache_write_scalar(std::vector<uint8_t> &bytes,
                                       size_t off,
                                       const ov::element::Type &elem_type,
                                       float value) {
    if (elem_type == ov::element::f32) {
        shifted_cache_write_scalar<float>(bytes, off, value);
        return;
    }
    if (elem_type == ov::element::f16) {
        shifted_cache_write_scalar<ov::float16>(bytes, off, value);
        return;
    }
    if (elem_type == ov::element::bf16) {
        shifted_cache_write_scalar<ov::bfloat16>(bytes, off, value);
        return;
    }
    throw std::runtime_error("OpenVINO shifted cold KV import requires f32/f16/bf16 KV cache precision");
}

static void copy_shifted_cache_token(const std::vector<uint8_t> &src,
                                     std::vector<uint8_t> &dst,
                                     const ov::Shape &shape,
                                     size_t elem_size,
                                     size_t src_token_offset,
                                     size_t dst_token_offset) {
    const size_t heads = shape[1];
    const size_t block_size = shape[2];
    const size_t head_size = shape[3];
    if (src_token_offset >= block_size || dst_token_offset >= block_size) {
        throw std::runtime_error("OpenVINO shifted cold KV token offset is outside cache block");
    }
    const size_t chunk = head_size * elem_size;
    for (size_t head = 0; head < heads; ++head) {
        const size_t src_off = shifted_cache_token_offset_bytes(shape, elem_size, head, src_token_offset, 0);
        const size_t dst_off = shifted_cache_token_offset_bytes(shape, elem_size, head, dst_token_offset, 0);
        if (src_off > src.size() || src.size() - src_off < chunk ||
            dst_off > dst.size() || dst.size() - dst_off < chunk) {
            throw std::runtime_error("OpenVINO shifted cold KV token copy is outside cache block");
        }
        std::memcpy(dst.data() + dst_off, src.data() + src_off, chunk);
    }
}

static void rotate_shifted_key_token(std::vector<uint8_t> &bytes,
                                     const ov::Shape &shape,
                                     const ov::element::Type &elem_type,
                                     size_t elem_size,
                                     size_t token_offset,
                                     double rope_theta,
                                     int64_t position_delta) {
    if (position_delta == 0) {
        return;
    }
    const size_t heads = shape[1];
    const size_t block_size = shape[2];
    const size_t head_size = shape[3];
    if (token_offset >= block_size) {
        throw std::runtime_error("OpenVINO shifted cold KV key token offset is outside cache block");
    }
    if (head_size % 2 != 0) {
        throw std::runtime_error("OpenVINO shifted cold KV key head size must be even for RoPE rotation");
    }
    if (!std::isfinite(rope_theta) || rope_theta <= 0.0) {
        throw std::runtime_error("OpenVINO shifted cold KV import requires a positive RoPE theta");
    }
    const size_t half = head_size / 2;
    for (size_t head = 0; head < heads; ++head) {
        for (size_t dim = 0; dim < half; ++dim) {
            const double exponent = -static_cast<double>(2 * dim) / static_cast<double>(head_size);
            const double angle = static_cast<double>(position_delta) * std::pow(rope_theta, exponent);
            const float c = static_cast<float>(std::cos(angle));
            const float sn = static_cast<float>(std::sin(angle));
            const size_t x_off = shifted_cache_token_offset_bytes(shape, elem_size, head, token_offset, dim);
            const size_t y_off = shifted_cache_token_offset_bytes(shape, elem_size, head, token_offset, dim + half);
            const float x = shifted_cache_read_scalar(bytes, x_off, elem_type);
            const float y = shifted_cache_read_scalar(bytes, y_off, elem_type);
            shifted_cache_write_scalar(bytes, x_off, elem_type, x * c - y * sn);
            shifted_cache_write_scalar(bytes, y_off, elem_type, x * sn + y * c);
        }
    }
}

static void ensure_shifted_dest_block_initialized(ov::genai::BlockManager &block_manager,
                                                  const ov::genai::KVCacheManager &kv_manager,
                                                  size_t layer,
                                                  size_t layer_count,
                                                  size_t block_size,
                                                  size_t dest_hash,
                                                  size_t dest_block_start,
                                                  size_t dest_start,
                                                  const std::vector<int64_t> &dest_prefix_tokens,
                                                  std::set<std::tuple<size_t, size_t>> &initialized) {
    const auto key = std::make_tuple(layer, dest_hash);
    if (initialized.find(key) != initialized.end()) {
        return;
    }

    auto dest_blocks = ensure_import_blocks_locked(block_manager, dest_hash);
    const size_t dest_block_index = cache_block_index_for_layer(dest_blocks, layer, layer_count);
    auto key_cache = kv_manager.get_key_cache(layer);
    auto value_cache = kv_manager.get_value_cache(layer);
    std::vector<uint8_t> key_bytes(cache_block_stride_bytes(key_cache), 0);
    std::vector<uint8_t> value_bytes(cache_block_stride_bytes(value_cache), 0);

    if (dest_block_start < dest_start) {
        const size_t seed_content_len = dest_start;
        const size_t seed_hash = prefix_hash_for_tokens(dest_prefix_tokens, seed_content_len, block_size);
        auto seed_blocks = find_prefix_blocks_locked(block_manager, seed_hash);
        if (seed_blocks.empty()) {
            throw std::runtime_error("OpenVINO shifted cold KV import cannot find resident prefix block to seed destination");
        }
        const size_t seed_block_index = cache_block_index_for_layer(seed_blocks, layer, layer_count);
        key_bytes = read_cache_block_bytes(key_cache, seed_block_index);
        value_bytes = read_cache_block_bytes(value_cache, seed_block_index);
    }

    write_cache_block_bytes(key_cache, dest_block_index, key_bytes);
    write_cache_block_bytes(value_cache, dest_block_index, value_bytes);
    initialized.insert(key);
}

struct shifted_dest_block_bytes {
    size_t block_index = 0;
    std::vector<uint8_t> key;
    std::vector<uint8_t> value;
};

static std::vector<uint8_t> genai_export_cold_kv_locked(cx_genai_session *s,
                                                        size_t start,
                                                        size_t end,
                                                        const std::vector<int64_t> &prefix_tokens,
                                                        const std::string &token_hash) {
    auto orchestrator = genai_cache_orchestrator(s);
    if (!genai_cold_kv_supported_locked(s)) {
        throw std::runtime_error("OpenVINO GenAI cold KV export requires prefix-cached KV blocks");
    }
    orchestrator->allocate_cache_if_needed();

    auto &block_manager = orchestrator->get_block_manager(ov::genai::CacheType::KV_CACHE);
    const auto &kv_manager = genai_kv_cache_manager(*orchestrator);
    const size_t block_size = block_manager.get_block_size();
    const size_t layer_count = kv_manager.get_num_layers();
    const auto content_lengths = cold_content_lengths_for_range(start, end, prefix_tokens.size(), block_size);

    cold_kv_writer writer;
    writer.bytes(k_cold_kv_magic.data(), k_cold_kv_magic.size());
    writer.pod<uint64_t>(k_cold_kv_version);
    writer.pod<uint64_t>(start);
    writer.pod<uint64_t>(end);
    writer.pod<uint64_t>(block_size);
    writer.pod<uint64_t>(layer_count);
    writer.pod<uint64_t>(content_lengths.size());
    writer.string(token_hash);

    auto &block_private = genai_private(block_manager);
    std::lock_guard<std::mutex> lock(block_private.m_cached_blocks_map_mutex);
    for (size_t content_len : content_lengths) {
        const size_t hash = prefix_hash_for_tokens(prefix_tokens, content_len, block_size);
        auto blocks = find_prefix_blocks_locked(block_manager, hash);
        if (blocks.empty()) {
            throw std::runtime_error("OpenVINO GenAI prefix cache block is not resident for cold export");
        }
        writer.pod<uint64_t>(content_len);
        writer.pod<uint64_t>(static_cast<uint64_t>(hash));
        for (size_t layer = 0; layer < layer_count; ++layer) {
            const size_t block_index = cache_block_index_for_layer(blocks, layer, layer_count);
            auto key_bytes = read_cache_block_bytes(kv_manager.get_key_cache(layer), block_index);
            auto value_bytes = read_cache_block_bytes(kv_manager.get_value_cache(layer), block_index);
            writer.pod<uint64_t>(key_bytes.size());
            writer.bytes(key_bytes.data(), key_bytes.size());
            writer.pod<uint64_t>(value_bytes.size());
            writer.bytes(value_bytes.data(), value_bytes.size());
        }
    }
    return std::move(writer.data);
}

static void genai_import_cold_kv_locked(cx_genai_session *s,
                                        size_t start,
                                        size_t end,
                                        size_t dest_start,
                                        const std::vector<int64_t> &prefix_tokens,
                                        const std::string &token_hash,
                                        const uint8_t *data,
                                        size_t data_len) {
    if (!data || data_len == 0) {
        throw std::runtime_error("OpenVINO cold KV import payload is empty");
    }
    auto orchestrator = genai_cache_orchestrator(s);
    if (!genai_cold_kv_supported_locked(s)) {
        throw std::runtime_error("OpenVINO GenAI cold KV import requires prefix-cached KV blocks");
    }
    orchestrator->allocate_cache_if_needed();

    auto &block_manager = orchestrator->get_block_manager(ov::genai::CacheType::KV_CACHE);
    const auto &kv_manager = genai_kv_cache_manager(*orchestrator);
    cold_kv_reader reader(data, data_len);
    auto magic = reader.bytes(k_cold_kv_magic.size());
    if (!std::equal(magic.begin(), magic.end(), k_cold_kv_magic.begin())) {
        throw std::runtime_error("OpenVINO cold KV payload has an unknown format");
    }
    const auto version = reader.pod<uint64_t>();
    if (version != k_cold_kv_version) {
        throw std::runtime_error("OpenVINO cold KV payload version is unsupported");
    }
    const size_t payload_start = checked_size(reader.pod<uint64_t>(), "cold KV range start");
    const size_t payload_end = checked_size(reader.pod<uint64_t>(), "cold KV range end");
    const size_t payload_block_size = checked_size(reader.pod<uint64_t>(), "cold KV block size");
    const size_t payload_layers = checked_size(reader.pod<uint64_t>(), "cold KV layer count");
    const size_t payload_blocks = checked_size(reader.pod<uint64_t>(), "cold KV block count");
    const auto payload_hash = reader.string();

    if (payload_start != start || payload_end != end) {
        throw std::runtime_error("OpenVINO cold KV payload range does not match import range");
    }
    if (!token_hash.empty() && !payload_hash.empty() && token_hash != payload_hash) {
        throw std::runtime_error("OpenVINO cold KV payload token hash does not match import range");
    }
    if (payload_block_size != block_manager.get_block_size()) {
        throw std::runtime_error("OpenVINO cold KV payload block size does not match this model");
    }
    if (payload_layers != kv_manager.get_num_layers()) {
        throw std::runtime_error("OpenVINO cold KV payload layer count does not match this model");
    }
    if (payload_blocks == 0) {
        throw std::runtime_error("OpenVINO cold KV payload contains no KV blocks");
    }
    if (end <= start) {
        throw std::runtime_error("OpenVINO cold KV import range is empty");
    }
    const size_t token_count = end - start;
    if (dest_start > prefix_tokens.size() || token_count > prefix_tokens.size() - dest_start) {
        throw std::runtime_error("OpenVINO cold KV destination prefix tokens do not cover shifted import range");
    }

    auto &block_private = genai_private(block_manager);
    std::lock_guard<std::mutex> lock(block_private.m_cached_blocks_map_mutex);
    if (dest_start == start) {
        for (size_t block = 0; block < payload_blocks; ++block) {
            const size_t content_len = checked_size(reader.pod<uint64_t>(), "cold KV content length");
            const size_t payload_prefix_hash = checked_size(reader.pod<uint64_t>(), "cold KV prefix hash");
            const size_t expected_hash = prefix_hash_for_tokens(prefix_tokens, content_len, payload_block_size);
            if (payload_prefix_hash != expected_hash) {
                throw std::runtime_error("OpenVINO cold KV payload prefix hash does not match prefix tokens");
            }
            auto blocks = ensure_import_blocks_locked(block_manager, expected_hash);
            for (size_t layer = 0; layer < payload_layers; ++layer) {
                const auto key_len = checked_size(reader.pod<uint64_t>(), "cold KV key block");
                auto key_bytes = reader.bytes(key_len);
                const auto value_len = checked_size(reader.pod<uint64_t>(), "cold KV value block");
                auto value_bytes = reader.bytes(value_len);
                const size_t block_index = cache_block_index_for_layer(blocks, layer, payload_layers);
                write_cache_block_bytes(kv_manager.get_key_cache(layer), block_index, key_bytes);
                write_cache_block_bytes(kv_manager.get_value_cache(layer), block_index, value_bytes);
            }
        }
        reader.expect_finished();
        return;
    }

    const int64_t position_delta = static_cast<int64_t>(dest_start) - static_cast<int64_t>(start);
    if (position_delta != 0 && !s->rope_scaling_json.empty()) {
        throw std::runtime_error("OpenVINO shifted cold KV import does not support rope_scaling in model config");
    }
    std::set<std::tuple<size_t, size_t>> initialized_dest_blocks;
    for (size_t block = 0; block < payload_blocks; ++block) {
        const size_t content_len = checked_size(reader.pod<uint64_t>(), "cold KV content length");
        (void)checked_size(reader.pod<uint64_t>(), "cold KV prefix hash");
        const size_t source_block_start = cache_content_block_start(content_len, payload_block_size);
        if (content_len <= source_block_start) {
            throw std::runtime_error("OpenVINO cold KV payload content length maps to an empty source block");
        }
        const size_t source_begin = std::max(start, source_block_start);
        const size_t source_end = std::min(end, content_len);
        for (size_t layer = 0; layer < payload_layers; ++layer) {
            const auto key_len = checked_size(reader.pod<uint64_t>(), "cold KV key block");
            auto key_bytes = reader.bytes(key_len);
            const auto value_len = checked_size(reader.pod<uint64_t>(), "cold KV value block");
            auto value_bytes = reader.bytes(value_len);
            auto key_cache = kv_manager.get_key_cache(layer);
            auto value_cache = kv_manager.get_value_cache(layer);
            validate_shifted_cache_layout(key_cache, payload_block_size, "key");
            validate_shifted_cache_layout(value_cache, payload_block_size, "value");
            if (key_len != cache_block_stride_bytes(key_cache) ||
                value_len != cache_block_stride_bytes(value_cache)) {
                throw std::runtime_error("OpenVINO shifted cold KV payload block size does not match KV tensor");
            }
            const auto key_shape = key_cache.get_shape();
            const auto value_shape = value_cache.get_shape();
            if (key_shape != value_shape) {
                throw std::runtime_error("OpenVINO shifted cold KV key/value cache layouts do not match");
            }
            const size_t key_elem_size = shifted_cache_elem_size(key_cache);
            const size_t value_elem_size = shifted_cache_elem_size(value_cache);
            if (source_begin >= source_end) {
                continue;
            }
            std::map<size_t, shifted_dest_block_bytes> dest_cache_blocks;
            for (size_t source_pos = source_begin; source_pos < source_end; ++source_pos) {
                const size_t source_token_offset = source_pos - source_block_start;
                const size_t dest_pos = dest_start + (source_pos - start);
                const size_t dest_content_len = std::min(((dest_pos / payload_block_size) + 1) * payload_block_size,
                                                         prefix_tokens.size());
                const size_t dest_block_start = cache_content_block_start(dest_content_len, payload_block_size);
                if (dest_pos < dest_block_start || dest_pos >= dest_content_len) {
                    throw std::runtime_error("OpenVINO shifted cold KV destination token is outside destination block");
                }
                const size_t dest_token_offset = dest_pos - dest_block_start;
                const size_t dest_hash = prefix_hash_for_tokens(prefix_tokens, dest_content_len, payload_block_size);
                ensure_shifted_dest_block_initialized(block_manager,
                                                      kv_manager,
                                                      layer,
                                                      payload_layers,
                                                      payload_block_size,
                                                      dest_hash,
                                                      dest_block_start,
                                                      dest_start,
                                                      prefix_tokens,
                                                      initialized_dest_blocks);
                auto dest_it = dest_cache_blocks.find(dest_hash);
                if (dest_it == dest_cache_blocks.end()) {
                    auto dest_blocks = ensure_import_blocks_locked(block_manager, dest_hash);
                    const size_t dest_block_index = cache_block_index_for_layer(dest_blocks, layer, payload_layers);
                    shifted_dest_block_bytes entry;
                    entry.block_index = dest_block_index;
                    entry.key = read_cache_block_bytes(key_cache, dest_block_index);
                    entry.value = read_cache_block_bytes(value_cache, dest_block_index);
                    dest_it = dest_cache_blocks.emplace(dest_hash, std::move(entry)).first;
                }
                auto &dest_bytes = dest_it->second;
                copy_shifted_cache_token(key_bytes, dest_bytes.key, key_shape, key_elem_size, source_token_offset, dest_token_offset);
                copy_shifted_cache_token(value_bytes, dest_bytes.value, value_shape, value_elem_size, source_token_offset, dest_token_offset);
                rotate_shifted_key_token(dest_bytes.key,
                                         key_shape,
                                         key_cache.get_element_type(),
                                         key_elem_size,
                                         dest_token_offset,
                                         s->rope_theta,
                                         position_delta);
            }
            for (const auto &dest_entry : dest_cache_blocks) {
                write_cache_block_bytes(key_cache, dest_entry.second.block_index, dest_entry.second.key);
                write_cache_block_bytes(value_cache, dest_entry.second.block_index, dest_entry.second.value);
            }
        }
    }
    reader.expect_finished();
}

extern "C" {

cx_genai_session *cx_genai_session_new(const char *model_dir, const char *device,
                                       const cx_genai_session_config *config,
                                       char *err, size_t err_len) {
    try {
        std::string model_path(model_dir ? model_dir : "");
        std::string dev = (device && device[0]) ? std::string(device) : std::string("CPU");
        auto rope_config = load_model_rope_config(model_path);
        std::unique_ptr<cx_genai_session> s(new cx_genai_session());
        cx_genai_session_config cfg_copy{};
        if (config) {
            cfg_copy = *config;
        } else {
            cfg_copy.cache_size = 1;
            cfg_copy.dynamic_split_fuse = 1;
            cfg_copy.enable_prefix_caching = 1;
            cfg_copy.use_sparse_attention = 1;
            cfg_copy.num_last_dense_tokens_in_prefill = 10;
            cfg_copy.xattention_threshold = 0.9f;
            cfg_copy.xattention_block_size = 128;
            cfg_copy.xattention_stride = 16;
        }
        std::string kv_precision = (config && config->kv_cache_precision)
            ? std::string(config->kv_cache_precision)
            : std::string("f16");
        cfg_copy.kv_cache_precision = kv_precision.c_str();
        s->kv_cache_precision = kv_precision;
        s->rope_theta = rope_config.theta;
        s->rope_scaling_json = std::move(rope_config.scaling_json);

        // Copy the adapter specs out of the C struct now: the pointers it carries
        // are only guaranteed valid for this call, and the adapter files are loaded
        // on the worker thread below.
        std::vector<std::pair<std::string, float>> adapter_specs;
        if (config && config->lora_adapters) {
            for (size_t i = 0; i < config->lora_adapter_count; ++i) {
                const cx_genai_lora_adapter &a = config->lora_adapters[i];
                if (!a.path || !a.path[0]) {
                    write_buf(err, err_len, "OpenVINO GenAI LoRA adapter path is empty");
                    return nullptr;
                }
                adapter_specs.emplace_back(std::string(a.path), a.alpha);
            }
        }

        cx_genai_session *session = s.get();
        session->run([session, model_path, dev, cfg_copy, kv_precision, adapter_specs] {
            auto cfg = scheduler_config_from(&cfg_copy);
            ov::AnyMap properties{{"KV_CACHE_PRECISION", kv_precision_from(&cfg_copy)}};

            // Dynamic LoRA: register the adapter set on the pipeline (MODE_DYNAMIC
            // keeps A/B/alpha variable so adapters can be activated/scaled without
            // recompiling), then activate it in the pipeline's default generation
            // config so every request built from get_config() inherits it.
            if (!adapter_specs.empty()) {
                ov::genai::AdapterConfig adapter_config(ov::genai::AdapterConfig::MODE_DYNAMIC);
                for (const auto &spec : adapter_specs) {
                    adapter_config.add(ov::genai::Adapter(std::filesystem::path(spec.first)), spec.second);
                }
                session->adapters = adapter_config;
                properties.insert(ov::genai::adapters(adapter_config));
            }

            session->pipe = std::make_unique<ov::genai::ContinuousBatchingPipeline>(
                std::filesystem::path(model_path),
                cfg,
                dev,
                properties);

            if (session->adapters) {
                ov::genai::GenerationConfig base = session->pipe->get_config();
                base.adapters = session->adapters;
                session->pipe->set_config(base);
            }
        });
        return s.release();
    } catch (const std::exception &e) {
        write_buf(err, err_len, e.what());
        return nullptr;
    }
}

void cx_genai_session_free(cx_genai_session *s) {
    delete s;
}

int cx_genai_session_cancel(cx_genai_session *s) {
    if (!s) {
        return 1;
    }
    s->cancel_requested.store(true);
    return 0;
}

int cx_genai_apply_chat_template(cx_genai_session *s,
                                 const char **roles,
                                 const char **contents,
                                 const char **tool_calls,
                                 const char **tool_call_ids,
                                 size_t n,
                                 const char *tools_json,
                                 int add_generation_prompt,
                                 int enable_thinking,
                                 const char *reasoning_effort,
                                 char **out,
                                 size_t *out_len,
                                 char *err,
                                 size_t err_len) {
    clear_c_output(out, out_len);
    if (!s) {
        write_buf(err, err_len, "OpenVINO GenAI session is nil");
        return 1;
    }
    if (!out || !out_len) {
        write_buf(err, err_len, "OpenVINO GenAI chat template output pointer is nil");
        return 1;
    }
    try {
        std::string templated;
        std::string tools_str = (tools_json && tools_json[0]) ? std::string(tools_json) : std::string();
        s->run([&] {
            if (!s->pipe) {
                throw std::runtime_error("OpenVINO GenAI session is closed");
            }
            std::vector<ov::AnyMap> msgs;
            msgs.reserve(n);
            for (size_t i = 0; i < n; i++) {
                std::string role = (roles && roles[i]) ? std::string(roles[i]) : std::string();
                std::string content = (contents && contents[i]) ? std::string(contents[i]) : std::string();
                ov::AnyMap msg{{"role", role}, {"content", content}};
                if (tool_calls && tool_calls[i] && tool_calls[i][0] != '\0') {
                    msg["tool_calls"] = ov::genai::JsonContainer::from_json_string(std::string(tool_calls[i]));
                }
                if (tool_call_ids && tool_call_ids[i] && tool_call_ids[i][0] != '\0') {
                    msg["tool_call_id"] = std::string(tool_call_ids[i]);
                }
                msgs.push_back(msg);
            }
            ov::genai::ChatHistory history(msgs);
            std::optional<ov::genai::JsonContainer> tools;
            if (!tools_str.empty()) {
                tools = ov::genai::JsonContainer::from_json_string(tools_str);
            }
            // Thinking/effort controls travel as chat-template extra context:
            // templates that consume enable_thinking (qwen3) or
            // reasoning_effort (harmony) read them as top-level variables;
            // templates that don't simply ignore them, matching the llama
            // backend's chat-template kwargs semantics.
            std::optional<ov::genai::JsonContainer> extra_context;
            if (enable_thinking >= 0 || (reasoning_effort && reasoning_effort[0] != '\0')) {
                ov::AnyMap extra_map;
                if (enable_thinking >= 0) {
                    extra_map["enable_thinking"] = (enable_thinking != 0);
                }
                if (reasoning_effort && reasoning_effort[0] != '\0') {
                    extra_map["reasoning_effort"] = std::string(reasoning_effort);
                }
                extra_context = ov::genai::JsonContainer(extra_map);
            }
            templated = s->pipe->get_tokenizer().apply_chat_template(history, add_generation_prompt != 0, std::string{}, tools, extra_context);
        });
        set_c_output(templated, out, out_len, "chat template output");
        return 0;
    } catch (const std::exception &e) {
        free_c_output(out, out_len);
        write_buf(err, err_len, e.what());
        return 1;
    }
}

int cx_genai_tokenize(cx_genai_session *s,
                      const char *prompt,
                      int add_special_tokens,
                      int64_t *tokens,
                      size_t tokens_len,
                      size_t *tokens_out,
                      char *err,
                      size_t err_len) {
    if (!s) {
        write_buf(err, err_len, "OpenVINO GenAI session is nil");
        return 1;
    }
    if (!prompt) {
        write_buf(err, err_len, "OpenVINO GenAI prompt is nil");
        return 1;
    }
    try {
        std::vector<int64_t> ids;
        std::string prompt_text(prompt);
        s->run([&] {
            if (!s->pipe) {
                throw std::runtime_error("OpenVINO GenAI session is closed");
            }
            auto encoded = s->pipe->get_tokenizer().encode(
                prompt_text,
                ov::genai::add_special_tokens(add_special_tokens != 0));
            auto input_ids = encoded.input_ids;
            const auto n = input_ids.get_size();
            const int64_t *src = input_ids.data<const int64_t>();
            ids.assign(src, src + n);
        });
        if (tokens_out) {
            *tokens_out = ids.size();
        }
        if (ids.size() > tokens_len) {
            write_buf(err, err_len, "OpenVINO GenAI token buffer too small");
            return 2;
        }
        if (!ids.empty() && !tokens) {
            write_buf(err, err_len, "OpenVINO GenAI token buffer is nil");
            return 1;
        }
        for (size_t i = 0; i < ids.size(); i++) {
            tokens[i] = ids[i];
        }
        return 0;
    } catch (const std::exception &e) {
        write_buf(err, err_len, e.what());
        return 1;
    }
}

int cx_genai_supports_cold_kv(cx_genai_session *s) {
    if (!s) {
        return 0;
    }
    try {
        bool supported = false;
        s->run([&] {
            supported = genai_cold_kv_supported_locked(s);
        });
        return supported ? 1 : 0;
    } catch (...) {
        return 0;
    }
}

int cx_genai_export_cold_kv(cx_genai_session *s,
                            int start,
                            int end,
                            const int64_t *tokens,
                            size_t tokens_len,
                            const int64_t *prefix_tokens,
                            size_t prefix_tokens_len,
                            const char *token_hash,
                            uint8_t **out,
                            size_t *out_len,
                            char *err,
                            size_t err_len) {
    if (out) {
        *out = nullptr;
    }
    if (out_len) {
        *out_len = 0;
    }
    if (!s) {
        write_buf(err, err_len, "OpenVINO GenAI session is nil");
        return 1;
    }
    if (!out || !out_len) {
        write_buf(err, err_len, "OpenVINO cold KV export output pointer is nil");
        return 1;
    }
    if (start < 0 || end <= start || !tokens || tokens_len == 0 || !prefix_tokens || prefix_tokens_len == 0) {
        write_buf(err, err_len, "OpenVINO cold KV export range is empty");
        return 1;
    }
    try {
        std::vector<int64_t> token_copy(tokens, tokens + tokens_len);
        (void)token_copy;
        std::vector<int64_t> prefix_copy(prefix_tokens, prefix_tokens + prefix_tokens_len);
        std::string hash = (token_hash && token_hash[0]) ? std::string(token_hash) : std::string();
        std::vector<uint8_t> payload;
        s->run([&] {
            if (!s->pipe) {
                throw std::runtime_error("OpenVINO GenAI session is closed");
            }
            payload = genai_export_cold_kv_locked(
                s,
                static_cast<size_t>(start),
                static_cast<size_t>(end),
                prefix_copy,
                hash);
        });
        if (payload.empty()) {
            throw std::runtime_error("OpenVINO GenAI cold KV export returned no bytes");
        }
        void *buf = std::malloc(payload.size());
        if (!buf) {
            throw std::runtime_error("allocate OpenVINO cold KV export payload");
        }
        std::memcpy(buf, payload.data(), payload.size());
        *out = reinterpret_cast<uint8_t *>(buf);
        *out_len = payload.size();
        return 0;
    } catch (const std::exception &e) {
        write_buf(err, err_len, e.what());
        return 1;
    } catch (...) {
        write_buf(err, err_len, "unknown OpenVINO cold KV export error");
        return 1;
    }
}

int cx_genai_import_cold_kv(cx_genai_session *s,
                            int start,
                            int end,
                            int dest_start,
                            const int64_t *tokens,
                            size_t tokens_len,
                            const int64_t *prefix_tokens,
                            size_t prefix_tokens_len,
                            const char *token_hash,
                            const uint8_t *data,
                            size_t data_len,
                            char *err,
                            size_t err_len) {
    if (!s) {
        write_buf(err, err_len, "OpenVINO GenAI session is nil");
        return 1;
    }
    if (start < 0 || end <= start || !tokens || tokens_len == 0 || !prefix_tokens || prefix_tokens_len == 0 || !data || data_len == 0) {
        write_buf(err, err_len, "OpenVINO cold KV import range or payload is empty");
        return 1;
    }
    if (dest_start < 0) {
        write_buf(err, err_len, "OpenVINO cold KV import destination is negative");
        return 1;
    }
    if (tokens_len != static_cast<size_t>(end - start)) {
        write_buf(err, err_len, "OpenVINO cold KV import token count does not match range");
        return 1;
    }
    try {
        std::vector<int64_t> token_copy(tokens, tokens + tokens_len);
        (void)token_copy;
        std::vector<int64_t> prefix_copy(prefix_tokens, prefix_tokens + prefix_tokens_len);
        std::string hash = (token_hash && token_hash[0]) ? std::string(token_hash) : std::string();
        std::vector<uint8_t> payload(data, data + data_len);
        s->run([&] {
            if (!s->pipe) {
                throw std::runtime_error("OpenVINO GenAI session is closed");
            }
            genai_import_cold_kv_locked(
                s,
                static_cast<size_t>(start),
                static_cast<size_t>(end),
                static_cast<size_t>(dest_start),
                prefix_copy,
                hash,
                payload.data(),
                payload.size());
        });
        return 0;
    } catch (const std::exception &e) {
        write_buf(err, err_len, e.what());
        return 1;
    } catch (...) {
        write_buf(err, err_len, "unknown OpenVINO cold KV import error");
        return 1;
    }
}

void cx_genai_kv_data_free(void *p) {
    cx_genai_data_free(p);
}

void cx_genai_data_free(void *p) {
    std::free(p);
}

int cx_genai_generate(cx_genai_session *s,
                      const char *prompt,
                      size_t max_new_tokens,
                      float temperature,
                      int use_temperature,
                      float top_p,
                      int use_top_p,
                      size_t top_k,
                      int use_top_k,
                      size_t seed,
                      int use_seed,
                      const char *structured_protocol,
                      const char *structured_payload,
                      const char *parser_protocols,
                      char **out,
                      size_t *out_len,
                      char **parsed,
                      size_t *parsed_len,
                      cx_genai_metrics *metrics,
                      char *err,
                      size_t err_len) {
    clear_c_output(out, out_len);
    clear_c_output(parsed, parsed_len);
    if (!s) {
        write_buf(err, err_len, "OpenVINO GenAI session is nil");
        return 1;
    }
    if (!prompt) {
        write_buf(err, err_len, "OpenVINO GenAI prompt is nil");
        return 1;
    }
    if (!out || !out_len || !parsed || !parsed_len) {
        write_buf(err, err_len, "OpenVINO GenAI output pointer is nil");
        return 1;
    }

    try {
        std::string generated;
        std::string parsed_message;
        ov::genai::PipelineMetrics latest_metrics;
        std::string prompt_text(prompt);
        std::vector<std::string> protocols = split_protocols(parser_protocols);
        bool canceled = false;

        s->run([&] {
            if (!s->pipe) {
                throw std::runtime_error("OpenVINO GenAI session is closed");
            }
            s->cancel_requested.store(false);

            auto gen = generation_config_from(s->pipe->get_config(), max_new_tokens, temperature, use_temperature, top_p, use_top_p, top_k, use_top_k, seed, use_seed);
            apply_structured_output(gen, structured_protocol, structured_payload);
            auto parsers = parsers_for_protocols(protocols);
            gen.parsers = parsers;
            gen.validate();

            ov::genai::StreamerVariant streamer = std::function<ov::genai::StreamingStatus(std::string)>(
                [s](std::string) {
                    if (s->cancel_requested.load()) {
                        return ov::genai::StreamingStatus::CANCEL;
                    }
                    return ov::genai::StreamingStatus::RUNNING;
                });
            auto results = s->pipe->generate(
                std::vector<std::string>{prompt_text},
                std::vector<ov::genai::GenerationConfig>{gen},
                streamer);
            canceled = s->cancel_requested.load();
            if (results.empty() || results[0].m_generation_ids.empty()) {
                if (canceled) {
                    return;
                }
                throw std::runtime_error("OpenVINO GenAI returned no generation");
            }
            generated = results[0].m_generation_ids[0];
            parsed_message = parse_generated(parsers, generated);
            latest_metrics = s->pipe->get_metrics();
        });

        if (canceled) {
            return 3;
        }
        set_c_output(generated, out, out_len, "generated text");
        set_c_output(parsed_message, parsed, parsed_len, "parsed output");
        copy_metrics(latest_metrics, metrics);
        return 0;
    } catch (const std::exception &e) {
        free_c_output(out, out_len);
        free_c_output(parsed, parsed_len);
        write_buf(err, err_len, e.what());
        return 1;
    }
}

int cx_genai_prefill_tokens(cx_genai_session *s,
                            const int64_t *tokens,
                            size_t tokens_len,
                            cx_genai_metrics *metrics,
                            char *err,
                            size_t err_len) {
    if (!s) {
        write_buf(err, err_len, "OpenVINO GenAI session is nil");
        return 1;
    }
    if (!tokens || tokens_len == 0) {
        write_buf(err, err_len, "OpenVINO GenAI token prompt is empty");
        return 1;
    }

    try {
        ov::genai::PipelineMetrics latest_metrics;
        std::vector<int64_t> token_copy(tokens, tokens + tokens_len);

        s->run([&] {
            if (!s->pipe) {
                throw std::runtime_error("OpenVINO GenAI session is closed");
            }
            s->cancel_requested.store(false);
            auto gen = prefill_config_from(s->pipe->get_config());
            auto input = tensor_from_tokens(token_copy);
            s->pipe->generate(
                std::vector<ov::Tensor>{input},
                std::vector<ov::genai::GenerationConfig>{gen},
                std::monostate{});
            latest_metrics = s->pipe->get_metrics();
        });

        copy_metrics(latest_metrics, metrics);
        return 0;
    } catch (const std::exception &e) {
        write_buf(err, err_len, e.what());
        return 1;
    } catch (...) {
        write_buf(err, err_len, "unknown OpenVINO token prefill error");
        return 1;
    }
}

int cx_genai_generate_tokens(cx_genai_session *s,
                             const int64_t *tokens,
                             size_t tokens_len,
                             size_t max_new_tokens,
                             float temperature,
                             int use_temperature,
                             float top_p,
                             int use_top_p,
                             size_t top_k,
                             int use_top_k,
                             size_t seed,
                             int use_seed,
                             const char *structured_protocol,
                             const char *structured_payload,
                             const char *parser_protocols,
                             char **out,
                             size_t *out_len,
                             char **parsed,
                             size_t *parsed_len,
                             cx_genai_metrics *metrics,
                             char *err,
                             size_t err_len) {
    clear_c_output(out, out_len);
    clear_c_output(parsed, parsed_len);
    if (!s) {
        write_buf(err, err_len, "OpenVINO GenAI session is nil");
        return 1;
    }
    if (!tokens || tokens_len == 0) {
        write_buf(err, err_len, "OpenVINO GenAI token prompt is empty");
        return 1;
    }
    if (!out || !out_len || !parsed || !parsed_len) {
        write_buf(err, err_len, "OpenVINO GenAI token output pointer is nil");
        return 1;
    }

    try {
        std::string generated;
        std::string parsed_message;
        ov::genai::PipelineMetrics latest_metrics;
        std::vector<int64_t> token_copy(tokens, tokens + tokens_len);
        std::vector<std::string> protocols = split_protocols(parser_protocols);
        bool canceled = false;

        s->run([&] {
            if (!s->pipe) {
                throw std::runtime_error("OpenVINO GenAI session is closed");
            }
            s->cancel_requested.store(false);

            auto gen = generation_config_from(s->pipe->get_config(), max_new_tokens, temperature, use_temperature, top_p, use_top_p, top_k, use_top_k, seed, use_seed);
            apply_structured_output(gen, structured_protocol, structured_payload);
            auto parsers = parsers_for_protocols(protocols);
            gen.parsers = parsers;
            gen.validate();

            ov::genai::StreamerVariant streamer = std::function<ov::genai::StreamingStatus(std::string)>(
                [s](std::string) {
                    if (s->cancel_requested.load()) {
                        return ov::genai::StreamingStatus::CANCEL;
                    }
                    return ov::genai::StreamingStatus::RUNNING;
                });
            auto input = tensor_from_tokens(token_copy);
            auto results = s->pipe->generate(
                std::vector<ov::Tensor>{input},
                std::vector<ov::genai::GenerationConfig>{gen},
                streamer);
            canceled = s->cancel_requested.load();
            generated = decode_first_generation(s->pipe->get_tokenizer(), results, canceled);
            parsed_message = parse_generated(parsers, generated);
            latest_metrics = s->pipe->get_metrics();
        });

        if (canceled) {
            return 3;
        }
        set_c_output(generated, out, out_len, "generated token text");
        set_c_output(parsed_message, parsed, parsed_len, "parsed token output");
        copy_metrics(latest_metrics, metrics);
        return 0;
    } catch (const std::exception &e) {
        free_c_output(out, out_len);
        free_c_output(parsed, parsed_len);
        write_buf(err, err_len, e.what());
        return 1;
    } catch (...) {
        free_c_output(out, out_len);
        free_c_output(parsed, parsed_len);
        write_buf(err, err_len, "unknown OpenVINO token generation error");
        return 1;
    }
}

cx_genai_stream *cx_genai_stream_new(void) {
    try {
        return new cx_genai_stream();
    } catch (...) {
        return nullptr;
    }
}

void cx_genai_stream_free(cx_genai_stream *stream) {
    delete stream;
}

void cx_genai_stream_abort(cx_genai_stream *stream, const char *message) {
    if (!stream) return;
    stream->finish(1, message ? std::string(message) : std::string("OpenVINO GenAI stream aborted"));
}

int cx_genai_stream_next(cx_genai_stream *stream,
                         char **out,
                         size_t *out_len,
                         char **thinking,
                         size_t *thinking_len,
                         char *err,
                         size_t err_len) {
    clear_c_output(out, out_len);
    clear_c_output(thinking, thinking_len);
    if (!stream) {
        write_buf(err, err_len, "OpenVINO GenAI stream is nil");
        return 2;
    }
    if (!out || !out_len || !thinking || !thinking_len) {
        write_buf(err, err_len, "OpenVINO GenAI stream output pointer is nil");
        return 2;
    }

    std::unique_lock<std::mutex> lock(stream->mu);
    stream->cv.wait(lock, [stream] {
        return !stream->chunks.empty() || stream->done;
    });

    if (!stream->chunks.empty()) {
        cx_genai_stream_chunk chunk = std::move(stream->chunks.front());
        stream->chunks.pop_front();
        lock.unlock();
        try {
            set_c_output(chunk.text, out, out_len, "stream text");
            set_c_output(chunk.thinking, thinking, thinking_len, "stream thinking");
        } catch (const std::exception &e) {
            free_c_output(out, out_len);
            free_c_output(thinking, thinking_len);
            write_buf(err, err_len, e.what());
            return 2;
        }
        return 0;
    }

    int rc = stream->rc;
    std::string message = stream->error;
    lock.unlock();
    if (rc != 0) {
        write_buf(err, err_len, message);
        return rc;
    }
    return 1;
}

int cx_genai_generate_stream(cx_genai_session *s,
                             const char *prompt,
                             size_t max_new_tokens,
                             float temperature,
                             int use_temperature,
                             float top_p,
                             int use_top_p,
                             size_t top_k,
                             int use_top_k,
                             size_t seed,
                             int use_seed,
                             const char *parser_protocols,
                             cx_genai_stream *stream,
                             cx_genai_metrics *metrics,
                             char *err,
                             size_t err_len) {
    if (!s) {
        write_buf(err, err_len, "OpenVINO GenAI session is nil");
        if (stream) stream->finish(1, "OpenVINO GenAI session is nil");
        return 1;
    }
    if (!prompt) {
        write_buf(err, err_len, "OpenVINO GenAI prompt is nil");
        if (stream) stream->finish(1, "OpenVINO GenAI prompt is nil");
        return 1;
    }
    if (!stream) {
        write_buf(err, err_len, "OpenVINO GenAI stream is nil");
        return 1;
    }

    try {
        ov::genai::PipelineMetrics latest_metrics;
        std::string prompt_text(prompt);
        bool canceled = false;
        std::vector<std::string> protocols;
        if (parser_protocols && parser_protocols[0] != '\0') {
            std::stringstream ss(parser_protocols);
            std::string item;
            while (std::getline(ss, item, '\n')) {
                if (!item.empty()) {
                    protocols.push_back(item);
                }
            }
        }

        s->run([&] {
            if (!s->pipe) {
                throw std::runtime_error("OpenVINO GenAI session is closed");
            }
            s->cancel_requested.store(false);

            auto inc_parsers = incremental_parsers_for_protocols(protocols);

            auto gen = generation_config_from(s->pipe->get_config(), max_new_tokens, temperature, use_temperature, top_p, use_top_p, top_k, use_top_k, seed, use_seed);
            
            ov::genai::StreamerVariant streamer_variant = std::function<ov::genai::StreamingStatus(std::string)>(
                [s, stream, inc_parsers](std::string chunk) mutable {
                    ov::genai::JsonContainer delta_message;
                    for (auto &parser : inc_parsers) {
                        chunk = parser->parse(delta_message, chunk);
                    }
                    stream->push(chunk, json_string_field(delta_message, "reasoning_content"));
                    if (s->cancel_requested.load()) {
                        return ov::genai::StreamingStatus::CANCEL;
                    }
                    return ov::genai::StreamingStatus::RUNNING;
                });
            s->pipe->generate(
                std::vector<std::string>{prompt_text},
                std::vector<ov::genai::GenerationConfig>{gen},
                streamer_variant);
            canceled = s->cancel_requested.load();
            latest_metrics = s->pipe->get_metrics();
        });

        if (canceled) {
            stream->finish(3, "OpenVINO GenAI generation canceled");
            return 3;
        }
        copy_metrics(latest_metrics, metrics);
        stream->finish(0);
        return 0;
    } catch (const std::exception &e) {
        write_buf(err, err_len, e.what());
        stream->finish(1, e.what());
        return 1;
    }
}

int cx_genai_generate_tokens_stream(cx_genai_session *s,
                                    const int64_t *tokens,
                                    size_t tokens_len,
                                    size_t max_new_tokens,
                                    float temperature,
                                    int use_temperature,
                                    float top_p,
                                    int use_top_p,
                                    size_t top_k,
                                    int use_top_k,
                                    size_t seed,
                                    int use_seed,
                                    const char *parser_protocols,
                                    cx_genai_stream *stream,
                                    cx_genai_metrics *metrics,
                                    char *err,
                                    size_t err_len) {
    if (!s) {
        write_buf(err, err_len, "OpenVINO GenAI session is nil");
        if (stream) stream->finish(1, "OpenVINO GenAI session is nil");
        return 1;
    }
    if (!tokens || tokens_len == 0) {
        write_buf(err, err_len, "OpenVINO GenAI token prompt is empty");
        if (stream) stream->finish(1, "OpenVINO GenAI token prompt is empty");
        return 1;
    }
    if (!stream) {
        write_buf(err, err_len, "OpenVINO GenAI stream is nil");
        return 1;
    }

    try {
        ov::genai::PipelineMetrics latest_metrics;
        std::vector<int64_t> token_copy(tokens, tokens + tokens_len);
        bool canceled = false;
        std::vector<std::string> protocols = split_protocols(parser_protocols);

        s->run([&] {
            if (!s->pipe) {
                throw std::runtime_error("OpenVINO GenAI session is closed");
            }
            s->cancel_requested.store(false);

            auto inc_parsers = incremental_parsers_for_protocols(protocols);
            auto gen = generation_config_from(s->pipe->get_config(), max_new_tokens, temperature, use_temperature, top_p, use_top_p, top_k, use_top_k, seed, use_seed);

            ov::genai::StreamerVariant streamer_variant = std::function<ov::genai::StreamingStatus(std::string)>(
                [s, stream, inc_parsers](std::string chunk) mutable {
                    ov::genai::JsonContainer delta_message;
                    for (auto &parser : inc_parsers) {
                        chunk = parser->parse(delta_message, chunk);
                    }
                    stream->push(chunk, json_string_field(delta_message, "reasoning_content"));
                    if (s->cancel_requested.load()) {
                        return ov::genai::StreamingStatus::CANCEL;
                    }
                    return ov::genai::StreamingStatus::RUNNING;
                });
            auto input = tensor_from_tokens(token_copy);
            s->pipe->generate(
                std::vector<ov::Tensor>{input},
                std::vector<ov::genai::GenerationConfig>{gen},
                streamer_variant);
            canceled = s->cancel_requested.load();
            latest_metrics = s->pipe->get_metrics();
        });

        if (canceled) {
            stream->finish(3, "OpenVINO GenAI generation canceled");
            return 3;
        }
        copy_metrics(latest_metrics, metrics);
        stream->finish(0);
        return 0;
    } catch (const std::exception &e) {
        write_buf(err, err_len, e.what());
        stream->finish(1, e.what());
        return 1;
    } catch (...) {
        write_buf(err, err_len, "unknown OpenVINO token stream error");
        stream->finish(1, "unknown OpenVINO token stream error");
        return 1;
    }
}

}
