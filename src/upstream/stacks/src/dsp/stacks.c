/*
 * Stacks -- chord progression sequencer for the Schwung chain.
 *
 * A port of Ableton Live 12's Stacks MIDI generator to Move's hardware. Live's
 * Stacks is OFFLINE: it writes a chord progression into a clip. Move has no
 * clip a MIDI FX can write to, so this plays the same data model in real time
 * into the slot's sound generator, and offers the round trip explicitly:
 *
 *     Read Clip  -- parse the playing clip out of the current set's Song.abl
 *                   and rebuild the progression from its chords
 *     edit       -- root / shape / inversion / length / offset per chord,
 *                   drawn as staff notation by canvas.js
 *     Preview    -- audition the selected chord, or loop the whole progression,
 *                   without the transport running
 *     Stamp Clip -- put it back, either by injecting into a record-armed Move
 *                   track or by writing Song.abl directly
 *
 * ---------------------------------------------------------------------------
 * REALTIME
 *
 * Every entry point below IS the SPI callback (see docs/REALTIME_SAFETY.md
 * rule 4). process_midi, tick, set_param and get_param must not allocate, must
 * not touch a file and must not take a lock a non-RT thread can hold.
 *
 * Reading Song.abl therefore does NOT happen in set_param. It happens on a
 * worker thread created once in create_instance and parked on a 100 ms poll,
 * so the RT side only ever sets an atomic request counter. The worker parses
 * into `staging` and publishes with a release-store to `staging_seq`; tick()
 * picks it up with an acquire-load and copies 8 small structs. No pointer
 * changes hands, so there is no lifetime for the RT side to get wrong.
 *
 * The worker is created with PTHREAD_EXPLICIT_SCHED + SCHED_OTHER. It has to
 * be: a thread created from an entry point inherits the SPI callback's FIFO 70,
 * and Move's own Link Main runs at FIFO 35, so an inheriting worker starves it
 * (docs/plans/2026-08-22-rt-thread-audit-findings.md).
 */

#include <stdlib.h>
#include <string.h>
#include <stdio.h>
#include <math.h>
#include <stdint.h>
#include <stdatomic.h>
#include <pthread.h>
#include <unistd.h>
#include <dirent.h>
#include <strings.h>
#include <sys/stat.h>

#include "host/midi_fx_api_v1.h"
#include "host/plugin_api_v1.h"
#include "host/move_info.h"     /* Move's selected track; Schwung >= 1.6.0 */

#define STK_STATE_MAX      16384   /* get_param("state") is ~1.5KB today; the map grows with params */
#define STK_STATE_DIR      "/data/UserData/schwung/stacks"
#define STK_STATE_FILE     STK_STATE_DIR "/state.json"
#define STK_SAVE_QUIET_MS  1500    /* a jog spin is one save, not one per detent */

static int  stk_get_param(void *instance, const char *key, char *buf, int buf_len);
static void stk_set_param_inner(void *instance, const char *key, const char *val);

/*
 * SIXTEEN, because DUPLICATE has to be able to double what is there. At eight
 * a four-chord phrase was the largest that could be doubled, which made the
 * gesture useless on exactly the progressions worth extending. Sixteen is also
 * the natural ceiling for the thing being written: sixteen bars.
 */
#define STK_UNDO_DEPTH   24
#define STK_MAX_CHORDS   16
/*
 * SIXTEEN PATTERN SLOTS, because that is how many buttons there are.
 *
 * A pattern is the BUFFER -- the progression, its clip length and its voice
 * grouping -- and emphatically not a clip: nothing here touches Song.abl, so
 * storing costs nothing and cannot be overwritten by Move. At ~16 chords a
 * slot that is a few KB for all sixteen, against a 128KB param value, and the
 * module already carries 24 undo snapshots of the same shape.
 */
#define STK_PATTERNS     16
#define STK_MAX_TONES    6
/* Repeat stamps the whole chord up to 4 times, so the schedule is that many
 * times the voice count. Sizing this to STK_MAX_TONES silently dropped every
 * note after the sixth -- a repeat of 4 on a triad lost the last two passes
 * and looked like the feature simply not working. */
/*
 * A rhythm is up to 16 hits per step, and a chord may run two steps, so the
 * schedule is far larger than the "repeat N times" it replaces. Sizing this to
 * the tone count silently dropped every hit after the sixth.
 */
#define RHYTHM_STEPS     16
#define STK_MAX_PEND     96
#ifndef SETS_DIR         /* overridable so a test can point Read Clip at a fixture */
#define SETS_DIR         "/data/UserData/UserLibrary/Sets"
#endif
#define CLOCKS_PER_QUARTER 24

static const host_api_v1_t *g_host = NULL;

static void stk_log(const char *msg) {
    if (g_host && g_host->log) g_host->log(msg);
}

/* ======================================================================
 * Chord shapes
 *
 * Interval sets from the root, in semitones. The NAME is what the staff
 * prints above each chord, so it is a display suffix and not the enum
 * option: the option list is what the knob shows.
 *
 * "note" is last on purpose. Appending keeps every existing index stable,
 * and a saved state is a set of indices.
 * ====================================================================== */
typedef struct {
    const char *opt;      /* enum option, as the knob shows it */
    const char *suffix;   /* printed after the root name on the staff */
    int         fam;      /* stk_fam_t -- must be non-decreasing in SHAPES[] */
    int         n;
    int         iv[STK_MAX_TONES];
} stk_shape_t;

/*
 * Ordered BY FAMILY, contiguously, and that ordering is load-bearing.
 *
 * Thirty shapes on one enum knob is a knob nobody can use -- it is four full
 * turns from "maj" to a ninth. Grouping them into runs makes `family` a coarse
 * jump straight to the right run and `shape` a fine step of three-to-ten
 * inside it, with no dynamic option list: the contract stays static, so the
 * knob grid needs no re-read and no cache invalidation when the family moves.
 *
 * `fam` must be non-decreasing down this table. A shape filed out of order
 * splits its family into two runs and silently breaks the jump;
 * tests/host/test_stacks_shapes.sh fails on exactly that.
 */
typedef enum { FAM_5TH = 0, FAM_TRIAD, FAM_6TH, FAM_7TH, FAM_9TH, FAM_EXT } stk_fam_t;
static const char *const FAMILIES[] = { "5th", "triad", "6th", "7th", "9th", "ext" };
#define NUM_FAMILIES ((int)(sizeof(FAMILIES) / sizeof(FAMILIES[0])))

static const stk_shape_t SHAPES[] = {
    /* --- intervals ------------------------------------------------------ */
    /*
     * REST IS A CHORD SHAPE, not a mute.
     *
     * A rest is something you WRITE -- a bar of silence is part of the
     * progression and moves with it when you insert or delete. Mute is
     * something you DO to a chord you are keeping, and it remembers what it
     * was muting. Modelling a rest as a muted chord would make "what note is
     * this" a question with an answer nobody wanted, and would lose the
     * silence the moment the mute was cleared.
     *
     * It has zero tones, so every path that walks the interval list naturally
     * produces nothing and no site needs to special-case it.
     */
    { "rest",   "REST",   FAM_5TH,   0, { 0 } },
    { "note",   "",       FAM_5TH,   1, { 0 } },
    { "5",      "5",      FAM_5TH,   2, { 0, 7 } },
    { "oct",    "oct",    FAM_5TH,   2, { 0, 12 } },
    /* --- triads --------------------------------------------------------- */
    { "maj",    "",       FAM_TRIAD, 3, { 0, 4, 7 } },
    { "min",    "m",      FAM_TRIAD, 3, { 0, 3, 7 } },
    { "dim",    "dim",    FAM_TRIAD, 3, { 0, 3, 6 } },
    { "aug",    "aug",    FAM_TRIAD, 3, { 0, 4, 8 } },
    { "sus2",   "sus2",   FAM_TRIAD, 3, { 0, 2, 7 } },
    { "sus4",   "sus4",   FAM_TRIAD, 3, { 0, 5, 7 } },
    /* --- sixths --------------------------------------------------------- */
    { "6",      "6",      FAM_6TH,   4, { 0, 4, 7, 9 } },
    { "m6",     "m6",     FAM_6TH,   4, { 0, 3, 7, 9 } },
    { "69",     "6/9",    FAM_6TH,   5, { 0, 4, 7, 9, 14 } },
    /* --- sevenths ------------------------------------------------------- */
    { "maj7",   "maj7",   FAM_7TH,   4, { 0, 4, 7, 11 } },
    { "min7",   "m7",     FAM_7TH,   4, { 0, 3, 7, 10 } },
    { "dom7",   "7",      FAM_7TH,   4, { 0, 4, 7, 10 } },
    { "m7b5",   "m7b5",   FAM_7TH,   4, { 0, 3, 6, 10 } },
    { "dim7",   "dim7",   FAM_7TH,   4, { 0, 3, 6, 9 } },
    { "mMaj7",  "mMaj7",  FAM_7TH,   4, { 0, 3, 7, 11 } },
    { "7sus4",  "7sus4",  FAM_7TH,   4, { 0, 5, 7, 10 } },
    { "7sus2",  "7sus2",  FAM_7TH,   4, { 0, 2, 7, 10 } },
    { "aug7",   "+7",     FAM_7TH,   4, { 0, 4, 8, 10 } },
    { "maj7#5", "maj7#5", FAM_7TH,   4, { 0, 4, 8, 11 } },
    { "7b5",    "7b5",    FAM_7TH,   4, { 0, 4, 6, 10 } },
    /* --- ninths --------------------------------------------------------- */
    { "add9",   "add9",   FAM_9TH,   4, { 0, 4, 7, 14 } },
    { "madd9",  "madd9",  FAM_9TH,   4, { 0, 3, 7, 14 } },
    { "maj9",   "maj9",   FAM_9TH,   5, { 0, 4, 7, 11, 14 } },
    { "min9",   "m9",     FAM_9TH,   5, { 0, 3, 7, 10, 14 } },
    { "9",      "9",      FAM_9TH,   5, { 0, 4, 7, 10, 14 } },
    { "7b9",    "7b9",    FAM_9TH,   5, { 0, 4, 7, 10, 13 } },
    { "7#9",    "7#9",    FAM_9TH,   5, { 0, 4, 7, 10, 15 } },
    { "m9b5",   "m9b5",   FAM_9TH,   5, { 0, 3, 6, 10, 14 } },
    /* --- elevenths and thirteenths -------------------------------------- */
    { "11",     "11",     FAM_EXT,   5, { 0, 7, 10, 14, 17 } },
    { "m11",    "m11",    FAM_EXT,   6, { 0, 3, 7, 10, 14, 17 } },
    { "maj11",  "maj11",  FAM_EXT,   6, { 0, 4, 7, 11, 14, 17 } },
    { "13",     "13",     FAM_EXT,   6, { 0, 4, 7, 10, 14, 21 } },
    { "maj13",  "maj13",  FAM_EXT,   6, { 0, 4, 7, 11, 14, 21 } },
    { "m13",    "m13",    FAM_EXT,   6, { 0, 3, 7, 10, 14, 21 } },
};
#define SHAPE_REST   0    /* silence, and it draws as an empty slot */
#define SHAPE_TRIAD_DEFAULT shape_by_opt("maj")
#define SHAPE_SINGLE 1    /* "note" -- the import's fallback for a cluster it
                           * cannot name. */
#define NUM_SHAPES ((int)(sizeof(SHAPES) / sizeof(SHAPES[0])))

static const char *NOTE_NAMES[12] = {
    "C", "C#", "D", "D#", "E", "F", "F#", "G", "G#", "A", "A#", "B"
};

/* ======================================================================
 * Scales -- pitch-class masks, bit n set = semitone n is in the scale.
 * Chromatic is all twelve, so snapping is a no-op and costs no branch.
 * ====================================================================== */
typedef struct { const char *name; uint16_t mask; } stk_scale_t;

static const stk_scale_t SCALES[] = {
    { "Chromatic",  0x0FFF },
    { "Major",      0x0AB5 },  /* 0 2 4 5 7 9 11 */
    { "Minor",      0x05AD },  /* 0 2 3 5 7 8 10 */
    { "Dorian",     0x06AD },  /* 0 2 3 5 7 9 10 */
    { "Phrygian",   0x05AB },  /* 0 1 3 5 7 8 10 */
    { "Lydian",     0x0AD5 },  /* 0 2 4 6 7 9 11 */
    { "Mixolydian", 0x06B5 },  /* 0 2 4 5 7 9 10 */
    { "Locrian",    0x056B },  /* 0 1 3 5 6 8 10 */
    { "HarmMinor",  0x09AD },  /* 0 2 3 5 7 8 11 */
    { "MelMinor",   0x0AAD },  /* 0 2 3 5 7 9 11 */
    { "PentMaj",    0x0295 },  /* 0 2 4 7 9 */
    { "PentMin",    0x04A9 },  /* 0 3 5 7 10 */
    { "Blues",      0x04E9 },  /* 0 3 5 6 7 10 */
};
#define NUM_SCALES ((int)(sizeof(SCALES) / sizeof(SCALES[0])))

/* Move writes its set scale as a name in Song.abl; map it onto ours. */
static int scale_index_from_name(const char *name) {
    if (!name) return 0;
    for (int i = 0; i < NUM_SCALES; i++)
        if (strcasecmp(name, SCALES[i].name) == 0) return i;
    /* Move spells a few of them differently. */
    if (strcasecmp(name, "NaturalMinor") == 0) return 2;
    if (strcasecmp(name, "Aeolian") == 0) return 2;
    if (strcasecmp(name, "Ionian") == 0) return 1;
    if (strcasecmp(name, "MajorPentatonic") == 0) return 10;
    if (strcasecmp(name, "MinorPentatonic") == 0) return 11;
    return 0;
}

/* ---- BEGIN GENERATED chain_params (tools/gen_chain_params.py) ---- */
static const char CHAIN_PARAMS_JSON[] =
    "[{\"key\":\"sel\",\"name\":\"Chord\",\"short_name\":\"Chord\",\"type\":\"int\",\"min\":1"
    ",\"max\":16,\"default\":1,\"step\":1},{\"key\":\"root\",\"name\":\"Root\",\"short_name\""
    ":\"Root\",\"options_as_string\":true,\"type\":\"enum\",\"options\":[\"C1\",\"C#1\",\"D1"
    "\",\"D#1\",\"E1\",\"F1\",\"F#1\",\"G1\",\"G#1\",\"A1\",\"A#1\",\"B1\",\"C2\",\"C#2\",\"D"
    "2\",\"D#2\",\"E2\",\"F2\",\"F#2\",\"G2\",\"G#2\",\"A2\",\"A#2\",\"B2\",\"C3\",\"C#3\",\""
    "D3\",\"D#3\",\"E3\",\"F3\",\"F#3\",\"G3\",\"G#3\",\"A3\",\"A#3\",\"B3\",\"C4\",\"C#4\","
    "\"D4\",\"D#4\",\"E4\",\"F4\",\"F#4\",\"G4\",\"G#4\",\"A4\",\"A#4\",\"B4\",\"C5\",\"C#5\""
    ",\"D5\",\"D#5\",\"E5\",\"F5\",\"F#5\",\"G5\",\"G#5\",\"A5\",\"A#5\",\"B5\"],\"default\":"
    "36},{\"key\":\"degree\",\"name\":\"Scale Degree\",\"short_name\":\"Deg\",\"options_as_st"
    "ring\":true,\"type\":\"enum\",\"access\":\"write\",\"options\":[\"I\",\"II\",\"III\",\"I"
    "V\",\"V\",\"VI\",\"VII\",\"rest\"],\"default\":0},{\"key\":\"coct\",\"name\":\"Chord Oct"
    "\",\"short_name\":\"Oct\",\"type\":\"int\",\"min\":1,\"max\":5,\"default\":4,\"step\":1}"
    ",{\"key\":\"family\",\"name\":\"Shape Family\",\"short_name\":\"Fam\",\"options_as_strin"
    "g\":true,\"type\":\"enum\",\"options\":[\"5th\",\"triad\",\"6th\",\"7th\",\"9th\",\"ext"
    "\"],\"default\":1},{\"key\":\"shape\",\"name\":\"Chord Shape\",\"short_name\":\"Shape\","
    "\"options_as_string\":true,\"type\":\"enum\",\"options\":[\"rest\",\"note\",\"5\",\"oct"
    "\",\"maj\",\"min\",\"dim\",\"aug\",\"sus2\",\"sus4\",\"6\",\"m6\",\"69\",\"maj7\",\"min7"
    "\",\"dom7\",\"m7b5\",\"dim7\",\"mMaj7\",\"7sus4\",\"7sus2\",\"aug7\",\"maj7#5\",\"7b5\","
    "\"add9\",\"madd9\",\"maj9\",\"min9\",\"9\",\"7b9\",\"7#9\",\"m9b5\",\"11\",\"m11\",\"maj"
    "11\",\"13\",\"maj13\",\"m13\"],\"default\":4},{\"key\":\"inv\",\"name\":\"Inversion\",\""
    "short_name\":\"Inv\",\"type\":\"int\",\"min\":-3,\"max\":3,\"default\":0,\"step\":1},{\""
    "key\":\"len\",\"name\":\"Length\",\"short_name\":\"Len\",\"type\":\"int\",\"min\":1,\"ma"
    "x\":64,\"default\":8,\"step\":1},{\"key\":\"off\",\"name\":\"Offset\",\"short_name\":\"O"
    "ff\",\"type\":\"int\",\"min\":0,\"max\":15,\"default\":0,\"step\":1},{\"key\":\"ccolour"
    "\",\"name\":\"Colour\",\"short_name\":\"Col\",\"options_as_string\":true,\"type\":\"enum"
    "\",\"options\":[\"close\",\"open\",\"drop2\",\"drop3\",\"drop2+4\",\"shell\",\"rootless"
    "\",\"quartal\",\"spread\",\"cluster\"],\"default\":0},{\"key\":\"cstrum\",\"name\":\"Str"
    "um\",\"short_name\":\"Strm\",\"type\":\"int\",\"min\":0,\"max\":100,\"default\":0,\"step"
    "\":1},{\"key\":\"cvel\",\"name\":\"Chord Vel\",\"short_name\":\"CVel\",\"type\":\"int\","
    "\"min\":-63,\"max\":63,\"default\":0,\"step\":1},{\"key\":\"cgate\",\"name\":\"Chord Gat"
    "e\",\"short_name\":\"CGat\",\"type\":\"int\",\"min\":-50,\"max\":50,\"default\":0,\"step"
    "\":1},{\"key\":\"cmute\",\"name\":\"Mute\",\"short_name\":\"Mute\",\"options_as_string\""
    ":true,\"type\":\"enum\",\"options\":[\"off\",\"on\"],\"default\":0},{\"key\":\"rhythm\","
    "\"name\":\"Rhythm\",\"short_name\":\"Rhy\",\"options_as_string\":true,\"type\":\"enum\","
    "\"options\":[\"hold\",\"half\",\"quarter\",\"eighth\",\"16th\",\"offbeat\",\"dotted\",\""
    "push\",\"tresillo\",\"son 3-2\",\"son 2-3\",\"rumba\",\"bossa\",\"baiao\",\"montuno\",\""
    "charl\",\"charl 2\",\"shuffle\",\"comp\",\"gallop\",\"drive\",\"anthem\",\"garage\",\"br"
    "oken\",\"backbeat\",\"deep stab\",\"after kick\",\"gate\",\"two-step\",\"one drop\",\"he"
    "artbeat\",\"in three\",\"and four\",\"anticipate\",\"cascara\"],\"default\":0},{\"key\":"
    "\"ctrans\",\"name\":\"Transpose\",\"short_name\":\"Trns\",\"type\":\"int\",\"min\":-12,"
    "\"max\":12,\"default\":0,\"step\":1},{\"key\":\"genre\",\"name\":\"Genre\",\"short_name"
    "\":\"Gen\",\"options_as_string\":true,\"type\":\"enum\",\"options\":[\"none\",\"Pop\",\""
    "Rock\",\"Metal\",\"Jazz\",\"Blues\",\"Soul\",\"Gospel\",\"Funk\",\"R&B\",\"Bossa\",\"Lat"
    "in\",\"House\",\"Melodic\",\"Lo-fi\",\"Reggae\",\"Country\",\"Cinematic\"],\"default\":0"
    "},{\"key\":\"progression\",\"name\":\"Progression\",\"short_name\":\"Prog\",\"options_as"
    "_string\":true,\"type\":\"enum\",\"options\":[\"none\",\"Axis\",\"Sad Axis\",\"Doo-wop\""
    ",\"Pachelbel\",\"Royal Road\",\"Emotional\",\"Bittersweet\",\"Creep\",\"Falling\",\"Risi"
    "ng minor\",\"Diamonds\",\"Crazy\",\"Sensitive 7\",\"I-IV-V\",\"Mixolydian\",\"Grunge\","
    "\"Anthem\",\"bVII descent\",\"Mario\",\"Minor rock\",\"Thrash\",\"Power Metal\",\"Doom\""
    ",\"Phrygian\",\"Diabolus\",\"Iron\",\"Neoclassical\",\"ii-V-I\",\"Minor ii-V\",\"Rhythm "
    "A\",\"Turnaround\",\"Coltrane\",\"Tritone\",\"Bird Blues\",\"Modal\",\"Circle\",\"A Trai"
    "n\",\"Passing dim\",\"Ragtime\",\"Minor turn\",\"Autumn\",\"12-bar\",\"Quick IV\",\"Mino"
    "r blues\",\"8-bar\",\"Blues turn\",\"Slow 9ths\",\"Neo-soul\",\"Montuno\",\"iii-vi-ii-V"
    "\",\"Slow jam\",\"Two of Us\",\"Silky\",\"Mediants\",\"Plagal\",\"Shouting\",\"Walk-up\""
    ",\"Amen\",\"6-2-5-1\",\"Sunday\",\"Pass through\",\"Four five six\",\"JB vamp\",\"Funk t"
    "urn\",\"P-Funk\",\"Clav\",\"Chameleon\",\"Two-chord\",\"Minor lift\",\"Quiet storm\",\"B"
    "orrowed iv 7\",\"Purple\",\"Ipanema\",\"Wave\",\"Desafinado\",\"Corcovado\",\"Blue bossa"
    "\",\"Guajira\",\"Bolero\",\"Salsa\",\"Descarga\",\"Deep vamp\",\"Two-bar\",\"Filter\",\""
    "Garage\",\"Feel it\",\"Deep two\",\"Reflective\",\"Dorian vamp\",\"MAW\",\"Descending 7"
    "\",\"Late night\",\"Ninths\",\"Sunrise\",\"Piano house\",\"Prog lift\",\"Soulful\",\"Aft"
    "erlife\",\"Hypnotic\",\"Anjuna\",\"Mysterious\",\"Haunting\",\"Endless fall\",\"Dark ped"
    "al\",\"Lifter\",\"Uplift\",\"Suspended\",\"Prog minor\",\"Add nines\",\"Nostalgia\",\"St"
    "udy 2-5-1-4\",\"Drift\",\"Tape\",\"Rainy\",\"Stairs\",\"Descent\",\"Sinking\",\"Skank\","
    "\"Roots minor\",\"Rockers\",\"Boot scoot\",\"Train\",\"Bluegrass\",\"Andalusian\",\"Aeol"
    "ian\",\"Dorian\",\"Picardy\",\"Ostinato\",\"Lament\",\"Sorrow\",\"Elegy\",\"Hope\",\"Int"
    "erstellar\",\"Pirates\",\"Leia\",\"Lydian lift\",\"Ideal\",\"Gothic\",\"Neapolitan\",\"B"
    "reath\",\"Folia\",\"Passamezzo\"],\"default\":0},{\"key\":\"common\",\"name\":\"Common P"
    "rog\",\"short_name\":\"Cmn\",\"options_as_string\":true,\"type\":\"enum\",\"options\":["
    "\"none\",\"I-V-vi-IV\",\"vi-IV-I-V\",\"I-vi-IV-V\",\"I-IV-V\",\"I-IV-I-V\",\"ii-V-I\",\""
    "I-V-IV\",\"I-iii-IV-V\",\"vi-V-IV-V\",\"i-VI-III-VII\",\"i-iv-v\",\"i-VII-VI-VII\"],\"de"
    "fault\":0},{\"key\":\"uncommon\",\"name\":\"Uncommon\",\"short_name\":\"Unc\",\"options_"
    "as_string\":true,\"type\":\"enum\",\"options\":[\"none\",\"I-bIII-IV-iv\",\"Phrygian\","
    "\"Lydian II\",\"Backdoor\",\"bVI-bVII-I\",\"i-v-bVI-bVII\",\"Chrom desc\",\"Borrowed IV"
    "\",\"Secondary V\",\"i-bVI-iv-V\",\"Double plagal\",\"Aug lift\",\"Tritone col\"],\"defa"
    "ult\":0},{\"key\":\"colour\",\"name\":\"Colour\",\"short_name\":\"Col\",\"options_as_str"
    "ing\":true,\"type\":\"enum\",\"options\":[\"close\",\"open\",\"drop2\",\"drop3\",\"drop2"
    "+4\",\"shell\",\"rootless\",\"quartal\",\"spread\",\"cluster\"],\"default\":0},{\"key\":"
    "\"grouping\",\"name\":\"Voice Grouping\",\"short_name\":\"Grp\",\"options_as_string\":tr"
    "ue,\"type\":\"enum\",\"options\":[\"none\",\"dynamic\",\"dyn +1oct\",\"dyn +2oct\",\"C1-"
    "B2\",\"C2-B3\",\"C3-B4\",\"open 1\",\"open 2\",\"open 3\",\"guitar\",\"drop 2\",\"drop 3"
    "\",\"drop 4\",\"drop 2+3\",\"drop 2+4\"],\"default\":0},{\"key\":\"scale\",\"name\":\"Sc"
    "ale\",\"short_name\":\"Scale\",\"options_as_string\":true,\"type\":\"enum\",\"options\":"
    "[\"Chromatic\",\"Major\",\"Minor\",\"Dorian\",\"Phrygian\",\"Lydian\",\"Mixolydian\",\"L"
    "ocrian\",\"HarmMinor\",\"MelMinor\",\"PentMaj\",\"PentMin\",\"Blues\"],\"default\":2},{"
    "\"key\":\"key\",\"name\":\"Key\",\"short_name\":\"Key\",\"options_as_string\":true,\"typ"
    "e\":\"enum\",\"options\":[\"C\",\"C#\",\"D\",\"D#\",\"E\",\"F\",\"F#\",\"G\",\"G#\",\"A"
    "\",\"A#\",\"B\"],\"default\":9},{\"key\":\"steps\",\"name\":\"Chords\",\"short_name\":\""
    "Chrds\",\"type\":\"int\",\"min\":1,\"max\":16,\"default\":4,\"step\":1},{\"key\":\"rate"
    "\",\"name\":\"New Chord Len\",\"short_name\":\"Rte\",\"options_as_string\":true,\"type\""
    ":\"enum\",\"options\":[\"2 bar\",\"1 bar\",\"1/2\",\"1/4\"],\"default\":1},{\"key\":\"ru"
    "n\",\"name\":\"Run\",\"short_name\":\"Run\",\"options_as_string\":true,\"type\":\"enum\""
    ",\"options\":[\"off\",\"on\"],\"default\":1},{\"key\":\"octave\",\"name\":\"Clip Octave"
    "\",\"short_name\":\"Oct\",\"type\":\"int\",\"min\":-2,\"max\":2,\"default\":0,\"step\":1"
    "},{\"key\":\"transpose\",\"name\":\"Clip Transpose\",\"short_name\":\"Trans\",\"type\":"
    "\"int\",\"min\":-12,\"max\":12,\"default\":0,\"step\":1},{\"key\":\"velocity\",\"name\":"
    "\"Velocity\",\"short_name\":\"Vel\",\"type\":\"int\",\"min\":1,\"max\":127,\"default\":1"
    "00,\"step\":1},{\"key\":\"gate\",\"name\":\"Gate\",\"short_name\":\"Gate\",\"type\":\"in"
    "t\",\"min\":5,\"max\":100,\"default\":90,\"step\":1},{\"key\":\"hum_vel\",\"name\":\"Hum"
    "an Vel\",\"short_name\":\"HuVel\",\"type\":\"int\",\"min\":0,\"max\":100,\"default\":0,"
    "\"step\":1},{\"key\":\"roll_vel\",\"name\":\"Randomize Vel\",\"short_name\":\"RndVel\","
    "\"options_as_string\":true,\"type\":\"enum\",\"access\":\"write\",\"options\":[\"off\","
    "\"on\"],\"default\":0},{\"key\":\"hum_time\",\"name\":\"Human Time\",\"short_name\":\"Hu"
    "Tim\",\"type\":\"int\",\"min\":0,\"max\":100,\"default\":0,\"step\":1},{\"key\":\"roll_t"
    "ime\",\"name\":\"Randomize Time\",\"short_name\":\"RndTim\",\"options_as_string\":true,"
    "\"type\":\"enum\",\"access\":\"write\",\"options\":[\"off\",\"on\"],\"default\":0},{\"ke"
    "y\":\"swing\",\"name\":\"Swing\",\"short_name\":\"Swng\",\"type\":\"int\",\"min\":0,\"ma"
    "x\":75,\"default\":0,\"step\":1},{\"key\":\"lanes\",\"name\":\"Note Lanes\",\"short_name"
    "\":\"Lanes\",\"options_as_string\":true,\"type\":\"enum\",\"options\":[\"scale\",\"diato"
    "nic\"],\"default\":0},{\"key\":\"preview\",\"name\":\"Preview\",\"short_name\":\"Prev\","
    "\"options_as_string\":true,\"type\":\"enum\",\"options\":[\"off\",\"chord\",\"loop\"],\""
    "default\":0},{\"key\":\"read\",\"name\":\"Read Clip\",\"short_name\":\"Read\",\"options_"
    "as_string\":true,\"type\":\"enum\",\"options\":[\"off\",\"on\"],\"default\":0,\"access\""
    ":\"write\"},{\"key\":\"read_mode\",\"name\":\"Read Mode\",\"short_name\":\"RdMod\",\"opt"
    "ions_as_string\":true,\"type\":\"enum\",\"options\":[\"replace\",\"append\"],\"default\""
    ":0},{\"key\":\"read_track\",\"name\":\"Read Track\",\"short_name\":\"RdT\",\"type\":\"in"
    "t\",\"min\":1,\"max\":4,\"default\":1,\"step\":1},{\"key\":\"defoct\",\"name\":\"Octave "
    "Default\",\"short_name\":\"Oct\",\"type\":\"int\",\"min\":1,\"max\":5,\"default\":3,\"st"
    "ep\":1},{\"key\":\"bars\",\"name\":\"Clip Bars\",\"short_name\":\"Bars\",\"type\":\"int"
    "\",\"min\":1,\"max\":16,\"default\":4,\"step\":1},{\"key\":\"stamp\",\"name\":\"Stamp Cl"
    "ip\",\"short_name\":\"Stamp\",\"options_as_string\":true,\"type\":\"enum\",\"options\":["
    "\"off\",\"on\"],\"default\":0,\"access\":\"write\"},{\"key\":\"clear\",\"name\":\"Clear"
    "\",\"short_name\":\"Clear\",\"options_as_string\":true,\"type\":\"enum\",\"options\":[\""
    "off\",\"on\"],\"default\":0,\"access\":\"write\"},{\"key\":\"pat_store\",\"name\":\"Stor"
    "e Pattern\",\"short_name\":\"PSt\",\"type\":\"int\",\"min\":1,\"max\":16,\"default\":1,"
    "\"step\":1},{\"key\":\"pat_recall\",\"name\":\"Recall Pattern\",\"short_name\":\"PRc\","
    "\"type\":\"int\",\"min\":1,\"max\":16,\"default\":1,\"step\":1},{\"key\":\"pat_delete\","
    "\"name\":\"Delete Pattern\",\"short_name\":\"PDl\",\"type\":\"int\",\"min\":1,\"max\":16"
    ",\"default\":1,\"step\":1},{\"key\":\"play\",\"name\":\"Play Chord\",\"short_name\":\"Pl"
    "ay\",\"options_as_string\":true,\"type\":\"enum\",\"access\":\"write\",\"options\":[\"of"
    "f\",\"on\"],\"default\":0},{\"key\":\"duplicate\",\"name\":\"Duplicate\",\"short_name\":"
    "\"Dup\",\"options_as_string\":true,\"type\":\"enum\",\"access\":\"write\",\"options\":["
    "\"off\",\"on\"],\"default\":0},{\"key\":\"stretch\",\"name\":\"Double Length\",\"short_n"
    "ame\":\"Dbl\",\"options_as_string\":true,\"type\":\"enum\",\"access\":\"write\",\"option"
    "s\":[\"off\",\"on\"],\"default\":0},{\"key\":\"compress\",\"name\":\"Half Length\",\"sho"
    "rt_name\":\"Hlf\",\"options_as_string\":true,\"type\":\"enum\",\"access\":\"write\",\"op"
    "tions\":[\"off\",\"on\"],\"default\":0},{\"key\":\"undo\",\"name\":\"Undo\",\"short_name"
    "\":\"Und\",\"options_as_string\":true,\"type\":\"enum\",\"access\":\"write\",\"options\""
    ":[\"off\",\"on\"],\"default\":0},{\"key\":\"redo\",\"name\":\"Redo\",\"short_name\":\"Re"
    "d\",\"options_as_string\":true,\"type\":\"enum\",\"access\":\"write\",\"options\":[\"off"
    "\",\"on\"],\"default\":0},{\"key\":\"insert\",\"name\":\"Insert Chord\",\"short_name\":"
    "\"Ins\",\"options_as_string\":true,\"type\":\"enum\",\"access\":\"write\",\"options\":["
    "\"off\",\"on\"],\"default\":0},{\"key\":\"remove\",\"name\":\"Delete Chord\",\"short_nam"
    "e\":\"Del\",\"options_as_string\":true,\"type\":\"enum\",\"access\":\"write\",\"options"
    "\":[\"off\",\"on\"],\"default\":0}]";
/* ---- END GENERATED chain_params ---- */

/* ======================================================================
 * RHYTHMS
 *
 * A chord does not just sound once and hold. `repeat` said "fire N times,
 * evenly", which is one degenerate case of the thing actually wanted -- and
 * the only case that has no name in music. These are patterns.
 *
 * Each is a mask over SIXTEEN subdivisions of ONE STEP, and it TILES across
 * the chord's length rather than stretching to fill it: a two-step chord plays
 * the figure twice, which is what a figure does. Stretching would make the
 * same pattern a different rhythm depending on how long you happened to hold
 * the chord.
 *
 * Sixteen because that is a sixteenth-note grid at the default one-bar step,
 * and every pattern below is written on it: the clave, the tresillo and the
 * charleston are all defined by sixteenth positions and cannot be spelled on
 * an eighth-note grid.
 *
 * Bit i = a hit at subdivision i. The duration of a hit runs to the next one,
 * scaled by Gate -- so a dense pattern shortens itself and needs no separate
 * length control.
 * ====================================================================== */
typedef struct { const char *name; uint16_t mask; } stk_rhythm_t;

#define B(i) (1u << (i))
static const stk_rhythm_t RHYTHMS[] = {
 /* --- the grid ---------------------------------------------------------- */
 { "hold",   B(0) },
 { "half",   B(0)|B(8) },
 { "quarter",B(0)|B(4)|B(8)|B(12) },
 { "eighth", B(0)|B(2)|B(4)|B(6)|B(8)|B(10)|B(12)|B(14) },
 { "16th",   0xFFFFu },

 /* --- syncopation -------------------------------------------------------
    The offbeat is the ska and house-piano skank; the dotted eighth is the
    figure that cycles against 4/4 every three bars. */
 { "offbeat",B(2)|B(6)|B(10)|B(14) },
 { "dotted", B(0)|B(3)|B(6)|B(9)|B(12)|B(15) },
 { "push",   B(7)|B(8) },

 /* --- Afro-Cuban and Brazilian ------------------------------------------
    These are the reason the grid is sixteen and not eight: the clave, the
    tresillo and the bossa are all DEFINED by sixteenth positions and cannot
    be spelled on an eighth-note grid at all. */
 { "tresillo",B(0)|B(3)|B(6)|B(8)|B(11)|B(14) },   /* 3+3+2, both halves */
 { "son 3-2", B(0)|B(3)|B(6)|B(10)|B(12) },
 { "son 2-3", B(2)|B(4)|B(8)|B(11)|B(14) },
 { "rumba",   B(0)|B(3)|B(7)|B(10)|B(12) },
 { "bossa",   B(0)|B(3)|B(6)|B(10)|B(13) },
 { "baiao",   B(0)|B(3)|B(8)|B(11) },
 { "montuno", B(0)|B(2)|B(3)|B(6)|B(8)|B(10)|B(11)|B(14) },

 /* --- jazz --------------------------------------------------------------
    Swing is a triplet feel and a sixteenth grid cannot place a triplet, so
    "shuffle" is the closest 2:1 approximation the grid allows. Said plainly
    rather than pretended otherwise. */
 { "charl",  B(0)|B(6) },                          /* Charleston: 1, & of 2 */
 { "charl 2",B(0)|B(10) },
 { "shuffle",B(0)|B(5)|B(8)|B(13) },
 { "comp",   B(0)|B(3)|B(6)|B(11) },

 /* --- rock --------------------------------------------------------------- */
 { "gallop", B(0)|B(3)|B(4)|B(8)|B(11)|B(12) },
 { "drive",  B(0)|B(2)|B(3)|B(6)|B(8)|B(10)|B(12)|B(14) },
 { "anthem", B(0)|B(6)|B(8)|B(14) },

 /* --- electronic --------------------------------------------------------- */
 { "garage", B(0)|B(3)|B(6)|B(10) },
 { "broken", B(0)|B(6)|B(10) },

 /* --- house ---------------------------------------------------------------
    The chord is a STAB here, and where it lands is the whole style: the
    backbeat, the "and" of 1 and 3 that deep house organs live on, the
    sixteenth right after each kick that a sidechain leaves room for, and the
    dotted gate a trance pad is chopped by. Gate shortens them; the position
    is the figure. */
 { "backbeat",  B(4)|B(12) },                            /* 2 and 4 */
 { "deep stab", B(2)|B(10) },                            /* & of 1, & of 3 */
 { "after kick",B(1)|B(5)|B(9)|B(13) },                  /* the e of every beat */
 { "gate",      B(0)|B(3)|B(4)|B(7)|B(8)|B(11)|B(12)|B(15) }, /* x..xx..xx..xx..x */
 { "two-step",  B(0)|B(6)|B(8)|B(10) },                  /* UK garage kick pattern */
 { "one drop",  B(8) },                                  /* reggae: beat 3 only */

 /* --- cinematic -----------------------------------------------------------
    Pulses and breaths rather than grooves. "in three" is 3 in the bar on a
    16 grid -- 5.33 apart, so 0/5/11 -- said plainly like shuffle is. */
 { "heartbeat", B(0)|B(2)|B(8)|B(10) },                  /* lub-dub, twice */
 { "in three",  B(0)|B(5)|B(11) },
 { "and four",  B(0)|B(12) },                            /* 1, and a lean into the bar line */
 { "anticipate",B(0)|B(7) },                             /* 1, and the & of 2 pushed */

 /* --- more Afro-Cuban -----------------------------------------------------
    The cascara in 2-3, as the timbale plays it: the sixteen-position figure
    the son clave sits inside. */
 { "cascara",   B(0)|B(2)|B(3)|B(5)|B(7)|B(8)|B(10)|B(11)|B(13)|B(15) },
};
#undef B
#define NUM_RHYTHMS ((int)(sizeof(RHYTHMS) / sizeof(RHYTHMS[0])))

/* ======================================================================
 * The starting-point library
 *
 * SEMITONES FROM THE TONIC, NOT SCALE DEGREES. This is the load-bearing
 * choice. Degrees (I..vii) can only say what is already in the scale, and the
 * progressions worth having are exactly the ones that step outside it: the
 * bVII of a rock vamp, the bVI of an Andalusian cadence, the borrowed minor iv
 * of a soul turnaround, the tritone sub, the major V in a minor key that is
 * really the harmonic-minor raised seventh. A degree table renders all of those
 * as their nearest diatonic neighbour -- which is a different chord, quietly.
 * Twelve semitones say everything and transpose exactly.
 *
 * Each entry carries its own mode and SETS the scale when applied: a minor
 * progression read against a major scale is not a variation of it.
 *
 * Grouped by genre but NAMED BY WHAT THEY ARE. Genre is a fuzzy axis -- pop,
 * house and lo-fi all use vi-IV-I-V -- so four genres over four near-identical
 * progressions would make the labels decorative. The name states the harmony;
 * the genre only groups it. Ordered by genre and contiguous, exactly like
 * SHAPES[], so Genre jumps and Progression steps: the Family/Shape gesture
 * this module already has.
 *
 * Lengths are in eighths of a BAR, so 8 is one bar and 16 is two.
 * ====================================================================== */
typedef enum { GEN_NONE = 0, GEN_POP, GEN_ROCK, GEN_METAL, GEN_JAZZ, GEN_BLUES, GEN_SOUL, GEN_GOSPEL, GEN_FUNK, GEN_RNB, GEN_BOSSA, GEN_LATIN, GEN_HOUSE, GEN_MELODIC, GEN_LOFI, GEN_REGGAE, GEN_COUNTRY, GEN_CINE } stk_genre_t;
static const char *const GENRES[] = { "none", "Pop", "Rock", "Metal", "Jazz", "Blues", "Soul", "Gospel", "Funk", "R&B", "Bossa", "Latin", "House", "Melodic", "Lo-fi", "Reggae", "Country", "Cinematic" };
#define NUM_GENRES ((int)(sizeof(GENRES) / sizeof(GENRES[0])))

typedef struct { int semi; const char *q; int len; } stk_step_t;
typedef struct {
    const char *name;
    int genre;
    int minor;      /* realise against, and select, the minor scale */
    int n;
    stk_step_t s[8];
} stk_preset_t;

/*
 * ORDERED COMMON FIRST INSIDE EACH GENRE. That ordering is the "common versus
 * uncommon" axis, and expressing it as ORDER rather than as a second filter
 * costs nothing: turning Progression walks from a genre's staples into its
 * deeper cuts, and the knob's direction means something. A separate rarity
 * control would have to filter non-contiguously, which this contract cannot
 * express without serving a different option list per setting.
 */
static const stk_preset_t PRESETS[] = {
 { "none",         GEN_NONE,0,0, {{0,"maj",8}} },
 /* --- Pop ------------------------------------------------------- */
 { "Axis",         GEN_POP,0,4, {{0,"maj",8},{7,"maj",8},{9,"min",8},{5,"maj",8}} },
 { "Sad Axis",     GEN_POP,0,4, {{9,"min",8},{5,"maj",8},{0,"maj",8},{7,"maj",8}} },
 { "Doo-wop",      GEN_POP,0,4, {{0,"maj",8},{9,"min",8},{5,"maj",8},{7,"maj",8}} },
 { "Pachelbel",    GEN_POP,0,8, {{0,"maj",8},{7,"maj",8},{9,"min",8},{4,"min",8},{5,"maj",8},{0,"maj",8},{5,"maj",8},{7,"maj",8}} },
 { "Royal Road",   GEN_POP,0,4, {{5,"maj7",8},{7,"dom7",8},{4,"min7",8},{9,"min",8}} },
 { "Emotional",    GEN_POP,0,4, {{5,"maj",8},{0,"maj",8},{7,"maj",8},{9,"min",8}} },
 { "Bittersweet",  GEN_POP,0,4, {{0,"maj",8},{4,"min",8},{9,"min",8},{5,"maj",8}} },
 { "Creep",        GEN_POP,0,4, {{0,"maj",8},{4,"maj",8},{5,"maj",8},{5,"min",8}} },
 { "Falling",      GEN_POP,1,4, {{0,"min",8},{7,"min",8},{5,"min",8},{10,"maj",8}} },
 { "Rising minor", GEN_POP,1,3, {{8,"maj",8},{10,"maj",8},{0,"min",16}} },
 { "Diamonds",     GEN_POP,1,4, {{8,"maj",8},{0,"min",8},{10,"maj",8},{3,"maj",8}} },
 { "Crazy",        GEN_POP,1,4, {{0,"min",8},{3,"dom7",8},{8,"maj",8},{7,"maj",8}} },
 { "Sensitive 7",  GEN_POP,0,4, {{9,"min7",8},{5,"maj7",8},{0,"maj7",8},{7,"dom7",8}} },
 /* --- Rock ------------------------------------------------------ */
 { "I-IV-V",       GEN_ROCK,0,4, {{0,"maj",8},{5,"maj",8},{7,"maj",8},{0,"maj",8}} },
 { "Mixolydian",   GEN_ROCK,0,4, {{0,"maj",8},{10,"maj",8},{5,"maj",8},{0,"maj",8}} },
 { "Grunge",       GEN_ROCK,1,4, {{0,"min",8},{3,"maj",8},{10,"maj",8},{5,"maj",8}} },
 { "Anthem",       GEN_ROCK,0,4, {{0,"maj",8},{5,"maj",8},{9,"min",8},{7,"maj",8}} },
 { "bVII descent", GEN_ROCK,0,4, {{0,"maj",8},{7,"maj",8},{10,"maj",8},{5,"maj",8}} },
 { "Mario",        GEN_ROCK,0,3, {{8,"maj",8},{10,"maj",8},{0,"maj",16}} },
 { "Minor rock",   GEN_ROCK,1,4, {{0,"min",8},{10,"maj",8},{8,"maj",8},{10,"maj",8}} },
 /* --- Metal ----------------------------------------------------- */
 { "Thrash",       GEN_METAL,1,4, {{0,"5",8},{1,"5",8},{0,"5",8},{10,"5",8}} },
 { "Power Metal",  GEN_METAL,1,4, {{0,"min",8},{8,"maj",8},{3,"maj",8},{10,"maj",8}} },
 { "Doom",         GEN_METAL,1,2, {{0,"min",16},{1,"maj",16}} },
 { "Phrygian",     GEN_METAL,1,4, {{0,"5",8},{1,"maj",8},{0,"5",8},{10,"maj",8}} },
 { "Diabolus",     GEN_METAL,1,2, {{0,"5",16},{6,"5",16}} },
 { "Iron",         GEN_METAL,1,4, {{0,"5",8},{3,"5",8},{10,"5",8},{5,"5",8}} },
 { "Neoclassical", GEN_METAL,1,3, {{0,"min",8},{8,"maj",8},{7,"maj",16}} },
 /* --- Jazz ------------------------------------------------------ */
 { "ii-V-I",       GEN_JAZZ,0,3, {{2,"min7",8},{7,"dom7",8},{0,"maj7",16}} },
 { "Minor ii-V",   GEN_JAZZ,1,3, {{2,"m7b5",8},{7,"dom7",8},{0,"min7",16}} },
 { "Rhythm A",     GEN_JAZZ,0,4, {{0,"maj7",8},{9,"min7",8},{2,"min7",8},{7,"dom7",8}} },
 { "Turnaround",   GEN_JAZZ,0,4, {{0,"maj7",8},{9,"dom7",8},{2,"min7",8},{7,"dom7",8}} },
 { "Coltrane",     GEN_JAZZ,0,5, {{8,"maj7",8},{11,"dom7",8},{4,"maj7",8},{7,"dom7",8},{0,"maj7",16}} },
 { "Tritone",      GEN_JAZZ,0,3, {{2,"min7",8},{1,"dom7",8},{0,"maj7",16}} },
 { "Bird Blues",   GEN_JAZZ,0,7, {{0,"dom7",8},{5,"dom7",8},{0,"dom7",8},{9,"dom7",8},{2,"min7",8},{7,"dom7",8},{0,"maj7",16}} },
 { "Modal",        GEN_JAZZ,1,2, {{0,"min7",16},{0,"min7",16}} },
 { "Circle",       GEN_JAZZ,0,4, {{9,"min7",8},{2,"min7",8},{7,"dom7",8},{0,"maj7",8}} },
 { "A Train",      GEN_JAZZ,0,4, {{0,"maj7",8},{2,"dom7",8},{2,"min7",8},{7,"dom7",8}} },
 { "Passing dim",  GEN_JAZZ,0,4, {{0,"maj7",8},{1,"dim7",8},{2,"min7",8},{7,"dom7",8}} },
 { "Ragtime",      GEN_JAZZ,0,4, {{4,"dom7",8},{9,"dom7",8},{2,"dom7",8},{7,"dom7",8}} },
 { "Minor turn",   GEN_JAZZ,1,4, {{0,"min7",8},{8,"dom7",8},{2,"m7b5",8},{7,"dom7",8}} },
 { "Autumn",       GEN_JAZZ,1,7, {{5,"min7",8},{10,"dom7",8},{3,"maj7",8},{8,"maj7",8},{2,"m7b5",8},{7,"dom7",8},{0,"min7",16}} },
 /* --- Blues ----------------------------------------------------- */
 { "12-bar",       GEN_BLUES,0,7, {{0,"dom7",16},{0,"dom7",16},{5,"dom7",16},{0,"dom7",16},{7,"dom7",8},{5,"dom7",8},{0,"dom7",16}} },
 { "Quick IV",     GEN_BLUES,0,4, {{0,"dom7",8},{5,"dom7",8},{0,"dom7",8},{7,"dom7",8}} },
 { "Minor blues",  GEN_BLUES,1,4, {{0,"min7",16},{5,"min7",8},{0,"min7",8},{7,"dom7",8}} },
 { "8-bar",        GEN_BLUES,0,5, {{0,"dom7",16},{5,"dom7",8},{0,"dom7",8},{7,"dom7",8},{0,"dom7",8}} },
 { "Blues turn",   GEN_BLUES,0,4, {{0,"dom7",8},{9,"dom7",8},{2,"min7",8},{7,"dom7",8}} },
 { "Slow 9ths",    GEN_BLUES,0,4, {{0,"9",16},{5,"9",8},{0,"9",8},{7,"9",8}} },
 /* --- Soul ------------------------------------------------------ */
 { "Neo-soul",     GEN_SOUL,0,4, {{0,"maj7",8},{4,"min7",8},{5,"maj7",8},{5,"min7",8}} },
 { "Montuno",      GEN_SOUL,1,4, {{0,"min7",8},{5,"min7",8},{7,"dom7",8},{5,"min7",8}} },
 { "iii-vi-ii-V",  GEN_SOUL,0,4, {{4,"min7",8},{9,"min7",8},{2,"min7",8},{7,"dom7",8}} },
 { "Slow jam",     GEN_SOUL,0,4, {{0,"maj7",8},{4,"min7",8},{9,"min7",8},{5,"maj7",8}} },
 { "Two of Us",    GEN_SOUL,0,4, {{5,"maj7",8},{4,"dom7",8},{9,"min7",8},{2,"dom7",8}} },
 { "Silky",        GEN_SOUL,0,3, {{2,"min9",8},{7,"dom7",8},{0,"maj9",16}} },
 { "Mediants",     GEN_SOUL,0,4, {{0,"maj",8},{3,"maj",8},{8,"maj",8},{7,"maj",8}} },
 /* --- Gospel ---------------------------------------------------- */
 { "Plagal",       GEN_GOSPEL,0,4, {{0,"maj",8},{5,"maj",8},{5,"min",8},{0,"maj",8}} },
 { "Shouting",     GEN_GOSPEL,0,4, {{0,"maj",8},{2,"dom7",8},{7,"maj",8},{0,"maj",8}} },
 { "Walk-up",      GEN_GOSPEL,0,4, {{0,"maj",8},{0,"aug",8},{0,"6",8},{0,"dom7",8}} },
 { "Amen",         GEN_GOSPEL,0,3, {{5,"maj7",8},{7,"dom7",8},{0,"maj7",16}} },
 { "6-2-5-1",      GEN_GOSPEL,0,4, {{9,"min7",8},{2,"min7",8},{7,"13",8},{0,"maj9",8}} },
 { "Sunday",       GEN_GOSPEL,0,3, {{0,"maj",8},{4,"dom7",8},{9,"min7",16}} },
 { "Pass through", GEN_GOSPEL,0,6, {{0,"maj",8},{0,"dom7",8},{5,"maj",8},{2,"dom7",8},{7,"maj",8},{0,"maj",8}} },
 { "Four five six",GEN_GOSPEL,0,3, {{5,"maj7",8},{7,"dom7",8},{9,"min7",16}} },
 /* --- Funk ------------------------------------------------------ */
 { "JB vamp",      GEN_FUNK,0,2, {{0,"9",16},{0,"9",16}} },
 { "Funk turn",    GEN_FUNK,0,4, {{0,"dom7",8},{5,"dom7",8},{0,"dom7",8},{0,"dom7",8}} },
 { "P-Funk",       GEN_FUNK,1,3, {{0,"min7",16},{5,"min7",8},{0,"min7",8}} },
 { "Clav",         GEN_FUNK,1,4, {{0,"min7",8},{3,"dom7",8},{0,"min7",8},{10,"dom7",8}} },
 { "Chameleon",    GEN_FUNK,1,2, {{0,"min7",16},{5,"dom7",16}} },
 /* --- R&B ------------------------------------------------------- */
 { "Two-chord",    GEN_RNB,0,2, {{0,"maj7",16},{5,"maj7",16}} },
 { "Minor lift",   GEN_RNB,1,2, {{0,"min7",16},{5,"maj7",16}} },
 { "Quiet storm",  GEN_RNB,0,4, {{0,"maj7",8},{2,"min7",8},{4,"min7",8},{5,"maj7",8}} },
 { "Borrowed iv 7",GEN_RNB,0,4, {{0,"maj7",8},{5,"maj7",8},{5,"min7",8},{0,"maj7",8}} },
 { "Purple",       GEN_RNB,0,4, {{0,"maj7",8},{4,"min7",8},{9,"min7",8},{5,"maj9",8}} },
 /* --- Bossa ----------------------------------------------------- */
 { "Ipanema",      GEN_BOSSA,0,4, {{0,"maj7",8},{2,"min7",8},{7,"dom7",8},{0,"maj7",8}} },
 { "Wave",         GEN_BOSSA,0,4, {{0,"maj7",8},{11,"m7b5",8},{4,"dom7",8},{9,"min7",8}} },
 { "Desafinado",   GEN_BOSSA,0,4, {{0,"maj7",8},{0,"dom7",8},{5,"maj7",8},{5,"min7",8}} },
 { "Corcovado",    GEN_BOSSA,1,4, {{0,"min7",8},{0,"dom7",8},{5,"min7",8},{10,"dom7",8}} },
 { "Blue bossa",   GEN_BOSSA,1,5, {{0,"min7",8},{5,"min7",8},{2,"m7b5",8},{7,"dom7",8},{0,"min7",16}} },
 /* --- Latin ----------------------------------------------------- */
 { "Guajira",      GEN_LATIN,0,4, {{0,"maj",8},{5,"maj",8},{7,"maj",8},{5,"maj",8}} },
 { "Bolero",       GEN_LATIN,1,4, {{0,"min",8},{5,"min",8},{7,"maj",8},{0,"min",8}} },
 { "Salsa",        GEN_LATIN,1,3, {{2,"dom7",8},{7,"dom7",8},{0,"min7",16}} },
 { "Descarga",     GEN_LATIN,0,2, {{2,"min7",16},{7,"dom7",16}} },
 /* --- House ----------------------------------------------------- */
 { "Deep vamp",    GEN_HOUSE,1,4, {{0,"min7",8},{10,"maj",8},{8,"maj",8},{10,"maj",8}} },
 { "Two-bar",      GEN_HOUSE,1,2, {{0,"min7",16},{5,"min7",16}} },
 { "Filter",       GEN_HOUSE,1,4, {{0,"min7",8},{8,"maj7",8},{10,"maj7",8},{0,"min7",8}} },
 { "Garage",       GEN_HOUSE,1,4, {{0,"min9",8},{5,"min9",8},{10,"maj7",8},{3,"maj7",8}} },
 { "Feel it",      GEN_HOUSE,1,4, {{0,"min",8},{8,"maj7",8},{10,"maj",8},{0,"maj7",8}} },
 { "Deep two",     GEN_HOUSE,0,2, {{5,"maj7",16},{4,"min7",16}} },
 { "Reflective",   GEN_HOUSE,0,2, {{9,"min7",16},{5,"maj7",16}} },
 { "Dorian vamp",  GEN_HOUSE,1,2, {{0,"min7",16},{5,"dom7",16}} },
 { "MAW",          GEN_HOUSE,1,3, {{0,"min7",8},{3,"maj7",8},{7,"min7",16}} },
 { "Descending 7", GEN_HOUSE,1,3, {{0,"min7",8},{7,"min7",8},{5,"min7",16}} },
 { "Late night",   GEN_HOUSE,1,4, {{0,"min7",8},{5,"min7",8},{10,"dom7",8},{3,"maj7",8}} },
 { "Ninths",       GEN_HOUSE,1,3, {{0,"min9",8},{8,"maj9",8},{10,"9",16}} },
 { "Sunrise",      GEN_HOUSE,1,4, {{8,"maj",8},{10,"maj",8},{0,"min",8},{8,"maj",8}} },
 { "Piano house",  GEN_HOUSE,0,4, {{9,"min7",8},{5,"maj7",8},{0,"maj7",8},{7,"dom7",8}} },
 { "Prog lift",    GEN_HOUSE,0,4, {{0,"maj",8},{8,"maj",8},{5,"maj",8},{7,"maj",8}} },
 { "Soulful",      GEN_HOUSE,0,4, {{4,"min7",8},{9,"min7",8},{2,"min9",8},{7,"13",8}} },
 /* --- Melodic ------------------------------------------------- */
 { "Afterlife",    GEN_MELODIC,1,4, {{0,"min9",8},{8,"maj7",8},{3,"maj7",8},{10,"add9",8}} },
 { "Hypnotic",     GEN_MELODIC,1,2, {{0,"min",16},{10,"maj",16}} },
 { "Anjuna",       GEN_MELODIC,1,4, {{0,"min",8},{3,"maj",8},{10,"maj",8},{8,"maj",8}} },
 { "Mysterious",   GEN_MELODIC,1,4, {{0,"min",8},{8,"maj",8},{5,"min",8},{10,"maj",8}} },
 { "Haunting",     GEN_MELODIC,1,4, {{0,"min",8},{5,"min",8},{7,"min",8},{3,"maj",8}} },
 { "Endless fall", GEN_MELODIC,1,4, {{0,"min",8},{10,"maj",8},{8,"maj",8},{7,"min",8}} },
 { "Dark pedal",   GEN_MELODIC,1,4, {{0,"min",8},{5,"min",8},{0,"min",8},{10,"maj",8}} },
 { "Lifter",       GEN_MELODIC,1,4, {{0,"min",8},{7,"maj",8},{8,"maj",8},{7,"maj",8}} },
 { "Uplift",       GEN_MELODIC,1,3, {{8,"maj",8},{10,"maj",8},{0,"min",16}} },
 { "Suspended",    GEN_MELODIC,1,4, {{0,"min",8},{8,"sus2",8},{3,"maj",8},{10,"sus2",8}} },
 { "Prog minor",   GEN_MELODIC,1,4, {{0,"min7",8},{3,"maj",8},{7,"min7",8},{5,"maj",8}} },
 { "Add nines",    GEN_MELODIC,1,4, {{8,"add9",8},{0,"min7",8},{10,"add9",8},{7,"min",8}} },
 /* --- Lo-fi --------------------------------------------------- */
 { "Nostalgia",    GEN_LOFI,0,4, {{0,"maj7",8},{9,"min7",8},{5,"maj7",8},{7,"dom7",8}} },
 { "Study 2-5-1-4",GEN_LOFI,0,4, {{2,"min7",8},{7,"dom7",8},{0,"maj7",8},{5,"maj7",8}} },
 { "Drift",        GEN_LOFI,0,4, {{0,"maj7",8},{4,"min7",8},{5,"maj7",8},{2,"min7",8}} },
 { "Tape",         GEN_LOFI,0,3, {{5,"maj7",8},{0,"maj7",8},{9,"min7",16}} },
 { "Rainy",        GEN_LOFI,0,4, {{0,"maj7",8},{9,"min7",8},{4,"min7",8},{11,"m7b5",8}} },
 { "Stairs",       GEN_LOFI,0,3, {{2,"min7",8},{4,"min7",8},{5,"maj7",16}} },
 { "Descent",      GEN_LOFI,0,4, {{5,"maj7",8},{4,"min7",8},{2,"min7",8},{0,"maj7",8}} },
 { "Sinking",      GEN_LOFI,1,4, {{8,"maj7",8},{7,"min7",8},{5,"min7",8},{3,"maj7",8}} },
 /* --- Reggae ---------------------------------------------------- */
 { "Skank",        GEN_REGGAE,0,2, {{0,"maj",16},{5,"maj",16}} },
 { "Roots minor",  GEN_REGGAE,1,4, {{0,"min",8},{10,"maj",8},{5,"maj",8},{0,"min",8}} },
 { "Rockers",      GEN_REGGAE,0,4, {{0,"maj",8},{5,"maj",8},{0,"maj",8},{7,"maj",8}} },
 /* --- Country --------------------------------------------------- */
 { "Boot scoot",   GEN_COUNTRY,0,4, {{0,"maj",8},{10,"maj",8},{5,"maj",8},{7,"maj",8}} },
 { "Train",        GEN_COUNTRY,0,3, {{0,"maj",16},{7,"maj",8},{0,"maj",8}} },
 { "Bluegrass",    GEN_COUNTRY,0,4, {{0,"maj",16},{5,"maj",16},{0,"maj",16},{7,"maj",16}} },
 /* --- Cinematic ------------------------------------------------- */
 { "Andalusian",   GEN_CINE,1,4, {{0,"min",8},{10,"maj",8},{8,"maj",8},{7,"maj",8}} },
 { "Aeolian",      GEN_CINE,1,3, {{0,"min",8},{8,"maj",8},{10,"maj",16}} },
 { "Dorian",       GEN_CINE,1,2, {{0,"min",16},{5,"maj",16}} },
 { "Picardy",      GEN_CINE,1,4, {{0,"min",8},{5,"min",8},{7,"maj",8},{0,"maj",8}} },
 { "Ostinato",     GEN_CINE,1,2, {{0,"min",16},{8,"maj",16}} },
 { "Lament",       GEN_CINE,1,4, {{0,"min",8},{11,"maj",8},{10,"maj",8},{8,"maj",8}} },
 { "Sorrow",       GEN_CINE,1,4, {{0,"min",8},{5,"min",8},{3,"maj",8},{10,"maj",8}} },
 { "Elegy",        GEN_CINE,1,4, {{0,"min",8},{3,"maj",8},{5,"min",8},{0,"min",8}} },
 { "Hope",         GEN_CINE,1,4, {{8,"maj",8},{3,"maj",8},{10,"maj",8},{0,"min",8}} },
 { "Interstellar", GEN_CINE,1,4, {{8,"maj7",8},{7,"min",8},{0,"min",8},{7,"min7",8}} },
 { "Pirates",      GEN_CINE,1,7, {{0,"min",8},{5,"maj",8},{7,"min",8},{0,"min",8},{8,"maj",8},{3,"maj",8},{0,"min",16}} },
 { "Leia",         GEN_CINE,0,2, {{0,"maj",16},{5,"min",16}} },
 { "Lydian lift",  GEN_CINE,0,2, {{0,"maj",16},{2,"maj",16}} },
 { "Ideal",        GEN_CINE,0,3, {{0,"maj",8},{8,"maj",8},{5,"maj",16}} },
 { "Gothic",       GEN_CINE,1,2, {{0,"min",16},{7,"maj7",16}} },
 { "Neapolitan",   GEN_CINE,1,4, {{0,"min",8},{1,"maj",8},{7,"maj",8},{0,"min",8}} },
 { "Breath",       GEN_CINE,1,2, {{0,"sus4",16},{0,"min",16}} },
 { "Folia",        GEN_CINE,1,8, {{0,"min",8},{7,"maj",8},{0,"min",8},{10,"maj",8},{3,"maj",8},{10,"maj",8},{0,"min",8},{7,"maj",8}} },
 { "Passamezzo",   GEN_CINE,1,8, {{0,"min",8},{10,"maj",8},{0,"min",8},{7,"maj",8},{3,"maj",8},{10,"maj",8},{0,"min",8},{7,"maj",8}} },
};
#define NUM_PRESETS ((int)(sizeof(PRESETS) / sizeof(PRESETS[0])))

/*
 * THE OTHER TWO LIBRARIES: generic progressions, written as plain triads.
 *
 * They carry no style of their own -- that is the point. A genre progression
 * says "this is bossa, and bossa is maj7 and m7"; these say only "I-V-vi-IV",
 * and Colour decides whether you hear that as triads, sevenths or ninths.
 * Splitting them from the genre table is what lets Colour be meaningful on one
 * and inert on the other, rather than a setting that sometimes vandalises a
 * curated voicing.
 *
 * COMMON is what most songs are built from. UNCOMMON is the chromatic and
 * modal material -- borrowed chords, secondary dominants, chromatic mediants --
 * that is worth having to hand precisely because it is not what you reach for
 * by reflex.
 */
static const stk_preset_t COMMON[] = {
 { "none",        GEN_NONE, 0, 0, {{0,"maj",8}} },
 { "I-V-vi-IV",   GEN_NONE, 0, 4, {{0,"maj",8},{7,"maj",8},{9,"min",8},{5,"maj",8}} },
 { "vi-IV-I-V",   GEN_NONE, 0, 4, {{9,"min",8},{5,"maj",8},{0,"maj",8},{7,"maj",8}} },
 { "I-vi-IV-V",   GEN_NONE, 0, 4, {{0,"maj",8},{9,"min",8},{5,"maj",8},{7,"maj",8}} },
 { "I-IV-V",      GEN_NONE, 0, 3, {{0,"maj",8},{5,"maj",8},{7,"maj",16}} },
 { "I-IV-I-V",    GEN_NONE, 0, 4, {{0,"maj",8},{5,"maj",8},{0,"maj",8},{7,"maj",8}} },
 { "ii-V-I",      GEN_NONE, 0, 3, {{2,"min",8},{7,"maj",8},{0,"maj",16}} },
 { "I-V-IV",      GEN_NONE, 0, 3, {{0,"maj",8},{7,"maj",8},{5,"maj",16}} },
 { "I-iii-IV-V",  GEN_NONE, 0, 4, {{0,"maj",8},{4,"min",8},{5,"maj",8},{7,"maj",8}} },
 { "vi-V-IV-V",   GEN_NONE, 0, 4, {{9,"min",8},{7,"maj",8},{5,"maj",8},{7,"maj",8}} },
 { "i-VI-III-VII",GEN_NONE, 1, 4, {{0,"min",8},{8,"maj",8},{3,"maj",8},{10,"maj",8}} },
 { "i-iv-v",      GEN_NONE, 1, 3, {{0,"min",8},{5,"min",8},{7,"min",16}} },
 { "i-VII-VI-VII",GEN_NONE, 1, 4, {{0,"min",8},{10,"maj",8},{8,"maj",8},{10,"maj",8}} },
};
#define NUM_COMMON ((int)(sizeof(COMMON) / sizeof(COMMON[0])))

static const stk_preset_t UNCOMMON[] = {
 { "none",        GEN_NONE, 0, 0, {{0,"maj",8}} },
 { "I-bIII-IV-iv",GEN_NONE, 0, 4, {{0,"maj",8},{3,"maj",8},{5,"maj",8},{5,"min",8}} },
 { "Phrygian",    GEN_NONE, 1, 4, {{0,"min",8},{1,"maj",8},{0,"min",8},{10,"maj",8}} },
 { "Lydian II",   GEN_NONE, 0, 4, {{0,"maj",8},{2,"maj",8},{5,"maj",8},{0,"maj",8}} },
 { "Backdoor",    GEN_NONE, 0, 3, {{8,"maj",8},{10,"maj",8},{0,"maj",16}} },
 { "bVI-bVII-I",  GEN_NONE, 0, 4, {{0,"maj",8},{8,"maj",8},{10,"maj",8},{0,"maj",8}} },
 { "i-v-bVI-bVII",GEN_NONE, 1, 4, {{0,"min",8},{7,"min",8},{8,"maj",8},{10,"maj",8}} },
 { "Chrom desc",  GEN_NONE, 0, 4, {{4,"min",8},{3,"maj",8},{2,"min",8},{1,"maj",8}} },
 { "Borrowed IV", GEN_NONE, 0, 3, {{5,"maj",8},{5,"min",8},{0,"maj",16}} },
 { "Secondary V", GEN_NONE, 0, 4, {{0,"maj",8},{4,"maj",8},{9,"min",8},{5,"maj",8}} },
 { "i-bVI-iv-V",  GEN_NONE, 1, 4, {{0,"min",8},{8,"maj",8},{5,"min",8},{7,"maj",8}} },
 { "Double plagal",GEN_NONE,0, 4, {{10,"maj",8},{5,"maj",8},{0,"maj",8},{0,"maj",8}} },
 { "Aug lift",    GEN_NONE, 1, 4, {{0,"min",8},{3,"aug",8},{5,"min",8},{7,"maj",8}} },
 { "Tritone col", GEN_NONE, 0, 4, {{0,"maj",8},{6,"dim",8},{5,"maj",8},{0,"maj",8}} },
};
#define NUM_UNCOMMON ((int)(sizeof(UNCOMMON) / sizeof(UNCOMMON[0])))

/* ======================================================================
 * The progression
 * ====================================================================== */
typedef struct {
    int root;   /* MIDI note of the chord root */
    int shape;  /* index into SHAPES */
    int inv;    /* -3..+3; positive rotates up, negative the same an 8ve down */
    int len;    /* slot and sounding length, in eighths of a bar */
    int off;    /* start offset within the step, in eighths */

    /* --- BANK 2, the per-chord voice ---
     * All offsets rather than absolutes, so the global Velocity and Gate stay
     * the thing you set once and these stay the thing you vary per chord. A
     * chord that has been left alone reads as zeros and behaves exactly as it
     * did before any of this existed. */
    int colour;  /* 0 written, 1 triad, 2 seventh, 3 ninth -- PER CHORD */
    /*
     * STYLED: this chord came from a GENRE progression, and its voicing is the
     * style. Colour does not touch it. A jazz ii-V-I is m7-dom7-maj7 because
     * that is what makes it jazz; flattening it to triads on a global setting
     * would quietly turn the library into something else.
     *
     * The Common and Uncommon libraries are the opposite: plain shapes with no
     * inherent density, which is exactly what Colour is for. A chord you build
     * by hand is unstyled too -- you chose its shape, so you may recolour it.
     */
    int styled;
    int strum;   /* 0-100 % of an eighth, spread across the voices */
    int vel;     /* -63..+63 on the global velocity */
    int gate;    /* -50..+50 % on the global gate */
    int mute;    /* 1 = this chord is skipped, and drawn hollow */
    int rhythm;  /* index into RHYTHMS[] */
    int trans;   /* -12..+12 semitones, after the shape is built */
    /*
     * EXACT: the root is the one that was WRITTEN, and the scale may not
     * move it. Set by every path that names a root outright -- the Root
     * knob, the libraries -- and cleared by the one path that asks for a
     * root BY DEGREE, which is the only kind of root the scale owns.
     *
     * Without it the library was defeated by its own player: the entries are
     * stored in semitones precisely so a bVII or a borrowed bVI can step
     * outside the scale, and `chord_notes` then snapped that root to the
     * nearest scale tone -- "bVII descent" in C Major played C G B F, and the
     * staff captioned the B as "A#" because the caption read the written
     * root. Measured 2026-09-19; tests/test_stacks_exact_root.sh.
     */
    int exact;
} stk_chord_t;

typedef struct {
    stk_chord_t ch[STK_MAX_CHORDS];
    int count;
    int scale;      /* index into SCALES */
    int key;        /* 0..11 */
    int rate;       /* index into RATES */
} stk_prog_t;

/*
 * A STORED PATTERN IS THE BUFFER, not a clip.
 *
 * The progression, the clip LENGTH it was written against, and the voice
 * grouping it was voiced with -- because recalling a 12-bar phrase into a
 * 4-bar clip would truncate it silently, which is the failure `bars` already
 * has a rule about elsewhere in this file.
 */
typedef struct {
    int used;
    stk_prog_t prog;
    int bars;
    int grouping;
} stk_pattern_t;

/*
 * THE PROGRESSION IS A SEQUENCE OF DURATIONS, NOT SLOTS ON A GRID.
 *
 * Each chord occupies `len` units and the next one begins where it ends, so
 * lengthening a chord PUSHES everything after it later -- which is what
 * "expand a chord" has to mean if the result is still the same progression.
 *
 * The alternative, a chord per fixed step, was what this was: expanding then
 * ran a chord ON TOP of its neighbour instead of moving it, and there was no
 * way to write a progression whose chords had different lengths without
 * silence or overlap appearing somewhere.
 *
 * `off` still delays a chord INSIDE its own slot, so it stays a placement and
 * not a length: it cannot push the next chord, and a chord with a big offset
 * is a late chord, not a longer one.
 */
static int chord_slot_start(const stk_prog_t *p, int k) {
    int acc = 0;
    for (int i = 0; i < k && i < p->count; i++) {
        int l = p->ch[i].len;
        acc += (l < 1 ? 1 : l);
    }
    return acc;
}

static int prog_total_units(const stk_prog_t *p) {
    int t = chord_slot_start(p, p->count);
    return t < 1 ? 1 : t;
}


/*
 * THE PLACEMENT GRID IS FIXED AT AN EIGHTH OF A STEP, deliberately.
 *
 * A settable resolution was tried and removed: it makes `len` and `off` mean
 * something different depending on a third knob, so the same numbers describe
 * different music and every value on screen needs the grid read beside it to
 * be understood. One baseline that never moves is worth more than the extra
 * placements, and a chord that wants to land somewhere else has `off`.
 */
#define STEP_SUBDIV 8

/*
 * LENGTH IS AN ABSOLUTE DURATION. RATE ONLY INITIALISES IT.
 *
 * `len` used to be counted in units of RATE, so turning Rate changed what
 * every existing length MEANT: the same four chords were 4 bars at "1 bar" and
 * 8 at "2 bar", and a library entry -- whose lengths are stored as numbers --
 * played at whatever scale happened to be selected. A "12-bar" blues was 6
 * bars at "1/2" and 24 at "2 bar", where it truncated against the 16-bar clip.
 * That is one control silently redefining the units of another, and it caused
 * three separate bugs before it was named.
 *
 * So a unit is now a fixed EIGHTH OF A BAR, everywhere and always, and Rate
 * does exactly one job: it chooses the length a NEWLY created chord gets. It
 * can no longer reach a chord that already exists, which is what makes a
 * stored progression mean the same thing whatever the rate is set to.
 */
#define UNITS_PER_BAR   8
#define BAR_CLOCKS      (4 * CLOCKS_PER_QUARTER)
#define UNIT_CLOCKS     (BAR_CLOCKS / UNITS_PER_BAR)
#define STK_MAX_LEN     64      /* 8 bars: eight times what a rate-relative
                                 * 16 could reach, because a unit no longer
                                 * grows to cover a longer chord */

/*
 * BAR ARITHMETIC, ONCE.
 *
 * "How long is a bar in units" was computed inline at four sites -- Duplicate,
 * Double Length, the sequencer's wrap and the staff -- and the fourth one
 * disagreeing is exactly how the playhead came to sweep a different length
 * than the music played. One fact, one function.
 */
static int bar_units_for(const stk_prog_t *p) {
    (void)p;                       /* a bar is eight units. Always. */
    return UNITS_PER_BAR;
}

/* Rate's ONLY job: the length a newly created chord is given. */
static const int RATE_DEFAULT_LEN[] = { 16, 8, 4, 2 };   /* 2bar 1bar 1/2 1/4 */
static int default_len(const stk_prog_t *p) {
    int r = p->rate;
    if (r < 0 || r >= (int)(sizeof(RATE_DEFAULT_LEN)/sizeof(int))) r = 1;
    return RATE_DEFAULT_LEN[r];
}

static const char *RATE_OPTS[] = { "2 bar", "1 bar", "1/2", "1/4" };
#define NUM_RATES ((int)(sizeof(RATE_DEFAULT_LEN) / sizeof(RATE_DEFAULT_LEN[0])))

/* One scheduled note-on: armed when the step starts, emitted when due. */
typedef struct {
    int note, vel, at, dur, fired;
} stk_pending_t;

/* A note this module is currently sounding, so it can be stopped exactly. */
typedef struct {
    int note;       /* -1 = free */
    int ch;
    int off_at;     /* step-clock at which to release */
} stk_voice_t;
#define STK_MAX_VOICES 12

typedef struct {
    stk_prog_t prog;

    /* Sixteen stored buffers and which one is on screen (-1 = none). */
    stk_pattern_t pat[STK_PATTERNS];
    int pat_cur;

    /* --- edit / playback settings --- */
    int sel;            /* 1..count, the chord the knobs edit */
    int run;            /* free-run with the transport */
    int preview;        /* 0 off, 1 audition selected chord, 2 loop */
    int octave;         /* -2..+2 */
    int transpose;      /* -12..+12 semitones on everything that sounds; see played() */
    int velocity;
    int gate;           /* percent of `len` actually sounded */
    int lanes;          /* 0 scale-aware note lanes, 1 diatonic */
    int read_mode;      /* 0 replace, 1 append */

    /* --- transport --- */
    int clock_running;
    int pulse;          /* clocks since the progression started */
    int cur_step;       /* which chord is sounding */
    int last_emitted;   /* step whose note-ons have been sent, -1 = none */

    stk_voice_t voice[STK_MAX_VOICES];

    /*
     * PENDING NOTES. A chord no longer fires as one event: humanised timing
     * gives each note its own onset, so the step is ARMED and its notes are
     * emitted as their moments arrive. `off` rides the same mechanism, which
     * is why there is only one scheduler rather than one for offset and
     * another for humanise.
     */
    stk_pending_t pend[STK_MAX_PEND];
    int pend_n;

    /* --- humanise ---
     * SEEDED, NOT ROLLED PER PASS. The offsets are a pure function of (seed,
     * chord, voice), so the same take repeats until you press Randomize --
     * which is what makes it auditionable and what makes Stamp write the feel
     * you actually heard. Re-rolling every bar would be a different performance
     * on every loop and a third one in the clip. */
    int bars;           /* clip length for Read/Stamp, in bars */
    int defoct;         /* octave a newly filled slot starts in */
    int grouping;       /* voice grouping, whole progression, non-destructive */
    /*
     * THE CLIP FOLLOWS THE MUSIC UNTIL YOU SAY OTHERWISE.
     *
     * `bars` used only to GROW, which was written to avoid discarding a clip
     * length someone had chosen -- but a grow-only rule cannot tell YOUR
     * twelve bars from a leftover twelve. Browse the 12-bar blues, then browse
     * anything shorter, and you were left with four bars of music looping
     * three times in a stale clip.
     *
     * So it FITS, both directions, while nobody has expressed an opinion; the
     * moment you turn Clip Bars yourself that becomes the opinion and fitting
     * stops. Growing still happens even then, because silently truncating
     * music is never the better failure.
     */
    int bars_manual;
    /*
     * UNDO IS A RING OF WHOLE PROGRESSIONS, NOT A LOG OF EDITS.
     *
     * A 16-chord progression is 16 * sizeof(stk_chord_t) -- under a kilobyte --
     * so storing the STATE costs less than describing the change, and it
     * cannot drift: replaying an edit log requires every edit to have a
     * correct inverse, and this module has forty parameters that mutate the
     * buffer. Statically sized, so no allocation happens on the SPI callback.
     *
     * `depth` is how many states are behind you and `ahead` how many in front,
     * which is what makes REDO a separate count rather than a second stack.
     */
    /*
     * The snapshot is the progression AND the clip length. `bars` lives
     * outside stk_prog_t but Duplicate and Double Length both grow it, so a
     * history that stored only the chords put the notes back and left the clip
     * stretched -- an undo that half-undoes, which is worse than none because
     * you stop trusting the button.
     */
    stk_prog_t hist[STK_UNDO_DEPTH];
    int hist_bars[STK_UNDO_DEPTH];
    int hist_head;      /* ring index of the newest state */
    int hist_depth;     /* states available to undo */
    int hist_ahead;     /* states available to redo */
    int hist_busy;      /* set while undo/redo writes, so it does not capture */
    int last_tones[STK_MAX_TONES + 2];  /* the chord the sequencer just played, */
    int last_n;                         /* so "dynamic" has something to lead from */
    int preset;         /* index into PRESETS[],  the last genre one applied */
    int common;         /* index into COMMON[]  */
    int uncommon;       /* index into UNCOMMON[] */
    int swing;          /* 0-75 % delay on off-beat eighths */
    int hum_vel;        /* 0-100 % of velocity */
    int hum_time;       /* 0-100 % of an eighth */
    unsigned seed_vel;
    unsigned seed_time;

    /* --- pad override: a pad hit jumps to and holds that chord --- */
    int held_pad;       /* -1 = none */
    unsigned held_mask; /* which pitch classes are held from OUTSIDE, one bit each */

    /*
     * FORCED STEP -- one mechanism for two gestures. A held pad and a preview
     * audition both mean "sound THIS chord instead of the sequence", and they
     * differ only in what ends them: a pad ends on release (ticks < 0), an
     * audition on a countdown. Two separate paths drifted apart the first time
     * one of them learned about `gate`.
     */
    int force_step;     /* -1 = follow the sequence */
    /*
     * An audition ends in MUSICAL time, not after a fixed number of blocks.
     * It was 380 ticks (~1.1s), which is shorter than one bar at any tempo
     * below 218 BPM -- so a click played the first half of the figure and
     * stopped, and every rhythm longer than an eighth looked broken.
     */
    int force_until;    /* absolute pulse a one-shot audition runs to */
    /*
     * THE JOG CLICK IS A GATE, AND IT RUNS THE PROGRESSION.
     *
     * Held, playback starts at the selected chord and CARRIES ON -- into the
     * next chord, and round the clip -- until released. Looping the one chord
     * instead made the click an audition of a single stack, which is not what
     * you reach for when you want to hear whether the progression works.
     *
     * It is the sequencer, self-clocked: the same path the transport drives,
     * so every parameter applies and the module's own position advances --
     * which is what lets the playhead move with no transport running.
     */
    int hold_run;
    /*
     * WHAT IS CURRENTLY ARMED, as one identity rather than a step number: the
     * sequence, a held pad and an audition all arm chords, and telling them
     * apart by index alone cannot distinguish "step 2 again next bar" from
     * "still step 2". Sequence arms carry the ABSOLUTE step, so looping back
     * round to the same chord re-arms it; the other two use tagged negatives.
     */
    int armed_id;
    int armed_at;       /* pulse of the armed chord's nominal onset */

    /*
     * A CUT OWED TO THE NEXT TICK.
     *
     * set_param IS the SPI callback but is not the emitter -- only tick()
     * returns messages -- so "stop what is sounding" cannot be done where the
     * gesture arrives. Auditioning a second chord while the first was still
     * ringing simply layered them, and holding the click down through a
     * progression stacked every chord you passed. The flag is consumed once,
     * at the top of the next tick, before anything new is armed.
     */
    int cut_pending;

    /*
     * STAMP AUTO-STOP. After a stamp the clip holds the same notes this module
     * is still generating, so leaving Run on plays everything TWICE -- the
     * commonest way to be confused about which of the two you are hearing.
     *
     * The two stamp modes need opposite handling and that is the whole reason
     * this is a state machine rather than a `run = 0`:
     *   rec arm     Move must RECORD a pass, so Run has to stay on through
     *               exactly one lap and stop at the top of the next.
     *   write file  nothing is playing for the write, so it stops as soon as
     *               the worker reports success.
     */
    int stamp_once;      /* rec arm: stop when the progression wraps */
    int stamp_armed;     /* rec arm: waiting for Move's downbeat to begin */
    int at_downbeat;     /* Start seen, its first clock not yet counted */
    int stamp_run_prev;  /* what Run was before arming turned it off */
    int stamp_restore;   /* Run is owed back once the take's transport stops */
    /*
     * PARKED, STACKS PLAYS ONLY FOR A TAKE IT ARMED.
     *
     * Back parks the tool and the host keeps the DSP ticking, so a stamp
     * survives it: arm, Back, Record on Move, and the lap plays from the
     * background (Record also passes through with the tool on screen). But Move plays an injected note on whichever track is
     * SELECTED, not on a channel -- so a parked Stacks that goes on following
     * Move's transport, or looping its preview, lands on every track you
     * touch afterwards. Heard as "the other track plays the stamped notes
     * too". So parking switches Run and the loop off unless a take is armed
     * or under way, and resuming puts back what it found -- including a Run
     * the take itself parked, which used to return on the transport stop and
     * now returns when you come back to the screen that can see it.
     */
    int parked;
    int park_run_prev;
    int park_preview_prev;
    double pulse_accum; /* internal clock, for preview with the transport stopped */

    /* --- worker handshake (see the REALTIME note at the top) --- */
    pthread_t        worker;
    int              worker_live;
    atomic_int       quit;
    atomic_int       req_read;      /* bumped by set_param, consumed by worker */
    atomic_int       staging_seq;   /* release-stored by worker, acquired by tick */
    int              staging_applied;
    stk_prog_t       staging;       /* written by worker only, before the store */
    atomic_int       status;        /* 0 idle, 1 working, 2 ok, 3 failed */
    int              read_track;    /* the Move track (1-4) whose clip Read Clip imports */
    atomic_int       move_track;    /* Move's selected track 1-4, 0 unknown; worker-stored */
    int              move_track_seen;  /* the last one tick applied to read_track */
    /*
     * THE TOOL TAB'S LIVE SYNC (docs/MODULES.md "Remote UI for overtake
     * tools"). `rev` bumps on every snapshot-visible change -- a write, a
     * parse landing, a status transition -- and the manager re-reads `state`
     * only when it moved; `blocks` is the device clock the browser time-bases
     * its playhead on, in render blocks so it costs nothing to keep.
     */
    atomic_uint      rev;
    int              status_rev_seen;
    unsigned long    blocks;
    /*
     * PERSISTENCE. A chain component's `state` rode in Move's set; a tool has
     * no slot in the set, so Stacks keeps its own file -- the progression,
     * the sixteen patterns and every setting, i.e. exactly get_param("state").
     * The audio thread never touches the disk: the worker asks for a
     * snapshot (snap_req), tick serialises into snap_buf and releases
     * snap_seq, and the worker writes it out -- temp file plus rename, so a
     * crash mid-write leaves the previous file whole. Saved when the rev has
     * been quiet for STK_SAVE_QUIET_MS, and synchronously on destroy.
     */
    char             state_path[256];
    atomic_int       snap_req;      /* worker -> tick: please serialise */
    atomic_int       snap_seq;      /* tick -> worker: snap_buf is ready */
    int              snap_done;     /* tick's copy of what it has answered */
    unsigned         snap_rev;      /* the rev snap_buf describes */
    char             snap_buf[STK_STATE_MAX];
} stk_t;

/* ======================================================================
 * A minimal JSON reader, worker-thread only.
 *
 * Song.abl is ~100 KB of deeply nested object, and the house style elsewhere
 * (shadow_overlay.c) counts braces character by character. That works for a
 * key at a known depth and does not survive what this needs -- indexing into
 * tracks[], then clipSlots[], then notes[] -- so this is a real skipper: it
 * always consumes exactly one value, which is what makes "give me key K of
 * this object" reliable at any depth.
 * ====================================================================== */
static const char *js_ws(const char *p) {
    while (*p == ' ' || *p == '\t' || *p == '\n' || *p == '\r') p++;
    return p;
}

static const char *js_skip_string(const char *p) {
    if (*p != '"') return p;
    p++;
    while (*p) {
        if (*p == '\\' && p[1]) { p += 2; continue; }
        if (*p == '"') return p + 1;
        p++;
    }
    return p;
}

/* Consume exactly one JSON value and return the character after it. */
static const char *js_skip_value(const char *p) {
    p = js_ws(p);
    if (*p == '"') return js_skip_string(p);
    if (*p == '{' || *p == '[') {
        char open = *p, close = (open == '{') ? '}' : ']';
        int depth = 0;
        while (*p) {
            if (*p == '"') { p = js_skip_string(p); continue; }
            if (*p == open) depth++;
            else if (*p == close) { depth--; p++; if (depth == 0) return p; continue; }
            p++;
        }
        return p;
    }
    while (*p && *p != ',' && *p != '}' && *p != ']') p++;
    return p;
}

/*
 * Value of `key` in the object starting at `obj`, or NULL.
 * Only this object's OWN members -- it skips each value whole, so a key of the
 * same name nested inside a child cannot be mistaken for one of ours.
 */
static const char *js_member(const char *obj, const char *key) {
    obj = js_ws(obj);
    if (*obj != '{') return NULL;
    const char *p = obj + 1;
    size_t klen = strlen(key);
    for (;;) {
        p = js_ws(p);
        if (*p == '}' || *p == '\0') return NULL;
        if (*p != '"') return NULL;
        const char *name = p + 1;
        const char *after = js_skip_string(p);
        size_t nlen = (size_t)(after - name - 1);
        p = js_ws(after);
        if (*p != ':') return NULL;
        p++;
        const char *val = js_ws(p);
        if (nlen == klen && strncmp(name, key, klen) == 0) return val;
        p = js_skip_value(val);
        p = js_ws(p);
        if (*p == ',') { p++; continue; }
        return NULL;
    }
}

/* Element `idx` of the array at `arr`, or NULL. */
static const char *js_elem(const char *arr, int idx) {
    arr = js_ws(arr);
    if (*arr != '[') return NULL;
    const char *p = arr + 1;
    for (int i = 0; ; i++) {
        p = js_ws(p);
        if (*p == ']' || *p == '\0') return NULL;
        if (i == idx) return p;
        p = js_skip_value(p);
        p = js_ws(p);
        if (*p == ',') { p++; continue; }
        return NULL;
    }
}

static int js_count(const char *arr) {
    arr = js_ws(arr);
    if (*arr != '[') return 0;
    const char *p = arr + 1;
    int n = 0;
    for (;;) {
        p = js_ws(p);
        if (*p == ']' || *p == '\0') return n;
        n++;
        p = js_skip_value(p);
        p = js_ws(p);
        if (*p == ',') { p++; continue; }
        return n;
    }
}

static double js_num(const char *v, double dflt) {
    if (!v) return dflt;
    v = js_ws(v);
    if (*v != '-' && *v != '+' && (*v < '0' || *v > '9')) return dflt;
    return atof(v);
}

static int js_true(const char *v) {
    return v && strncmp(js_ws(v), "true", 4) == 0;
}

/* Copy a JSON string value into buf without its quotes. */
static void js_str(const char *v, char *buf, size_t buf_len) {
    buf[0] = '\0';
    if (!v) return;
    v = js_ws(v);
    if (*v != '"') return;
    v++;
    size_t i = 0;
    while (*v && *v != '"' && i + 1 < buf_len) {
        if (*v == '\\' && v[1]) v++;
        buf[i++] = *v++;
    }
    buf[i] = '\0';
}

/* ======================================================================
 * Naming a chord from a set of pitches
 *
 * The import has pitches; the staff needs a NAME. Both halves come from the
 * same search: every pitch class present is tried as the root against every
 * shape, and a shape matches only when its pitch-class set is EQUAL to the
 * chord's -- not a subset, or every triad would answer "maj9" for something
 * with the right three notes in it.
 *
 * A root that is also the bass wins outright (inversion 0). Otherwise the
 * bass tells us the inversion, which is what lets a first-inversion C major
 * read as "C" over E rather than as a rootless E-something.
 * ====================================================================== */
static uint16_t pc_mask(const int *notes, int n) {
    uint16_t m = 0;
    for (int i = 0; i < n; i++) m |= (uint16_t)(1u << (((notes[i] % 12) + 12) % 12));
    return m;
}

static uint16_t shape_mask(int root_pc, int shape) {
    uint16_t m = 0;
    for (int i = 0; i < SHAPES[shape].n; i++)
        m |= (uint16_t)(1u << ((root_pc + SHAPES[shape].iv[i]) % 12));
    return m;
}

static int popcount16(uint16_t v) {
    int c = 0;
    while (v) { c += (v & 1); v >>= 1; }
    return c;
}

/*
 * `notes` must be ascending. Writes root (a MIDI note near the bass), shape
 * and inversion. Always succeeds: an unrecognisable cluster falls back to the
 * bass note as a single "note", which is honest -- it draws and plays what was
 * actually there rather than asserting a chord nobody wrote.
 */
static void name_chord(const int *notes, int n, int *out_root, int *out_shape, int *out_inv) {
    int bass = notes[0];
    int bass_pc = ((bass % 12) + 12) % 12;
    uint16_t want = pc_mask(notes, n);
    int want_n = popcount16(want);

    int best_shape = -1, best_root_pc = bass_pc, best_inv = 0, best_score = -1;

    for (int rpc = 0; rpc < 12; rpc++) {
        if (!(want & (1u << rpc))) continue;          /* root must be sounding */
        for (int s = 0; s < NUM_SHAPES; s++) {
            if (SHAPES[s].n != want_n) continue;
            if (shape_mask(rpc, s) != want) continue;

            /* Inversion = how far up the shape the BASS sits. */
            int inv = 0;
            for (int i = 0; i < SHAPES[s].n; i++) {
                if ((rpc + SHAPES[s].iv[i]) % 12 == bass_pc) { inv = i; break; }
            }
            /* Prefer root position, then the earliest shape in the table --
             * which orders triads before their extended spellings. */
            int score = (inv == 0 ? 1000 : 0) + (NUM_SHAPES - s);
            if (score > best_score) {
                best_score = score; best_shape = s; best_root_pc = rpc; best_inv = inv;
            }
        }
    }

    if (best_shape < 0) {
        *out_root = bass;
        *out_shape = SHAPE_SINGLE;
        *out_inv = 0;
        return;
    }

    /* Put the root in the octave that keeps the chord where it was played. */
    int root = bass - ((bass_pc - best_root_pc + 12) % 12);
    while (root < 12) root += 12;
    while (root > 115) root -= 12;

    *out_root = root;
    *out_shape = best_shape;
    *out_inv = best_inv;
}

/* ======================================================================
 * Building the sounding notes of one chord
 * ====================================================================== */
/*
 * COLOUR IS VOICING. ADDING A DIATONIC NOTE IS *SHAPE*.
 *
 * Colour was a density ladder -- triad, 7th, 9th -- and every rung of it
 * produced a chord that ALREADY EXISTS in SHAPES[]: maj7, add9, maj9, 11, 13,
 * 6/9 and the altered dominants are all there. So it was a second, slower
 * route to Shape, and two controls writing one musical fact is exactly the
 * confusion this module keeps having to untangle.
 *
 * What Shape cannot express is how a chord is ARRANGED: the same notes close
 * or open, dropped, without their fifth, without their root, restacked in
 * fourths. Same chord, different colour -- and no other control can say it.
 * So Colour is that axis and only that axis, and the old `Spread` knob is
 * absorbed into it rather than sitting beside it meaning half the same thing.
 *
 * It applies in TWO STAGES, because the two halves need different information.
 * Omissions and restacking are decided on the INTERVALS, where the module
 * still knows which voice is the fifth and which the root; spacing is decided
 * on the sounding NOTES, after the root and inversion have been applied. Doing
 * either in the other place is how "drop 2" starts depending on which knob was
 * turned first.
 */
enum { COLOUR_CLOSE = 0, COLOUR_OPEN, COLOUR_DROP2, COLOUR_DROP3,
       COLOUR_DROP24, COLOUR_SHELL, COLOUR_ROOTLESS, COLOUR_QUARTAL,
       COLOUR_SPREAD, COLOUR_CLUSTER };
static const char *const COLOURS[] = {
    "close", "open", "drop2", "drop3", "drop2+4",
    "shell", "rootless", "quartal", "spread", "cluster" };
#define NUM_COLOURS ((int)(sizeof(COLOURS) / sizeof(COLOURS[0])))


/*
 * COLOUR, STAGE A -- what is left OUT, and what is restacked.
 *
 * Decided on the INTERVALS, where the module still knows which voice is the
 * fifth and which is the root; spacing (stage B) is decided later on the
 * sounding notes. A dyad is left alone whatever is asked of it: there is no
 * fifth to drop and no root to spare.
 */
/*
 * VOICE GROUPING -- how the chords sit AGAINST EACH OTHER, and where.
 *
 * Colour arranges ONE chord. This is the axis above it: voice leading from the
 * chord before, and the register the whole progression lives in. That is why
 * it is a MAIN control with progression scope and not a per-chord one -- half
 * of what it does is meaningless without the previous chord.
 *
 * NON-DESTRUCTIVE, and by construction rather than by promise: it is applied
 * inside `chord_notes`, so the roots and shapes in the buffer never change and
 * `none` restores exactly what was there. That placement is also what makes
 * STAMP respect it -- the stamp builder, the sequencer and the staff all
 * resolve notes through this one function, so there is no fourth path that
 * could write a clip you never heard.
 */
enum { GRP_NONE = 0, GRP_DYN, GRP_DYN1, GRP_DYN2,
       GRP_C1B2, GRP_C2B3, GRP_C3B4,
       GRP_OPEN1, GRP_OPEN2, GRP_OPEN3, GRP_GUITAR,
       GRP_DROP2, GRP_DROP3, GRP_DROP4, GRP_DROP23, GRP_DROP24 };
static const char *const GROUPINGS[] = {
    "none", "dynamic", "dyn +1oct", "dyn +2oct",
    "C1-B2", "C2-B3", "C3-B4",
    "open 1", "open 2", "open 3", "guitar",
    "drop 2", "drop 3", "drop 4", "drop 2+3", "drop 2+4" };
#define NUM_GROUPINGS ((int)(sizeof(GROUPINGS) / sizeof(GROUPINGS[0])))

/* Total distance from each voice to the nearest voice of the chord before --
 * the thing "dynamic" minimises. */
static int lead_cost(const int *a, int an, const int *prev, int pn) {
    int cost = 0;
    for (int i = 0; i < an; i++) {
        int best = 9999;
        for (int j = 0; j < pn; j++) {
            int d = a[i] - prev[j]; if (d < 0) d = -d;
            if (d < best) best = d;
        }
        cost += best;
    }
    return cost;
}

static void fold_into(int *out, int n, int lo, int hi) {
    for (int i = 0; i < n; i++) {
        while (out[i] < lo) out[i] += 12;
        while (out[i] > hi) out[i] -= 12;
    }
}

static void apply_grouping(int *out, int n, int grp, const int *prev, int pn) {
    if (grp <= GRP_NONE || n < 1) return;

    switch (grp) {
    case GRP_DYN: case GRP_DYN1: case GRP_DYN2: {
        /*
         * Pick the octave placement and inversion that moves least from the
         * chord before. With no previous chord there is nothing to lead from,
         * so the voicing is left alone rather than guessed at.
         *
         * THE LIFT COMES FIRST, and that ordering is the whole correctness of
         * the +1/+2 variants. Lifting AFTER the leading made every chord lead
         * from an already-lifted neighbour and then get lifted again, so the
         * progression climbed away: measured 81 -> 104 -> 122 -> 117, with a
         * total movement of 105 against 23 for no grouping at all. Lifting
         * first puts only the FIRST chord in the new register; the rest follow
         * it there, because that is what leading from it means.
         */
        int lift = (grp == GRP_DYN1) ? 12 : (grp == GRP_DYN2) ? 24 : 0;
        if (lift && (!prev || pn <= 0))
            for (int i = 0; i < n; i++) out[i] += lift;
        if (prev && pn > 0) {
            int best[STK_MAX_TONES + 2], bestc = -1, cur[STK_MAX_TONES + 2];
            for (int rot = 0; rot < n; rot++) {
                for (int oct = -2; oct <= 2; oct++) {
                    for (int i = 0; i < n; i++)
                        cur[i] = out[(i + rot) % n] + 12 * oct
                               + ((i + rot) >= n ? 12 : 0);
                    int c = lead_cost(cur, n, prev, pn);
                    if (bestc < 0 || c < bestc) {
                        bestc = c;
                        for (int i = 0; i < n; i++) best[i] = cur[i];
                    }
                }
            }
            if (bestc >= 0) for (int i = 0; i < n; i++) out[i] = best[i];
        }
        break;
    }
    /* Two-octave registers, named by the notes they span. */
    case GRP_C1B2: fold_into(out, n, 24, 47); break;
    case GRP_C2B3: fold_into(out, n, 36, 59); break;
    case GRP_C3B4: fold_into(out, n, 48, 71); break;

    /* Progressively wider spacing: 1 lifts the top voice, 2 every other, 3
     * gives each voice its own octave. */
    case GRP_OPEN1: if (n >= 2) out[n - 1] += 12; break;
    case GRP_OPEN2: for (int i = 1; i < n; i += 2) out[i] += 12; break;
    case GRP_OPEN3: for (int i = 1; i < n; i++) out[i] += 12 * i; break;

    case GRP_GUITAR:
        /* A guitar spreads the low voices and stacks the top ones: the bass
         * note sits alone, the rest gather above it. */
        if (n >= 3) {
            out[1] += 12;
            for (int i = 2; i < n; i++) out[i] += 12;
        }
        break;

    case GRP_DROP2:  if (n >= 2) out[n - 2] -= 12; break;
    case GRP_DROP3:  if (n >= 3) out[n - 3] -= 12; break;
    case GRP_DROP4:  if (n >= 4) out[n - 4] -= 12; break;
    case GRP_DROP23: if (n >= 3) { out[n - 2] -= 12; out[n - 3] -= 12; } break;
    case GRP_DROP24: if (n >= 4) { out[n - 2] -= 12; out[n - 4] -= 12; } break;
    default: break;
    }
    for (int i = 0; i < n; i++) {
        if (out[i] < 0) out[i] = 0;
        if (out[i] > 127) out[i] = 127;
    }
}

static int chord_notes_g(const stk_chord_t *c, int octave, int snap_scale,
                         int snap_key, int st_colour, int grp,
                         const int *prev, int pn, int *out, int max_out);
static int coloured_intervals(const stk_shape_t *sh, int styled, int st_colour,
                              int *iv) {
    int n = 0;
    for (int i = 0; i < sh->n && n < STK_MAX_TONES; i++) iv[n++] = sh->iv[i];
    if (sh->n <= 1 || styled) return n;

    if (st_colour == COLOUR_SHELL && n >= 3) {
        /* Drop the fifth: it carries the least information in a seventh
         * chord, which is why comping pianists leave it out. */
        int t = 0;
        for (int i = 0; i < n; i++) {
            int pc = iv[i] % 12;
            if (pc == 6 || pc == 7 || pc == 8) continue;
            iv[t++] = iv[i];
        }
        if (t >= 2) n = t;
    } else if (st_colour == COLOUR_ROOTLESS && n >= 3) {
        /* Drop the root -- the bass has it, the hand does not. */
        int t = 0;
        for (int i = 0; i < n; i++) if (iv[i] % 12 != 0) iv[t++] = iv[i];
        if (t >= 2) n = t;
    } else if (st_colour == COLOUR_QUARTAL) {
        /* Restack in fourths, keeping the chord's own third on top so a minor
         * chord stays audibly minor -- only the spacing becomes quartal. */
        int third = -1;
        for (int i = 0; i < n; i++) {
            int pc = iv[i] % 12;
            if (pc == 3 || pc == 4) { third = pc; break; }
        }
        n = 0;
        iv[n++] = 0; iv[n++] = 5; iv[n++] = 10;
        if (third >= 0) iv[n++] = third + 12;
    }
    return n;
}

/*
 * THE ROOT THAT SOUNDS, in one place: the written root, snapped to the scale
 * unless the chord is `exact`, then transposed. The sequencer builds from it
 * and the staff names the chord by it, so the caption cannot name a root the
 * synth did not play -- which it did while the caption read `c->root`.
 *
 * Snap the ROOT to the scale, never the chord tones: a major third forced
 * into a minor scale stops being the chord the name promises. The scale
 * chooses which roots a DEGREE can reach; the shape decides what sits on top.
 */
static int sounding_root(const stk_chord_t *c, int octave, int snap_scale, int snap_key) {
    int root = c->root + octave * 12;
    if (!c->exact && snap_scale > 0 && snap_scale < NUM_SCALES) {
        uint16_t mask = SCALES[snap_scale].mask;
        int pc = (((root - snap_key) % 12) + 12) % 12;
        if (!(mask & (1u << pc))) {
            for (int d = 1; d <= 6; d++) {
                if (mask & (1u << ((pc + d) % 12))) { root += d; break; }
                if (mask & (1u << ((pc - d + 12) % 12))) { root -= d; break; }
            }
        }
    }
    return root + c->trans;   /* transpose the whole chord, shape intact */
}

static int chord_notes_g(const stk_chord_t *c, int octave, int snap_scale, int snap_key,
                         int st_colour, int grp, const int *prev, int pn,
                         int *out, int max_out) {
    if (!c || c->shape < 0 || c->shape >= NUM_SHAPES) return 0;
    const stk_shape_t *sh = &SHAPES[c->shape];

    int root = sounding_root(c, octave, snap_scale, snap_key);

    /*
     * COLOUR reshapes every chord to one density, without touching what the
     * progression WROTE. Applied to the intervals, so it is reversible: set it
     * back to `written` and the library's own qualities return.
     *
     * Derived from the interval set rather than from a per-shape table. With
     * 38 shapes a table is 38 chances to be wrong, and the rule is simple
     * enough to state: a triad is the root, the third and the fifth; a seventh
     * is a triad plus a seventh chosen by the third it already has; a ninth is
     * that plus a ninth. Anything already carrying a seventh keeps its own --
     * so a dom7 thickened to a ninth becomes a 9, not a maj9, which is the
     * whole reason not to rebuild from scratch.
     */
    int iv[STK_MAX_TONES + 2];
    int n = coloured_intervals(sh, c->styled, st_colour, iv);

    for (int i = 0; i < n && i < max_out; i++) out[i] = root + iv[i];
    if (n > max_out) n = max_out;

    /*
     * SPREAD is applied before inversion, because it changes which voice is
     * lowest and inversion rotates from the bottom. Doing it after would make
     * "drop2 + 1st inversion" mean something different depending on the order
     * two independent knobs happened to be turned.
     *
     * drop2 / drop3 take the 2nd / 3rd voice from the TOP down an octave --
     * the standard arranger's voicings -- so they need at least that many
     * voices and are a no-op on a dyad rather than an error.
     */
    /* STAGE B -- spacing, on the sounding notes. */
    switch (st_colour) {
    case COLOUR_OPEN:                           /* lift alternate voices */
        for (int i = 1; i < n; i += 2) out[i] += 12;
        break;
    case COLOUR_DROP2:
        if (n >= 2) out[n - 2] -= 12;
        break;
    case COLOUR_DROP3:
        if (n >= 3) out[n - 3] -= 12;
        break;
    case COLOUR_DROP24:
        if (n >= 2) out[n - 2] -= 12;
        if (n >= 4) out[n - 4] -= 12;
        break;
    case COLOUR_SPREAD:                         /* one octave per voice up */
        for (int i = 1; i < n; i++) out[i] += 12 * ((i + 1) / 2);
        break;
    case COLOUR_CLUSTER:
        /* Pack every voice into ONE octave above the lowest. Voices can
         * collide there; the note builder already drops duplicates, so a
         * cluster legitimately sounds thinner than it is wide. */
        for (int i = 1; i < n; i++) {
            while (out[i] - out[0] >= 12) out[i] -= 12;
            while (out[i] < out[0]) out[i] += 12;
        }
        break;
    default:                                    /* close, and the stage-A ones */
        break;
    }

    /*
     * INVERSION IS ONE LADDER THROUGH ZERO, NOT TWO HALVES THAT MIRROR.
     *
     * Up rotates the LOWEST voice an octave up; down rotates the HIGHEST voice
     * an octave down. Each detent moves exactly one voice, so sweeping -3..+3
     * walks the voicing steadily and lands an octave below and above root
     * position at the ends.
     *
     * It used to do |inv| UPWARD rotations and then drop the whole chord an
     * octave, which is a different thing wearing the same name. On a triad
     * that made -1 and -2 the voicings of +1 and +2 an octave down -- so the
     * two halves mirrored instead of continuing -- and made -3 BYTE-IDENTICAL
     * to inv=0, a wasted position in the middle of a seven-step control:
     *
     *      -3  48 52 55   <- the same chord as 0
     *      -2  43 48 52       (the +2 voicing, moved down)
     *      -1  40 43 48       (the +1 voicing, moved down)
     *       0  48 52 55
     */
    int inv = c->inv;
    for (int k = 0; k < inv && n > 0; k++) {
        int lowest = 0;
        for (int i = 1; i < n; i++) if (out[i] < out[lowest]) lowest = i;
        out[lowest] += 12;
    }
    for (int k = 0; k > inv && n > 0; k--) {
        int highest = 0;
        for (int i = 1; i < n; i++) if (out[i] > out[highest]) highest = i;
        out[highest] -= 12;
    }

    for (int i = 0; i < n; i++) {
        if (out[i] < 0) out[i] = 0;
        if (out[i] > 127) out[i] = 127;
    }

    /*
     * GROUPING LAST, because it is the only stage that looks OUTSIDE this
     * chord -- it needs the finished voicing to lead from the one before, and
     * the register folds have to bound whatever Colour and inversion produced.
     */
    apply_grouping(out, n, grp, prev, pn);

    /*
     * ASCENDING, ALWAYS. Inverting rotates a voice up an octave in place, so
     * the array comes out of that loop in voicing order rather than pitch
     * order -- and the staff draws noteheads bottom-up from this same array.
     * Sorting here rather than in canvas.js keeps one answer to "what does
     * this chord sound like"; an insertion sort of at most six ints.
     */
    for (int i = 1; i < n; i++) {
        int v = out[i], j = i - 1;
        while (j >= 0 && out[j] > v) { out[j + 1] = out[j]; j--; }
        out[j + 1] = v;
    }
    return n;
}

static int clampi(int v, int lo, int hi) { return v < lo ? lo : (v > hi ? hi : v); }

/* ======================================================================
 * Humanise
 *
 * A HASH, NOT A RANDOM NUMBER GENERATOR. The deviation for one voice is a pure
 * function of (seed, chord, voice index), so it is identical on every pass,
 * survives a preset save as two integers rather than a table of offsets, and
 * -- the point -- makes the clip that Stamp writes the same performance the
 * preview played. A per-pass rand() cannot promise any of that.
 * ====================================================================== */
static unsigned hash3(unsigned a, unsigned b, unsigned c) {
    unsigned x = a * 0x9E3779B1u ^ (b + 0x165667B1u) * 0x85EBCA6Bu
               ^ (c + 0x27D4EB2Fu) * 0xC2B2AE35u;
    x ^= x >> 15; x *= 0x2C1B3C6Du;
    x ^= x >> 12; x *= 0x297A2D39u;
    x ^= x >> 15;
    return x;
}

/* -1000..+1000, i.e. -1.0..+1.0 in thousandths. */
static int hash_bipolar(unsigned seed, int chord, int voice) {
    return (int)(hash3(seed, (unsigned)chord, (unsigned)voice) % 2001u) - 1000;
}

static int humanised_velocity(const stk_t *st, int chord, int voice) {
    int v = st->velocity;
    if (st->hum_vel > 0) {
        int r = hash_bipolar(st->seed_vel, chord, voice);
        v += (int)((long)st->velocity * r * st->hum_vel / 100000L);
    }
    return clampi(v, 1, 127);
}

/*
 * WHY THIS MODULE DOES NOT PRESS MOVE'S RECORD BUTTON.
 *
 * It was tried, and the attempt is worth recording so it is not tried again.
 * The reasoning was sound: the only way a clip reaches the LIVE set is Move's
 * own recorder, because Move holds the set in memory and autosaves over the
 * file -- measured, a spliced Song.abl was overwritten 142 seconds later with
 * nothing touched. So writing the file cannot win, and pressing record can.
 *
 * The injection works. `midi_inject_to_move` accepts { 0x0B, 0xB0, 118, 127 }
 * on cable 0 -- byte-identical to the track tap the shim itself injects at
 * schwung_shim.c -- and the shim logs "MIDI inject: drained 2 pkts", so the
 * packets reach MIDI_IN.
 *
 * WHAT DEFEATED IT WAS THE CC NUMBER. 118 is SAMPLE, not Record -- Record is
 * 86 (docs/API.md; Schwung's rec_arm.h measured it on Move's own LED,
 * 2026-09-12) -- and the shim uses 118 for its sampler gestures, which is why
 * the injected press opened the Quantized Sampler's fullscreen screen. An
 * injected CC 86 has NOT been tried; it is the experiment to run before
 * concluding that a module cannot start Move's recorder.
 *
 * Until then the lap is ARMED and Move's transport starts it (see the clock
 * handler). That costs one button press on the hardware and works.
 */

/*
 * HOW LOUD ONE NOTE IS, in one place.
 *
 * humanised_velocity() answers a DEVIATION around the global velocity; it does
 * not know about the chord's own offset. Playback added that back
 * (`base_vel + (humanised - velocity)`) and the STAMP did not -- it wrote
 * humanised_velocity() straight out, so a chord set to Chord Vel -63 played
 * quiet and stamped at full level. That contradicts the one thing Stamp
 * promises, that the clip is the take Preview played, and it is invisible
 * until the clip is heard later without the module.
 *
 * Both callers ask here now, and the wire does too, so the three cannot drift.
 */
/*
 * PER HIT, NOT PER CHORD. A rhythm figure strikes a chord up to sixteen times
 * a bar, and the deviation used to be hashed on (chord, voice) alone -- so
 * every hit of the bar got the identical offset and the identical velocity,
 * and "humanise" on a 16th figure was a constant lean repeated sixteen times,
 * which is the opposite of a human. The hit index joins the hash; hit 0 hashes
 * exactly as before, so a saved seed still plays the same first strike.
 */
static int note_velocity(const stk_t *st, const stk_chord_t *c, int chord, int voice, int hit) {
    int base = clampi(st->velocity + c->vel, 1, 127);
    return clampi(base + (humanised_velocity(st, chord, voice + 16 * hit) - st->velocity), 1, 127);
}

/* Deviation in CLOCKS, both directions. The caller clamps the resulting onset
 * so a chord can never be dragged in front of its own step. */
static int humanised_delay(const stk_t *st, int chord, int voice, int hit, int eighth) {
    if (st->hum_time <= 0) return 0;
    int r = hash_bipolar(st->seed_time, chord, voice + 64 + 16 * hit);
    return (int)((long)eighth * r * st->hum_time / 100000L);
}

/* ======================================================================
 * Reading the playing clip out of Song.abl        [worker thread only]
 * ====================================================================== */
typedef struct { int note; double start; double dur; } stk_note_t;

static int note_cmp(const void *a, const void *b) {
    const stk_note_t *x = (const stk_note_t *)a, *y = (const stk_note_t *)b;
    if (x->start < y->start) return -1;
    if (x->start > y->start) return  1;
    return x->note - y->note;
}

/*
 * Newest Song.abl under Sets/<uuid>/<name>/. Same construction as
 * shadow_read_set_mute_states: the set NAME is not known here, so the most
 * recently written file is the loaded set.
 */
static int find_song_abl(char *out, size_t out_len) {
    DIR *sets = opendir(SETS_DIR);
    if (!sets) return 0;
    char best[512] = "";
    time_t best_mtime = 0;
    struct dirent *uuid_ent;
    while ((uuid_ent = readdir(sets)) != NULL) {
        if (uuid_ent->d_name[0] == '.') continue;
        char uuid_path[512];
        snprintf(uuid_path, sizeof(uuid_path), "%s/%s", SETS_DIR, uuid_ent->d_name);
        DIR *inner = opendir(uuid_path);
        if (!inner) continue;
        struct dirent *name_ent;
        while ((name_ent = readdir(inner)) != NULL) {
            if (name_ent->d_name[0] == '.') continue;
            char path[1024];
            snprintf(path, sizeof(path), "%s/%s/Song.abl", uuid_path, name_ent->d_name);
            struct stat st;
            if (stat(path, &st) == 0 && S_ISREG(st.st_mode) && st.st_mtime > best_mtime) {
                best_mtime = st.st_mtime;
                snprintf(best, sizeof(best), "%s", path);
            }
        }
        closedir(inner);
    }
    closedir(sets);
    if (!best[0]) return 0;
    snprintf(out, out_len, "%s", best);
    return 1;
}

static char *slurp(const char *path, long *out_len) {
    FILE *f = fopen(path, "rb");
    if (!f) return NULL;
    if (fseek(f, 0, SEEK_END) != 0) { fclose(f); return NULL; }
    long n = ftell(f);
    if (n <= 0 || n > 32L * 1024 * 1024) { fclose(f); return NULL; }
    rewind(f);
    char *buf = (char *)malloc((size_t)n + 1);
    if (!buf) { fclose(f); return NULL; }
    /*
     * A SHORT READ MUST NEVER LOOK LIKE A COMPLETE ONE. The quantized
     * sampler's preroll trim spent months silently truncating takes because a
     * failed read fell through to the success path (CLAUDE.md); a half-read
     * Song.abl here would parse as a set with fewer tracks and quietly import
     * the wrong clip.
     */
    size_t got = fread(buf, 1, (size_t)n, f);
    fclose(f);
    if (got != (size_t)n) { free(buf); return NULL; }
    buf[n] = '\0';
    *out_len = n;
    return buf;
}

/*
 * Fill `out` from the clip playing on `track` (0-based). Returns the number of
 * chords found, or -1 if the file, the track or a clip could not be read --
 * which the caller reports as FAILED rather than as "the clip is empty".
 * A read that did not complete must never produce a plan (CLAUDE.md).
 */
static int read_clip_chords(int track, stk_prog_t *out) {
    char path[1024];
    if (!find_song_abl(path, sizeof(path))) return -1;

    long len = 0;
    char *json = slurp(path, &len);
    if (!json) return -1;

    int rc = -1;
    const char *tracks = js_member(json, "tracks");
    if (!tracks) goto done;

    /* The set's own key and scale seed the selector -- Move already knows the
     * answer, so asking the user to restate it is a way to get it wrong. */
    out->key   = ((int)js_num(js_member(json, "rootNote"), 0) % 12 + 12) % 12;
    char scale_name[32];
    js_str(js_member(json, "scale"), scale_name, sizeof(scale_name));
    out->scale = scale_index_from_name(scale_name);

    const char *tr = js_elem(tracks, track);
    if (!tr) goto done;
    const char *slots = js_member(tr, "clipSlots");
    if (!slots) goto done;

    /* The PLAYING clip, else the first clip present -- so Stamp/Read still
     * work with the transport stopped, which is when you are editing. */
    const char *clip = NULL, *fallback = NULL;
    int nslots = js_count(slots);
    for (int i = 0; i < nslots; i++) {
        const char *slot = js_elem(slots, i);
        if (!slot) break;
        const char *c = js_member(slot, "clip");
        if (!c || *js_ws(c) != '{') continue;
        if (!fallback) fallback = c;
        if (js_true(js_member(c, "isPlaying"))) { clip = c; break; }
    }
    if (!clip) clip = fallback;
    if (!clip) goto done;

    const char *notes = js_member(clip, "notes");
    if (!notes) goto done;
    int nn = js_count(notes);
    if (nn <= 0) { out->count = 0; rc = 0; goto done; }
    if (nn > 4096) nn = 4096;

    stk_note_t *ns = (stk_note_t *)calloc((size_t)nn, sizeof(stk_note_t));
    if (!ns) goto done;
    int got = 0;
    for (int i = 0; i < nn; i++) {
        const char *e = js_elem(notes, i);
        if (!e) break;
        int nnum = (int)js_num(js_member(e, "noteNumber"), -1);
        if (nnum < 0 || nnum > 127) continue;
        ns[got].note  = nnum;
        ns[got].start = js_num(js_member(e, "startTime"), 0.0);
        ns[got].dur   = js_num(js_member(e, "duration"), 0.25);
        got++;
    }
    if (got <= 0) { free(ns); out->count = 0; rc = 0; goto done; }
    qsort(ns, (size_t)got, sizeof(stk_note_t), note_cmp);

    /*
     * SIMULTANEITY IS THE CHORD. Notes within a 32nd of each other are one
     * stack; anything later starts the next. A strummed or humanised clip
     * would otherwise arrive as a run of one-note "chords".
     */
    const double EPS = 0.125;
    double onset[STK_MAX_CHORDS];
    double dur[STK_MAX_CHORDS];
    int    pitches[STK_MAX_CHORDS][STK_MAX_TONES];
    int    npitch[STK_MAX_CHORDS];
    int    groups = 0;

    for (int i = 0; i < got && groups < STK_MAX_CHORDS; ) {
        double t = ns[i].start;
        onset[groups] = t;
        dur[groups]   = ns[i].dur;
        npitch[groups] = 0;
        while (i < got && ns[i].start - t < EPS) {
            if (npitch[groups] < STK_MAX_TONES)
                pitches[groups][npitch[groups]++] = ns[i].note;
            if (ns[i].dur > dur[groups]) dur[groups] = ns[i].dur;
            i++;
        }
        groups++;
    }
    free(ns);
    if (groups <= 0) { out->count = 0; rc = 0; goto done; }

    /*
     * PLACED AS DURATIONS, IN EIGHTHS OF A BAR -- the model the sequencer
     * plays, where chord k begins where chord k-1 ends.
     *
     * This used to lay the clip on a STEP grid inferred from the first gap and
     * count `len` in eighths of that step, which was the pre-2026-09 model:
     * once `len` became an absolute eighth of a bar, a clip of two-bar chords
     * (step = 2 bars, so `len` 8 = "one step") came back as one-bar chords
     * and an eight-bar clip imported as four; anything not on a uniform grid
     * landed on the wrong bar, and `len` was still clamped at 16 against a
     * range of 64. Measured 2026-09-19, tests/test_stacks_read_clip.sh.
     *
     * So: a chord's slot runs from its onset to the next chord's onset, and
     * the last runs for as long as it sounds. Only the FIRST chord can carry
     * an offset -- every later slot starts exactly where its chord does, so
     * the offset is the whole slot's and nothing is lost to it.
     */
    const double unit_beats = 4.0 / UNITS_PER_BAR;   /* an eighth of a 4/4 bar */
    int start_u[STK_MAX_CHORDS + 1];
    for (int g = 0; g < groups; g++)
        start_u[g] = (int)floor(onset[g] / unit_beats + 0.5);
    start_u[groups] = start_u[groups - 1]
                    + (int)floor(dur[groups - 1] / unit_beats + 0.5);
    int first_off = clampi(start_u[0], 0, 15);
    for (int g = 0; g < groups; g++) {
        int root, shape, inv;
        name_chord(pitches[g], npitch[g], &root, &shape, &inv);
        int ln = start_u[g + 1] - start_u[g];
        if (g == 0) ln += first_off;              /* the first slot starts at zero */
        out->ch[g].root  = root;
        out->ch[g].shape = shape;
        out->ch[g].inv   = inv;
        out->ch[g].exact = 1;      /* the clip held these notes; the scale may not move them */
        out->ch[g].len   = clampi(ln, 1, STK_MAX_LEN);
        out->ch[g].off   = g == 0 ? first_off : 0;
    }
    /* Rate is only what a NEW chord gets; the clip's own slot length is the
     * natural choice, when it is one of the four. */
    out->rate = 1;
    for (int r = 0; r < NUM_RATES; r++)
        if (RATE_DEFAULT_LEN[r] == out->ch[0].len) { out->rate = r; break; }
    out->count = groups;
    rc = groups;

done:
    free(json);
    return rc;
}

/*
 * There is no file stamp. Move holds the set in memory and autosaves over
 * the file -- measured: a spliced Song.abl was overwritten 142 seconds later
 * with nothing touched -- and Schwung's rule is to work within Move's
 * workflow rather than override it. The one way a clip reaches the live set
 * is Move's own recorder: Stamp ARMS, the user presses Record, and the lap
 * plays for Move to capture (see the clock handler). The splice-and-rename
 * writer that lived here until 2026-09-15 is in git if it is ever wanted.
 */

/* ======================================================================
 * Persistence   [worker thread and load/unload only -- never the audio thread]
 * ====================================================================== */
static int state_file_write(const char *path, const char *json) {
    if (!path[0] || !json || !json[0]) return 0;
    char tmp[300];
    snprintf(tmp, sizeof tmp, "%s.tmp", path);
    FILE *f = fopen(tmp, "wb");
    if (!f) return 0;
    size_t n = strlen(json);
    int ok = fwrite(json, 1, n, f) == n && fputc('\n', f) != EOF;
    ok = (fclose(f) == 0) && ok;
    if (!ok) { unlink(tmp); return 0; }
    if (rename(tmp, path) != 0) { unlink(tmp); return 0; }
    return 1;
}

/* Restore from the file, if there is one. Runs in create_instance, before
 * the worker exists and before any callback can. A file this build cannot
 * parse is left alone and ignored: set_param("state") refuses a version it
 * does not know rather than half-applying it. */
static void state_file_load(stk_t *st) {
    long len = 0;
    char *json = slurp(st->state_path, &len);
    if (!json) return;
    if (len > 0 && len < STK_STATE_MAX) stk_set_param_inner(st, "state", json);
    free(json);
    stk_log("stacks: state restored");
}

/* ======================================================================
 * The worker
 * ====================================================================== */
static stk_prog_t g_req_base;   /* snapshot taken on the RT side, for append */

static void *stk_worker(void *arg) {
    stk_t *st = (stk_t *)arg;
    int last_read = 0;
    /* Zero, not "the rev now": pthread_create returns before this line runs,
     * and an edit made in that gap would count as already saved. The rev IS
     * zero at create -- the restore goes through the inner setter -- so zero
     * means "nothing since the file was read". */
    unsigned saved_rev = 0;
    unsigned quiet_rev = 0;
    int quiet_ticks = 0;            /* 100ms each */
    int snap_asked = 0, snap_seen = 0;

    while (!atomic_load(&st->quit)) {
        /*
         * SAVE ON QUIET. The rev moves on every write; a jog spin is dozens
         * of them a second. So the file is written once the rev has held
         * still for STK_SAVE_QUIET_MS -- one save per gesture, not per
         * detent -- and only if it moved since the last save.
         */
        {
            unsigned rev = atomic_load_explicit(&st->rev, memory_order_relaxed);
            if (rev != quiet_rev) { quiet_rev = rev; quiet_ticks = 0; }
            else if (quiet_ticks < 1000000) quiet_ticks++;
            if (rev != saved_rev && quiet_ticks * 100 >= STK_SAVE_QUIET_MS
                && snap_asked == snap_seen) {
                snap_asked++;
                atomic_store_explicit(&st->snap_req, snap_asked, memory_order_release);
            }
            int got = atomic_load_explicit(&st->snap_seq, memory_order_acquire);
            if (got != snap_seen) {
                snap_seen = got;
                if (state_file_write(st->state_path, st->snap_buf)) saved_rev = st->snap_rev;
                else stk_log("stacks: state save failed");
            }
        }

        /*
         * WHICH TRACK MOVE HAS SELECTED, asked HERE and not in create or
         * tick: the first move_info_read() resolves the host's export with
         * dlsym (the loader's lock) or, on a host without it, tries
         * shm_open -- neither belongs on the SPI callback. Published as a
         * plain 1-4 for tick to apply; 0 is "unknown" (an older Schwung, a
         * set loading), which leaves Read Track to the knob.
         */
        {
            move_info_t mi;
            int t = 0;
            if (move_info_read(&mi) && mi.valid
                && mi.selected_track >= 0 && mi.selected_track < 4)
                t = mi.selected_track + 1;
            atomic_store_explicit(&st->move_track, t, memory_order_relaxed);
        }

        int r = atomic_load(&st->req_read);
        if (r != last_read) {
            last_read = r;
            atomic_store(&st->status, 1);
            stk_prog_t got;
            memset(&got, 0, sizeof(got));
            int n = read_clip_chords(st->read_track - 1, &got);
            if (n < 0) {
                /* A read that did not complete is NOT "the clip is empty". */
                atomic_store(&st->status, 3);
                stk_log("stacks: read clip failed (no set, track or clip)");
            } else {
                stk_prog_t merged = got;
                if (st->read_mode == 1) {          /* append */
                    merged = g_req_base;
                    for (int i = 0; i < got.count && merged.count < STK_MAX_CHORDS; i++)
                        merged.ch[merged.count++] = got.ch[i];
                    merged.scale = got.scale;
                    merged.key = got.key;
                }
                if (merged.count < 1) merged.count = 1;
                st->staging = merged;
                atomic_store_explicit(&st->staging_seq,
                                      atomic_load(&st->staging_seq) + 1,
                                      memory_order_release);
                atomic_store(&st->status, 2);
            }
        }

        usleep(100000);
    }
    return NULL;
}

/*
 * How long an audition lasts: the chord's OWN length, so the whole figure is
 * heard. A minimum of one beat keeps a very short chord from being a click.
 */
static int audition_clocks(const stk_t *st, int step) {
    if (step < 0 || step >= st->prog.count) return CLOCKS_PER_QUARTER;
    int unit = UNIT_CLOCKS;
    int len = st->prog.ch[step].len;
    int c = unit * (len < 1 ? 1 : len);
    return c < CLOCKS_PER_QUARTER ? CLOCKS_PER_QUARTER : c;
}

/* ======================================================================
 * Voices
 * ====================================================================== */
#define STK_HOLD_FOREVER 0x7FFFFFFF

static int emit_off_all(stk_t *st, uint8_t out[][3], int lens[], int max_out, int n) {
    for (int v = 0; v < STK_MAX_VOICES && n < max_out; v++) {
        if (st->voice[v].note < 0) continue;
        out[n][0] = (uint8_t)(0x80 | (st->voice[v].ch & 0x0F));
        out[n][1] = (uint8_t)st->voice[v].note;
        out[n][2] = 0;
        lens[n] = 3;
        n++;
        st->voice[v].note = -1;
    }
    return n;
}

/* How late a grid position leans under Swing -- see the note in arm_chord. */
static int swing_lean(const stk_t *st, int swing16, int sub, int pos) {
    if (st->swing <= 0) return 0;
    if (swing16 ? (pos & 1) : ((pos & 3) == 2))
        return (swing16 ? sub : 2 * sub) * st->swing / 100;
    return 0;
}

/*
 * THE CHORD AS IT IS PLAYED: the written chord with Clip Transpose added to
 * its own Transpose. A performance control, so it shifts what SOUNDS and
 * leaves what is WRITTEN alone -- Key, Scale, the roots, the degree pads --
 * and it lands where the chord's Transpose does, AFTER the scale snap, or the
 * snap would pull a shifted root back into the old key. The sequencer, the
 * staff's resolved notes and its caption all ask here, so none of them can
 * name or draw a chord the synth did not play.
 */
static stk_chord_t played(const stk_t *st, const stk_chord_t *c) {
    stk_chord_t t = *c;
    t.trans += st->transpose;
    return t;
}

/*
 * Arm a step: work out every note's pitch, velocity and onset ONCE, then let
 * tick emit each as its moment arrives. Nothing is sounded here.
 *
 * `hold` is a pad being held, which sustains until release; a sequenced chord
 * gets `len` scaled by `gate`. `phase` is where the chord's onset falls in
 * clocks from a bar line -- the sequencer passes its absolute onset, an
 * audition the chord's place in the phrase -- and it is what anchors the
 * figure to the BAR (see below).
 */
static void arm_chord(stk_t *st, int step, int hold, int phase) {
    st->pend_n = 0;
    if (step < 0 || step >= st->prog.count) return;
    const stk_chord_t *c = &st->prog.ch[step];

    /* MUTE is a scheduling decision, not a volume of zero: a muted chord
     * sends no notes at all, so it cannot leave a hanging note-off behind and
     * costs nothing to skip. */
    if (c->mute && !hold) return;

    int tones[STK_MAX_TONES];
    const stk_chord_t pl = played(st, c);
    int cnt = chord_notes_g(&pl, st->octave, st->prog.scale, st->prog.key,
                            c->colour, st->grouping,
                            st->last_n ? st->last_tones : NULL, st->last_n,
                            tones, STK_MAX_TONES);
    /* Remember it: "dynamic" leads from whatever actually sounded last. */
    st->last_n = cnt > STK_MAX_TONES ? STK_MAX_TONES : cnt;
    for (int i = 0; i < st->last_n; i++) st->last_tones[i] = tones[i];
    int eighth = UNIT_CLOCKS;

    /* Per-chord offsets ride on the globals rather than replacing them. */
    int gate = clampi(st->gate + c->gate, 5, 100);
    int base_vel = clampi(st->velocity + c->vel, 1, 127);

    /*
     * STRUM is a spread ACROSS THE VOICES, humanise is a deviation PER VOICE,
     * and they are added rather than combined: strum is the shape you asked
     * for and humanise is the imprecision on top of it. Keeping them separate
     * is what lets you hear a tight strum and a loose one. The strum is the
     * same at every hit -- it is a shape; the humanise is rolled per hit.
     */
    int strum[STK_MAX_TONES];
    for (int i = 0; i < cnt; i++)
        strum[i] = (!hold && c->strum > 0 && cnt > 1)
                 ? (int)((long)eighth * c->strum * i / (100L * (cnt - 1))) : 0;

    uint16_t mask = RHYTHMS[clampi(c->rhythm, 0, NUM_RHYTHMS - 1)].mask;
    if (!mask) mask = 1u;
    int total = eighth * (c->len < 1 ? 1 : c->len);   /* the chord's own length */

    /*
     * "HOLD" MEANS ONCE, FOR THE WHOLE CHORD -- it must not tile.
     *
     * Every other figure is a BAR-LENGTH pattern and repeating it each bar is
     * the point: a clave over a two-bar chord is two claves. `hold` is the
     * degenerate case, a single hit at position 0, and tiling it re-articulates
     * the chord every bar. On a two-bar chord that came out as two one-bar
     * chords -- audibly, and drawn that way too, so it read as the module
     * splitting the chord in half.
     *
     * A held pad is the same shape from the other side: one strike at the
     * onset, sustained until the release, whatever figure the chord carries.
     */
    if (hold || mask == 1u) {
        int sound = total * gate / 100;
        if (sound < 1) sound = 1;
        int delay[STK_MAX_TONES], lo = 0;
        for (int i = 0; i < cnt; i++) {
            delay[i] = strum[i] + (hold ? 0 : humanised_delay(st, step, i, 0, eighth));
            if (delay[i] < lo) lo = delay[i];
        }
        for (int i = 0; i < cnt && st->pend_n < STK_MAX_PEND; i++) {
            st->pend[st->pend_n].note  = tones[i];
            st->pend[st->pend_n].vel   = hold ? base_vel : note_velocity(st, c, step, i, 0);
            st->pend[st->pend_n].at    = delay[i] - lo;
            st->pend[st->pend_n].dur   = hold ? STK_HOLD_FOREVER : sound;
            st->pend[st->pend_n].fired = 0;
            st->pend_n++;
        }
        return;
    }

    /*
     * THE RHYTHM TILES, IT DOES NOT STRETCH. The mask covers one BAR and a
     * chord two bars long plays the figure twice -- which is what a figure
     * does. Stretching would make the same pattern a different rhythm
     * depending on how long you happened to hold the chord.
     *
     * AND IT IS ANCHORED TO THE BAR, NOT TO THE CHORD. A clave, a tresillo, a
     * bossa are defined by where in the BAR their hits fall; a chord that
     * starts on beat three must play the second half of the figure, not the
     * first half again. Counting the grid from the chord's own onset played
     * two half-bar chords under "son 3-2" as `3 | 3` -- the "2" side never
     * sounded (measured 2026-09-19, tests/test_stacks_figure_phase.sh). So
     * the grid position is the chord's place in the bar plus its own elapsed
     * time, and only the chord's LENGTH is counted from its onset.
     */
    int sub = BAR_CLOCKS / RHYTHM_STEPS;          /* one grid position */
    if (sub < 1) sub = 1;
    int base = (((phase % BAR_CLOCKS) + BAR_CLOCKS) % BAR_CLOCKS) / sub;
    /*
     * SWING DISPLACES THE OFF-BEAT AT THE FINEST LEVEL THE FIGURE USES.
     *
     * The grid is sixteenths and swing moved the odd sixteenths -- Move's own
     * swing does the same -- which left every figure written on the eighth
     * grid untouched: turning Swing under "eighth" changed nothing at all,
     * and that is the one figure swing is FOR. A figure with no odd-sixteenth
     * hit is an eighth-note figure, so its off-beat eighths (the "and"s, grid
     * 2 6 10 14) are what lean; a figure that uses the sixteenths keeps the
     * sixteenth swing, because leaning both levels at once collides them.
     * The lean is `swing` percent of the level's own gap.
     */
    int swing16 = (mask & 0xAAAAu) != 0;
    int hit = 0;

    for (int at = 0; at < total && st->pend_n < STK_MAX_PEND; at += sub) {
        int pos = (base + at / sub) % RHYTHM_STEPS;
        if (!(mask & (1u << pos))) continue;
        /*
         * A HIT LASTS UNTIL ITS OWN NEXT HIT, scaled by Gate -- so a dense
         * pattern shortens itself and needs no separate length control.
         *
         * ITS OWN, not the figure's first gap applied to every hit: with the
         * gap to the second mask bit used throughout, an uneven figure gave
         * its short hits a long duration, and the note-off of hit N then fell
         * AFTER hit N+1 had re-struck the same pitches -- cutting it. In
         * tresillo (3+3+2) the "2" sounded for four clocks, a 32nd, before
         * the previous hit's release silenced it.
         */
        int gap = RHYTHM_STEPS;
        for (int b = 1; b < RHYTHM_STEPS; b++)
            if (mask & (1u << ((pos + b) % RHYTHM_STEPS))) { gap = b; break; }
        /* A swung hit is LATE, so it is shorter by its own lean and longer
         * by the next hit's -- otherwise the note runs into the next strike
         * of the same pitches and is cut off by its retrigger. */
        int swing_at = swing_lean(st, swing16, sub, pos);
        int sound = (gap * sub - swing_at
                     + swing_lean(st, swing16, sub, (pos + gap) % RHYTHM_STEPS))
                    * gate / 100;
        if (sound < 1) sound = 1;
        int delay[STK_MAX_TONES], lo = 0;
        for (int i = 0; i < cnt; i++) {
            delay[i] = strum[i] + humanised_delay(st, step, i, hit, eighth);
            if (delay[i] < lo) lo = delay[i];
        }
        /* Shifted so the earliest voice lands on the hit rather than clamped
         * at it -- clamping makes a chord tighter the more you humanise. */
        for (int i = 0; i < cnt && st->pend_n < STK_MAX_PEND; i++) {
            st->pend[st->pend_n].note  = tones[i];
            st->pend[st->pend_n].vel   = note_velocity(st, c, step, i, hit);
            st->pend[st->pend_n].at    = at + swing_at + delay[i] - lo;
            st->pend[st->pend_n].dur   = sound;
            st->pend[st->pend_n].fired = 0;
            st->pend_n++;
        }
        hit++;
    }
}

/* Emit whatever is now due. `elapsed` is clocks since the step was armed. */
static int emit_due(stk_t *st, int elapsed,
                    uint8_t out[][3], int lens[], int max_out, int n) {
    for (int i = 0; i < st->pend_n && n < max_out; i++) {
        if (st->pend[i].fired) continue;
        if (elapsed < st->pend[i].at) continue;
        /*
         * A PITCH ALREADY SOUNDING IS RETRIGGERED, NOT DOUBLED. Two entries
         * for one note meant the older one's release cut the newer one
         * mid-flight -- the same failure the per-hit gaps above fix from the
         * other side, and it can still arise across chords that share a
         * pitch. One voice per pitch: release it, then strike it again.
         */
        int v = -1, retrig = 0;
        for (int j = 0; j < STK_MAX_VOICES; j++)
            if (st->voice[j].note == st->pend[i].note) { v = j; retrig = 1; break; }
        if (v < 0)
            for (int j = 0; j < STK_MAX_VOICES; j++) if (st->voice[j].note < 0) { v = j; break; }
        if (v < 0) break;
        if (retrig) {
            if (n >= max_out - 1) break;      /* the off and the on go together */
            out[n][0] = 0x80;
            out[n][1] = (uint8_t)st->pend[i].note;
            out[n][2] = 0;
            lens[n] = 3;
            n++;
        }
        st->voice[v].note = st->pend[i].note;
        st->voice[v].ch = 0;
        st->voice[v].off_at = st->pend[i].dur == STK_HOLD_FOREVER
                            ? STK_HOLD_FOREVER : st->pulse + st->pend[i].dur;
        out[n][0] = 0x90;
        out[n][1] = (uint8_t)st->pend[i].note;
        out[n][2] = (uint8_t)st->pend[i].vel;
        lens[n] = 3;
        n++;
        st->pend[i].fired = 1;
    }
    return n;
}

/* ======================================================================
 * Entry points -- ALL OF THESE ARE THE SPI CALLBACK
 * ====================================================================== */
/* By NAME, so inserting a shape cannot silently change the default chords. */
static int shape_by_opt(const char *opt) {
    for (int i = 0; i < NUM_SHAPES; i++)
        if (strcmp(SHAPES[i].opt, opt) == 0) return i;
    return SHAPE_SINGLE;
}

/*
 * BOOTS EMPTY. One rest, in A minor, at the default octave.
 *
 * It used to seed Am-F-C-G on the argument that the module should make a sound
 * before anyone has typed anything -- which does not survive being asked out
 * loud. This is a chord SEQUENCER: handing someone a progression they did not
 * write is presumptuous, and it made two different starting states where there
 * should be one, since Clear already produced an empty buffer.
 *
 * Empty is not a dead end now: the grid says how to fill a slot, and the
 * Start branch offers 25 progressions to begin from. Offering a starting point
 * is a better answer than imposing one.
 */
/*
 * FOUR EMPTY BARS, NOT ONE.
 *
 * The buffer used to open as a single rest, which was defensible -- empty must
 * not be ZERO chords, because every add gesture copies the chord you are on --
 * but one slot is not the shape of anything anybody writes. Four one-bar slots
 * in a four-bar clip is the ordinary case, so the module opens on it: you turn
 * Chord Shape four times and you have a progression, rather than inserting
 * three slots first.
 *
 * It is still EMPTY -- four rests, drawn as framed empty slots -- so nothing
 * has been written for you, and the three setup controls still settle each
 * other until you write something.
 */
#define STK_DEFAULT_CHORDS 4

static void seed_default(stk_prog_t *p) {
    memset(p, 0, sizeof(*p));
    p->count = STK_DEFAULT_CHORDS;
    p->scale = 2;   /* Minor */
    p->key = 9;     /* A */
    p->rate = 1;    /* 1 bar */
    /* AFTER the rate is set, not before: `default_len` reads it, and the memset
     * above leaves it zero. Computing the length first gave every rate the
     * same 2-bar chord and made Rate look like it did nothing at all. */
    int len = default_len(p);
    for (int i = 0; i < p->count; i++) {
        p->ch[i].shape  = SHAPE_REST;
        p->ch[i].root   = 69;      /* A3 -- Key A at the default octave */
        p->ch[i].rhythm = 0;
        p->ch[i].len    = len;
    }
}

static void *stk_create(const char *module_dir, const char *config_json) {
    (void)module_dir;
    stk_t *st = (stk_t *)calloc(1, sizeof(stk_t));
    if (!st) return NULL;

    seed_default(&st->prog);
    st->sel = 1;
    st->run = 1;
    st->preview = 0;   /* the click auditions; edits do so only on request */
    st->octave = 0;
    st->transpose = 0;
    st->velocity = 100;
    st->gate = 90;
    st->lanes = 0;
    /*
     * FOUR BARS OF EMPTY CANVAS, and it is not an exception to the fitting
     * rule -- it is what fitting gives you once there is anything to fit. The
     * boot buffer is ONE REST, so an exact fit would open on a one-bar clip
     * that jumped to four the moment you made your first chord. Starting at
     * four and letting the first edit confirm it is the same destination
     * without the jump.
     */
    st->bars = 4;
    st->bars_manual = 0;
    st->defoct = 3;
    st->swing = 0;
    st->hum_vel = 0;
    st->hum_time = 0;
    st->seed_vel = 0x1234567u;
    st->seed_time = 0x89ABCDEu;
    st->read_mode = 0;
    st->held_pad = -1;
    st->held_mask = 0;
    st->pat_cur = 0;   /* slot 1 is active, and empty */
    st->cur_step = -1;
    st->read_track = 1;
    for (int i = 0; i < STK_MAX_VOICES; i++) st->voice[i].note = -1;
    atomic_init(&st->quit, 0);
    atomic_init(&st->req_read, 0);
    atomic_init(&st->staging_seq, 0);
    atomic_init(&st->status, 0);
    atomic_init(&st->move_track, 0);
    atomic_init(&st->rev, 0);
    atomic_init(&st->snap_req, 0);
    atomic_init(&st->snap_seq, 0);

    /* Where the instrument lives between sessions. Overridable for the
     * tests (and for a second instance) through the config JSON; the
     * directory is created here, once, before any thread exists. */
    snprintf(st->state_path, sizeof st->state_path, "%s", STK_STATE_FILE);
    if (config_json) {
        const char *p = strstr(config_json, "\"state_path\":\"");
        if (p) {
            p += 14;
            const char *e = strchr(p, '"');
            if (e && e - p < (long)sizeof st->state_path)
                snprintf(st->state_path, sizeof st->state_path, "%.*s", (int)(e - p), p);
        }
    }
    {
        char dir[256];
        snprintf(dir, sizeof dir, "%s", st->state_path);
        char *slash = strrchr(dir, '/');
        if (slash && slash != dir) { *slash = 0; mkdir(dir, 0755); }
    }
    state_file_load(st);

    /*
     * SCHED_OTHER, EXPLICITLY. create_instance runs on the SPI callback, so a
     * thread that inherits gets FIFO 70 -- above Move's own Link Main at 35.
     * The audit that found five modules doing this is in docs/plans/.
     */
    pthread_attr_t attr;
    if (pthread_attr_init(&attr) == 0) {
        struct sched_param sp;
        memset(&sp, 0, sizeof(sp));
        sp.sched_priority = 0;
        pthread_attr_setinheritsched(&attr, PTHREAD_EXPLICIT_SCHED);
        pthread_attr_setschedpolicy(&attr, SCHED_OTHER);
        pthread_attr_setschedparam(&attr, &sp);
        if (pthread_create(&st->worker, &attr, stk_worker, st) == 0) st->worker_live = 1;
        pthread_attr_destroy(&attr);
    }
    if (!st->worker_live) stk_log("stacks: worker thread failed; clip read/stamp disabled");
    return st;
}

static void stk_destroy(void *instance) {
    stk_t *st = (stk_t *)instance;
    if (!st) return;
    if (st->worker_live) {
        atomic_store(&st->quit, 1);
        pthread_join(st->worker, NULL);
    }
    /* The last word: whatever the quiet timer had not yet written. No
     * callback can run now, so serialising here is safe. */
    if (stk_get_param(st, "state", st->snap_buf, sizeof st->snap_buf) > 0)
        state_file_write(st->state_path, st->snap_buf);
    free(st);
}

/*
 * Is this note one of OURS, sounding right now?
 *
 * The module's output goes to the track and the track comes back -- as Move's
 * echo while it plays, and again as a clip once you have stamped one. Without
 * this, those notes are read as chord SELECTORS (see the note handler), so the
 * module spends the bar being dragged around by its own tail.
 */
static int is_own_voice(const stk_t *st, int note) {
    for (int v = 0; v < STK_MAX_VOICES; v++)
        if (st->voice[v].note == note) return 1;
    return 0;
}

static int stk_process_midi(void *instance, const uint8_t *in, int in_len,
                            uint8_t out[][3], int lens[], int max_out) {
    stk_t *st = (stk_t *)instance;
    if (!st || in_len < 1 || max_out < 1) return 0;
    uint8_t status = in[0];

    /* Transport. Consumed, never passed on -- the chain's synth does not want
     * our clock and Move already has its own. */
    /*
     * THE LAP BEGINS ON MOVE'S DOWNBEAT, NOT ON THE BUTTON PRESS.
     *
     * `stamp` used to set pulse = 0 the instant it was pressed, which makes
     * the module's bar 1 wherever the finger landed while Move's clock carries
     * on at its own position -- so a recorded lap lined up only by luck, and
     * no amount of care at the two ends could fix it. Pressing the button ARMS
     * instead, and the start is taken from the transport: a MIDI Start is a
     * downbeat by definition, and on an already-running clock the next bar
     * line is `pulse % BAR_CLOCKS == 0`.
     *
     * Zeroing pulse there is what puts chord 1 on that bar, and it is the same
     * thing Start itself does, so the module's grid and Move's agree from that
     * moment on.
     */
    if (status == 0xF8) {
        /*
         * THE FIRST CLOCK AFTER START **IS** THE DOWNBEAT, so it does not
         * advance anything -- it IS pulse 0.
         *
         * Counting it made `pulse` one ahead of the music forever, and the
         * module fired everything a clock early: measured natively at 95
         * clocks for a bar line that belongs at 96, with chord 1 sounding at
         * the Start message itself, before the downbeat had happened at all.
         * On the device that is every note of a stamped take sitting just
         * before the line it belongs on -- and the error is TEMPO-SCALED, one
         * clock being 20.8ms at 120 BPM and 125ms at 20.
         *
         * The shim carries the same war story, measured at two tempos, in
         * src/host/transport_grid.h: two consumers there had this identical
         * off-by-one because the fact was never written down. This is a third.
         * It is fixed at the COUNTER here rather than in the consumers because
         * this module's `pulse` has a second producer -- the internal clock an
         * audition runs on, which has no Start and no downbeat to be offset
         * from -- so a correction applied at the reading sites would have to
         * know which clock it was reading, and there are a dozen of them.
         */
        if (st->at_downbeat) st->at_downbeat = 0;   /* this clock is pulse 0 */
        else st->pulse++;
        if (st->stamp_armed && st->clock_running && (st->pulse % BAR_CLOCKS) == 0) {
            st->stamp_armed = 0;
            atomic_store(&st->status, 1);   /* working: the lap Move records */
            st->pulse = 0;
            st->armed_id = 0;
            st->pend_n = 0;
            st->run = 1;
            st->stamp_once = 1;
        }
        return 0;
    }
    if (status == 0xFA) {
        st->pulse = 0; st->clock_running = 1; st->armed_id = 0;
        st->at_downbeat = 1;   /* the next clock is the downbeat, not the one after */
        if (st->stamp_armed) {          /* a Start IS the downbeat */
            st->stamp_armed = 0;
            atomic_store(&st->status, 1);   /* working: the lap Move records */
            st->run = 1;
            st->stamp_once = 1;
        }
        return 0;
    }
    /* Continue resumes MID-STREAM -- there is no downbeat to wait for, and
     * claiming one would swallow a clock and shift the phase the other way. */
    if (status == 0xFB) { st->clock_running = 1; st->at_downbeat = 0; return 0; }
    if (status == 0xFC) {
        st->clock_running = 0;
        st->at_downbeat = 0;
        /*
         * A STOP REWINDS, because Move's does.
         *
         * Move returns its playhead to the top on stop, and this did not --
         * it only stopped counting. The two then disagreed about where the
         * music was: the next Start happened to paper over it (a Start IS
         * zero), but anything reading the position in between saw a module
         * parked mid-phrase while Move sat at the beginning, and a panel
         * asking the module where it is was told the wrong thing.
         */
        st->pulse = 0;
        st->pulse_accum = 0.0;
        /*
         * A stop CANCELS a pending lap, so it must also retract the word AND
         * PUT RUN BACK. Arming switches Run off so the module is silent until
         * the downbeat it is waiting for; cancel without restoring it and the
         * next press of play produces nothing at all -- a module that has gone
         * quiet for a reason nothing on either screen mentions, which reads as
         * the stamp having broken something.
         */
        if (st->stamp_armed) {
            atomic_store(&st->status, 0);
            st->run = st->stamp_run_prev;
        }
        if (st->stamp_restore && !st->parked) {   /* the take is over: give Run back */
            st->run = 1;
            st->stamp_restore = 0;
        }
        /* Parked: the restore waits for resume -- see `parked` in set_param. */
        st->stamp_armed = 0;
        st->armed_id = 0;
        st->pend_n = 0;
        st->cur_step = -1;
        return emit_off_all(st, out, lens, max_out, 0);
    }

    /*
     * A PAD SELECTS A CHORD, by pitch class: C picks chord 1, C# chord 2, and
     * so on for as many chords as the progression has. Pitch class rather than
     * absolute note so it works the same from a Move pad, a clip and an
     * external keyboard, none of which agree on where "the first pad" is.
     */
    /*
     * A NOTE THIS MODULE IS SOUNDING IS NOT A NOTE SOMEBODY PLAYED AT IT.
     *
     * An Am7 contains a C, C is pitch class 0, and pitch class 0 is chord 1 --
     * so the chords this module played were selecting chords, and a selection
     * is armed with HOLD, whose voices never expire. Captured on a device as
     * `playing` jumping 3,1,1,0,2,2,1,2 while the position advanced perfectly
     * smoothly, and heard as a chord that never moves on.
     *
     * Filtering on the SOUNDING VOICES rather than on "is the transport
     * running" keeps both real uses alive: a clip written to drive the module
     * still drives it, a keyboard still plays it live, and only its own tail
     * is ignored. A coincidence -- playing a note it is already sounding --
     * costs one selection, which is nothing beside the loop it prevents.
     */
    if ((status & 0xF0) == 0x90 && in_len >= 3 && in[2] > 0) {
        if (is_own_voice(st, in[1])) return 0;
        int pc = in[1] % 12;
        if (pc < st->prog.count) {
            st->held_mask |= 1u << pc;
            st->held_pad = pc;
        }
        return 0;
    }
    if (((status & 0xF0) == 0x80 || ((status & 0xF0) == 0x90 && in_len >= 3 && in[2] == 0))
        && in_len >= 2) {
        if (is_own_voice(st, in[1])) return 0;
        /*
         * HELD IS A SET, NOT A SLOT. It was one pitch class, cleared only when
         * a note-off matched it -- so two notes held together left the first
         * one's release doing nothing, and if the matching release was ever
         * lost (a MIDI_IN slot zeroed behind a terminator is a documented way
         * to lose one) the selection was stranded ON, holding that chord for
         * the rest of the session. What is still down decides.
         */
        int pc = in[1] % 12;
        st->held_mask &= ~(1u << pc);
        if (st->held_mask == 0) st->held_pad = -1;
        else if (pc == st->held_pad) {
            for (int i = 0; i < 12; i++)
                if (st->held_mask & (1u << i)) { st->held_pad = i; break; }
        }
        return 0;
    }

    /* Everything else -- CC, bend, pressure -- passes through untouched. */
    out[0][0] = in[0];
    out[0][1] = in_len > 1 ? in[1] : 0;
    out[0][2] = in_len > 2 ? in[2] : 0;
    lens[0] = in_len;
    return 1;
}

/* ======================================================================
 * tick -- the sequencer
 *
 * Time comes from Move's clock while the transport runs. With it stopped and
 * Preview set to "loop" the progression still has to move, so the same `pulse`
 * is advanced from the block length and the project tempo instead. One counter
 * either way, so every downstream calculation is written once.
 * ====================================================================== */
static int stk_tick(void *instance, int frames, int sample_rate,
                    uint8_t out[][3], int lens[], int max_out) {
    stk_t *st = (stk_t *)instance;
    if (!st || max_out < 1) return 0;
    int n = 0;

    /* Pick up a progression the worker finished parsing. An acquire-load
     * against its release-store, so `staging` is fully written before we can
     * observe the sequence number that publishes it. */
    st->blocks++;
    /* Read Track follows Move's selection -- on a CHANGE only, so the knob
     * is still yours until the selection moves (see "read_track"). A write
     * like any other, so the rev says so and both surfaces show it. */
    {
        int mt = atomic_load_explicit(&st->move_track, memory_order_relaxed);
        if (mt != st->move_track_seen) {
            st->move_track_seen = mt;
            if (mt >= 1 && mt != st->read_track) {
                st->read_track = mt;
                atomic_fetch_add_explicit(&st->rev, 1, memory_order_relaxed);
            }
        }
    }
    {
        int sn = atomic_load(&st->status);
        if (sn != st->status_rev_seen) {
            st->status_rev_seen = sn;
            atomic_fetch_add_explicit(&st->rev, 1, memory_order_relaxed);
        }
    }
    /* The worker asked for a snapshot to save: serialise here, where the
     * state is ours, and release it. One snprintf pass, the same one the
     * Tool tab costs on a rev change; never more often than the quiet time. */
    {
        int want = atomic_load_explicit(&st->snap_req, memory_order_acquire);
        if (want != st->snap_done) {
            st->snap_done = want;
            st->snap_rev = atomic_load_explicit(&st->rev, memory_order_relaxed);
            if (stk_get_param(st, "state", st->snap_buf, sizeof st->snap_buf) < 0)
                st->snap_buf[0] = 0;
            atomic_store_explicit(&st->snap_seq, want, memory_order_release);
        }
    }
    int seq = atomic_load_explicit(&st->staging_seq, memory_order_acquire);
    if (seq != st->staging_applied) {
        st->staging_applied = seq;
        atomic_fetch_add_explicit(&st->rev, 1, memory_order_relaxed);
        st->prog = st->staging;
        if (st->sel > st->prog.count) st->sel = st->prog.count;
        if (st->sel < 1) st->sel = 1;
        st->armed_id = 0;
        st->pend_n = 0;
        n = emit_off_all(st, out, lens, max_out, n);
    }

    /* The cut owed by an audition or a click. Before the release loop, so a
     * voice silenced here is not also released a second time below. */
    if (st->cut_pending) {
        st->cut_pending = 0;
        n = emit_off_all(st, out, lens, max_out, n);
    }


    /*
     * NOT BETWEEN START AND THE FIRST CLOCK. The transport is running there
     * but the downbeat has not arrived, and a sequencer that starts on the
     * Start message plays chord 1 early by whatever gap Move leaves before its
     * first clock -- which the native measurement caught as a note sounding
     * with the count still at zero.
     */
    int free_running = (st->clock_running && st->run && !st->at_downbeat)
                     || st->hold_run;

    /*
     * AN AUDITION NEEDS A CLOCK OF ITS OWN.
     *
     * Everything this module schedules is measured in pulses, and pulses come
     * from Move's transport -- so with the transport stopped `pulse` is frozen
     * and `elapsed` never leaves zero. A rhythm whose hits sit at 3, 6 and 10
     * then fires only the one at 0: clicking a chord played the downbeat and
     * nothing else, and the figure you had just chosen appeared to be ignored.
     *
     * So the internal clock runs for anything that sounds without the
     * transport: a looping preview, a held pad, and an audition. It does NOT
     * set `free_running` for the latter two -- that flag is what hands control
     * to the SEQUENCER, and a click on a stopped transport must play one chord,
     * not start the progression.
     */
    int self_clocked = (!st->clock_running || !st->run) &&
        (st->preview == 2 || st->hold_run || st->force_until > st->pulse
         || st->held_pad >= 0);
    if (self_clocked) {
        float bpm = (g_host && g_host->get_bpm) ? g_host->get_bpm() : 120.0f;
        if (bpm < 20.0f || bpm > 300.0f) bpm = 120.0f;
        if (sample_rate > 0) {
            st->pulse_accum += (double)frames / (double)sample_rate
                             * (double)bpm * (double)CLOCKS_PER_QUARTER / 60.0;
            while (st->pulse_accum >= 1.0) { st->pulse_accum -= 1.0; st->pulse++; }
        }
        if (st->preview == 2) free_running = 1;
    }

    /* Release any voice whose time is up, whatever put it there. */
    for (int v = 0; v < STK_MAX_VOICES && n < max_out; v++) {
        if (st->voice[v].note < 0) continue;
        if (st->voice[v].off_at == STK_HOLD_FOREVER) continue;
        if (st->pulse < st->voice[v].off_at) continue;
        out[n][0] = 0x80;
        out[n][1] = (uint8_t)st->voice[v].note;
        out[n][2] = 0;
        lens[n] = 3;
        n++;
        st->voice[v].note = -1;
    }

    /*
     * WHAT SHOULD BE ARMED. A held pad outranks an audition, which outranks
     * the sequence.
     *
     * NOTHING FORCES A NOTE OFF HERE. Voices expire on their own `off_at`, so
     * a chord whose Length runs past its step rings into the next one -- which
     * is the entire reason Length goes to 16 eighths against a step of 8. The
     * earlier version cut every voice on each step change and made lengths
     * above 8 indistinguishable from 8.
     */
    int want_id = 0, want_step = -1, hold = 0, onset_pulse = 0;
    int phase = 0;      /* the chord's onset, in clocks from a bar line */

    if (st->held_pad >= 0 && st->held_pad < st->prog.count) {
        want_id = -1000 - st->held_pad;
        want_step = st->held_pad;
        hold = 1;
        onset_pulse = st->pulse;
    } else if (st->force_until > st->pulse) {
        if (st->force_step >= 0 && st->force_step < st->prog.count) {
            want_id = -2000 - st->force_step;
            want_step = st->force_step;
            /* An audition plays the chord where it SITS in the phrase, so the
             * figure it carries is the one the sequence would play. */
            phase = (chord_slot_start(&st->prog, want_step)
                     + st->prog.ch[want_step].off) * UNIT_CLOCKS;
            if (st->armed_id == want_id) {
                /* Already sounding. While the jog is HELD, re-arm in phase
                 * when the figure reaches the chord's end, so holding loops
                 * the rhythm instead of leaving silence after one pass. */
                onset_pulse = st->armed_at;
            } else {
                onset_pulse = st->pulse;
            }
        }
    } else if (free_running && st->prog.count > 0) {
        int unit = UNIT_CLOCKS;
        if (unit < 1) unit = 1;
        int total_u = prog_total_units(&st->prog);
        /*
         * THE LOOP IS THE CLIP, AND THE PROGRESSION TILES INSIDE IT.
         *
         * This wrapped at the PROGRESSION length while the playhead wrapped at
         * the CLIP, so the two agreed only when the bars happened to be a
         * multiple of the phrase. At bars=3 against a 4-bar progression the
         * clip is 24 units and the phrase 32: the head restarted three
         * quarters of the way through the music, every lap.
         *
         * Wrapping here instead makes all THREE surfaces one answer -- what
         * plays, what the staff draws, and what Stamp writes are the same
         * `bars` of music, with the progression repeating to fill them.
         */
        int clip_u = clampi(st->bars, 1, 16) * bar_units_for(&st->prog);
        if (clip_u < 1) clip_u = total_u;
        int total_c = clip_u * unit;
        int cycle = st->pulse / total_c;         /* which pass through it */
        /* One lap recorded: stop at the top of the second, so the clip holds
         * exactly one pass and nothing doubles it afterwards. */
        if (st->stamp_once && cycle >= 1) {
            st->stamp_once = 0;
            atomic_store(&st->status, 2);   /* one lap played: that is the take */
            /*
             * RUN IS OWED BACK. Stopping after one lap REQUIRES switching Run
             * off -- the sequencer follows it, so there is no other way to
             * stop without stopping Move -- but leaving it off is a setting
             * silently changed by an action: the take ends, and from then on
             * pressing play on the Move does nothing at all, with nothing on
             * either screen saying why. Reported exactly that way. It comes
             * back when the transport this take belongs to stops.
             */
            st->stamp_restore = st->stamp_run_prev;
            st->run = 0;
            st->armed_id = 0;
            st->pend_n = 0;
            return emit_off_all(st, out, lens, max_out, n);
        }
        /*
         * Position inside the clip, then inside the phrase that tiles it --
         * and WHICH TILE, because the two facts below must name it.
         *
         * They did not, and a phrase shorter than the clip broke in two ways
         * (measured 2026-09-19, tests/test_stacks_tiling.sh): a one-chord
         * progression in a four-bar clip sounded ONCE per lap, because the
         * second tile's chord had the same `want_id` as the first and never
         * re-armed; and on the second tile of any phrase `onset_pulse` was
         * still the first tile's, so `elapsed` began a whole phrase late and
         * every rhythm hit of the chord fired at once, as one stacked chord.
         */
        int pos_u  = (st->pulse % total_c) / unit;
        int tile = 0, tiles = 1;
        if (total_u > 0) {
            tile   = pos_u / total_u;
            pos_u %= total_u;
            tiles  = (clip_u + total_u - 1) / total_u;
        }

        /* Walk the durations to find the chord this moment belongs to. */
        int idx = 0, acc = 0;
        for (; idx < st->prog.count; idx++) {
            int l = st->prog.ch[idx].len;
            if (l < 1) l = 1;
            if (pos_u < acc + l) break;
            acc += l;
        }
        if (idx >= st->prog.count) { idx = st->prog.count - 1; acc = chord_slot_start(&st->prog, idx); }

        int onset_u = acc + st->prog.ch[idx].off;
        /* Before its offset the chord has not started; that is not the same as
         * "nothing is playing", and must not silence the one still ringing. */
        if (pos_u >= onset_u) {
            /* Absolute across passes AND tiles, so looping round to the same
             * chord re-arms it rather than reading as "still that one". */
            want_id = (cycle * tiles + tile) * st->prog.count + idx + 1;
            want_step = idx;
            onset_pulse = cycle * total_c + (tile * total_u + onset_u) * unit;
            phase = onset_pulse;
        }
    }

    if (want_id != 0 && want_id != st->armed_id) {
        arm_chord(st, want_step, hold, phase);
        st->armed_id = want_id;
        st->armed_at = onset_pulse;
        st->cur_step = want_step;   /* what `prog` reports as playing */
    }
    /* A released pad or a finished audition stops sounding at once -- unlike a
     * sequenced chord, both are gestures with an explicit end. */
    if (want_id == 0 && st->armed_id < 0) {
        st->armed_id = 0;
        st->pend_n = 0;
        st->cur_step = -1;
        n = emit_off_all(st, out, lens, max_out, n);
    }

    if (st->armed_id != 0 && st->pend_n > 0)
        n = emit_due(st, st->pulse - st->armed_at, out, lens, max_out, n);

    return n;
}

/* ======================================================================
 * Parameters
 * ====================================================================== */
static stk_chord_t *sel_chord(stk_t *st) {
    int i = st->sel - 1;
    if (i < 0) i = 0;
    if (i >= st->prog.count) i = st->prog.count - 1;
    if (i < 0) i = 0;
    return &st->prog.ch[i];
}


/* Index of `val` in an option list, or -1. Accepts the index as a number too,
 * because the chain writes enums both ways depending on the caller. */
static int opt_index(const char *val, const char *const *opts, int n) {
    if (!val) return -1;
    for (int i = 0; i < n; i++) if (strcmp(val, opts[i]) == 0) return i;
    if (val[0] >= '0' && val[0] <= '9') {
        int i = atoi(val);
        if (i >= 0 && i < n) return i;
    }
    return -1;
}

static int shape_index(const char *val) {
    if (!val) return -1;
    for (int i = 0; i < NUM_SHAPES; i++) if (strcmp(val, SHAPES[i].opt) == 0) return i;
    if (val[0] >= '0' && val[0] <= '9') {
        int i = atoi(val);
        if (i >= 0 && i < NUM_SHAPES) return i;
    }
    return -1;
}

static int scale_index(const char *val) {
    if (!val) return -1;
    for (int i = 0; i < NUM_SCALES; i++) if (strcmp(val, SCALES[i].name) == 0) return i;
    if (val[0] >= '0' && val[0] <= '9') {
        int i = atoi(val);
        if (i >= 0 && i < NUM_SCALES) return i;
    }
    return -1;
}

/* Root is an enum of note names starting at C1 = MIDI 24. */
#define ROOT_BASE 24
#define ROOT_SPAN 60
static int root_index(const char *val) {
    if (!val) return -1;
    for (int i = 0; i < ROOT_SPAN; i++) {
        char name[8];
        snprintf(name, sizeof(name), "%s%d", NOTE_NAMES[i % 12], 1 + i / 12);
        if (strcmp(val, name) == 0) return i;
    }
    if (val[0] >= '0' && val[0] <= '9') {
        int i = atoi(val);
        if (i >= 0 && i < ROOT_SPAN) return i;
    }
    return -1;
}

/*
 * WHERE A NEWLY FILLED SLOT STARTS.
 *
 * A rest has no pitch, so the root it carries is latent -- whatever the slot
 * last held, or the seed value. Filling one and getting that stale note is a
 * small betrayal: you set a Key in SETUP precisely so new material lands in
 * it. Computed at fill time rather than stored, so changing the Key or the
 * default octave afterwards is reflected by the next slot you fill.
 */
static int default_root(const stk_t *st) {
    int oct = clampi(st->defoct, 1, ROOT_SPAN / 12);
    return clampi(ROOT_BASE + (oct - 1) * 12 + st->prog.key,
                  ROOT_BASE, ROOT_BASE + ROOT_SPAN - 1);
}

/*
 * TRANSPOSE THE WHOLE BUFFER, PRESERVING THE INTERVALS BETWEEN CHORDS.
 *
 * Key and Octave Default describe the harmony the progression sits in, so
 * changing one has to MOVE what is already there -- a progression that stays
 * put while its key changes is in neither key. It applies to the buffer
 * whatever put it there: a library entry, a clip read, or chords you dialled
 * in by hand.
 *
 * THE DELTA IS CLAMPED, NEVER THE CHORDS. Clamping each root independently
 * would squash the progression against the end of the range and silently
 * change the intervals between chords -- the one thing a transposition must
 * not do. So the largest in-range shift is found first and applied to every
 * chord equally: at the edge the progression stops moving instead of
 * deforming.
 *
 * AND THE CLAMP IS ROUNDED DOWN TO `unit`. Octave Default moves in twelves,
 * and a clamp that landed on any semitone turned a two-octave move into +13 --
 * a transposition into a DIFFERENT KEY, which is the one thing that control
 * must never do. Measured: chord one moved +14 and chord two +15, because a
 * shift that is not a multiple of twelve changes pitch classes and the root
 * snapping then bends each chord by a different amount. Pass 12 for octaves
 * and 1 for semitones, and the clamp can only ever give back whole units.
 */
static void transpose_prog(stk_t *st, int semis, int unit) {
    if (semis == 0 || st->prog.count <= 0) return;
    if (unit < 1) unit = 1;
    const int lo = ROOT_BASE, hi = ROOT_BASE + ROOT_SPAN - 1;
    for (int i = 0; i < st->prog.count; i++) {
        int r = st->prog.ch[i].root;
        if (r + semis < lo) semis = lo - r;
        if (r + semis > hi) semis = hi - r;
    }
    /* Truncate TOWARD ZERO so a clamped move keeps its direction. */
    semis = (semis / unit) * unit;
    if (semis == 0) return;
    for (int i = 0; i < st->prog.count; i++)
        st->prog.ch[i].root = clampi(st->prog.ch[i].root + semis, lo, hi);
}

/*
 * CAPTURE THE BUFFER IF IT CHANGED.
 *
 * Called at the END of every parameter write rather than at the start of the
 * ones believed to mutate: this module has forty keys that can touch the
 * progression and a list of them would be wrong the first time one was added.
 * Comparing the result against the newest stored state is O(one memcmp of
 * under a kilobyte) and cannot be forgotten by a future edit path.
 */
static int buffer_is_empty(const stk_prog_t *p);
static void seed_default(stk_prog_t *p);
static int default_len(const stk_prog_t *p);
static int default_root(const stk_t *st);

/*
 * YOU ARE ALWAYS IN A SLOT, and everything runs against the one you are in.
 *
 * This is pattern SELECT, not save and load. Slot 1 is active and empty when
 * the module starts; every edit belongs to the active slot; activating
 * another slot READS it and you carry on there. Nothing is ever "stored" as a
 * separate act, because there is nowhere else for an edit to go.
 *
 * That is worth stating because the first version of this was the other
 * model -- a working buffer plus sixteen snapshots you saved into -- and it
 * produced two complaints that are really the same one: tapping an empty slot
 * either captured the buffer into it (so a slot you meant to leave empty
 * became a pattern) or did nothing at all (so the slot could not be reached).
 * Neither is what a pattern button does anywhere else.
 *
 * `used` therefore means HAS MUSIC IN IT, and for the active slot it is
 * computed from the live buffer rather than stored -- so the button lights the
 * moment you write a chord into it, and goes dark again if you clear it.
 */
static void buffer_empty_keeping_harmony(stk_t *st) {
    /* ONE definition of "empty", the same one Clear and boot use. */
    int key = st->prog.key, scale = st->prog.scale, rate = st->prog.rate;
    seed_default(&st->prog);
    st->prog.key = key;
    st->prog.scale = scale;
    st->prog.rate = rate;
    {
        int l = default_len(&st->prog);
        for (int k = 0; k < st->prog.count; k++) st->prog.ch[k].len = l;
    }
    st->prog.ch[0].root = default_root(st);
    st->bars_manual = 0;
    st->bars = 4;
}

/* The buffer IS the active slot's content; this is where they are made one. */
static void pattern_sync_active(stk_t *st) {
    int i = st->pat_cur;
    if (i < 0 || i >= STK_PATTERNS) return;
    st->pat[i].prog     = st->prog;
    st->pat[i].bars     = st->bars;
    st->pat[i].grouping = st->grouping;
    st->pat[i].used     = !buffer_is_empty(&st->prog);
}

static void pattern_activate(stk_t *st, int i) {
    if (i < 0 || i >= STK_PATTERNS || i == st->pat_cur) return;
    pattern_sync_active(st);        /* what you wrote stays where you wrote it */
    st->pat_cur = i;
    if (st->pat[i].used) {
        st->prog        = st->pat[i].prog;
        st->bars        = clampi(st->pat[i].bars, 1, 16);
        st->bars_manual = 1;        /* the length came WITH the pattern */
        st->grouping    = clampi(st->pat[i].grouping, 0, NUM_GROUPINGS - 1);
    } else {
        buffer_empty_keeping_harmony(st);
    }
    if (st->prog.count < 1) st->prog.count = 1;
    st->sel = clampi(st->sel, 1, st->prog.count);
    /* The chords under the playhead have just changed: stop what was armed,
     * exactly as a release does. */
    st->armed_id = 0;
    st->pend_n = 0;
    st->cut_pending = 1;
}

/* The long press: put a COPY of what you are playing into another slot and
 * carry on there -- "save as", the one thing select alone cannot do. */
static void pattern_copy_to(stk_t *st, int i) {
    if (i < 0 || i >= STK_PATTERNS) return;
    pattern_sync_active(st);
    if (i == st->pat_cur) return;
    st->pat[i] = st->pat[st->pat_cur];
    st->pat_cur = i;
}

static void pattern_delete(stk_t *st, int i) {
    if (i < 0 || i >= STK_PATTERNS) return;
    st->pat[i].used = 0;
    memset(&st->pat[i].prog, 0, sizeof st->pat[i].prog);
    /* Clearing the slot you are IN empties what you are playing, because they
     * are the same thing -- anything else would leave the lamp dark over a
     * buffer full of music. */
    if (i == st->pat_cur) {
        buffer_empty_keeping_harmony(st);
        st->sel = 1;
        st->armed_id = 0;
        st->pend_n = 0;
        st->cut_pending = 1;
    }
}

static void undo_capture(stk_t *st) {
    if (st->hist_busy) return;
    if (st->hist_depth > 0 &&
        st->hist_bars[st->hist_head] == st->bars &&
        memcmp(&st->hist[st->hist_head], &st->prog, sizeof(stk_prog_t)) == 0)
        return;                                   /* nothing actually moved */
    st->hist_head = (st->hist_head + 1) % STK_UNDO_DEPTH;
    st->hist[st->hist_head] = st->prog;
    st->hist_bars[st->hist_head] = st->bars;
    if (st->hist_depth < STK_UNDO_DEPTH) st->hist_depth++;
    /* A new edit after undoing discards the redo branch -- the future you
     * were walking back into no longer follows from the present. */
    st->hist_ahead = 0;
}

static void undo_step(stk_t *st, int back) {
    if (back) {
        if (st->hist_depth <= 1) return;          /* nothing behind the current */
        st->hist_head = (st->hist_head - 1 + STK_UNDO_DEPTH) % STK_UNDO_DEPTH;
        st->hist_depth--; st->hist_ahead++;
    } else {
        if (st->hist_ahead <= 0) return;
        st->hist_head = (st->hist_head + 1) % STK_UNDO_DEPTH;
        st->hist_depth++; st->hist_ahead--;
    }
    st->hist_busy = 1;
    st->prog = st->hist[st->hist_head];
    st->bars = st->hist_bars[st->hist_head];
    st->hist_busy = 0;
    if (st->sel > st->prog.count) st->sel = st->prog.count ? st->prog.count : 1;
    st->armed_id = 0;
    st->pend_n = 0;
    st->cut_pending = 1;
}

/*
 * The write-access parameters: things you DO, not values you set.
 *
 * Kept as one list so the relative path and any future caller agree on what a
 * trigger is; `test_stacks_shapes.sh` checks it against the access:"write"
 * entries in module.json, so adding a trigger there and forgetting it here
 * fails the build rather than shipping another silently dead knob.
 */
static int is_trigger_key(const char *k) {
    static const char *const T[] = {
        "read", "stamp", "clear", "duplicate", "insert", "remove", "play",
        "roll_vel", "roll_time", "undo", "redo", "stretch", "compress",
        "degree" };
    for (unsigned i = 0; i < sizeof(T) / sizeof(T[0]); i++)
        if (strcmp(k, T[i]) == 0) return 1;
    return 0;
}

/* Call around any change that can turn a rest into a chord. */
static void fill_from_rest(stk_t *st, stk_chord_t *c, int was_rest) {
    if (was_rest && c->shape != SHAPE_REST) { c->root = default_root(st); c->exact = 0; }
}

/*
 * Lay a library progression into the buffer.
 *
 * IT OVERWRITES, and that is the design rather than a compromise: Progression
 * is a BROWSER -- you turn it to hear what a Doo-wop or an Andalusian sounds
 * like in your key, and a browser that asked permission on every detent would
 * be unusable. What you do afterwards on CHORD, VOICE and FEEL is the
 * refinement; turning it again is asking for a different starting point.
 *
 * Roots come from `default_root` so the library lands in the Key and Octave
 * Default you set, and each entry's own mode is written to Scale -- otherwise
 * a minor progression would draw its lanes against a major grid.
 */
/*
 * THREE LIBRARIES, ONE APPLY. `styled` is the only thing that differs, and it
 * is a property of the LIBRARY, not of the chord: a genre entry is a voicing
 * somebody chose, the other two are plain shapes waiting for Colour to decide.
 */
/*
 * GROW THE CLIP TO HOLD THE MUSIC. Never shrink it.
 *
 * Four things lengthen a progression -- browsing a library entry, Duplicate,
 * Double Length, and adding chords with CHD -- and a fifth rescales it without
 * touching a single chord: RTE changes what a LEN unit MEANS, so the same four
 * chords are 4 bars at "1 bar" and 8 bars at "2 bar".
 *
 * Every one of them could push the music past the end of the clip, where it
 * still plays and is still drawn but is absent from anything Stamp writes.
 * That is silent truncation with the chords visible on screen, which is the
 * hardest kind to notice. So the rule lives in one function that all of them
 * call, rather than as four copies of the same four lines -- which is how CHD
 * and RTE came to be the two that never got it.
 *
 * NOT shrunk, because a shorter progression does not mean you wanted a shorter
 * clip; BAR is the one control whose whole job is to say that, and it is left
 * to say it.
 */
/*
 * WHILE THE BUFFER IS EMPTY, CHORDS AND BARS DIVIDE THE CLIP BETWEEN THEM.
 *
 * "Four chords across eight bars" should give four two-bar chords, and it is
 * only safe to answer that while there is nothing to destroy -- so this runs
 * ONLY when every slot is still a rest. The moment you write a chord the
 * lengths are yours and nothing derives them again.
 *
 * IT SETS THE LENGTHS, NOT THE RATE. Rate has four values, so BARS/CHORDS
 * lands on one of them only when the answer is exactly 2, 1, 1/2 or 1/4 bars:
 * three chords over eight bars needs 21.33 units and no rate can say it.
 * Deriving the rate would have worked for the tidy cases and silently done
 * nothing for the rest, which is worse than not doing it at all. `len` is
 * per-chord and runs to 64, so it can carry any of them; the rate follows only
 * when it happens to agree.
 */
static int buffer_is_empty(const stk_prog_t *p) {
    if (p->count < 1) return 0;
    for (int i = 0; i < p->count; i++)
        if (p->ch[i].shape != SHAPE_REST) return 0;
    return 1;
}

static void divide_clip_across_slots(stk_t *st) {
    if (!buffer_is_empty(&st->prog)) return;
    int n = st->prog.count;
    int len = clampi(clampi(st->bars, 1, 16) * UNITS_PER_BAR / n, 1, STK_MAX_LEN);
    for (int i = 0; i < n; i++) st->prog.ch[i].len = len;
    for (int r = 0; r < NUM_RATES; r++)
        if (RATE_DEFAULT_LEN[r] == len) { st->prog.rate = r; break; }
}

static int grow_bars_to_fit(stk_t *st) {
    int bu = bar_units_for(&st->prog);
    int need = clampi((prog_total_units(&st->prog) + bu - 1) / bu, 1, 16);
    /* Auto: match exactly. Manual: only ever grow, so nothing is truncated
     * behind your back, but a clip you asked for is not taken away. */
    if (!st->bars_manual) { int ch = st->bars != need; st->bars = need; return ch; }
    if (need > st->bars) { st->bars = need; return 1; }
    return 0;
}

static void apply_lib(stk_t *st, const stk_preset_t *lib, int n_lib,
                      int idx, int styled) {
    idx = clampi(idx, 0, n_lib - 1);
    const stk_preset_t *pr = &lib[idx];
    if (pr->n <= 0) return;              /* "none" leaves the buffer alone */

    /*
     * THE LIBRARY LANDS IN *YOUR* HARMONY; IT DOES NOT SET IT.
     *
     * This used to write `st->prog.scale = pr->minor ? 2 : 1`, so browsing
     * moved a control on a different bank -- you would set Minor on MAIN, turn
     * Progression to audition something, and find Scale had become Major
     * without touching it. Key, Octave Default and Scale live on MAIN and are
     * the harmony you chose; a browser is a source of MATERIAL, not of key.
     *
     * The entry's own minor/major flag is therefore dropped. Roots still come
     * from `default_root`, so the progression lands in your Key and Octave
     * Default, and root snapping in `chord_notes` bends it into your Scale.
     */
    int base = default_root(st);
    for (int i = 0; i < pr->n && i < STK_MAX_CHORDS; i++) {
        stk_chord_t *c = &st->prog.ch[i];
        memset(c, 0, sizeof(*c));
        c->root   = clampi(base + pr->s[i].semi, 0, 127);
        c->shape  = shape_by_opt(pr->s[i].q);
        c->len    = pr->s[i].len;
        c->rhythm = 0;
        c->styled = styled;
        c->exact  = 1;      /* the bVII is the point of the entry -- see stk_chord_t */
    }
    st->prog.count = pr->n > STK_MAX_CHORDS ? STK_MAX_CHORDS : pr->n;
    /*
     * THE CLIP GROWS TO HOLD WHAT WAS LOADED.
     *
     * It did not, and a twelve-bar blues browsed into a four-bar clip left two
     * thirds of itself outside the clip: playing, drawn on the staff, and
     * absent from anything Stamp wrote. Silent truncation, with the chords
     * visible on screen the whole time.
     *
     * Grown, never shrunk -- the same rule Duplicate follows. Browsing to a
     * shorter progression must not throw away a longer clip you set on
     * purpose, because the next thing you browse to may need it back.
     */
    grow_bars_to_fit(st);
    st->sel = 1;
    st->armed_id = 0;
    st->pend_n = 0;
    st->cut_pending = 1;
}

static void apply_preset(stk_t *st, int idx) {
    st->preset = clampi(idx, 0, NUM_PRESETS - 1);
    apply_lib(st, PRESETS, NUM_PRESETS, st->preset, 1);
}
static void apply_common(stk_t *st, int idx) {
    st->common = clampi(idx, 0, NUM_COMMON - 1);
    apply_lib(st, COMMON, NUM_COMMON, st->common, 0);
}
static void apply_uncommon(stk_t *st, int idx) {
    st->uncommon = clampi(idx, 0, NUM_UNCOMMON - 1);
    apply_lib(st, UNCOMMON, NUM_UNCOMMON, st->uncommon, 0);
}

static void audition(stk_t *st) {
    /* Preview "chord": hear the chord you are editing, as you edit it. ~0.7s
     * at the 344 Hz block rate. Suppressed while a pad is held, which is
     * already sounding the thing. */
    if (st->preview != 1 || st->held_pad >= 0) return;
    st->cut_pending = 1;
    st->armed_id = 0;
    st->pend_n = 0;
    st->force_step = st->sel - 1;
    st->force_until = st->pulse + audition_clocks(st, st->sel - 1);
}

static void stk_set_param_inner(void *instance, const char *key, const char *val);

/*
 * ONE capture point for every write. Wrapping is what makes "undo covers every
 * edit" true by construction rather than by remembering to call it in forty
 * places -- the failure mode of the latter is an edit that silently cannot be
 * undone, which is worse than no undo at all because it is discovered only
 * after the work is lost.
 */
static void stk_set_param(void *instance, const char *key, const char *val) {
    stk_t *st = (stk_t *)instance;
    if (!st || !key || !val) return;
    atomic_fetch_add_explicit(&st->rev, 1, memory_order_relaxed);   /* every write is snapshot-visible */
    stk_set_param_inner(instance, key, val);

    /*
     * THE CLIP RE-FITS AT THE DOOR, not at each call site.
     *
     * It was called from the four handlers somebody remembered -- and `len`,
     * `off`, `insert` and `remove` were not among them, so editing one chord's
     * length quietly pushed the progression past the end of the clip: 5.375
     * bars of music in a 4-bar clip, still playing, still drawn, and absent
     * from anything Stamp wrote. Exactly the failure the fitting rule exists
     * to prevent, reintroduced by four handlers that predate it.
     *
     * Three keys are exempt, and each for its own reason:
     *   bars  -- setting it IS the instruction; growing it back would refuse
     *            the one control whose job is to shorten the clip.
     *   undo/redo -- they restore a clip length from history; re-fitting would
     *            overwrite what was restored with what happens to fit now.
     */
    if (strcmp(key, "pat_recall") == 0) return;   /* it brought its own bars */
    if (strcmp(key, "bars") != 0 && strcmp(key, "undo") != 0 &&
        strcmp(key, "redo") != 0 && strcmp(key, "state") != 0)
        grow_bars_to_fit(st);

    /*
     * Storing and deleting a pattern do not touch the buffer, so they capture
     * nothing -- an undo stack filling with "you pressed store" would push the
     * edit you actually wanted to undo off the end of it. RECALL does replace
     * the buffer and is captured, so recalling by accident is undoable.
     */
    if (strcmp(key, "undo") != 0 && strcmp(key, "redo") != 0 &&
        strcmp(key, "state") != 0 &&
        strcmp(key, "pat_store") != 0 && strcmp(key, "pat_delete") != 0)
        undo_capture(st);
}

static void stk_set_param_inner(void *instance, const char *key, const char *val) {
    stk_t *st = (stk_t *)instance;
    if (!st || !key || !val) return;
    stk_chord_t *c = sel_chord(st);

    /*
     * A TRIGGER FIRES ON A TURN, whichever way you turn it.
     *
     * Every write-access parameter that sits on a bank -- Clear, Read Clip,
     * Stamp Clip and the two Randomizers -- was DEAD to the encoder: the
     * relative path below walks enums by name and simply had no branch for
     * them, so a turn fell through and nothing happened. The click cannot be
     * used instead, because in the grid it plays a chord.
     *
     * Direction is deliberately ignored. These have no ordering to walk --
     * "off" then "on" is a representation, not a range -- so the only sensible
     * reading of a detent is "do it".
     *
     * "~N" -- move an ENUM by N option positions.
     *
     * THE STEP IS IN THE VALUE, NOT THE KEY, and that is not a style choice.
     * The first version used "<key>:nudge", which never arrived: the canvas
     * runtime builds a full param key with `if (key.includes(":")) return key`
     * -- a key that already contains a colon is assumed to be fully qualified,
     * so "root:nudge" was sent WITHOUT the component prefix and addressed
     * nobody. Every enum in the takeover was silently dead; nothing logged,
     * because a write to an unknown key is not an error anywhere.
     *
     * A relative value on the plain key cannot collide with that rule. It
     * needs a sigil because "-1" is a legitimate absolute value for `inv` and
     * `cvel`, so a bare sign cannot mean "relative".
     *
     * The walk lives here because only this file knows the option lists --
     * a copy in JS is exactly the drift test_stacks_shapes.sh exists to stop.
     * Clamped, never wrapped, except `key`, which is a circle of twelve.
     */
    if (val[0] == '~') {
        int step = atoi(val + 1);
        if (step == 0) return;
        if (strcmp(key, "root") == 0) {
            c->root = ROOT_BASE + clampi(c->root - ROOT_BASE + step, 0, ROOT_SPAN - 1);
            c->exact = 1;                       /* you named it; the scale may not move it */
            audition(st);
        } else if (strcmp(key, "shape") == 0) {
            int was_rest = (c->shape == SHAPE_REST);
            c->shape = clampi(c->shape + step, 0, NUM_SHAPES - 1);
            fill_from_rest(st, c, was_rest);
            audition(st);
        } else if (strcmp(key, "family") == 0) {
            int was_rest = (c->shape == SHAPE_REST);
            int f = clampi(SHAPES[c->shape].fam + step, 0, NUM_FAMILIES - 1);
            for (int i = 0; i < NUM_SHAPES; i++)
                if (SHAPES[i].fam == f) { c->shape = i; break; }
            fill_from_rest(st, c, was_rest);
            audition(st);
        } else if (strcmp(key, "rhythm") == 0) {
            c->rhythm = clampi(c->rhythm + step, 0, NUM_RHYTHMS - 1);
                } else if (strcmp(key, "cmute") == 0) {
            c->mute = !c->mute;                 /* two options: a step is a flip */
        } else if (strcmp(key, "progression") == 0) {
            apply_preset(st, st->preset + step);
        } else if (strcmp(key, "common") == 0) {
            apply_common(st, st->common + step);
        } else if (strcmp(key, "uncommon") == 0) {
            apply_uncommon(st, st->uncommon + step);
        } else if (strcmp(key, "genre") == 0) {
            int cur = PRESETS[clampi(st->preset, 0, NUM_PRESETS - 1)].genre;
            int g = clampi(cur + step, 0, NUM_GENRES - 1);
            for (int i = 0; i < NUM_PRESETS; i++)
                if (PRESETS[i].genre == g) { apply_preset(st, i); break; }
        } else if (strcmp(key, "colour") == 0) {
            int v = clampi(c->colour + step, 0, NUM_COLOURS - 1);
            /* A styled chord is not recoloured -- see stk_chord_t::styled. */
            for (int k = 0; k < st->prog.count; k++)
                if (!st->prog.ch[k].styled) st->prog.ch[k].colour = v;
            audition(st);
        } else if (strcmp(key, "ccolour") == 0) {
            if (!c->styled) c->colour = clampi(c->colour + step, 0, NUM_COLOURS - 1);
            audition(st);
        } else if (is_trigger_key(key)) {
            stk_set_param_inner(st, key, "on");
        } else if (strcmp(key, "grouping") == 0) {
            st->grouping = clampi(st->grouping + step, 0, NUM_GROUPINGS - 1);
            audition(st);
        } else if (strcmp(key, "scale") == 0) {
            st->prog.scale = clampi(st->prog.scale + step, 0, NUM_SCALES - 1);
        } else if (strcmp(key, "key") == 0) {
            /* The detent IS the interval, so it transposes directly -- no
             * shortest-path guess, and turning up twelve times is an octave. */
            st->prog.key = ((st->prog.key + step) % 12 + 12) % 12;
            transpose_prog(st, step, 1);
            audition(st);
        } else if (strcmp(key, "rate") == 0) {
            st->prog.rate = clampi(st->prog.rate + step, 0, NUM_RATES - 1);
            if (buffer_is_empty(&st->prog)) {   /* rests are not written music */
                int len = default_len(&st->prog);
                for (int k = 0; k < st->prog.count; k++)
                    st->prog.ch[k].len = len;
            }

        } else if (strcmp(key, "run") == 0) {
            st->run = !st->run;
        } else if (strcmp(key, "lanes") == 0) {
            st->lanes = !st->lanes;
        } else if (strcmp(key, "preview") == 0) {
            st->preview = clampi(st->preview + step, 0, 2);
        } else if (strcmp(key, "read_mode") == 0) {
            st->read_mode = !st->read_mode;
        }
        return;
    }

    if (strcmp(key, "sel") == 0) {
        /*
         * SELECTING IS SILENT. Moving the cursor is navigation, not
         * performance: the jog writes `sel` on every detent, so auditioning
         * here made scrolling through a progression fire a chord per click and
         * turned a look at what you have into a performance of it. `play` is
         * the gesture that sounds a chord, and it is the only one that does so
         * unconditionally.
         */
        st->sel = clampi(atoi(val), 1, st->prog.count);
    } else if (strcmp(key, "root") == 0) {
        int i = root_index(val);
        if (i >= 0) { c->root = ROOT_BASE + i; c->exact = 1; audition(st); }
    } else if (strcmp(key, "shape") == 0) {
        int i = shape_index(val);
        if (i >= 0) {
            int was_rest = (c->shape == SHAPE_REST);
            c->shape = i;
            fill_from_rest(st, c, was_rest);
            audition(st);
        }
    } else if (strcmp(key, "coct") == 0) {
        /*
         * THE CHORD'S OCTAVE, AND IT IS DERIVED -- there is no second field.
         * `root` is one absolute note, so an octave stored beside it is a
         * second answer to "where is this chord", and the two disagree the
         * first time either is written alone (a preset restore, a clip read).
         * Setting this moves `root` by whole octaves and keeps its pitch
         * class; reading it reports the octave `root` is actually in.
         *
         * Distinct from the global Octave, which transposes the WHOLE
         * progression and leaves every chord's own octave alone.
         */
        int o = clampi(atoi(val), 1, ROOT_SPAN / 12);
        int rel = c->root - ROOT_BASE;
        if (rel < 0) rel = 0;
        c->root = ROOT_BASE + (o - 1) * 12 + (rel % 12);
        c->root = clampi(c->root, ROOT_BASE, ROOT_BASE + ROOT_SPAN - 1);
        audition(st);
    } else if (strcmp(key, "cstrum") == 0) {
        c->strum = clampi(atoi(val), 0, 100);
    } else if (strcmp(key, "cvel") == 0) {
        c->vel = clampi(atoi(val), -63, 63);
    } else if (strcmp(key, "cgate") == 0) {
        c->gate = clampi(atoi(val), -50, 50);
    } else if (strcmp(key, "cmute") == 0) {
        c->mute = (strcmp(val, "on") == 0 || atoi(val) == 1);
    } else if (strcmp(key, "rhythm") == 0) {
        for (int i = 0; i < NUM_RHYTHMS; i++)
            if (strcmp(val, RHYTHMS[i].name) == 0) { c->rhythm = i; return; }
        if (val[0] >= '0' && val[0] <= '9')
            c->rhythm = clampi(atoi(val), 0, NUM_RHYTHMS - 1);
    } else if (strcmp(key, "ctrans") == 0) {
        c->trans = clampi(atoi(val), -12, 12); audition(st);
    } else if (strcmp(key, "family") == 0) {
        /*
         * FAMILY IS A JUMP, NOT A FILTER, and that is what keeps the contract
         * static. It moves `shape` to the first chord of the chosen family;
         * `shape` then steps within that family because SHAPES[] is ordered by
         * it. Filtering the option list instead would mean serving a different
         * chain_params per family -- metadata the knob grid caches and settles
         * on, so the list it drew would stop matching the values it writes.
         */
        int f = opt_index(val, FAMILIES, NUM_FAMILIES);
        if (f >= 0 && SHAPES[c->shape].fam != f) {
            int was_rest = (c->shape == SHAPE_REST);
            for (int i = 0; i < NUM_SHAPES; i++)
                if (SHAPES[i].fam == f) { c->shape = i; break; }
            fill_from_rest(st, c, was_rest);
            audition(st);
        }
    } else if (strcmp(key, "inv") == 0) {
        c->inv = clampi(atoi(val), -3, 3); audition(st);
    } else if (strcmp(key, "len") == 0) {
        c->len = clampi(atoi(val), 1, STK_MAX_LEN);
    } else if (strcmp(key, "off") == 0) {
        c->off = clampi(atoi(val), 0, 15);
    } else if (strcmp(key, "grouping") == 0) {
        int i = opt_index(val, GROUPINGS, NUM_GROUPINGS);
        if (i >= 0) { st->grouping = i; audition(st); }
    } else if (strcmp(key, "scale") == 0) {
        int i = scale_index(val);
        if (i >= 0) { st->prog.scale = i; audition(st); }
    } else if (strcmp(key, "key") == 0) {
        int i = opt_index(val, NOTE_NAMES, 12);
        if (i >= 0) {
            /* An absolute key names a PITCH CLASS, so the interval is
             * ambiguous by an octave. Take the SHORT way round (-6..+5): the
             * progression moves as little as possible to reach the new key,
             * which is what "change key" means to a listener. */
            int d = ((i - st->prog.key) % 12 + 12) % 12;
            if (d > 6) d -= 12;
            st->prog.key = i;
            transpose_prog(st, d, 1);
            audition(st);
        }
    } else if (strcmp(key, "steps") == 0) {
        int want = clampi(atoi(val), 1, STK_MAX_CHORDS);
        /* Growing copies the LAST chord rather than inserting a default, so
         * "add a chord" starts from where the progression is, which is what
         * Live's + button does and what you almost always want to edit from. */
        for (int i = st->prog.count; i < want; i++)
            st->prog.ch[i] = st->prog.ch[st->prog.count > 0 ? st->prog.count - 1 : 0];
        st->prog.count = want;
        if (st->sel > want) st->sel = want;
        /*
         * DIVIDE ONLY IF THE CLIP IS THE THING YOU FIXED.
         *
         * On an empty buffer, Chords, Rate and Clip Bars settle each other and
         * the one you turned LAST should win. Dividing unconditionally broke
         * that: set Rate to "2 bar", then Chords to 4, and Chords divided the
         * clip instead -- four half-bar slots, with the rate you had just
         * chosen silently overwritten.
         *
         * So Chords divides the clip only when you have SET a clip length
         * (`bars_manual`). Otherwise it keeps the slot length Rate gave it and
         * lets the clip follow, which is the other half of the same rule.
         */
        if (st->bars_manual) divide_clip_across_slots(st);
        grow_bars_to_fit(st);      /* fewer chords is less music, too */
    } else if (strcmp(key, "rate") == 0) {
        int i = opt_index(val, RATE_OPTS, NUM_RATES);
        if (i >= 0) {
            st->prog.rate = i;
            /*
             * A REST IS NOT MUSIC YOU WROTE.
             *
             * "Rate cannot reach a chord that already exists" is there to stop
             * it redefining lengths you chose -- and an empty slot is not one
             * of those. Applying the rule to rests too meant that on an empty
             * buffer, CHD=4 and RTE="2 bar" left four one-bar slots in a
             * four-bar clip: two of the three controls silently ignored.
             *
             * So while nothing is written, Rate re-lengths every slot and the
             * clip follows. That completes the triangle the empty buffer
             * already had -- any two of Chords, Rate and Clip Bars settle the
             * third, and the one you turned last is the one that wins.
             */
            if (buffer_is_empty(&st->prog)) {
                int len = default_len(&st->prog);
                for (int k = 0; k < st->prog.count; k++)
                    st->prog.ch[k].len = len;
                /* Rate is now the thing you fixed, so the clip goes back to
                 * following. Nothing is written, so nothing is lost. */
                st->bars_manual = 0;
            }
        }
    } else if (strcmp(key, "run") == 0) {
        st->run = (strcmp(val, "on") == 0 || atoi(val) == 1);
    } else if (strcmp(key, "parked") == 0) {
        int p = (strcmp(val, "on") == 0 || atoi(val) == 1);
        if (p && !st->parked) {
            st->parked = 1;
            st->park_run_prev = st->run;
            st->park_preview_prev = st->preview;
            /* A take that is armed or under way keeps Run as the stamp set
             * it; anything else stops following the transport. */
            if (!st->stamp_armed && !st->stamp_once) st->run = 0;
            if (st->preview == 2) st->preview = 0;
            st->hold_run = 0;
            st->cut_pending = 1;           /* nothing rings on into Move's screen */
        } else if (!p && st->parked) {
            st->parked = 0;
            /* Give back what parking took, and what a take parked meanwhile. */
            if (st->park_run_prev || st->stamp_restore) st->run = 1;
            st->stamp_restore = 0;
            if (st->park_preview_prev == 2 && !st->clock_running) st->preview = 2;
        }
    } else if (strcmp(key, "read_track") == 0) {
        /* The Song.abl track Read Clip imports from. NOT where the output
         * goes: Move plays an injected note on whichever track is SELECTED
         * and ignores the channel (measured 2026-09-17). It FOLLOWS that
         * selection where the host publishes it (move_info.h, see the
         * worker) -- on a CHANGE, so a turn of this knob still holds until
         * you pick another track on Move. On an older host it is the only
         * answer, as before. */
        st->read_track = clampi(atoi(val), 1, 4);
    } else if (strcmp(key, "preview") == 0) {
        static const char *const P[] = { "off", "chord", "loop" };
        int i = opt_index(val, P, 3);
        if (i >= 0) {
            /*
             * AND THE MODULE'S OWN STOP REWINDS TOO -- same rule, other
             * transport. Leaving `loop` is the panel's Stop button when
             * nothing else is driving, so it must land where Move's stop
             * lands: at the top. Only when Move's clock is NOT running,
             * because then the position is MOVE'S and zeroing it here would
             * be this module arguing with the transport it is following.
             */
            if (st->preview == 2 && i != 2 && !st->clock_running) {
                st->pulse = 0;
                st->pulse_accum = 0.0;
                st->cur_step = -1;
            }
            /*
             * LEAVING `loop` MUST STOP LIKE A RELEASE DOES, not merely stop
             * scheduling. Assigning st->preview alone left whatever was armed
             * still armed and whatever was sounding still sounding, so the
             * chord rang on past the moment you asked for silence -- and with
             * Move's transport stopped there is no clock edge coming that
             * would have ended it. Same three lines the `play` release uses,
             * for the same reason it uses them: stop now, whatever was still
             * scheduled.
             */
            if (st->preview == 2 && i != 2) {
                st->armed_id = 0;
                st->pend_n = 0;
                st->cut_pending = 1;
            }
            st->preview = i;
        }
    } else if (strcmp(key, "octave") == 0) {
        st->octave = clampi(atoi(val), -2, 2);
    } else if (strcmp(key, "transpose") == 0) {
        st->transpose = clampi(atoi(val), -12, 12);
    } else if (strcmp(key, "velocity") == 0) {
        st->velocity = clampi(atoi(val), 1, 127);
    } else if (strcmp(key, "gate") == 0) {
        st->gate = clampi(atoi(val), 5, 100);
    } else if (strcmp(key, "lanes") == 0) {
        static const char *const L[] = { "scale", "diatonic" };
        int i = opt_index(val, L, 2);
        if (i >= 0) st->lanes = i;
    } else if (strcmp(key, "degree") == 0) {
        /*
         * SET THE ROOT BY SCALE DEGREE -- the primary act of writing a
         * progression, and the one thing the pads make fast.
         *
         * Counted through the SCALE's own mask rather than a table of sevens,
         * so a pentatonic has five degrees and a blues six: the pads can then
         * light only what exists instead of offering notes that would just be
         * snapped somewhere else. "rest" is the eighth pad, because emptying a
         * slot is the same gesture as choosing what fills it.
         */
        if (strcmp(val, "rest") == 0) {
            c->shape = SHAPE_REST;
        } else {
            int want = atoi(val);
            if (val[0] == 'I' || val[0] == 'V') {          /* roman numerals */
                static const char *const R[] = { "I","II","III","IV","V","VI","VII" };
                want = 0;
                for (int i = 0; i < 7; i++)
                    if (strcmp(val, R[i]) == 0) { want = i + 1; break; }
            }
            if (want >= 1) {
                uint16_t mask = SCALES[clampi(st->prog.scale, 0, NUM_SCALES-1)].mask;
                int seen = 0;
                for (int semi = 0; semi < 12; semi++) {
                    if (!(mask & (1u << semi))) continue;
                    if (++seen != want) continue;
                    int was_rest = (c->shape == SHAPE_REST);
                    c->root = clampi(default_root(st) + semi,
                                     ROOT_BASE, ROOT_BASE + ROOT_SPAN - 1);
                    c->exact = 0;               /* a degree is the scale's to move */
                    if (was_rest) c->shape = SHAPE_TRIAD_DEFAULT;
                    break;
                }
            }
        }
        audition(st);
    } else if (strcmp(key, "undo") == 0) {
        if (strcmp(val, "on") == 0 || atoi(val) == 1) undo_step(st, 1);
    } else if (strcmp(key, "redo") == 0) {
        if (strcmp(val, "on") == 0 || atoi(val) == 1) undo_step(st, 0);
    } else if (strcmp(key, "stretch") == 0) {
        /*
         * DOUBLE THE PROGRESSION IN TIME -- every chord twice as long, so the
         * phrase plays at half speed over twice the bars. Distinct from
         * Duplicate, which repeats the sequence at the same tempo: one changes
         * how long each chord is held, the other how many chords there are.
         *
         * Refused outright if the clip cannot hold the result, rather than
         * stretching some chords and not others -- a half-stretched phrase is
         * not a thing anyone asked for and is tedious to undo by hand.
         */
        if (strcmp(val, "on") == 0 || atoi(val) == 1) {
            int fits = 1;
            for (int k = 0; k < st->prog.count; k++)
                if (st->prog.ch[k].len * 2 > STK_MAX_LEN) fits = 0;
            if (fits) {
                for (int k = 0; k < st->prog.count; k++) st->prog.ch[k].len *= 2;
                grow_bars_to_fit(st);
                st->armed_id = 0;
                st->pend_n = 0;
                st->cut_pending = 1;
            }
        }
    } else if (strcmp(key, "compress") == 0) {
        /*
         * DOUBLE-TIME: the inverse of Double Length. All or nothing, like its
         * opposite -- halving some chords and not others is not "twice as
         * fast", it is a different progression, and it cannot be undone by
         * eye. A chord already at one unit cannot halve, so the whole gesture
         * is refused rather than flattening the relative lengths.
         */
        if (strcmp(val, "on") == 0 || atoi(val) == 1) {
            int fits = 1;
            for (int k = 0; k < st->prog.count; k++)
                if (st->prog.ch[k].len < 2) fits = 0;
            if (fits) {
                for (int k = 0; k < st->prog.count; k++) st->prog.ch[k].len /= 2;
                st->armed_id = 0;
                st->pend_n = 0;
                st->cut_pending = 1;
            }
        }
    } else if (strcmp(key, "duplicate") == 0) {
        /*
         * DUPLICATE THE WHOLE PROGRESSION, and grow the clip to hold it.
         *
         * The point of the gesture is a variation: double the phrase, then
         * edit the second half. Copying only as many chords as happened to fit
         * would leave a broken phrase, so it does nothing at all when the
         * result would not fit -- a refusal you can see is better than a
         * mangling you cannot.
         *
         * The clip has to follow. `bars` is the length that gets stamped, and
         * doubling the music inside a clip that did not grow would simply
         * truncate half of it -- so the bars are raised to whatever the longer
         * progression needs, and never lowered: shrinking someone's clip is
         * not implied by asking for more music.
         */
        if ((strcmp(val, "on") == 0 || atoi(val) == 1)
            && st->prog.count * 2 <= STK_MAX_CHORDS) {
            int n = st->prog.count;
            for (int i = 0; i < n; i++) st->prog.ch[n + i] = st->prog.ch[i];
            st->prog.count = n * 2;

            grow_bars_to_fit(st);

            st->armed_id = 0;
            st->pend_n = 0;
        }
    } else if (strcmp(key, "insert") == 0) {
        /*
         * INSERT AFTER THE SELECTED CHORD -- a PERMUTATION of the array, not a
         * rebuild. The new chord is a COPY of the one you were on, because you
         * insert while editing something you want a variation of; a default
         * triad would throw away the root and length you just dialled in.
         *
         * `armed_id` is cleared because it names an ABSOLUTE step: after a
         * shift, the same number is a different chord, and the sequencer would
         * hold notes belonging to a chord that has moved.
         */
        if ((strcmp(val, "on") == 0 || atoi(val) == 1) && st->prog.count < STK_MAX_CHORDS) {
            int at = clampi(st->sel, 1, st->prog.count);   /* insert AT index `at` */
            for (int i = st->prog.count; i > at; i--)
                st->prog.ch[i] = st->prog.ch[i - 1];
            st->prog.ch[at] = st->prog.ch[at - 1];
            st->prog.count++;
            st->sel = at + 1;                              /* land on the new one */
            st->armed_id = 0;
        }
    } else if (strcmp(key, "remove") == 0) {
        /* Delete the selected chord. A progression of one is the floor: zero
         * chords is a state with no way back from the grid, since every add
         * gesture copies the chord you are on. */
        if ((strcmp(val, "on") == 0 || atoi(val) == 1) && st->prog.count > 1) {
            int at = clampi(st->sel, 1, st->prog.count) - 1;
            for (int i = at; i < st->prog.count - 1; i++)
                st->prog.ch[i] = st->prog.ch[i + 1];
            st->prog.count--;
            if (st->sel > st->prog.count) st->sel = st->prog.count;
            st->armed_id = 0;
        }
    } else if (strcmp(key, "play") == 0) {
        /*
         * Audition the selected chord on demand -- the takeover's jog click.
         *
         * It reuses the force-step mechanism a held pad and an edit audition
         * already use, rather than sounding a chord here: set_param IS the SPI
         * callback, and emitting from it would put note-ons outside the
         * scheduler that owns every other note this module plays.
         */
        if (strcmp(val, "on") == 0 || atoi(val) == 1) {
            /* RETRIGGER: the new chord replaces whatever is ringing rather
             * than joining it, so a press is always ONE audible chord. */
            /* Start the sequence AT the selected chord, so what you hear
             * begins where you are looking. */
            int unit = UNIT_CLOCKS;
            st->pulse = chord_slot_start(&st->prog, st->sel - 1) * unit;
            st->pulse_accum = 0.0;
            st->hold_run = 1;
            st->force_step = -1;
            st->force_until = 0;
            st->cut_pending = 1;
            st->armed_id = 0;
            st->pend_n = 0;
        } else {
            /* Released: stop now, whatever was still scheduled. */
            st->hold_run = 0;
            st->armed_id = 0;
            st->pend_n = 0;
            st->cut_pending = 1;
        }
    } else if (strcmp(key, "genre") == 0) {
        /* Genre JUMPS to the first progression of that genre and applies it --
         * the same gesture Family performs on Shape, so there is nothing new
         * to learn. PRESETS[] is ordered by genre for exactly this. */
        int g = opt_index(val, GENRES, NUM_GENRES);
        if (g >= 0 && PRESETS[clampi(st->preset,0,NUM_PRESETS-1)].genre != g) {
            for (int i = 0; i < NUM_PRESETS; i++)
                if (PRESETS[i].genre == g) { apply_preset(st, i); break; }
        }
    } else if (strcmp(key, "common") == 0) {
        for (int i = 0; i < NUM_COMMON; i++)
            if (strcmp(val, COMMON[i].name) == 0) { apply_common(st, i); return; }
        if (val[0] >= '0' && val[0] <= '9') apply_common(st, atoi(val));
    } else if (strcmp(key, "uncommon") == 0) {
        for (int i = 0; i < NUM_UNCOMMON; i++)
            if (strcmp(val, UNCOMMON[i].name) == 0) { apply_uncommon(st, i); return; }
        if (val[0] >= '0' && val[0] <= '9') apply_uncommon(st, atoi(val));
    } else if (strcmp(key, "progression") == 0) {
        for (int i = 0; i < NUM_PRESETS; i++)
            if (strcmp(val, PRESETS[i].name) == 0) { apply_preset(st, i); return; }
        if (val[0] >= '0' && val[0] <= '9') apply_preset(st, atoi(val));
    } else if (strcmp(key, "colour") == 0) {
        /*
         * THE PROGRESSION'S colour: writes every chord.
         *
         * It is the same setting as the per-chord one, reached at a different
         * scope -- Start colours the whole thing while you are browsing, Voice
         * colours the one you are editing, and each reads back what the other
         * left. Two knobs, one fact; a separate global value shadowing the
         * per-chord ones would give two answers to "what colour is this
         * chord", and the pair would drift the first time either was set
         * alone.
         *
         * A STYLED chord is skipped -- see stk_chord_t::styled. It is skipped
         * rather than written-and-ignored so that reading Colour back is
         * honest: a genre progression reports `written` and stays there while
         * you turn the knob, which is what actually happens to it.
         */
        int i = opt_index(val, COLOURS, NUM_COLOURS);
        if (i >= 0) {
            for (int k = 0; k < st->prog.count; k++)
                if (!st->prog.ch[k].styled) st->prog.ch[k].colour = i;
            audition(st);
        }
    } else if (strcmp(key, "ccolour") == 0) {
        int i = opt_index(val, COLOURS, NUM_COLOURS);
        if (i >= 0 && !c->styled) { c->colour = i; audition(st); }
    } else if (strcmp(key, "defoct") == 0) {
        int want = clampi(atoi(val), 1, ROOT_SPAN / 12);
        transpose_prog(st, (want - st->defoct) * 12, 12);
        st->defoct = want;
        audition(st);
    } else if (strcmp(key, "bars") == 0) {
        st->bars = clampi(atoi(val), 1, 16);
        st->bars_manual = 1;       /* you have said what you want */
        divide_clip_across_slots(st);
    } else if (strcmp(key, "swing") == 0) {
        st->swing = clampi(atoi(val), 0, 75);
    } else if (strcmp(key, "hum_vel") == 0) {
        st->hum_vel = clampi(atoi(val), 0, 100);
    } else if (strcmp(key, "hum_time") == 0) {
        st->hum_time = clampi(atoi(val), 0, 100);
    } else if (strcmp(key, "roll_vel") == 0) {
        /* A NEW SEED, not a new algorithm. Randomize re-rolls which deviations
         * every note gets; the AMOUNT stays where the knob is, so pressing it
         * gives a different feel of the same strength. Separate seeds for the
         * two axes so re-rolling the timing does not disturb a velocity
         * pattern you had settled on. */
        if (strcmp(val, "on") == 0 || atoi(val) == 1)
            st->seed_vel = hash3(st->seed_vel, 0x5EEDu, (unsigned)st->pulse);
    } else if (strcmp(key, "roll_time") == 0) {
        if (strcmp(val, "on") == 0 || atoi(val) == 1)
            st->seed_time = hash3(st->seed_time, 0x7113u, (unsigned)st->pulse);
    } else if (strcmp(key, "read_mode") == 0) {
        static const char *const M[] = { "replace", "append" };
        int i = opt_index(val, M, 2);
        if (i >= 0) st->read_mode = i;
    } else if (strcmp(key, "read") == 0) {
        if (strcmp(val, "on") == 0 || atoi(val) == 1) {
            /*
             * NO FILE I/O HERE. This is the SPI callback; all it does is take
             * a snapshot for append mode and bump a counter the worker polls.
             * granny stalls the param channel by loading a WAV in set_param
             * and that is exactly the bug being avoided.
             */
            g_req_base = st->prog;
            atomic_store(&st->req_read, atomic_load(&st->req_read) + 1);
        }
    } else if (strcmp(key, "stamp") == 0) {
        if (strcmp(val, "on") == 0 || atoi(val) == 1) {
            if (st->stamp_armed) {
                /*
                 * ARMED IS A STATE YOU CAN LEAVE. Pressing again was a second
                 * arm, so the only way out was Move's stop button -- a state
                 * the surface that entered it could not exit. Pressing the
                 * same control takes it back, Run included.
                 */
                st->stamp_armed = 0;
                st->run = st->stamp_run_prev;
                atomic_store(&st->status, 0);
                return;
            }
            /* ARM, and let Move's transport say when. See the clock handler
             * -- the lap starts on a Start or on the next bar line, so the
             * pass Move records begins on a downbeat rather than wherever
             * the press landed. The one-lap stop is armed with it, so nothing
             * doubles the clip afterwards. The user presses Record on Move;
             * this never touches it (see press_move_record's obituary). */
            st->stamp_armed = 1;
            atomic_store(&st->status, 4);   /* armed: waiting for a downbeat */
            st->armed_id = 0;
            st->stamp_run_prev = st->run;
            if (!st->clock_running) {
                /* No clock to follow yet: the next Start begins the lap. */
                st->run = 0;
            }
        }
    } else if (strcmp(key, "pat_store") == 0) {
        pattern_copy_to(st, atoi(val) - 1);    /* 1-based on the wire, like sel */
    } else if (strcmp(key, "pat_recall") == 0) {
        pattern_activate(st, atoi(val) - 1);
    } else if (strcmp(key, "pat_delete") == 0) {
        pattern_delete(st, atoi(val) - 1);
    } else if (strcmp(key, "clear") == 0) {
        /*
         * CLEAR MEANS EMPTY, NOT "LOAD THE DEMO".
         *
         * This used to reseed C-G-Am-F -- which is the right thing at LOAD,
         * where the module must make a sound before anyone has typed anything,
         * and the wrong thing on a button: pressing Clear and receiving
         * somebody else's chord progression is not clearing.
         *
         * Empty is ONE REST, not zero chords. Every add gesture copies the
         * chord you are on, so a progression of length zero is a state with no
         * way out of it -- and a single rest is visible in the grid as a framed
         * empty slot, which is what "nothing here yet" should look like.
         */
        if (strcmp(val, "on") == 0 || atoi(val) == 1) {
            /* ONE definition of "empty", shared with boot. Keeping a second
             * copy here is how the two drifted into meaning different things
             * the first time. */
            int key = st->prog.key, scale = st->prog.scale, rate = st->prog.rate;
            seed_default(&st->prog);
            st->prog.key = key;            /* keep the harmony you set */
            st->prog.scale = scale;
            st->prog.rate = rate;
            /* and the new slots take THEIR length from the rate you kept */
            {
                int l = default_len(&st->prog);
                for (int k = 0; k < st->prog.count; k++) st->prog.ch[k].len = l;
            }
            st->prog.ch[0].root = default_root(st);
            /* Clear returns to the BOOT state, and the clip is part of that:
             * four bars, fitting again. Leaving the previous length behind
             * meant an emptied buffer still carried the shape of whatever you
             * had just thrown away. */
            st->bars_manual = 0;
            st->bars = 4;
            st->preset = 0;                /* all three browsers back to "none": */
            st->common = 0;                /* an emptied buffer that still names */
            st->uncommon = 0;              /* a progression is claiming a lie */
            st->sel = 1;
            st->armed_id = 0;
            st->pend_n = 0;
            st->cut_pending = 1;
        }
    } else if (strcmp(key, "state") == 0) {
        /* Symmetrical with the writer above, field for field. A version this
         * build does not know is REFUSED rather than partially applied. */
        const char *p = strstr(val, "\"s\":\"");
        if (!p) return;
        /* The map around the blob may carry read_track (it is not a blob
         * field); a set written before it existed simply lacks it. */
        const char *rt = strstr(val, "\"read_track\":\"");
        if (rt) st->read_track = clampi(atoi(rt + 14), 1, 4);
        /* So may transpose, for the same reason: it rides in the map, and a
         * blob written before it existed restores at 0. */
        const char *tp = strstr(val, "\"transpose\":\"");
        st->transpose = tp ? clampi(atoi(tp + 13), -12, 12) : 0;
        p += 5;
        int key_, scale_, rate_, bars_, defoct_, oct_, vel_, gate_, swing_;
        int hv_, ht_, lanes_, prev_, rm_, sm_, run_, preset_, sel_, count_;
        int common_, uncommon_, grouping_;
        unsigned sv_, stv_;
        int n = sscanf(p, "v6|%d|%d|%d|%d|%d|%d|%d|%d|%d|%d|%d|%u|%u|%d|%d|%d|%d|%d|%d|%d|%d|%d|%d|%d|",
                       &key_, &scale_, &rate_, &bars_, &defoct_, &oct_, &vel_,
                       &gate_, &swing_, &hv_, &ht_, &sv_, &stv_, &lanes_,
                       &prev_, &rm_, &sm_, &run_, &grouping_, &preset_, &common_, &uncommon_,
                       &sel_, &count_);
        if (n != 24) return;

        st->prog.key   = clampi(key_, 0, 11);
        st->prog.scale = clampi(scale_, 0, NUM_SCALES - 1);
        st->prog.rate  = clampi(rate_, 0, NUM_RATES - 1);
        st->bars       = clampi(bars_, 1, 16);
        st->defoct     = clampi(defoct_, 1, ROOT_SPAN / 12);
        st->octave     = clampi(oct_, -2, 2);
        st->velocity   = clampi(vel_, 1, 127);
        st->gate       = clampi(gate_, 5, 100);
        st->swing      = clampi(swing_, 0, 75);
        st->hum_vel    = clampi(hv_, 0, 100);
        st->hum_time   = clampi(ht_, 0, 100);
        st->seed_vel   = sv_;
        st->seed_time  = stv_;
        st->lanes      = lanes_ ? 1 : 0;
        st->preview    = clampi(prev_, 0, 2);
        st->read_mode  = rm_ ? 1 : 0;
        (void)sm_;                      /* stamp_mode: field kept, meaning gone */
        st->run        = run_ ? 1 : 0;
        st->grouping   = clampi(grouping_, 0, NUM_GROUPINGS - 1);
        st->preset     = clampi(preset_, 0, NUM_PRESETS - 1);
        st->common     = clampi(common_, 0, NUM_COMMON - 1);
        st->uncommon   = clampi(uncommon_, 0, NUM_UNCOMMON - 1);

        /*
         * Walk to the chord list. TWENTY-FIVE pipes, not twenty-four: the tag
         * and the 24 values are 25 tokens, so 25 separators stand before the
         * chords. Counting the VALUES and forgetting the tag landed the parser
         * on `count`, whose first field then failed to match -- and the
         * recovery skipped to the next ';', silently dropping chord one and
         * restoring a progression one chord short.
         */
        const char *c = p;
        for (int i = 0; i < 25 && c; i++) { c = strchr(c, '|'); if (c) c++; }
        int k = 0;
        while (c && *c && *c != '"' && *c != '@' && *c != '#' && k < STK_MAX_CHORDS) {
            int r, sh, iv, ln, of, str_, cv, cg, mu, rh, tr, co, sy, ex = 0;
            /* Thirteen fields, and a FOURTEENTH (`exact`) that a blob written
             * before it existed simply lacks: thirteen matched is a chord. */
            if (sscanf(c, "%d.%d.%d.%d.%d.%d.%d.%d.%d.%d.%d.%d.%d.%d",
                       &r, &sh, &iv, &ln, &of, &str_, &cv, &cg, &mu, &rh, &tr,
                       &co, &sy, &ex) >= 13) {
                stk_chord_t *q = &st->prog.ch[k];
                q->root   = clampi(r, 0, 127);
                q->shape  = clampi(sh, 0, NUM_SHAPES - 1);
                q->inv    = clampi(iv, -3, 3);
                q->len    = clampi(ln, 1, STK_MAX_LEN);
                q->off    = clampi(of, 0, 15);
                q->strum  = clampi(str_, 0, 100);
                q->vel    = clampi(cv, -63, 63);
                q->gate   = clampi(cg, -50, 50);
                q->mute   = mu ? 1 : 0;
                q->rhythm = clampi(rh, 0, NUM_RHYTHMS - 1);
                q->trans  = clampi(tr, -12, 12);
                q->colour = clampi(co, 0, NUM_COLOURS - 1);
                q->styled = sy ? 1 : 0;
                q->exact  = ex ? 1 : 0;
                k++;
            }
            const char *semi = strchr(c, ';');
            if (!semi) break;
            c = semi + 1;
        }
        if (k > 0) st->prog.count = k;
        else st->prog.count = clampi(count_, 1, STK_MAX_CHORDS);
        st->sel = clampi(sel_, 1, st->prog.count);
        st->armed_id = 0;
        st->pend_n = 0;
        st->cut_pending = 1;

        /*
         * THE PATTERNS, if this blob carries any. Cleared first: restoring a
         * state means BECOMING it, and leaving the previous set's patterns
         * behind would be a preset that half-applied -- the failure this whole
         * serialiser is written against.
         */
        for (int i = 0; i < STK_PATTERNS; i++) st->pat[i].used = 0;
        st->pat_cur = 0;      /* you are always IN a slot; slot 1 by default */
        for (const char *g = strchr(p, '@'); g; g = strchr(g + 1, '@')) {
            int slot, pk, ps, pr, pb, pg, pc;
            if (sscanf(g + 1, "%d.%d.%d.%d.%d.%d.%d:", &slot, &pk, &ps, &pr,
                       &pb, &pg, &pc) != 7) continue;
            if (slot < 1 || slot > STK_PATTERNS) continue;
            stk_pattern_t *P = &st->pat[slot - 1];
            memset(P, 0, sizeof *P);
            P->prog.key   = clampi(pk, 0, 11);
            P->prog.scale = clampi(ps, 0, NUM_SCALES - 1);
            P->prog.rate  = clampi(pr, 0, NUM_RATES - 1);
            P->bars       = clampi(pb, 1, 16);
            P->grouping   = clampi(pg, 0, NUM_GROUPINGS - 1);
            const char *q = strchr(g, ':');
            int m = 0;
            while (q && *++q && *q != '@' && *q != '"' && m < STK_MAX_CHORDS) {
                int r, sh, iv, ln, of, str_, cv, cg, mu, rh, tr, co, sy, ex = 0;
                if (sscanf(q, "%d.%d.%d.%d.%d.%d.%d.%d.%d.%d.%d.%d.%d.%d",
                           &r, &sh, &iv, &ln, &of, &str_, &cv, &cg, &mu, &rh,
                           &tr, &co, &sy, &ex) >= 13) {
                    stk_chord_t *z = &P->prog.ch[m];
                    z->root   = clampi(r, 0, 127);
                    z->shape  = clampi(sh, 0, NUM_SHAPES - 1);
                    z->inv    = clampi(iv, -3, 3);
                    z->len    = clampi(ln, 1, STK_MAX_LEN);
                    z->off    = clampi(of, 0, 15);
                    z->strum  = clampi(str_, 0, 100);
                    z->vel    = clampi(cv, -63, 63);
                    z->gate   = clampi(cg, -50, 50);
                    z->mute   = mu ? 1 : 0;
                    z->rhythm = clampi(rh, 0, NUM_RHYTHMS - 1);
                    z->trans  = clampi(tr, -12, 12);
                    z->colour = clampi(co, 0, NUM_COLOURS - 1);
                    z->styled = sy ? 1 : 0;
                    z->exact  = ex ? 1 : 0;
                    m++;
                }
                const char *comma = strchr(q, ',');
                const char *nextp = strchr(q, '@');
                if (!comma || (nextp && comma > nextp)) break;
                q = comma;
            }
            P->prog.count = m > 0 ? m : clampi(pc, 1, STK_MAX_CHORDS);
            P->used = m > 0;
        }
        const char *cm = strchr(p, '#');
        if (cm) {
            int cur = atoi(cm + 1) - 1;
            /* Only if it points at a slot that actually holds something: a
             * lamp on an empty button is the one thing the eye cannot check. */
            if (cur >= 0 && cur < STK_PATTERNS) st->pat_cur = cur;
        }
    }
}

/*
 * `prog` -- ONE READ THAT DRAWS THE WHOLE STAFF.
 *
 * canvas.js needs every chord's name, length and actual PITCHES to lay out the
 * staff, and a param read is ~2.8 ms against a 1.68 ms whole-page render, so
 * asking per chord is not affordable. It arrives as a single viz extra_key.
 *
 * The RESOLVED notes are published, not the recipe. Voicing, inversion, octave
 * and scale snapping are decided here and would otherwise be reimplemented in
 * JS -- two implementations of the same chord, guaranteed to disagree the
 * first time either is touched, with the staff drawing something the synth
 * never played.
 *
 *   v1|count|sel|playing|rate|scale|key|NAME,len,off,n.n.n;...
 */
static int fmt_prog(const stk_t *st, char *buf, int buf_len) {
    /*
     * THE SCALE TRAVELS AS ITS MASK, not as its index. The staff's horizontal
     * lines ARE the selected scale, so canvas.js needs to know which pitch
     * classes are in it -- and a copy of the scale table in JS is a second
     * answer to that, guaranteed to disagree with SCALES[] the first time
     * either is edited. The index would be exactly such a copy; twelve bits
     * are the fact itself.
     */
    /*
     * THE LANE MASK IS NOT ALWAYS THE SCALE'S. "Note Lanes" chooses what the
     * horizontal lines mean, and it is a DISPLAY choice with no effect on what
     * is played -- snapping still uses prog.scale either way:
     *
     *   scale     one lane per note of the selected scale, so the grid changes
     *             when the scale does and every chord tone lands on a line.
     *   diatonic  seven lanes per octave on the natural letters, which is what
     *             notation means by a staff. Eb and E share a lane and are
     *             told apart by the accidental mark, exactly as on paper.
     *
     * Sending the MASK rather than the mode keeps canvas.js free of a second
     * copy of the scale table -- it draws lanes from twelve bits and does not
     * need to know which of the two questions produced them.
     */
    unsigned lane_mask = st->lanes ? 0xAB5u : (unsigned)SCALES[st->prog.scale].mask;
    int lane_key = st->lanes ? 0 : st->prog.key;
    /*
     * v6 sends the two TIME facts the grid cannot infer.
     *
     * A step is not always eight eighths -- Rate makes it 16, 8, 4 or 2 -- and
     * the drawer had that as a constant, so every rate but "1 bar" drew the
     * wrong widths. And the clip is `bars` long, which is a different length
     * from the progression: the grid spans the CLIP and repeats the
     * progression to fill it, exactly as Stamp writes it. Sending the rate
     * INDEX would make the drawer keep its own copy of the rate table; sending
     * the eighths sends the fact.
     */
    int unit = UNIT_CLOCKS;
    int step_eighths = UNITS_PER_BAR;
    /* The clip is `bars` BARS long; a unit is an eighth of a STEP, and a step
     * is not always a bar -- so the two have to be converted through clocks
     * rather than assumed equal. */
    int clip_eighths = clampi(st->bars, 1, 16) * 4 * CLOCKS_PER_QUARTER / unit;
    /*
     * v10 adds BEATS PER STEP, which is what lets the grid draw a playhead
     * without an IPC read per frame. The overlay can see the transport and the
     * tempo cheaply (shadow_get_overlay_state is a SHM read), but it has no
     * way to know how long one of this module's steps is in musical time --
     * that depends on Rate, which lives here.
     */
    int step_beats = BAR_CLOCKS / CLOCKS_PER_QUARTER;   /* beats in a bar */

    /*
     * v11 adds the TEMPO and the module's own POSITION, and both exist for the
     * playhead.
     *
     * The overlay was extrapolating from `samplerBpm` in shared memory, which
     * is only populated once the sampler has run -- so the FIRST play used the
     * 120 fallback and swept at the wrong rate, and every play after it was
     * right. Reading the tempo from the same place the scheduler reads it
     * removes the discrepancy by construction.
     *
     * The position turns every read into a re-sync. Anchoring only on the
     * transport's start edge meant entering mid-loop had no anchor at all, and
     * that wall-clock drift had no way to be corrected.
     */
    float bpm_f = (g_host && g_host->get_bpm) ? g_host->get_bpm() : 120.0f;
    if (bpm_f < 20.0f || bpm_f > 300.0f) bpm_f = 120.0f;
    int pos_units = unit > 0 ? (st->pulse / unit) : 0;

    /*
     * v12 appends the VOICE GROUPING, because the staff has to show that it is
     * on. It is a whole-progression setting with no cell of its own in the
     * takeover's chord row, so without a mark the notes move and nothing says
     * why -- which reads as the module having drifted rather than as a setting
     * doing its job.
     */
    /*
     * v13 adds a VELOCITY TO EVERY NOTE -- "60:110", where v12 sent "60".
     *
     * Humanise is a per-NOTE deviation, so with one velocity per chord it was
     * unrepresentable: pressing Randomize genuinely changed the take and
     * nothing on either screen moved, which reads as a dead button. The
     * drawers size a notehead by velocity, so publishing it per note is what
     * makes the feel visible at all.
     *
     * It comes from note_velocity(), the same function playback and the stamp
     * use, so the picture cannot show a velocity the synth did not play.
     */
    int at = snprintf(buf, buf_len, "v13|%d|%d|%d|%d|%03x|%d|%d|%d|%d|%d|%d|%d|",
                      st->prog.count, st->sel, st->cur_step,
                      step_eighths, lane_mask, lane_key, clip_eighths,
                      ((st->clock_running && st->run) || st->hold_run
                       || st->preview == 2) ? 1 : 0,
                      step_beats < 1 ? 1 : step_beats,
                      (int)(bpm_f + 0.5f), pos_units, st->grouping);
    int pv_prev[STK_MAX_TONES + 2], pv_n = 0;
    for (int k = 0; k < st->prog.count && at < buf_len - 1; k++) {
        const stk_chord_t *c = &st->prog.ch[k];
        /* The NAME is the chord that SOUNDS -- snapped and transposed -- and
         * the ROOT field after it is the one the knob edits. They differ
         * exactly when the scale or Transpose moved the chord, which is what
         * the caption is there to tell you. */
        int pc = ((c->root % 12) + 12) % 12;
        const stk_chord_t pl = played(st, c);
        int spc = ((sounding_root(&pl, 0, st->prog.scale, st->prog.key) % 12) + 12) % 12;
        int is_rest = (c->shape == SHAPE_REST);
        /*
         * v3 adds the ROOT NAME beside the chord name. The takeover's top row
         * shows the root you are editing, and the chord name gives only its
         * pitch class -- "Gm7" is the same string an octave up. Deriving the
         * octave in JS would mean re-deriving it from the notes, which
         * inversion makes wrong: after one inversion the lowest note is no
         * longer the root. The module knows; it says.
         */
        int oct = clampi((c->root - ROOT_BASE) / 12 + 1, 1, ROOT_SPAN / 12);
        /*
         * The suffix names the chord that will SOUND. Colour reshapes the
         * intervals at play time, so a triad coloured to a ninth is drawn with
         * five noteheads -- and used to be captioned "Am", the written shape,
         * contradicting the picture it sat above.
         */
        /*
         * The caption is the WRITTEN shape, and that is now correct by
         * construction: Colour is voicing, and a shell or drop-2 Amaj7 is
         * still an Amaj7. While Colour was a density ladder it changed which
         * chord sounded, so the name had to be re-derived -- that whole
         * apparatus went with the ladder.
         */
        const char *suffix = SHAPES[c->shape].suffix;
        /* NAME,ROOT,INV,LEN,OFF,notes -- every field the top row shows, so a
         * field it displays can never be one the format forgot to send. */
        /*
         * v5 adds VEL and MUTE, because the grid draws them: a quiet chord is
         * drawn as a lighter block and a muted one as an outline. Those are
         * facts about how it SOUNDS, so they come from the module that decides
         * it rather than being re-derived from the knob positions -- the knob
         * is an offset on a global the drawer cannot see.
         */
        int eff_vel = clampi(st->velocity + c->vel, 1, 127);
        /*
         * v8 adds REPEAT, because the grid has to draw what is played. A chord
         * set to retrigger three times sounded three times and was drawn as one
         * long block -- so the only way to see the setting was to remember
         * turning the knob.
         */
        at += snprintf(buf + at, buf_len - at, "%s%s%s,%s%d,%d,%d,%d,%d,%d,%d,",
                       k ? ";" : "",
                       is_rest ? "" : NOTE_NAMES[spc],
                       is_rest ? "REST" : suffix,
                       NOTE_NAMES[pc], oct, c->inv, c->len, c->off,
                       eff_vel, c->mute ? 1 : 0,
                       (unsigned)RHYTHMS[clampi(c->rhythm,0,NUM_RHYTHMS-1)].mask);
        int tones[STK_MAX_TONES];
        int n = chord_notes_g(&pl, st->octave, st->prog.scale, st->prog.key,
                              c->colour, st->grouping,
                              pv_n ? pv_prev : NULL, pv_n, tones, STK_MAX_TONES);
        pv_n = n > STK_MAX_TONES ? STK_MAX_TONES : n;
        for (int q = 0; q < pv_n; q++) pv_prev[q] = tones[q];
        for (int i = 0; i < n && at < buf_len - 1; i++)
            at += snprintf(buf + at, buf_len - at, "%s%d:%d", i ? "." : "",
                           tones[i], note_velocity(st, c, k, i, 0));
    }
    return at;
}

/* Append `,"key":"value"` with the two characters JSON cannot carry raw
 * escaped. Values here are option names, numbers and the module's own
 * delimited strings; none carries a quote today, and this is what keeps
 * that from ever mattering. */
static int state_append_kv(char *buf, int at, int buf_len,
                           const char *key, const char *val) {
    int n = snprintf(buf + at, buf_len - at, ",\"%s\":\"", key);
    if (n < 0 || at + n >= buf_len - 3) return 0;
    int start = at; at += n;
    for (const char *p = val; *p && at < buf_len - 3; p++) {
        if (*p == '"' || *p == '\\') buf[at++] = '\\';
        if ((unsigned char)*p < 0x20) continue;
        buf[at++] = *p;
    }
    buf[at++] = '"'; buf[at] = 0;
    return at - start;
}

static int stk_get_param(void *instance, const char *key, char *buf, int buf_len);

static int state_append_map(const stk_t *st, char *buf, int at, int buf_len) {
    static const char *const DERIVED[] = { "prog", "status", "patterns", "module_id" };
    static const char *const SKIP[] = { "state", "chain_params", "rui_poll", "grid" };
    char v[4096];
    int start = at;
    for (unsigned i = 0; i < sizeof(DERIVED) / sizeof(DERIVED[0]); i++) {
        if (stk_get_param((void *)st, DERIVED[i], v, sizeof v) < 0) continue;
        at += state_append_kv(buf, at, buf_len, DERIVED[i], v);
    }
    /* Every declared key: walk the declaration for "key":"..." */
    const char *p = CHAIN_PARAMS_JSON;
    while ((p = strstr(p, "\"key\":\"")) != NULL) {
        p += 7;
        const char *e = strchr(p, '"');
        if (!e || e - p >= 40) break;
        char k[41];
        memcpy(k, p, (size_t)(e - p)); k[e - p] = 0;
        p = e;
        int skip = 0;
        for (unsigned i = 0; i < sizeof(SKIP) / sizeof(SKIP[0]); i++)
            if (strcmp(k, SKIP[i]) == 0) skip = 1;
        if (skip || is_trigger_key(k)) continue;
        if (stk_get_param((void *)st, k, v, sizeof v) < 0) continue;
        at += state_append_kv(buf, at, buf_len, k, v);
    }
    return at - start;
}

static int stk_get_param(void *instance, const char *key, char *buf, int buf_len) {
    stk_t *st = (stk_t *)instance;
    if (!st || !key || !buf || buf_len < 2) return -1;
    const stk_chord_t *c = sel_chord((stk_t *)st);

    /*
     * THE SHADOW UI ASKS THE COMPONENT, NOT module.json. Declaring
     * chain_params in module.json alone gets the knobs (those come from
     * ui_hierarchy) and silently loses everything that lives ONLY here -- the
     * `view` as_page param and every `viz`. The result is the staff page
     * appearing as an ordinary row of knob cells with no grid, and nothing
     * logged, because "no chain_params" is a legal answer.
     *
     * Generated from module.json by tools/gen_chain_params.py so the
     * two cannot drift; test_stacks_shapes.sh fails when they do.
     */
    if (strcmp(key, "chain_params") == 0)
        return snprintf(buf, buf_len, "%s", CHAIN_PARAMS_JSON);
    /*
     * The canvas param has no value, and must say so EXPLICITLY.
     *
     * Returning -1 means "the read did not complete", which the grid draws as
     * "--" -- the placeholder for an unresolved read. An empty string is the
     * other answer: the channel served us and the key produced nothing. The
     * distinction is the tri-state this codebase already pays for elsewhere,
     * and here it is the difference between a cell that looks broken and one
     * that looks like a door.
     */
    /*
     * The canvas cell's value is the WORD ON THE DOOR. An empty string is
     * rendered by the grid as "NONE", which on the entry point to the whole
     * editor reads as "disabled" -- the opposite of what the cell does.
     */
    if (strcmp(key, "grid") == 0) return snprintf(buf, buf_len, "OPEN");
    if (strcmp(key, "prog") == 0)  return fmt_prog(st, buf, buf_len);
    if (strcmp(key, "sel") == 0)   return snprintf(buf, buf_len, "%d", st->sel);
    if (strcmp(key, "root") == 0) {
        int i = clampi(c->root - ROOT_BASE, 0, ROOT_SPAN - 1);
        return snprintf(buf, buf_len, "%s%d", NOTE_NAMES[i % 12], 1 + i / 12);
    }
    if (strcmp(key, "shape") == 0) return snprintf(buf, buf_len, "%s", SHAPES[c->shape].opt);
    /* Both derived, never stored beside what they describe: two fields that
     * must agree are two fields that eventually will not. */
    if (strcmp(key, "coct") == 0)
        return snprintf(buf, buf_len, "%d",
                        clampi((c->root - ROOT_BASE) / 12 + 1, 1, ROOT_SPAN / 12));
    if (strcmp(key, "family") == 0)
        return snprintf(buf, buf_len, "%s", FAMILIES[SHAPES[c->shape].fam]);
    if (strcmp(key, "inv") == 0)   return snprintf(buf, buf_len, "%d", c->inv);
    if (strcmp(key, "cstrum") == 0)  return snprintf(buf, buf_len, "%d", c->strum);
    if (strcmp(key, "cvel") == 0)    return snprintf(buf, buf_len, "%d", c->vel);
    if (strcmp(key, "cgate") == 0)   return snprintf(buf, buf_len, "%d", c->gate);
    if (strcmp(key, "cmute") == 0)   return snprintf(buf, buf_len, "%s", c->mute ? "on" : "off");
    if (strcmp(key, "rhythm") == 0)
        return snprintf(buf, buf_len, "%s", RHYTHMS[clampi(c->rhythm,0,NUM_RHYTHMS-1)].name);
    if (strcmp(key, "ctrans") == 0)  return snprintf(buf, buf_len, "%d", c->trans);
    if (strcmp(key, "len") == 0)   return snprintf(buf, buf_len, "%d", c->len);
    if (strcmp(key, "off") == 0)   return snprintf(buf, buf_len, "%d", c->off);
    if (strcmp(key, "grouping") == 0)
        return snprintf(buf, buf_len, "%s", GROUPINGS[clampi(st->grouping,0,NUM_GROUPINGS-1)]);
    if (strcmp(key, "scale") == 0) return snprintf(buf, buf_len, "%s", SCALES[st->prog.scale].name);
    if (strcmp(key, "key") == 0)   return snprintf(buf, buf_len, "%s", NOTE_NAMES[st->prog.key]);
    if (strcmp(key, "steps") == 0) return snprintf(buf, buf_len, "%d", st->prog.count);
    if (strcmp(key, "rate") == 0)  return snprintf(buf, buf_len, "%s", RATE_OPTS[st->prog.rate]);
    if (strcmp(key, "run") == 0)   return snprintf(buf, buf_len, "%s", st->run ? "on" : "off");
    if (strcmp(key, "read_track") == 0) return snprintf(buf, buf_len, "%d", st->read_track);
    if (strcmp(key, "parked") == 0) return snprintf(buf, buf_len, "%s", st->parked ? "on" : "off");
    if (strcmp(key, "module_id") == 0) return snprintf(buf, buf_len, "stacks");
    if (strcmp(key, "rui_poll") == 0) {
        /*
         * rev:on:tick:bpm[:devms] -- the Tool tab's cheap digest. The shim
         * probes it in-process every few frames and pushes on change, so the
         * expensive `state` read happens only when `rev` moved. `devms` rides
         * only while playing: the stopped digest must stay byte-stable or
         * every probe is a change.
         */
        int on = ((st->clock_running && st->run) || st->hold_run
                  || st->preview == 2) ? 1 : 0;
        float bpm_f = (g_host && g_host->get_bpm) ? g_host->get_bpm() : 120.0f;
        if (bpm_f < 20.0f || bpm_f > 300.0f) bpm_f = 120.0f;
        int pos_units = UNIT_CLOCKS > 0 ? (st->pulse / UNIT_CLOCKS) : 0;
        unsigned rev = atomic_load_explicit(&st->rev, memory_order_relaxed);
        if (on)
            return snprintf(buf, buf_len, "%u:1:%d:%d:%lu", rev, pos_units,
                            (int)(bpm_f + 0.5f), st->blocks * 128UL / 44UL);
        return snprintf(buf, buf_len, "%u:0:%d:%d", rev, pos_units,
                        (int)(bpm_f + 0.5f));
    }
    if (strcmp(key, "preview") == 0) {
        static const char *const P[] = { "off", "chord", "loop" };
        return snprintf(buf, buf_len, "%s", P[clampi(st->preview, 0, 2)]);
    }
    if (strcmp(key, "octave") == 0)   return snprintf(buf, buf_len, "%d", st->octave);
    if (strcmp(key, "transpose") == 0) return snprintf(buf, buf_len, "%d", st->transpose);
    if (strcmp(key, "velocity") == 0) return snprintf(buf, buf_len, "%d", st->velocity);
    if (strcmp(key, "gate") == 0)     return snprintf(buf, buf_len, "%d", st->gate);
    if (strcmp(key, "lanes") == 0)
        return snprintf(buf, buf_len, "%s", st->lanes ? "diatonic" : "scale");
    /* Both derived from the applied preset, never stored twice. */
    if (strcmp(key, "common") == 0)
        return snprintf(buf, buf_len, "%s", COMMON[clampi(st->common,0,NUM_COMMON-1)].name);
    if (strcmp(key, "uncommon") == 0)
        return snprintf(buf, buf_len, "%s", UNCOMMON[clampi(st->uncommon,0,NUM_UNCOMMON-1)].name);
    if (strcmp(key, "genre") == 0)
        return snprintf(buf, buf_len, "%s",
                        GENRES[PRESETS[clampi(st->preset,0,NUM_PRESETS-1)].genre]);
    if (strcmp(key, "progression") == 0)
        return snprintf(buf, buf_len, "%s", PRESETS[clampi(st->preset,0,NUM_PRESETS-1)].name);
    /* Both report the SELECTED chord, so Start and Voice always agree. */
    if (strcmp(key, "colour") == 0 || strcmp(key, "ccolour") == 0)
        return snprintf(buf, buf_len, "%s", COLOURS[clampi(c->colour, 0, NUM_COLOURS - 1)]);
    if (strcmp(key, "defoct") == 0)   return snprintf(buf, buf_len, "%d", st->defoct);
    if (strcmp(key, "bars") == 0)     return snprintf(buf, buf_len, "%d", st->bars);
    if (strcmp(key, "swing") == 0)    return snprintf(buf, buf_len, "%d", st->swing);
    if (strcmp(key, "hum_vel") == 0)  return snprintf(buf, buf_len, "%d", st->hum_vel);
    if (strcmp(key, "hum_time") == 0) return snprintf(buf, buf_len, "%d", st->hum_time);
    if (strcmp(key, "read_mode") == 0)
        return snprintf(buf, buf_len, "%s", st->read_mode ? "append" : "replace");
    /*
     * Write-only triggers read back as off, so a click is never sticky. ONE
     * table -- is_trigger_key's -- because this was a hand-kept copy that
     * had fallen five keys behind it, and a bank refresh reading `degree`
     * got -1 for it: an errored read per second in the host's param log,
     * for a key that exists.
     */
    if (is_trigger_key(key))
        return snprintf(buf, buf_len, "off");
    if (strcmp(key, "patterns") == 0) {
        /*
         * ONE READ DRAWS ALL SIXTEEN. A mask of which slots hold something,
         * and which one this buffer came from -- the two facts a button needs
         * to pick its colour, in one value, because the staff already spends
         * its read budget on `prog`.
         *
         * Both 1-BASED on the wire, as `sel` is: the surfaces talk about
         * "pattern 3", and an off-by-one between two UIs and a DSP is a bug
         * nobody can see until it is on hardware.
         */
        const stk_t *k = (const stk_t *)st;
        unsigned mask = 0;
        for (int i = 0; i < STK_PATTERNS; i++) {
            /* The ACTIVE slot is whatever the buffer currently holds -- so the
             * button lights as soon as you write a chord into an empty slot,
             * and goes dark again if you clear it. Anything else would need an
             * explicit "store" to tell the truth. */
            int used = (i == k->pat_cur) ? !buffer_is_empty(&k->prog)
                                         : k->pat[i].used;
            if (used) mask |= 1u << i;
        }
        return snprintf(buf, buf_len, "%04x|%d", mask, k->pat_cur + 1);
    }
    if (strcmp(key, "status") == 0) {
        /*
         * ARMED IS A STATE OF THE MODULE, NOT A GUESS BY A SURFACE.
         *
         * Rec-arm stamping reported NOTHING between the press and the
         * transport -- the worker sets this field and the rec-arm path never
         * touched it -- so the panel invented an "armed" flag of its own,
         * started when IT pressed the button. Which meant the Move's own stamp
         * lit nothing anywhere, a stamp armed from the browser stayed lit on
         * the browser after a transport stop had silently cancelled it, and
         * the two screens disagreed about a state only one of them could see.
         * Published here, both read the same answer and neither owns it.
         */
        static const char *const S[] = { "idle", "working", "ok", "failed", "armed" };
        return snprintf(buf, buf_len, "%s", S[clampi(atomic_load(&((stk_t *)st)->status), 0, 4)]);
    }
    if (strcmp(key, "state") == 0) {
        /*
         * THE WHOLE INSTRUMENT, or the preset is a lie.
         *
         * This used to write root.shape.inv.len.off and nothing else, so
         * saving a preset silently discarded Spread, Strum, per-chord Velocity
         * and Gate, Mute, Rhythm, Transpose, and every global but four --
         * including Key and Scale. A preset that restores a different sound
         * than it saved is worse than no preset, because you only discover it
         * after the original is gone.
         *
         * Wrapped in JSON with one opaque string: the blob's format is mine,
         * but anything that tries to parse it as JSON still succeeds. Versioned
         * so an older blob can be refused rather than misread -- half a
         * progression restored from a field that moved is the failure this
         * whole exercise is about.
         */
        int at = snprintf(buf, buf_len,
            "{\"s\":\"v6|%d|%d|%d|%d|%d|%d|%d|%d|%d|%d|%d|%u|%u|%d|%d|%d|%d|%d|%d|%d|%d|%d|%d|%d|",
            st->prog.key, st->prog.scale, st->prog.rate, st->bars, st->defoct,
            st->octave, st->velocity, st->gate, st->swing,
            st->hum_vel, st->hum_time, st->seed_vel, st->seed_time,
            st->lanes, st->preview, st->read_mode, 0 /* was stamp_mode */,
            st->run, st->grouping, st->preset, st->common, st->uncommon,
            st->sel, st->prog.count);
        for (int k = 0; k < st->prog.count && at < buf_len - 40; k++) {
            const stk_chord_t *q = &st->prog.ch[k];
            at += snprintf(buf + at, buf_len - at,
                           "%s%d.%d.%d.%d.%d.%d.%d.%d.%d.%d.%d.%d.%d.%d",
                           k ? ";" : "", q->root, q->shape, q->inv, q->len,
                           q->off, q->strum, q->vel, q->gate,
                           q->mute, q->rhythm, q->trans, q->colour, q->styled,
                           q->exact);
        }
        /*
         * THE PATTERNS RIDE ALONG, after the buffer and behind a marker.
         *
         * Appended rather than versioned into the head so a blob written
         * before patterns existed still loads: the reader below walks chords
         * until it meets '@' or the closing quote, and an old blob simply has
         * neither section. The separators are deliberately NOT ';' -- that is
         * the chord separator, and a pattern containing one would send the
         * chord walk off into the pattern list.
         */
        /* The buffer is the active slot's content, so it has to be written
         * through before the slots are serialised -- otherwise a set reloads
         * with the pattern as it was when you last switched away from it. */
        pattern_sync_active((stk_t *)st);
        for (int i = 0; i < STK_PATTERNS && at < buf_len - 64; i++) {
            if (!st->pat[i].used) continue;
            const stk_prog_t *g = &st->pat[i].prog;
            at += snprintf(buf + at, buf_len - at, "@%d.%d.%d.%d.%d.%d.%d:",
                           i + 1, g->key, g->scale, g->rate,
                           st->pat[i].bars, st->pat[i].grouping, g->count);
            for (int k = 0; k < g->count && at < buf_len - 48; k++) {
                const stk_chord_t *q = &g->ch[k];
                at += snprintf(buf + at, buf_len - at,
                               "%s%d.%d.%d.%d.%d.%d.%d.%d.%d.%d.%d.%d.%d.%d",
                               k ? "," : "", q->root, q->shape, q->inv, q->len,
                               q->off, q->strum, q->vel, q->gate,
                               q->mute, q->rhythm, q->trans, q->colour, q->styled,
                               q->exact);
            }
        }
        /* And WHICH slot this buffer came from -- otherwise a restored set
         * has its patterns but a dark lamp, and the button that is actually
         * loaded looks like the ones that are not. */
        at += snprintf(buf + at, buf_len - at, "#%d", st->pat_cur + 1);
        at += snprintf(buf + at, buf_len - at, "\"");
        /*
         * AND THE MAP THE TOOL TAB SEEDS FROM. As an overtake tool the manager
         * reads `state` as a FLAT OBJECT OF STRINGS -- one value per param the
         * panel binds, plus the derived ones a chain component published as
         * viz.extra_keys. The blob stays first, under "s", so a restore that
         * looks for it finds it and a set written before the map existed still
         * loads. Keys come from the one declaration (CHAIN_PARAMS_JSON), so a
         * param added there is in the snapshot without a second list.
         */
        at += state_append_map(st, buf, at, buf_len);
        at += snprintf(buf + at, buf_len - at, "}");
        return at;
    }
    return -1;
}

static midi_fx_api_v1_t g_api = {
    .api_version    = MIDI_FX_API_VERSION,
    .create_instance = stk_create,
    .destroy_instance = stk_destroy,
    .process_midi   = stk_process_midi,
    .tick           = stk_tick,
    .set_param      = stk_set_param,
    .get_param      = stk_get_param,
};

midi_fx_api_v1_t* move_midi_fx_init(const host_api_v1_t *host) {
    g_host = host;
    return &g_api;
}

/* =====================================================================
 * THE TOOL ENTRY (plugin_api_v2) -- an overtake DSP, addressing a Move track.
 *
 * Same instance, same state machine, same entry points: on_midi is
 * stk_process_midi and render_block is stk_tick, and whatever they hand back
 * is INJECTED into Move on cable 2 (host->midi_inject_to_move) on the
 * channel of `track`, instead of being returned to a chain. Every test that
 * drives the MIDI FX surface drives this one too, which is the point of
 * keeping both symbols exported (the API header sanctions it).
 *
 * Two things the chain never sent that the shim sends an overtake DSP:
 *
 *   CABLE-0 NOTES ARE NOT PITCHES. Pad presses arrive on the audio thread as
 *   pad IDENTIFIERS 68-99 and knob touches as notes 0-9, tagged
 *   MOVE_MIDI_SOURCE_INTERNAL. A pad id would enter the pitch-class selector
 *   as a note -- 68 is G#, chord 9 -- so they are dropped here; the UI owns
 *   the pads and writes params.
 *
 *   ITS OWN OUTPUT COMES BACK. Move echoes injected notes on MIDI_OUT cable 2
 *   and the shim routes MIDI_OUT to the overtake DSP, so the sounding-voices
 *   filter inside stk_process_midi is still load-bearing (see
 *   tests/test_stacks_own_tail.sh).
 * ===================================================================== */
#define STK_TOOL_MAX_OUT 32

static void tool_inject(stk_t *st, uint8_t out[][3], int lens[], int n) {
    (void)st;
    if (!g_host || !g_host->midi_inject_to_move) return;
    /* Channel 1, always: Move plays an injected note on the SELECTED track
     * whatever channel it carries, so there is nothing to choose here. */
    uint8_t ch = 0;
    for (int i = 0; i < n; i++) {
        if (lens[i] < 2) continue;
        uint8_t type = out[i][0] & 0xF0;
        /* Notes only, on the way out as on the way in (see tool_on_midi):
         * nothing else this module returns is something it generated. */
        if (type != 0x80 && type != 0x90) continue;
        /* USB-MIDI: cable 2, and the CIN is the status nibble for every
         * channel-voice type (0x8 note-off ... 0xE bend). */
        uint8_t pkt[4] = {
            (uint8_t)((2 << 4) | (type >> 4)),
            (uint8_t)(type | ch),
            out[i][1],
            (uint8_t)(lens[i] >= 3 ? out[i][2] : 0),
        };
        g_host->midi_inject_to_move(pkt, 4);
    }
}

static void tool_on_midi(void *instance, const uint8_t *msg, int len, int source) {
    stk_t *st = (stk_t *)instance;
    if (!st || !msg || len < 1) return;
    uint8_t type = msg[0] & 0xF0;
    if (source == MOVE_MIDI_SOURCE_INTERNAL &&
        (type == 0x80 || type == 0x90 || type == 0xA0))
        return;                                   /* pad ids, not pitches */
    /*
     * THE TOOL INJECTS WHAT IT GENERATES, NEVER WHAT IT RECEIVED.
     *
     * In a chain slot process_midi passes a message it does not understand
     * straight through -- a sustain pedal should reach the synth. As a tool
     * the output goes INTO Move and Move echoes it back on MIDI_OUT, which the
     * shim routes here. Press Stop and Move sends All Sound Off (CC 120) on
     * its tracks; passed through and injected, Move echoed it, and the loop
     * ran once per block for the rest of the session -- every chord you
     * triggered afterwards was silenced within 2.9ms (measured: 16 packets
     * per 46ms, "no chords anymore after stop"). So a channel-voice message
     * that is not a note never reaches process_midi at all. Notes still do:
     * a keyboard selects chords, and the module's own echo is filtered on its
     * sounding voices inside.
     */
    if (type != 0x80 && type != 0x90 && msg[0] < 0xF0) return;
    uint8_t out[STK_TOOL_MAX_OUT][3];
    int lens[STK_TOOL_MAX_OUT];
    int n = stk_process_midi(st, msg, len, out, lens, STK_TOOL_MAX_OUT);
    tool_inject(st, out, lens, n);
}

static void tool_render_block(void *instance, int16_t *out_lr, int frames) {
    stk_t *st = (stk_t *)instance;
    if (out_lr && frames > 0)
        memset(out_lr, 0, (size_t)frames * 2 * sizeof(int16_t));   /* silent */
    if (!st || frames <= 0) return;
    uint8_t out[STK_TOOL_MAX_OUT][3];
    int lens[STK_TOOL_MAX_OUT];
    int n = stk_tick(st, frames, 44100, out, lens, STK_TOOL_MAX_OUT);
    tool_inject(st, out, lens, n);
}

static int tool_get_error(void *instance, char *buf, int buf_len) {
    (void)instance;
    if (buf && buf_len > 0) buf[0] = 0;
    return 0;
}

static plugin_api_v2_t g_tool_api = {
    .api_version      = MOVE_PLUGIN_API_VERSION_2,
    .create_instance  = stk_create,
    .destroy_instance = stk_destroy,
    .on_midi          = tool_on_midi,
    .set_param        = stk_set_param,
    .get_param        = stk_get_param,
    .get_error        = tool_get_error,
    .render_block     = tool_render_block,
};

plugin_api_v2_t* move_plugin_init_v2(const host_api_v1_t *host) {
    g_host = host;
    return &g_tool_api;
}
