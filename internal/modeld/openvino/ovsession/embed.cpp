//go:build openvino && openvino_genai

#include "embed.h"

#include <cstring>
#include <exception>
#include <filesystem>
#include <memory>
#include <string>
#include <vector>

#include <openvino/openvino.hpp>
#include <openvino/genai/rag/text_embedding_pipeline.hpp>

static void write_buf(char *dst, size_t dst_len, const std::string &value) {
    if (!dst || dst_len == 0) return;
    std::strncpy(dst, value.c_str(), dst_len - 1);
    dst[dst_len - 1] = '\0';
}

struct cx_embed_session {
    std::unique_ptr<ov::genai::TextEmbeddingPipeline> pipe;
};

extern "C" {

cx_embed_session *cx_embed_session_new(const char *model_dir, const char *device, char *err, size_t err_len) {
    try {
        auto *s = new cx_embed_session();
        std::string model_path(model_dir ? model_dir : "");
        std::string dev = (device && device[0]) ? std::string(device) : std::string("CPU");
        s->pipe = std::make_unique<ov::genai::TextEmbeddingPipeline>(
            std::filesystem::path(model_path),
            dev
        );
        return s;
    } catch (const std::exception &e) {
        write_buf(err, err_len, e.what());
        return nullptr;
    }
}

void cx_embed_session_free(cx_embed_session *s) {
    delete s;
}

int cx_embed_embed(cx_embed_session *s, const char *prompt, float *out, size_t out_len, size_t *out_written, char *err, size_t err_len) {
    if (!s || !s->pipe) {
        write_buf(err, err_len, "OpenVINO Embed session is nil");
        return 1;
    }
    if (!prompt) {
        write_buf(err, err_len, "OpenVINO Embed prompt is nil");
        return 1;
    }

    try {
        std::string prompt_text(prompt);
        ov::genai::EmbeddingResult res = s->pipe->embed_query(prompt_text);
        
        const std::vector<float>& vec = std::get<std::vector<float>>(res);
        size_t n = vec.size();
        if (n > out_len) {
            write_buf(err, err_len, "OpenVINO Embed output buffer too small");
            return 2;
        }

        for (size_t i = 0; i < n; ++i) {
            out[i] = vec[i];
        }

        if (out_written) {
            *out_written = n;
        }

        return 0;
    } catch (const std::exception &e) {
        write_buf(err, err_len, e.what());
        return 1;
    }
}

}
