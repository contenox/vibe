//go:build openvino && openvino_genai

#include "vlm.h"

#include <atomic>
#include <condition_variable>
#include <cstdint>
#include <cstdlib>
#include <cstring>
#include <deque>
#include <exception>
#include <filesystem>
#include <functional>
#include <memory>
#include <mutex>
#include <stdexcept>
#include <string>
#include <thread>
#include <utility>
#include <vector>

#include <openvino/openvino.hpp>

#include <openvino/genai/chat_history.hpp>
#include <openvino/genai/generation_config.hpp>
#include <openvino/genai/streamer_base.hpp>
#include <openvino/genai/tokenizer.hpp>
#include <openvino/genai/visual_language/pipeline.hpp>

// stb_image is the pinned single-header decoder GenAI's own C VLM sample uses
// (samples/c/visual_language_chat/load_image.c). It turns the caller's encoded
// PNG/JPEG/BMP bytes into the uint8 RGB [1,H,W,3] tensors VLMPipeline expects.
#define STB_IMAGE_IMPLEMENTATION
#define STBI_NO_STDIO
#include "stb_image.h"

// The vlm cell is a deliberately public-surface-only wrapper around
// ov::genai::VLMPipeline. Unlike the text cell (genai.cpp), which reaches
// ContinuousBatchingPipeline internals for prefix-cache warm-up and cold-KV
// import/export, VLMPipeline hides its impl behind a private pimpl with no
// accessible hook — so this cell has NO prefix-cache reuse and NO cold-KV in
// v1: every generation re-prefills the full multimodal prompt. The session
// adapter (modeld/openvino/visionsession.go) documents the same limitation at
// the transport boundary.

static void write_buf(char *dst, size_t dst_len, const std::string &value) {
    if (!dst || dst_len == 0) return;
    std::strncpy(dst, value.c_str(), dst_len - 1);
    dst[dst_len - 1] = '\0';
}

static char *alloc_c_string(const std::string &value, const char *what) {
    void *buf = std::malloc(value.size() + 1);
    if (!buf) {
        throw std::runtime_error(std::string("allocate OpenVINO VLM ") + what);
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
        throw std::runtime_error(std::string("OpenVINO VLM ") + what + " output pointer is nil");
    }
    char *buf = alloc_c_string(value, what);
    *out = buf;
    *out_len = value.size();
}

// decode_image_rgb decodes encoded image bytes into the uint8 RGB [1,H,W,3]
// NHWC tensor layout VLMPipeline documents for its image inputs.
static ov::Tensor decode_image_rgb(const uint8_t *data, size_t len) {
    if (!data || len == 0) {
        throw std::runtime_error("OpenVINO VLM image bytes are empty");
    }
    int width = 0, height = 0, channels = 0;
    unsigned char *pixels = stbi_load_from_memory(data, static_cast<int>(len), &width, &height, &channels, 3);
    if (!pixels) {
        const char *reason = stbi_failure_reason();
        throw std::runtime_error(std::string("OpenVINO VLM image decode: ") + (reason ? reason : "unrecognized image format"));
    }
    ov::Tensor tensor(ov::element::u8, ov::Shape{1, static_cast<size_t>(height), static_cast<size_t>(width), 3});
    std::memcpy(tensor.data<uint8_t>(), pixels, static_cast<size_t>(width) * static_cast<size_t>(height) * 3);
    stbi_image_free(pixels);
    return tensor;
}

struct cx_vlm_session {
    std::unique_ptr<ov::genai::VLMPipeline> pipe;
    // chat_template is the model's ORIGINAL chat template, captured before the
    // pipeline's tokenizer template is overridden with the identity template.
    // cx_vlm_apply_chat_template renders with it explicitly.
    std::string chat_template;
    std::atomic<bool> cancel_requested{false};

    // Single worker thread owning all pipeline calls, mirroring the text cell
    // (genai.cpp): OpenVINO pipelines are not thread-safe and some device
    // plugins bind state to the constructing thread.
    std::mutex mu;
    std::condition_variable cv;
    bool stopping = false;
    bool busy = false;
    bool has_task = false;
    bool done = false;
    std::function<void()> task;
    std::exception_ptr task_error;
    std::thread worker;

    cx_vlm_session() : worker([this] { loop(); }) {}

    ~cx_vlm_session() {
        shutdown();
    }

    void run(std::function<void()> fn) {
        std::unique_lock<std::mutex> lock(mu);
        cv.wait(lock, [this] { return (!busy && !has_task) || stopping; });
        if (stopping) {
            throw std::runtime_error("OpenVINO VLM session is closing");
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

struct cx_vlm_stream {
    std::mutex mu;
    std::condition_variable cv;
    std::deque<std::string> chunks;
    bool done = false;
    int rc = 0;
    std::string error;

    void push(const std::string &text) {
        if (text.empty()) return;
        std::lock_guard<std::mutex> lock(mu);
        chunks.push_back(text);
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

// generation_config_from seeds from the pipeline's own defaults (eos ids,
// sampling defaults from generation_config.json) and overrides only what the
// caller specifies — the same policy as the text cell. apply_chat_template
// stays TRUE here, unlike the text cell: with the pipeline's template
// overridden to the identity template, the "apply" is a no-op wrap of our
// already-templated prompt, but it keeps the embedder encoding with
// add_special_tokens=false (GenAI's convention for templated text), so the
// BOS supplied by the model's real template is not doubled by the tokenizer.
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
    // Some HF generation_config.json files carry a small total-token max_length;
    // clear the inherited total cap so the explicit prompt-exclusive budget
    // governs generation (mirrors the text cell).
    gen.max_length = SIZE_MAX;
    gen.apply_chat_template = true;
    if (use_temperature) {
        gen.temperature = temperature;
        gen.do_sample = temperature > 0.0f;
    }
    if (use_top_p) {
        gen.top_p = top_p;
    }
    if (use_top_k) {
        gen.top_k = top_k;
    }
    if (use_seed) {
        gen.rng_seed = seed;
    }
    return gen;
}

extern "C" {

int cx_vlm_image_probe(const uint8_t *data, size_t len,
                       int *width, int *height,
                       char *err, size_t err_len) {
    try {
        if (!data || len == 0) {
            throw std::runtime_error("OpenVINO VLM image bytes are empty");
        }
        int w = 0, h = 0, c = 0;
        if (!stbi_info_from_memory(data, static_cast<int>(len), &w, &h, &c)) {
            const char *reason = stbi_failure_reason();
            throw std::runtime_error(std::string("OpenVINO VLM image decode: ") + (reason ? reason : "unrecognized image format"));
        }
        if (width) *width = w;
        if (height) *height = h;
        return 0;
    } catch (const std::exception &e) {
        write_buf(err, err_len, e.what());
        return 1;
    }
}

cx_vlm_session *cx_vlm_session_new(const char *model_dir, const char *device,
                                   char *err, size_t err_len) {
    try {
        std::string model_path(model_dir ? model_dir : "");
        if (model_path.empty()) {
            write_buf(err, err_len, "OpenVINO VLM model directory is required");
            return nullptr;
        }
        std::string dev = (device && device[0]) ? std::string(device) : std::string("CPU");

        std::unique_ptr<cx_vlm_session> s(new cx_vlm_session());
        cx_vlm_session *session = s.get();
        session->run([session, model_path, dev] {
            // Pin the stateful SDPA implementation. The default PagedAttention
            // path wraps a ContinuousBatchingPipeline behind an adapter whose
            // set_chat_template() throws ("Chat mode is not supported."),
            // breaking the identity-template contract below — and GenAI itself
            // already forces the SDPA impl for model families it marks
            // requires_sdpa (gemma3/gemma4). Pinning SDPA gives one uniform
            // backend for every VLM family; this cell is single-stream and
            // latency-oriented, which is what the stateful impl serves.
            session->pipe = std::make_unique<ov::genai::VLMPipeline>(
                std::filesystem::path(model_path),
                dev,
                ov::AnyMap{{"ATTENTION_BACKEND", std::string("SDPA")}});
            // Capture the model's own chat template, then override the
            // pipeline's template with the identity template (the escape hatch
            // the VLMPipeline header documents for deactivating templating).
            // The session adapter templates the FULL multi-turn conversation
            // itself via cx_vlm_apply_chat_template (single-user-message
            // wrapping inside the pipeline would mangle roles), and the
            // identity template makes the pipeline's own "apply" pass the
            // rendered prompt through unchanged — while keeping GenAI's
            // add_special_tokens=false encoding convention for templated text.
            ov::genai::Tokenizer tok = session->pipe->get_tokenizer();
            session->chat_template = tok.get_chat_template();
            if (session->chat_template.empty()) {
                // Without a model chat template the session cannot render
                // multi-turn conversations correctly; refuse instead of
                // degrading to concatenated text.
                throw std::runtime_error("OpenVINO VLM model has no chat template; cannot serve chat sessions");
            }
            session->pipe->set_chat_template("{% for message in messages %}{{ message['content'] }}{% endfor %}");
        });
        return s.release();
    } catch (const std::exception &e) {
        write_buf(err, err_len, e.what());
        return nullptr;
    }
}

void cx_vlm_session_free(cx_vlm_session *s) {
    delete s;
}

int cx_vlm_session_cancel(cx_vlm_session *s) {
    if (!s) {
        return 1;
    }
    s->cancel_requested.store(true);
    return 0;
}

int cx_vlm_apply_chat_template(cx_vlm_session *s,
                               const char **roles,
                               const char **contents,
                               size_t n,
                               int add_generation_prompt,
                               char **out,
                               size_t *out_len,
                               char *err,
                               size_t err_len) {
    clear_c_output(out, out_len);
    if (!s) {
        write_buf(err, err_len, "OpenVINO VLM session is nil");
        return 1;
    }
    if (!out || !out_len) {
        write_buf(err, err_len, "OpenVINO VLM chat template output pointer is nil");
        return 1;
    }
    try {
        std::string templated;
        s->run([&] {
            if (!s->pipe) {
                throw std::runtime_error("OpenVINO VLM session is closed");
            }
            std::vector<ov::AnyMap> msgs;
            msgs.reserve(n);
            for (size_t i = 0; i < n; i++) {
                std::string role = (roles && roles[i]) ? std::string(roles[i]) : std::string();
                std::string content = (contents && contents[i]) ? std::string(contents[i]) : std::string();
                msgs.push_back(ov::AnyMap{{"role", role}, {"content", content}});
            }
            ov::genai::ChatHistory history(msgs);
            // Pass the captured original template explicitly: the pipeline's
            // (shared-impl) tokenizer now carries the identity template.
            templated = s->pipe->get_tokenizer().apply_chat_template(history, add_generation_prompt != 0, s->chat_template);
        });
        set_c_output(templated, out, out_len, "chat template output");
        return 0;
    } catch (const std::exception &e) {
        free_c_output(out, out_len);
        write_buf(err, err_len, e.what());
        return 1;
    }
}

int cx_vlm_tokenize(cx_vlm_session *s,
                    const char *prompt,
                    int add_special_tokens,
                    int64_t *tokens,
                    size_t tokens_len,
                    size_t *tokens_out,
                    char *err,
                    size_t err_len) {
    if (!s) {
        write_buf(err, err_len, "OpenVINO VLM session is nil");
        return 1;
    }
    if (!prompt) {
        write_buf(err, err_len, "OpenVINO VLM prompt is nil");
        return 1;
    }
    try {
        std::vector<int64_t> ids;
        std::string prompt_text(prompt);
        s->run([&] {
            if (!s->pipe) {
                throw std::runtime_error("OpenVINO VLM session is closed");
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
            write_buf(err, err_len, "OpenVINO VLM token buffer too small");
            return 2;
        }
        if (!ids.empty() && !tokens) {
            write_buf(err, err_len, "OpenVINO VLM token buffer is nil");
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

cx_vlm_stream *cx_vlm_stream_new(void) {
    try {
        return new cx_vlm_stream();
    } catch (...) {
        return nullptr;
    }
}

void cx_vlm_stream_free(cx_vlm_stream *stream) {
    delete stream;
}

void cx_vlm_stream_abort(cx_vlm_stream *stream, const char *message) {
    if (!stream) return;
    stream->finish(1, message ? std::string(message) : std::string("OpenVINO VLM stream aborted"));
}

int cx_vlm_stream_next(cx_vlm_stream *stream,
                       char **out,
                       size_t *out_len,
                       char *err,
                       size_t err_len) {
    clear_c_output(out, out_len);
    if (!stream) {
        write_buf(err, err_len, "OpenVINO VLM stream is nil");
        return 2;
    }
    if (!out || !out_len) {
        write_buf(err, err_len, "OpenVINO VLM stream output pointer is nil");
        return 2;
    }
    std::unique_lock<std::mutex> lock(stream->mu);
    stream->cv.wait(lock, [stream] {
        return !stream->chunks.empty() || stream->done;
    });

    if (!stream->chunks.empty()) {
        std::string chunk = std::move(stream->chunks.front());
        stream->chunks.pop_front();
        lock.unlock();
        try {
            set_c_output(chunk, out, out_len, "stream text");
        } catch (const std::exception &e) {
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
    }
    return rc == 0 ? 1 : rc;
}

int cx_vlm_generate_stream(cx_vlm_session *s,
                           const char *prompt,
                           const cx_vlm_image *images,
                           size_t image_count,
                           size_t max_new_tokens,
                           float temperature,
                           int use_temperature,
                           float top_p,
                           int use_top_p,
                           size_t top_k,
                           int use_top_k,
                           size_t seed,
                           int use_seed,
                           cx_vlm_stream *stream,
                           char *err,
                           size_t err_len) {
    if (!s) {
        write_buf(err, err_len, "OpenVINO VLM session is nil");
        if (stream) stream->finish(1, "OpenVINO VLM session is nil");
        return 1;
    }
    if (!prompt) {
        write_buf(err, err_len, "OpenVINO VLM prompt is nil");
        if (stream) stream->finish(1, "OpenVINO VLM prompt is nil");
        return 1;
    }
    if (!stream) {
        write_buf(err, err_len, "OpenVINO VLM stream is nil");
        return 1;
    }

    try {
        std::string prompt_text(prompt);
        // Decode outside the worker: stb needs no pipeline, and a bad
        // attachment should fail before any inference work is queued.
        std::vector<ov::Tensor> tensors;
        tensors.reserve(image_count);
        for (size_t i = 0; i < image_count; i++) {
            if (!images) {
                throw std::runtime_error("OpenVINO VLM image array is nil");
            }
            tensors.push_back(decode_image_rgb(images[i].data, images[i].len));
        }

        bool canceled = false;
        s->run([&] {
            if (!s->pipe) {
                throw std::runtime_error("OpenVINO VLM session is closed");
            }
            s->cancel_requested.store(false);

            auto gen = generation_config_from(s->pipe->get_generation_config(), max_new_tokens,
                                              temperature, use_temperature, top_p, use_top_p,
                                              top_k, use_top_k, seed, use_seed);

            ov::genai::StreamerVariant streamer = std::function<ov::genai::StreamingStatus(std::string)>(
                [s, stream](std::string chunk) {
                    stream->push(chunk);
                    if (s->cancel_requested.load()) {
                        return ov::genai::StreamingStatus::CANCEL;
                    }
                    return ov::genai::StreamingStatus::RUNNING;
                });
            s->pipe->generate(prompt_text, tensors, gen, streamer);
            canceled = s->cancel_requested.load();
        });

        if (canceled) {
            stream->finish(3, "OpenVINO VLM generation canceled");
            return 3;
        }
        stream->finish(0);
        return 0;
    } catch (const std::exception &e) {
        write_buf(err, err_len, e.what());
        stream->finish(1, e.what());
        return 1;
    }
}

}
