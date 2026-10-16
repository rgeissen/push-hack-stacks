/* stacks_push.h -- C side of the Go bridge (host.c). Go cannot call a C
 * function pointer, so every plugin_api_v2_t entry gets a plain wrapper. */
#ifndef STACKS_PUSH_H
#define STACKS_PUSH_H

#include <stdint.h>

int   sp_init(int sample_rate, int frames_per_block);
void *sp_create(const char *module_dir, const char *config_json);
void  sp_destroy(void *inst);
void  sp_on_midi(void *inst, const uint8_t *msg, int len, int source);
void  sp_set_param(void *inst, const char *key, const char *val);
int   sp_get_param(void *inst, const char *key, char *buf, int buf_len);
void  sp_render(void *inst, int16_t *out_lr, int frames);

#endif
