/*
 * stacks_unit.c -- the vendored Stacks DSP (upstream/stacks/src/dsp/stacks.c,
 * pinned in upstream/stacks/UPSTREAM_REF) built UNCHANGED for the Push host.
 * Everything Push-specific sits in front of the #include, never inside it.
 */

/* Read Clip parses Move's Song.abl. Push has none, so point it at a folder
 * that never exists: a read reports "failed", not a wrong import. */
#define SETS_DIR "/data/push-hack/data/stacks/no-sets"

/* move_info is Schwung's view of Move's set (selected track, tempo, key).
 * Push has no such source yet, so report "unknown" and Read Track stays a
 * knob -- the same path Stacks takes on Schwung before 1.6. */
#define MOVE_INFO_NO_READER
#include "host/move_info.h"
static int move_info_read(move_info_t *out) {
    (void)out;
    return 0;
}

#include "dsp/stacks.c"

/* ======================================================================
 * Push Hack Harmony (sync): what the sequencer will play, read by the Go
 * host on the engine thread. After the #include, so stacks.c stays the
 * vendored file unchanged; built from its own helpers, the same walk tick
 * does, so the two cannot drift without the Go test noticing
 * (TestSeqAtPredictsTheCore).
 * ====================================================================== */

/* sp_seq_at: the progression chord the sequence sounds `ahead` clocks from
 * now (0-based), or -1 when the sequence is not running (stopped, Run off,
 * parked) or empty. A chord waiting out its start offset leaves the one
 * before it sounding, so that is the answer there. *pulse gets the clock
 * count now, -1 while waiting for the downbeat. */
int sp_seq_at(void *inst, int ahead, int *pulse) {
    stk_t *st = (stk_t *)inst;
    int now = st->at_downbeat ? -1 : st->pulse;
    if (pulse) *pulse = now;
    if (!st->clock_running || !st->run || st->prog.count <= 0) return -1;
    int p = now + ahead;
    if (p < 0) p = 0;
    int unit = UNIT_CLOCKS;
    if (unit < 1) unit = 1;
    int total_u = prog_total_units(&st->prog);
    int clip_u = clampi(st->bars, 1, 16) * bar_units_for(&st->prog);
    if (clip_u < 1) clip_u = total_u;
    int total_c = clip_u * unit;
    int pos_u = (p % total_c) / unit;
    if (total_u > 0) pos_u %= total_u;
    int idx = 0, acc = 0;
    for (; idx < st->prog.count; idx++) {
        int l = st->prog.ch[idx].len;
        if (l < 1) l = 1;
        if (pos_u < acc + l) break;
        acc += l;
    }
    if (idx >= st->prog.count) { idx = st->prog.count - 1; acc = chord_slot_start(&st->prog, idx); }
    if (pos_u < acc + st->prog.ch[idx].off)   /* not started: the one before rings */
        idx = idx > 0 ? idx - 1 : st->prog.count - 1;
    return idx;
}

/* sp_cur: the chord the core has armed now (a held pad, an audition or the
 * sequence), 0-based, or -1. */
int sp_cur(void *inst) {
    stk_t *st = (stk_t *)inst;
    return st->armed_id != 0 ? st->cur_step : -1;
}
