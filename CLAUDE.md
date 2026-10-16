# CLAUDE.md -- push-hack-stacks

Stacks (the Schwung/Move chord tool, `../schwung-stacks`) hosted on Push 3
as a push-hack catalog hack. Framework rules live in
`../ableton-push-hack/CLAUDE.md` and apply here; the port plan is
`../ableton-push-hack/plans/2026-10-16-stacks-port-to-push3.md`.

## Rules that are not optional

- **`src/upstream/stacks/` is never edited.** It is schwung-stacks at
  `UPSTREAM_REF`. A fix to Stacks goes to schwung-stacks first, then
  `scripts/sync-stacks.sh`. Push-specific code goes AROUND it, in
  `src/dsp/stacks_unit.c` (defines before the `#include`) and `host.c`.
- **One goroutine calls into C.** `engine.go` owns the instance on a locked
  OS thread. HTTP handlers use `Engine.Call`; the MIDI reader sends on
  `Engine.midi`. Never call a `dsp.Instance` method from anywhere else.
- **The host struct comes from the vendored header.** `host.c` fills a
  `host_api_v1_t` from `host/plugin_api_v1.h`; never redeclare it. A
  layout mismatch is the boot-loop bug schwung-stacks' CLAUDE.md describes.
- **Static link.** Push has glibc 2.35, the build image 2.41. `make build`
  links with `-extldflags -static`; a dynamic cgo binary fails to exec.
- **No `/dev/snd` before `alsaseq.WaitForBootSettle()`** (USB-A wedge).
- **Display: through push-manager only** (`core/pmclient`), never shm.
- **Strings drawn on Push's screen are ASCII only** (`core/gfx/text`).

## Measured on device (2026-10-16, Live 12.4.15b7)

- Live connects to `Stacks Out` by itself; notes play on the selected track.
- `Stacks In` needs Sync on in Push's MIDI settings; Live then sends Start
  (30), clock (36), Stop (32). Live remembers Sync by port name.
- At push-catalog's nice 19: block timer late 0.56 ms avg, 2.0 ms max, no
  skipped blocks, ~2% of one core, 7 MB RSS.

## UX bar for the Push surface

World-class, industrial-style: a strict grid on the 8 encoders and 16
buttons, a small set of meaningful colors, screen and pads telling one
story. Mock the 960x160 screen and the 8x8 pads as images and review them
with the user BEFORE writing drawing code. After the implementation, owe a
Push control-matrix artifact like the Move one
(https://claude.ai/artifact/3hAogpmmAxZvgFqbf38WvS).

## Commands

    make test | make vet | make build
    ./scripts/deploy.sh push.local [stop]
    ./scripts/sync-stacks.sh [ref | --check]
