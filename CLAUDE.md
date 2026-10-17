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
  Make every client with `pmClient()` (control.go): it names the app
  (`X-Push-Hack-App: stacks`). While Stacks owns the surface push-manager
  refuses (409) a client that does not -- the screen's did not, and under
  Push Hack Home Stacks' screen never showed while it lit the pads.
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

## The staff's lanes hold still (surface.FitLanes)

Reported twice on device as inconsistent; the rules that fixed it, each
pinned in `surface/inversion_test.go` with voicings measured off the core:

- Lane spacing is one of `laneSteps` (6..2 px), never height/span: a
  derived spacing changed on every edit and moved every chord.
- Zoom OUT only when notes would leave the window; zoom IN only when the
  harmony (chord names, count) changes. A margin-based zoom-in pumped on a
  multi-chord inversion sweep -- no margin outruns a wider sweep.
- Otherwise pan, by whole octaves when that suffices (tonic rows stay).
- Zooms leave half an octave of headroom; key/scale/lane mode refit.
- Undo and Redo give back the zoom their state had (`surface.ZoomMemory`,
  user's request 2026-10-18): the window last used with each set of
  notes is remembered, and taken back only on the frame an Undo/Redo
  changed. Turning a value back by hand keeps the hold-still rules.
- The window lives in `screen` (main) across frames; `Scene.Lanes` zero
  fits afresh (mockups). The register bar shows octave pans.

## Listen mode (listen.go, surface/listen.go)

- With Push Hack Home (push-manager #38) Stacks is an app: the session
  (control.go `session`, pmclient.RunSurface) says focus (-> modeEdit) and
  blur (-> modeLive); `askMode` asks push-manager for the rest: Shift +
  LIVE -> modeListen (SurfaceInput false), LIVE -> SurfaceLeave. Stacks
  must not handle Shift + Help then: it is push-manager's. Never call
  SetMode or SetMidiFilter when managed; push-manager owns both.
- Without Home (an older push-manager), Shift + Help cycles modeLive ->
  modeEdit -> modeListen (control.go setMode). Listening: display only,
  intercept off, Stacks parked, no LED painting.
- The notes come from port `Stacks Listen`, fed by Browser Bridge's listen
  track (input = the selected track, Post FX). On Push the pads are not a
  MIDI input another track can take; a track as input is.
- Listened notes NEVER reach the core's MIDI input; the core only names
  them (`chord_of:` -> name_played). One naming for staff and listen screen.
- Motion is reported by RenderListen; the screen ticks at 33 ms only to
  draw it, and sends nothing when the frame did not change.
- Test on a device without hands: `cmd/notetest -dst <client:port>` plays
  chords note by note (straight to `Stacks Listen`, or into a Live input).

## Commands

    make test | make vet | make build
    ./scripts/deploy.sh push.local [stop]   # SSH_OPTS="-o HostKeyAlias=push.local" for an IP
    # with the catalog's service installed, deploy.sh swaps only the binary through it
    ./scripts/sync-stacks.sh [ref | --check]
