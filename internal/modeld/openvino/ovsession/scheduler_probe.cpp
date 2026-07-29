//go:build openvino && openvino_genai

#include "scheduler_probe.h"

#include <cstring>
#include <filesystem>
#include <sstream>
#include <string>
#include <vector>

#include <openvino/openvino.hpp>
#include <openvino/genai/continuous_batching_pipeline.hpp>
#include <openvino/genai/generation_config.hpp>
#include <openvino/genai/scheduler_config.hpp>
#include <openvino/genai/sparse_attention.hpp>

static void write_buf(char *dst, size_t dst_len, const std::string &value) {
    if (!dst || dst_len == 0) return;
    std::strncpy(dst, value.c_str(), dst_len - 1);
    dst[dst_len - 1] = '\0';
}

extern "C" {

int cx_genai_scheduler_probe(const char *model_dir, const char *device,
                             char *report, size_t report_len,
                             char *err, size_t err_len) {
    try {
        ov::genai::SchedulerConfig cfg;
        cfg.cache_size = 1;
        cfg.dynamic_split_fuse = true;
        cfg.enable_prefix_caching = true;
        cfg.use_sparse_attention = true;
        cfg.sparse_attention_config.mode = ov::genai::SparseAttentionMode::XATTENTION;
        cfg.sparse_attention_config.num_last_dense_tokens_in_prefill = 10;
        cfg.sparse_attention_config.xattention_threshold = 0.9f;
        cfg.sparse_attention_config.xattention_block_size = 128;
        cfg.sparse_attention_config.xattention_stride = 16;
        cfg.validate();

        ov::AnyMap properties{{"KV_CACHE_PRECISION", ov::element::f16}};
        // This probe is invoked from a short-lived test subprocess. Keep the
        // pipeline process-owned because destroying ContinuousBatchingPipeline
        // from the cgo callback path segfaults in the pinned GenAI runtime.
        auto *pipe = new ov::genai::ContinuousBatchingPipeline(
            std::filesystem::path(model_dir),
            cfg,
            std::string(device && device[0] ? device : "CPU"),
            properties);

        ov::genai::GenerationConfig gen;
        gen.max_new_tokens = 1;
        gen.apply_chat_template = false;

        auto results = pipe->generate(
            std::vector<std::string>{"def add(a, b):"},
            std::vector<ov::genai::GenerationConfig>{gen});
        auto metrics = pipe->get_metrics();

        std::ostringstream out;
        out << cfg.to_string() << "\n";
        out << "PipelineMetrics {\n";
        out << "  requests: " << metrics.requests << "\n";
        out << "  scheduled_requests: " << metrics.scheduled_requests << "\n";
        out << "  cache_usage: " << metrics.cache_usage << "\n";
        out << "  max_cache_usage: " << metrics.max_cache_usage << "\n";
        out << "  avg_cache_usage: " << metrics.avg_cache_usage << "\n";
        out << "  cache_size_in_bytes: " << metrics.cache_size_in_bytes << "\n";
        out << " }\n";
        out << "GenerationResultCount: " << results.size() << "\n";
        write_buf(report, report_len, out.str());
        return 0;
    } catch (const std::exception &e) {
        write_buf(err, err_len, e.what());
        return 1;
    }
}

}
