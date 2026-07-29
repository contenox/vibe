#ifndef CONTENOX_OV_VLM_H
#define CONTENOX_OV_VLM_H

#include <stdint.h>
#include <stddef.h>

#ifdef __cplusplus
extern "C" {
#endif

typedef struct cx_vlm_session cx_vlm_session;
typedef struct cx_vlm_stream cx_vlm_stream;

/* One encoded image file (PNG/JPEG/BMP/... bytes as attached by the caller).
   The cell decodes it with stb_image into the uint8 RGB [1,H,W,C] tensor
   ov::genai::VLMPipeline expects. mime is advisory; the decoder sniffs the
   actual format from the bytes. */
typedef struct cx_vlm_image {
    const uint8_t *data;
    size_t len;
    const char *mime;
} cx_vlm_image;

/* Decode-probe an image without a model: returns 0 and fills width/height on
   success, non-zero with err on undecodable bytes. Lets the Go side (and unit
   tests) validate attachments before paying for pipeline work. */
int cx_vlm_image_probe(const uint8_t *data, size_t len,
                       int *width, int *height,
                       char *err, size_t err_len);

cx_vlm_session *cx_vlm_session_new(const char *model_dir, const char *device,
                                   char *err, size_t err_len);
void cx_vlm_session_free(cx_vlm_session *s);
int cx_vlm_session_cancel(cx_vlm_session *s);

/* Render role/content turns with the MODEL's original chat template (captured
   before the pipeline's template is overridden with the identity template; see
   vlm.cpp for why generation still runs with apply_chat_template enabled). */
int cx_vlm_apply_chat_template(cx_vlm_session *s,
                               const char **roles,
                               const char **contents,
                               size_t n,
                               int add_generation_prompt,
                               char **out,
                               size_t *out_len,
                               char *err,
                               size_t err_len);

int cx_vlm_tokenize(cx_vlm_session *s,
                    const char *prompt,
                    int add_special_tokens,
                    int64_t *tokens,
                    size_t tokens_len,
                    size_t *tokens_out,
                    char *err,
                    size_t err_len);

cx_vlm_stream *cx_vlm_stream_new(void);
void cx_vlm_stream_free(cx_vlm_stream *stream);
void cx_vlm_stream_abort(cx_vlm_stream *stream, const char *message);
int cx_vlm_stream_next(cx_vlm_stream *stream,
                       char **out,
                       size_t *out_len,
                       char *err,
                       size_t err_len);

/* Generate over an already-templated prompt (universal <ov_genai_image_i>
   tags mark image positions) plus the decoded images, streaming text deltas
   into `stream`. Returns 0 on success, 3 on cancel, non-zero otherwise. */
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
                           size_t err_len);

#ifdef __cplusplus
}
#endif

#endif
