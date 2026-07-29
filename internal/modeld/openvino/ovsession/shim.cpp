//go:build openvino && openvino_legacy_shim

#include "shim.h"

#include <cstring>
#include <cstdlib>
#include <fstream>
#include <sstream>
#include <stdexcept>
#include <string>

#include <openvino/openvino.hpp>

static void set_err(char *err, size_t errlen, const std::string &m) {
    if (!err || errlen == 0) return;
    std::strncpy(err, m.c_str(), errlen - 1);
    err[errlen - 1] = '\0';
}

template <typename T> static void wr(std::ostream &o, const T &v) {
    o.write(reinterpret_cast<const char *>(&v), sizeof(T));
}

template <typename T> static void rd(std::istream &i, T &v) {
    i.read(reinterpret_cast<char *>(&v), sizeof(T));
}

struct cx_session {
    ov::Core core;
    ov::CompiledModel compiled;
    ov::InferRequest req;
    int64_t cursor = 0;
    int64_t pending = -1;
};

static std::string model_xml_path(const char *model_dir) {
    std::string base = std::string(model_dir);
    const char *candidates[] = {"openvino_model.xml", "openvino_language_model.xml"};
    for (const char *name : candidates) {
        std::string path = base + "/" + name;
        std::ifstream f(path);
        if (f.good()) {
            return path;
        }
    }
    return base + "/openvino_model.xml";
}

static int64_t run_infer(cx_session *s, const int64_t *ids, size_t n, int64_t past) {
    size_t total = static_cast<size_t>(past) + n;

    ov::Tensor input_ids(ov::element::i64, ov::Shape{1, n});
    std::memcpy(input_ids.data(), ids, n * sizeof(int64_t));

    ov::Tensor attn(ov::element::i64, ov::Shape{1, total});
    auto *attn_data = attn.data<int64_t>();
    for (size_t i = 0; i < total; i++) attn_data[i] = 1;

    ov::Tensor pos(ov::element::i64, ov::Shape{1, n});
    auto *pos_data = pos.data<int64_t>();
    for (size_t i = 0; i < n; i++) pos_data[i] = past + static_cast<int64_t>(i);

    ov::Tensor beam(ov::element::i32, ov::Shape{1});
    beam.data<int32_t>()[0] = 0;

    s->req.set_tensor("input_ids", input_ids);
    s->req.set_tensor("attention_mask", attn);
    s->req.set_tensor("position_ids", pos);
    s->req.set_tensor("beam_idx", beam);
    s->req.infer();

    ov::Tensor logits = s->req.get_tensor("logits");
    ov::Shape shape = logits.get_shape();
    size_t seq = shape[1], vocab = shape[2];
    const float *row = logits.data<float>() + (seq - 1) * vocab;

    int64_t best = 0;
    float best_value = row[0];
    for (size_t i = 1; i < vocab; i++) {
        if (row[i] > best_value) {
            best_value = row[i];
            best = static_cast<int64_t>(i);
        }
    }
    return best;
}

static void write_snapshot(cx_session *s, std::ostream &out) {
    wr(out, s->cursor);
    wr(out, s->pending);
    auto states = s->req.query_state();
    int32_t state_count = static_cast<int32_t>(states.size());
    wr(out, state_count);

    for (auto &state : states) {
        ov::Tensor tensor = state.get_state();
        ov::Shape shape = tensor.get_shape();
        int32_t rank = static_cast<int32_t>(shape.size());
        wr(out, rank);
        for (size_t dim : shape) {
            int64_t stored_dim = static_cast<int64_t>(dim);
            wr(out, stored_dim);
        }
        int64_t bytes = static_cast<int64_t>(tensor.get_byte_size());
        wr(out, bytes);
        out.write(static_cast<const char *>(tensor.data()), bytes);
    }
    if (!out) {
        throw std::runtime_error("snapshot write failed");
    }
}

static void read_snapshot(cx_session *s, std::istream &in) {
    rd(in, s->cursor);
    rd(in, s->pending);
    int32_t state_count;
    rd(in, state_count);
    if (!in) {
        throw std::runtime_error("snapshot header is truncated");
    }

    s->req.reset_state();
    auto states = s->req.query_state();
    if (static_cast<int32_t>(states.size()) != state_count) {
        throw std::runtime_error("state count mismatch");
    }

    for (int32_t i = 0; i < state_count; i++) {
        int32_t rank;
        rd(in, rank);
        if (!in || rank < 0) {
            throw std::runtime_error("snapshot tensor rank is invalid");
        }
        ov::Shape shape(static_cast<size_t>(rank));
        for (int32_t j = 0; j < rank; j++) {
            int64_t dim;
            rd(in, dim);
            if (!in || dim < 0) {
                throw std::runtime_error("snapshot tensor shape is invalid");
            }
            shape[static_cast<size_t>(j)] = static_cast<size_t>(dim);
        }

        int64_t bytes;
        rd(in, bytes);
        // Dynamically query the element type expected by this state to avoid
        // hardcoding f32, which might break on different devices or plugins.
        ov::element::Type el_type = states[static_cast<size_t>(i)].get_state().get_element_type();
        ov::Tensor tensor(el_type, shape);
        if (static_cast<int64_t>(tensor.get_byte_size()) != bytes) {
            throw std::runtime_error("state byte size mismatch");
        }
        in.read(static_cast<char *>(tensor.data()), bytes);
        if (!in) {
            throw std::runtime_error("snapshot tensor data is truncated");
        }
        states[static_cast<size_t>(i)].set_state(tensor);
    }
}

extern "C" {

cx_session *cx_session_new(const char *model_dir, const char *device, char *err, size_t errlen) {
    try {
        auto *s = new cx_session();
        std::string xml = model_xml_path(model_dir);
        ov::AnyMap cfg{{"KV_CACHE_PRECISION", ov::element::f16}};
        s->compiled = s->core.compile_model(xml, device, cfg);
        s->req = s->compiled.create_infer_request();
        return s;
    } catch (const std::exception &e) {
        set_err(err, errlen, e.what());
        return nullptr;
    }
}

void cx_session_free(cx_session *s) { delete s; }

int cx_prefill(cx_session *s, const int64_t *tokens, size_t n, char *err, size_t errlen) {
    try {
        s->req.reset_state();
        s->pending = run_infer(s, tokens, n, 0);
        s->cursor = static_cast<int64_t>(n);
        return 0;
    } catch (const std::exception &e) {
        set_err(err, errlen, e.what());
        return 1;
    }
}

int64_t cx_decode_next(cx_session *s, char *err, size_t errlen) {
    try {
        int64_t emit = s->pending;
        s->pending = run_infer(s, &emit, 1, s->cursor);
        s->cursor += 1;
        return emit;
    } catch (const std::exception &e) {
        set_err(err, errlen, e.what());
        return -1;
    }
}

int cx_snapshot_save(cx_session *s, const char *path, char *err, size_t errlen) {
    try {
        std::ofstream out(path, std::ios::binary);
        if (!out) throw std::runtime_error("cannot open snapshot for write");
        write_snapshot(s, out);
        return 0;
    } catch (const std::exception &e) {
        set_err(err, errlen, e.what());
        return 1;
    }
}

int cx_snapshot_restore(cx_session *s, const char *path, char *err, size_t errlen) {
    try {
        std::ifstream in(path, std::ios::binary);
        if (!in) throw std::runtime_error("cannot open snapshot for read");
        read_snapshot(s, in);
        return 0;
    } catch (const std::exception &e) {
        set_err(err, errlen, e.what());
        return 1;
    }
}

int cx_snapshot_data(cx_session *s, uint8_t **out, size_t *out_len, char *err, size_t errlen) {
    try {
        if (!out || !out_len) {
            throw std::runtime_error("snapshot output pointer is null");
        }
        std::ostringstream ss(std::ios::out | std::ios::binary);
        write_snapshot(s, ss);
        const std::string data = ss.str();
        void *buf = std::malloc(data.size());
        if (!buf && !data.empty()) {
            throw std::runtime_error("cannot allocate snapshot data");
        }
        if (!data.empty()) {
            std::memcpy(buf, data.data(), data.size());
        }
        *out = static_cast<uint8_t *>(buf);
        *out_len = data.size();
        return 0;
    } catch (const std::exception &e) {
        set_err(err, errlen, e.what());
        return 1;
    }
}

void cx_snapshot_data_free(void *p) {
    std::free(p);
}

int cx_snapshot_restore_data(cx_session *s, const uint8_t *data, size_t data_len, char *err, size_t errlen) {
    try {
        if (!data && data_len != 0) {
            throw std::runtime_error("snapshot data pointer is null");
        }
        std::string raw;
        if (data_len > 0) {
            raw.assign(reinterpret_cast<const char *>(data), data_len);
        }
        std::istringstream in(raw, std::ios::in | std::ios::binary);
        read_snapshot(s, in);
        return 0;
    } catch (const std::exception &e) {
        set_err(err, errlen, e.what());
        return 1;
    }
}

}
