/*
 * host.c -- the host_api_v1_t Stacks is given on Push.
 *
 * Built from the vendored header, never redeclared, so the layout cannot
 * drift from the one stacks.c was compiled against (plugin_api_v1.h's own
 * static assert guards the offsets). Only the three callbacks Stacks calls
 * are set; every other field is NULL, which Stacks checks before use.
 */
#include <string.h>

#include "host/plugin_api_v1.h"
#include "stacks_push.h"
#include "_cgo_export.h"

plugin_api_v2_t *move_plugin_init_v2(const host_api_v1_t *host);

static host_api_v1_t g_push_host;
static plugin_api_v2_t *g_api;

static void h_log(const char *msg) { goStacksLog((char *)msg); }
static float h_bpm(void) { return goStacksBPM(); }
static int h_inject(const uint8_t *msg, int len) { return goStacksInject((uint8_t *)msg, len); }

int sp_init(int sample_rate, int frames_per_block) {
    memset(&g_push_host, 0, sizeof g_push_host);
    g_push_host.api_version = MOVE_PLUGIN_API_VERSION;
    g_push_host.sample_rate = sample_rate;
    g_push_host.frames_per_block = frames_per_block;
    g_push_host.log = h_log;
    g_push_host.get_bpm = h_bpm;
    g_push_host.midi_inject_to_move = h_inject;
    g_api = move_plugin_init_v2(&g_push_host);
    return g_api != NULL && g_api->api_version == MOVE_PLUGIN_API_VERSION_2;
}

void *sp_create(const char *module_dir, const char *config_json) {
    return g_api->create_instance(module_dir, config_json);
}

void sp_destroy(void *inst) { g_api->destroy_instance(inst); }

void sp_on_midi(void *inst, const uint8_t *msg, int len, int source) {
    g_api->on_midi(inst, msg, len, source);
}

void sp_set_param(void *inst, const char *key, const char *val) {
    g_api->set_param(inst, key, val);
}

int sp_get_param(void *inst, const char *key, char *buf, int buf_len) {
    return g_api->get_param(inst, key, buf, buf_len);
}

void sp_render(void *inst, int16_t *out_lr, int frames) {
    g_api->render_block(inst, out_lr, frames);
}
