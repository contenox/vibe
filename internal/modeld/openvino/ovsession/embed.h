#ifndef CONTENOX_OV_EMBED_H
#define CONTENOX_OV_EMBED_H

#include <stddef.h>

#ifdef __cplusplus
extern "C" {
#endif

typedef struct cx_embed_session cx_embed_session;

cx_embed_session *cx_embed_session_new(const char *model_dir, const char *device, char *err, size_t err_len);
void cx_embed_session_free(cx_embed_session *s);
int cx_embed_embed(cx_embed_session *s, const char *prompt, float *out, size_t out_len, size_t *out_written, char *err, size_t err_len);

#ifdef __cplusplus
}
#endif

#endif
