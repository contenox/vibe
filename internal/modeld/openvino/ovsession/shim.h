#ifndef CONTENOX_OV_SHIM_H
#define CONTENOX_OV_SHIM_H

#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

typedef struct cx_session cx_session;

cx_session *cx_session_new(const char *model_dir, const char *device,
                           char *err, size_t errlen);
void        cx_session_free(cx_session *s);

int cx_prefill(cx_session *s, const int64_t *tokens, size_t n,
               char *err, size_t errlen);
int64_t cx_decode_next(cx_session *s, char *err, size_t errlen);

int cx_snapshot_save(cx_session *s, const char *path, char *err, size_t errlen);
int cx_snapshot_restore(cx_session *s, const char *path, char *err, size_t errlen);
int cx_snapshot_data(cx_session *s, uint8_t **out, size_t *out_len, char *err, size_t errlen);
void cx_snapshot_data_free(void *p);
int cx_snapshot_restore_data(cx_session *s, const uint8_t *data, size_t data_len,
                             char *err, size_t errlen);

#ifdef __cplusplus
}
#endif

#endif
