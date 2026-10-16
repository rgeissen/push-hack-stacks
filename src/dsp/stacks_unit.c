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
