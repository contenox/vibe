#ifndef CONTENOX_OV_SCHEDULER_PROBE_H
#define CONTENOX_OV_SCHEDULER_PROBE_H

#include <stddef.h>

#ifdef __cplusplus
extern "C" {
#endif

int cx_genai_scheduler_probe(const char *model_dir, const char *device,
                             char *report, size_t report_len,
                             char *err, size_t err_len);

#ifdef __cplusplus
}
#endif

#endif
